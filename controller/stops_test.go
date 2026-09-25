package controller

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// A stop a rule of the language sends while the run goes on, superseded or sibling_failed, ends its
// task as it goes out, so that the heartbeat's cancel repeats it to a runner that never heard it on
// agentiik.stops, which keeps nothing.

// failingFastWorkflow fans invoice out with fail_fast beside archive, which keeps the run going
// once invoice has failed.
const failingFastWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: invoice, port: ok } }
steps:
  invoice:
    image: ` + theImage + `
    inputs:
      orders: ${{ workflow.inputs.orders }}
    strategy: { fan_out: item, fail_fast: true }
    outputs: [ok]
  archive:
    image: ` + theImage + `
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok]
`

// joinedAsTheRunner writes theRunner as a runner of the pool default, which is what a heartbeat
// is recorded against.
func joinedAsTheRunner(t *testing.T, super string) {
	t.Helper()
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `
		insert into runners (id, pool, cpu, memory_bytes, disk_bytes, architecture, agent_version,
		                     credential_hash, rotate_by, public_key)
		values ($1, 'default', 4, 8589934592, 137438953472, 'amd64', '0.2.0',
		        repeat('a', 64), now() + interval '30 days', decode(repeat('00', 32), 'hex'))`,
		theRunner); err != nil {
		t.Fatal(err)
	}
}

// cancelled is what the heartbeat answers theRunner to stop, holding keys.
func cancelled(t *testing.T, co *Core, keys ...agk.TaskID) []agk.TaskID {
	t.Helper()
	var beaten db.Beaten
	if err := co.controller.pool.Installation(t.Context(), db.Heartbeat, func(ctx context.Context, w *db.Wide) error {
		var err error
		beaten, err = w.Beat(ctx, theRunner, db.Beating{AgentVersion: "0.2.0", State: "ready", Concurrency: 4, Holding: keys}, co.now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return beaten.Cancel
}

// rowOf reads one dispatch's state, runner and exit code.
func rowOf(t *testing.T, super, row string) (state string, runner *string, code *int) {
	t.Helper()
	if err := dbtest.Superuser(t, super).QueryRow(t.Context(),
		`select state, runner, exit_code from tasks where id = $1`, row).Scan(&state, &runner, &code); err != nil {
		t.Fatal(err)
	}
	return state, runner, code
}

// logOf reads where one dispatch's log went, how many lines it had, whether it was cut and what
// the attempt cost.
func logOf(t *testing.T, super, row string) (uri *string, lines *int, cut bool, usage map[string]any) {
	t.Helper()
	if err := dbtest.Superuser(t, super).QueryRow(t.Context(),
		`select log_uri, log_lines, log_truncated, usage from tasks where id = $1`, row).Scan(&uri, &lines, &cut, &usage); err != nil {
		t.Fatal(err)
	}
	return uri, lines, cut, usage
}

// fail_fast stops the shard still running beside the one that failed, and its runner never hears
// the stop. The shard ends cancelled in the pass that sends it, while the run goes on: its step is
// judged at once rather than when the container reaches its deadline, the heartbeat names it
// until the runner reports, its runner's progress does not move it back, and the code its
// container exits with, its log and its usage land on the row when the report comes, and stay
// there through the decisions that follow.
func TestAShardFailFastStoppedIsRepeatedByTheHeartbeatWhileTheRunGoesOn(t *testing.T) {
	core, q, pool, super := decidingOn(t, failingFastWorkflow)
	joinedAsTheRunner(t, super)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.CreateRun(ctx, db.NewRun{
			ID: decidedRun, Workflow: "monthly-invoicing", Commit: "a3f9c1e",
			Trigger: agk.TriggerManual, TriggeredBy: "alice",
			Inputs: json.RawMessage(`{"orders": [{"customer_id": "C-1042"}, {"customer_id": "C-1043"}]}`),
			Steps:  []agk.Step{"invoice", "archive"},
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	var first, second, archive Dispatch
	for _, d := range q.dispatched() {
		switch {
		case d.Task.Step == "archive":
			archive = d
		case d.Task.Shard.Index == 1:
			first = d
		default:
			second = d
		}
	}
	if first.Row == "" || second.Row == "" || archive.Row == "" {
		t.Fatal("the first pass did not dispatch both shards of invoice and archive")
	}
	for _, d := range []Dispatch{first, second, archive} {
		if err := core.redeem(t, d, theRunner); err != nil {
			t.Fatal(err)
		}
	}
	if err := core.Progress(t.Context(), progress(second, agk.TaskRunning, theRunner)); err != nil {
		t.Fatal(err)
	}

	q.stops()
	core.answer(t, failed(first.Task, 7, core.now()))
	if stops := q.stops(); !slices.Contains(stops, graph.Stop{Task: second.Task.ID, Reason: graph.StopSiblingFailed}) {
		t.Fatalf("the failure of the first shard stopped %+v, and the second was running beside it", stops)
	}
	if got := stateOf(t, core); got != agk.Running {
		t.Fatalf("the run is %s, and archive is still in flight", got)
	}
	if state, runner, code := rowOf(t, super, second.Row); state != "cancelled" || runner == nil || *runner != theRunner || code != nil {
		t.Errorf("the stopped shard reads %s, bound to %v, exit %v, as its stop goes out", state, runner, code)
	}
	var verdict string
	if err := dbtest.Superuser(t, super).QueryRow(t.Context(),
		`select state::text from steps where run_id = $1 and step = 'invoice'`, string(decidedRun)).Scan(&verdict); err != nil {
		t.Fatal(err)
	}
	if verdict != "failed" {
		t.Errorf("invoice reads %s once its failed shard stopped the other", verdict)
	}

	// The runner never heard the stop and still holds both keys it has not reported.
	if got := cancelled(t, core, second.Task.ID, archive.Task.ID); !slices.Equal(got, []agk.TaskID{second.Task.ID}) {
		t.Errorf("the heartbeat answers cancel %v, and the runner holds the stopped %s", got, second.Task.ID)
	}
	if err := core.Progress(t.Context(), progress(second, agk.TaskPublishing, theRunner)); err != nil {
		t.Errorf("progress of the stopped shard was refused: %s", err)
	}
	if state, _, _ := rowOf(t, super, second.Row); state != "cancelled" {
		t.Errorf("progress moved the stopped shard to %s", state)
	}

	at := core.now()
	log, err := agk.NewLogURI(second.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	late := Answer{
		Result: graph.Result{Task: second.Task.ID, State: agk.TaskCancelled, ExitCode: 143, StartedAt: at, FinishedAt: at},
		Row:    second.Row, Runner: theRunner,
		Log: log, LogLines: 42, LogCut: true,
		Usage: map[string]any{"cpu_seconds": 1.5, "image_pull_ms": 7.0},
	}
	if err := core.Answer(t.Context(), late); err != nil {
		t.Fatalf("the report of the stopped shard answered %s", err)
	}
	if state, _, code := rowOf(t, super, second.Row); state != "cancelled" || code == nil || *code != 143 {
		t.Errorf("after its runner reported exit 143 the stopped shard reads %s, exit %v", state, code)
	}
	reported := func(when string) {
		t.Helper()
		uri, lines, cut, usage := logOf(t, super, second.Row)
		if uri == nil || *uri != log.String() || lines == nil || *lines != 42 || !cut {
			t.Errorf("%s the stopped shard's log reads %v, %v lines, cut %t", when, uri, lines, cut)
		}
		if usage["cpu_seconds"] != 1.5 || usage["image_pull_ms"] != 7.0 {
			t.Errorf("%s the stopped shard's usage reads %v", when, usage)
		}
	}
	reported("after its runner reported")
	if got := cancelled(t, core, archive.Task.ID); len(got) != 0 {
		t.Errorf("the heartbeat answers cancel %v to a runner that reported the stopped shard", got)
	}

	core.answer(t, succeeded(t, archive.Task, core.now()))
	if got := stateOf(t, core); got != agk.Failed {
		t.Fatalf("the run is %s once archive succeeded beside a failed invoice", got)
	}
	if state, _, code := rowOf(t, super, second.Row); state != "cancelled" || code == nil || *code != 143 {
		t.Errorf("once the run ended the stopped shard reads %s, exit %v", state, code)
	}
	reported("once the run ended")
}

// A runner that finished the task before the stop reached it reports how it ended, and the task
// stays as the stop ended it, taking the code: the step was judged when the stop went out.
func TestAStoppedShardThatSucceededAnywayKeepsItsStop(t *testing.T) {
	core, q, pool, super := decidingOn(t, failingFastWorkflow)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.CreateRun(ctx, db.NewRun{
			ID: decidedRun, Workflow: "monthly-invoicing", Commit: "a3f9c1e",
			Trigger: agk.TriggerManual, TriggeredBy: "alice",
			Inputs: json.RawMessage(`{"orders": [{"customer_id": "C-1042"}, {"customer_id": "C-1043"}]}`),
			Steps:  []agk.Step{"invoice", "archive"},
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	var first, second Dispatch
	for _, d := range q.dispatched() {
		switch {
		case d.Task.Step == "archive":
		case d.Task.Shard.Index == 1:
			first = d
		default:
			second = d
		}
	}
	if err := core.redeem(t, second, theRunner); err != nil {
		t.Fatal(err)
	}
	core.answer(t, failed(first.Task, 7, core.now()))

	clock.advance(time.Second)
	core.answer(t, succeeded(t, second.Task, core.now()))
	if state, _, code := rowOf(t, super, second.Row); state != "cancelled" || code == nil || *code != 0 {
		t.Errorf("a stopped shard that succeeded anyway reads %s, exit %v", state, code)
	}
	if got := stateOf(t, core); got != agk.Running {
		t.Errorf("the run is %s, and archive is still in flight", got)
	}
}

// supersedingBesideAVault is supersedingWorkflow with a third step, vault, that waits on nothing
// and so waits only on the namespace's quota.
const supersedingBesideAVault = supersedingWorkflow + `  vault:
    image: ` + theImage + `
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok]
`

// The slot a superseded task held is free in the pass that stops it, and what the quota held back
// goes out in that pass rather than on the next sweep: the runner's report of the stopped task
// comes to a task that is over and decides nothing.
func TestTheSlotAStopFreesIsFreeInThePassThatStopsIt(t *testing.T) {
	core, q, pool, super := decidingOn(t, supersedingBesideAVault)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(),
		`update namespaces set max_concurrent_tasks = 2 where name = 'finance'`); err != nil {
		t.Fatal(err)
	}
	createRunOf(t, pool, "normalize", "archive", "pick", "vault")
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	var fast, slow Dispatch
	for _, d := range q.dispatched() {
		switch d.Task.Step {
		case "normalize":
			fast = d
		case "archive":
			slow = d
		default:
			t.Fatalf("the first pass published %s, and the test wants normalize and archive first", d.Task.Step)
		}
	}
	if fast.Row == "" || slow.Row == "" {
		t.Fatal("the first pass did not publish normalize and archive")
	}
	if err := core.redeem(t, slow, theRunner); err != nil {
		t.Fatal(err)
	}

	core.answer(t, succeeded(t, fast.Task, core.now()))
	if stops := q.stops(); !slices.Contains(stops, graph.Stop{Task: slow.Task.ID, Reason: graph.StopSuperseded}) {
		t.Fatalf("the pass that lifted the barrier stopped %+v", stops)
	}
	var went []agk.Step
	for _, task := range q.taken() {
		went = append(went, task.Step)
	}
	slices.Sort(went)
	if !slices.Equal(went, []agk.Step{"pick", "vault"}) {
		t.Errorf("with archive stopped and normalize over, the pass that stopped archive published %v of pick and vault", went)
	}
}
