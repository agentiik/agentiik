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
