package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// The rules a run ends by, played out through the controller rather than through the evaluator
// alone. Every one of them is package graph's and is already tested there; what is tested here
// is that the controller carries them, which is a different claim: a rule the evaluator holds
// and the controller loses on the way to a column is a rule the engine does not have.

// failed is what a runner sends back for a container that exited non-zero. "Which failure it is,
// and whether it is worth another attempt, is read off the exit code and nowhere else."
func failed(task graph.Task, code int, at time.Time) graph.Result {
	return graph.Result{
		Task: task.ID, State: agk.TaskFailed, ExitCode: code,
		DispatchedAt: at, StartedAt: at, FinishedAt: at,
	}
}

// "failed: At least one step failed without continue_on_error."
func TestAStepThatFailsFailsTheRun(t *testing.T) {
	core, q, pool, _ := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	taken := q.taken()
	if len(taken) != 1 {
		t.Fatalf("the first pass published %d tasks", len(taken))
	}
	core.answer(t, failed(taken[0], 1, core.now()))

	if got := stateOf(t, core); got != agk.Failed {
		t.Fatalf("a run whose only reached step failed is %s", got)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("a failed run published %d more tasks", len(got))
	}
}

const tolerantWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: archive, port: ok } }
steps:
  normalize:
    image: ` + theImage + `
    continue_on_error: true
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
  archive:
    image: ` + theImage + `
    when: [succeeded, failed]
    needs:
      - { step: normalize, port: ok, as: orders }
    outputs: [ok]
`

// "continue_on_error: A failure of this step does not fail the run. Downstream steps see the
// failed state through when."
func TestContinueOnErrorKeepsTheRunGoing(t *testing.T) {
	core, q, pool, _ := decidingOn(t, tolerantWorkflow)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	taken := q.taken()
	if len(taken) != 1 || taken[0].Step != "normalize" {
		t.Fatalf("the first pass published %+v", taken)
	}
	core.answer(t, failed(taken[0], 1, core.now()))

	if got := stateOf(t, core); got.Terminal() {
		t.Fatalf("a step declared continue_on_error failed and the run is already %s", got)
	}
	after := q.taken()
	if len(after) != 1 || after[0].Step != "archive" {
		t.Fatalf("the step downstream of a tolerated failure was not published: %+v", after)
	}
	core.answer(t, succeeded(t, after[0], core.now()))

	// "succeeded: Every reached step finished, none failed beyond tolerance."
	if got := stateOf(t, core); got != agk.Succeeded {
		t.Errorf("the run ended in %s, and the only failure was one the workflow tolerated", got)
	}
}

const retryingWorkflow = `
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
    retry:
      max: 2
      on: [failed]
      backoff: { type: exponential, base: 2s, max: 60s }
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
`

// "A transient failure sends the task back to dispatched with an exponential backoff." What the
// controller owes that rule is the clock: it has to write down when to come back, and come back.
func TestARetryWaitsAndThenRunsAgain(t *testing.T) {
	core, q, pool, super := decidingOn(t, retryingWorkflow)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.taken()
	if len(first) != 1 || first[0].Attempt != 1 {
		t.Fatalf("the first pass published %+v", first)
	}
	core.answer(t, failed(first[0], 1, core.now()))

	// The run is not failed: an attempt is owed.
	if got := stateOf(t, core); got.Terminal() {
		t.Fatalf("a step with retry left is %s after one failure", got)
	}
	if got := q.taken(); len(got) != 0 {
		t.Fatalf("the retry was published before its backoff had passed: %+v", got)
	}

	// And the run says when to come back, which is what the sweep orders by.
	conn := dbtest.Superuser(t, super)
	var wake *time.Time
	if err := conn.QueryRow(t.Context(),
		`select wake_at from runs where id = $1`, string(decidedRun)).Scan(&wake); err != nil {
		t.Fatal(err)
	}
	if wake == nil {
		t.Fatal("a run owed a retry has no wake time, so nothing would ever come back for it")
	}
	if !wake.After(core.now()) {
		t.Errorf("the wake time is %s and the clock says %s", wake, core.now())
	}

	// Before the backoff has passed, a sweep finds nothing.
	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	if got := q.taken(); len(got) != 0 {
		t.Fatalf("a sweep before the backoff published %+v", got)
	}

	// After it, the second attempt goes out.
	clock.set(wake.Add(time.Second))
	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	second := q.taken()
	if len(second) != 1 {
		t.Fatalf("after the backoff the sweep published %d tasks", len(second))
	}
	if second[0].Attempt != 2 {
		t.Errorf("the retry is attempt %d", second[0].Attempt)
	}
	if second[0].ID == first[0].ID {
		t.Error("the retry carries the identifier of the attempt that failed, and an attempt is what a task identifier counts")
	}

	// "max counts further attempts, which is why max: 2 is three attempts", so one more
	// failure is owed an attempt and the one after it is not.
	core.answer(t, failed(second[0], 1, core.now()))
	if got := stateOf(t, core); got.Terminal() {
		t.Fatalf("with one further attempt still owed the run is %s", got)
	}
	clock.advance(2 * time.Minute)
	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	third := q.taken()
	if len(third) != 1 || third[0].Attempt != 3 {
		t.Fatalf("the third attempt reads %+v", third)
	}
	core.answer(t, failed(third[0], 1, core.now()))
	if got := stateOf(t, core); got != agk.Failed {
		t.Errorf("after the last of three attempts failed the run is %s", got)
	}
}

const boundedWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
timeout: 1h
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: normalize, port: ok } }
steps:
  normalize:
    image: ` + theImage + `
    timeout: 30m
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
`

// "timed_out: The deadline set by the root timeout of the entry point expired, and the tasks
// still running were stopped."
func TestARunEndsAtItsDeadlineAndStopsWhatItHolds(t *testing.T) {
	core, q, pool, super := decidingOn(t, boundedWorkflow)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	taken := q.taken()
	if len(taken) != 1 {
		t.Fatalf("the first pass published %d tasks", len(taken))
	}

	// The step's own timeout lands on the task, which is what AGK_DEADLINE carries and what
	// the runner stops the container at.
	if taken[0].Deadline.IsZero() {
		t.Fatal("the task carries no deadline, and the runner has nothing to stop the container at")
	}
	conn := dbtest.Superuser(t, super)
	var deadline *time.Time
	if err := conn.QueryRow(t.Context(),
		`select deadline from tasks where idempotency_key = $1`, string(taken[0].ID)).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	if deadline == nil || !deadline.Equal(taken[0].Deadline) {
		t.Errorf("the row says the deadline is %v and the task says %s", deadline, taken[0].Deadline)
	}

	// Past the run's own deadline, the run ends and what it was holding is asked to stop.
	clock.advance(2 * time.Hour)
	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, core); got != agk.TimedOut {
		t.Fatalf("past its root timeout the run is %s", got)
	}
	stops := q.stops()
	if len(stops) != 1 || stops[0].Task != taken[0].ID {
		t.Fatalf("the tasks still running were not stopped: %+v", stops)
	}
	if stops[0].Reason != graph.StopDeadline {
		t.Errorf("the stop says %s", stops[0].Reason)
	}
}

// boundedBothWorkflow is bothAtOnceWorkflow with a root timeout, so that one run reaching its
// deadline holds a task a runner has redeemed and a task still waiting on the queue for one.
const boundedBothWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
timeout: 1h
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

// A run that reaches its deadline ends its tasks where they stand, as a cancelled one does: each
// row reads timed_out, so the namespace holds nothing for a run that has ended, and the row a
// runner redeemed stays bound to it, which is what the heartbeat answers cancel from to a runner
// that missed the stop on agentiik.stops.
func TestARunPastItsDeadlineEndsItsTasksTimedOut(t *testing.T) {
	core, q, pool, super := decidingOn(t, boundedBothWorkflow)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	sent := q.dispatched()
	if len(sent) != 2 {
		t.Fatalf("the first pass published %d tasks", len(sent))
	}
	var held Dispatch
	for _, d := range sent {
		if d.Task.Step == "archive" {
			held = d
		}
	}
	if err := core.redeem(t, held, theRunner); err != nil {
		t.Fatal(err)
	}

	// Woken for the run rather than by a sweep, which after two silent hours would find the
	// redeemed task lost first: its runner is heartbeating it here, only off the bus.
	clock.advance(2 * time.Hour)
	if err := core.Wake(t.Context(), Wake{Run: decidedRun}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, core); got != agk.TimedOut {
		t.Fatalf("past its root timeout the run is %s", got)
	}

	var tasks []db.TaskSummary
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		d, err := ns.RunDetail(ctx, decidedRun)
		tasks = d.Tasks
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("the run has %d tasks", len(tasks))
	}
	for _, task := range tasks {
		if task.State != agk.TaskTimedOut {
			t.Errorf("%s reads %s in a run past its deadline", task.Task, task.State)
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
		t.Errorf("the namespace has %d of its 20 slots free, and the run holding the rest has ended", free)
	}
	var runner *string
	if err := dbtest.Superuser(t, super).QueryRow(t.Context(),
		`select runner from tasks where idempotency_key = $1`, string(held.Task.ID)).Scan(&runner); err != nil {
		t.Fatal(err)
	}
	if runner == nil || *runner != theRunner {
		t.Errorf("the task %s redeemed reads as bound to %v", theRunner, runner)
	}
}

// A pass that published a task and died before recording the dispatch leaves the task pending in
// the document, where the evaluator names nothing to stop. Reaching the deadline stops that task
// all the same, from the row its redemption bound.
func TestADeadlineStopsATaskTakenBeforeItsDispatchWasRecorded(t *testing.T) {
	core, q, pool, _ := decidingOn(t, boundedWorkflow)
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

	clock.advance(2 * time.Hour)
	if err := core.Wake(t.Context(), Wake{Run: decidedRun}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, core); got != agk.TimedOut {
		t.Fatalf("past its root timeout the run is %s", got)
	}
	stops := q.stops()
	if len(stops) != 1 || stops[0].Task != sent[0].Task.ID || stops[0].Reason != graph.StopDeadline {
		t.Errorf("the deadline stopped %+v, and %s had redeemed %s", stops, theRunner, sent[0].Task.ID)
	}
}

// supersedingWorkflow reaches its output through whichever of two steps answers first, so that a
// run of it can end while the other is still running.
const supersedingWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: pick, port: ok } }
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
  pick:
    image: ` + theImage + `
    merge: first
    needs:
      - { step: normalize, port: ok, as: orders }
      - { step: archive, port: ok, as: orders }
    outputs: [ok]
`

// A task a merge: first superseded is stopped on the pass that lifts the barrier on another edge,
// and ended there, while its run goes on: its row reads cancelled rather than in flight until its
// runner answers, it holds no slot of the namespace, and the heartbeat's cancel names it to a runner
// that never heard the stop. The code its container exited with lands on the row when the runner
// reports it, and the run's ending, succeeded or failed, leaves the row as it was.
func TestATaskAMergeFirstSupersededEndsAsItsStopGoesOut(t *testing.T) {
	for _, c := range []struct {
		verdict agk.RunState
		answer  func(t *testing.T, task graph.Task, at time.Time) graph.Result
	}{
		{agk.Succeeded, succeeded},
		{agk.Failed, func(_ *testing.T, task graph.Task, at time.Time) graph.Result { return failed(task, 1, at) }},
	} {
		t.Run(c.verdict.String(), func(t *testing.T) {
			core, q, pool, super := decidingOn(t, supersedingWorkflow)
			joinedAsTheRunner(t, super)
			createRunOf(t, pool, "normalize", "archive", "pick")
			if err := core.Decide(t.Context(), decidedRun); err != nil {
				t.Fatal(err)
			}
			sent := q.dispatched()
			if len(sent) != 2 {
				t.Fatalf("the first pass published %d tasks", len(sent))
			}
			var fast, slow Dispatch
			for _, d := range sent {
				if d.Task.Step == "archive" {
					slow = d
				} else {
					fast = d
				}
			}
			if err := core.redeem(t, slow, theRunner); err != nil {
				t.Fatal(err)
			}

			core.answer(t, succeeded(t, fast.Task, core.now()))
			picked := q.taken()
			if len(picked) != 1 || picked[0].Step != "pick" {
				t.Fatalf("the barrier lifted on normalize and published %+v", picked)
			}
			if stops := q.stops(); !slices.Contains(stops, graph.Stop{Task: slow.Task.ID, Reason: graph.StopSuperseded}) {
				t.Errorf("the pass that lifted the barrier stopped %+v, and %s holds %s", stops, theRunner, slow.Task.ID)
			}
			if got := stateOf(t, core); got != agk.Running {
				t.Fatalf("the run is %s, and pick is still to run", got)
			}

			conn := dbtest.Superuser(t, super)
			var state string
			var finished *time.Time
			if err := conn.QueryRow(t.Context(),
				`select state, finished_at from tasks where id = $1`, slow.Row).Scan(&state, &finished); err != nil {
				t.Fatal(err)
			}
			if state != "cancelled" || finished == nil || !finished.Equal(core.now()) {
				t.Errorf("the superseded task reads %s, finished at %v, as its stop goes out", state, finished)
			}
			var free int
			if err := core.controller.Fenced(t.Context(), core.term, func(ctx context.Context, w *db.Wide) error {
				var err error
				free, err = w.Slots(ctx, "finance")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if free != 19 {
				t.Errorf("the namespace has %d of its 20 slots free, and only pick holds one", free)
			}
			if got := cancelled(t, core, slow.Task.ID); !slices.Equal(got, []agk.TaskID{slow.Task.ID}) {
				t.Errorf("the heartbeat answers cancel %v, and %s holds the superseded %s", got, theRunner, slow.Task.ID)
			}

			at := core.now()
			late := Answer{
				Result: graph.Result{Task: slow.Task.ID, State: agk.TaskCancelled, ExitCode: 143, StartedAt: at, FinishedAt: at},
				Row:    slow.Row, Runner: theRunner,
			}
			if err := core.Answer(t.Context(), late); err != nil {
				t.Fatalf("the late report of the superseded task answered %s", err)
			}
			core.answer(t, c.answer(t, picked[0], core.now()))
			if got := stateOf(t, core); got != c.verdict {
				t.Fatalf("the run is %s, want %s", got, c.verdict)
			}
			var code *int
			if err := conn.QueryRow(t.Context(),
				`select state, exit_code from tasks where id = $1`, slow.Row).Scan(&state, &code); err != nil {
				t.Fatal(err)
			}
			if state != "cancelled" || code == nil || *code != 143 {
				t.Errorf("after its runner reported exit 143 and the run ended, the superseded task reads %s, exit %v", state, code)
			}
		})
	}
}

// A task whose message the bus took but whose acknowledgement never reached the controller is not
// recorded as dispatched, and stays pending in the document; a runner may have redeemed it all the
// same. Superseded, it is ended and stopped in the pass that lifts the barrier, as one the document
// saw dispatched is, rather than when the run ends: its row, which the redemption bound, reads
// cancelled from then on, and the heartbeat's cancel names it to a runner that missed the stop.
func TestASupersededTaskWhoseDispatchWasNeverRecordedIsStoppedAsTheBarrierLifts(t *testing.T) {
	core, _, pool, super := decidingOn(t, supersedingWorkflow)
	joinedAsTheRunner(t, super)
	createRunOf(t, pool, "normalize", "archive", "pick")
	q := &unanswered{step: "archive"}
	core.queue = q
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	fast := q.dispatched()
	if len(fast) != 1 || fast[0].Task.Step != "normalize" || len(q.lost) != 1 {
		t.Fatalf("the first pass published %+v, and the bus took %+v without saying so", fast, q.lost)
	}
	slow := q.lost[0]
	if err := core.redeem(t, slow, theRunner); err != nil {
		t.Fatal(err)
	}

	core.answer(t, succeeded(t, fast[0].Task, core.now()))
	picked := q.taken()
	if len(picked) != 1 || picked[0].Step != "pick" {
		t.Fatalf("the barrier lifted on normalize and published %+v", picked)
	}
	if stops := q.stops(); !slices.Equal(stops, []graph.Stop{{Task: slow.Task.ID, Reason: graph.StopSuperseded}}) {
		t.Errorf("the pass that lifted the barrier stopped %+v, and %s had redeemed the superseded %s", stops, theRunner, slow.Task.ID)
	}
	if state, runner, _ := rowOf(t, super, slow.Row); state != "cancelled" || runner == nil || *runner != theRunner {
		t.Errorf("the superseded task reads %s, bound to %s, while the run goes on", state, shownOf(runner))
	}
	if got := cancelled(t, core, slow.Task.ID); !slices.Equal(got, []agk.TaskID{slow.Task.ID}) {
		t.Errorf("the heartbeat of %s holding %s answers cancel %v", theRunner, slow.Task.ID, got)
	}

	core.answer(t, succeeded(t, picked[0], core.now()))
	if got := stateOf(t, core); got != agk.Succeeded {
		t.Fatalf("the run is %s", got)
	}
}

// unanswered is a bus that takes the message of one step and whose answer never arrives, so the
// controller counts it as not sent.
type unanswered struct {
	fakeQueue
	step agk.Step
	lost []Dispatch
}

func (u *unanswered) Publish(ctx context.Context, d Dispatch) error {
	if d.Task.Step != u.step {
		return u.fakeQueue.Publish(ctx, d)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.lost = append(u.lost, d)
	issued.Store(d.Row, d.Grant)
	return context.DeadlineExceeded
}

// The stop a run's ending sends to a runner still holding one of its tasks is the one the table of
// stops names for why the run ended, and for a run that succeeded or failed that is superseded:
// the only step of it whose tasks can still be running is one a merge: first cancelled.
func TestARunsEndingStopsWhatItHoldsWithTheReasonItEnded(t *testing.T) {
	for run, want := range map[agk.RunState]graph.StopReason{
		agk.TimedOut:  graph.StopDeadline,
		agk.Cancelled: graph.StopCancelled,
		agk.Succeeded: graph.StopSuperseded,
		agk.Failed:    graph.StopSuperseded,
	} {
		if got := stopOf(run); got != want {
			t.Errorf("a %s run stops what it holds with %s, want %s", run, got, want)
		}
	}
}

// "cancelled: Cancelled by a principal holding workflow:run, by a concurrency group or by a
// merge: first."
func TestCancellingARunStopsWhatItHolds(t *testing.T) {
	core, q, pool, _ := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	sent := q.dispatched()
	if len(sent) != 1 {
		t.Fatalf("the first pass published %d tasks", len(sent))
	}
	taken := []graph.Task{sent[0].Task}
	// Taken by a runner before the run is called off, as a runner still finishing when the
	// stop reaches it took it.
	if err := core.redeem(t, sent[0], theRunner); err != nil {
		t.Fatal(err)
	}

	if err := core.Cancel(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, core); got != agk.Cancelled {
		t.Fatalf("a cancelled run is %s", got)
	}
	stops := q.stops()
	if len(stops) != 1 || stops[0].Task != taken[0].ID {
		t.Fatalf("cancelling stopped %+v", stops)
	}
	if stops[0].Reason != graph.StopCancelled {
		t.Errorf("the stop says %s, and the run was cancelled", stops[0].Reason)
	}

	// Cancelling twice is not an error: a principal asking again, or asking about a run that
	// ended while they were asking, has got what they wanted either way.
	if err := core.Cancel(t.Context(), decidedRun); err != nil {
		t.Errorf("cancelling a cancelled run answered %s", err)
	}

	// And a result arriving afterwards changes nothing, which a runner that was already
	// finishing when the stop reached it produces.
	core.answer(t, succeeded(t, taken[0], core.now()))
	if got := stateOf(t, core); got != agk.Cancelled {
		t.Errorf("a late result took a cancelled run to %s", got)
	}
}

// stateOf reads the run's state back through the door the controller uses.
func stateOf(t *testing.T, co *Core) agk.RunState {
	t.Helper()
	var e db.Evaluation
	if err := co.controller.Fenced(t.Context(), co.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		e, err = w.Run(ctx, decidedRun)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return e.State
}

// An exit code is recorded where a container exited, a stopped one included, and where the
// evaluator gave a task it could not build the code for invalid input. A failure or a stop that
// never reached a container has neither, nor has a loss, and recording the 0 such a shard holds
// would be recording success.
func TestAnExitCodeIsRecordedWhereOneWasGiven(t *testing.T) {
	at := time.Date(2026, 9, 10, 6, 41, 9, 0, time.UTC)
	for _, c := range []struct {
		why   string
		shard graph.ShardState
		want  *int
	}{
		{"a success", graph.ShardState{Task: agk.TaskSucceeded, StartedAt: at}, ptr(0)},
		{"a container that exited non-zero", graph.ShardState{Task: agk.TaskFailed, ExitCode: 108, StartedAt: at}, ptr(108)},
		{"a container that exited 0 and whose outputs could not be collected", graph.ShardState{Task: agk.TaskFailed, StartedAt: at}, ptr(0)},
		{"a task the evaluator could not build", graph.ShardState{Task: agk.TaskFailed, ExitCode: 120}, ptr(120)},
		{"a failure that never reached a container", graph.ShardState{Task: agk.TaskFailed}, nil},
		{"a container that broke the output contract", graph.ShardState{Task: agk.TaskFailed, ExitCode: 121, StartedAt: at}, ptr(121)},
		{"a container killed at its deadline after the grace", graph.ShardState{Task: agk.TaskTimedOut, ExitCode: 137, StartedAt: at}, ptr(137)},
		{"a container that obeyed the stop at its deadline", graph.ShardState{Task: agk.TaskTimedOut, ExitCode: 143, StartedAt: at}, ptr(143)},
		{"a container that obeyed a cancellation", graph.ShardState{Task: agk.TaskCancelled, ExitCode: 143, StartedAt: at}, ptr(143)},
		{"a cancelled container that exited 0", graph.ShardState{Task: agk.TaskCancelled, StartedAt: at}, ptr(0)},
		{"a task whose deadline passed before it reached a container", graph.ShardState{Task: agk.TaskTimedOut}, nil},
		{"a task cancelled before it reached a container", graph.ShardState{Task: agk.TaskCancelled}, nil},
		{"a lost task", graph.ShardState{Task: agk.TaskLost, StartedAt: at}, nil},
		{"a stopped container its runner reported no code for", graph.ShardState{Task: agk.TaskTimedOut, StartedAt: at, NoExitCode: true}, nil},
	} {
		c.shard.Attempt = 1
		got := taskOf(decidedRun, "normalize", c.shard).ExitCode
		switch {
		case c.want == nil && got != nil:
			t.Errorf("%s is recorded as exiting %d", c.why, *got)
		case c.want != nil && (got == nil || *got != *c.want):
			t.Errorf("%s is recorded as exiting %v, want %d", c.why, got, *c.want)
		}
	}
}

func ptr(i int) *int { return &i }
