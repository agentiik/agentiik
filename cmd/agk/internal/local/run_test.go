package local

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
)

// The loop is tested with no Docker in reach, on the precedent the evaluator set: a fake
// graph.Driver is four lines, and every rule this package owns is a rule about what is asked
// of the driver and what is written down, not about containers.

// fanOutAndMerge is the shape the milestone sentence names, in script steps so that nothing
// needs a manifest: two producers, a zip, a fan-out over items, and a merge.
const fanOutAndMerge = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: collect, port: out } }
steps:
  seed:
    image: alpine:3.21
    inputs:
      orders: ${{ workflow.inputs.orders }}
    script: [ "true" ]
    outputs: [out]
  fan:
    image: alpine:3.21
    needs:
      - { step: seed, port: out, as: in }
    strategy: { fan_out: item }
    script: [ "true" ]
    outputs: [out]
  collect:
    image: alpine:3.21
    needs:
      - { step: fan, port: out, as: in }
    script: [ "true" ]
    outputs: [out]
`

// fake is a driver that reports what it is told to and remembers what it was asked.
type fake struct {
	mu    sync.Mutex
	seen  map[agk.TaskID]int
	stops []graph.Stop

	// answer decides what becomes of one task. A nil answer succeeds every task with
	// one item per declared port.
	answer func(graph.Task) (graph.Result, error)
}

func newFake(answer func(graph.Task) (graph.Result, error)) *fake {
	return &fake{seen: map[agk.TaskID]int{}, answer: answer}
}

func (f *fake) Run(ctx context.Context, t graph.Task) (graph.Result, error) {
	f.mu.Lock()
	f.seen[t.ID]++
	f.mu.Unlock()
	if f.answer != nil {
		return f.answer(t)
	}
	return published(t), nil
}

func (f *fake) Stop(ctx context.Context, s graph.Stop) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, s)
	return nil
}

func (f *fake) calls(id agk.TaskID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[id]
}

func (f *fake) stopped() []graph.Stop {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]graph.Stop{}, f.stops...)
}

// published is what a container that did its job hands back: one item out for every item in,
// derived from the task so that two runs of the same task publish the same thing. A step
// given nothing publishes one item, which is what a step that makes its own work does.
func published(t graph.Task) graph.Result {
	out := map[agk.Port]agk.Envelope{}
	count := 0
	for _, envelope := range t.Inputs {
		count += len(envelope.Items)
	}
	if count == 0 {
		count = 1
	}
	for _, port := range t.Outputs {
		items := make([]agk.Item, 0, count)
		for i := range count {
			items = append(items, agk.Item{
				ID:    string(t.Step) + "-" + string(port) + shardSuffix(t.Shard) + "-" + strconv.Itoa(i+1),
				Data:  map[string]any{"step": string(t.Step), "rank": i + 1},
				Files: []agk.File{},
			})
		}
		out[port] = agk.Envelope{
			Meta: agk.Meta{
				RunID:      t.Run,
				Step:       t.Step,
				Port:       port,
				Attempt:    t.Attempt,
				Count:      len(items),
				ProducedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			Items: items,
		}
	}
	return graph.Result{Task: t.ID, State: agk.TaskSucceeded, ExitCode: 0, Outputs: out}
}

func shardSuffix(s agk.Shard) string {
	if s.IsZero() {
		return ""
	}
	return "-" + s.String()
}

// session builds one with no daemon behind it, which is every rule of the loop.
func session(t *testing.T, d graph.Driver) *Session {
	t.Helper()
	l, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatalf("the layout: %s", err)
	}
	return &Session{
		layout:       l,
		limits:       agk.DefaultLimits(),
		tasks:        d,
		announce:     func(s string) { t.Log(s) },
		observations: make(chan driver.Event, observationQueue),
		stores:       map[string]*artifact.Store{},
	}
}

// built parses and resolves one document. Every step is a script step, so no manifest is
// needed and nothing is pulled.
func built(t *testing.T, doc string) *graph.Graph {
	t.Helper()
	wf, err := graph.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("parsing the workflow: %s", err)
	}
	g, err := graph.Build(wf, nil)
	if err != nil {
		t.Fatalf("building the graph: %s", err)
	}
	return g
}

func orders(n int) map[string]any {
	list := make([]any, 0, n)
	for i := range n {
		list = append(list, map[string]any{"id": i + 1})
	}
	return map[string]any{"orders": list}
}

func TestAFanOutAndAMergeRunThroughTheLoopToSucceeded(t *testing.T) {
	f := newFake(nil)
	s := session(t, f)
	g := built(t, fanOutAndMerge)

	var events []Event
	out, err := s.Run(t.Context(), Request{
		Graph:  g,
		Tree:   t.TempDir(),
		Inputs: orders(3),
		Events: func(e Event) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("running the graph: %s", err)
	}
	if out.Run.State != agk.Succeeded {
		t.Fatalf("the run is %s, want succeeded: %v", out.Run.State, out.Failures)
	}
	if out.Run.TriggeredBy != "local" {
		t.Errorf("the run was triggered by %q, want local, which is the word the flag uses", out.Run.TriggeredBy)
	}
	if out.Run.Commit != "" {
		t.Errorf("the run names commit %q: a local run has none, so AGK_COMMIT is absent rather than empty", out.Run.Commit)
	}
	if out.Run.Trigger != agk.TriggerManual {
		t.Errorf("the trigger is %s, want manual", out.Run.Trigger)
	}
	if n := len(out.State.Steps["fan"].Shards); n != 3 {
		t.Fatalf("the fan-out ran %d shards, want one per item", n)
	}

	// One container per task and never two. A loop that started a task without
	// recording the dispatch first would start it again on the next pass.
	for _, shard := range out.State.Steps["fan"].Shards {
		id := agk.NewTaskID(out.Run.ID, "fan", shard.Attempt, shard.Shard)
		if n := f.calls(id); n != 1 {
			t.Errorf("task %s ran %d times, want once", id, n)
		}
	}

	// The declared output is handed back and written down.
	invoices, ok := out.Outputs["invoices"]
	if !ok {
		t.Fatalf("the workflow output invoices was not handed back: %v", out.Outputs)
	}
	if len(invoices.Items) != 3 {
		t.Errorf("invoices carries %d items, want the three the merge handed collect", len(invoices.Items))
	}
	written, err := os.ReadFile(s.layout.Outputs(out.Run.ID) + "/invoices.json")
	if err != nil {
		t.Fatalf("the output envelope was not written: %s", err)
	}
	if !strings.Contains(string(written), "collect-out") {
		t.Errorf("the output envelope on disk is %s", written)
	}

	// The state is beside it, and the run record says this was local.
	if _, err := os.Stat(s.layout.State(out.Run.ID)); err != nil {
		t.Errorf("the state was not written: %s", err)
	}
	record, err := os.ReadFile(s.layout.RunFile(out.Run.ID))
	if err != nil {
		t.Fatalf("the run record was not written: %s", err)
	}
	var held struct {
		agk.Run
		Local bool `json:"local"`
	}
	if err := json.Unmarshal(record, &held); err != nil {
		t.Fatalf("the run record does not read back: %s", err)
	}
	if !held.Local {
		t.Errorf("the run record is %s: a local run is labelled so that a history never mistakes it for a server run", record)
	}
	if held.State != agk.Succeeded {
		t.Errorf("the run record says %s, want the state the run ended in", held.State)
	}

	// The narration names every step and the shards of the fan-out, in the graph's order.
	var steps []string
	for _, e := range events {
		if e.Attempt == 0 && e.Verdict == agk.VerdictSucceeded {
			steps = append(steps, string(e.Step))
		}
	}
	if strings.Join(steps, ",") != "seed,fan,collect" {
		t.Errorf("the narration ends the steps in the order %v, want seed,fan,collect", steps)
	}
}

func TestTwoRunsOfOneGraphProduceTheSameEnvelopes(t *testing.T) {
	s := session(t, newFake(nil))
	g := built(t, fanOutAndMerge)

	first, err := s.Run(t.Context(), Request{Graph: g, Tree: t.TempDir(), Inputs: orders(2)})
	if err != nil {
		t.Fatalf("the first run: %s", err)
	}
	second, err := s.Run(t.Context(), Request{Graph: g, Tree: t.TempDir(), Inputs: orders(2)})
	if err != nil {
		t.Fatalf("the second run: %s", err)
	}
	if first.Run.ID == second.Run.ID {
		t.Fatalf("both runs are %s: a run is minted per run", first.Run.ID)
	}
	a, b := first.Outputs["invoices"], second.Outputs["invoices"]
	if len(a.Items) != len(b.Items) {
		t.Fatalf("the two runs published %d and %d items", len(a.Items), len(b.Items))
	}
	for i := range a.Items {
		if a.Items[i].ID != b.Items[i].ID {
			t.Errorf("item %d is %s and then %s", i, a.Items[i].ID, b.Items[i].ID)
		}
	}
	// The run identifier and the moment are the two things that differ by definition,
	// which is exactly what diff.Default holds aside.
	if a.Meta.RunID == b.Meta.RunID {
		t.Errorf("both envelopes name run %s", a.Meta.RunID)
	}
}

func TestAFailedContainerIsReportedWithItsStepItsCodeAndItsBand(t *testing.T) {
	f := newFake(func(t graph.Task) (graph.Result, error) {
		return graph.Result{Task: t.ID, State: agk.TaskFailed, ExitCode: 17}, nil
	})
	s := session(t, f)

	out, err := s.Run(t.Context(), Request{
		Graph: built(t, oneScriptStep),
		Tree:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("running the graph: %s", err)
	}
	if out.Run.State != agk.Failed {
		t.Fatalf("the run is %s, want failed", out.Run.State)
	}
	if len(out.Failures) != 1 {
		t.Fatalf("%d failures reported, want the one step that failed", len(out.Failures))
	}
	f0 := out.Failures[0]
	if f0.Step != "only" || !f0.HasExit || f0.ExitCode != 17 {
		t.Errorf("the failure is %+v, want step only with exit code 17", f0)
	}
	if f0.Band != agk.BandApplicationFailure {
		t.Errorf("the band is %s, want the application failure row of the table", f0.Band)
	}
	if f0.Refused != "" {
		t.Errorf("the failure says %q was refused: a container that exited reported, and an exit code is not a refusal", f0.Refused)
	}
	if out.Outputs != nil {
		t.Errorf("a failed run was asked for its outputs: %v", out.Outputs)
	}
}

// oneScriptStep is the smallest workflow there is: one step, one port.
const oneScriptStep = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: one, namespace: finance }
steps:
  only:
    image: alpine:3.21
    script: [ "true" ]
    outputs: [out]
`

func TestADriverThatProducedNoContainerIsChargedAndReportsNoExitCode(t *testing.T) {
	refusal := errors.New("driver: step only: the Docker daemon could not be reached, so no container was created and no exit code exists")
	f := newFake(func(t graph.Task) (graph.Result, error) { return graph.Result{}, refusal })
	s := session(t, f)

	out, err := s.Run(t.Context(), Request{Graph: built(t, oneScriptStep), Tree: t.TempDir()})
	if err != nil {
		t.Fatalf("running the graph: %s", err)
	}
	if out.Run.State != agk.Failed {
		t.Fatalf("the run is %s, want failed: a shard left dispatched would hang the run", out.Run.State)
	}
	if len(out.Failures) != 1 {
		t.Fatalf("%d failures reported: %+v", len(out.Failures), out.Failures)
	}
	f0 := out.Failures[0]
	if f0.HasExit {
		t.Errorf("the failure carries exit code %d: no container ran, and the driver invents none", f0.ExitCode)
	}
	if f0.Refused != refusal.Error() {
		t.Errorf("what was refused reads %q, want the driver's own sentence", f0.Refused)
	}
	// 125 is the runtime row of the exit-code table, which agk.Band already makes
	// unretryable and nameless to retry.on.
	if got := out.State.Steps["only"].Shards[0].ExitCode; got != platformFailure {
		t.Errorf("the evaluator was told exit code %d, want %d", got, platformFailure)
	}
	if f0.Band != agk.BandRuntimeFailure {
		t.Errorf("the band is %s, want the runtime row", f0.Band)
	}
}

func TestABrickChargeWithNoContainerIsInvalidInput(t *testing.T) {
	// A manifest that breaks the contract is the brick's, and the code for it is 120:
	// a permanent failure, never retried, whatever retry says.
	f := newFake(func(t graph.Task) (graph.Result, error) {
		return graph.Result{}, &driver.Fault{Step: t.Step, Charge: driver.ChargeBrick, Detail: "the image does not honour the brick contract"}
	})
	s := session(t, f)

	out, err := s.Run(t.Context(), Request{Graph: built(t, oneScriptStep), Tree: t.TempDir()})
	if err != nil {
		t.Fatalf("running the graph: %s", err)
	}
	if got := out.State.Steps["only"].Shards[0].ExitCode; got != brickWithNoContainer {
		t.Errorf("the evaluator was told exit code %d, want %d, which is invalid input", got, brickWithNoContainer)
	}
	if len(out.Failures) != 1 || out.Failures[0].Charge != driver.ChargeBrick {
		t.Errorf("the failure is %+v, want one charged to the brick", out.Failures)
	}
}

func TestAnInterruptStopsTheTasksAndTheRunReportsCancelled(t *testing.T) {
	cancel := make(chan struct{})
	stopped := make(chan struct{})
	var once sync.Once

	var f *fake
	f = newFake(func(task graph.Task) (graph.Result, error) {
		// The container is running, which is what the interrupt arrives during.
		once.Do(func() { close(cancel) })
		<-stopped
		return graph.Result{Task: task.ID, State: agk.TaskCancelled}, nil
	})
	s := session(t, f)

	ctx, interrupt := context.WithCancel(t.Context())
	go func() {
		<-cancel
		interrupt()
		// The stop reaches the driver through the plan, and the task reports once it
		// has. Waiting for the stop here is what proves the order.
		for len(f.stopped()) == 0 {
			time.Sleep(time.Millisecond)
		}
		close(stopped)
	}()

	out, err := s.Run(ctx, Request{Graph: built(t, oneScriptStep), Tree: t.TempDir()})
	if err != nil {
		t.Fatalf("running the graph: %s", err)
	}
	if out.Run.State != agk.Cancelled {
		t.Fatalf("the run is %s, want cancelled", out.Run.State)
	}
	if len(f.stopped()) == 0 {
		t.Errorf("no stop reached the driver: a terminal run still has containers to call off")
	}
	if len(out.Failures) != 0 {
		t.Errorf("a cancelled task is reported as a failure: %+v", out.Failures)
	}
}

func TestASessionHoldsOneRunAtATime(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f := newFake(func(task graph.Task) (graph.Result, error) {
		once.Do(func() { close(started) })
		<-release
		return published(task), nil
	})
	s := session(t, f)
	g := built(t, oneScriptStep)

	done := make(chan error, 1)
	go func() {
		_, err := s.Run(t.Context(), Request{Graph: g, Tree: t.TempDir()})
		done <- err
	}()
	<-started

	if _, err := s.Run(t.Context(), Request{Graph: g, Tree: t.TempDir()}); err == nil {
		t.Errorf("a second run was accepted while the first was in flight")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the first run: %s", err)
	}
}

func TestTheObservationsOfTheDriverAreNarratedAndNotRecorded(t *testing.T) {
	s := session(t, nil)
	f := newFake(func(task graph.Task) (graph.Result, error) {
		// The driver's own transitions, posted the way the observer posts them.
		s.observations <- driver.Event{Task: task.ID, State: agk.TaskRunning}
		s.observations <- driver.Event{Task: task.ID, State: agk.TaskPublishing}
		return published(task), nil
	})
	s.tasks = f

	var seen []agk.TaskState
	out, err := s.Run(t.Context(), Request{
		Graph: built(t, oneScriptStep),
		Tree:  t.TempDir(),
		// An attempt of zero is the step itself rather than one of its shards, which
		// is how the two kinds of line are told apart.
		Events: func(e Event) {
			if e.Attempt > 0 {
				seen = append(seen, e.State)
			}
		},
	})
	if err != nil {
		t.Fatalf("running the graph: %s", err)
	}
	if out.Run.State != agk.Succeeded {
		t.Fatalf("the run is %s", out.Run.State)
	}
	var narrated []string
	for _, state := range seen {
		narrated = append(narrated, state.String())
	}
	joined := strings.Join(narrated, ",")
	for _, want := range []string{"dispatched", "running", "publishing", "succeeded"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the narration is %s, and %s is missing", joined, want)
		}
	}
}

func TestASecretNobodySuppliedIsNamedWithEveryStepThatMountsIt(t *testing.T) {
	doc := `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: one, namespace: finance }
secrets: [billing_api, ledger]
steps:
  first:
    image: alpine:3.21
    script: [ "true" ]
    outputs: [out]
    secrets: [billing_api]
  second:
    image: alpine:3.21
    needs: [first]
    script: [ "true" ]
    outputs: [out]
    secrets: [billing_api, ledger]
`
	wf, err := graph.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("parsing: %s", err)
	}
	missing := Missing(wf, map[string][]byte{"ledger": []byte("x")})
	if len(missing) != 1 {
		t.Fatalf("%d secrets missing, want the one that was not supplied: %v", len(missing), missing)
	}
	steps := missing["billing_api"]
	if len(steps) != 2 || steps[0] != "first" || steps[1] != "second" {
		t.Errorf("billing_api is missing from %v, want both steps that mount it", steps)
	}
	if Missing(wf, map[string][]byte{"ledger": []byte("x"), "billing_api": []byte("y")}) != nil {
		t.Errorf("a workflow whose secrets were all supplied still reported one missing")
	}
}

func TestASecretIsRedeemedAtTheLastMomentAndARefusalNamesTheFlag(t *testing.T) {
	s := session(t, nil)
	release, err := s.hold(&inflight{
		run:     agk.Run{ID: agk.NewRunID()},
		tree:    t.TempDir(),
		secrets: map[string][]byte{"billing_api": []byte("sk-1")},
	})
	if err != nil {
		t.Fatalf("holding the run: %s", err)
	}
	defer release()

	value, err := (secrets{s}).Value(t.Context(), "billing_api")
	if err != nil {
		t.Fatalf("redeeming a supplied secret: %s", err)
	}
	if string(value) != "sk-1" {
		t.Errorf("the value is %q", value)
	}
	_, err = (secrets{s}).Value(t.Context(), "ledger")
	if err == nil || !strings.Contains(err.Error(), "--secret") {
		t.Errorf("the refusal of an unsupplied secret is %v, and it has to name the flag that supplies one", err)
	}
}

// retryOnce is a step that says one further attempt is made on a transient failure, which is
// the one row of the exit-code table the step's policy reaches.
const retryOnce = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: one, namespace: finance }
steps:
  only:
    image: alpine:3.21
    script: [ "true" ]
    outputs: [out]
    retry: { max: 1, on: [transient], backoff: { base: 30s } }
`

func TestAShardThatRetriedAndSucceededIsNotAFailure(t *testing.T) {
	var attempts int
	f := newFake(func(task graph.Task) (graph.Result, error) {
		attempts++
		if attempts == 1 {
			// 100 to 119 is the transient band, which is the one the step policy
			// can ask for another attempt on.
			return graph.Result{Task: task.ID, State: agk.TaskFailed, ExitCode: 100}, nil
		}
		return published(task), nil
	})
	s := session(t, f)

	// The clock moves on every reading, so the backoff the evaluator places in the
	// future is already past by the time the loop asks again and the test takes no
	// thirty seconds to run. The waiting itself is still a timer and not a poll.
	base := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	var ticks int
	s.now = func() time.Time {
		ticks++
		return base.Add(time.Duration(ticks) * 10 * time.Second)
	}

	var dispatches int
	out, err := s.Run(t.Context(), Request{
		Graph: built(t, retryOnce),
		Tree:  t.TempDir(),
		Events: func(e Event) {
			if e.State == agk.TaskDispatched {
				dispatches++
			}
		},
	})
	if err != nil {
		t.Fatalf("running the graph: %s", err)
	}
	if out.Run.State != agk.Succeeded {
		t.Fatalf("the run is %s, want succeeded on the second attempt: %+v", out.Run.State, out.Failures)
	}
	if attempts != 2 {
		t.Errorf("%d attempts ran, want the one that failed and the one the policy allowed", attempts)
	}
	if got := out.State.Steps["only"].Shards[0].Attempt; got != 2 {
		t.Errorf("the shard ended on attempt %d, want 2", got)
	}
	if dispatches != 2 {
		t.Errorf("%d dispatches were narrated, want one per attempt", dispatches)
	}
	if len(out.Failures) != 0 {
		t.Errorf("a shard that failed and then succeeded is reported as a failure: %+v", out.Failures)
	}
}

func TestTheStateLeftBehindIsOneASecondProcessCouldResumeFrom(t *testing.T) {
	s := session(t, newFake(nil))
	g := built(t, fanOutAndMerge)

	out, err := s.Run(t.Context(), Request{Graph: g, Tree: t.TempDir(), Inputs: orders(2)})
	if err != nil {
		t.Fatalf("running the graph: %s", err)
	}

	doc, err := os.ReadFile(s.layout.State(out.Run.ID))
	if err != nil {
		t.Fatalf("the state was not written: %s", err)
	}
	var state graph.State
	if err := json.Unmarshal(doc, &state); err != nil {
		t.Fatalf("the state does not read back: %s", err)
	}
	if state.Seq == 0 {
		t.Errorf("the state counts no decisions, and every Next and every Record is one")
	}
	// "Failover is a state resume and never a rebuild": the file on disk is a state the
	// evaluator takes back. Resuming is not claimed at v0.1.0, and the file being one a
	// second process could resume from is why it is written at all.
	again, err := graph.New(g, &state, agk.DefaultLimits())
	if err != nil {
		t.Fatalf("the state is not one the evaluator takes back: %s", err)
	}
	plan, err := again.Next(time.Now())
	if err != nil {
		t.Fatalf("asking the resumed evaluator what to do next: %s", err)
	}
	if len(plan.Start) != 0 || len(plan.Stop) != 0 {
		t.Errorf("the resumed run wants to start %d and stop %d: the run it was written from had ended", len(plan.Start), len(plan.Stop))
	}
	if again.State().Run.State != agk.Succeeded {
		t.Errorf("the resumed run is %s, want the state the run ended in", again.State().Run.State)
	}
}
