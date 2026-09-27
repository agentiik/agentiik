package controller

import (
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
)

// Recording the artifacts a run produced.
//
// The bytes are already in the store: a runner spills a value past inline_max_bytes and uploads
// it through a presigned URL it redeemed its grant for. What is missing is the row that says a
// run, a step and a port point at those bytes, because "Expiry applies to the reference, never
// to the object" and until the reference exists nothing expires and nothing is counted.
//
// The controller is what records it, and it has to be, because the reference carries how long it
// lives and how long it lives is declared in the workflow. A runner does not have the graph and
// the API does not read it; the controller has both.

// artifactsOf is every file the envelopes of this pass reference, with the retention the
// workflow declared for the port that published it.
//
// Only what a step published, and not what a shard produced. A shard's envelope is working
// material that the publication concatenates, so its files are the publication's files and
// counting them twice would keep an artifact alive for twice as long as it was declared to be.
func artifactsOf(g *graph.Graph, s *graph.State) []db.Reference {
	if g == nil || s == nil {
		return nil
	}
	wf := g.Workflow()
	if wf == nil {
		return nil
	}

	var out []db.Reference
	for _, name := range sortedSteps(s) {
		st := s.Steps[name]
		for _, port := range sortedPorts(st.Ports) {
			// A workflow that declared no default and no retention on this output
			// still has its artifacts recorded, with no duration, which keeps them
			// as long as the namespace allows. Left unrecorded, they would be bytes
			// nothing expires, nothing collects and max_artifact_bytes stops counting
			// once their upload lapses.
			retain := retainOf(wf, name, port)
			for _, item := range st.Ports[port].Items {
				for _, f := range item.Files {
					out = append(out, db.Reference{
						URI:       f.URI,
						Digest:    f.SHA256,
						Size:      f.Size,
						MediaType: f.MediaType,
						For:       time.Duration(retain.For),
						Fetches:   retain.Fetches,
					})
				}
			}
		}
	}
	return out
}

// retainOf is how long an artifact published on one port lives.
//
// "retain is written on a workflow output or in defaults, never on a step. Everything travelling
// between two steps is an intermediate and lives by the workflow's default, so keeping one heavy
// intermediate longer than the rest is a change to defaults rather than something a step can ask
// for."
func retainOf(wf *graph.Workflow, step agk.Step, port agk.Port) graph.Retain {
	for _, out := range wf.Outputs {
		if out.From.Step == step && out.From.Port == port && out.Retain.For > 0 {
			return out.Retain
		}
	}
	if wf.Defaults.Retain != nil {
		return *wf.Defaults.Retain
	}
	return graph.Retain{}
}

// runRetain is how long a run's envelopes and logs are kept once it has finished: "Envelopes and
// logs are not declared one at a time the way an output is, so they live by the workflow's
// defaults.retain". Zero where the workflow declares none, which the database resolves, as it caps
// every retention, to the namespace's max_retention_days.
func runRetain(g *graph.Graph) time.Duration {
	if g == nil || g.Workflow() == nil || g.Workflow().Defaults.Retain == nil {
		return 0
	}
	return time.Duration(g.Workflow().Defaults.Retain.For)
}
