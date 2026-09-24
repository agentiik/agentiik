package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// A task's progress, from the runner holding it to the row the run detail reads. "running: The
// container is running. publishing: The container is finished; its outputs are being collected and
// uploaded." Only the runner sees either, so it says so, and the controller writes it only
// forwards, only from that runner, and never after an ending.

// twoAtOnce dispatches two steps on the first pass, so that one of them ending is a decision
// written while the other is still in flight.
const twoAtOnce = `
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
    outputs: [ok, rejected]
`

// progress is what the runner holding d says of it.
func progress(d Dispatch, state agk.TaskState, runner string) Progress {
	return Progress{Task: d.Task.ID, Row: d.Row, State: state, Runner: runner}
}

// shown is what the run detail says of one task, which is what the API serves.
func shown(t *testing.T, pool *db.Pool, key agk.TaskID) agk.TaskState {
	t.Helper()
	var state agk.TaskState
	found := false
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		d, err := ns.RunDetail(ctx, decidedRun)
		if err != nil {
			return err
		}
		for _, task := range d.Tasks {
			if task.Task == key {
				state, found = task.State, true
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("the run detail shows no task %s", key)
	}
	return state
}

// firstPass decides the run and answers the dispatches of normalize and archive it handed out.
func firstPass(t *testing.T, core *Core, q *fakeQueue) (normalize, archive Dispatch) {
	t.Helper()
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	for _, d := range q.dispatched() {
		switch d.Task.Step {
		case "normalize":
			normalize = d
		case "archive":
			archive = d
		}
	}
	if normalize.Row == "" || archive.Row == "" {
		t.Fatal("the first pass did not dispatch both steps")
	}
	return normalize, archive
}

// The run detail shows a task running while its container runs and publishing while its outputs
// go up, rather than dispatched until it ends. A decision written meanwhile, for the other step's
// ending, knows nothing of either and does not move the task back, and the task's own ending is
// written over both.
func TestATaskReadsRunningWhileItsContainerRuns(t *testing.T) {
	core, q, pool, _ := decidingOn(t, twoAtOnce)
	createRun(t, pool)
	normalize, archive := firstPass(t, core, q)
	if err := core.redeem(t, normalize, theRunner); err != nil {
		t.Fatal(err)
	}
	if got := shown(t, pool, normalize.Task.ID); got != agk.TaskDispatched {
		t.Fatalf("a task redeemed and not yet started shows %s", got)
	}

	if err := core.Progress(t.Context(), progress(normalize, agk.TaskRunning, theRunner)); err != nil {
		t.Fatal(err)
	}
	if got := shown(t, pool, normalize.Task.ID); got != agk.TaskRunning {
		t.Fatalf("a task whose runner said its container started shows %s", got)
	}

	// archive ends, which is a decision writing every task of the run, normalize among them as
	// the evaluator holds it: dispatched.
	core.answer(t, succeeded(t, archive.Task, core.now()))
	if got := shown(t, pool, archive.Task.ID); got != agk.TaskSucceeded {
		t.Fatalf("archive shows %s after its ending", got)
	}
	if got := shown(t, pool, normalize.Task.ID); got != agk.TaskRunning {
		t.Errorf("a decision written while normalize ran moved it back to %s", got)
	}

	if err := core.Progress(t.Context(), progress(normalize, agk.TaskPublishing, theRunner)); err != nil {
		t.Fatal(err)
	}
	if got := shown(t, pool, normalize.Task.ID); got != agk.TaskPublishing {
		t.Fatalf("a task whose container exited shows %s", got)
	}
	// running delivered again, late, is behind publishing and moves nothing.
	if err := core.Progress(t.Context(), progress(normalize, agk.TaskRunning, theRunner)); err != nil {
		t.Errorf("progress delivered late was refused: %s", err)
	}
	if got := shown(t, pool, normalize.Task.ID); got != agk.TaskPublishing {
		t.Errorf("running delivered after publishing moved the task back to %s", got)
	}

	core.answer(t, succeeded(t, normalize.Task, core.now()))
	if got := stateOf(t, core); got != agk.Succeeded {
		t.Fatalf("the run is %s once both steps succeeded", got)
	}
	if got := shown(t, pool, normalize.Task.ID); got != agk.TaskSucceeded {
		t.Errorf("normalize shows %s after its ending", got)
	}
}

// "never after an ending": progress the bus delivers after the result it preceded, as it may once
// either was left for a later delivery, changes nothing and is no error, so the bus takes it off
// the queue.
func TestProgressDeliveredAfterTheEndingChangesNothing(t *testing.T) {
	core, q, pool, super := decidingOn(t, twoAtOnce)
	createRun(t, pool)
	_, archive := firstPass(t, core, q)
	core.answer(t, succeeded(t, archive.Task, core.now()))

	for _, state := range []agk.TaskState{agk.TaskRunning, agk.TaskPublishing} {
		if err := core.Progress(t.Context(), progress(archive, state, theRunner)); err != nil {
			t.Errorf("%s delivered after the ending was refused: %s", state, err)
		}
	}
	conn := dbtest.Superuser(t, super)
	if got, want := dispatchesOf(t, conn, archive.Task.ID), []string{"0 succeeded " + theRunner}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}
}

// A host cut off long enough to be declared lost goes on running its container and says so. The
// dispatch it holds is lost, and stays so, and the requeue somebody else may take is untouched.
func TestProgressOfALostDispatchMovesNothing(t *testing.T) {
	core, q, pool, super := decidingOn(t, requeueingWorkflow)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	lost := q.dispatched()[0]
	if err := core.redeem(t, lost, "runner-1"); err != nil {
		t.Fatal(err)
	}
	core.silence(t)
	if len(q.dispatched()) != 1 {
		t.Fatal("the loss was not requeued")
	}

	if err := core.Progress(t.Context(), progress(lost, agk.TaskRunning, "runner-1")); err != nil {
		t.Errorf("progress of a dispatch declared lost was refused: %s", err)
	}
	conn := dbtest.Superuser(t, super)
	if got, want := dispatchesOf(t, conn, lost.Task.ID), []string{"0 lost runner-1", "1 dispatched -"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}
}

// Taken from the runner the dispatch is bound to and from no other, as a result is: "Its reach is
// the tasks in its hands". Refused the same way, as ErrNotTheHolder wrapped in ErrNotAResult, and
// moving nothing.
func TestProgressIsTakenOnlyFromTheRunnerHoldingTheTask(t *testing.T) {
	core, q, pool, super := decidingOn(t, twoAtOnce)
	createRun(t, pool)
	normalize, archive := firstPass(t, core, q)
	if err := core.redeem(t, normalize, "runner-1"); err != nil {
		t.Fatal(err)
	}

	for name, p := range map[string]Progress{
		"another runner's task":              progress(normalize, agk.TaskRunning, "runner-2"),
		"a task nobody redeemed":             progress(archive, agk.TaskRunning, "runner-1"),
		"a dispatch of another key":          {Task: archive.Task.ID, Row: normalize.Row, State: agk.TaskRunning, Runner: "runner-1"},
		"a dispatch that was never recorded": {Task: normalize.Task.ID, Row: "01M2AAZ9G62NQXFAFCXKRPJEH5", State: agk.TaskRunning, Runner: "runner-1"},
	} {
		err := core.Progress(t.Context(), p)
		if !errors.Is(err, ErrNotAResult) || !errors.Is(err, ErrNotTheHolder) {
			t.Errorf("progress of %s answered %v, want ErrNotTheHolder", name, err)
		}
	}
	conn := dbtest.Superuser(t, super)
	for _, key := range []agk.TaskID{normalize.Task.ID, archive.Task.ID} {
		rows := dispatchesOf(t, conn, key)
		if len(rows) != 1 || !strings.HasPrefix(rows[0], "0 dispatched ") {
			t.Errorf("refused progress left %s as %q", key, rows)
		}
	}
}

// What progress is not: an ending, a state before dispatched, or a message that names no dispatch,
// no runner or no task. Each is the same on every delivery.
func TestWhatIsNotProgress(t *testing.T) {
	core, q, pool, _ := decidingOn(t, twoAtOnce)
	createRun(t, pool)
	normalize, _ := firstPass(t, core, q)
	if err := core.redeem(t, normalize, theRunner); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]Progress{
		"an ending":          progress(normalize, agk.TaskSucceeded, theRunner),
		"dispatched":         progress(normalize, agk.TaskDispatched, theRunner),
		"no dispatch":        {Task: normalize.Task.ID, State: agk.TaskRunning, Runner: theRunner},
		"no runner":          {Task: normalize.Task.ID, Row: normalize.Row, State: agk.TaskRunning},
		"no task":            {Task: "not a key", Row: normalize.Row, State: agk.TaskRunning, Runner: theRunner},
		"a run nobody holds": {Task: "01M2AAZ9G62NQXFAFCXKRPJEH5/normalize/1", Row: normalize.Row, State: agk.TaskRunning, Runner: theRunner},
	} {
		if err := core.Progress(t.Context(), p); !errors.Is(err, ErrNotAResult) {
			t.Errorf("%s was answered %v, want ErrNotAResult", name, err)
		}
	}
	if got := shown(t, pool, normalize.Task.ID); got != agk.TaskDispatched {
		t.Errorf("what is not progress moved the task to %s", got)
	}
}

// A run that has ended has nothing to learn, whatever its rows still say: progress on it moves
// nothing, as a result on it records nothing.
func TestProgressOnARunThatHasEndedMovesNothing(t *testing.T) {
	core, q, pool, super := decidingOn(t, twoAtOnce)
	createRun(t, pool)
	normalize, _ := firstPass(t, core, q)
	if err := core.redeem(t, normalize, theRunner); err != nil {
		t.Fatal(err)
	}
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `update runs set state = 'failed' where id = $1`, string(decidedRun)); err != nil {
		t.Fatal(err)
	}
	if err := core.Progress(t.Context(), progress(normalize, agk.TaskRunning, theRunner)); err != nil {
		t.Errorf("progress on a run that has ended was refused: %s", err)
	}
	if got := shown(t, pool, normalize.Task.ID); got != agk.TaskDispatched {
		t.Errorf("progress on a run that has ended moved its task to %s", got)
	}

	// Nor is it early on a dispatch the pass that published it never recorded: no later
	// delivery will find it recorded, so it is no news rather than a message that comes round
	// for as long as the stream keeps it.
	if _, err := conn.Exec(t.Context(), `update tasks set state = 'pending' where id = $1`, normalize.Row); err != nil {
		t.Fatal(err)
	}
	if err := core.Progress(t.Context(), progress(normalize, agk.TaskRunning, theRunner)); err != nil {
		t.Errorf("progress on a run that has ended, for a dispatch never recorded, answered %v", err)
	}
	if got := shown(t, pool, normalize.Task.ID); got != agk.TaskPending {
		t.Errorf("progress on a run that has ended moved its task to %s", got)
	}
}

// Every controller write carries the term, and so does this one: a former holder that has not
// noticed moves nothing.
func TestProgressIsFenced(t *testing.T) {
	core, q, pool, super := decidingOn(t, twoAtOnce)
	createRun(t, pool)
	normalize, _ := firstPass(t, core, q)
	if err := core.redeem(t, normalize, theRunner); err != nil {
		t.Fatal(err)
	}
	resumeOn(t, pool, super, core)
	if err := core.Progress(t.Context(), progress(normalize, agk.TaskRunning, theRunner)); !errors.Is(err, db.ErrFenced) {
		t.Errorf("a former holder's progress answered %v, want db.ErrFenced", err)
	}
	if got := shown(t, pool, normalize.Task.ID); got != agk.TaskDispatched {
		t.Errorf("a former holder moved the task to %s", got)
	}
}

// A runner quick enough reports a task running before the pass that published it has recorded the
// dispatch. That is early rather than wrong: it is left for a later delivery, not refused and not
// taken as no news, which would lose it for good.
func TestProgressBeforeTheDispatchIsRecordedComesRoundAgain(t *testing.T) {
	core, q, pool, super := decidingOn(t, twoAtOnce)
	createRun(t, pool)
	normalize, _ := firstPass(t, core, q)
	if err := core.redeem(t, normalize, theRunner); err != nil {
		t.Fatal(err)
	}
	// As the rows stand between the message going and the dispatch being recorded.
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `update tasks set state = 'pending' where id = $1`, normalize.Row); err != nil {
		t.Fatal(err)
	}
	err := core.Progress(t.Context(), progress(normalize, agk.TaskRunning, theRunner))
	if !errors.Is(err, db.ErrNotYetDispatched) || errors.Is(err, ErrNotAResult) {
		t.Fatalf("progress before the dispatch was recorded answered %v, want db.ErrNotYetDispatched and no refusal", err)
	}
	if _, err := conn.Exec(t.Context(), `update tasks set state = 'dispatched' where id = $1`, normalize.Row); err != nil {
		t.Fatal(err)
	}
	if err := core.Progress(t.Context(), progress(normalize, agk.TaskRunning, theRunner)); err != nil {
		t.Fatal(err)
	}
	if got := shown(t, pool, normalize.Task.ID); got != agk.TaskRunning {
		t.Errorf("progress delivered again once the dispatch was recorded left the task %s", got)
	}
}
