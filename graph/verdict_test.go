package graph

import (
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
)

var outcomeAt = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// outcomeBatch builds one shard envelope carrying the items named, so that a concatenation can
// be read by the identifiers that came out of it.
func outcomeBatch(run agk.RunID, step agk.Step, port agk.Port, attempt int, ids ...string) agk.Envelope {
	e := agk.Empty(run, step, port, attempt, outcomeAt)
	for _, id := range ids {
		e.Items = append(e.Items, agk.Item{ID: id, Data: map[string]any{"id": id}, Files: []agk.File{}})
	}
	e.Meta.Count = len(e.Items)
	return e
}

// TestTheExitCodeTableDecidesTheShard holds where a verdict comes from. The driver
// reports a state and a code; the table reads the code, and a runner that maps its own
// exit codes cannot publish a failure as a success.
func TestTheExitCodeTableDecidesTheShard(t *testing.T) {
	for _, c := range []struct {
		task    agk.TaskState
		exit    int
		verdict agk.Verdict
	}{
		{agk.TaskPending, 0, agk.VerdictPending},
		{agk.TaskDispatched, 0, agk.VerdictRunning},
		{agk.TaskRunning, 0, agk.VerdictRunning},
		{agk.TaskPublishing, 0, agk.VerdictRunning},
		{agk.TaskSucceeded, 0, agk.VerdictSucceeded},
		{agk.TaskSucceeded, 3, agk.VerdictFailed},
		{agk.TaskSucceeded, 120, agk.VerdictFailed},
		{agk.TaskFailed, 7, agk.VerdictFailed},
		{agk.TaskFailed, 0, agk.VerdictFailed},
		{agk.TaskLost, 0, agk.VerdictFailed},
		{agk.TaskTimedOut, 0, agk.VerdictFailed},
		{agk.TaskCancelled, 0, agk.VerdictCancelled},
	} {
		if got := shardVerdict(c.task, c.exit); got != c.verdict {
			t.Errorf("a task %s with exit %d is %s and not %s", c.task, c.exit, got, c.verdict)
		}
	}
}

// TestAFailureNobodyCanNameIsNobodyToRetry holds the reading failureOf records: a task
// reported failed whose code the table calls success is still failed, and names no kind,
// so no policy can ask for it again.
func TestAFailureNobodyCanNameIsNobodyToRetry(t *testing.T) {
	if _, named := failureOf(agk.TaskFailed, 0); named {
		t.Error("a task reported failed with exit 0 names a failure kind a policy could retry")
	}
	if shardVerdict(agk.TaskFailed, 0) != agk.VerdictFailed {
		t.Error("a task reported failed with exit 0 is not a failed shard")
	}
	for _, c := range []struct {
		task agk.TaskState
		exit int
		kind agk.Failure
	}{
		{agk.TaskFailed, 7, agk.FailureFailed},
		{agk.TaskFailed, 100, agk.FailureTransient},
		{agk.TaskLost, 0, agk.FailureLost},
		{agk.TaskTimedOut, 0, agk.FailureTimeout},
	} {
		kind, named := failureOf(c.task, c.exit)
		if !named || kind != c.kind {
			t.Errorf("a task %s with exit %d names %s (%v) and not %s", c.task, c.exit, kind, named, c.kind)
		}
	}
	for _, task := range []agk.TaskState{agk.TaskPending, agk.TaskRunning, agk.TaskPublishing, agk.TaskCancelled} {
		if _, named := failureOf(task, 100); named {
			t.Errorf("a task %s names a failure, and it has not failed", task)
		}
	}
}

// TestTheShardsReduceToTheStep holds what a step is once its shards are in: a failure
// outranks a cancellation, because under fail_fast the failure is what caused the
// cancellation, and a shard with another attempt coming means the step is still running.
func TestTheShardsReduceToTheStep(t *testing.T) {
	done := func(v agk.Verdict) ShardState {
		switch v {
		case agk.VerdictFailed:
			return ShardState{Attempt: 1, Task: agk.TaskFailed, ExitCode: 7}
		case agk.VerdictCancelled:
			return ShardState{Attempt: 1, Task: agk.TaskCancelled}
		default:
			return ShardState{Attempt: 1, Task: agk.TaskSucceeded}
		}
	}
	for _, c := range []struct {
		name    string
		shards  []ShardState
		verdict agk.Verdict
	}{
		{"every shard succeeded", []ShardState{done(agk.VerdictSucceeded), done(agk.VerdictSucceeded)}, agk.VerdictSucceeded},
		{"one shard failed", []ShardState{done(agk.VerdictSucceeded), done(agk.VerdictFailed)}, agk.VerdictFailed},
		{"a failure beside a cancellation", []ShardState{done(agk.VerdictFailed), done(agk.VerdictCancelled)}, agk.VerdictFailed},
		{"every shard cancelled", []ShardState{done(agk.VerdictCancelled)}, agk.VerdictCancelled},
		{"a shard still running", []ShardState{done(agk.VerdictSucceeded), {Attempt: 1, Task: agk.TaskRunning}}, agk.VerdictRunning},
		{"a fan-out over an empty batch", nil, agk.VerdictSucceeded},
	} {
		if got := stepVerdict(c.shards); got != c.verdict {
			t.Errorf("%s: the step is %s and not %s", c.name, got, c.verdict)
		}
	}
}

// TestAStepWaitingOnAnotherAttemptIsStillRunning holds the one thing that would let a
// downstream when: [failed] start too early: a shard whose last attempt failed but whose
// next one is on the clock has not finished failing.
func TestAStepWaitingOnAnotherAttemptIsStillRunning(t *testing.T) {
	waiting := ShardState{
		Attempt:       1,
		Task:          agk.TaskFailed,
		ExitCode:      100,
		FinishedAt:    outcomeAt,
		NextAttemptAt: outcomeAt.Add(2 * time.Second),
	}
	if got := stepVerdict([]ShardState{waiting}); got != agk.VerdictRunning {
		t.Errorf("a step with an attempt due in two seconds is %s", got)
	}
	// The same shard once the attempts are spent.
	waiting.NextAttemptAt = time.Time{}
	waiting.Attempt = 3
	if got := stepVerdict([]ShardState{waiting}); got != agk.VerdictFailed {
		t.Errorf("a step whose last attempt failed is %s", got)
	}
}

// TestContinueOnErrorIsReadOnceAndOnlyForTheRun holds the keyword. A failure without it
// fixes the run at failed; with it the run carries on, and the step is failed either way,
// because that is what a downstream when reads.
func TestContinueOnErrorIsReadOnceAndOnlyForTheRun(t *testing.T) {
	state := &State{
		Run:   agk.Run{ID: "01HZX", State: agk.Running},
		Steps: map[agk.Step]StepState{"fetch": {Verdict: agk.VerdictSucceeded}, "notify": {Verdict: agk.VerdictFailed}},
	}
	if got := runVerdict(state, func(agk.Step) bool { return false }); got != agk.Failed {
		t.Errorf("a step failed without continue_on_error leaves the run %s", got)
	}
	if got := runVerdict(state, func(s agk.Step) bool { return s == "notify" }); got != agk.Succeeded {
		t.Errorf("a step failed with continue_on_error leaves the run %s", got)
	}
	if got := state.Steps["notify"].Verdict; got != agk.VerdictFailed {
		t.Errorf("continue_on_error changed the step's own verdict to %s", got)
	}
}

// TestTheStepsReduceToTheRun holds the run states table, which is where a run verdict
// comes from: succeeded when every reached step finished and none failed beyond
// tolerance, failed when one did.
func TestTheStepsReduceToTheRun(t *testing.T) {
	for _, c := range []struct {
		name  string
		run   agk.RunState
		steps map[agk.Step]StepState
		state agk.RunState
	}{
		{"nothing has started", agk.Queued, map[agk.Step]StepState{"a": {}}, agk.Queued},
		{"a step is running", agk.Running, map[agk.Step]StepState{"a": {Verdict: agk.VerdictRunning}}, agk.Running},
		{"one is done and one is not", agk.Running, map[agk.Step]StepState{"a": {Verdict: agk.VerdictSucceeded}, "b": {}}, agk.Running},
		{"every step succeeded", agk.Running, map[agk.Step]StepState{"a": {Verdict: agk.VerdictSucceeded}}, agk.Succeeded},
		{"a skipped step is finished", agk.Running, map[agk.Step]StepState{"a": {Verdict: agk.VerdictSkipped}}, agk.Succeeded},
		{"a step failed", agk.Running, map[agk.Step]StepState{"a": {Verdict: agk.VerdictFailed}}, agk.Failed},
		{"a step was cancelled", agk.Running, map[agk.Step]StepState{"a": {Verdict: agk.VerdictSucceeded}, "b": {Verdict: agk.VerdictCancelled}}, agk.Succeeded},
		{"the run is waiting", agk.Waiting, map[agk.Step]StepState{"a": {Verdict: agk.VerdictSucceeded}, "b": {}}, agk.Waiting},
		{"the state holds no steps yet", agk.Queued, nil, agk.Queued},
		{"the state holds no steps yet on a started run", agk.Running, nil, agk.Running},
	} {
		state := &State{Run: agk.Run{ID: "01HZX", State: c.run}, Steps: c.steps}
		if got := runVerdict(state, func(agk.Step) bool { return false }); got != c.state {
			t.Errorf("%s: the run is %s and not %s", c.name, got, c.state)
		}
	}
}

// TestWhatHappenedToTheRunIsNotRewritten holds the precedence the documentation leaves
// open. Cancelled and timed_out happened to the run at a moment, and the steps stopped
// because of it; reducing the wreckage afterwards would report failed on a run that was
// cancelled.
func TestWhatHappenedToTheRunIsNotRewritten(t *testing.T) {
	for _, fixed := range []agk.RunState{agk.Cancelled, agk.TimedOut, agk.Succeeded, agk.Failed} {
		state := &State{
			Run:   agk.Run{ID: "01HZX", State: fixed},
			Steps: map[agk.Step]StepState{"a": {Verdict: agk.VerdictFailed}, "b": {Verdict: agk.VerdictCancelled}},
		}
		if got := runVerdict(state, func(agk.Step) bool { return false }); got != fixed {
			t.Errorf("a run already %s was reduced to %s", fixed, got)
		}
	}
}

// TestAPortPublishesExactlyOneEnvelope holds the scheduling rule the barrier downstream
// rests on: one envelope per declared port, once, when the step ends, and an empty one
// where the container wrote nothing.
func TestAPortPublishesExactlyOneEnvelope(t *testing.T) {
	const run, step = agk.RunID("01HZX"), agk.Step("normalize")
	outputs := []agk.Port{"ok", "rejected"}
	shards := []ShardState{{
		Shard:   agk.Shard{Index: 1, Of: 1},
		Attempt: 1,
		Task:    agk.TaskSucceeded,
		Ports: map[agk.Port]agk.Envelope{
			"ok": outcomeBatch(run, step, "ok", 1, "a", "b"),
			// A port outputs does not name is not one of the step's, and
			// what a step publishes is what it declared.
			"surprise": outcomeBatch(run, step, "surprise", 1, "c"),
		},
	}}

	ports, err := publish(step, &Step{Outputs: outputs}, shards, run, outcomeAt, agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != len(outputs) {
		t.Fatalf("the step published %d ports and declares %d", len(ports), len(outputs))
	}
	if _, ok := ports["surprise"]; ok {
		t.Error("a port the step does not declare in outputs was published")
	}
	if got := ports["ok"].Meta.Count; got != 2 {
		t.Errorf("the written port carries %d items", got)
	}
	empty, ok := ports["rejected"]
	if !ok {
		t.Fatal("a port declared but never written published nothing at all")
	}
	if empty.Meta.Count != 0 || len(empty.Items) != 0 {
		t.Errorf("a port never written published %d items", empty.Meta.Count)
	}
	if empty.Meta.RunID != run || empty.Meta.Step != step || empty.Meta.Port != "rejected" {
		t.Errorf("the empty envelope says it came from %s %s %s", empty.Meta.RunID, empty.Meta.Step, empty.Meta.Port)
	}
	for port, e := range ports {
		if err := e.Validate(agk.DefaultLimits()); err != nil {
			t.Errorf("the envelope published on %s is not one: %v", port, err)
		}
	}
}

// TestTheShardsAreConcatenatedPortByPort holds the other half of the same rule: a step
// split into shards publishes one envelope per port, in shard order, with the items the
// shards produced.
func TestTheShardsAreConcatenatedPortByPort(t *testing.T) {
	const run, step = agk.RunID("01HZX"), agk.Step("invoice")
	shards := []ShardState{
		{Shard: agk.Shard{Index: 2, Of: 3}, Attempt: 1, Task: agk.TaskSucceeded, Ports: map[agk.Port]agk.Envelope{
			"out": outcomeBatch(run, step, "out", 1, "b"),
		}},
		{Shard: agk.Shard{Index: 1, Of: 3}, Attempt: 1, Task: agk.TaskSucceeded, Ports: map[agk.Port]agk.Envelope{
			"out": outcomeBatch(run, step, "out", 1, "a"),
		}},
		{Shard: agk.Shard{Index: 3, Of: 3}, Attempt: 2, Task: agk.TaskSucceeded, Ports: map[agk.Port]agk.Envelope{
			"out":   outcomeBatch(run, step, "out", 2, "c"),
			"error": outcomeBatch(run, step, "error", 2, "e"),
		}},
	}

	ports, err := publish(step, &Step{Outputs: []agk.Port{"out", "error"}}, shards, run, outcomeAt, agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, item := range ports["out"].Items {
		ids = append(ids, item.ID)
	}
	if strings.Join(ids, ",") != "a,b,c" {
		t.Errorf("the port carries the items %v, and the shards produced a, b then c", ids)
	}
	if got := ports["out"].Meta.Attempt; got != 2 {
		t.Errorf("the publication is attributed to attempt %d, and a shard needed 2", got)
	}
	if got := ports["error"].Meta.Count; got != 1 {
		t.Errorf("the port one shard wrote carries %d items", got)
	}
}

// TestAFailedStepStillPublishes holds the reading publish records: the barrier is a
// barrier, so a step reached through when: [failed] needs its input ports satisfied, and
// a failed step that published nothing would make the error path unreachable.
func TestAFailedStepStillPublishes(t *testing.T) {
	const run, step = agk.RunID("01HZX"), agk.Step("invoice")
	shards := []ShardState{{Shard: agk.Shard{Index: 1, Of: 1}, Attempt: 3, Task: agk.TaskFailed, ExitCode: 7}}
	ports, err := publish(step, &Step{Outputs: []agk.Port{"out"}}, shards, run, outcomeAt, agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	e, ok := ports["out"]
	if !ok {
		t.Fatal("a failed step published nothing, and a downstream when: [failed] would never start")
	}
	if e.Meta.Count != 0 || e.Meta.Attempt != 3 {
		t.Errorf("the port carries %d items on attempt %d", e.Meta.Count, e.Meta.Attempt)
	}
}

// TestAWorkflowOutputIsOneStepPort holds what a run hands back: the envelope one
// declared output names, taken from the step port it names and from nowhere else.
func TestAWorkflowOutputIsOneStepPort(t *testing.T) {
	const run = agk.RunID("01HZX")
	state := &State{
		Run: agk.Run{ID: run, State: agk.Running},
		Steps: map[agk.Step]StepState{
			"archive": {Verdict: agk.VerdictSucceeded, Ports: map[agk.Port]agk.Envelope{
				"out": outcomeBatch(run, "archive", "out", 1, "a"),
			}},
			"invoice": {Verdict: agk.VerdictRunning},
		},
	}

	e, err := outputOf(state, "archive", "out")
	if err != nil {
		t.Fatal(err)
	}
	if e.Meta.Count != 1 {
		t.Errorf("the output carries %d items", e.Meta.Count)
	}
	for _, c := range []struct{ step, port, why string }{
		{"archive", "nowhere", "a port the step never published"},
		{"missing", "out", "a step the run does not hold"},
		{"invoice", "out", "a step that has not ended"},
	} {
		if _, err := outputOf(state, agk.Step(c.step), agk.Port(c.port)); err == nil {
			t.Errorf("%s was handed back as an output", c.why)
		}
	}
}

// TestARunIsReducedFromWhatTheDriverReported walks the whole reduction once, from what a
// driver said about each shard to the state of the run and the envelope the run hands
// back. Each rule below has its own test; this one holds that they compose into the run
// the documentation describes and not into four answers that never meet.
func TestARunIsReducedFromWhatTheDriverReported(t *testing.T) {
	const run = agk.RunID("01HZX")

	// normalize ran two shards and both came back with items.
	normalize := []ShardState{
		{Shard: agk.Shard{Index: 1, Of: 2}, Attempt: 1, Task: agk.TaskSucceeded, FinishedAt: outcomeAt, Ports: map[agk.Port]agk.Envelope{
			"ok": outcomeBatch(run, "normalize", "ok", 1, "a"),
		}},
		{Shard: agk.Shard{Index: 2, Of: 2}, Attempt: 1, Task: agk.TaskSucceeded, FinishedAt: outcomeAt, Ports: map[agk.Port]agk.Envelope{
			"ok": outcomeBatch(run, "normalize", "ok", 1, "b"),
		}},
	}
	// notify failed on an application code, with no policy asking for another
	// attempt, and the step carries continue_on_error.
	notify := []ShardState{
		{Attempt: 1, Task: agk.TaskFailed, ExitCode: 7, FinishedAt: outcomeAt},
	}
	if _, again := nextAttempt(Retry{}, notify[0]); again {
		t.Fatal("a step with no retry policy was given another attempt")
	}

	steps := map[agk.Step]*Step{
		"normalize": {Outputs: []agk.Port{"ok", "rejected"}},
		"notify":    {Outputs: []agk.Port{"out"}, ContinueOnError: true},
	}
	state := &State{Run: agk.Run{ID: run, State: agk.Running}, Steps: map[agk.Step]StepState{}}
	for name, shards := range map[agk.Step][]ShardState{"normalize": normalize, "notify": notify} {
		ports, err := publish(name, steps[name], shards, run, outcomeAt, agk.DefaultLimits())
		if err != nil {
			t.Fatalf("step %s: %v", name, err)
		}
		state.Steps[name] = StepState{Verdict: stepVerdict(shards), Shards: shards, Ports: ports, Since: outcomeAt}
	}

	if got := state.Steps["normalize"].Verdict; got != agk.VerdictSucceeded {
		t.Errorf("normalize is %s", got)
	}
	if got := state.Steps["notify"].Verdict; got != agk.VerdictFailed {
		t.Errorf("notify is %s, and continue_on_error does not change what a step is", got)
	}

	tolerates := func(name agk.Step) bool { return steps[name].ContinueOnError }
	if got := runVerdict(state, tolerates); got != agk.Succeeded {
		t.Errorf("the run is %s, and the one step that failed was allowed to", got)
	}

	// The run hands back the port a workflow output names, with the items both
	// shards produced, in shard order.
	e, err := outputOf(state, "normalize", "ok")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, item := range e.Items {
		ids = append(ids, item.ID)
	}
	if strings.Join(ids, ",") != "a,b" {
		t.Errorf("the output carries %v", ids)
	}
	if err := e.Validate(agk.DefaultLimits()); err != nil {
		t.Errorf("what the run hands back is not an envelope: %v", err)
	}
}

// A step that calls a sub-workflow declares no outputs of its own: "its ports are the
// declared outputs of the workflow it calls, which this commit does not carry". Reading
// that absence as a step with nothing to publish would drop every envelope the sub-run
// produced, and every edge below the call would resolve to a batch of nothing.
func TestASubWorkflowCallPublishesWhatCameBack(t *testing.T) {
	const run, step = agk.RunID("01HZX"), agk.Step("remind")
	shards := []ShardState{{
		Attempt: 1,
		Task:    agk.TaskSucceeded,
		Ports: map[agk.Port]agk.Envelope{
			"out":    outcomeBatch(run, step, "out", 1, "a", "b"),
			"errors": outcomeBatch(run, step, "errors", 1, "c"),
		},
	}}

	ports, err := publish(step, &Step{Call: &Call{Workflow: "finance/common"}}, shards, run, outcomeAt, agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 2 {
		t.Fatalf("the call published %d ports, and the sub-run came back with two", len(ports))
	}
	if got := ports["out"].Meta.Count; got != 2 {
		t.Errorf("out carries %d items, and the sub-run produced two", got)
	}
	if got := ports["errors"].Meta.Count; got != 1 {
		t.Errorf("errors carries %d items, and the sub-run produced one", got)
	}
}

// A step that runs a container publishes what outputs names and nothing else, whatever a
// shard came back with. That is the other half of the same rule, and it is what keeps the
// reading above from becoming a licence for any step to publish a port it never declared.
func TestAStepThatRunsAContainerPublishesOnlyWhatItDeclares(t *testing.T) {
	const run, step = agk.RunID("01HZX"), agk.Step("normalize")
	shards := []ShardState{{
		Shard:   agk.Shard{Index: 1, Of: 1},
		Attempt: 1,
		Task:    agk.TaskSucceeded,
		Ports: map[agk.Port]agk.Envelope{
			"ok":       outcomeBatch(run, step, "ok", 1, "a"),
			"surprise": outcomeBatch(run, step, "surprise", 1, "b"),
		},
	}}

	ports, err := publish(step, &Step{Image: "ghcr.io/acme/x@sha256:0", Outputs: []agk.Port{"ok"}}, shards, run, outcomeAt, agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ports["surprise"]; ok {
		t.Error("a port the step does not declare in outputs was published")
	}
	if len(ports) != 1 {
		t.Errorf("the step published %d ports and declares one", len(ports))
	}
}
