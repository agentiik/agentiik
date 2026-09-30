package db

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A runner's heartbeats and tasks, laid out from two hours back, as the chart of the pools reads
// them: its slots held and offered bucket by bucket, and its silences.
//
// It joins lan and reports ready with four slots at 5 seconds, then at 15, then not until 40, a
// silence of 25 seconds in which the sweep declared one of its tasks lost at 35; then at 70, a
// silence of exactly 30 seconds, which still offers its slots at the minute; is drained at 80,
// reports so at 85, 95, 105 and 115, offering nothing though heard, and is heard no more. It held a task from 10 to 50, the lost
// one from 20 to 35, one from 50, as the first ended, to 100, and one from 90 still running.
func TestAPoolsSlotsAndARunnersSilencesAreCountedFromItsHeartbeatsAndTasks(t *testing.T) {
	pool, super := joining(t)
	t0 := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	at := func(seconds int) time.Time { return t0.Add(time.Duration(seconds) * time.Second) }

	var runner string
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		issued, err := w.IssueJoinToken(ctx, "lan", nil, "admin", t0, t0.Add(time.Hour))
		if err != nil {
			return err
		}
		joined, err := w.Join(ctx, Joining{
			Token: issued.Clear, PublicKey: hostKey(7), CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
			Architecture: "amd64", AgentVersion: "0.2.0",
		}, time.Hour, t0)
		runner = joined.Runner
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	beat := func(seconds int) {
		t.Helper()
		if err := pool.Installation(t.Context(), Heartbeat, func(ctx context.Context, w *Wide) error {
			_, err := w.Beat(ctx, runner, beating(), at(seconds))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []int{5, 15, 40, 70} {
		beat(s)
	}
	if err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		_, err := w.Drain(ctx, runner, "admin", "the host is being replaced", at(80))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []int{85, 95, 105, 115} {
		beat(s)
	}

	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	// Joined on the database's clock, which is now, where everything else here is two hours back.
	if _, err := conn.Exec(t.Context(), `update runners set joined_at = $2 where id = $1`, runner, t0); err != nil {
		t.Fatal(err)
	}
	for i, task := range []struct {
		step           string
		state          string
		started, ended int
	}{
		{"render", "succeeded", 10, 50}, {"archive", "lost", 20, 35}, {"render", "succeeded", 50, 100}, {"archive", "running", 90, -1},
	} {
		var finished any
		if task.ended >= 0 {
			finished = at(task.ended)
		}
		if _, err := conn.Exec(t.Context(), `
			insert into tasks (namespace, id, run_id, step, attempt, state, runner, dispatched_at, started_at, finished_at)
			values ('finance', $1, $2, $3, $4, $5, $6, $7, $7, $8)`,
			fmt.Sprintf("01M2P%021d", i), financeRun, task.step, i+1, task.state, runner, at(task.started), finished); err != nil {
			t.Fatal(err)
		}
	}

	var got []PoolSeries
	if err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		got, err = w.PoolStatistics(ctx, Buckets{First: t0, Width: time.Minute, Count: 3})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var names []string
	var lan PoolSeries
	for _, p := range got {
		names = append(names, p.Pool)
		if p.Pool == "lan" {
			lan = p
		} else if len(p.Runners) != 0 || !reflect.DeepEqual(p.Buckets, make([]SlotBucket, 3)) {
			t.Errorf("pool %s, which nobody joined, counted %+v and %+v", p.Pool, p.Buckets, p.Runners)
		}
	}
	if want := []string{"default", "dmz", "lan"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("the pools read %v, want %v", names, want)
	}
	// Two held at once in the first minute, one carried into the second, which holds two again,
	// and one carried into the third, still running; four slots offered at the first minute's
	// end, none at the second's, drained though heard, and none at the third's, silent.
	slots := []SlotBucket{{InUseMax: 2, Capacity: 4}, {InUseMax: 2, Capacity: 0}, {InUseMax: 1, Capacity: 0}}
	if !reflect.DeepEqual(lan.Buckets, slots) {
		t.Errorf("lan held and offered %+v, want %+v", lan.Buckets, slots)
	}
	if len(lan.Runners) != 1 || lan.Runners[0].Runner != runner {
		t.Fatalf("lan's runners read %+v", lan.Runners)
	}
	r := lan.Runners[0]
	if !reflect.DeepEqual(r.Buckets, slots) {
		t.Errorf("the runner held and offered %+v, want %+v", r.Buckets, slots)
	}
	silences := []RunnerSilence{
		{At: at(15), Length: 25 * time.Second, TasksLost: 1},
		{At: at(40), Length: 30 * time.Second, TasksLost: 0},
		// Still going at the range's end, three minutes in, since the heartbeat at 115.
		{At: at(115), Length: 65 * time.Second, TasksLost: 0},
	}
	if len(r.Silences) != len(silences) {
		t.Fatalf("the runner's silences read %+v, want %+v", r.Silences, silences)
	}
	for i, s := range r.Silences {
		if !s.At.Equal(silences[i].At) || s.Length != silences[i].Length || s.TasksLost != silences[i].TasksLost {
			t.Errorf("silence %d reads %+v, want %+v", i, s, silences[i])
		}
	}
}
