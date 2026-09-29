package graph

import (
	"fmt"
	"maps"
	"slices"

	"github.com/agentiik/agentiik/agk"
)

// Replay from a step: "Reuses the envelopes and artifacts already produced upstream; content
// addressing guarantees nothing is rewritten."
//
// A replay is a new run of the commit the run it replays pinned, over the same inputs. What it
// reuses is what the named step reads, directly or through the steps above it: those steps are
// given the verdict and the envelopes the run they replay left them, and are never handed out
// again. Every other step runs, the named one and everything below it, and so does a step beside
// it that it does not read from, since nothing it read is known to be the same.

// Upstream answers the steps a step reads from, through its edges and theirs, in an order no edge
// points backwards through: what a replay from it reuses.
func (g *Graph) Upstream(name agk.Step) []agk.Step {
	above := map[agk.Step]bool{}
	var walk func(agk.Step)
	walk = func(step agk.Step) {
		for _, e := range g.Edges(step) {
			if !above[e.Step] {
				above[e.Step] = true
				walk(e.Step)
			}
		}
	}
	walk(name)
	return slices.DeleteFunc(g.Order(), func(s agk.Step) bool { return !above[s] })
}

// Reused is the state a replay starts a step it reuses in: the verdict, the published envelopes
// and when, of the run it replays, stamped as the new run's own, since a step's inputs are
// concatenated from what its edges carry and one batch names one run. It carries no shard: no task
// of the new run did the work, and none is shown as having done it.
func Reused(from StepState, run agk.RunID) StepState {
	out := StepState{Verdict: from.Verdict, Since: from.Since, Reason: from.Reason}
	if len(from.Ports) > 0 {
		out.Ports = make(map[agk.Port]agk.Envelope, len(from.Ports))
		for port, e := range from.Ports {
			e.Meta.RunID = run
			out.Ports[port] = e
		}
	}
	return out
}

// reuse puts the steps a replay reuses in the state it starts from, refusing a step the graph does
// not have and one that is not over: a step reused before its run ended it would never end in this
// one, since nothing is handed out for it.
func reuse(s *State, g *Graph, steps map[agk.Step]StepState) error {
	for _, name := range slices.Sorted(maps.Keys(steps)) {
		ss := steps[name]
		if _, ok := g.Step(name); !ok {
			return fmt.Errorf("graph: the replay reuses step %s, which the graph does not have", name)
		}
		if !ss.Verdict.Terminal() {
			return fmt.Errorf("graph: the replay reuses step %s, which is %s and not over", name, ss.Verdict)
		}
		s.Steps[name] = ss
	}
	return nil
}
