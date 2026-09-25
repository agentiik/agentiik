package graph

import (
	"slices"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// A task stopped as superseded or sibling_failed while the run goes on ends cancelled in the pass
// that names its stop, which is where agk run --local and a server both read it.

// failingFast fans invoice out over two orders with fail_fast, and cleanup reads invoice whatever
// became of it, so a pass that judges invoice can start cleanup.
const failingFast = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
steps:
  invoice:
    image: ` + image + `
    inputs:
      orders: ${{ workflow.inputs.orders }}
    strategy: { fan_out: item, fail_fast: true }
    outputs: [ok]
  cleanup:
    image: ` + image + `
    needs:
      - { step: invoice, port: ok, as: orders }
    when: [always]
    outputs: [ok]
`

// twoOrders is what failingFast is started with.
var twoOrders = Options{Inputs: map[string]any{"orders": []any{
	map[string]any{"customer_id": "c1"},
	map[string]any{"customer_id": "c2"},
}}}

// failedFast runs failingFast to the pass where the first shard of invoice has failed with the
// second running beside it, and answers that pass's plan and the second shard's task.
func failedFast(t *testing.T) (*Evaluator, Plan, Task) {
	t.Helper()
	e := started(t, failingFast, twoOrders)
	plan := next(t, e, runAt)
	first, second := plan.Start[0], plan.Start[1]
	record(t, e, Result{Task: first.ID, State: agk.TaskDispatched, DispatchedAt: runAt}, runAt)
	record(t, e, Result{Task: second.ID, State: agk.TaskDispatched, DispatchedAt: runAt}, runAt)
	record(t, e, Result{Task: second.ID, State: agk.TaskRunning, StartedAt: runAt}, runAt)
	record(t, e, Result{Task: first.ID, State: agk.TaskFailed, ExitCode: 7, StartedAt: runAt, FinishedAt: runAt.Add(time.Minute)}, runAt.Add(time.Minute))
	return e, next(t, e, runAt.Add(2*time.Minute)), second
}

// fail_fast "stops the shards still running beside it, freeing their runners at once": the pass
// that names the stop ends the sibling cancelled, judges the step failed on the shard that failed,
// and goes round again to start what follows it, keeping the stop the first round named. The next
// pass names the stop no more.
func TestAShardFailFastStopsEndsAsItsStopGoesOut(t *testing.T) {
	e, plan, second := failedFast(t)
	stopped := Stop{Task: second.ID, Reason: StopSiblingFailed}
	if !slices.Equal(plan.Stop, []Stop{stopped}) {
		t.Fatalf("the pass that heard the failure stops %+v, want %+v", plan.Stop, stopped)
	}
	invoice := e.State().Steps["invoice"]
	sh := invoice.Shards[1]
	if sh.Task != agk.TaskCancelled || !sh.Stopped || !sh.NoExitCode || !sh.FinishedAt.Equal(runAt.Add(2*time.Minute)) {
		t.Errorf("the stopped shard is %s, stopped %t, no exit code %t, finished at %s, as its stop goes out", sh.Task, sh.Stopped, sh.NoExitCode, sh.FinishedAt)
	}
	if invoice.Verdict != agk.VerdictFailed {
		t.Errorf("invoice is %s once its failed shard stopped the other", invoice.Verdict)
	}
	if len(plan.Start) != 1 || plan.Start[0].Step != "cleanup" {
		t.Errorf("the pass that judged invoice starts %s, want cleanup", starts(plan))
	}

	again := next(t, e, runAt.Add(2*time.Minute))
	if len(again.Stop) != 0 {
		t.Errorf("a second pass stops %+v, and the stopped shard is over", again.Stop)
	}
}

// merge: first cancels the step behind the edge it abandoned, and that step's task in flight ends
// cancelled in the same pass rather than when its driver reports.
func TestATaskAMergeFirstSupersededEndsAsItsStopGoesOut(t *testing.T) {
	e := started(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: whichever, namespace: finance }
steps:
  quick:
    image: `+image+`
    outputs: [ok]
  slow:
    image: `+image+`
    outputs: [ok]
  whichever:
    image: `+image+`
    needs:
      - { step: quick, port: ok, as: orders }
      - { step: slow,  port: ok, as: orders }
    merge: first
    outputs: [ok]
`, Options{})
	plan := next(t, e, runAt)
	slow := taskOf(t, plan, "slow")
	record(t, e, Result{Task: slow.ID, State: agk.TaskDispatched, DispatchedAt: runAt}, runAt)
	record(t, e, succeeded(taskOf(t, plan, "quick"), ports("ok", item("a1"))), runAt.Add(time.Minute))

	plan = next(t, e, runAt.Add(2*time.Minute))
	if !slices.Equal(plan.Stop, []Stop{{Task: slow.ID, Reason: StopSuperseded}}) {
		t.Fatalf("the pass that lifted the barrier stops %+v", plan.Stop)
	}
	if sh := e.State().Steps["slow"].Shards[0]; sh.Task != agk.TaskCancelled || !sh.Stopped || !sh.NoExitCode {
		t.Errorf("the superseded shard is %s, stopped %t, no exit code %t, as its stop goes out", sh.Task, sh.Stopped, sh.NoExitCode)
	}
	if again := next(t, e, runAt.Add(2*time.Minute)); len(again.Stop) != 0 {
		t.Errorf("a second pass stops %+v, and the superseded shard is over", again.Stop)
	}
}

// The driver's report of a stopped shard comes to a shard that is over. A sibling that exited 0
// in the moment before the stop reached it stays cancelled and publishes nothing, and its code is
// kept, once, as a decision; nothing else about the report is.
func TestTheReportOfAStoppedShardAddsItsExitCodeAndNothingElse(t *testing.T) {
	e, _, second := failedFast(t)
	was := e.State().Seq
	at := runAt.Add(3 * time.Minute)

	late := succeeded(second, ports("ok", item("c2")))
	late.StartedAt, late.FinishedAt = runAt, at
	record(t, e, late, at)
	sh := e.State().Steps["invoice"].Shards[1]
	if sh.Task != agk.TaskCancelled || sh.ExitCode != 0 || sh.NoExitCode || len(sh.Ports) != 0 {
		t.Errorf("after it reported exit 0 the stopped shard is %s, exit %d, no exit code %t, ports %v", sh.Task, sh.ExitCode, sh.NoExitCode, sh.Ports)
	}
	if !sh.FinishedAt.Equal(runAt.Add(2*time.Minute)) || !sh.StartedAt.Equal(runAt) {
		t.Errorf("the stopped shard started at %s and finished at %s, and it ended when its stop went out", sh.StartedAt, sh.FinishedAt)
	}
	if e.State().Seq != was+1 {
		t.Errorf("taking the code counted %d decisions, want one", e.State().Seq-was)
	}
	if got := e.State().Steps["invoice"].Verdict; got != agk.VerdictFailed {
		t.Errorf("invoice is %s after the stopped shard reported", got)
	}

	// Once: a second report of the same container, or of a code the stop caused, changes
	// nothing.
	record(t, e, Result{Task: second.ID, State: agk.TaskCancelled, ExitCode: 143, StartedAt: runAt, FinishedAt: at}, at)
	if sh := e.State().Steps["invoice"].Shards[1]; sh.ExitCode != 0 || e.State().Seq != was+1 {
		t.Errorf("a second report moved the stopped shard to exit %d", sh.ExitCode)
	}
}

// What cannot say how a container the stop reached exited adds nothing: a loss, an ending that
// never started a container, one that reported no code, and the ending of another dispatch.
func TestAReportOfAStoppedShardWithNoContainerAddsNothing(t *testing.T) {
	for name, r := range map[string]Result{
		"lost":         {State: agk.TaskLost, ExitCode: 143, StartedAt: runAt, FinishedAt: runAt},
		"never ran":    {State: agk.TaskFailed, ExitCode: 125, FinishedAt: runAt},
		"no exit code": {State: agk.TaskCancelled, NoExitCode: true, StartedAt: runAt, FinishedAt: runAt},
		"requeued":     {State: agk.TaskCancelled, ExitCode: 143, Requeue: 1, StartedAt: runAt, FinishedAt: runAt},
		"not over":     {State: agk.TaskPublishing, StartedAt: runAt},
	} {
		t.Run(name, func(t *testing.T) {
			e, _, second := failedFast(t)
			was := e.State().Seq
			r.Task = second.ID
			record(t, e, r, runAt.Add(3*time.Minute))
			sh := e.State().Steps["invoice"].Shards[1]
			if sh.Task != agk.TaskCancelled || !sh.NoExitCode || sh.Requeue != 0 || e.State().Seq != was {
				t.Errorf("the stopped shard is %s, no exit code %t, on dispatch %d, after %d decisions", sh.Task, sh.NoExitCode, sh.Requeue, e.State().Seq-was)
			}
		})
	}
}

// A shard a driver reported cancelled was not stopped by the evaluator, and a later report of it
// is a duplicate like any other.
func TestAShardADriverReportedCancelledTakesNoLaterCode(t *testing.T) {
	e := started(t, oneStep, Options{})
	plan := next(t, e, runAt)
	record(t, e, Result{Task: plan.Start[0].ID, State: agk.TaskCancelled, NoExitCode: true, FinishedAt: runAt}, runAt)
	was := e.State().Seq
	record(t, e, Result{Task: plan.Start[0].ID, State: agk.TaskCancelled, ExitCode: 143, StartedAt: runAt, FinishedAt: runAt}, runAt)
	if sh := e.State().Steps["invoice"].Shards[0]; !sh.NoExitCode || e.State().Seq != was {
		t.Errorf("a duplicate report gave a code to a shard its driver ended: %+v", sh)
	}
}

// "max_parallel: 1 makes it a staged rollout, fail_fast stops it at the first broken region": a
// region nobody has handed out yet never starts once one has failed for good. It ends cancelled
// in the pass that hears the failure, which judges the step there, and its stop is named in case
// a server published it without recording the dispatch.
func TestFailFastStartsNoRegionAfterTheBrokenOne(t *testing.T) {
	e := started(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: rollout, namespace: finance }
steps:
  deploy:
    image: `+image+`
    strategy:
      matrix: { region: [eu-west, eu-central, us-east] }
      max_parallel: 1
      fail_fast: true
    outputs: [ok]
`, Options{})
	plan := next(t, e, runAt)
	if len(plan.Start) != 1 {
		t.Fatalf("the first pass starts %s, and max_parallel is 1", starts(plan))
	}
	first := plan.Start[0]
	record(t, e, Result{Task: first.ID, State: agk.TaskDispatched, DispatchedAt: runAt}, runAt)
	record(t, e, Result{Task: first.ID, State: agk.TaskFailed, ExitCode: 1, StartedAt: runAt, FinishedAt: runAt}, runAt)

	plan = next(t, e, runAt.Add(time.Minute))
	if len(plan.Start) != 0 {
		t.Errorf("the pass that heard %s fail starts %s", first.ID, starts(plan))
	}
	deploy := e.State().Steps["deploy"]
	if deploy.Verdict != agk.VerdictFailed {
		t.Errorf("deploy is %s once its first region failed", deploy.Verdict)
	}
	var stopped []agk.TaskID
	for _, sh := range deploy.Shards[1:] {
		if sh.Task != agk.TaskCancelled || !sh.Stopped || !sh.DispatchedAt.IsZero() {
			t.Errorf("the region of shard %d is %s, stopped %t, dispatched at %s", sh.Shard.Index, sh.Task, sh.Stopped, sh.DispatchedAt)
		}
		stopped = append(stopped, e.taskID("deploy", sh))
	}
	var named []agk.TaskID
	for _, s := range plan.Stop {
		if s.Reason == StopSiblingFailed {
			named = append(named, s.Task)
		}
	}
	if !slices.Equal(named, stopped) {
		t.Errorf("the pass names the stops of %v, want %v", named, stopped)
	}
	if e.State().Run.State != agk.Failed {
		t.Errorf("the run is %s", e.State().Run.State)
	}
}

// A sibling waiting out a backoff has another attempt coming, and fail_fast calls that attempt off
// too: the step is judged in the pass that hears a shard fail for good, not when the backoff ends.
func TestFailFastCallsOffASiblingsNextAttempt(t *testing.T) {
	e := started(t, replace(failingFast, "    strategy: { fan_out: item, fail_fast: true }\n",
		"    strategy: { fan_out: item, fail_fast: true }\n    retry: { max: 2, on: [failed], backoff: { type: exponential, base: 30s, max: 60s } }\n"), twoOrders)
	plan := next(t, e, runAt)
	first, second := plan.Start[0], plan.Start[1]
	record(t, e, Result{Task: second.ID, State: agk.TaskFailed, ExitCode: 1, StartedAt: runAt, FinishedAt: runAt}, runAt)
	if sh := e.State().Steps["invoice"].Shards[1]; sh.NextAttemptAt.IsZero() {
		t.Fatal("the second shard has no attempt coming, so this is not the case under test")
	}
	record(t, e, Result{Task: first.ID, State: agk.TaskFailed, ExitCode: 120, StartedAt: runAt, FinishedAt: runAt}, runAt)

	next(t, e, runAt.Add(time.Second))
	invoice := e.State().Steps["invoice"]
	if sh := invoice.Shards[1]; sh.Task != agk.TaskCancelled || !sh.NextAttemptAt.IsZero() {
		t.Errorf("the second shard is %s with an attempt at %s", sh.Task, sh.NextAttemptAt)
	}
	if invoice.Verdict != agk.VerdictFailed {
		t.Errorf("invoice is %s once its first shard failed for good", invoice.Verdict)
	}
}
