package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
)

// Replay from a step: "Reuses the envelopes and artifacts already produced upstream".
//
// A replay is created as a run of its own, of the commit the run it replays pinned, over the same
// inputs, naming that run and the step it replays from. Nothing here writes it: what the controller
// adds is the state it starts in, the steps above that step in the verdicts and with the envelopes
// the run it replays left them, read out of that run's document on the pass that lets it in, and
// never handed out. A replay from the start reuses nothing and starts as any run does.

// reused reads the steps a replay from a step reuses, each as graph.Reused makes it, or says why
// there is nothing to reuse: the run it replays is gone, or never decided anything, or its
// envelopes have gone with its retention. That is no failure of the installation's but a replay
// that cannot be made, and the run is refused with the reason rather than retried.
func (co *Core) reused(ctx context.Context, e db.Evaluation, g *graph.Graph) (map[agk.Step]graph.StepState, string, error) {
	var of db.Evaluation
	err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		of, err = w.Run(ctx, e.ReplayOf)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoRun):
		return nil, fmt.Sprintf("the run it replays, %s, is gone, and with it what the steps above %s produced", e.ReplayOf, e.ReplayFrom), nil
	case err != nil:
		return nil, "", err
	case of.Namespace != e.Namespace || of.Workflow != e.Workflow || of.Commit != e.Commit:
		// Written together by the route that creates a replay, and never apart.
		return nil, fmt.Sprintf("the run it replays, %s, is not a run of %s/%s@%s", e.ReplayOf, e.Namespace, e.Workflow, e.Commit), nil
	case len(of.Document) == 0:
		return nil, fmt.Sprintf("the run it replays, %s, never started, so no step above %s produced anything", e.ReplayOf, e.ReplayFrom), nil
	}
	var doc Document
	if err := asWritten(of.Document, &doc); err != nil {
		return nil, "", fmt.Errorf("controller: the document of run %s could not be read: %w", of.Run, err)
	}
	state, err := Rehydrate(ctx, doc, of.Namespace, co.objects, co.limits)
	if err != nil {
		return nil, fmt.Sprintf("what the steps above %s produced in the run it replays, %s, could not be read back, and a replay from a step reuses it: %v; a replay from the start reuses nothing", e.ReplayFrom, e.ReplayOf, err), nil
	}
	steps := map[agk.Step]graph.StepState{}
	for _, name := range g.Upstream(e.ReplayFrom) {
		ss, ok := state.Steps[name]
		if !ok || !graph.Reusable(ss.Verdict, state.Run.State) {
			return nil, fmt.Sprintf("step %s, above %s, did not finish in the run it replays, %s", name, e.ReplayFrom, e.ReplayOf), nil
		}
		steps[name] = graph.Reused(ss, e.Run)
	}
	return steps, "", nil
}
