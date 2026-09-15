package controller

import (
	"context"
	"fmt"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
)

// What queued waits on.
//
// "queued: Created, waiting on a concurrency lock or on namespace quota." Two things, and the
// controller is what holds both: "Holds concurrency-group locks, per-step parallelism counters
// and per-namespace quotas." The middle one is the evaluator's, since max_parallel is read per
// step and across attempts; the other two are here.

// admitted says whether this run may start, and cancels the run in its way where the workflow
// asked for that.
//
// A run that has already started is admitted by definition: a concurrency group decides who
// begins, not who continues. Stopping a run in the middle because a trigger fired is what
// cancel_in_progress asks for explicitly, and doing it without being asked would make a group a
// way to lose work rather than a way to order it.
func (co *Core) admitted(ctx context.Context, e db.Evaluation, g *graph.Graph) (bool, error) {
	if len(e.Document) > 0 {
		return true, nil
	}
	group := ""
	if wf := g.Workflow(); wf != nil {
		group = wf.Concurrency.Group
	}

	var admit bool
	var cancel agk.RunID
	err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		// Stamped before it is read, so that a run queueing behind this one sees it in
		// the group rather than outside it. The stamp is idempotent and costs one update
		// on the pass that admits.
		if err := w.SetGroup(ctx, e.Namespace, e.Run, group); err != nil {
			return err
		}
		if group == "" {
			admit = true
			return nil
		}

		holder, err := w.Holding(ctx, e.Namespace, group)
		if err != nil {
			return err
		}
		if holder == "" {
			// The group is free, and it goes to whoever queued first.
			admit, err = w.NextInLine(ctx, e.Namespace, group, e.Run)
			return err
		}
		if holder == e.Run {
			admit = true
			return nil
		}
		if wf := g.Workflow(); wf != nil && wf.Concurrency.CancelInProgress {
			cancel = holder
		}
		return nil
	})
	if err != nil {
		return false, err
	}

	if cancel != "" {
		// Cancelled in its own pass, and the caller comes back for this run on the next
		// one. Admitting here would be two runs holding one group for as long as the
		// cancellation took to reach the runners, which is the thing the group exists to
		// prevent.
		if err := co.Cancel(ctx, cancel); err != nil {
			return false, fmt.Errorf("controller: run %s could not be cancelled to make way for %s: %w", cancel, e.Run, err)
		}
		return false, nil
	}
	return admit, nil
}

// withinTheQuota is what this namespace may still hand out, and the tasks that fit.
//
// A task held back is not refused: it stays pending in the evaluator's state, which is what
// makes the next pass hand it out again, and the run stays actionable because its message never
// went. So a namespace at its ceiling slows down rather than failing, which is what a quota is
// for: "a fan-out of ten thousand items cannot starve everyone else".
func (co *Core) withinTheQuota(ctx context.Context, namespace string, start []graph.Task) ([]graph.Task, error) {
	if len(start) == 0 {
		return start, nil
	}
	var free int
	if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		free, err = w.Slots(ctx, namespace)
		return err
	}); err != nil {
		return nil, err
	}
	if free >= len(start) {
		return start, nil
	}
	return start[:free], nil
}
