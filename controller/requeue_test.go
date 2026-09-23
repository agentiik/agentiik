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
	if err := core.Answer(t.Context(), Answer{Result: failed(requeued.Task, 1, core.now()), Row: requeued.Row, Runner: "runner-2"}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, core); got.Terminal() {
		t.Fatalf("a first attempt that failed after a loss left the run %s, and max: 1 owes a second", got)
	}
	second := q.taken()
	if len(second) != 1 || second[0].Attempt != 2 {
		t.Fatalf("after the failure the controller published %+v, want attempt 2", second)
	}

	// And once the key has completed, no dispatch of it is redeemed again.
	if err := core.redeem(t, requeued, "runner-2"); !errors.Is(err, db.ErrTaskHeld) {
		t.Errorf("a key that completed was redeemed again, answering %v", err)
	}
	if got, want := dispatchesOf(t, conn, lost.Task.ID), []string{"0 lost runner-1", "1 failed runner-2"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}
}

// A runner that recovers a task it had lost says so on a result, and the bus may deliver that
// result twice, the second time after the same runner has taken the requeue. The first is heard as
// the heartbeat's losses are, and the second names the dispatch that is already lost: one requeue,
// one decision, and the requeue left running.
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

	loss := Answer{
		Result: graph.Result{Task: first[0].Task.ID, State: agk.TaskLost},
		Row:    first[0].Row, Runner: "runner-dmz-02",
	}
	if err := core.Answer(t.Context(), loss); err != nil {
		t.Fatal(err)
	}
	again := q.dispatched()
	if len(again) != 1 || again[0].Task.ID != first[0].Task.ID {
		t.Fatalf("the reported loss dispatched %+v, want %s again", again, first[0].Task.ID)
	}
	if err := core.redeem(t, again[0], "runner-dmz-02"); err != nil {
		t.Fatal(err)
	}

	conn := dbtest.Superuser(t, super)
	before := seqOf(t, conn)
	if err := core.Answer(t.Context(), loss); err != nil {
		t.Fatal(err)
	}
	if after := seqOf(t, conn); after != before {
		t.Errorf("the loss delivered again took the run from seq %d to %d", before, after)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("the loss delivered again published %+v", got)
	}
	if got, want := dispatchesOf(t, conn, first[0].Task.ID), []string{"0 lost runner-dmz-02", "1 dispatched runner-dmz-02"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}

	// A runner speaking for a dispatch it never held is refused, the same way on every
	// delivery, and so is a loss that names no dispatch at all.
	for _, c := range []struct {
		answer Answer
		why    string
	}{
		{Answer{Result: loss.Result, Row: again[0].Row, Runner: "runner-lan-01"}, "a runner that never held the task"},
		{Answer{Result: loss.Result, Runner: "runner-dmz-02"}, "no dispatch"},
	} {
		if err := core.Answer(t.Context(), c.answer); !errors.Is(err, ErrNotAResult) {
			t.Errorf("a loss reported naming %s answered %v", c.why, err)
		}
	}
	if got, want := dispatchesOf(t, conn, first[0].Task.ID), []string{"0 lost runner-dmz-02", "1 dispatched runner-dmz-02"}; !slices.Equal(got, want) {
		t.Errorf("after the refusals the key holds %q, want %q", got, want)
	}
}

// The heartbeat declares a dispatch lost, the requeue goes out, and the runner that went quiet
// comes back, takes the requeue, and reports the loss it recovered. That loss is about the first
// dispatch, which the heartbeat has already moved, and not about the requeue the same runner now
// holds.
func TestALossReportedAfterTheHeartbeatDeclaredItMovesNothing(t *testing.T) {
	core, q, pool, super := decidingOn(t, requeueingWorkflow)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.dispatched()
	if len(first) != 1 {
		t.Fatalf("the first pass dispatched %d tasks", len(first))
	}
	if err := core.redeem(t, first[0], "runner-1"); err != nil {
		t.Fatal(err)
	}
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
	if err := core.redeem(t, again[0], "runner-1"); err != nil {
		t.Fatal(err)
	}

	conn := dbtest.Superuser(t, super)
	before := seqOf(t, conn)
	if err := core.Answer(t.Context(), Answer{
		Result: graph.Result{Task: first[0].Task.ID, State: agk.TaskLost},
		Row:    first[0].Row, Runner: "runner-1",
	}); err != nil {
		t.Fatal(err)
	}
	if after := seqOf(t, conn); after != before {
		t.Errorf("a loss the heartbeat had already declared took the run from seq %d to %d", before, after)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("a loss the heartbeat had already declared published %+v", got)
	}
	if got, want := dispatchesOf(t, conn, first[0].Task.ID), []string{"0 lost runner-1", "1 dispatched runner-1"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}
}

// The heartbeat declares a dispatch lost and the requeue goes to another runner. The runner that
// went quiet comes back and reports how its container ended, which is not news: the attempt
// stopped waiting on that dispatch when it was requeued, and the requeue is still running and
// owed an ending of its own. So nothing is decided, no further attempt goes out, and the requeue's
// row does not take the name of a runner that never held it. The requeue's ending is the one that
// counts.
func TestAnEndingOfALostDispatchChangesNothing(t *testing.T) {
	core, q, pool, super := decidingOn(t, requeueingWorkflow)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.dispatched()
	if len(first) != 1 {
		t.Fatalf("the first pass dispatched %d tasks", len(first))
	}
	if err := core.redeem(t, first[0], "runner-1"); err != nil {
		t.Fatal(err)
	}
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
	if err := core.redeem(t, again[0], "runner-2"); err != nil {
		t.Fatal(err)
	}

	conn := dbtest.Superuser(t, super)
	before := seqOf(t, conn)
	if err := core.Answer(t.Context(), Answer{
		Result: failed(first[0].Task, 1, core.now()), Row: first[0].Row, Runner: "runner-1",
	}); err != nil {
		t.Fatal(err)
	}
	if after := seqOf(t, conn); after != before {
		t.Errorf("a failure of the dispatch that was lost took the run from seq %d to %d", before, after)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("a failure of the dispatch that was lost published %+v", got)
	}
	if got, want := dispatchesOf(t, conn, first[0].Task.ID), []string{"0 lost runner-1", "1 dispatched runner-2"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}

	// An ending naming a row that is no dispatch of its key is one no delivery could make
	// sense of.
	if err := core.Answer(t.Context(), Answer{
		Result: failed(first[0].Task, 1, core.now()), Row: "01M2HZZZZZZZZZZZZZZZZZZZZZ", Runner: "runner-1",
	}); !errors.Is(err, ErrNotAResult) {
		t.Errorf("an ending naming no dispatch of its key answered %v", err)
	}

	if err := core.Answer(t.Context(), Answer{
		Result: succeeded(t, again[0].Task, core.now()), Row: again[0].Row, Runner: "runner-2",
	}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, core); got != agk.Succeeded {
		t.Errorf("the run is %s after the requeue succeeded", got)
	}
	if got, want := dispatchesOf(t, conn, first[0].Task.ID), []string{"0 lost runner-1", "1 succeeded runner-2"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}

}

// "lost: The runner holding it stopped reporting." A task no runner has taken is waiting on the
// queue, however long a busy pool keeps it there, and the heartbeat declares nothing about it: a
// step that does not requeue is not failed for the wait, and one that does is not sent out again
// into the queue its task is already waiting on.
func TestATaskNoRunnerHasTakenIsNeverLost(t *testing.T) {
	for _, c := range []struct{ name, workflow string }{
		{"no retry policy", theWorkflow},
		{"retry on lost", requeueingWorkflow},
	} {
		t.Run(c.name, func(t *testing.T) {
			core, q, pool, super := decidingOn(t, c.workflow)
			createRun(t, pool)
			if err := core.Decide(t.Context(), decidedRun); err != nil {
				t.Fatal(err)
			}
			first := q.dispatched()
			if len(first) != 1 {
				t.Fatalf("the first pass dispatched %d tasks", len(first))
			}

			// Dispatched on the test's clock, days behind the database's, and taken by
			// nobody since.
			if n, err := pool.Lost(t.Context(), 30*time.Second, 0); err != nil || n != 0 {
				t.Fatalf("the heartbeat declared %d tasks lost that no runner had taken, answering %v", n, err)
			}
			if err := core.Decide(t.Context(), decidedRun); err != nil {
				t.Fatal(err)
			}
			if got := stateOf(t, core); got != agk.Running {
				t.Errorf("a run whose one task is waiting on the queue is %s", got)
			}
			if again := q.dispatched(); len(again) != 0 {
				t.Errorf("a task waiting on the queue was sent out again as %+v", again)
			}
			conn := dbtest.Superuser(t, super)
			if got, want := dispatchesOf(t, conn, first[0].Task.ID), []string{"0 dispatched -"}; !slices.Equal(got, want) {
				t.Errorf("the key holds %q, want %q", got, want)
			}
		})
	}
}
