package db

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
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
