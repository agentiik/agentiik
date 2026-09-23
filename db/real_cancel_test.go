package db

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// A cancellation, against a real PostgreSQL: asked for by the API on the run's row, and found there
// by the controller.

// decidedAs writes one pass over theRun, taking it from sequence 0 to 1 in state.
func decidedAs(t *testing.T, pool *Pool, state agk.RunState, now time.Time, tasks ...TaskRow) {
	t.Helper()
	d := Decision{
		Namespace: "finance", Run: theRun, Was: 0, Seq: 1,
		Document: json.RawMessage(`{"version":1}`), State: state, StartedAt: now,
		WakeAt: now.Add(time.Hour), Tasks: tasks,
	}
	if state.Terminal() {
		d.FinishedAt, d.WakeAt, d.ExpiresAt = now, time.Time{}, now.Add(7*24*time.Hour)
	}
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		return w.SaveDecision(ctx, d)
	}); err != nil {
		t.Fatal(err)
	}
}

func askToCancel(t *testing.T, pool *Pool, namespace string, run agk.RunID, at time.Time) (agk.RunState, error) {
	t.Helper()
	var state agk.RunState
	err := pool.In(t.Context(), namespace, func(ctx context.Context, ns *NS) error {
		var err error
		state, err = ns.RequestCancel(ctx, run, at)
		return err
	})
	return state, err
}

func evaluated(t *testing.T, pool *Pool) Evaluation {
	t.Helper()
	var e Evaluation
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		var err error
		e, err = w.Run(ctx, theRun)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

// Asking writes down when, once, and decides nothing: the run is in the state it was in until the
// controller reads the request, and the sweep finds it however far off its clock is.
func TestACancellationIsAskedForOnceAndDecidedByNobody(t *testing.T) {
	pool, _ := created(t)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	decidedAs(t, pool, agk.Running, now)
	if swept(t, pool, now) {
		t.Fatal("a run waiting on its clock with nothing to publish was swept, so the sweep below proves nothing")
	}

	for i, at := range []time.Time{now, now.Add(time.Minute)} {
		state, err := askToCancel(t, pool, "finance", theRun, at)
		if err != nil || state != agk.Running {
			t.Fatalf("asking a %s time answered %s, %v", []string{"first", "second"}[i], state, err)
		}
	}
	e := evaluated(t, pool)
	if !e.CancelRequestedAt.Equal(now) {
		t.Errorf("the request reads as asked at %s, and it was first asked at %s", e.CancelRequestedAt, now)
	}
	if e.State != agk.Running || e.Seq != 1 {
		t.Errorf("asking moved the run to %s at sequence %d, and asking decides nothing", e.State, e.Seq)
	}
	if !swept(t, pool, now) {
		t.Error("a run somebody asked to cancel waits for its clock before the sweep finds it")
	}

	// Another namespace's run, and a name that is not a run at all, are nothing to cancel.
	if _, err := askToCancel(t, pool, "team-ops", theRun, now); !errors.Is(err, ErrNoRun) {
		t.Errorf("a run asked for under another namespace answered %v", err)
	}
	if _, err := askToCancel(t, pool, "finance", "not-a-run", now); !errors.Is(err, ErrNoRun) {
		t.Errorf("an identifier that is not one answered %v", err)
	}
}

// A run that has ended is answered in the state it ended in, and nothing is written on it.
func TestARunThatHasEndedIsLeftAsItEnded(t *testing.T) {
	pool, _ := created(t)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	decidedAs(t, pool, agk.Succeeded, now)

	state, err := askToCancel(t, pool, "finance", theRun, now)
	if err != nil || state != agk.Succeeded {
		t.Fatalf("asking to cancel a run that succeeded answered %s, %v", state, err)
	}
	if e := evaluated(t, pool); !e.CancelRequestedAt.IsZero() {
		t.Errorf("a run that had ended was asked to cancel at %s", e.CancelRequestedAt)
	}
}

// The workflow a run is of is found inside the namespace the run is in, and nowhere else.
func TestARunsWorkflowIsFoundInItsOwnNamespace(t *testing.T) {
	pool, _ := created(t)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}
	of := func(namespace string, run agk.RunID) (string, error) {
		var workflow string
		err := pool.In(t.Context(), namespace, func(ctx context.Context, ns *NS) error {
			var err error
			workflow, err = ns.WorkflowOf(ctx, run)
			return err
		})
		return workflow, err
	}
	if workflow, err := of("finance", theRun); err != nil || workflow != "monthly-invoicing" {
		t.Errorf("the run is of %q, %v", workflow, err)
	}
	for _, c := range []struct {
		namespace string
		run       agk.RunID
	}{{"team-ops", theRun}, {"finance", "not-a-run"}, {"finance", "01M2ZZZZZZZZZZZZZZZZZZZZZZ"}} {
		if workflow, err := of(c.namespace, c.run); !errors.Is(err, ErrNoRun) {
			t.Errorf("%s in %s is of %q, %v", c.run, c.namespace, workflow, err)
		}
	}
}
