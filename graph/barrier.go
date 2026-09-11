package graph

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// The barrier: a step becomes runnable once every declared input port is satisfied. It
// is a barrier and not a continuous stream, so nothing starts on a partial arrival, and
// a port carries exactly one envelope, published once, when the emitting step ends.
//
// What the barrier answers is one of three things, and the evaluator does the rest:
//
//	gateWait   an edge has not resolved yet, and nothing about this step is decided
//	gateSkip   when refuses the upstream states, so the step is skipped, and what a
//	           skipped step publishes is publish with no shards: an empty envelope on
//	           every port it declares
//	gateStart  every port is satisfied, and the arrival says what each one carries
//
// A step whose if condition is false is skipped in the same way, but the condition is an
// expression and evaluating one is not the barrier's job: the evaluator asks the barrier
// first, evaluates if against the ports the arrival carries, which is what lets
// ${{ inputs.in.count > 0 }} be written at all, and publishes skipped on a false one.
//
// Two readings the documentation does not state are taken here.
//
// A when that does not admit an upstream state moves the step to skipped rather than to
// some fourth thing. The documentation says what a false if does and says nothing about
// this one, and the two are the same event: the step does not run, its ports still have
// to carry something, and the steps below still have to be able to decide their own fate
// through their own when. Anything else leaves a graph hanging on a port nobody will
// ever publish.
//
// An upstream that ended without publishing a port arrives as an empty envelope all the
// same. A step publishes when it ends whatever its verdict is, so in a run the port is
// already there and this is the same answer reached from the other side; it is stated
// here because the barrier must not depend on it. Otherwise when: [failed] on a cleanup
// step would name a state whose ports might never arrive, the barrier would never lift
// and the keyword would mean nothing.

// gate is what the barrier decides about one step at one moment.
type gate int

const (
	gateWait gate = iota
	gateSkip
	gateStart
)

// arrival is what a step is given once its barrier lifts: one envelope per declared
// input port, what a key join could not match, and the edges merge: first left behind.
type arrival struct {
	Inputs    map[agk.Port]agk.Envelope
	Unmatched []agk.Item
	Abandoned []Edge
}

// edgeState is what one edge has become at the moment the barrier is read.
type edgeState int

const (
	edgeWaiting  edgeState = iota // the emitting step has not ended
	edgeProduced                  // the port was published and is here
	edgeSilent                    // the step ended without publishing the port
)

// barrier reads one step against the run as it stands and says whether it may start.
//
// The reason it returns is the one a skipped step records, in the documentation's own
// words, so that a run detail says which upstream state kept a step from running rather
// than only that it did not run.
func barrier(name agk.Step, st *Step, edges []Edge, steps map[agk.Step]StepState, fed map[agk.Port]agk.Envelope, run agk.RunID, l agk.Limits, at time.Time) (gate, *arrival, string, error) {
	ports, byPort := inboundPorts(edges, fed)

	// Every port first, because this is a barrier: nothing about the step is decided
	// while an edge has not resolved, and a port that has arrived decides nothing on
	// its own.
	arrived := make(map[agk.Port][]part, len(ports))
	out := &arrival{Inputs: make(map[agk.Port]agk.Envelope, len(ports))}
	for _, port := range ports {
		parts, abandoned, ready := satisfyPort(st, port, byPort[port], steps, fed, run, at)
		if !ready {
			return gateWait, nil, "", nil
		}
		arrived[port] = parts
		out.Abandoned = append(out.Abandoned, abandoned...)
	}

	// when is read against the upstream states that actually reached this step, and
	// it is read before anything is merged: a step that is not going to run should not
	// be refused for a zip it was never going to be given. An edge merge: first
	// abandoned is work the engine itself called off, and judging a step on a state
	// the engine caused would skip it for the engine's own decision.
	for _, port := range ports {
		for _, p := range arrived[port] {
			up, known := steps[p.Edge.Step]
			if p.Edge.Step == "" || !known {
				continue
			}
			if !admits(whenOf(st), up.Verdict) {
				return gateSkip, nil, fmt.Sprintf("when %s does not admit %s, which %s", whenList(whenOf(st)), p.Edge.Step, up.Verdict), nil
			}
		}
	}

	for _, port := range ports {
		e, unmatched, err := mergePort(name, port, st, arrived[port], l)
		if err != nil {
			return gateWait, nil, "", err
		}
		out.Inputs[port] = e
		out.Unmatched = append(out.Unmatched, unmatched...)
	}
	return gateStart, out, "", nil
}

// inboundPorts lists the input ports a step declares, in the order the file declares
// them, with the edges arriving on each. A port fed by the step's own inputs keyword is
// declared too and depends on no step, so it sits after the edges that share its name.
func inboundPorts(edges []Edge, fed map[agk.Port]agk.Envelope) ([]agk.Port, map[agk.Port][]Edge) {
	byPort := make(map[agk.Port][]Edge)
	var ports []agk.Port
	for _, e := range edges {
		if _, seen := byPort[e.As]; !seen {
			ports = append(ports, e.As)
		}
		byPort[e.As] = append(byPort[e.As], e)
	}
	for _, port := range slices.Sorted(maps.Keys(fed)) {
		if _, seen := byPort[port]; !seen {
			ports = append(ports, port)
			byPort[port] = nil
		}
	}
	return ports, byPort
}

// satisfyPort says whether one port is satisfied and what arrives on it.
//
// Every edge has to have resolved, which is what makes this a barrier, with the single
// exception the language states: merge: first lifts the barrier as soon as one upstream
// port has produced, and the edges that did not are abandoned.
func satisfyPort(st *Step, port agk.Port, edges []Edge, steps map[agk.Step]StepState, fed map[agk.Port]agk.Envelope, run agk.RunID, at time.Time) ([]part, []Edge, bool) {
	states := make([]edgeState, len(edges))
	held := make([]agk.Envelope, len(edges))
	waiting := false
	for i, e := range edges {
		states[i], held[i] = resolveEdge(e, steps, run, at)
		if states[i] == edgeWaiting {
			waiting = true
		}
	}

	var parts []part
	var abandoned []Edge
	switch {
	case len(edges) == 0:
		// A port the step's own inputs keyword feeds depends on no step, so it is
		// satisfied the moment the run has the value.

	case mergeOf(st) == MergeFirst:
		winner := -1
		for i := range edges {
			if states[i] != edgeProduced {
				continue
			}
			if winner < 0 || held[i].Meta.ProducedAt.Before(held[winner].Meta.ProducedAt) {
				winner = i
			}
		}
		if winner < 0 {
			if waiting {
				return nil, nil, false
			}
			// Every edge ended without publishing. There is nothing to lift the
			// barrier with and nothing to abandon, so the port carries a batch of
			// nothing and when decides what that means.
			winner = 0
		}
		parts = append(parts, part{Edge: edges[winner], Envelope: held[winner]})
		for i, e := range edges {
			if i != winner {
				abandoned = append(abandoned, e)
			}
		}

	default:
		if waiting {
			return nil, nil, false
		}
		for i, e := range edges {
			parts = append(parts, part{Edge: e, Envelope: held[i]})
		}
	}

	if e, ok := fed[port]; ok {
		parts = append(parts, part{Edge: Edge{As: port}, Envelope: e})
	}
	if len(parts) == 0 {
		return nil, nil, false
	}
	return parts, abandoned, true
}

// resolveEdge reads one edge against the run: whether the step it comes from has ended, and
// what it published on the port the edge names.
//
// A step that ended without publishing the port carries a batch of nothing, stamped at
// the moment that step ended rather than at the moment this is read. The state is what
// the moment is taken from so that the same run replays to the same envelope, and so
// that a cache key built over an input envelope does not move every time the evaluator
// is asked.
func resolveEdge(e Edge, steps map[agk.Step]StepState, run agk.RunID, at time.Time) (edgeState, agk.Envelope) {
	up, known := steps[e.Step]
	if !known || !up.Verdict.Terminal() {
		return edgeWaiting, agk.Envelope{}
	}
	if held, ok := up.Ports[e.Port]; ok {
		return edgeProduced, held
	}
	since := up.Since
	if since.IsZero() {
		since = at
	}
	return edgeSilent, agk.Empty(run, e.Step, e.Port, 1, since)
}

// admits says whether an upstream verdict is one of the states when lets a step start on.
//
// when names succeeded, failed, skipped and always, and cancelled is not among them: a
// step that was cancelled is work a principal, a concurrency group or a merge: first
// called off, and only always speaks for it. A when that is empty is [succeeded], which
// is the default the language states.
func admits(when []When, v agk.Verdict) bool {
	if len(when) == 0 {
		when = []When{WhenSucceeded}
	}
	for _, w := range when {
		switch {
		case w == WhenAlways:
			return true
		case w == WhenSucceeded && v == agk.VerdictSucceeded:
			return true
		case w == WhenFailed && v == agk.VerdictFailed:
			return true
		case w == WhenSkipped && v == agk.VerdictSkipped:
			return true
		}
	}
	return false
}

// whenOf and whenList are where the keyword is read and where it is written back into a
// refusal, so that the reason a step was skipped quotes the list the author wrote.
func whenOf(st *Step) []When {
	if len(st.When) == 0 {
		return []When{WhenSucceeded}
	}
	return st.When
}

func whenList(when []When) string {
	names := make([]string, 0, len(when))
	for _, w := range when {
		names = append(names, w.String())
	}
	return "[" + strings.Join(names, ", ") + "]"
}

// truth reads the answer an if condition gave.
//
// The condition decides whether the step runs at all, so it answers true or false and
// nothing else. Reading a number, a string or a batch as a truth value would invent a
// rule the language does not state, and it is also what the documented condition asks
// for: ${{ inputs.in.count > 0 }} is a comparison, written so that the test is in the
// file rather than in whatever reads it.
func truth(name agk.Step, source string, v any) (bool, error) {
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("graph: step %s: if: %s: a run condition is true or false, and this one answered %T", name, source, v)
	}
	return b, nil
}
