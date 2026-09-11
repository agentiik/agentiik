package graph

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// wait_all waits for every upstream port, then concatenates items in edge declaration
// order. The order is the file's, not the order the steps happened to finish in.
func TestWaitAllConcatenatesInEdgeDeclarationOrder(t *testing.T) {
	parts := []part{
		schedPart("invoice", "out", "invoices", schedItem("a1", nil), schedItem("a2", nil)),
		schedPart("normalize", "rejected", "invoices", schedItem("b1", nil)),
	}
	// The later edge produced first, which changes nothing: the order is declared.
	parts[1].Envelope.Meta.ProducedAt = schedAt.Add(-time.Minute)

	e, unmatched, err := mergePort("archive", "invoices", &Step{Merge: MergeWaitAll}, parts, agk.DefaultLimits())
	if err != nil {
		t.Fatalf("wait_all: %v", err)
	}
	if got := schedIDs(e); !reflect.DeepEqual(got, []string{"a1", "a2", "b1"}) {
		t.Errorf("items %v, want the edges in declaration order", got)
	}
	if e.Meta.Count != 3 {
		t.Errorf("count %d, want 3", e.Meta.Count)
	}
	if e.Meta.Step != "invoice" || e.Meta.Port != "out" {
		t.Errorf("the merged batch says it came from %s.%s, want the first edge", e.Meta.Step, e.Meta.Port)
	}
	if len(unmatched) != 0 {
		t.Errorf("wait_all left %d items unmatched, and only a join leaves any", len(unmatched))
	}
}

// A port fed by one edge carries what its emitter wrote, untouched, whatever the step
// says it merges by.
func TestOneEdgePassesThroughUnchanged(t *testing.T) {
	for _, m := range []Merge{MergeWaitAll, MergeZip, MergeJoin, MergeFirst} {
		parts := []part{schedPart("normalize", "ok", "in", schedItem("a1", map[string]any{"n": 1.0}))}
		e, _, err := mergePort("invoice", "in", &Step{Merge: m, Join: Join{On: "$.data.n"}}, parts, agk.DefaultLimits())
		if err != nil {
			t.Fatalf("merge %v: %v", m, err)
		}
		if !reflect.DeepEqual(e, parts[0].Envelope) {
			t.Errorf("merge %v changed an envelope that had nothing to be merged with", m)
		}
	}
}

// zip pairs items by rank. The pair keeps the identity of the item from the first edge,
// because the engine mints no identifier and a replay speaks about the same element.
func TestZipPairsItemsByRank(t *testing.T) {
	parts := []part{
		schedPart("fetch-ledger", "out", "in", schedItem("l1", map[string]any{"amount": 10.0}), schedItem("l2", map[string]any{"amount": 20.0})),
		schedPart("fetch-bank", "out", "in", schedItem("b1", map[string]any{"cleared": true}), schedItem("b2", map[string]any{"cleared": false})),
	}
	e, _, err := mergePort("zip-rows", "in", &Step{Merge: MergeZip}, parts, agk.DefaultLimits())
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	if got := schedIDs(e); !reflect.DeepEqual(got, []string{"l1", "l2"}) {
		t.Errorf("items %v, want the identities of the first edge", got)
	}
	want := map[string]any{"amount": 10.0, "cleared": true}
	if !reflect.DeepEqual(e.Items[0].Data, want) {
		t.Errorf("the pair carries %v, want %v", e.Items[0].Data, want)
	}
}

// A key both items carry keeps the value of the item that keeps the identity: a later
// edge enriches the first with what it does not already say.
func TestZipKeepsTheFirstEdgesValueForAKeyBothCarry(t *testing.T) {
	parts := []part{
		schedPart("a", "out", "in", schedItem("a1", map[string]any{"k": "left"})),
		schedPart("b", "out", "in", schedItem("b1", map[string]any{"k": "right"})),
	}
	e, _, err := mergePort("s", "in", &Step{Merge: MergeZip}, parts, agk.DefaultLimits())
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	if got := e.Items[0].Data["k"]; got != "left" {
		t.Errorf("the pair carries %v, want the first edge's value", got)
	}
}

// Envelopes of differing lengths produce a validation failure.
func TestZipRefusesEnvelopesOfDifferingLengths(t *testing.T) {
	parts := []part{
		schedPart("fetch-ledger", "out", "in", schedItem("l1", nil), schedItem("l2", nil)),
		schedPart("fetch-bank", "out", "in", schedItem("b1", nil)),
	}
	_, _, err := mergePort("zip-rows", "in", &Step{Merge: MergeZip}, parts, agk.DefaultLimits())
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("zip of 2 items and 1 returned %v, want a refusal", err)
	}
	if refusal.Rule != RuleZipLength {
		t.Errorf("rule %q, want %q", refusal.Rule, RuleZipLength)
	}
	if refusal.Step != "zip-rows" || refusal.Port != "in" {
		t.Errorf("the refusal names %s.%s, want the step and the port it is about", refusal.Step, refusal.Port)
	}
}

// join matches items on the path it names. What matches nothing leaves on the unmatched
// port rather than disappearing quietly, and both sides leave by it.
func TestJoinMatchesOnTheKeyAndLeavesTheRestUnmatched(t *testing.T) {
	left := schedPart("ledger", "out", "in",
		schedItem("l1", map[string]any{"customer_id": "c1"}),
		schedItem("l2", map[string]any{"customer_id": "c2"}))
	right := schedPart("bank", "out", "in",
		schedItem("r1", map[string]any{"customer_id": "c2", "amount": 12.0}),
		schedItem("r2", map[string]any{"customer_id": "c3"}))

	st := &Step{Merge: MergeJoin, Join: Join{On: "$.data.customer_id"}}
	e, unmatched, err := mergePort("match", "in", st, []part{left, right}, agk.DefaultLimits())
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if got := schedIDs(e); !reflect.DeepEqual(got, []string{"l2"}) {
		t.Errorf("matched %v, want the one item both sides carry", got)
	}
	if got := e.Items[0].Data["amount"]; got != 12.0 {
		t.Errorf("the match carries amount %v, want what the other side added", got)
	}
	if got := schedItemIDs(unmatched); !reflect.DeepEqual(got, []string{"l1", "r2"}) {
		t.Errorf("unmatched %v, want the left side's first and then the right side's", got)
	}
}

// Matching is one item to one item: an item whose counterpart has already been taken is
// unmatched, since a fan of matches would need items nobody has minted.
func TestJoinMatchesOneItemToOneItem(t *testing.T) {
	left := schedPart("ledger", "out", "in",
		schedItem("l1", map[string]any{"k": "same"}),
		schedItem("l2", map[string]any{"k": "same"}))
	right := schedPart("bank", "out", "in", schedItem("r1", map[string]any{"k": "same"}))

	st := &Step{Merge: MergeJoin, Join: Join{On: "$.data.k"}}
	e, unmatched, err := mergePort("match", "in", st, []part{left, right}, agk.DefaultLimits())
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if got := schedIDs(e); !reflect.DeepEqual(got, []string{"l1"}) {
		t.Errorf("matched %v, want the first item only", got)
	}
	if got := schedItemIDs(unmatched); !reflect.DeepEqual(got, []string{"l2"}) {
		t.Errorf("unmatched %v, want the item whose counterpart was taken", got)
	}
}

func TestJoinLeavesAnItemWithoutTheKeyUnmatched(t *testing.T) {
	left := schedPart("ledger", "out", "in", schedItem("l1", map[string]any{"other": 1.0}))
	right := schedPart("bank", "out", "in", schedItem("r1", map[string]any{"customer_id": "c1"}))

	st := &Step{Merge: MergeJoin, Join: Join{On: "$.data.customer_id"}}
	e, unmatched, err := mergePort("match", "in", st, []part{left, right}, agk.DefaultLimits())
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if len(e.Items) != 0 {
		t.Errorf("matched %v, want nothing", schedIDs(e))
	}
	if got := schedItemIDs(unmatched); !reflect.DeepEqual(got, []string{"l1", "r1"}) {
		t.Errorf("unmatched %v, want both sides", got)
	}
}

// first lifts the barrier as soon as one upstream port has produced, and the one that
// produced first is the one the port carries.
func TestFirstTakesTheEarliestToProduce(t *testing.T) {
	early := schedPart("zip-rows", "out", "in", schedItem("z1", nil))
	late := schedPart("match", "out", "in", schedItem("m1", nil))
	early.Envelope.Meta.ProducedAt = schedAt.Add(-time.Minute)

	e, _, err := mergePort("whichever-first", "in", &Step{Merge: MergeFirst}, []part{late, early}, agk.DefaultLimits())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if e.Meta.Step != "zip-rows" {
		t.Errorf("the port carries %s, want the step that produced first", e.Meta.Step)
	}
}

func TestFirstBreaksATieOnTheDeclarationOrder(t *testing.T) {
	a := schedPart("zip-rows", "out", "in", schedItem("z1", nil))
	b := schedPart("match", "out", "in", schedItem("m1", nil))

	e, _, err := mergePort("whichever-first", "in", &Step{Merge: MergeFirst}, []part{a, b}, agk.DefaultLimits())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if e.Meta.Step != "zip-rows" {
		t.Errorf("the port carries %s, want the first edge declared", e.Meta.Step)
	}
}

// The merged batch is attributed where its first item came from, and says it was
// complete when the last edge produced and at the highest attempt any of them needed.
func TestTheMergedBatchSaysWhenItWasComplete(t *testing.T) {
	a := schedPart("a", "out", "in", schedItem("a1", nil))
	b := schedPart("b", "out", "in", schedItem("b1", nil))
	b.Envelope.Meta.ProducedAt = schedAt.Add(time.Minute)
	b.Envelope.Meta.Attempt = 3

	e, _, err := mergePort("s", "in", &Step{Merge: MergeZip}, []part{a, b}, agk.DefaultLimits())
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	if !e.Meta.ProducedAt.Equal(schedAt.Add(time.Minute)) {
		t.Errorf("produced_at %s, want the moment the last edge produced", e.Meta.ProducedAt)
	}
	if e.Meta.Attempt != 3 {
		t.Errorf("attempt %d, want the highest any edge needed", e.Meta.Attempt)
	}
}

// A file two items name identically and agree on the bytes of is one file under the
// mount, so the merged item holds it once.
func TestFoldingHoldsTheSameFileOnce(t *testing.T) {
	f := agk.File{Name: "invoice.pdf", SHA256: "abc"}
	other := agk.File{Name: "invoice.pdf", SHA256: "def"}
	got := foldInto(agk.Item{ID: "a", Files: []agk.File{f}}, agk.Item{ID: "b", Files: []agk.File{f, other}})
	if len(got.Files) != 2 {
		t.Errorf("the merged item holds %d files, want the one they agree on and the one they do not", len(got.Files))
	}
	if got.ID != "a" {
		t.Errorf("identity %q, want the item that was merged into", got.ID)
	}
}

func TestFoldingLeavesTheItemsItMergedAlone(t *testing.T) {
	into := schedItem("a", map[string]any{"k": "left"})
	with := schedItem("b", map[string]any{"k": "right", "other": 1.0})
	foldInto(into, with)
	if into.Data["other"] != nil {
		t.Error("folding wrote into the item it merged from")
	}
}

// The other edges of a merge: first are abandoned and their steps cancelled if no other
// consumer needs them.
func TestSupersededCancelsWhatNobodyElseNeeds(t *testing.T) {
	g := &schedGraph{
		wf: &Workflow{},
		steps: map[agk.Step]*Step{
			"slow":     {Outputs: []agk.Port{"out"}},
			"fast":     {Outputs: []agk.Port{"out"}},
			"consumer": {Needs: []Edge{{Step: "fast", Port: "out", As: "in"}, {Step: "slow", Port: "out", As: "in"}}, Merge: MergeFirst},
		},
		order: []agk.Step{"slow", "fast", "consumer"},
	}
	abandoned := []Edge{{Step: "slow", Port: "out", As: "in"}}
	got := superseded(g, "consumer", abandoned, map[agk.Step]StepState{"slow": {Verdict: agk.VerdictRunning}})
	if !reflect.DeepEqual(got, []agk.Step{"slow"}) {
		t.Errorf("superseded %v, want the abandoned step", got)
	}
}

func TestSupersededKeepsAStepAnotherConsumerNeeds(t *testing.T) {
	g := &schedGraph{
		wf: &Workflow{},
		steps: map[agk.Step]*Step{
			"slow":     {Outputs: []agk.Port{"out"}},
			"other":    {Needs: []Edge{{Step: "slow", Port: "out", As: "in"}}},
			"consumer": {Needs: []Edge{{Step: "slow", Port: "out", As: "in"}}, Merge: MergeFirst},
		},
		order: []agk.Step{"slow", "other", "consumer"},
	}
	abandoned := []Edge{{Step: "slow", Port: "out", As: "in"}}
	if got := superseded(g, "consumer", abandoned, map[agk.Step]StepState{}); len(got) != 0 {
		t.Errorf("superseded %v, want nothing: another consumer is still waiting on it", got)
	}
}

// A workflow output is a consumer the steps block does not show.
func TestSupersededKeepsAStepAWorkflowOutputTakes(t *testing.T) {
	taken := Output{}
	taken.From.Step = "slow"
	taken.From.Port = "out"
	g := &schedGraph{
		wf:    &Workflow{Outputs: map[string]Output{"errors": taken}},
		steps: map[agk.Step]*Step{"slow": {Outputs: []agk.Port{"out"}}, "consumer": {Merge: MergeFirst}},
		order: []agk.Step{"slow", "consumer"},
	}
	abandoned := []Edge{{Step: "slow", Port: "out", As: "in"}}
	if got := superseded(g, "consumer", abandoned, map[agk.Step]StepState{}); len(got) != 0 {
		t.Errorf("superseded %v, want nothing: the run itself takes that port", got)
	}
}

func TestSupersededLeavesAStepThatHasAlreadyEnded(t *testing.T) {
	g := &schedGraph{
		wf:    &Workflow{},
		steps: map[agk.Step]*Step{"slow": {Outputs: []agk.Port{"out"}}, "consumer": {Merge: MergeFirst}},
		order: []agk.Step{"slow", "consumer"},
	}
	abandoned := []Edge{{Step: "slow", Port: "out", As: "in"}}
	steps := map[agk.Step]StepState{"slow": {Verdict: agk.VerdictSucceeded}}
	if got := superseded(g, "consumer", abandoned, steps); len(got) != 0 {
		t.Errorf("superseded %v, want nothing: there is nothing left to cancel", got)
	}
}

// schedGraph answers what the abandonment rule reads off the resolved graph.
type schedGraph struct {
	wf    *Workflow
	steps map[agk.Step]*Step
	order []agk.Step
}

func (g *schedGraph) Step(name agk.Step) (*Step, bool) {
	st, ok := g.steps[name]
	return st, ok
}

// published answers the same question the resolved graph answers: every port anything
// takes from a step, which for a sub-workflow call is not the list the step wrote down.
func (g *schedGraph) published(name agk.Step) []agk.Port {
	var ports []agk.Port
	add := func(port agk.Port) {
		if !slices.Contains(ports, port) {
			ports = append(ports, port)
		}
	}
	if st, ok := g.steps[name]; ok {
		for _, port := range st.Outputs {
			add(port)
		}
	}
	for _, other := range g.order {
		for _, e := range g.steps[other].Needs {
			if e.Step == name {
				add(e.Port)
			}
		}
	}
	for _, out := range g.wf.Outputs {
		if out.From.Step == name {
			add(out.From.Port)
		}
	}
	return ports
}

func (g *schedGraph) Consumers(name agk.Step, port agk.Port) []agk.Step {
	var out []agk.Step
	for _, other := range g.order {
		for _, e := range g.steps[other].Needs {
			if e.Step == name && e.Port == port {
				out = append(out, other)
				break
			}
		}
	}
	return out
}

func (g *schedGraph) Workflow() *Workflow { return g.wf }

var schedAt = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

func schedItem(id string, data map[string]any) agk.Item {
	if data == nil {
		data = map[string]any{}
	}
	return agk.Item{ID: id, Data: data, Files: []agk.File{}}
}

func schedEnvelope(step agk.Step, port agk.Port, items ...agk.Item) agk.Envelope {
	if items == nil {
		items = []agk.Item{}
	}
	return agk.Envelope{
		Meta: agk.Meta{
			RunID:      "01HRUN",
			Step:       step,
			Port:       port,
			Attempt:    1,
			Count:      len(items),
			ProducedAt: schedAt,
		},
		Items: items,
	}
}

func schedPart(step agk.Step, port, as agk.Port, items ...agk.Item) part {
	return part{
		Edge:     Edge{Step: step, Port: port, As: as},
		Envelope: schedEnvelope(step, port, items...),
	}
}

func schedIDs(e agk.Envelope) []string { return schedItemIDs(e.Items) }

func schedItemIDs(items []agk.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

// What a key join could not match leaves on the unmatched port, when the step declares
// one, rather than disappearing quietly.
func TestUnmatchedLeavesOnThePortTheStepDeclares(t *testing.T) {
	items := []agk.Item{schedItem("l1", nil), schedItem("r2", nil)}
	st := &Step{Outputs: []agk.Port{"out", "unmatched"}}
	e, ok, err := unmatchedEnvelope("match", st, items, "01HRUN", schedAt, agk.DefaultLimits())
	if err != nil || !ok {
		t.Fatalf("unmatched: %v, %v", ok, err)
	}
	if e.Meta.Port != "unmatched" || e.Meta.Step != "match" || e.Meta.Count != 2 {
		t.Errorf("the unmatched batch carries %#v", e.Meta)
	}
	if err := e.Validate(agk.DefaultLimits()); err != nil {
		t.Errorf("unmatched: %v", err)
	}
}

func TestUnmatchedGoesNowhereWhenTheStepDeclaresNoPort(t *testing.T) {
	st := &Step{Outputs: []agk.Port{"out"}}
	_, ok, err := unmatchedEnvelope("match", st, []agk.Item{schedItem("l1", nil)}, "01HRUN", schedAt, agk.DefaultLimits())
	if err != nil {
		t.Fatalf("unmatched: %v", err)
	}
	if ok {
		t.Error("an unmatched batch was published on a port the step does not declare")
	}
}

// The size rules are measured on what travels, which is the merged batch and not the
// pieces it was made of.
func TestTheMergedBatchIsHeldToTheSizeRules(t *testing.T) {
	parts := []part{
		schedPart("a", "out", "in", schedItem("a1", nil), schedItem("a2", nil)),
		schedPart("b", "out", "in", schedItem("b1", nil), schedItem("b2", nil)),
	}
	// wait_all concatenates, so four items travel; zip pairs, so two do. Each is
	// held to the rule by what it produced and not by what it was given.
	for _, c := range []struct {
		merge Merge
		items int
	}{{MergeWaitAll, 3}, {MergeZip, 1}} {
		narrow := agk.DefaultLimits()
		narrow.MaxItems = c.items
		if _, _, err := mergePort("s", "in", &Step{Merge: c.merge}, parts, narrow); err == nil {
			t.Errorf("merge %v: a merged batch travelled past a limit of %d items", c.merge, c.items)
		}
	}
}

// A step that calls a sub-workflow declares no outputs of its own: "its ports are the
// declared outputs of the workflow it calls, which this commit does not carry". Reading
// the absence of outputs as a step nobody takes anything from would cancel, on the first
// merge: first in the file, a call another consumer is still waiting on.
func TestSupersededKeepsASubWorkflowCallAnotherConsumerNeeds(t *testing.T) {
	g := &schedGraph{
		wf: &Workflow{},
		steps: map[agk.Step]*Step{
			"remind":   {Call: &Call{Workflow: "finance/common"}},
			"escalate": {Needs: []Edge{{Step: "remind", Port: "out", As: "in"}}, Outputs: []agk.Port{"out"}},
			"consumer": {Needs: []Edge{{Step: "remind", Port: "out", As: "in"}}, Merge: MergeFirst},
		},
		order: []agk.Step{"remind", "escalate", "consumer"},
	}
	abandoned := []Edge{{Step: "remind", Port: "out", As: "in"}}
	if got := superseded(g, "consumer", abandoned, map[agk.Step]StepState{}); len(got) != 0 {
		t.Errorf("superseded %v, want nothing: escalate is still waiting on that call", got)
	}
}

// The same absence hides the other consumer a step can have. A workflow output is not in
// the steps block, and a call that feeds one is running for the run itself.
func TestSupersededKeepsASubWorkflowCallAWorkflowOutputTakes(t *testing.T) {
	taken := Output{}
	taken.From.Step = "remind"
	taken.From.Port = "out"
	g := &schedGraph{
		wf: &Workflow{Outputs: map[string]Output{"report": taken}},
		steps: map[agk.Step]*Step{
			"remind":   {Call: &Call{Workflow: "finance/common"}},
			"consumer": {Needs: []Edge{{Step: "remind", Port: "out", As: "in"}}, Merge: MergeFirst},
		},
		order: []agk.Step{"remind", "consumer"},
	}
	abandoned := []Edge{{Step: "remind", Port: "out", As: "in"}}
	if got := superseded(g, "consumer", abandoned, map[agk.Step]StepState{}); len(got) != 0 {
		t.Errorf("superseded %v, want nothing: the run itself takes that port", got)
	}
}
