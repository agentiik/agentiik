package db

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// A dispatch's progress on its row, against a real PostgreSQL: written by Progress from the runner
// holding it, only forwards and never over an ending, and kept by every decision that knows nothing
// of it.
func TestADispatchMovesOnlyForwardsOnTheWayToItsEnding(t *testing.T) {
	pool, _ := created(t)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}
	key := agk.NewTaskID(theRun, "invoice", 1, agk.Shard{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	seq := 0
	decide := func(task TaskRow) {
		t.Helper()
		task.ID, task.Step, task.Attempt = key, "invoice", 1
		if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
			return w.SaveDecision(ctx, Decision{
				Namespace: "finance", Run: theRun, Was: seq, Seq: seq + 1,
				Document: json.RawMessage(`{"version":1}`), State: agk.Running, StartedAt: now,
				WakeAt: now.Add(time.Hour), Tasks: []TaskRow{task},
			})
		}); err != nil {
			t.Fatalf("pass %d: %s", seq+1, err)
		}
		seq++
	}
	var row string
	progress := func(runner, of string, to agk.TaskState) (bool, error) {
		t.Helper()
		var moved bool
		err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
			var err error
			moved, err = w.Progress(ctx, "finance", key, of, runner, to)
			return err
		})
		return moved, err
	}
	state := func() string {
		t.Helper()
		var s string
		if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			return ns.tx.QueryRow(ctx, `select state::text from tasks where id = $1`, row).Scan(&s)
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}

	decide(TaskRow{State: agk.TaskDispatched, Runner: "runner-1", DispatchedAt: now})
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		var err error
		if row, err = w.TaskRow(ctx, "finance", key); err != nil {
			return err
		}
		_, err = w.Published(ctx, "finance", []agk.TaskID{key}, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Only from the runner holding it, only running or publishing, and only on a dispatch of
	// the key.
	for name, try := range map[string]func() (bool, error){
		"another runner":             func() (bool, error) { return progress("runner-2", row, agk.TaskRunning) },
		"no dispatch of the key":     func() (bool, error) { return progress("runner-1", "01M2AAZ9G62NQXFAFCXKRPJEH5", agk.TaskRunning) },
		"a row that is not a ULID":   func() (bool, error) { return progress("runner-1", "not-a-row", agk.TaskRunning) },
		"no runner":                  func() (bool, error) { return progress("", row, agk.TaskRunning) },
		"an ending on a runner word": func() (bool, error) { return progress("runner-1", row, agk.TaskSucceeded) },
		"a state before running":     func() (bool, error) { return progress("runner-1", row, agk.TaskDispatched) },
	} {
		if moved, err := try(); err == nil || moved {
			t.Errorf("progress from %s moved %v, answering %v", name, moved, err)
		}
	}
	if moved, err := progress("runner-2", row, agk.TaskRunning); !errors.Is(err, ErrNotHeld) || moved {
		t.Errorf("progress from a runner that does not hold the dispatch moved %v, answering %v, want ErrNotHeld", moved, err)
	}
	if got := state(); got != "dispatched" {
		t.Fatalf("refused progress left the dispatch %s", got)
	}

	if moved, err := progress("runner-1", row, agk.TaskRunning); err != nil || !moved {
		t.Fatalf("the holder's running moved %v, answering %v", moved, err)
	}
	if swept(t, pool, now) {
		t.Fatal("a run waiting on its clock with its one task running was swept, so the sweep below proves nothing")
	}

	// A decision knows nothing of running: it writes dispatched, and the row keeps running, as
	// what it says more rather than a loss the decision has not heard of, which would leave the
	// run for every sweep.
	decide(TaskRow{State: agk.TaskDispatched, DispatchedAt: now})
	if got := state(); got != "running" {
		t.Errorf("a decision moved a running dispatch back to %s", got)
	}
	if swept(t, pool, now) {
		t.Error("a decision that kept a running dispatch left the run for every sweep, as if it had not heard of a loss")
	}

	if moved, err := progress("runner-1", row, agk.TaskPublishing); err != nil || !moved {
		t.Fatalf("the holder's publishing moved %v, answering %v", moved, err)
	}
	for _, behind := range []agk.TaskState{agk.TaskPending, agk.TaskDispatched, agk.TaskRunning} {
		decide(TaskRow{State: behind, DispatchedAt: now})
		if got := state(); got != "publishing" {
			t.Errorf("a decision writing %s moved a publishing dispatch back to %s", behind, got)
		}
	}
	if moved, err := progress("runner-1", row, agk.TaskRunning); err != nil || moved {
		t.Errorf("running after publishing moved %v, answering %v", moved, err)
	}
	if moved, err := progress("runner-1", row, agk.TaskPublishing); err != nil || moved {
		t.Errorf("publishing twice moved %v the second time, answering %v", moved, err)
	}

	// The ending is written over both, and nothing moves it after.
	exit := 0
	decide(TaskRow{State: agk.TaskSucceeded, ExitCode: &exit, StartedAt: now, FinishedAt: now.Add(time.Second)})
	for _, late := range []agk.TaskState{agk.TaskRunning, agk.TaskPublishing} {
		if moved, err := progress("runner-1", row, late); err != nil || moved {
			t.Errorf("%s after the ending moved %v, answering %v", late, moved, err)
		}
	}
	if got := state(); got != "succeeded" {
		t.Errorf("the dispatch reads %s after its ending", got)
	}
}
