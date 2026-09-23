package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
)

// A cancellation a principal asked for, which reaches the controller the way everything from the
// API does: written in the database, and read there.

// bothAtOnceWorkflow starts two steps on the first pass, so that one run can hold a task a runner
// has redeemed and a task still waiting on the queue for one.
const bothAtOnceWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: normalize, port: ok } }
steps:
  normalize:
    image: ` + theImage + `
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
  archive:
    image: ` + theImage + `
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok]
`

// askedToCancel writes the request as the API writes it, and nothing else.
func askedToCancel(t *testing.T, pool *db.Pool, co *Core, run agk.RunID) {
	t.Helper()
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		_, err := ns.RequestCancel(ctx, run, co.now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// "Cancels pending tasks and sends SIGTERM to running containers." The request is found by a
// sweep, as it is when the notification never arrived, and the run it names ends: the task a
// runner holds is stopped, and the one still on the queue is ended where it stands, so that its
// grant opens nothing when a runner takes the message.
func TestACancellationAskedForOnTheRunStopsWhatItHolds(t *testing.T) {
	core, q, pool, _ := decidingOn(t, bothAtOnceWorkflow)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	sent := q.dispatched()
	if len(sent) != 2 {
		t.Fatalf("the first pass published %d tasks", len(sent))
	}
	var held, queued Dispatch
	for _, d := range sent {
		if d.Task.Step == "archive" {
			held = d
		} else {
			queued = d
		}
	}
	if err := core.redeem(t, held, theRunner); err != nil {
		t.Fatal(err)
	}

	// Asked, and nothing has moved: the request is not the cancellation.
	askedToCancel(t, pool, core, decidedRun)
	if got := stateOf(t, core); got != agk.Running {
		t.Fatalf("asking to cancel took the run to %s before the controller read the request", got)
	}

	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, core); got != agk.Cancelled {
		t.Fatalf("a run somebody asked to cancel is %s after a sweep", got)
	}
	stopped := map[agk.TaskID]graph.StopReason{}
	for _, s := range q.stops() {
		stopped[s.Task] = s.Reason
	}
	if len(stopped) != 2 || stopped[held.Task.ID] != graph.StopCancelled || stopped[queued.Task.ID] != graph.StopCancelled {
		t.Errorf("cancelling stopped %v", stopped)
	}

	// Every task of the run reads cancelled, the namespace holds nothing for it, and the
	// message still on the queue starts nothing.
	var tasks []db.TaskSummary
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		d, err := ns.RunDetail(ctx, decidedRun)
		tasks = d.Tasks
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.State != agk.TaskCancelled {
			t.Errorf("%s reads %s in a cancelled run", task.Task, task.State)
		}
	}
	var free int
	if err := core.controller.Fenced(t.Context(), core.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		free, err = w.Slots(ctx, "finance")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if free != 20 {
		t.Errorf("the namespace has %d of its 20 slots free, and the run holding the rest was cancelled", free)
	}
	if err := core.redeem(t, queued, "runner-dmz-03"); !errors.Is(err, db.ErrTaskHeld) {
		t.Errorf("the grant of a task still on the queue when its run was cancelled redeemed, answering %v", err)
	}

	// The request stays on the row, and the run it names has ended: a sweep does not offer
	// it again, and a notification arriving late stops nothing twice.
	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	if err := core.Wake(t.Context(), Wake{Run: decidedRun}); err != nil {
		t.Fatal(err)
	}
	if got := q.stops(); len(got) != 0 {
		t.Errorf("a run already cancelled was stopped again: %+v", got)
	}
}

// A pass that published a task and died before recording the dispatch leaves the task pending in
// the document, where the evaluator names nothing to stop, and a runner may take the message and
// redeem its grant before any pass comes round. Cancelling the run stops that task all the same:
// the redemption bound its row, and the row is what says a runner holds it.
func TestACancellationStopsATaskTakenBeforeItsDispatchWasRecorded(t *testing.T) {
	core, q, pool, _ := deciding(t)
	createRun(t, pool)
	ctx, cancel := context.WithCancel(t.Context())
	died := &diesOnPublishing{cancel: cancel}
	dead, err := NewCore(core.controller, core.term, Options{
		Queue: died, Versions: core.versions, Objects: core.objects, Now: core.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dead.Decide(ctx, decidedRun); err == nil {
		t.Fatal("a pass that died after publishing answered as if it had recorded the dispatch")
	}
	sent := died.dispatched()
	if len(sent) != 1 {
		t.Fatalf("the pass that died published %d tasks", len(sent))
	}
	if err := core.redeem(t, sent[0], theRunner); err != nil {
		t.Fatal(err)
	}

	askedToCancel(t, pool, core, decidedRun)
	if err := core.Wake(t.Context(), Wake{Run: decidedRun}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, core); got != agk.Cancelled {
		t.Fatalf("a run somebody asked to cancel is %s", got)
	}
	stops := q.stops()
	if len(stops) != 1 || stops[0].Task != sent[0].Task.ID || stops[0].Reason != graph.StopCancelled {
		t.Errorf("cancelling stopped %+v, and %s had redeemed %s", stops, theRunner, sent[0].Task.ID)
	}
}

// A run still queued is cancelled before it is let in, so nothing is ever published for it, and it
// ends without ever having started.
func TestARunAskedToCancelBeforeItStartedStartsNothing(t *testing.T) {
	core, q, pool, _ := deciding(t)
	createRun(t, pool)
	askedToCancel(t, pool, core, decidedRun)

	if err := core.Wake(t.Context(), Wake{Run: decidedRun}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, core); got != agk.Cancelled {
		t.Fatalf("a queued run somebody asked to cancel is %s", got)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("a run cancelled before it started published %+v", got)
	}
	if got := q.stops(); len(got) != 0 {
		t.Errorf("a run cancelled before it started stopped %+v", got)
	}
	var d db.RunDetail
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		var err error
		d, err = ns.RunDetail(ctx, decidedRun)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !d.StartedAt.IsZero() || !d.FinishedAt.Equal(core.now()) {
		t.Errorf("a run cancelled from queued reads started at %s and finished at %s", d.StartedAt, d.FinishedAt)
	}
}
