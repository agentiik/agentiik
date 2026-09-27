package db

import (
	"context"
	"fmt"
	"time"
)

// Occupancy is one runner as the metrics read it: how many tasks it said it runs at once, whether
// it takes new work, and how many dispatches bound to it are in flight.
type Occupancy struct {
	Runner string
	Pool   string

	// Slots is the concurrency it last reported at a heartbeat.
	Slots int64

	// Ready says it takes new work: the installation has neither drained nor revoked it, and it
	// last said ready rather than draining or unhealthy. A runner that is not still finishes what
	// it holds, so its tasks are counted all the same.
	Ready bool

	// Held is the dispatches bound to it by a redemption and not yet ended.
	Held int
}

// Occupancy reads every runner heard from within LostAfter of now, in pool and runner order.
//
// Only those, because a runner silent for longer holds nothing any more as far as the controller is
// concerned, its tasks being declared lost, and one that left for good would otherwise be reported
// for ever. So the gauges built from this hold the runners that exist at the moment of the scrape,
// however many have come and gone, which is what bounds them.
//
// The tasks are counted once for every runner, from the in-flight rows alone, which the partial
// index tasks_held covers: a scrape reads what is running, never the history.
func (w *Wide) Occupancy(ctx context.Context, now time.Time) ([]Occupancy, error) {
	rows, err := w.tx.Query(ctx, `
		with held as (
		  select t.runner, count(*)::int as n from tasks t
		  where t.state in ('dispatched', 'running', 'publishing') and t.runner is not null
		  group by t.runner
		)
		select r.id, r.pool, coalesce(r.concurrency, 0),
		       r.state = 'ready' and r.reported_state is not distinct from 'ready',
		       coalesce(h.n, 0)
		from runners r left join held h on h.runner = r.id
		where r.last_heartbeat_at >= $1
		order by r.pool, r.id`, now.Add(-LostAfter))
	if err != nil {
		return nil, fmt.Errorf("db: the runners' occupancy could not be read: %w", err)
	}
	defer rows.Close()
	var out []Occupancy
	for rows.Next() {
		var o Occupancy
		if err := rows.Scan(&o.Runner, &o.Pool, &o.Slots, &o.Ready, &o.Held); err != nil {
			return nil, fmt.Errorf("db: the runners' occupancy could not be read: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Consumption is one namespace as the quota metrics read it: what it holds against each quota that
// counts something, and the quota where it sets one.
type Consumption struct {
	Namespace string

	// Tasks is what max_concurrent_tasks counts, as Slots counts it: tasks handed out, or on
	// their way to a runner, and not yet over.
	Tasks int64

	// RunsLastHour is what max_runs_per_hour counts: the runs created in the last 60 minutes.
	RunsLastHour int64

	// ArtifactBytes is what max_artifact_bytes counts, as MakeRoom counts it: the bytes of its
	// live artifacts, each digest once, and of the uploads not yet referenced.
	ArtifactBytes int64

	// The quotas, zero where the namespace sets none. max_concurrent_tasks always holds one.
	MaxConcurrentTasks int64
	MaxRunsPerHour     int64
	MaxArtifactBytes   int64
}

// Consumption reads every namespace, in name order, with what it holds against its quotas.
//
// Each count is read namespace by namespace, through the index its quota is counted with where it
// is enforced, so a scrape reads what is in flight, the last hour's runs and the live artifacts,
// never the history.
func (w *Wide) Consumption(ctx context.Context) ([]Consumption, error) {
	rows, err := w.tx.Query(ctx, `
		select n.name, t.n, r.n, b.bytes,
		       n.max_concurrent_tasks, coalesce(n.max_runs_per_hour, 0), coalesce(n.max_artifact_bytes, 0)
		from namespaces n
		cross join lateral (
		  select count(*) as n from tasks
		  where namespace = n.name and state in ('pending', 'dispatched', 'running', 'publishing')
		    and published_at is not null) t
		cross join lateral (
		  select count(*) as n from runs
		  where namespace = n.name and created_at > now() - interval '60 minutes') r
		cross join lateral (
		  select coalesce(sum(bytes), 0)::bigint as bytes
		  from (`+heldBytes(` and namespace = n.name`, ` and namespace = n.name`)+`) h) b
		order by n.name`)
	if err != nil {
		return nil, fmt.Errorf("db: what the namespaces hold against their quotas could not be read: %w", err)
	}
	defer rows.Close()
	var out []Consumption
	for rows.Next() {
		var c Consumption
		if err := rows.Scan(&c.Namespace, &c.Tasks, &c.RunsLastHour, &c.ArtifactBytes,
			&c.MaxConcurrentTasks, &c.MaxRunsPerHour, &c.MaxArtifactBytes); err != nil {
			return nil, fmt.Errorf("db: what the namespaces hold against their quotas could not be read: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
