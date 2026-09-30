package db

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// The statistics the console's charts read, computed from the run and task records rather than
// kept apart: "a series is computed from the run, task and artifact records rather than kept
// apart, so a range reaches back as far as the namespace keeps its runs, and no further". What a
// series counts is the runs of the workflows the authorizer allowed, and of no others, as the
// listing of runs reads them, which is why it is read through a *Wide opened for RunListing.

// Buckets are the buckets a series is counted in: Count of them, each Width long, the first
// starting at First. The API lays them on whole minutes, quarters, hours or days; this package
// counts in whatever it is given.
type Buckets struct {
	First time.Time
	Width time.Duration
	Count int
}

// End is the instant after the last bucket, which no bucket holds.
func (b Buckets) End() time.Time { return b.First.Add(time.Duration(b.Count) * b.Width) }

// MaxBuckets is the most buckets one series is counted in: more than any chart draws, which a
// query would pay for all the same.
const MaxBuckets = 1000

func (b Buckets) check() error {
	switch {
	case b.Width <= 0:
		return errors.New("db: a series counted in buckets of no length")
	case b.Count < 1 || b.Count > MaxBuckets:
		return fmt.Errorf("db: a series counted in %d buckets, where it takes 1 to %d", b.Count, MaxBuckets)
	}
	return nil
}

// Percentiles are three percentiles of a length of time, in whole milliseconds, and Taken is false
// where there was nothing to take them over.
type Percentiles struct {
	P50, P95, P99 int64
	Taken         bool
}

// ExitCodeCount is how many attempts ended with one exit code, or with none where ExitCode is nil:
// lost with their runner, or stopped before a container reported one.
type ExitCodeCount struct {
	ExitCode *int
	Attempts int
}

// RunBucket is what the runs created in one bucket came to.
type RunBucket struct {
	// Runs are the runs created in the bucket by state: an ended run under its final state, one
	// still going under where it is now, so that they add up to what GET /api/v1/runs lists over
	// the bucket, within the namespace's retention. A run's record outlives its retention, which
	// lets go of its envelopes and logs, and a series stops where the retention does all the same.
	Runs map[string]int

	// Duration is how long the runs of the bucket that ended took, from started_at to finished_at.
	Duration Percentiles

	// QueueWait is how long their tasks waited to be handed out, from ready_at to dispatched_at.
	QueueWait Percentiles

	// Retries are the attempts that were retried, by the exit code of the attempt that failed, the
	// most frequent first.
	Retries []ExitCodeCount
}

// Bin is one bin of a histogram of durations: the runs whose duration, in milliseconds, is at
// least From and below Until, or at most Until for the last bin.
type Bin struct {
	From, Until int64
	Runs        int
}

// runStates are the states a run is counted under, in the order the documentation names them.
var runStates = []string{"queued", "running", "waiting", "succeeded", "failed", "cancelled", "timed_out"}

// counted is the runs a series counts: those of the allowed workflows created within the buckets,
// and within the namespace's retention, since "a range reaches back as far as the namespace keeps
// its runs, and no further". Each carries the bucket it falls in, from 0.
//
// $1 and $2 are the namespaces and workflows allowed, $3 the first bucket's start, $4 the instant
// after the last one, and $5 a bucket's width in seconds.
const counted = `
	counted as (
	  select r.namespace, r.id, r.state, r.started_at, r.finished_at,
	         floor(extract(epoch from r.created_at - $3::timestamptz) / $5::float8)::int as b
	  from runs r
	  join namespaces n on n.name = r.namespace
	  where (r.namespace, r.workflow::text) in (select * from unnest($1::text[], $2::text[]))
	    and r.created_at >= $3 and r.created_at < $4
	    and r.created_at >= now() - make_interval(days => n.max_retention_days)
	)`

// ms is a length of time in whole milliseconds, never below zero: the two instants of a run's
// duration or a task's wait are written by clocks that may disagree by a little, and a wait read
// below zero was none.
func ms(from, to string) string {
	return "greatest(0, extract(epoch from " + to + " - " + from + ") * 1000)"
}

// RunStatistics counts the runs of the workflows among, which the authorizer allowed, bucket by
// bucket. Every bucket is answered, those counting nothing included, oldest first.
func (w *Wide) RunStatistics(ctx context.Context, among []Workflow, b Buckets) ([]RunBucket, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	out := make([]RunBucket, b.Count)
	for i := range out {
		out[i].Runs = make(map[string]int, len(runStates))
		for _, s := range runStates {
			out[i].Runs[s] = 0
		}
	}
	if len(among) == 0 {
		return out, nil
	}
	namespaces, workflows := make([]string, len(among)), make([]string, len(among))
	for i, wf := range among {
		namespaces[i], workflows[i] = wf.Namespace, wf.Name
	}
	args := []any{namespaces, workflows, b.First, b.End(), b.Width.Seconds()}

	// The runs by state. The percentiles are asked apart, over every state of a bucket together,
	// since percentiles taken state by state do not combine into those of the bucket.
	rows, err := w.tx.Query(ctx, `with `+counted+`
		select b, state::text, count(*)
		from counted
		group by b, state`, args...)
	if err != nil {
		return nil, fmt.Errorf("db: the runs could not be counted: %w", err)
	}
	for rows.Next() {
		var at, n int
		var state string
		if err := rows.Scan(&at, &state, &n); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: the runs could not be counted: %w", err)
		}
		if at >= 0 && at < len(out) {
			out[at].Runs[state] += n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: the runs could not be counted: %w", err)
	}

	if err := w.percentiles(ctx, `with `+counted+`
		select b, percentile_cont(array[0.5, 0.95, 0.99]) within group (order by `+ms("started_at", "finished_at")+`)
		from counted
		where started_at is not null and finished_at is not null
		group by b`, args, out, func(r *RunBucket) *Percentiles { return &r.Duration }); err != nil {
		return nil, fmt.Errorf("db: the durations of the runs could not be read: %w", err)
	}
	if err := w.percentiles(ctx, `with `+counted+`
		select c.b, percentile_cont(array[0.5, 0.95, 0.99]) within group (order by `+ms("t.ready_at", "t.dispatched_at")+`)
		from counted c
		join tasks t on t.namespace = c.namespace and t.run_id = c.id
		where t.ready_at is not null and t.dispatched_at is not null
		group by c.b`, args, out, func(r *RunBucket) *Percentiles { return &r.QueueWait }); err != nil {
		return nil, fmt.Errorf("db: the queue waits of the tasks could not be read: %w", err)
	}

	// An attempt was retried where the same shard of the same step has a further attempt. A lost
	// dispatch handed out again is the same attempt, a row of its own under the same key, so an
	// attempt ends as its last dispatch did, and that is the exit code it is counted under.
	rows, err = w.tx.Query(ctx, `with `+counted+`,
		attempts as (
		  select distinct on (t.namespace, t.run_id, t.step, t.shard_index, t.attempt)
		         c.b, t.namespace, t.run_id, t.step, t.shard_index, t.attempt, t.exit_code
		  from counted c
		  join tasks t on t.namespace = c.namespace and t.run_id = c.id
		  order by t.namespace, t.run_id, t.step, t.shard_index, t.attempt, t.requeue desc
		)
		select a.b, a.exit_code, count(*)
		from attempts a
		where exists (select 1 from tasks n
		              where n.namespace = a.namespace and n.run_id = a.run_id and n.step = a.step
		                and n.shard_index is not distinct from a.shard_index and n.attempt = a.attempt + 1)
		group by a.b, a.exit_code
		order by a.b, count(*) desc, a.exit_code nulls last`, args...)
	if err != nil {
		return nil, fmt.Errorf("db: the retried attempts could not be counted: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var at, n int
		var code *int
		if err := rows.Scan(&at, &code, &n); err != nil {
			return nil, fmt.Errorf("db: the retried attempts could not be counted: %w", err)
		}
		if at >= 0 && at < len(out) {
			out[at].Retries = append(out[at].Retries, ExitCodeCount{ExitCode: code, Attempts: n})
		}
	}
	return out, rows.Err()
}

// percentiles reads one row per bucket, the bucket and an array of three percentiles, into the
// Percentiles into answers for each bucket.
func (w *Wide) percentiles(ctx context.Context, query string, args []any, out []RunBucket, into func(*RunBucket) *Percentiles) error {
	rows, err := w.tx.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var at int
		var p []float64
		if err := rows.Scan(&at, &p); err != nil {
			return err
		}
		if at < 0 || at >= len(out) || len(p) != 3 {
			continue
		}
		*into(&out[at]) = Percentiles{P50: round(p[0]), P95: round(p[1]), P99: round(p[2]), Taken: true}
	}
	return rows.Err()
}

func round(f float64) int64 { return int64(math.Round(f)) }

// RunDurations is the histogram of how long the runs of the workflows among that ended took,
// counted over the buckets b spans, in at most bins bins of one width from the shortest to the
// longest; fewer where fewer runs ended, and none where none did.
func (w *Wide) RunDurations(ctx context.Context, among []Workflow, b Buckets, bins int) ([]Bin, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	if bins < 1 {
		return nil, fmt.Errorf("db: a histogram of %d bins", bins)
	}
	if len(among) == 0 {
		return []Bin{}, nil
	}
	namespaces, workflows := make([]string, len(among)), make([]string, len(among))
	for i, wf := range among {
		namespaces[i], workflows[i] = wf.Namespace, wf.Name
	}
	args := []any{namespaces, workflows, b.First, b.End(), b.Width.Seconds()}
	durations := `with ` + counted + `,
		durations as (
		  select ` + ms("started_at", "finished_at") + `::bigint as d
		  from counted
		  where started_at is not null and finished_at is not null
		)`

	var lo, hi *int64
	var ended int
	if err := w.tx.QueryRow(ctx, durations+` select min(d), max(d), count(*) from durations`, args...).Scan(&lo, &hi, &ended); err != nil {
		return nil, fmt.Errorf("db: the durations of the runs could not be read: %w", err)
	}
	if ended == 0 || lo == nil || hi == nil {
		return []Bin{}, nil
	}
	bins = min(bins, ended)
	if *lo == *hi {
		bins = 1
	}
	// The bins span from the shortest to one past the longest, in integers so that where a duration
	// falls and where a bin is said to start never disagree by a rounding: the i-th, from 0, holds
	// the durations d with i <= (d - lo) * bins / (hi + 1 - lo) < i + 1, which start at lo plus
	// i * (hi + 1 - lo) / bins rounded up.
	across := *hi + 1 - *lo
	from := func(i int) int64 { return *lo + (int64(i)*across+int64(bins)-1)/int64(bins) }
	out := make([]Bin, bins)
	for i := range out {
		out[i].From, out[i].Until = from(i), from(i+1)
	}
	out[bins-1].Until = *hi

	rows, err := w.tx.Query(ctx, durations+`
		select (d - $6::bigint) * $8::bigint / ($7::bigint + 1 - $6::bigint), count(*)
		from durations
		group by 1`, append(args, *lo, *hi, bins)...)
	if err != nil {
		return nil, fmt.Errorf("db: the durations of the runs could not be counted: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var at int64
		var n int
		if err := rows.Scan(&at, &n); err != nil {
			return nil, fmt.Errorf("db: the durations of the runs could not be counted: %w", err)
		}
		if at >= 0 && at < int64(bins) {
			out[at].Runs += n
		}
	}
	return out, rows.Err()
}

// StepBucket is what one step came to in the runs created in one bucket.
type StepBucket struct {
	// Attempts are the attempts it was handed out, retries and shards included: a shard of a
	// fan-out is an attempt of its own, and a lost dispatch handed out again is the same one.
	Attempts int

	// Duration is how long the attempts that ended ran, from their last dispatch to their end.
	Duration Percentiles

	// ExitCodes are the attempts that ended by the exit code they ended with, the most frequent
	// first, one with none under a nil code.
	ExitCodes []ExitCodeCount

	// ItemsPerMinute is, for a step that fans out, the items its shards consumed a minute while
	// any of them ran, and nil for any other step or where none ran.
	ItemsPerMinute *float64
}

// attempts is every attempt of the step or steps a series reads, each as its last dispatch left
// it: a lost dispatch handed out again is the same attempt, a row of its own under the same key,
// so an attempt ends as its last dispatch did. dispatched says whether any dispatch of it went
// out, which a requeue not yet handed out has not, and items is what its last dispatch was handed,
// the longest of its input ports, which is what a fan-out cuts on.
const attempts = `
	attempts as (
	  select distinct on (t.namespace, t.run_id, t.step, t.shard_index, t.attempt)
	         c.b, t.step::text as step, t.shard_of, t.state::text as state, t.exit_code,
	         t.dispatched_at, t.finished_at,
	         bool_or(t.dispatched_at is not null)
	           over (partition by t.namespace, t.run_id, t.step, t.shard_index, t.attempt) as dispatched,
	         (select max((i ->> 'items')::bigint)
	            from task_grants g, jsonb_array_elements(coalesce(g.scope -> 'inputs', '[]'::jsonb)) i
	           where g.namespace = t.namespace and g.task_id = t.id) as items
	  from counted c
	  join tasks t on t.namespace = c.namespace and t.run_id = c.id
	  order by t.namespace, t.run_id, t.step, t.shard_index, t.attempt, t.requeue desc
	)`

// ended is the states an attempt has ended in, which a running one and one waiting to be handed
// out again have not.
const ended = `state in ('succeeded', 'failed', 'lost', 'timed_out', 'cancelled')`

// StepStatistics counts the steps of the one workflow of, which the authorizer allowed, bucket by
// bucket, answering each step whose tasks the buckets reach. Every bucket of a step is answered,
// those counting nothing included, oldest first.
func (w *Wide) StepStatistics(ctx context.Context, of Workflow, b Buckets) (map[string][]StepBucket, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	out := map[string][]StepBucket{}
	at := func(step string, i int) *StepBucket {
		if out[step] == nil {
			out[step] = make([]StepBucket, b.Count)
			for j := range out[step] {
				out[step][j].ExitCodes = []ExitCodeCount{}
			}
		}
		return &out[step][i]
	}
	args := []any{[]string{of.Namespace}, []string{of.Name}, b.First, b.End(), b.Width.Seconds()}

	rows, err := w.tx.Query(ctx, `with `+counted+`, `+attempts+`
		select b, step, exit_code, count(*) filter (where dispatched), count(*) filter (where dispatched and `+ended+`)
		from attempts
		group by b, step, exit_code
		order by b, step, count(*) filter (where dispatched and `+ended+`) desc, exit_code nulls last`, args...)
	if err != nil {
		return nil, fmt.Errorf("db: the attempts of the steps could not be counted: %w", err)
	}
	for rows.Next() {
		var i, dispatched, finished int
		var step string
		var code *int
		if err := rows.Scan(&i, &step, &code, &dispatched, &finished); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: the attempts of the steps could not be counted: %w", err)
		}
		if i < 0 || i >= b.Count {
			continue
		}
		s := at(step, i)
		s.Attempts += dispatched
		if finished > 0 {
			s.ExitCodes = append(s.ExitCodes, ExitCodeCount{ExitCode: code, Attempts: finished})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: the attempts of the steps could not be counted: %w", err)
	}

	rows, err = w.tx.Query(ctx, `with `+counted+`, `+attempts+`
		select b, step, percentile_cont(array[0.5, 0.95, 0.99]) within group (order by `+ms("dispatched_at", "finished_at")+`)
		from attempts
		where dispatched and `+ended+` and dispatched_at is not null and finished_at is not null
		group by b, step`, args...)
	if err != nil {
		return nil, fmt.Errorf("db: the durations of the steps could not be read: %w", err)
	}
	for rows.Next() {
		var i int
		var step string
		var p []float64
		if err := rows.Scan(&i, &step, &p); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: the durations of the steps could not be read: %w", err)
		}
		if i >= 0 && i < b.Count && len(p) == 3 {
			at(step, i).Duration = Percentiles{P50: round(p[0]), P95: round(p[1]), P99: round(p[2]), Taken: true}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: the durations of the steps could not be read: %w", err)
	}

	// A fan-out's throughput: the items of the shards that succeeded, over the time any shard of
	// the step ran in the bucket's runs, every dispatch counted, so that a minute two shards ran
	// side by side is one minute and a gap between them is none. One still running runs until now.
	rows, err = w.tx.Query(ctx, `with `+counted+`, `+attempts+`,
		items as (
		  select b, step, sum(items) filter (where state = 'succeeded') as items
		  from attempts
		  where shard_of is not null
		  group by b, step
		),
		ran as (
		  select c.b, t.step::text as step,
		         range_agg(tstzrange(t.started_at, greatest(t.started_at, coalesce(t.finished_at, now())))) as spans
		  from counted c
		  join tasks t on t.namespace = c.namespace and t.run_id = c.id
		  where t.shard_of is not null and t.started_at is not null
		  group by c.b, t.step
		)
		select i.b, i.step, coalesce(i.items, 0),
		       coalesce((select sum(extract(epoch from upper(s) - lower(s))) from unnest(r.spans) s), 0)::float8
		from items i
		join ran r on r.b = i.b and r.step = i.step`, args...)
	if err != nil {
		return nil, fmt.Errorf("db: the throughput of the fan-outs could not be read: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var i int
		var step string
		var items int64
		var seconds float64
		if err := rows.Scan(&i, &step, &items, &seconds); err != nil {
			return nil, fmt.Errorf("db: the throughput of the fan-outs could not be read: %w", err)
		}
		if i < 0 || i >= b.Count || seconds <= 0 {
			continue
		}
		perMinute := math.Round(float64(items)/(seconds/60)*10) / 10
		at(step, i).ItemsPerMinute = &perMinute
	}
	return out, rows.Err()
}

// HoursOfTheWeek is how many cells a heatmap of a week holds, an hour of each of its days.
const HoursOfTheWeek = 7 * 24

// StepHours is the median of how long each step of the one workflow of ran, by the weekday and the
// hour of the day its attempts were dispatched, in UTC, over the runs created from from to to: a
// cell for every hour of the week, Monday at midnight first, and not Taken where none ran.
func (w *Wide) StepHours(ctx context.Context, of Workflow, from, to time.Time) (map[string][]Percentiles, error) {
	if !from.Before(to) {
		return nil, fmt.Errorf("db: a heatmap from %s to %s, which is no range", from, to)
	}
	out := map[string][]Percentiles{}
	// The runs are counted in one bucket spanning the range, which is what counted needs to hand
	// every run a b; nothing here reads it.
	rows, err := w.tx.Query(ctx, `with `+counted+`, `+attempts+`
		select step,
		       extract(isodow from dispatched_at at time zone 'UTC')::int,
		       extract(hour from dispatched_at at time zone 'UTC')::int,
		       percentile_cont(0.5) within group (order by `+ms("dispatched_at", "finished_at")+`)
		from attempts
		where dispatched and `+ended+` and dispatched_at is not null and finished_at is not null
		group by 1, 2, 3`,
		[]string{of.Namespace}, []string{of.Name}, from, to, to.Sub(from).Seconds())
	if err != nil {
		return nil, fmt.Errorf("db: the steps' hours could not be read: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var step string
		var weekday, hour int
		var p50 float64
		if err := rows.Scan(&step, &weekday, &hour, &p50); err != nil {
			return nil, fmt.Errorf("db: the steps' hours could not be read: %w", err)
		}
		if weekday < 1 || weekday > 7 || hour < 0 || hour > 23 {
			continue
		}
		if out[step] == nil {
			out[step] = make([]Percentiles, HoursOfTheWeek)
		}
		out[step][(weekday-1)*24+hour] = Percentiles{P50: round(p50), Taken: true}
	}
	return out, rows.Err()
}

// PortStatistics counts what each step of the one workflow of published, port by port, bucket by
// bucket: the items of the envelope each port published, which the step's row records with its
// digest, from the envelope's meta.count and never its items. A port that published in no run of a
// bucket is absent from it; every bucket of a step that published anywhere is answered.
func (w *Wide) PortStatistics(ctx context.Context, of Workflow, b Buckets) (map[string][]map[string]int64, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	rows, err := w.tx.Query(ctx, `with `+counted+`
		select c.b, s.step::text, p.key, sum(coalesce((p.value ->> 'items')::bigint, 0))
		from counted c
		join steps s on s.namespace = c.namespace and s.run_id = c.id
		cross join lateral jsonb_each(s.ports) p
		group by 1, 2, 3`,
		[]string{of.Namespace}, []string{of.Name}, b.First, b.End(), b.Width.Seconds())
	if err != nil {
		return nil, fmt.Errorf("db: what the steps published could not be counted: %w", err)
	}
	defer rows.Close()
	out := map[string][]map[string]int64{}
	for rows.Next() {
		var i int
		var step, port string
		var items int64
		if err := rows.Scan(&i, &step, &port, &items); err != nil {
			return nil, fmt.Errorf("db: what the steps published could not be counted: %w", err)
		}
		if i < 0 || i >= b.Count {
			continue
		}
		if out[step] == nil {
			out[step] = make([]map[string]int64, b.Count)
			for j := range out[step] {
				out[step][j] = map[string]int64{}
			}
		}
		out[step][i][port] = items
	}
	return out, rows.Err()
}

// QuotaBucket is what a namespace asked of its quotas in one bucket.
type QuotaBucket struct {
	// RunsCreated are the runs created in the bucket, against max_runs_per_hour, and RunsRefused
	// those refused for it, whatever asked for them: see migration 0064.
	RunsCreated, RunsRefused int

	// TasksInFlightMax is the most tasks on their way to a runner or at one and not yet ended at
	// once in the bucket, which is what max_concurrent_tasks counts.
	TasksInFlightMax int

	// ArtifactBytes is what max_artifact_bytes counted at the bucket's end: the bytes of the live
	// artifacts, each digest once. ArtifactBytesAdded is the bytes of the artifacts written in the
	// bucket, each digest once.
	ArtifactBytes, ArtifactBytesAdded int64
}

// QuotaStatistics counts what the namespace asked of its quotas, bucket by bucket, every bucket
// answered, oldest first. A bucket that ended before the namespace's max_retention_days counts
// nothing, as a series of its runs does: "a range reaches back as far as the namespace keeps its
// runs, and no further".
//
// Read through the namespace's own handle, since what it counts is every workflow of the namespace
// alike, as a quota bounds them, and whoever reads the namespace reads it.
func (n *NS) QuotaStatistics(ctx context.Context, b Buckets) ([]QuotaBucket, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	w, namespace := n, n.namespace
	out := make([]QuotaBucket, b.Count)
	var kept time.Time
	if err := w.tx.QueryRow(ctx,
		`select now() - make_interval(days => max_retention_days) from namespaces where name = $1`,
		namespace).Scan(&kept); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return out, nil
		}
		return nil, fmt.Errorf("db: the retention of namespace %s could not be read: %w", namespace, err)
	}
	first := b.First
	if kept.After(first) {
		first = kept
	}
	args := []any{namespace, b.First, b.End(), b.Width.Seconds(), first}
	bucket := func(at string) string {
		return "floor(extract(epoch from " + at + " - $2::timestamptz) / $4::float8)::int"
	}
	each := func(what, query string, into func(i int, v int64)) error {
		rows, err := w.tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("db: %s could not be counted: %w", what, err)
		}
		defer rows.Close()
		for rows.Next() {
			var i int
			var v int64
			if err := rows.Scan(&i, &v); err != nil {
				return fmt.Errorf("db: %s could not be counted: %w", what, err)
			}
			if i >= 0 && i < b.Count {
				into(i, v)
			}
		}
		return rows.Err()
	}

	if err := each("the runs created", `
		select `+bucket("created_at")+`, count(*) from runs
		where namespace = $1 and created_at >= $5 and created_at < $3
		group by 1`, func(i int, v int64) { out[i].RunsCreated = int(v) }); err != nil {
		return nil, err
	}
	if err := each("the runs refused", `
		select `+bucket("minute")+`, sum(refused) from run_refusals
		where namespace = $1 and minute >= $5 and minute < $3
		group by 1`, func(i int, v int64) { out[i].RunsRefused = int(v) }); err != nil {
		return nil, err
	}
	if err := each("the artifact bytes written", `
		select `+bucket("created_at")+`, sum(bytes) from (
		  select min(created_at) as created_at, max(size_bytes) as bytes from artifacts
		  where namespace = $1 and created_at >= $5 and created_at < $3
		  group by digest, `+bucket("created_at")+`
		) written
		group by 1`, func(i int, v int64) { out[i].ArtifactBytesAdded = v }); err != nil {
		return nil, err
	}
	// Live at the end of each bucket, as max_artifact_bytes counted it then: written by then, not
	// yet expired, and not yet retired, each digest once.
	if err := each("the artifact bytes held", `
		select e.i, coalesce((
		  select sum(bytes) from (
		    select max(a.size_bytes) as bytes from artifacts a
		    where a.namespace = $1 and a.created_at < e.at and a.expires_at > e.at
		      and (a.retired_at is null or a.retired_at > e.at)
		    group by a.digest) held), 0)::bigint
		from (select i, $2::timestamptz + make_interval(secs => (i + 1) * $4::float8) as at
		      from generate_series(0, `+fmt.Sprint(b.Count-1)+`) i) e
		where e.at > $5 and e.at <= $3::timestamptz`, func(i int, v int64) { out[i].ArtifactBytes = v }); err != nil {
		return nil, err
	}

	// The tasks in flight, from when each was handed to the bus to when it ended, or until now for
	// one that has not: a level that rises and falls at those instants, read at each bucket's
	// highest and carried into the next from where the bucket before left it. Instants are merged,
	// so that a task ending as another starts is not two at once.
	rows, err := w.tx.Query(ctx, `
		with spans as (
		  select greatest(published_at, $5::timestamptz) as began,
		         coalesce(finished_at, case when state in ('succeeded', 'failed', 'lost', 'timed_out', 'cancelled')
		                                    then published_at else now() end) as ended
		  from tasks
		  where namespace = $1 and published_at is not null and published_at < $3
		),
		changes as (
		  select at, sum(d) as d from (
		    select began as at, 1 as d from spans where ended > began
		    union all
		    select ended, -1 from spans where ended > began and ended < $3
		  ) c
		  group by at
		),
		levels as (
		  select at, sum(d) over (order by at) as level from changes
		)
		select `+bucket("at")+`, max(level), (array_agg(level order by at desc))[1]
		from levels
		where at >= $5
		group by 1
		order by 1`, args...)
	if err != nil {
		return nil, fmt.Errorf("db: the tasks in flight could not be counted: %w", err)
	}
	defer rows.Close()
	type read struct{ high, last int }
	levels := map[int]read{}
	for rows.Next() {
		var i int
		var high, last int64
		if err := rows.Scan(&i, &high, &last); err != nil {
			return nil, fmt.Errorf("db: the tasks in flight could not be counted: %w", err)
		}
		levels[i] = read{int(high), int(last)}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: the tasks in flight could not be counted: %w", err)
	}
	carried := 0
	for i := range out {
		if !b.First.Add(time.Duration(i+1) * b.Width).After(first) {
			continue
		}
		out[i].TasksInFlightMax = carried
		if l, changed := levels[i]; changed {
			out[i].TasksInFlightMax = max(carried, l.high)
			carried = l.last
		}
	}
	return out, nil
}

// SlotBucket is what a runner, or a pool's runners together, held and offered in one bucket: the
// most tasks at once it held, and what it offered at the bucket's end.
type SlotBucket struct {
	InUseMax int
	Capacity int64
}

// RunnerSilence is one silence between two heartbeats of a runner of Silence or more: from the
// last heartbeat before it, lasting until the next, or until the end of the range for one still
// going, and the tasks of the runner the sweep declared lost in it.
type RunnerSilence struct {
	At        time.Time
	Length    time.Duration
	TasksLost int
}

// RunnerSeries is one runner's slots, bucket by bucket, and its silences over the range.
type RunnerSeries struct {
	Runner   string
	Buckets  []SlotBucket
	Silences []RunnerSilence
}

// PoolSeries is one pool's slots, its runners' together, bucket by bucket, and each runner's.
type PoolSeries struct {
	Pool    string
	Buckets []SlotBucket
	Runners []RunnerSeries
}

// lostAfter is LostAfter as an interval SQL reads: how long a runner is silent before the sweep
// declares its tasks lost, and from which it is counted as offering nothing.
var lostAfter = fmt.Sprintf("%d seconds", int(LostAfter/time.Second))

// PoolStatistics answers every pool, by name, with its runners, by name, bucket by bucket: those
// that were in it at any time over the range, whether or not they are there now. A task holds a
// slot of its runner from when the runner redeemed it, or started it where no redemption is
// recorded, to when it ended, or until now for one that has not. A runner offers what it last
// wrote at the bucket's end, or nothing once it has been silent for lostAfter.
func (w *Wide) PoolStatistics(ctx context.Context, b Buckets) ([]PoolSeries, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	zero := func() []SlotBucket { return make([]SlotBucket, b.Count) }
	var pools []PoolSeries
	at := map[string]int{}
	rows, err := w.tx.Query(ctx, `select name from runner_pools order by name`)
	if err != nil {
		return nil, fmt.Errorf("db: the pools could not be read: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: the pools could not be read: %w", err)
		}
		at[name] = len(pools)
		pools = append(pools, PoolSeries{Pool: name, Buckets: zero(), Runners: []RunnerSeries{}})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: the pools could not be read: %w", err)
	}

	args := []any{b.First, b.End(), b.Width.Seconds()}
	// The runners that were in a pool at any time over the range: joined before its end and heard,
	// or joined, within lostAfter of its start or later.
	rows, err = w.tx.Query(ctx, `
		select id, pool from runners
		where joined_at < $2
		  and coalesce(last_heartbeat_at, joined_at) > $1::timestamptz - interval '`+lostAfter+`'
		order by pool, id`, b.First, b.End())
	if err != nil {
		return nil, fmt.Errorf("db: the runners could not be read: %w", err)
	}
	where := map[string][2]int{}
	var runners []string
	for rows.Next() {
		var id, pool string
		if err := rows.Scan(&id, &pool); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: the runners could not be read: %w", err)
		}
		p, ok := at[pool]
		if !ok {
			continue
		}
		where[id] = [2]int{p, len(pools[p].Runners)}
		pools[p].Runners = append(pools[p].Runners, RunnerSeries{Runner: id, Buckets: zero(), Silences: []RunnerSilence{}})
		runners = append(runners, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: the runners could not be read: %w", err)
	}
	if len(runners) == 0 {
		return pools, nil
	}
	runner := func(id string) *RunnerSeries {
		w := where[id]
		return &pools[w[0]].Runners[w[1]]
	}

	// The slots held, as a level per runner and per pool that rises and falls where a task is taken
	// and ends, read at each bucket's highest and carried into the next from where the bucket
	// before left it. Instants are merged, so that a task ending as another is taken is not two.
	rows, err = w.tx.Query(ctx, `
		with held as (
		  select t.runner, r.pool,
		         greatest(coalesce((select min(g.redeemed_at) from task_grants g
		                             where g.namespace = t.namespace and g.task_id = t.id),
		                           t.started_at, t.dispatched_at), $1::timestamptz) as began,
		         coalesce(t.finished_at,
		                  case when t.state in ('succeeded', 'failed', 'lost', 'timed_out', 'cancelled')
		                       then coalesce(t.started_at, t.dispatched_at) else now() end) as ended
		  from tasks t
		  join runners r on r.id = t.runner
		  where t.runner = any($4) and coalesce(t.started_at, t.dispatched_at) < $2
		),
		changes as (
		  select grp, at, sum(d) as d from (
		    select 'r' || runner as grp, began as at, 1 as d from held where ended > began
		    union all
		    select 'r' || runner, ended, -1 from held where ended > began and ended < $2
		    union all
		    select 'p' || pool, began, 1 from held where ended > began
		    union all
		    select 'p' || pool, ended, -1 from held where ended > began and ended < $2
		  ) c
		  group by grp, at
		),
		levels as (
		  select grp, at, sum(d) over (partition by grp order by at) as level from changes
		)
		select grp, floor(extract(epoch from at - $1::timestamptz) / $3::float8)::int,
		       max(level), (array_agg(level order by at desc))[1]
		from levels
		group by 1, 2
		order by 1, 2`, append(args, runners)...)
	if err != nil {
		return nil, fmt.Errorf("db: the slots held could not be counted: %w", err)
	}
	type read struct{ high, last int }
	levels := map[string]map[int]read{}
	for rows.Next() {
		var grp string
		var i int
		var high, last int64
		if err := rows.Scan(&grp, &i, &high, &last); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: the slots held could not be counted: %w", err)
		}
		if levels[grp] == nil {
			levels[grp] = map[int]read{}
		}
		levels[grp][i] = read{int(high), int(last)}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: the slots held could not be counted: %w", err)
	}
	carry := func(grp string, into []SlotBucket) {
		carried := 0
		for i := range into {
			into[i].InUseMax = carried
			if l, changed := levels[grp][i]; changed {
				into[i].InUseMax, carried = max(carried, l.high), l.last
			}
		}
	}
	for p := range pools {
		carry("p"+pools[p].Pool, pools[p].Buckets)
		for r := range pools[p].Runners {
			carry("r"+pools[p].Runners[r].Runner, pools[p].Runners[r].Buckets)
		}
	}

	// What each runner offered at each bucket's end, or at now for a bucket that has not ended.
	rows, err = w.tx.Query(ctx, `
		select r.id, e.i,
		       case when r.joined_at > e.at then 0
		            when exists (select 1 from runner_silences s
		                         where s.runner = r.id and e.at >= s.began + interval '`+lostAfter+`' and e.at < s.ended) then 0
		            when r.last_heartbeat_at is not null and e.at >= r.last_heartbeat_at + interval '`+lostAfter+`' then 0
		            else coalesce((select c.capacity from runner_capacity c
		                           where c.runner = r.id and c.at <= e.at order by c.at desc limit 1), 0)
		       end
		from runners r
		cross join lateral (
		  select i, least($1::timestamptz + make_interval(secs => (i + 1) * $3::float8), now()) as at
		  from generate_series(0, `+fmt.Sprint(b.Count-1)+`) i
		) e
		where r.id = any($4) and e.at <= $2::timestamptz`, append(args, runners)...)
	if err != nil {
		return nil, fmt.Errorf("db: what the runners offered could not be read: %w", err)
	}
	for rows.Next() {
		var id string
		var i int
		var capacity int64
		if err := rows.Scan(&id, &i, &capacity); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: what the runners offered could not be read: %w", err)
		}
		if i < 0 || i >= b.Count {
			continue
		}
		runner(id).Buckets[i].Capacity = capacity
		pools[where[id][0]].Buckets[i].Capacity += capacity
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: what the runners offered could not be read: %w", err)
	}

	// The silences that began over the range, those written and the one still going, with the
	// runner's tasks the sweep declared lost in each.
	rows, err = w.tx.Query(ctx, `
		with silences as (
		  select runner, began, ended from runner_silences
		  where runner = any($3) and began >= $1 and began < $2
		  union all
		  select id, last_heartbeat_at, least($2::timestamptz, now()) from runners
		  where id = any($3) and last_heartbeat_at >= $1 and last_heartbeat_at < $2
		    and least($2::timestamptz, now()) >= last_heartbeat_at + interval '`+fmt.Sprintf("%d seconds", int(Silence/time.Second))+`'
		)
		select s.runner, s.began, s.ended,
		       (select count(*) from tasks t
		         where t.runner = s.runner and t.state = 'lost' and t.finished_at >= s.began and t.finished_at <= s.ended)
		from silences s
		order by s.runner, s.began`, b.First, b.End(), runners)
	if err != nil {
		return nil, fmt.Errorf("db: the runners' silences could not be read: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var began, ended time.Time
		var lost int
		if err := rows.Scan(&id, &began, &ended, &lost); err != nil {
			return nil, fmt.Errorf("db: the runners' silences could not be read: %w", err)
		}
		r := runner(id)
		r.Silences = append(r.Silences, RunnerSilence{At: began, Length: ended.Sub(began), TasksLost: lost})
	}
	return pools, rows.Err()
}
