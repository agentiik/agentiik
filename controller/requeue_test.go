package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
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

// silence lets more than three heartbeat intervals pass with nothing heard, and sweeps, as the
// controller does on its interval: what went unaccounted for is declared lost, and the runs that
// woke are decided.
func (co *Core) silence(t *testing.T) {
	t.Helper()
	clock.advance(db.LostAfter + time.Second)
	if err := co.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
}

// bounded gives a workflow a root timeout of an hour, which is a clock its run waits on: a sweep
// inside the hour finds the run only where something woke it.
func bounded(workflow string) string {
	return strings.Replace(workflow, "\ninputs:", "\ntimeout: 1h\ninputs:", 1)
}

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

	// runner-1 goes quiet for three intervals, and the sweep declares the task lost and
	// decides the run it woke.
	core.silence(t)

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
	if err := core.Answer(t.Context(), Answer{Result: loss.Result, Row: again[0].Row, Runner: "runner-lan-01"}); !errors.Is(err, ErrNotTheHolder) {
		t.Errorf("a loss reported by a runner that never held the task answered %v, and it is speaking for somebody else's", err)
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
	core.silence(t)
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
	core.silence(t)
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

	// Its envelopes uploaded and named by digest, as the wire carries them, by the runner the
	// requeue is bound to.
	ending := core.answerOf(t, succeeded(t, again[0].Task, core.now()))
	if ending.Row != again[0].Row || ending.Runner != "runner-2" {
		t.Fatalf("the requeue's ending names dispatch %s from %s", ending.Row, ending.Runner)
	}
	if err := core.Answer(t.Context(), ending); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, core); got != agk.Succeeded {
		t.Errorf("the run is %s after the requeue succeeded", got)
	}
	if got, want := dispatchesOf(t, conn, first[0].Task.ID), []string{"0 lost runner-1", "1 succeeded runner-2"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}

}

// "lost: The runner holding it stopped reporting." A task no runner has redeemed is waiting on the
// queue, or on a runner that took it and has not acknowledged it, which the bus hands on should
// that runner die, and the sweep declares nothing about it however long a busy pool keeps it
// there: a step that does not requeue is not failed for the wait, and one that does is not sent
// out again into the queue its task is already waiting on.
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

			// Taken by nobody for three intervals and more.
			core.silence(t)
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

// The sweep that declares a loss decides the run the loss woke, on the same pass. Both runs wait on
// a root timeout an hour off, so a sweep inside the hour decides one only where something woke it:
// declared after the runs were chosen, or without a wake, the loss would wait for the next sweep
// that found the run due, which here is its deadline. A step that requeues sends the task out again
// under its key; one that does not fails for it.
func TestTheSweepDecidesTheRunItsLossWoke(t *testing.T) {
	for _, c := range []struct {
		name, workflow string
		run            agk.RunState
		dispatches     []string
	}{
		{"no retry policy", bounded(theWorkflow), agk.Failed, []string{"0 lost runner-1"}},
		{"retry on lost", bounded(requeueingWorkflow), agk.Running, []string{"0 lost runner-1", "1 dispatched -"}},
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
			if err := core.redeem(t, first[0], "runner-1"); err != nil {
				t.Fatal(err)
			}

			core.silence(t)
			conn := dbtest.Superuser(t, super)
			if got := dispatchesOf(t, conn, first[0].Task.ID); !slices.Equal(got, c.dispatches) {
				t.Errorf("after the sweep the key holds %q, want %q", got, c.dispatches)
			}
			if got := stateOf(t, core); got != c.run {
				t.Errorf("after the sweep the run is %s, want %s", got, c.run)
			}
			again := q.dispatched()
			switch {
			case c.run == agk.Failed && len(again) != 0:
				t.Errorf("a step that does not requeue sent its task out again as %+v", again)
			case c.run == agk.Running && (len(again) != 1 || again[0].Task.ID != first[0].Task.ID || again[0].Row == first[0].Row):
				t.Errorf("the requeue went out as %+v, want %s again under a new task_id", again, first[0].Task.ID)
			}
		})
	}
}

// A key is handed out again after a loss as often as the installation's max_requeues allows, three
// where it sets nothing and none where it sets zero, and no more. Each dispatch is redeemed by a
// runner of its own, which then goes quiet, as a container that takes down every host it lands on
// would leave it, and the heartbeat declares it lost. The loss past the bound stands: the key goes
// out no more, its last grant opens nothing even to the runner it was bound to, and the run fails
// on it rather than spending the attempt max: 1 has left, since the brick never failed. A negative
// bound counts no number of times, and no core is built on one.
func TestAKeyLostPastMaxRequeuesIsNotRequeued(t *testing.T) {
	for _, c := range []struct {
		name       string
		set        *int
		dispatches []string
	}{
		{"the default", nil, []string{"0 lost runner-1", "1 lost runner-2", "2 lost runner-3", "3 lost runner-4"}},
		{"one", new(1), []string{"0 lost runner-1", "1 lost runner-2"}},
		{"none", new(0), []string{"0 lost runner-1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			deciding, q, pool, super := decidingOn(t, requeueingWorkflow)
			core, err := NewCore(deciding.controller, deciding.term, Options{
				Queue: q, Versions: deciding.versions, Objects: deciding.objects, Now: deciding.now,
				MaxRequeues: c.set,
			})
			if err != nil {
				t.Fatal(err)
			}
			createRun(t, pool)
			if err := core.Decide(t.Context(), decidedRun); err != nil {
				t.Fatal(err)
			}
			sent := q.dispatched()
			if len(sent) != 1 {
				t.Fatalf("the first pass dispatched %d tasks", len(sent))
			}
			key := sent[0].Task.ID
			for n := range len(c.dispatches) {
				last, holder := sent[0], fmt.Sprintf("runner-%d", n+1)
				if err := core.redeem(t, last, holder); err != nil {
					t.Fatal(err)
				}
				core.silence(t)
				sent = q.dispatched()
				if n == len(c.dispatches)-1 {
					if len(sent) != 0 {
						t.Errorf("a key lost past max_requeues went out again as %+v", sent)
					}
					// Its own holder, since a stranger is refused a dispatch bound to
					// somebody else before anything asks whether it is over.
					if err := core.redeem(t, last, holder); !errors.Is(err, db.ErrTaskHeld) {
						t.Errorf("the grant of a dispatch lost past max_requeues was redeemed again, answering %v", err)
					}
					break
				}
				if len(sent) != 1 || sent[0].Task.ID != key || sent[0].Task.Attempt != 1 || sent[0].Row == last.Row {
					t.Fatalf("after loss %d the controller dispatched %+v, want %s again on attempt 1 under a new task_id", n+1, sent, key)
				}
			}

			conn := dbtest.Superuser(t, super)
			if got := dispatchesOf(t, conn, key); !slices.Equal(got, c.dispatches) {
				t.Errorf("the key holds %q, want %q", got, c.dispatches)
			}
			if got := stateOf(t, core); got != agk.Failed {
				t.Errorf("the run is %s after its one step lost a key past max_requeues", got)
			}
		})
	}

	deciding, q, _, _ := decidingOn(t, requeueingWorkflow)
	if _, err := NewCore(deciding.controller, deciding.term, Options{
		Queue: q, Versions: deciding.versions, Objects: deciding.objects, Now: deciding.now,
		MaxRequeues: new(-1),
	}); err == nil || !strings.Contains(err.Error(), "max_requeues") {
		t.Errorf("a core was built under max_requeues: -1, which is no number of times, answering %v", err)
	}
}

// A sweep that cannot look for lost tasks still decides the runs that are due, and says why it could
// not. The losses wait for the next sweep, and the runs have nothing to do with them: a sweep that
// stopped there would leave every run of the installation waiting on the one statement.
func TestASweepThatCannotLookForLossesStillDecides(t *testing.T) {
	core, q, pool, super := decidingOn(t, theWorkflow)
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

	// Nothing may be moved to lost, so looking for losses fails whole.
	conn := dbtest.Superuser(t, super)
	for _, stmt := range []string{
		`create function refuse_loss() returns trigger language plpgsql as $$
		 begin raise exception 'no loss may be written here'; end $$`,
		`create trigger refuse_loss before update on tasks for each row
		 when (new.state = 'lost') execute function refuse_loss()`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}

	// A second run, created while the runner holding the first one's task went quiet.
	const later agk.RunID = "01M2Z8V1P9C4XQ7K2N4D6F8H0D"
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.CreateRun(ctx, db.NewRun{
			ID: later, Workflow: "monthly-invoicing", Commit: "a3f9c1e",
			Trigger: agk.TriggerManual, TriggeredBy: "alice",
			Inputs: json.RawMessage(`{"orders": []}`),
			Steps:  []agk.Step{"normalize", "archive"},
		})
	}); err != nil {
		t.Fatal(err)
	}

	var trouble []error
	core.controller.Trouble = func(_ agk.RunID, err error) { trouble = append(trouble, err) }
	clock.advance(db.LostAfter + time.Second)
	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatalf("a sweep that could not look for losses answered %v", err)
	}
	if len(trouble) != 1 {
		t.Errorf("a sweep that could not look for losses reported %v", trouble)
	}
	if sent := q.taken(); len(sent) != 1 || sent[0].Run != later {
		t.Errorf("the sweep published %+v, want the first task of run %s", sent, later)
	}
}

// lostAndRequeued is a run whose one task went out, was redeemed by runner-1, was declared lost
// when runner-1 went quiet, and went out again under the same key. It answers the dispatch that
// was lost and the requeue, which nobody has redeemed yet.
func lostAndRequeued(t *testing.T) (*Core, *fakeQueue, *pgx.Conn, Dispatch, Dispatch) {
	t.Helper()
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
	core.silence(t)
	again := q.dispatched()
	if len(again) != 1 || again[0].Task.ID != first[0].Task.ID || again[0].Row == first[0].Row {
		t.Fatalf("after the loss the controller dispatched %+v, want %s again under a new task_id", again, first[0].Task.ID)
	}
	return core, q, dbtest.Superuser(t, super), first[0], again[0]
}

// The runner a dispatch was bound to is the one its ending is taken from, and that stays true of a
// dispatch the key was requeued past: runner-1 held it, went quiet, and comes back with the
// success its container had after all, envelopes uploaded and named by digest. It is runner-1's to
// report and it is taken, and it is not news, since the attempt waits on the requeue. So nothing is
// decided or published, and nothing lands on the requeue's row, neither the log nor a runner that
// never redeemed it. The same success from runner-2, which holds the requeue and never held this
// dispatch, is not late but somebody else's, and refused.
func TestALateEndingOfARequeuedPastDispatchFromItsOwnRunnerIsNotNews(t *testing.T) {
	core, q, conn, lost, requeued := lostAndRequeued(t)
	if err := core.redeem(t, requeued, "runner-2"); err != nil {
		t.Fatal(err)
	}

	// answerOf uploads the envelopes and names the latest dispatch, which runner-2 holds; the
	// late ending is the same bytes about the dispatch before it.
	late := core.answerOf(t, succeeded(t, lost.Task, core.now()))
	if len(late.Outputs) == 0 {
		t.Fatal("the late success names no envelope, so this is not the case under test")
	}
	late.Row = lost.Row

	before := seqOf(t, conn)
	late.Runner = "runner-2"
	if err := core.Answer(t.Context(), late); !errors.Is(err, ErrNotAResult) || !errors.Is(err, ErrNotTheHolder) {
		t.Errorf("the requeue's holder reporting the dispatch it replaced answered %v", err)
	}
	late.Runner = "runner-1"
	if err := core.Answer(t.Context(), late); err != nil {
		t.Fatalf("the late success of the runner the lost dispatch was bound to was refused: %s", err)
	}
	if after := seqOf(t, conn); after != before {
		t.Errorf("a late ending of a dispatch requeued past took the run from seq %d to %d", before, after)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("a late ending of a dispatch requeued past published %+v", got)
	}
	if got := stateOf(t, core); got != agk.Running {
		t.Errorf("the run is %s, and the requeue is still owed its ending", got)
	}
	if got, want := dispatchesOf(t, conn, lost.Task.ID), []string{"0 lost runner-1", "1 dispatched runner-2"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}
	var lines *int
	if err := conn.QueryRow(t.Context(),
		`select log_lines from tasks where idempotency_key = $1 and requeue = 1`, string(lost.Task.ID)).
		Scan(&lines); err != nil {
		t.Fatal(err)
	}
	if lines != nil {
		t.Errorf("the requeue's row counts %d lines of log from the dispatch it replaced", *lines)
	}

	// And the requeue's own ending is the one that counts.
	ending := core.answerOf(t, succeeded(t, requeued.Task, core.now()))
	if ending.Row != requeued.Row || ending.Runner != "runner-2" {
		t.Fatalf("the requeue's ending names dispatch %s from %s", ending.Row, ending.Runner)
	}
	if err := core.Answer(t.Context(), ending); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, core); got != agk.Succeeded {
		t.Errorf("the run is %s after the requeue succeeded", got)
	}
}

// A requeue keeps the key and takes a new task_id, and the binding is the task_id's. runner-1 held
// the dispatch that was lost, which gives it nothing of a requeue runner-2 redeems: once runner-2
// has, an ending from a container runner-1 says ran for the requeue, or a loss of it, is refused
// as somebody else's and binds runner-1 to nothing. Before anybody redeemed it, runner-1's loss of
// it is refused, since a runner cannot lose what it never held, and so is an ending from
// runner-3, which was never given the key at all. Only runner-2's ending is taken. An ending that
// never reached a container is not this case, and nor is one runner-1's host answers from its
// record before anybody redeems the requeue: the first runner to report either is bound by it.
func TestTheRequeuesEndingIsRefusedFromARunnerNotBoundToItsTaskID(t *testing.T) {
	core, q, conn, lost, requeued := lostAndRequeued(t)
	before := seqOf(t, conn)

	ran := failed(requeued.Task, 1, core.now())
	ran.DispatchedAt = time.Time{}
	loss := graph.Result{Task: requeued.Task.ID, State: agk.TaskLost}
	refused := func(when string, answers ...Answer) {
		t.Helper()
		for _, a := range answers {
			if err := core.Answer(t.Context(), a); !errors.Is(err, ErrNotAResult) || !errors.Is(err, ErrNotTheHolder) {
				t.Errorf("%s, %s reporting the requeue %s answered %v", when, a.Runner, a.Result.State, err)
			}
		}
	}

	refused("before anybody redeemed it",
		Answer{Result: loss, Row: requeued.Row, Runner: "runner-1"},
		Answer{Result: ran, Row: requeued.Row, Runner: "runner-3"})
	if got, want := dispatchesOf(t, conn, lost.Task.ID), []string{"0 lost runner-1", "1 dispatched -"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}
	if err := core.redeem(t, requeued, "runner-2"); err != nil {
		t.Fatalf("runner-2 could not redeem the requeue after the refused reports: %s", err)
	}
	refused("once runner-2 redeemed it",
		Answer{Result: ran, Row: requeued.Row, Runner: "runner-1"},
		Answer{Result: loss, Row: requeued.Row, Runner: "runner-1"})

	if after := seqOf(t, conn); after != before {
		t.Errorf("refused reports took the run from seq %d to %d", before, after)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("refused reports published %+v", got)
	}
	if got, want := dispatchesOf(t, conn, lost.Task.ID), []string{"0 lost runner-1", "1 dispatched runner-2"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}

	// runner-2's failure is taken, and max: 1 owes the second attempt.
	mine := Answer{Result: ran, Row: requeued.Row, Runner: "runner-2"}
	if err := core.Answer(t.Context(), mine); err != nil {
		t.Fatalf("the failure of the runner holding the requeue was refused: %s", err)
	}
	if second := q.taken(); len(second) != 1 || second[0].Attempt != 2 {
		t.Errorf("after the requeue failed the controller published %+v, want attempt 2", second)
	}
	if got, want := dispatchesOf(t, conn, lost.Task.ID), []string{"0 lost runner-1", "1 failed runner-2"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}
}

// fromTheRecord is what a host that ended a key on an earlier dispatch answers when the requeue of
// that key comes back to it: the ending it recorded, the envelopes named by the digest they were
// uploaded under the first time, and the log where it went, under the requeue's task_id and from
// the runner that redeemed the dispatch it ended. Nobody redeems the requeue for it.
func (co *Core) fromTheRecord(t *testing.T, r graph.Result, row, runner string) Answer {
	t.Helper()
	log, err := agk.NewLogURI(r.Task)
	if err != nil {
		t.Fatal(err)
	}
	a := Answer{Row: row, Runner: runner, Log: log, LogLines: 412}
	for _, port := range sortedPorts(r.Outputs) {
		e := r.Outputs[port]
		digest, _, err := artifact.PutEnvelope(t.Context(), co.objects, "finance", e)
		if err != nil {
			t.Fatal(err)
		}
		a.Outputs = append(a.Outputs, Output{Port: port, Digest: digest, Items: e.Meta.Count})
	}
	r.Outputs, r.DispatchedAt = nil, time.Time{}
	a.Result = r
	return a
}

// runner-1 redeemed the dispatch, ran it to its end and reported the success into a silence, and
// the heartbeat declared the dispatch lost. The requeue comes back to runner-1's host, which
// refuses to run the key again and answers with the ending it recorded, under the requeue's
// task_id. Nobody redeems the requeue, so nobody else would ever answer it: the answer is taken
// from runner-1, which was given the key, as the requeue's, and binds runner-1 to it. The run goes
// on, the requeue's grant opens nothing any more, and the same answer delivered again is written
// once. A runner of the pool that was never given the key is refused the same answer.
func TestARequeueThatCameBackToTheHostThatEndedItsKeyIsAnsweredFromItsRecord(t *testing.T) {
	core, q, conn, lost, requeued := lostAndRequeued(t)
	recorded := core.fromTheRecord(t, succeeded(t, requeued.Task, core.now()), requeued.Row, "runner-1")
	if len(recorded.Outputs) == 0 {
		t.Fatal("the recorded success names no envelope, so this is not the case under test")
	}

	before := seqOf(t, conn)
	stranger := recorded
	stranger.Runner = "runner-lan-01"
	if err := core.Answer(t.Context(), stranger); !errors.Is(err, ErrNotAResult) || !errors.Is(err, ErrNotTheHolder) {
		t.Errorf("a runner never given the key answering the requeue answered %v", err)
	}
	if after := seqOf(t, conn); after != before {
		t.Errorf("the refused answer took the run from seq %d to %d", before, after)
	}

	if err := core.Answer(t.Context(), recorded); err != nil {
		t.Fatalf("the ending runner-1 recorded, reported under the requeue's task_id, was refused: %s", err)
	}
	if got := stateOf(t, core); got != agk.Succeeded {
		t.Errorf("the run is %s after the requeue was answered from the record", got)
	}
	if got, want := dispatchesOf(t, conn, lost.Task.ID), []string{"0 lost runner-1", "1 succeeded runner-1"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}
	var lines *int
	if err := conn.QueryRow(t.Context(),
		`select log_lines from tasks where idempotency_key = $1 and requeue = 1`, string(lost.Task.ID)).
		Scan(&lines); err != nil {
		t.Fatal(err)
	}
	if lines == nil || *lines != 412 {
		t.Errorf("the requeue's row counts %v lines of log, and its answer named 412", lines)
	}
	if err := core.redeem(t, requeued, "runner-2"); !errors.Is(err, db.ErrTaskHeld) {
		t.Errorf("the grant of a requeue answered from the record was redeemed, answering %v", err)
	}

	written := seqOf(t, conn)
	if err := core.Answer(t.Context(), recorded); err != nil {
		t.Fatalf("the same answer delivered again was refused: %s", err)
	}
	if after := seqOf(t, conn); after != written {
		t.Errorf("the answer delivered again took the run from seq %d to %d", written, after)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("the run published %+v after its one step succeeded", got)
	}
}

// runner-1 redeemed the dispatch and was cut off while its container ran, and the heartbeat declared
// the dispatch lost. The requeue reaches runner-1's host with the container still running there,
// and the host refuses it as it writes the key down, before anything is redeemed
// (driver.ErrTaskInFlight), so nobody holds the requeue and no sweep declares it lost while it
// waits. The container's one ending goes to the dispatch that was lost, which is not news, and the
// requeue, come round once the key has ended, is answered from the record. One cut spends one
// requeue, and the run succeeds under max_requeues: 1: the requeue redeemed and bound to a host
// that would never answer it would have been lost in its turn, and the bound would have failed a
// step whose brick succeeded.
func TestOneCutSpendsOneRequeueWhereTheRequeueReachesTheHostStillRunningItsKey(t *testing.T) {
	deciding, q, pool, super := decidingOn(t, requeueingWorkflow)
	core, err := NewCore(deciding.controller, deciding.term, Options{
		Queue: q, Versions: deciding.versions, Objects: deciding.objects, Now: deciding.now,
		MaxRequeues: new(1),
	})
	if err != nil {
		t.Fatal(err)
	}
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
	core.silence(t)
	again := q.dispatched()
	if len(again) != 1 || again[0].Task.ID != lost.Task.ID || again[0].Row == lost.Row {
		t.Fatalf("after the loss the controller dispatched %+v, want %s again under a new task_id", again, lost.Task.ID)
	}
	requeued := again[0]

	// The host refused the requeue unredeemed, and the container runs on past another sweep.
	core.silence(t)
	if got := q.dispatched(); len(got) != 0 {
		t.Errorf("a requeue nobody redeemed went out again as %+v", got)
	}
	conn := dbtest.Superuser(t, super)
	if got, want := dispatchesOf(t, conn, lost.Task.ID), []string{"0 lost runner-1", "1 dispatched -"}; !slices.Equal(got, want) {
		t.Errorf("while the container ran the key holds %q, want %q", got, want)
	}

	ran := core.fromTheRecord(t, succeeded(t, lost.Task, core.now()), lost.Row, "runner-1")
	if err := core.Answer(t.Context(), ran); err != nil {
		t.Fatalf("runner-1's ending of the dispatch it held was refused: %s", err)
	}
	if got := stateOf(t, core); got != agk.Running {
		t.Fatalf("the run is %s, and the ending of the dispatch that was lost is not news", got)
	}
	recorded := core.fromTheRecord(t, succeeded(t, requeued.Task, core.now()), requeued.Row, "runner-1")
	if err := core.Answer(t.Context(), recorded); err != nil {
		t.Fatalf("the ending runner-1 recorded, reported under the requeue's task_id, was refused: %s", err)
	}
	if got := stateOf(t, core); got != agk.Succeeded {
		t.Errorf("the run is %s after one cut and one requeue answered from the record", got)
	}
	if got, want := dispatchesOf(t, conn, lost.Task.ID), []string{"0 lost runner-1", "1 succeeded runner-1"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}
}

// A recorded ending is news the way any ending is. A failure answered from the record is the
// attempt failing, and max: 1 owes the second, which goes out as a new key.
func TestAFailureAnsweredFromTheRecordSpendsItsAttempt(t *testing.T) {
	core, q, conn, lost, requeued := lostAndRequeued(t)
	ran := failed(requeued.Task, 1, core.now())
	ran.DispatchedAt = time.Time{}
	if err := core.Answer(t.Context(), Answer{Result: ran, Row: requeued.Row, Runner: "runner-1"}); err != nil {
		t.Fatalf("the failure runner-1 recorded, reported under the requeue's task_id, was refused: %s", err)
	}
	if second := q.taken(); len(second) != 1 || second[0].Attempt != 2 {
		t.Errorf("after the requeue was answered failed the controller published %+v, want attempt 2", second)
	}
	if got, want := dispatchesOf(t, conn, lost.Task.ID), []string{"0 lost runner-1", "1 failed runner-1"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}
}

// A runner may redeem the requeue after a recorded ending was read and before it was written: the
// requeue went out twice, or its acknowledgement never reached the bus. The redemption bound first,
// so the recorded ending is somebody else's word on the dispatch, refused as such, and nothing of
// it is written.
func TestARecordedEndingIsRefusedOnceARedemptionBoundTheRequeueMeanwhile(t *testing.T) {
	core, q, conn, lost, requeued := lostAndRequeued(t)
	racing, err := NewCore(core.controller, core.term, Options{
		Queue: q, Objects: core.objects, Now: core.now,
		Versions: &redeemsMeanwhile{Versions: core.versions, redeem: func() error { return core.redeem(t, requeued, "runner-2") }},
	})
	if err != nil {
		t.Fatal(err)
	}
	before := seqOf(t, conn)
	recorded := core.fromTheRecord(t, succeeded(t, requeued.Task, core.now()), requeued.Row, "runner-1")
	if err := racing.Answer(t.Context(), recorded); !errors.Is(err, ErrNotAResult) || !errors.Is(err, ErrNotTheHolder) {
		t.Errorf("a recorded ending of a requeue runner-2 redeemed meanwhile answered %v", err)
	}
	if after := seqOf(t, conn); after != before {
		t.Errorf("the refused ending took the run from seq %d to %d", before, after)
	}
	if got, want := dispatchesOf(t, conn, lost.Task.ID), []string{"0 lost runner-1", "1 dispatched runner-2"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}
}

// Holding the dispatch that was lost neither gives a runner the requeue nor keeps it from it. The
// requeue's message may reach runner-1 as well as anybody, and a pull refused there ends it before
// any redemption: runner-1 is the first to report that ending, so it is bound to the requeue by
// it, and runner-2 redeeming the requeue's grant afterwards is told the work is somebody else's.
func TestTheRunnerThatLostADispatchMayEndItsRequeueUnreached(t *testing.T) {
	core, _, conn, lost, requeued := lostAndRequeued(t)
	pulled := Answer{Result: graph.Result{Task: requeued.Task.ID, State: agk.TaskFailed}, Row: requeued.Row, Runner: "runner-1"}
	if err := core.Answer(t.Context(), pulled); err != nil {
		t.Fatalf("runner-1 reporting the requeue never reached a container answered %v", err)
	}
	if got, want := dispatchesOf(t, conn, lost.Task.ID), []string{"0 lost runner-1", "1 failed runner-1"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}
	if err := core.redeem(t, requeued, "runner-2"); !errors.Is(err, db.ErrTaskHeld) {
		t.Errorf("runner-2 redeeming a requeue runner-1 ended answered %v", err)
	}
}

// A task result on the wire names its dispatch by task_id and its unit of work by idempotency_key,
// two fields a runner writes separately, and the bus hands them on unchanged as Row and the key.
// One whose task_id is no dispatch of its key is refused for good, even where both halves are real
// and the runner that sent it holds each of them: it is an answer assembled out of two, and neither
// half says anything about the other. It is refused as no dispatch rather than as somebody else's,
// and it moves neither task, whether it reports an ending or a loss.
func TestAWireResultWhoseTaskIDIsNoDispatchOfItsKeyIsRefused(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.dispatched()
	if len(first) != 1 || first[0].Task.Step != "normalize" {
		t.Fatalf("the first pass dispatched %+v", first)
	}
	normalize := first[0]
	core.answer(t, succeeded(t, normalize.Task, core.now()))
	next := q.dispatched()
	if len(next) != 1 || next[0].Task.Step != "archive" {
		t.Fatalf("normalize's success dispatched %+v", next)
	}
	archive := next[0]
	if err := core.redeem(t, archive, theRunner); err != nil {
		t.Fatal(err)
	}

	// What the bus hands on from a taskResult: task_id as Row, idempotency_key as the task, and
	// the rest of what a container that exited 1 reports.
	at := core.now()
	fromTheWire := func(taskID string, key agk.TaskID, state agk.TaskState) Answer {
		r := graph.Result{Task: key, State: state}
		if state != agk.TaskLost {
			r.ExitCode, r.StartedAt, r.FinishedAt = 1, at, at
		}
		return Answer{Result: r, Row: taskID, Runner: theRunner}
	}

	conn := dbtest.Superuser(t, super)
	before := seqOf(t, conn)
	for _, c := range []struct {
		answer Answer
		why    string
	}{
		{fromTheWire(normalize.Row, archive.Task.ID, agk.TaskFailed), "normalize's task_id under archive's key"},
		{fromTheWire(archive.Row, normalize.Task.ID, agk.TaskFailed), "archive's task_id under normalize's key"},
		{fromTheWire(normalize.Row, archive.Task.ID, agk.TaskLost), "a loss of normalize's task_id under archive's key"},
		{fromTheWire("01M2HZZZZZZZZZZZZZZZZZZZZZ", archive.Task.ID, agk.TaskFailed), "a task_id nobody minted"},
	} {
		err := core.Answer(t.Context(), c.answer)
		switch {
		case !errors.Is(err, ErrNotAResult) || !errors.Is(err, db.ErrNoDispatch):
			t.Errorf("%s answered %v, and it names no dispatch on every delivery", c.why, err)
		case errors.Is(err, ErrNotTheHolder):
			t.Errorf("%s was refused as somebody else's, and the runner holds both halves: %s", c.why, err)
		}
	}

	if after := seqOf(t, conn); after != before {
		t.Errorf("results naming no dispatch of their key took the run from seq %d to %d", before, after)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("results naming no dispatch of their key published %+v", got)
	}
	for key, want := range map[agk.TaskID][]string{
		normalize.Task.ID: {"0 succeeded " + theRunner},
		archive.Task.ID:   {"0 dispatched " + theRunner},
	} {
		if got := dispatchesOf(t, conn, key); !slices.Equal(got, want) {
			t.Errorf("%s holds %q, want %q", key, got, want)
		}
	}

	// And the pair that belongs together is taken.
	if err := core.Answer(t.Context(), fromTheWire(archive.Row, archive.Task.ID, agk.TaskFailed)); err != nil {
		t.Fatalf("archive's own task_id under its own key was refused: %s", err)
	}
	if got := stateOf(t, core); got != agk.Failed {
		t.Errorf("the run is %s after archive failed", got)
	}
}
