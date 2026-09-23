package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/jackc/pgx/v5"
)

// A lost task, requeued. "A requeue after loss keeps the idempotency key and takes a new
// task_id", and "a loss does not use up a retry.max attempt": the heartbeat declares the loss in
// the table, the controller hears of it on its next pass, and what goes out again is the same unit
// of work under a row, a grant and a message identifier of its own.

const requeueingWorkflow = `
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
    retry: { max: 1, on: [lost, failed] }
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
`

// redeem binds a dispatch to a runner, which is what the runner's first call does and what the
// heartbeat and a runner's own loss are read against.
func (co *Core) redeem(t *testing.T, d Dispatch, runner string) error {
	t.Helper()
	return co.controller.Fenced(t.Context(), co.term, func(ctx context.Context, w *db.Wide) error {
		_, err := w.Redeem(ctx, d.Grant, d.Task.ID, runner, co.now())
		return err
	})
}

// dispatchesOf reads every row of one key as "requeue state runner", in the order it was
// dispatched.
func dispatchesOf(t *testing.T, conn *pgx.Conn, key agk.TaskID) []string {
	t.Helper()
	rows, err := conn.Query(t.Context(),
		`select requeue || ' ' || state || ' ' || coalesce(runner, '-') from tasks
		 where idempotency_key = $1 order by requeue`, string(key))
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The heartbeat declares the loss, the controller comes round to the run, and the task goes out
// again: the same key and attempt, a new row, a new grant. The dispatch that was lost opens
// nothing any more, and the attempt it was lost on is still the first of the two max: 1 allows.
func TestALostTaskIsRequeuedUnderTheSameKey(t *testing.T) {
	core, q, pool, super := decidingOn(t, requeueingWorkflow)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.dispatched()
	if len(first) != 1 {
		t.Fatalf("the first pass dispatched %d tasks", len(first))
	}
	lost := first[0]
	if err := core.redeem(t, lost, "runner-1"); err != nil {
		t.Fatal(err)
	}

	// runner-1 goes quiet. The task was dispatched on the test's clock, which is days behind
	// the database's, so three missed intervals have long passed. The wake the heartbeat
	// leaves is on the database's clock too, so the run is decided here as a sweep would
	// decide it once its own clock got there.
	if n, err := pool.Lost(t.Context(), 30*time.Second, 0); err != nil || n != 1 {
		t.Fatalf("the heartbeat declared %d tasks lost, answering %v", n, err)
	}
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}

	again := q.dispatched()
	if len(again) != 1 {
		t.Fatalf("after the loss the controller dispatched %d tasks", len(again))
	}
	requeued := again[0]
	if requeued.Task.ID != lost.Task.ID || requeued.Task.Attempt != 1 {
		t.Errorf("the requeue is %s, attempt %d, and it keeps the key %s it was lost under", requeued.Task.ID, requeued.Task.Attempt, lost.Task.ID)
	}
	if requeued.Row == lost.Row || requeued.Grant == lost.Grant {
		t.Errorf("the requeue went out as row %s, and a requeue takes a new task_id and a grant of its own", requeued.Row)
	}
	conn := dbtest.Superuser(t, super)
	if got, want := dispatchesOf(t, conn, lost.Task.ID), []string{"0 lost runner-1", "1 dispatched -"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}

	// The dispatch that was lost is over, whoever asks; the requeue is anybody's to take.
	if err := core.redeem(t, lost, "runner-1"); !errors.Is(err, db.ErrTaskHeld) {
		t.Errorf("the grant of the dispatch that was lost was redeemed again, answering %v", err)
	}
	if err := core.redeem(t, requeued, "runner-2"); err != nil {
		t.Errorf("the requeue could not be redeemed by another runner: %s", err)
	}

	// "A loss does not use up a retry.max attempt": the first attempt failing now is owed the
	// second.
	core.answer(t, failed(requeued.Task, 1, core.now()))
	if got := stateOf(t, core); got.Terminal() {
		t.Fatalf("a first attempt that failed after a loss left the run %s, and max: 1 owes a second", got)
	}
	second := q.taken()
	if len(second) != 1 || second[0].Attempt != 2 {
		t.Fatalf("after the failure the controller published %+v, want attempt 2", second)
	}
}

// A runner that recovers a task it had lost says so on a result, and the bus may deliver that
// result twice. The first is heard as the heartbeat's losses are, and the second finds the
// dispatch already lost: one requeue, one decision.
func TestALossReportedTwiceIsRequeuedOnce(t *testing.T) {
	core, q, pool, super := decidingOn(t, requeueingWorkflow)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.dispatched()
	if len(first) != 1 {
		t.Fatalf("the first pass dispatched %d tasks", len(first))
	}
	if err := core.redeem(t, first[0], "runner-dmz-02"); err != nil {
		t.Fatal(err)
	}

	loss := graph.Result{Task: first[0].Task.ID, State: agk.TaskLost}
	core.answer(t, loss)
	again := q.taken()
	if len(again) != 1 || again[0].ID != first[0].Task.ID {
		t.Fatalf("the reported loss published %+v, want %s again", again, first[0].Task.ID)
	}

	conn := dbtest.Superuser(t, super)
	before := seqOf(t, conn)
	core.answer(t, loss)
	if after := seqOf(t, conn); after != before {
		t.Errorf("the loss delivered again took the run from seq %d to %d", before, after)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("the loss delivered again published %+v", got)
	}
	if got, want := dispatchesOf(t, conn, first[0].Task.ID), []string{"0 lost runner-dmz-02", "1 dispatched -"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}

	// A runner speaking for a dispatch it never held is refused, the same way on every
	// delivery.
	err := core.Answer(t.Context(), Answer{Result: loss, Runner: "runner-lan-01"})
	if !errors.Is(err, ErrNotAResult) {
		t.Errorf("a loss reported by a runner that never held the task answered %v", err)
	}
}
