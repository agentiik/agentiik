package graph

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// The evaluator, run end to end with nothing behind it: no controller, no bus, no
// database and no driver. Every test here is a rule the documentation states, played out
// as a run, because a rule that holds in isolation and not in a run is a rule the engine
// does not have.

// The whole path, in the documentation's own order: the controller evaluates the graph
// and finds the steps whose every declared input port is satisfied, creates one task per
// shard, takes back a result, evaluates again, and the run reaches a terminal state.
func TestARunGoesFromTheFirstStepToTheOutputs(t *testing.T) {
	e := started(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: archive, port: ok } }
steps:
  normalize:
    image: `+image+`
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
  archive:
    image: `+image+`
    needs:
      - { step: normalize, port: ok, as: orders }
    outputs: [ok]
`, Options{Inputs: map[string]any{"orders": []any{map[string]any{"customer_id": "c1"}}}})

	// The first evaluation finds normalize alone: archive's port is not satisfied.
	plan := next(t, e, runAt)
	if len(plan.Start) != 1 || plan.Start[0].Step != "normalize" {
		t.Fatalf("the first plan starts %s, want normalize alone: archive waits on a port nobody has published", starts(plan))
	}
	if got := plan.Start[0].Inputs["orders"]; got.Meta.Count != 1 {
		t.Errorf("normalize is given %d items on orders, want the one the workflow input carries", got.Meta.Count)
	}
	if plan.Start[0].ID != "01HZXRUN/normalize/1" {
		t.Errorf("task %s, want the identifier composed of the run, the step and the attempt", plan.Start[0].ID)
	}

	record(t, e, succeeded(plan.Start[0], ports("ok", item("a1"))), runAt.Add(time.Minute))

	plan = next(t, e, runAt.Add(2*time.Minute))
	if len(plan.Start) != 1 || plan.Start[0].Step != "archive" {
		t.Fatalf("the second plan starts %s, want archive: its port is satisfied now", starts(plan))
	}
	if got := plan.Start[0].Inputs["orders"]; got.Meta.Count != 1 || got.Items[0].ID != "a1" {
		t.Errorf("archive is given %#v, want the item normalize published, by the identity normalize gave it", got.Items)
	}

	record(t, e, succeeded(plan.Start[0], ports("ok", item("a1"))), runAt.Add(3*time.Minute))
	plan = next(t, e, runAt.Add(4*time.Minute))
	if len(plan.Start) != 0 {
		t.Errorf("the run started %d more tasks, and every step has ended", len(plan.Start))
	}
	if e.State().Run.State != agk.Succeeded {
		t.Fatalf("the run is %s, want succeeded: every reached step finished and none failed", e.State().Run.State)
	}

	out, err := e.Outputs()
	if err != nil {
		t.Fatalf("the workflow outputs: %v", err)
	}
	if got := out["invoices"]; got.Meta.Count != 1 || got.Meta.Step != "archive" {
		t.Errorf("the output invoices is %#v, want the port of archive it is a view of", got.Meta)
	}
}

// "Nothing in a Plan has happened yet", so asking twice at the same moment asks the same
// question. A controller that dies between deciding and dispatching loses the work of
// deciding and nothing else.
func TestNextAtTheSameMomentDecidesTheSameThing(t *testing.T) {
	e := started(t, oneStep, Options{})
	first := next(t, e, runAt)
	second := next(t, e, runAt)
	if !reflect.DeepEqual(first, second) {
		t.Errorf("two plans at one moment differ:\n%#v\n%#v", first, second)
	}
}

// A controller sweeps a run waiting on its tasks every few seconds, and writes a decision
// wherever the sequence moved. A pass with nothing new is no decision, however many there are,
// and the first pass after something new is one at once.
func TestAPassThatChangesNothingIsNoDecision(t *testing.T) {
	e := started(t, twoSteps, Options{})
	plan := next(t, e, runAt)
	if e.State().Seq != 1 {
		t.Fatalf("the pass that started the run counted %d decisions, want one", e.State().Seq)
	}
	record(t, e, Result{Task: plan.Start[0].ID, State: agk.TaskDispatched, DispatchedAt: runAt}, runAt)
	decided := e.State().Seq

	for i := 1; i <= 4; i++ {
		at := runAt.Add(time.Duration(i) * 10 * time.Second)
		if again := next(t, e, at); len(again.Start) != 0 || len(again.Stop) != 0 || !again.Wake.IsZero() {
			t.Errorf("pass %d over a run waiting on its task planned %+v", i, again)
		}
		if e.State().Seq != decided {
			t.Fatalf("pass %d over a run nothing changed took the sequence from %d to %d", i, decided, e.State().Seq)
		}
	}

	record(t, e, succeeded(plan.Start[0], ports("ok", item("a1"))), runAt.Add(time.Minute))
	heard := e.State().Seq
	if plan = next(t, e, runAt.Add(time.Minute)); len(plan.Start) != 1 || plan.Start[0].Step != "archive" {
		t.Fatalf("the pass after the result starts %s, want archive", starts(plan))
	}
	if e.State().Seq != heard+1 {
		t.Errorf("the pass that started archive counted %d decisions, want one", e.State().Seq-heard)
	}
}

// "A step whose if condition is false moves to skipped and publishes empty envelopes on
// all its ports. Downstream steps decide their own fate through when."
func TestAFalseConditionSkipsTheStepAndStillPublishes(t *testing.T) {
	doc := `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
steps:
  normalize:
    image: ` + image + `
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
  invoice:
    image: ` + image + `
    needs:
      - { step: normalize, port: ok, as: orders }
    if: ${{ inputs.orders.count > 0 }}
    outputs: [ok]
  archive:
    image: ` + image + `
    needs:
      - { step: invoice, port: ok, as: orders }
    when: [succeeded, skipped]
    outputs: [ok]
`
	e := started(t, doc, Options{Inputs: map[string]any{"orders": []any{}}})

	plan := next(t, e, runAt)
	record(t, e, succeeded(plan.Start[0], ports("ok")), runAt.Add(time.Minute))

	plan = next(t, e, runAt.Add(2*time.Minute))
	invoice := e.State().Steps["invoice"]
	if invoice.Verdict != agk.VerdictSkipped {
		t.Fatalf("invoice is %s, want skipped: its condition read an empty port", invoice.Verdict)
	}
	if got, ok := invoice.Ports["ok"]; !ok || got.Meta.Count != 0 {
		t.Errorf("invoice published %#v on ok, want the empty envelope a skipped step publishes on all its ports", got)
	}
	// archive names skipped in its when, so it starts on a batch of nothing.
	if len(plan.Start) != 1 || plan.Start[0].Step != "archive" {
		t.Fatalf("the plan starts %s, want archive: its when names skipped", starts(plan))
	}

	// The same graph with the default when leaves archive skipped instead.
	e = started(t, replace(doc, "    when: [succeeded, skipped]\n", ""), Options{Inputs: map[string]any{"orders": []any{}}})
	plan = next(t, e, runAt)
	record(t, e, succeeded(plan.Start[0], ports("ok")), runAt.Add(time.Minute))
	next(t, e, runAt.Add(2*time.Minute))
	if got := e.State().Steps["archive"].Verdict; got != agk.VerdictSkipped {
		t.Errorf("archive is %s, want skipped: when defaults to [succeeded] and invoice was skipped", got)
	}
	if e.State().Run.State != agk.Succeeded {
		t.Errorf("the run is %s, want succeeded: a skipped step is not a failure", e.State().Run.State)
	}
}

// "One container per item, capped by max_parallel. Each shard receives a single-item
// envelope."
func TestFanOutItemIsCappedByMaxParallel(t *testing.T) {
	e := started(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
steps:
  invoice:
    image: `+image+`
    inputs:
      orders: ${{ workflow.inputs.orders }}
    strategy:
      fan_out: item
      max_parallel: 2
    params:
      currency: ${{ item.data.currency }}
    outputs: [ok]
`, Options{Inputs: map[string]any{"orders": []any{
		map[string]any{"currency": "EUR"},
		map[string]any{"currency": "GBP"},
		map[string]any{"currency": "CHF"},
	}}})

	plan := next(t, e, runAt)
	if len(plan.Start) != 2 {
		t.Fatalf("the plan starts %d shards, want the 2 max_parallel allows of the 3 items", len(plan.Start))
	}
	if got := plan.Start[0]; got.Shard != (agk.Shard{Index: 1, Of: 3}) || len(got.Inputs["orders"].Items) != 1 {
		t.Errorf("the first shard is %s carrying %d items, want 1/3 carrying one", got.Shard, len(got.Inputs["orders"].Items))
	}
	// The current item is the shard's own, which is what fan_out: item exposes.
	if got := plan.Start[1].Params["currency"]; got != "GBP" {
		t.Errorf("the second shard resolved currency to %v, want the item it was cut on", got)
	}

	// max_parallel counts what holds a runner, so the third shard waits until one of
	// the two the driver took has ended.
	for _, task := range plan.Start {
		record(t, e, Result{Task: task.ID, State: agk.TaskRunning}, runAt)
	}
	if got := next(t, e, runAt.Add(time.Minute)); len(got.Start) != 0 {
		t.Fatalf("the plan starts %d shards while 2 hold a runner and max_parallel is 2", len(got.Start))
	}
	record(t, e, succeeded(plan.Start[0], ports("ok", item("a1"))), runAt.Add(time.Minute))
	plan = next(t, e, runAt.Add(2*time.Minute))
	if len(plan.Start) != 1 || plan.Start[0].Shard.Index != 3 {
		t.Fatalf("the plan starts %d shards, want the third once a slot came free", len(plan.Start))
	}
}

// "Every combination is a shard, with its variables injected into params."
func TestAMatrixInjectsItsCombinationIntoParams(t *testing.T) {
	e := started(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: rates, namespace: finance }
steps:
  rate-table:
    image: `+image+`
    strategy:
      matrix:
        region: [fr, be]
    outputs: [ok]
`, Options{})

	plan := next(t, e, runAt)
	if len(plan.Start) != 2 {
		t.Fatalf("the plan starts %d shards, want one per combination", len(plan.Start))
	}
	for i, want := range []string{"fr", "be"} {
		if got := plan.Start[i].Params["region"]; got != want {
			t.Errorf("shard %d carries region %v, want %s injected into its params", i+1, got, want)
		}
	}
}

// "100 to 119: transient failure, retried according to the step policy", with the wait
// starting at base. The moment the attempt is due is what Wake names.
func TestATransientFailureIsTriedAgainAfterTheBackoff(t *testing.T) {
	e := started(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
steps:
  invoice:
    image: `+image+`
    retry:
      max: 2
      on: [transient]
      backoff: { type: exponential, base: 30s, max: 60s }
    outputs: [ok]
`, Options{})

	plan := next(t, e, runAt)
	failed := Result{Task: plan.Start[0].ID, State: agk.TaskFailed, ExitCode: 108, FinishedAt: runAt.Add(time.Minute)}
	record(t, e, failed, runAt.Add(time.Minute))

	plan = next(t, e, runAt.Add(time.Minute))
	if len(plan.Start) != 0 {
		t.Errorf("the plan starts the attempt at once, and the policy asked for a wait of 30s")
	}
	if want := runAt.Add(90 * time.Second); !plan.Wake.Equal(want) {
		t.Errorf("wake at %s, want %s: the wait starts at base and runs from the moment the attempt ended", plan.Wake, want)
	}
	if e.State().Run.State != agk.Running {
		t.Errorf("the run is %s, and a step with another attempt coming has not failed", e.State().Run.State)
	}

	plan = next(t, e, runAt.Add(2*time.Minute))
	if len(plan.Start) != 1 || plan.Start[0].Attempt != 2 {
		t.Fatalf("the plan starts %d tasks, want attempt 2 now that the wait has passed", len(plan.Start))
	}

	// 120 is never retried, whatever retry says.
	record(t, e, Result{Task: plan.Start[0].ID, State: agk.TaskFailed, ExitCode: 120, FinishedAt: runAt.Add(3 * time.Minute)}, runAt.Add(3*time.Minute))
	plan = next(t, e, runAt.Add(4*time.Minute))
	if len(plan.Start) != 0 {
		t.Errorf("the plan starts another attempt after exit 120, which is a permanent failure never retried")
	}
	if e.State().Run.State != agk.Failed {
		t.Errorf("the run is %s, want failed: a step failed without continue_on_error", e.State().Run.State)
	}
}

// "continue_on_error: a failure of this step does not fail the run. Downstream steps see
// the failed state through when."
func TestContinueOnErrorKeepsTheFailureOutOfTheRunVerdict(t *testing.T) {
	doc := `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
steps:
  invoice:
    image: ` + image + `
    continue_on_error: true
    outputs: [ok]
  cleanup:
    image: ` + image + `
    needs:
      - { step: invoice, port: ok, as: orders }
    when: [always]
    outputs: [ok]
`
	e := started(t, doc, Options{})
	plan := next(t, e, runAt)
	record(t, e, Result{Task: plan.Start[0].ID, State: agk.TaskFailed, ExitCode: 3, FinishedAt: runAt}, runAt)

	plan = next(t, e, runAt.Add(time.Minute))
	if got := e.State().Steps["invoice"].Verdict; got != agk.VerdictFailed {
		t.Errorf("invoice is %s, want failed: the keyword decides the run verdict and not the step's", got)
	}
	if len(plan.Start) != 1 || plan.Start[0].Step != "cleanup" {
		t.Fatalf("the plan starts %s, want cleanup: when: [always] reaches a failed upstream", starts(plan))
	}
	if got := plan.Start[0].Inputs["orders"]; got.Meta.Count != 0 {
		t.Errorf("cleanup is given %#v, want the batch of nothing a failed step leaves on its port", got.Meta)
	}
	record(t, e, succeeded(plan.Start[0], ports("ok")), runAt.Add(2*time.Minute))
	next(t, e, runAt.Add(3*time.Minute))
	if e.State().Run.State != agk.Succeeded {
		t.Errorf("the run is %s, want succeeded: the only failure was tolerated", e.State().Run.State)
	}
}

// "first lifts the barrier as soon as one upstream port has produced. The other edges are
// abandoned and their steps cancelled if no other consumer needs them."
func TestMergeFirstCancelsTheStepItLeftBehind(t *testing.T) {
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
	if len(plan.Start) != 2 {
		t.Fatalf("the plan starts %d tasks, want quick and slow together", len(plan.Start))
	}
	slow := taskOf(t, plan, "slow")
	record(t, e, Result{Task: slow.ID, State: agk.TaskRunning}, runAt)
	record(t, e, succeeded(taskOf(t, plan, "quick"), ports("ok", item("a1"))), runAt.Add(time.Minute))

	plan = next(t, e, runAt.Add(2*time.Minute))
	if got := e.State().Steps["slow"].Verdict; got != agk.VerdictCancelled {
		t.Errorf("slow is %s, want cancelled: its edge was abandoned and no other consumer needs it", got)
	}
	if len(plan.Stop) != 1 || plan.Stop[0].Task != slow.ID || plan.Stop[0].Reason != StopSuperseded {
		t.Fatalf("the plan stops %#v, want the task of slow, superseded", plan.Stop)
	}
	if len(plan.Start) != 1 || plan.Start[0].Step != "whichever" {
		t.Fatalf("the plan starts %s, want whichever: the barrier lifted on the edge that produced", starts(plan))
	}
	if got := plan.Start[0].Inputs["orders"]; got.Meta.Step != "quick" {
		t.Errorf("whichever is given the port of %s, want the edge that lifted the barrier", got.Meta.Step)
	}
}

// "fail_fast: the first shard to fail stops the shards still running."
func TestFailFastStopsTheSiblingsOfAFailedShard(t *testing.T) {
	e := started(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
steps:
  invoice:
    image: `+image+`
    inputs:
      orders: ${{ workflow.inputs.orders }}
    strategy:
      fan_out: item
      fail_fast: true
    outputs: [ok]
`, Options{Inputs: map[string]any{"orders": []any{
		map[string]any{"customer_id": "c1"},
		map[string]any{"customer_id": "c2"},
	}}})

	plan := next(t, e, runAt)
	record(t, e, Result{Task: plan.Start[1].ID, State: agk.TaskRunning}, runAt)
	record(t, e, Result{Task: plan.Start[0].ID, State: agk.TaskFailed, ExitCode: 7, FinishedAt: runAt}, runAt)

	plan = next(t, e, runAt.Add(time.Minute))
	if len(plan.Stop) != 1 || plan.Stop[0].Task != agk.TaskID("01HZXRUN/invoice/1/2/2") || plan.Stop[0].Reason != StopSiblingFailed {
		t.Fatalf("the plan stops %#v, want the shard still running beside the one that failed", plan.Stop)
	}
}

// "The deadline set by the root timeout of the entry point expired, and the tasks still
// running were stopped."
func TestTheRootTimeoutEndsTheRunAndStopsWhatIsRunning(t *testing.T) {
	e := started(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
timeout: 4h
steps:
  invoice:
    image: `+image+`
    outputs: [ok]
`, Options{})

	plan := next(t, e, runAt)
	if want := runAt.Add(4 * time.Hour); !plan.Wake.Equal(want) {
		t.Errorf("wake at %s, want the deadline at %s", plan.Wake, want)
	}
	record(t, e, Result{Task: plan.Start[0].ID, State: agk.TaskRunning}, runAt)

	plan = next(t, e, runAt.Add(4*time.Hour))
	if e.State().Run.State != agk.TimedOut {
		t.Fatalf("the run is %s, want timed_out: the deadline of the entry point passed", e.State().Run.State)
	}
	if len(plan.Stop) != 1 || plan.Stop[0].Reason != StopDeadline {
		t.Fatalf("the plan stops %#v, want the task still running, at the deadline", plan.Stop)
	}
	// What happened to the run is not rewritten by what happens after it.
	record(t, e, Result{Task: plan.Stop[0].Task, State: agk.TaskCancelled}, runAt.Add(4*time.Hour))
	if got := next(t, e, runAt.Add(5*time.Hour)); len(got.Stop) != 0 || len(got.Start) != 0 {
		t.Errorf("the plan after the run ended is %#v, want nothing left to do", got)
	}
	if e.State().Run.State != agk.TimedOut {
		t.Errorf("the run is %s, and a run does not move out of a terminal state", e.State().Run.State)
	}
}

// "Failover is a state resume and never a rebuild": load the State, call Next, get the
// Plan the instance that died would have got.
func TestARunIsPickedUpWhereItWasLeft(t *testing.T) {
	e := started(t, twoSteps, Options{})
	plan := next(t, e, runAt)
	record(t, e, succeeded(plan.Start[0], ports("ok", item("a1"))), runAt.Add(time.Minute))

	doc, err := json.Marshal(e.State())
	if err != nil {
		t.Fatal(err)
	}
	var resumed State
	if err := json.Unmarshal(doc, &resumed); err != nil {
		t.Fatal(err)
	}
	second, err := New(e.Graph(), &resumed, agk.DefaultLimits(), DefaultMaxRequeues)
	if err != nil {
		t.Fatalf("resuming the run: %v", err)
	}

	want := next(t, e, runAt.Add(2*time.Minute))
	got := next(t, second, runAt.Add(2*time.Minute))
	if !reflect.DeepEqual(want.Start, got.Start) {
		t.Errorf("the resumed run decided\n%#v\nand the run that never stopped decided\n%#v", got.Start, want.Start)
	}
}

// A secret reaches a brick as a parameter by one route and travels as a reference: "the
// task message names the secrets it needs and carries none of them".
func TestASecretInParamsTravelsAsAReferenceAndNeverAsAValue(t *testing.T) {
	doc := `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
secrets: [billing]
steps:
  invoice:
    image: ` + image + `
    secrets: [billing]
    params:
      token: ${{ secrets.billing }}
    outputs: [ok]
`
	manifest := `
apiVersion: agentiik.dev/v1
kind: Brick
metadata: { name: invoice, version: 1.0.0 }
spec:
  outputs:
    ok: {}
  params:
    token: { type: string, sensitive: true }
  secrets:
    - { name: billing, mount: /agk/secrets/billing }
  runtime: { user: "65532:65532" }
`
	e := evaluator(t, doc, manifest, Options{})
	plan := next(t, e, runAt)
	task := plan.Start[0]
	if got := task.Params["token"]; got != (SecretParam{Secret: "billing"}) {
		t.Errorf("the parameter token is %#v, want the reference the runner redeems", got)
	}
	if len(task.Secrets) != 1 || task.Secrets[0].Mount != "/agk/secrets/billing" {
		t.Errorf("the task mounts %#v, want the name and the path the manifest asks for", task.Secrets)
	}
	written, err := json.Marshal(task.Params)
	if err != nil {
		t.Fatalf("the task message carries params that cannot be written down: %v", err)
	}
	if string(written) != `{"token":{"secret":"billing"}}` {
		t.Errorf("the task message carries %s, and it must contain nothing sensitive", written)
	}

	// A manifest that has not marked the parameter sensitive refuses it, because that
	// is the only route by which a secret reaches a brick as a parameter. The step
	// fails on invalid input, which is a run the state can describe rather than an
	// evaluation that stopped.
	e = evaluator(t, doc, replace(manifest, "sensitive: true", "sensitive: false"), Options{})
	if got := next(t, e, runAt); len(got.Start) != 0 {
		t.Fatalf("the plan starts %s, and the parameter is a secret the manifest does not accept", starts(got))
	}
	if got := e.State().Steps["invoice"].Shards[0].ExitCode; got != 120 {
		t.Errorf("the shard failed with %d, want 120: a task that could not be built is invalid input", got)
	}
	// The run reaches its verdict in the same evaluation, because nothing in the plan
	// would ever bring a caller back to ask again.
	if e.State().Run.State != agk.Failed {
		t.Errorf("the run is %s, want failed", e.State().Run.State)
	}
}

// "zip pairs items by rank. Envelopes of differing lengths produce a validation failure."
// A rule broken while a run is under way fails the step that broke it, so that the run
// reaches a terminal state rather than stopping where it stands.
func TestZipOnDifferingLengthsFailsTheStep(t *testing.T) {
	e := started(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
steps:
  left:
    image: `+image+`
    outputs: [ok]
  right:
    image: `+image+`
    outputs: [ok]
  pair:
    image: `+image+`
    needs:
      - { step: left,  port: ok, as: orders }
      - { step: right, port: ok, as: orders }
    merge: zip
    outputs: [ok]
`, Options{})

	plan := next(t, e, runAt)
	record(t, e, succeeded(taskOf(t, plan, "left"), ports("ok", item("c1"), item("c2"))), runAt)
	record(t, e, succeeded(taskOf(t, plan, "right"), ports("ok", item("c1"))), runAt)

	plan = next(t, e, runAt.Add(time.Minute))
	if len(plan.Start) != 0 {
		t.Fatalf("the plan starts %s, and the two envelopes cannot be paired", starts(plan))
	}
	pair := e.State().Steps["pair"]
	if pair.Verdict != agk.VerdictFailed {
		t.Fatalf("pair is %s, want failed: zip was given 2 items and 1", pair.Verdict)
	}
	if !strings.Contains(pair.Reason, "differing lengths") {
		t.Errorf("pair failed with %q, want the rule in the documentation's own words", pair.Reason)
	}
	if got, ok := pair.Ports["ok"]; !ok || got.Meta.Count != 0 {
		t.Errorf("pair published %#v, and a failed step still publishes so that the error path below it is reachable", got)
	}
	if e.State().Run.State != agk.Failed {
		t.Errorf("the run is %s, want failed", e.State().Run.State)
	}
}

// "With cache: true, the key combines the image digest, the resolved parameters and the
// digests of the input envelopes, prefixed by the namespace, and it never crosses a
// namespace boundary."
func TestTheCacheKeyIsTheImageTheParametersAndTheInputs(t *testing.T) {
	doc := `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
steps:
  normalize:
    image: ` + image + `
    cache: true
    params:
      currency: EUR
    outputs: [ok]
`
	first := next(t, started(t, doc, Options{}), runAt).Start[0]
	if !first.Cache || first.CacheKey == "" {
		t.Fatalf("the task carries cache %v and the key %q, want both", first.Cache, first.CacheKey)
	}
	if got := first.CacheKey[:len("finance/sha256/")]; got != "finance/sha256/" {
		t.Errorf("the key is %q, and it is prefixed by the namespace so that it never crosses one", first.CacheKey)
	}
	// The same file gives the same key, and a changed parameter gives another.
	again := next(t, started(t, doc, Options{}), runAt).Start[0]
	if again.CacheKey != first.CacheKey {
		t.Errorf("two runs of one file gave two keys, %q and %q", first.CacheKey, again.CacheKey)
	}
	other := next(t, started(t, replace(doc, "EUR", "GBP"), Options{}), runAt).Start[0]
	if other.CacheKey == first.CacheKey {
		t.Errorf("a changed parameter gave the key %q, which the first task already had", other.CacheKey)
	}
	// "Only an idempotent step can be cached."
	none := next(t, started(t, doc+"    idempotent: false\n", Options{}), runAt).Start[0]
	if none.Cache || none.CacheKey != "" {
		t.Errorf("a step declared idempotent: false carries cache %v and the key %q", none.Cache, none.CacheKey)
	}
}

// A run cancelled by a principal ends there, and the tasks in flight are named so that
// something can stop them.
func TestACancelledRunNamesWhatIsStillRunning(t *testing.T) {
	e := started(t, oneStep, Options{})
	plan := next(t, e, runAt)
	record(t, e, Result{Task: plan.Start[0].ID, State: agk.TaskRunning}, runAt)

	e.Cancel(runAt.Add(time.Minute))
	plan = next(t, e, runAt.Add(time.Minute))
	if len(plan.Stop) != 1 || plan.Stop[0].Reason != StopCancelled {
		t.Fatalf("the plan stops %#v, want the task in flight, cancelled", plan.Stop)
	}
	if e.State().Run.State != agk.Cancelled {
		t.Errorf("the run is %s, want cancelled", e.State().Run.State)
	}
}

// A result for an attempt that is over changes nothing: the task identifier is the
// idempotency key and a bus is allowed to deliver twice.
func TestADuplicateResultChangesNothing(t *testing.T) {
	e := started(t, oneStep, Options{})
	plan := next(t, e, runAt)
	record(t, e, succeeded(plan.Start[0], ports("ok", item("a1"))), runAt)

	unchanged(t, e, Result{Task: plan.Start[0].ID, State: agk.TaskFailed, ExitCode: 9, FinishedAt: runAt}, "a second result for a finished attempt")
}

// An attempt a retry replaced is over too, though nothing on its shard says so but the
// attempt number: the shard is pending again and a guard that read only its state would
// take the old attempt's result as news about the new one.
func TestAResultForAnAttemptAlreadyRetriedChangesNothing(t *testing.T) {
	e := started(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
steps:
  invoice:
    image: `+image+`
    retry: { max: 2, on: [failed] }
    outputs: [ok]
`, Options{})
	plan := next(t, e, runAt)
	failed := Result{Task: plan.Start[0].ID, State: agk.TaskFailed, ExitCode: 1, FinishedAt: runAt}
	record(t, e, failed, runAt)
	if sh := e.State().Steps["invoice"].Shards[0]; sh.Attempt != 2 || sh.Task != agk.TaskPending {
		t.Fatalf("the shard is attempt %d, %s, and a failure the policy retries leaves attempt 2 pending", sh.Attempt, sh.Task)
	}

	unchanged(t, e, failed, "a failure delivered again for an attempt already retried")
	unchanged(t, e, succeeded(plan.Start[0], ports("ok", item("a1"))), "a success for an attempt already retried")
}

const requeueing = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
steps:
  invoice:
    image: ` + image + `
    retry:
      max: 1
      on: [lost, transient]
      backoff: { type: exponential, base: 30s, max: 60s }
    outputs: [ok]
`

// "A requeue after loss keeps the idempotency key and takes a new task_id." So the task
// the plan hands out again is the task that was lost, attempt and identifier alike, and
// it is handed out at once: the backoff spaces attempts, and this is not one.
func TestALostTaskIsRequeuedUnderTheSameKey(t *testing.T) {
	e := started(t, requeueing, Options{})
	first := next(t, e, runAt).Start[0]
	record(t, e, Result{Task: first.ID, State: agk.TaskDispatched, DispatchedAt: runAt}, runAt)
	record(t, e, Result{Task: first.ID, State: agk.TaskLost}, runAt.Add(time.Minute))

	sh := e.State().Steps["invoice"].Shards[0]
	if sh.Attempt != 1 || sh.Requeue != 1 || sh.Task != agk.TaskPending {
		t.Fatalf("the shard is attempt %d, requeue %d, %s, and a lost task of an idempotent step is requeued on the attempt it was lost on", sh.Attempt, sh.Requeue, sh.Task)
	}
	plan := next(t, e, runAt.Add(time.Minute))
	if len(plan.Start) != 1 {
		t.Fatalf("the plan starts %s after the loss, and the requeue is due at once", starts(plan))
	}
	if again := plan.Start[0]; again.ID != first.ID || again.Attempt != 1 {
		t.Errorf("the requeue is %s, attempt %d, and it keeps the key %s it was lost under", again.ID, again.Attempt, first.ID)
	}

	// The loss of the first dispatch, heard again, is about a dispatch the shard has left
	// behind, while the same key now names the second.
	unchanged(t, e, Result{Task: first.ID, State: agk.TaskLost}, "a loss of the first dispatch delivered again")

	// Nor is any other ending of the first dispatch, from a runner that came back after it
	// was declared lost: the attempt waits on the requeue, which is still going. The
	// requeue's own ending is the one that ends it.
	unchanged(t, e, succeeded(first, ports("ok", item("a1"))), "a success of the first dispatch after the requeue went out")
	unchanged(t, e, Result{Task: first.ID, State: agk.TaskFailed, ExitCode: 1}, "a failure of the first dispatch after the requeue went out")
	ended := succeeded(first, ports("ok", item("a1")))
	ended.Requeue = 1
	record(t, e, ended, runAt.Add(2*time.Minute))
	next(t, e, runAt.Add(2*time.Minute))
	if got := e.State().Run.State; got != agk.Succeeded {
		t.Errorf("the run is %s after the requeue came back with a success", got)
	}
}

// "A loss does not use up a retry.max attempt." max: 1 is two attempts, and a first
// attempt lost twice still has its second attempt to fail on.
func TestALossDoesNotUseUpAnAttempt(t *testing.T) {
	e := started(t, requeueing, Options{})
	task := next(t, e, runAt).Start[0]
	for requeue := range 2 {
		record(t, e, Result{Task: task.ID, State: agk.TaskDispatched, DispatchedAt: runAt}, runAt)
		record(t, e, Result{Task: task.ID, State: agk.TaskLost, Requeue: requeue}, runAt)
		plan := next(t, e, runAt)
		if len(plan.Start) != 1 || plan.Start[0].ID != task.ID {
			t.Fatalf("after loss %d the plan starts %s, want %s again", requeue+1, starts(plan), task.ID)
		}
	}

	record(t, e, Result{Task: task.ID, State: agk.TaskFailed, ExitCode: 108, Requeue: 2, FinishedAt: runAt}, runAt)
	plan := next(t, e, runAt.Add(time.Minute))
	if len(plan.Start) != 1 || plan.Start[0].Attempt != 2 {
		t.Fatalf("a transient failure after two losses starts %s, and max: 1 still owes attempt 2", starts(plan))
	}
	if sh := e.State().Steps["invoice"].Shards[0]; sh.Requeue != 0 {
		t.Errorf("attempt 2 carries requeue %d, and a further attempt is a new key handed out for the first time", sh.Requeue)
	}

	record(t, e, Result{Task: plan.Start[0].ID, State: agk.TaskFailed, ExitCode: 108, FinishedAt: runAt.Add(time.Minute)}, runAt.Add(time.Minute))
	next(t, e, runAt.Add(time.Hour))
	if got := e.State().Run.State; got != agk.Failed {
		t.Errorf("the run is %s after both attempts max: 1 allows failed", got)
	}
}

// "idempotent: false ... never requeued after loss." The loss ends the shard, whatever
// the policy names, because the work may well have been done.
func TestALostTaskOfAStepThatIsNotIdempotentIsNotRequeued(t *testing.T) {
	e := started(t, replace(requeueing, "    outputs: [ok]", "    idempotent: false\n    outputs: [ok]"), Options{})
	task := next(t, e, runAt).Start[0]
	record(t, e, Result{Task: task.ID, State: agk.TaskDispatched, DispatchedAt: runAt}, runAt)
	record(t, e, Result{Task: task.ID, State: agk.TaskLost}, runAt)

	plan := next(t, e, runAt)
	if len(plan.Start) != 0 {
		t.Errorf("the plan starts %s after a step declared idempotent: false lost its task", starts(plan))
	}
	if got := e.State().Run.State; got != agk.Failed {
		t.Errorf("the run is %s, and its one step lost a task it may not run again", got)
	}
}

// A failure no container explains carries its reason, the controller's refusal to publish a
// task to a pool that will not run it for one, and the reason becomes the step's where the
// failure stands. One that is retried says nothing of the step, which has an attempt to come.
func TestAFailureThatStandsGivesItsStepItsReason(t *testing.T) {
	e := started(t, requeueing, Options{})
	first := next(t, e, runAt).Start[0]
	record(t, e, Result{Task: first.ID, State: agk.TaskFailed, ExitCode: 100, FinishedAt: runAt, Reason: "the registry did not answer"}, runAt)
	if st := e.State().Steps["invoice"]; st.Reason != "" {
		t.Errorf("a failure that is retried gave its step the reason %q", st.Reason)
	}

	later := runAt.Add(time.Hour)
	second := next(t, e, later).Start[0]
	const why = "step invoice runs on the runner pool ops, which does not accept the namespace finance"
	record(t, e, Result{Task: second.ID, State: agk.TaskFailed, ExitCode: 125, FinishedAt: later, Reason: why}, later)
	next(t, e, later)
	if st := e.State().Steps["invoice"]; st.Verdict != agk.VerdictFailed || st.Reason != why {
		t.Errorf("the step is %s because %q", st.Verdict, st.Reason)
	}
}

// lose dispatches a task and loses it on the dispatch named, as the controller records a
// loss the heartbeat declared.
func lose(t *testing.T, e *Evaluator, task Task, requeue int, at time.Time) {
	t.Helper()
	record(t, e, Result{Task: task.ID, State: agk.TaskDispatched, DispatchedAt: at}, at)
	record(t, e, Result{Task: task.ID, State: agk.TaskLost, Requeue: requeue, FinishedAt: at}, at)
}

// Past max_requeues a lost task is not requeued: it stays lost, and its step fails,
// charged to the infrastructure and not to the brick. So under the default of three a key
// goes out four times, and the fourth loss ends the shard lost rather than failed: the
// attempt max: 1 has left is not spent on it, since the brick never failed, and the step
// says why it failed.
func TestAKeyLostPastMaxRequeuesFailsItsStep(t *testing.T) {
	e := started(t, requeueing, Options{})
	task := next(t, e, runAt).Start[0]
	for requeue := range DefaultMaxRequeues {
		lose(t, e, task, requeue, runAt)
		plan := next(t, e, runAt)
		if len(plan.Start) != 1 || plan.Start[0].ID != task.ID || plan.Start[0].Attempt != 1 {
			t.Fatalf("after loss %d the plan starts %s, want %s again", requeue+1, starts(plan), task.ID)
		}
	}

	lose(t, e, task, DefaultMaxRequeues, runAt)
	plan := next(t, e, runAt.Add(time.Hour))
	if len(plan.Start) != 0 {
		t.Errorf("a key lost past max_requeues went out again as %s", starts(plan))
	}
	st := e.State().Steps["invoice"]
	if sh := st.Shards[0]; sh.Task != agk.TaskLost || sh.Attempt != 1 || sh.Requeue != DefaultMaxRequeues {
		t.Errorf("the shard is attempt %d, requeue %d, %s, and a loss past the bound stands where it happened", sh.Attempt, sh.Requeue, sh.Task)
	}
	if st.Verdict != agk.VerdictFailed || !strings.Contains(st.Reason, "max_requeues") {
		t.Errorf("the step is %s because %q, and a loss past max_requeues fails it and says so", st.Verdict, st.Reason)
	}
	if got := e.State().Run.State; got != agk.Failed {
		t.Errorf("the run is %s, and its one step failed on a loss", got)
	}

	// The loss the controller keeps hearing until the run ends is no news.
	unchanged(t, e, Result{Task: task.ID, State: agk.TaskLost, Requeue: DefaultMaxRequeues}, "the last loss delivered again")
}

// The bound is the installation's, handed to the evaluator rather than read from anywhere:
// one requeue under max_requeues: 1, and none under max_requeues: 0, which says none and
// not the default the setting takes where it is not written. A negative bound counts no
// number of times and is refused. A resumed run decides under the bound it is resumed
// with, since a bound is not the run's.
func TestMaxRequeuesIsWhatTheInstallationPasses(t *testing.T) {
	for _, c := range []struct {
		name     string
		most     int
		requeues int
	}{
		{"one", 1, 1},
		{"none", 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := started(t, requeueing, Options{MaxRequeues: new(c.most)})
			task := next(t, e, runAt).Start[0]
			for requeue := range c.requeues + 1 {
				lose(t, e, task, requeue, runAt)
				next(t, e, runAt)
			}
			if sh := e.State().Steps["invoice"].Shards[0]; sh.Task != agk.TaskLost || sh.Requeue != c.requeues {
				t.Errorf("the shard is requeue %d, %s, want lost after %d requeues", sh.Requeue, sh.Task, c.requeues)
			}
			if got := e.State().Run.State; got != agk.Failed {
				t.Errorf("the run is %s once the bound refused a requeue", got)
			}
		})
	}

	g := built(t, requeueing, evaluatorManifest)
	if _, err := Start(g, agk.Run{ID: "01HZXRUN", Namespace: "finance"}, Options{MaxRequeues: new(-1)}, runAt); err == nil || !strings.Contains(err.Error(), "max_requeues") {
		t.Errorf("a run started under max_requeues: -1, which is no number of times, answering %v", err)
	}

	e := started(t, requeueing, Options{})
	task := next(t, e, runAt).Start[0]
	lose(t, e, task, 0, runAt)
	next(t, e, runAt)
	if _, err := New(e.Graph(), e.State(), agk.DefaultLimits(), -1); err == nil || !strings.Contains(err.Error(), "max_requeues") {
		t.Errorf("a run resumed under max_requeues: -1, which is no number of times, answering %v", err)
	}
	resumed, err := New(e.Graph(), e.State(), agk.DefaultLimits(), 1)
	if err != nil {
		t.Fatal(err)
	}
	lose(t, resumed, task, 1, runAt)
	if plan := next(t, resumed, runAt); len(plan.Start) != 0 || resumed.State().Run.State != agk.Failed {
		t.Errorf("resumed under max_requeues: 1, a second loss started %s and left the run %s", starts(plan), resumed.State().Run.State)
	}
}

// A step a merge: first cancelled keeps the reason that cancelled it. Its task handed out
// before its dispatch was recorded ended cancelled as the barrier lifted, still pending in the
// state, and a loss heard of it afterwards adds nothing. A document decided before the
// evaluator ended such shards still holds it pending, and a loss past max_requeues reported of
// it there fails nothing either: the verdict is already cancelled, and a reason saying the step
// fails would contradict it.
func TestACancelledStepKeepsItsReasonThroughALossPastMaxRequeues(t *testing.T) {
	for _, pending := range []bool{false, true} {
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
    retry: { on: [lost] }
    outputs: [ok]
  whichever:
    image: `+image+`
    needs:
      - { step: quick, port: ok, as: orders }
      - { step: slow,  port: ok, as: orders }
    merge: first
    outputs: [ok]
`, Options{MaxRequeues: new(0)})

		plan := next(t, e, runAt)
		slow := taskOf(t, plan, "slow")
		record(t, e, succeeded(taskOf(t, plan, "quick"), ports("ok", item("a1"))), runAt.Add(time.Minute))
		next(t, e, runAt.Add(2*time.Minute))
		cancelled := e.State().Steps["slow"]
		if cancelled.Verdict != agk.VerdictCancelled {
			t.Fatalf("slow is %s, want cancelled: its edge was abandoned and no other consumer needs it", cancelled.Verdict)
		}
		want := agk.TaskCancelled
		if pending {
			cancelled.Shards[0] = ShardState{Shard: cancelled.Shards[0].Shard, Attempt: 1}
			want = agk.TaskLost
		}

		record(t, e, Result{Task: slow.ID, State: agk.TaskLost, FinishedAt: runAt.Add(3 * time.Minute)}, runAt.Add(3*time.Minute))
		next(t, e, runAt.Add(3*time.Minute))
		st := e.State().Steps["slow"]
		if st.Verdict != agk.VerdictCancelled || st.Reason != cancelled.Reason {
			t.Errorf("pending %t: slow is %s because %q after its task was lost, and it was cancelled because %q", pending, st.Verdict, st.Reason, cancelled.Reason)
		}
		if sh := st.Shards[0]; sh.Task != want {
			t.Errorf("pending %t: the shard of slow is %s, want %s", pending, sh.Task, want)
		}
	}
}

// The count is per key. A further attempt is a new key, granted by max for a failure the
// brick reported, so it is handed out again after a loss as often as the first was.
func TestAFurtherAttemptIsRequeuedAsOftenAsTheFirst(t *testing.T) {
	e := started(t, requeueing, Options{MaxRequeues: new(1)})
	first := next(t, e, runAt).Start[0]
	lose(t, e, first, 0, runAt)
	next(t, e, runAt)
	record(t, e, Result{Task: first.ID, State: agk.TaskFailed, ExitCode: 108, Requeue: 1, FinishedAt: runAt}, runAt)

	second := next(t, e, runAt.Add(time.Minute)).Start
	if len(second) != 1 || second[0].Attempt != 2 {
		t.Fatalf("a transient failure after a requeue starts %s, and max: 1 owes attempt 2", starts(Plan{Start: second}))
	}
	lose(t, e, second[0], 0, runAt.Add(time.Minute))
	again := next(t, e, runAt.Add(time.Minute)).Start
	if len(again) != 1 || again[0].ID != second[0].ID {
		t.Errorf("attempt 2 lost for the first time starts %s, and its key has not been requeued yet", starts(Plan{Start: again}))
	}
}

// unchanged records a result that should be a duplicate and holds that it was: the shard
// is as it was, and so is the sequence, which is what the controller reads to decide
// whether there is anything to write and anything to decide again.
func unchanged(t *testing.T, e *Evaluator, r Result, what string) {
	t.Helper()
	_, name, _, _, err := agk.ParseTaskID(string(r.Task))
	if err != nil {
		t.Fatal(err)
	}
	before, seq := e.State().Steps[name].Shards[0], e.State().Seq
	record(t, e, r, runAt.Add(time.Minute))
	if after := e.State().Steps[name].Shards[0]; !reflect.DeepEqual(before, after) {
		t.Errorf("%s changed the shard:\n%#v\n%#v", what, before, after)
	}
	if after := e.State().Seq; after != seq {
		t.Errorf("%s took the sequence from %d to %d, which the controller writes down as a decision and decides again after", what, seq, after)
	}
}

// The one manifest these runs are held to: two output ports, one parameter, and no
// required input, so that a workflow can feed what it likes.
const evaluatorManifest = `
apiVersion: agentiik.dev/v1
kind: Brick
metadata: { name: invoice, version: 1.0.0 }
spec:
  inputs:
    orders: {}
  outputs:
    ok: {}
    rejected: {}
  params:
    currency: { type: string }
    region: { type: string }
    token: { type: string, sensitive: true }
  runtime: { user: "65532:65532" }
`

const image = "ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc"

const oneStep = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
steps:
  invoice:
    image: ` + image + `
    outputs: [ok]
`

const twoSteps = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
steps:
  normalize:
    image: ` + image + `
    outputs: [ok]
  archive:
    image: ` + image + `
    needs:
      - { step: normalize, port: ok, as: orders }
    outputs: [ok]
`

// runAt is the moment every run here starts, fixed so that a plan read twice says the
// same thing and a wake is a value a test can name.
var runAt = time.Date(2026, 3, 1, 6, 0, 0, 0, time.UTC)

// started builds a graph, starts a run of it and hands back the evaluator.
func started(t *testing.T, doc string, o Options) *Evaluator {
	t.Helper()
	return evaluator(t, doc, evaluatorManifest, o)
}

func evaluator(t *testing.T, doc, manifest string, o Options) *Evaluator {
	t.Helper()
	e, err := Start(built(t, doc, manifest), agk.Run{
		ID:        "01HZXRUN",
		Workflow:  "finance/monthly-invoicing@a3f9c1e",
		Namespace: "finance",
		Commit:    "a3f9c1e",
		StartedAt: runAt,
	}, o, runAt)
	if err != nil {
		t.Fatalf("starting the run: %v", err)
	}
	return e
}

func next(t *testing.T, e *Evaluator, at time.Time) Plan {
	t.Helper()
	plan, err := e.Next(at)
	if err != nil {
		t.Fatalf("evaluating at %s: %v", at, err)
	}
	return plan
}

func record(t *testing.T, e *Evaluator, r Result, at time.Time) {
	t.Helper()
	if err := e.Record(r, at); err != nil {
		t.Fatalf("recording %s: %v", r.Task, err)
	}
}

// succeeded is what a driver reports for a container that exited 0.
func succeeded(task Task, out map[agk.Port]agk.Envelope) Result {
	for port, envelope := range out {
		envelope.Meta.RunID = task.Run
		envelope.Meta.Step = task.Step
		envelope.Meta.Port = port
		envelope.Meta.Attempt = task.Attempt
		out[port] = envelope
	}
	return Result{Task: task.ID, State: agk.TaskSucceeded, Outputs: out, FinishedAt: runAt}
}

// ports is one envelope on one port, as brick.Collect would hand it back.
func ports(port agk.Port, items ...agk.Item) map[agk.Port]agk.Envelope {
	if items == nil {
		items = []agk.Item{}
	}
	return map[agk.Port]agk.Envelope{port: {
		Meta:  agk.Meta{Attempt: 1, Count: len(items), ProducedAt: runAt},
		Items: items,
	}}
}

func item(id string) agk.Item {
	return agk.Item{ID: id, Data: map[string]any{"customer_id": id}, Files: []agk.File{}}
}

// taskOf finds the task of one step in a plan.
func taskOf(t *testing.T, plan Plan, step agk.Step) Task {
	t.Helper()
	for _, task := range plan.Start {
		if task.Step == step {
			return task
		}
	}
	t.Fatalf("the plan starts nothing for %s", step)
	return Task{}
}

// starts names the steps a plan starts, for a failure that has to say what it got
// instead of what it wanted.
func starts(plan Plan) string {
	names := make([]string, 0, len(plan.Start))
	for _, task := range plan.Start {
		names = append(names, string(task.Step))
	}
	if len(names) == 0 {
		return "nothing"
	}
	return strings.Join(names, ", ")
}

// replace is one substitution in a document, so that two tests can differ by one line
// without two copies of the file between them.
func replace(doc, from, to string) string {
	return strings.Replace(doc, from, to, 1)
}

// "join: unmatched items leave on the unmatched port if the step declares one." The
// engine writes that port and the container never touches it, which is a publication only
// a whole run shows.
func TestAKeyJoinPublishesWhatItCouldNotMatch(t *testing.T) {
	e := started(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
steps:
  left:
    image: `+image+`
    outputs: [ok]
  right:
    image: `+image+`
    outputs: [ok]
  match:
    image: `+image+`
    needs:
      - { step: left,  port: ok, as: orders }
      - { step: right, port: ok, as: orders }
    merge: { join: { on: "$.data.customer_id" } }
    outputs: [ok, unmatched]
`, Options{})

	plan := next(t, e, runAt)
	record(t, e, succeeded(taskOf(t, plan, "left"), ports("ok", item("c1"), item("c2"))), runAt)
	record(t, e, succeeded(taskOf(t, plan, "right"), ports("ok", item("c1"), item("c3"))), runAt)

	plan = next(t, e, runAt.Add(time.Minute))
	task := taskOf(t, plan, "match")
	if got := task.Inputs["orders"]; got.Meta.Count != 1 || got.Items[0].ID != "c1" {
		t.Errorf("match is given %#v, want the one item both sides carry", got.Items)
	}
	// The port the engine writes is not one the container is asked for.
	if slices.Contains(task.Outputs, unmatchedPort) {
		t.Errorf("the task is asked to collect %v, and unmatched is written by the engine", task.Outputs)
	}

	record(t, e, succeeded(task, ports("ok", item("c1"))), runAt.Add(2*time.Minute))
	next(t, e, runAt.Add(3*time.Minute))
	got, ok := e.State().Steps["match"].Ports[unmatchedPort]
	if !ok {
		t.Fatalf("match published %v, and it declares an unmatched port", e.State().Steps["match"].Ports)
	}
	if got.Meta.Count != 2 {
		t.Errorf("unmatched carries %d items, want the one each side could not match", got.Meta.Count)
	}
}
