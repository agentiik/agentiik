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
// workflow declared for the port that published it or will: what the steps published, and what
// their shards produced.
//
// A shard's envelope is working material that the publication concatenates, so the files of a
// step that published are its publication's, recorded once under the URI they share. A step that
// has not published yet, or never will since its run ended first, keeps its shards' envelopes all
// the same, counted until the run's retention runs out, and a file one of them names is one that
// envelope is read back with: recorded as the publication would record it, the orphan sweep never
// takes it for a file no row names.
//
// And a file living by the workflow's defaults, one of an intermediate port or a shard's, lives
// as long as its run keeps the envelopes naming it, which is what defaults.retain dates them by
// too: it is never collected from under an envelope still kept. An output declaring its own retain
// keeps the instant it declared.
func artifactsOf(g *graph.Graph, s *graph.State) []db.Reference {
	if g == nil || s == nil {
		return nil
	}
	wf := g.Workflow()
	if wf == nil {
		return nil
	}

	var out []db.Reference
	seen := map[agk.URI]bool{}
	add := func(step agk.Step, port agk.Port, e agk.Envelope, shard bool) {
		// A workflow that declared no default and no retention on this output still has its
		// artifacts recorded, with no duration, which keeps them as long as the namespace
		// allows. Left unrecorded, they would be bytes nothing expires, nothing collects and
		// max_artifact_bytes stops counting once their upload lapses.
		retain := retainOf(wf, step, port)
		for _, item := range e.Items {
			for _, f := range item.Files {
				if seen[f.URI] {
					continue
				}
				seen[f.URI] = true
				out = append(out, db.Reference{
					URI:       f.URI,
					Digest:    f.SHA256,
					Size:      f.Size,
					MediaType: f.MediaType,
					For:       time.Duration(retain.For),
					Fetches:   retain.Fetches,
					WithRun:   shard || !declaresRetain(wf, step, port),
				})
			}
		}
	}
	for _, name := range sortedSteps(s) {
		for _, port := range sortedPorts(s.Steps[name].Ports) {
			add(name, port, s.Steps[name].Ports[port], false)
		}
	}
	for _, name := range sortedSteps(s) {
		for _, shard := range s.Steps[name].Shards {
			for _, port := range sortedPorts(shard.Ports) {
				add(name, port, shard.Ports[port], true)
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

// declaresRetain says whether a workflow output published on one port declares a retain of its
// own, which its artifacts keep, rather than living by the workflow's defaults.
func declaresRetain(wf *graph.Workflow, step agk.Step, port agk.Port) bool {
	for _, out := range wf.Outputs {
		if out.From.Step == step && out.From.Port == port && out.Retain.For > 0 {
			return true
		}
	}
	return false
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
