package graph

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// A step becomes runnable once every declared input port is satisfied, and a step
// declaring none is runnable from the start: there is no implicit stage and no order the
// file's own layout imposes.
func TestAStepWithNoEdgesStartsAtOnce(t *testing.T) {
	g, arrival, _ := schedBarrier(t, &Step{}, nil, nil, nil)
	if g != gateStart {
		t.Fatalf("gate %v, want the barrier lifted", g)
	}
	if len(arrival.Inputs) != 0 {
		t.Errorf("inputs %v, want nothing: the step declares no port", arrival.Inputs)
	}
}

// This is a barrier and not a continuous stream: a port that has arrived starts nothing
// while another has not.
func TestTheBarrierWaitsForEveryDeclaredPort(t *testing.T) {
	st := &Step{Needs: []Edge{
		{Step: "invoice", Port: "out", As: "invoices"},
		{Step: "normalize", Port: "rejected", As: "rejected"},
	}}
	steps := map[agk.Step]StepState{
		"invoice":   schedPublished("invoice", "out", schedItem("a1", nil)),
		"normalize": {Verdict: agk.VerdictRunning},
	}
	if g, _, _ := schedBarrier(t, st, st.Needs, steps, nil); g != gateWait {
		t.Errorf("gate %v, want the barrier held", g)
	}
}

func TestTheBarrierLiftsWhenEveryPortIsSatisfied(t *testing.T) {
	st := &Step{Needs: []Edge{
		{Step: "invoice", Port: "out", As: "invoices"},
		{Step: "normalize", Port: "rejected", As: "rejected"},
	}}
	steps := map[agk.Step]StepState{
		"invoice":   schedPublished("invoice", "out", schedItem("a1", nil)),
		"normalize": schedPublished("normalize", "rejected", schedItem("b1", nil)),
	}
	gate, arrival, _ := schedBarrier(t, st, st.Needs, steps, nil)
	if gate != gateStart {
		t.Fatalf("gate %v, want the barrier lifted", gate)
	}
	if got := schedIDs(arrival.Inputs["invoices"]); !reflect.DeepEqual(got, []string{"a1"}) {
		t.Errorf("invoices carries %v", got)
	}
	if got := schedIDs(arrival.Inputs["rejected"]); !reflect.DeepEqual(got, []string{"b1"}) {
		t.Errorf("rejected carries %v", got)
	}
}

// A port fed by the step's own inputs keyword creates no dependency on another step, so
// it is satisfied the moment the run holds the value.
func TestAPortTheStepFeedsItselfWaitsForNobody(t *testing.T) {
	fed := map[agk.Port]agk.Envelope{"orders": schedEnvelope("normalize", "orders", schedItem("o1", nil))}
	gate, arrival, _ := schedBarrier(t, &Step{}, nil, nil, fed)
	if gate != gateStart {
		t.Fatalf("gate %v, want the barrier lifted", gate)
	}
	if got := schedIDs(arrival.Inputs["orders"]); !reflect.DeepEqual(got, []string{"o1"}) {
		t.Errorf("orders carries %v", got)
	}
}

// A skipped step publishes empty envelopes on all its ports, and the steps below decide
// their own fate through when: the default, [succeeded], does not admit it.
func TestWhenDecidesWhatASkippedUpstreamMeans(t *testing.T) {
	edges := []Edge{{Step: "normalize", Port: "ok", As: "in"}}
	steps := map[agk.Step]StepState{"normalize": {
		Verdict: agk.VerdictSkipped,
		Ports:   map[agk.Port]agk.Envelope{"ok": agk.Empty("01HRUN", "normalize", "ok", 1, schedAt)},
		Since:   schedAt,
	}}

	gate, _, reason := schedBarrier(t, &Step{Needs: edges}, edges, steps, nil)
	if gate != gateSkip {
		t.Fatalf("gate %v, want the step skipped: when defaults to [succeeded]", gate)
	}
	if !strings.Contains(reason, "normalize") || !strings.Contains(reason, "skipped") {
		t.Errorf("reason %q, want the step and the state it reached", reason)
	}

	st := &Step{Needs: edges, When: []When{WhenSucceeded, WhenSkipped}}
	gate, arrival, _ := schedBarrier(t, st, edges, steps, nil)
	if gate != gateStart {
		t.Fatalf("gate %v, want the step started: when names skipped", gate)
	}
	if got := arrival.Inputs["in"]; got.Meta.Count != 0 || len(got.Items) != 0 {
		t.Errorf("in carries %d items, want the batch of nothing a skipped step publishes", len(got.Items))
	}
}

// A step that failed published nothing at all. A when that names the state still has to
// be able to start, so the port carries a batch of nothing.
func TestAFailedUpstreamArrivesAsAnEmptyEnvelope(t *testing.T) {
	edges := []Edge{{Step: "invoice", Port: "out", As: "in"}}
	steps := map[agk.Step]StepState{"invoice": {Verdict: agk.VerdictFailed, Since: schedAt}}

	if gate, _, _ := schedBarrier(t, &Step{Needs: edges}, edges, steps, nil); gate != gateSkip {
		t.Errorf("gate %v, want the step skipped: when defaults to [succeeded]", gate)
	}

	st := &Step{Needs: edges, When: []When{WhenAlways}}
	gate, arrival, _ := schedBarrier(t, st, edges, steps, nil)
	if gate != gateStart {
		t.Fatalf("gate %v, want the cleanup step started", gate)
	}
	got := arrival.Inputs["in"]
	if got.Meta.Count != 0 || got.Meta.Step != "invoice" || got.Meta.Port != "out" {
		t.Errorf("in carries %#v, want a batch of nothing from the step that failed", got.Meta)
	}
	if !got.Meta.ProducedAt.Equal(schedAt) {
		t.Errorf("produced_at %s, want the moment the step ended and not the moment this was read", got.Meta.ProducedAt)
	}
}

// merge: first lifts the barrier as soon as one upstream port has produced. The other
// edges are abandoned, whether or not their steps have ended.
func TestFirstLiftsTheBarrierOnOneEdge(t *testing.T) {
	edges := []Edge{
		{Step: "slow", Port: "out", As: "in"},
		{Step: "fast", Port: "out", As: "in"},
	}
	steps := map[agk.Step]StepState{
		"slow": {Verdict: agk.VerdictRunning},
		"fast": schedPublished("fast", "out", schedItem("f1", nil)),
	}
	st := &Step{Needs: edges, Merge: MergeFirst}
	gate, arrival, _ := schedBarrier(t, st, edges, steps, nil)
	if gate != gateStart {
		t.Fatalf("gate %v, want the barrier lifted by the edge that produced", gate)
	}
	if arrival.Inputs["in"].Meta.Step != "fast" {
		t.Errorf("in carries %s, want the edge that produced", arrival.Inputs["in"].Meta.Step)
	}
	if !reflect.DeepEqual(arrival.Abandoned, []Edge{edges[0]}) {
		t.Errorf("abandoned %v, want the edge nobody waited for", arrival.Abandoned)
	}
}

// The state of a step merge: first abandoned is the engine's own doing, so it is not
// read as an upstream state the step is judged on.
func TestFirstDoesNotJudgeAStepOnTheEdgeItAbandoned(t *testing.T) {
	edges := []Edge{
		{Step: "fast", Port: "out", As: "in"},
		{Step: "slow", Port: "out", As: "in"},
	}
	steps := map[agk.Step]StepState{
		"fast": schedPublished("fast", "out", schedItem("f1", nil)),
		"slow": {Verdict: agk.VerdictFailed, Since: schedAt},
	}
	st := &Step{Needs: edges, Merge: MergeFirst}
	if gate, _, reason := schedBarrier(t, st, edges, steps, nil); gate != gateStart {
		t.Errorf("gate %v (%s), want the barrier lifted by the edge that produced", gate, reason)
	}
}

// A step whose if condition is false, and a step when kept from starting, publish empty
// envelopes on all of their ports, which is not the same as publishing nothing: a step
// that published nothing would hang every step below it on a port that is never coming.
func TestASkippedStepPublishesAnEmptyEnvelopeOnEveryPort(t *testing.T) {
	st := &Step{Outputs: []agk.Port{"out", "error"}}
	published, err := publish("invoice", st, nil, "01HRUN", schedAt, agk.DefaultLimits())
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(published) != 2 {
		t.Fatalf("published %d ports, want every port the step declares", len(published))
	}
	for _, port := range st.Outputs {
		e, ok := published[port]
		if !ok {
			t.Errorf("port %s published nothing", port)
			continue
		}
		if e.Meta.Count != 0 || len(e.Items) != 0 {
			t.Errorf("port %s carries %d items, want a batch of nothing", port, len(e.Items))
		}
		if e.Meta.Step != "invoice" || e.Meta.Port != port || e.Meta.Attempt != 1 {
			t.Errorf("port %s carries %#v", port, e.Meta)
		}
		if err := e.Validate(agk.DefaultLimits()); err != nil {
			t.Errorf("port %s: %v", port, err)
		}
	}
}

// A step that is not going to run is not refused for a merge it was never going to be
// given, so when is read before anything is merged.
func TestWhenIsReadBeforeAnythingIsMerged(t *testing.T) {
	edges := []Edge{
		{Step: "ledger", Port: "out", As: "in"},
		{Step: "bank", Port: "out", As: "in"},
	}
	steps := map[agk.Step]StepState{
		"ledger": schedPublished("ledger", "out", schedItem("l1", nil), schedItem("l2", nil)),
		"bank":   {Verdict: agk.VerdictFailed, Since: schedAt},
	}
	st := &Step{Needs: edges, Merge: MergeZip}
	gate, _, reason := schedBarrier(t, st, edges, steps, nil)
	if gate != gateSkip {
		t.Fatalf("gate %v, want the step skipped rather than refused for lengths it never had to pair", gate)
	}
	if !strings.Contains(reason, "bank") {
		t.Errorf("reason %q, want the upstream that kept it from starting", reason)
	}
}

// Reading the barrier decides nothing and changes nothing, so reading it twice at the
// same moment says the same thing. Next is pure, and this is where that starts.
func TestTheBarrierIsAReading(t *testing.T) {
	edges := []Edge{{Step: "normalize", Port: "ok", As: "in"}}
	steps := map[agk.Step]StepState{"normalize": schedPublished("normalize", "ok", schedItem("a1", nil))}
	st := &Step{Needs: edges}

	first, firstArrival, _ := schedBarrier(t, st, edges, steps, nil)
	second, secondArrival, _ := schedBarrier(t, st, edges, steps, nil)
	if first != second || !reflect.DeepEqual(firstArrival, secondArrival) {
		t.Error("the barrier read twice at the same moment said two different things")
	}
}

// when names succeeded, failed, skipped and always, and defaults to [succeeded].
func TestAdmitsReadsTheUpstreamStatesWhenNames(t *testing.T) {
	for _, c := range []struct {
		when    []When
		verdict agk.Verdict
		want    bool
	}{
		{nil, agk.VerdictSucceeded, true},
		{nil, agk.VerdictFailed, false},
		{nil, agk.VerdictSkipped, false},
		{[]When{WhenSucceeded}, agk.VerdictSucceeded, true},
		{[]When{WhenFailed}, agk.VerdictFailed, true},
		{[]When{WhenFailed}, agk.VerdictSucceeded, false},
		{[]When{WhenSkipped}, agk.VerdictSkipped, true},
		{[]When{WhenSucceeded, WhenSkipped}, agk.VerdictSkipped, true},
		{[]When{WhenAlways}, agk.VerdictFailed, true},
		{[]When{WhenAlways}, agk.VerdictCancelled, true},
		// Cancelled is work a principal, a concurrency group or a merge: first
		// called off, and when has no name for it.
		{[]When{WhenSucceeded, WhenFailed, WhenSkipped}, agk.VerdictCancelled, false},
	} {
		if got := admits(c.when, c.verdict); got != c.want {
			t.Errorf("admits(%v, %s) = %v, want %v", c.when, c.verdict, got, c.want)
		}
	}
}

// schedBarrier reads the barrier the way the evaluator does, with the moment fixed so
// that reading it twice says the same thing.
func schedBarrier(t *testing.T, st *Step, edges []Edge, steps map[agk.Step]StepState, fed map[agk.Port]agk.Envelope) (gate, *arrival, string) {
	t.Helper()
	if steps == nil {
		steps = map[agk.Step]StepState{}
	}
	g, a, reason, err := barrier("consumer", st, edges, steps, fed, "01HRUN", agk.DefaultLimits(), schedAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("barrier: %v", err)
	}
	return g, a, reason
}

// schedPublished is a step that ended and published one port.
func schedPublished(step agk.Step, port agk.Port, items ...agk.Item) StepState {
	return StepState{
		Verdict: agk.VerdictSucceeded,
		Ports:   map[agk.Port]agk.Envelope{port: schedEnvelope(step, port, items...)},
		Since:   schedAt,
	}
}

// A run condition answers true or false, and a workflow whose condition answers
// something else has written a test that does not test anything.
func TestARunConditionIsTrueOrFalse(t *testing.T) {
	if got, err := truth("invoice", "${{ inputs.in.count > 0 }}", true); err != nil || !got {
		t.Errorf("truth(true) = %v, %v", got, err)
	}
	if got, err := truth("invoice", "${{ inputs.in.count > 0 }}", false); err != nil || got {
		t.Errorf("truth(false) = %v, %v", got, err)
	}
	if _, err := truth("invoice", "${{ inputs.in.count }}", 3); err == nil {
		t.Error("a condition answering a number was read as a truth value")
	}
	if _, err := truth("invoice", "${{ vars.currency }}", "EUR"); err == nil {
		t.Error("a condition answering a string was read as a truth value")
	}
}
