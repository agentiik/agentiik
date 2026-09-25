package db

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The occupancy the metrics read, against a scripted fleet: a runner of each kind the gauges have
// to tell apart, and tasks in every state a runner may be bound to them in.
func TestOccupancyReadsTheRunnersHeardFromAndWhatTheyHold(t *testing.T) {
	pool, super := joining(t)
	now := time.Now().UTC()

	// Four machines: busy on lan, draining on dmz, and two on lan the scrape does not see, one
	// that never reported and one that went quiet.
	join := func(w *Wide, ctx context.Context, p string, seed byte) (string, error) {
		issued, err := w.IssueJoinToken(ctx, p, nil, "admin", now, now.Add(time.Hour))
		if err != nil {
			return "", err
		}
		joined, err := w.Join(ctx, Joining{
			Token: issued.Clear, PublicKey: hostKey(seed), CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
			Architecture: "amd64", AgentVersion: "0.2.0",
		}, time.Hour, now)
		return joined.Runner, err
	}
	var busy, draining, silent, quiet string
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		var err error
		if busy, err = join(w, ctx, "lan", 1); err != nil {
			return err
		}
		if draining, err = join(w, ctx, "dmz", 2); err != nil {
			return err
		}
		if silent, err = join(w, ctx, "lan", 3); err != nil {
			return err
		}
		quiet, err = join(w, ctx, "lan", 4)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = pool.Installation(t.Context(), Heartbeat, func(ctx context.Context, w *Wide) error {
		for _, r := range []string{busy, draining, quiet} {
			b := beating()
			if r == draining {
				b.Concurrency = 2
			}
			if _, err := w.Beat(ctx, r, b, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		_, err := w.Drain(ctx, draining, "admin", "kernel upgrade", now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(), `update runners set last_heartbeat_at = $2 where id = $1`,
		quiet, now.Add(-LostAfter-time.Second)); err != nil {
		t.Fatal(err)
	}

	// Tasks: three in flight on busy, one of them publishing, and one it has ended; one
	// publishing on draining; one in flight on quiet; and one dispatched that nobody redeemed.
	for i, task := range []struct {
		runner, state string
	}{
		{busy, "dispatched"}, {busy, "running"}, {busy, "publishing"}, {busy, "succeeded"},
		{draining, "publishing"}, {quiet, "running"}, {"", "dispatched"},
	} {
		var runner any
		if task.runner != "" {
			runner = task.runner
		}
		if _, err := conn.Exec(t.Context(), `
			insert into tasks (namespace, id, run_id, step, attempt, shard_index, shard_of,
			                   state, runner, dispatched_at)
			values ('finance', $1, $2, 'render', 1, $3, 7, $4, $5, now())`,
			fmt.Sprintf("01M2HQCC%018d", i), financeRun, i+1, task.state, runner); err != nil {
			t.Fatal(err)
		}
	}

	var got []Occupancy
	err = pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		got, err = w.Occupancy(ctx, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Occupancy{
		{Runner: draining, Pool: "dmz", Slots: 2, Ready: false, Held: 1},
		{Runner: busy, Pool: "lan", Slots: 4, Ready: true, Held: 3},
	}
	if !slices.Equal(got, want) {
		t.Errorf("the occupancy reads\n%+v\nwant\n%+v, and neither %s, which never reported, nor %s, silent past the loss bound, is in it", got, want, silent, quiet)
	}
}
