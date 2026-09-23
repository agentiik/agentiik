package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
)

// Calling a run off.
//
// "cancelled: Cancelled by a principal holding workflow:run, by a concurrency group or by a
// merge: first." The third of those is the evaluator's own and arrives through an ordinary
// pass; the first two come from outside and arrive here. A principal's arrives through the
// database, since the API and the controller share it and nothing else: the API writes the
// request on the run, and Decide, reading it there, calls this.

// Cancel ends a run and stops what it is holding.
//
// It has a path of its own rather than going through Decide, and the reason is worth writing
// down: cancelling makes the run terminal, and Decide leaves a terminal run alone because a
// finished run has nothing to decide. The one pass that must still happen is the pass that
// names what to stop, which is why the evaluator's own comment says "the tasks in flight are
// named by the next Plan" rather than by Cancel itself.
//
// Cancelling a run that has already ended does nothing, and says so by answering no error: a
// principal asking twice, or asking about a run that finished while they were asking, has got
// what they wanted either way.
func (co *Core) Cancel(ctx context.Context, run agk.RunID) error {
	var e db.Evaluation
	if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		e, err = w.Run(ctx, run)
		return err
	}); err != nil {
		return err
	}
	if e.State.Terminal() {
		return nil
	}

	g, err := co.versions.Graph(ctx, e.Namespace, e.Workflow, e.Commit)
	if err != nil {
		return fmt.Errorf("controller: the graph of run %s could not be resolved: %w", run, err)
	}
	now := co.now().UTC()
	ev, err := co.resume(ctx, e, g, now)
	if err != nil {
		return err
	}

	ev.Cancel(now)
	plan, err := ev.Next(now)
	if err != nil {
		return fmt.Errorf("controller: run %s could not be evaluated after being cancelled: %w", run, err)
	}

	state := ev.State()
	doc, err := Elide(ctx, state, e.Namespace, co.objects)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("controller: the document of run %s could not be written: %w", run, err)
	}
	steps, tasks := project(state)
	var held []agk.TaskID
	if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		if err := w.SaveDecision(ctx, db.Decision{
			Namespace: e.Namespace, Run: run,
			Was: e.Seq, Seq: state.Seq,
			Document:   encoded,
			State:      state.Run.State,
			StartedAt:  state.Run.StartedAt,
			FinishedAt: state.Run.FinishedAt,
			Steps:      steps, Tasks: tasks,
			Envelopes: referencesOf(doc),
			Artifacts: artifactsOf(g, state),
		}); err != nil {
			return err
		}
		// "Cancels pending tasks and sends SIGTERM to running containers." The stops below
		// are the second half. The first is written here, in the same transaction, because
		// the evaluator ends the run and leaves its tasks as they were, and a task whose
		// message is still on the queue is held by no runner a stop can reach: written as
		// cancelled, its grant no longer redeems, so no container starts for it. The rows then
		// say more than the document does, which nothing reads again once the run has ended.
		var err error
		held, err = w.CancelTasks(ctx, e.Namespace, run, now)
		return err
	}); err != nil {
		return err
	}

	// And they say more about what a runner holds. A pass that published a task and died
	// before recording the dispatch left it pending in the document, where the evaluator
	// stops nothing, and a runner may have taken the message and started the container since:
	// its redemption bound the row, so the row is what names it.
	for _, key := range held {
		if !slices.ContainsFunc(plan.Stop, func(s graph.Stop) bool { return s.Task == key }) {
			plan.Stop = append(plan.Stop, graph.Stop{Task: key, Reason: graph.StopCancelled})
		}
	}

	// The stops go after the commit, like everything else that leaves this process. A stop
	// that never arrives costs a container that runs to its deadline and is then stopped
	// anyway, which is why this is reported rather than retried.
	co.hand(ctx, e.Namespace, run, plan)
	return nil
}
