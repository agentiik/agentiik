package graph

import (
	"fmt"
	"slices"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// The four merge strategies, which are what several edges arriving on one input port are
// combined by. A port carries exactly one envelope, so a port fed by three edges has to
// become one envelope before the step can be given anything at all, and the rule that
// makes it one is written in the workflow file rather than hidden in whatever runs the
// container.
//
//	wait_all  waits for every upstream port, then concatenates items in edge
//	          declaration order. The default when a port has more than one edge.
//	zip       pairs items by rank. Envelopes of differing lengths are a failure.
//	join      matches items on a JSON path. What matches nothing leaves on the
//	          step's unmatched output port, when the step declares one.
//	first     lifts the barrier as soon as one upstream port has produced.
//
// Three readings the documentation does not state are taken here, because two items
// becoming one has to settle what happens to the identity and to a key they both carry.
//
// A zipped pair and a joined match keep the identity of the item from the first edge,
// and the other items fold into it. The engine mints no identifier, deliberately, and an
// identifier invented twice would make two items one; keeping the first edge's item is
// also what lets a replay speak about the same element after the merge.
//
// A key both items carry keeps the value of the item that keeps the identity. The first
// edge declared is the one the merged item is, and a later edge enriches it with what it
// does not already say rather than writing over it.
//
// The merged envelope carries the metadata of the first edge's envelope, with the count
// rebuilt, the highest attempt any of them needed and the latest moment any of them was
// produced. That is Concat's own reading applied one level up: the batch is complete
// when the last edge has produced, and it is attributed where its first item came from.

// part is one edge arriving on an input port, with the envelope its step published on it.
type part struct {
	Edge     Edge
	Envelope agk.Envelope
}

// mergePort combines what arrives on one input port into the single envelope that port
// carries, and returns beside it the items a join could not match. The unmatched items
// are the caller's to publish on the step's own unmatched output port, because it is the
// engine that writes that port and not the container.
//
// A port fed by one edge is that edge's envelope, whatever the strategy says: there is
// nothing to pair, to match or to wait for, and the envelope travels as its emitter
// wrote it.
func mergePort(step agk.Step, port agk.Port, st *Step, parts []part, l agk.Limits) (agk.Envelope, []agk.Item, error) {
	switch {
	case len(parts) == 0:
		return agk.Envelope{}, nil, fmt.Errorf("graph: step %s: port %s: a port is merged from at least one edge and there is nothing here to merge", step, port)
	case len(parts) == 1:
		return parts[0].Envelope, nil, nil
	}

	switch mergeOf(st) {
	case MergeZip:
		e, err := zipParts(step, port, parts, l)
		return e, nil, err
	case MergeJoin:
		return joinParts(step, port, joinOn(st), parts, l)
	case MergeFirst:
		return firstPart(parts).Envelope, nil, nil
	default:
		e, err := waitAllParts(parts, l)
		return e, nil, err
	}
}

// waitAllParts concatenates the edges in declaration order.
//
// The concatenation is agk.Concat, which is where the shards of one port are joined and
// where the size rules are measured on the result rather than on any piece of it. Concat
// holds the pieces of one port to one run, one step and one port, which is true of shards
// and is exactly what is not true of edges, so each envelope is restated as coming from
// the first edge before they are handed over. Nothing about the items changes: the
// metadata says where the batch that now travels came from, and the batch came from the
// port the first edge named.
func waitAllParts(parts []part, l agk.Limits) (agk.Envelope, error) {
	base := parts[0].Envelope.Meta
	pieces := make([]agk.Envelope, 0, len(parts))
	for _, p := range parts {
		e := p.Envelope
		e.Meta.RunID = base.RunID
		e.Meta.Step = base.Step
		e.Meta.Port = base.Port
		pieces = append(pieces, e)
	}
	return agk.Concat(pieces, l)
}

// zipParts pairs items by rank.
//
// Envelopes of differing lengths produce a validation failure, which is the one rule the
// documentation states about zip and the reason it is a refusal here rather than a
// shorter result: a workflow zipping two batches has said the two batches correspond,
// and pairing eight rows with seven is a workflow whose assumption has broken, not a
// batch of seven.
func zipParts(step agk.Step, port agk.Port, parts []part, l agk.Limits) (agk.Envelope, error) {
	n := len(parts[0].Envelope.Items)
	for _, p := range parts[1:] {
		if got := len(p.Envelope.Items); got != n {
			return agk.Envelope{}, refuse(RuleZipLength, step, port, fmt.Sprintf(
				"merge: zip pairs items by rank, and envelopes of differing lengths produce a validation failure: %s.%s carries %d %s and %s.%s carries %d",
				parts[0].Edge.Step, parts[0].Edge.Port, n, itemsWord(n), p.Edge.Step, p.Edge.Port, got))
		}
	}

	items := make([]agk.Item, 0, n)
	for i := range n {
		it := parts[0].Envelope.Items[i]
		for _, p := range parts[1:] {
			it = foldInto(it, p.Envelope.Items[i])
		}
		items = append(items, it)
	}
	return rebuild(parts, items, l)
}

// joinParts matches items on the JSON path the join names.
//
// Matching is one item to one item, in order, and an item whose counterpart has already
// been taken is unmatched. The documentation leaves the multiplicity open, and this is
// the reading the rest of the design forces: a key matching three items on one side and
// two on the other would have to produce items that do not exist yet, and the engine
// mints no identifier. Nothing is lost by it, because what is not matched leaves on the
// unmatched port instead of disappearing quietly.
//
// The unmatched items are the left side's first, then each other edge's in declaration
// order, so that reading the unmatched port tells you which side an item came from by
// where it sits.
func joinParts(step agk.Step, port agk.Port, on string, parts []part, l agk.Limits) (agk.Envelope, []agk.Item, error) {
	sel, err := parsePath(on)
	if err != nil {
		return agk.Envelope{}, nil, fmt.Errorf("graph: step %s: port %s: merge: join: on: %w", step, port, err)
	}

	// The right hand sides, indexed by key in the order they arrived, with what has
	// already been taken marked so that one item answers to one match.
	rights := make([]map[string][]int, len(parts)-1)
	taken := make([][]bool, len(parts)-1)
	for i, p := range parts[1:] {
		index := make(map[string][]int, len(p.Envelope.Items))
		for j, it := range p.Envelope.Items {
			if k, ok := sel.key(it); ok {
				index[k] = append(index[k], j)
			}
		}
		rights[i] = index
		taken[i] = make([]bool, len(p.Envelope.Items))
	}

	left := parts[0].Envelope.Items
	items := make([]agk.Item, 0, len(left))
	unmatched := make([]agk.Item, 0)
	for _, it := range left {
		k, ok := sel.key(it)
		if !ok {
			unmatched = append(unmatched, it)
			continue
		}
		// Every side has to answer before any of them is marked as taken, so that a
		// key matching on one edge and not on the next leaves both items free for a
		// later one.
		found := make([]int, len(rights))
		complete := true
		for i := range rights {
			at := -1
			for _, j := range rights[i][k] {
				if !taken[i][j] {
					at = j
					break
				}
			}
			if at < 0 {
				complete = false
				break
			}
			found[i] = at
		}
		if !complete {
			unmatched = append(unmatched, it)
			continue
		}
		merged := it
		for i, j := range found {
			taken[i][j] = true
			merged = foldInto(merged, parts[i+1].Envelope.Items[j])
		}
		items = append(items, merged)
	}
	for i, p := range parts[1:] {
		for j, it := range p.Envelope.Items {
			if !taken[i][j] {
				unmatched = append(unmatched, it)
			}
		}
	}

	e, err := rebuild(parts, items, l)
	if err != nil {
		return agk.Envelope{}, nil, err
	}
	return e, unmatched, nil
}

// unmatchedPort is the name a join leaves what it could not match on. It is the step's
// own output port, published by the engine without the container ever touching it, which
// is why a step doing a join declares it in outputs.
const unmatchedPort agk.Port = "unmatched"

// unmatchedEnvelope is the batch a key join could not match, as the port carries it. The
// second result is false where the step declares no unmatched port, which is the
// language's own condition: what could not be matched leaves on that port if the step
// declares one, and a workflow that does not declare it has said it does not want it.
//
// The batch is attributed to the step that did the join and to attempt 1. The join is
// the engine's own work, done once when the barrier lifted, and not the output of a
// container attempt.
func unmatchedEnvelope(name agk.Step, st *Step, items []agk.Item, run agk.RunID, at time.Time, l agk.Limits) (agk.Envelope, bool, error) {
	if !slices.Contains(st.Outputs, unmatchedPort) {
		return agk.Envelope{}, false, nil
	}
	if items == nil {
		items = []agk.Item{}
	}
	e := agk.Envelope{
		Meta: agk.Meta{
			RunID:      run,
			Step:       name,
			Port:       unmatchedPort,
			Attempt:    1,
			Count:      len(items),
			ProducedAt: at.UTC(),
		},
		Items: items,
	}
	if err := e.Validate(l); err != nil {
		return agk.Envelope{}, false, fmt.Errorf("graph: step %s: port %s: %w", name, unmatchedPort, err)
	}
	return e, true, nil
}

// firstPart is the edge that lifted the barrier: the one that produced first, and the
// first declared among those that produced together. The barrier has already chosen it
// and hands a single part over, so this settles the tie for a caller that did not.
func firstPart(parts []part) part {
	first := parts[0]
	for _, p := range parts[1:] {
		if p.Envelope.Meta.ProducedAt.Before(first.Envelope.Meta.ProducedAt) {
			first = p
		}
	}
	return first
}

// foldInto folds one item into the item that keeps the identity: what the other carries
// and this one does not is added, and its files are appended.
//
// A file the two items name identically and agree on the bytes of is one file, kept once,
// because the mount a port is written into holds one file per name and two entries for
// the same bytes would say nothing the first did not.
func foldInto(into, with agk.Item) agk.Item {
	data := make(map[string]any, len(into.Data)+len(with.Data))
	for k, v := range with.Data {
		data[k] = v
	}
	for k, v := range into.Data {
		data[k] = v
	}

	files := make([]agk.File, 0, len(into.Files)+len(with.Files))
	files = append(files, into.Files...)
	for _, f := range with.Files {
		if slices.ContainsFunc(files, func(held agk.File) bool { return held.Name == f.Name && held.SHA256 == f.SHA256 }) {
			continue
		}
		files = append(files, f)
	}
	return agk.Item{ID: into.ID, Data: data, Files: files}
}

// rebuild puts the merged items back into an envelope and holds the result to the size
// rules, which are measured on what travels and not on the pieces it was made of.
func rebuild(parts []part, items []agk.Item, l agk.Limits) (agk.Envelope, error) {
	m := parts[0].Envelope.Meta
	for _, p := range parts[1:] {
		if p.Envelope.Meta.Attempt > m.Attempt {
			m.Attempt = p.Envelope.Meta.Attempt
		}
		if p.Envelope.Meta.ProducedAt.After(m.ProducedAt) {
			m.ProducedAt = p.Envelope.Meta.ProducedAt
		}
	}
	m.Count = len(items)
	e := agk.Envelope{Meta: m, Items: items}
	if err := e.Validate(l); err != nil {
		return agk.Envelope{}, err
	}
	return e, nil
}

// mergeOf and joinOn are the two places the merge keywords are read, so that the rules
// above speak about strategies and the file's shape is answered for once.
func mergeOf(st *Step) Merge { return st.Merge }

func joinOn(st *Step) string { return st.Join.On }

// itemsWord writes the unit of a count the way a refusal reads it aloud.
func itemsWord(n int) string {
	if n == 1 {
		return "item"
	}
	return "items"
}

// topology is the part of the resolved graph the abandonment rule reads: what a step
// declares, which of its ports anything takes, who consumes one, and what the workflow
// itself takes. *Graph is what answers it in a run, and naming it here keeps this rule
// readable with nothing built.
type topology interface {
	Step(agk.Step) (*Step, bool)
	published(agk.Step) []agk.Port
	Consumers(agk.Step, agk.Port) []agk.Step
	Workflow() *Workflow
}

// superseded names the steps whose work merge: first left behind: the other edges are
// abandoned and their steps cancelled if no other consumer needs them.
//
// Needing it is read off the graph and off the run together. A step is still needed when
// another consumer of any of its ports has not ended yet, and when a workflow output
// takes one of its ports, since an output is a consumer the steps block does not show. A
// consumer that has already ended needs nothing more, and the step that abandoned the
// edge is not counted as needing what it abandoned.
//
// Only the edges of this merge are looked at. Cancelling a step may in turn leave its own
// upstream with nobody waiting, and that is the caller's to notice on the next evaluation
// rather than a recursion here: the step it cancels has not ended yet when this is read.
func superseded(g topology, consumer agk.Step, abandoned []Edge, steps map[agk.Step]StepState) []agk.Step {
	dropped := make(map[agk.Step]map[agk.Port]bool, len(abandoned))
	var order []agk.Step
	for _, e := range abandoned {
		if _, seen := dropped[e.Step]; !seen {
			dropped[e.Step] = make(map[agk.Port]bool)
			order = append(order, e.Step)
		}
		dropped[e.Step][e.Port] = true
	}

	var out []agk.Step
	for _, name := range order {
		if _, ok := g.Step(name); !ok {
			continue
		}
		if up, known := steps[name]; known && up.Verdict.Terminal() {
			continue
		}
		if !stillNeeded(g, name, consumer, dropped[name], steps) {
			out = append(out, name)
		}
	}
	return out
}

// stillNeeded says whether anything is still waiting on what a step publishes.
//
// The ports it reads are the ports anything takes and not only the ports the step wrote
// down. A sub-workflow call declares none of its own, because "its ports are the declared
// outputs of the workflow it calls, which this commit does not carry", so a rule that read
// outputs alone would find nothing to be waiting on and cancel every call the first
// merge: first in the file abandoned an edge from, with its other consumers still waiting.
func stillNeeded(g topology, name, consumer agk.Step, dropped map[agk.Port]bool, steps map[agk.Step]StepState) bool {
	for _, port := range g.published(name) {
		if feedsWorkflowOutput(g.Workflow(), name, port) {
			return true
		}
		for _, c := range g.Consumers(name, port) {
			if c == consumer && dropped[port] {
				continue
			}
			if down, known := steps[c]; known && down.Verdict.Terminal() {
				continue
			}
			return true
		}
	}
	return false
}

// feedsWorkflowOutput says whether a workflow output takes this port. A workflow output
// is a consumer like any other, and a step feeding one is running for the run itself
// rather than for a step, which no edge in the graph shows.
func feedsWorkflowOutput(wf *Workflow, step agk.Step, port agk.Port) bool {
	if wf == nil {
		return false
	}
	for _, o := range wf.Outputs {
		if o.From.Step == step && o.From.Port == port {
			return true
		}
	}
	return false
}
