package db

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Two namespaces' runs and tasks laid out from two hours back, as the installation's activity reads
// them: the runs created by state, every namespace together, the most tasks in flight at once, and
// what runs now beside the slots the ready runners offer.
//
// The seed's two runs, created now and queued, are what runs now. Laid out from t0: in finance a
// run created at 10 s that succeeded and one at 70 s that failed; in team-ops one at 20 s that is
// running. Tasks in flight: finance's from 5 to 50 and from 30 to 65, team-ops' from 40 still going,
// so three at once in the first minute, two carried into the second, and one into the third. A
// runner of lan joins and reports four slots, which it offers while heard from.
func TestTheInstallationsActivityCountsEveryNamespaceTogether(t *testing.T) {
	pool, super := joining(t)
	t0 := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	at := func(seconds int) time.Time { return t0.Add(time.Duration(seconds) * time.Second) }

	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	for i, r := range []struct {
		namespace, workflow, commit, state string
		created                            int
	}{
		{"finance", "monthly-invoicing", "a3f9c1e", "succeeded", 10},
		{"team-ops", "nightly", "b1c2d3e", "running", 20},
		{"finance", "monthly-invoicing", "a3f9c1e", "failed", 70},
	} {
		if _, err := conn.Exec(t.Context(), `
			insert into runs (namespace, id, workflow, commit, trigger, state, created_at)
			values ($1, $2, $3, $4, 'manual', $5, $6)`,
			r.namespace, fmt.Sprintf("01M2R%021d", i), r.workflow, r.commit, r.state, at(r.created)); err != nil {
			t.Fatal(err)
		}
	}
	for i, task := range []struct {
		namespace, run, state string
		published, ended      int
	}{
		{"finance", financeRun, "succeeded", 5, 50},
		{"finance", financeRun, "failed", 30, 65},
		{"team-ops", opsRun, "running", 40, -1},
	} {
		var finished any
		if task.ended >= 0 {
			finished = at(task.ended)
		}
		if _, err := conn.Exec(t.Context(), `
			insert into tasks (namespace, id, run_id, step, attempt, state, published_at, dispatched_at, finished_at)
			values ($1, $2, $3, 'archive', $4, $5, $6, $6, $7)`,
			task.namespace, fmt.Sprintf("01M2T%021d", i), task.run, i+2, task.state, at(task.published), finished); err != nil {
			t.Fatal(err)
		}
	}

	var runner string
	if err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		issued, err := w.IssueJoinToken(ctx, "lan", nil, "admin", time.Now(), time.Now().Add(time.Hour))
		if err != nil {
			return err
		}
		joined, err := w.Join(ctx, Joining{
			Token: issued.Clear, PublicKey: hostKey(9), CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
			Architecture: "amd64", AgentVersion: "0.2.0",
		}, time.Hour, time.Now())
		runner = joined.Runner
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.Installation(t.Context(), Heartbeat, func(ctx context.Context, w *Wide) error {
		_, err := w.Beat(ctx, runner, beating(), time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var got []ActivityBucket
	var now Activity
	if err := pool.Installation(t.Context(), InstallationActivity, func(ctx context.Context, w *Wide) error {
		got, now, err = w.ActivityStatistics(ctx, Buckets{First: t0, Width: time.Minute, Count: 3})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	runs := func(by map[string]int) map[string]int {
		all := map[string]int{"queued": 0, "running": 0, "waiting": 0, "succeeded": 0, "failed": 0, "cancelled": 0, "timed_out": 0}
		for s, n := range by {
			all[s] = n
		}
		return all
	}
	want := []ActivityBucket{
		{Runs: runs(map[string]int{"succeeded": 1, "running": 1}), TasksInFlightMax: 3},
		{Runs: runs(map[string]int{"failed": 1}), TasksInFlightMax: 2},
		{Runs: runs(nil), TasksInFlightMax: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the activity counted\n%+v\nwant\n%+v", got, want)
	}

	if !reflect.DeepEqual(now.Runs, map[string]int{"queued": 2, "running": 1, "waiting": 0}) {
		t.Errorf("the runs not ended read %v, want the seed's two queued and team-ops' running", now.Runs)
	}
	if now.TasksInFlight != 1 || now.Slots != 4 || now.RunnersReady != 1 || now.Runners != 1 {
		t.Errorf("now read %d tasks in flight, %d slots of %d runners ready of %d, want 1, 4, 1 and 1", now.TasksInFlight, now.Slots, now.RunnersReady, now.Runners)
	}
	if time.Since(now.At) > time.Minute {
		t.Errorf("now was read at %s", now.At)
	}

	// A runner drained offers nothing and is no longer ready, and is counted all the same.
	if err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		_, err := w.Drain(ctx, runner, "admin", "the host is being replaced", time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.Installation(t.Context(), InstallationActivity, func(ctx context.Context, w *Wide) error {
		_, now, err = w.ActivityStatistics(ctx, Buckets{First: t0, Width: time.Minute, Count: 3})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if now.Slots != 0 || now.RunnersReady != 0 || now.Runners != 1 {
		t.Errorf("a drained runner read %d slots, %d ready of %d", now.Slots, now.RunnersReady, now.Runners)
	}
}
