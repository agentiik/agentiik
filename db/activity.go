package db

import (
	"context"
	"fmt"
	"time"
)

// ActivityBucket is what every namespace together did in one bucket, in counts that name no run,
// workflow or namespace.
type ActivityBucket struct {
	// Runs are the runs created in the bucket, in every namespace, by state, as RunBucket's are:
	// an ended run under its final state, one still going under where it is now.
	Runs map[string]int

	// TasksInFlightMax is the most tasks on their way to a runner or at one and not yet ended at
	// once in the bucket, every namespace together, as QuotaBucket's is for one.
	TasksInFlightMax int
}

// Activity is what the installation is doing as it is read.
type Activity struct {
	// At is when it was read, the database's clock.
	At time.Time

	// Runs are the runs not ended, by state: queued, running and waiting.
	Runs map[string]int

	// TasksInFlight are the tasks handed to the bus and not yet ended.
	TasksInFlight int

	// Slots is what the ready runners offer together, and RunnersReady how many they are: ready,
	// reporting themselves ready and heard from within LostAfter, which is what runner_capacity
	// counts as capacity. Runners is the runners not revoked, ready or not.
	Slots                 int64
	RunnersReady, Runners int
}

// unended are the states of a run that has not ended.
var unended = []string{"queued", "running", "waiting"}

// ActivityStatistics counts what every namespace did, bucket by bucket, every bucket answered,
// oldest first, and reads what the installation is doing now. A namespace's runs and tasks count
// within its retention, as in every series, so that the installation's figures add up to its
// namespaces'.
func (w *Wide) ActivityStatistics(ctx context.Context, b Buckets) ([]ActivityBucket, Activity, error) {
	if err := b.check(); err != nil {
		return nil, Activity{}, err
	}
	out := make([]ActivityBucket, b.Count)
	for i := range out {
		out[i].Runs = make(map[string]int, len(runStates))
		for _, s := range runStates {
			out[i].Runs[s] = 0
		}
	}
	args := []any{b.First, b.End(), b.Width.Seconds()}
	bucket := func(at string) string {
		return "floor(extract(epoch from " + at + " - $1::timestamptz) / $3::float8)::int"
	}

	rows, err := w.tx.Query(ctx, `
		select `+bucket("r.created_at")+`, r.state::text, count(*)
		from runs r
		join namespaces n on n.name = r.namespace
		where r.created_at >= $1 and r.created_at < $2
		  and r.created_at >= now() - make_interval(days => n.max_retention_days)
		group by 1, 2`, args...)
	if err != nil {
		return nil, Activity{}, fmt.Errorf("db: the runs could not be counted: %w", err)
	}
	for rows.Next() {
		var i, n int
		var state string
		if err := rows.Scan(&i, &state, &n); err != nil {
			rows.Close()
			return nil, Activity{}, fmt.Errorf("db: the runs could not be counted: %w", err)
		}
		if i >= 0 && i < b.Count {
			out[i].Runs[state] += n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, Activity{}, fmt.Errorf("db: the runs could not be counted: %w", err)
	}

	// The tasks in flight, as QuotaStatistics counts one namespace's: a level that rises where a
	// task is handed to the bus and falls where it ends, read at each bucket's highest and carried
	// into the next. A task is counted from the range's start or its namespace's retention,
	// whichever is later, so every instant counted falls in the range and the level starts at 0.
	rows, err = w.tx.Query(ctx, `
		with spans as (
		  select greatest(t.published_at, $1::timestamptz, now() - make_interval(days => n.max_retention_days)) as began,
		         coalesce(t.finished_at, case when t.state in ('succeeded', 'failed', 'lost', 'timed_out', 'cancelled')
		                                      then t.published_at else now() end) as ended
		  from tasks t
		  join namespaces n on n.name = t.namespace
		  where t.published_at is not null and t.published_at < $2
		    and (t.finished_at is null or t.finished_at > $1)
		),
		changes as (
		  select at, sum(d) as d from (
		    select began as at, 1 as d from spans where ended > began
		    union all
		    select ended, -1 from spans where ended > began and ended < $2
		  ) c
		  group by at
		),
		levels as (
		  select at, sum(d) over (order by at) as level from changes
		)
		select `+bucket("at")+`, max(level), (array_agg(level order by at desc))[1]
		from levels
		group by 1
		order by 1`, args...)
	if err != nil {
		return nil, Activity{}, fmt.Errorf("db: the tasks in flight could not be counted: %w", err)
	}
	type read struct{ high, last int }
	levels := map[int]read{}
	for rows.Next() {
		var i int
		var high, last int64
		if err := rows.Scan(&i, &high, &last); err != nil {
			rows.Close()
			return nil, Activity{}, fmt.Errorf("db: the tasks in flight could not be counted: %w", err)
		}
		levels[i] = read{int(high), int(last)}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, Activity{}, fmt.Errorf("db: the tasks in flight could not be counted: %w", err)
	}
	carried := 0
	for i := range out {
		out[i].TasksInFlightMax = carried
		if l, changed := levels[i]; changed {
			out[i].TasksInFlightMax = max(carried, l.high)
			carried = l.last
		}
	}

	now := Activity{Runs: make(map[string]int, len(unended))}
	for _, s := range unended {
		now.Runs[s] = 0
	}
	if err := w.tx.QueryRow(ctx, `
		select now(),
		       (select count(*) from tasks
		         where published_at is not null and finished_at is null
		           and state not in ('succeeded', 'failed', 'lost', 'timed_out', 'cancelled')),
		       coalesce(sum(coalesce(r.concurrency, 0)) filter (where ready), 0),
		       count(*) filter (where ready),
		       count(*) filter (where r.state <> 'revoked')
		from (select 1) one
		left join lateral (
		  select state, concurrency,
		         state = 'ready' and coalesce(reported_state, 'ready') = 'ready'
		           and coalesce(last_heartbeat_at, joined_at) > now() - interval '`+lostAfter+`' as ready
		  from runners
		) r on true`).Scan(&now.At, &now.TasksInFlight, &now.Slots, &now.RunnersReady, &now.Runners); err != nil {
		return nil, Activity{}, fmt.Errorf("db: what the installation is doing could not be read: %w", err)
	}
	rows, err = w.tx.Query(ctx, `select state::text, count(*) from runs where state = any($1) group by 1`, unended)
	if err != nil {
		return nil, Activity{}, fmt.Errorf("db: the runs not ended could not be counted: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, Activity{}, fmt.Errorf("db: the runs not ended could not be counted: %w", err)
		}
		now.Runs[state] = n
	}
	return out, now, rows.Err()
}
