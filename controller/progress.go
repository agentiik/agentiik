package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

// Progress is a runner saying a task it holds has moved on without ending: running once its
// container has started, publishing once the container has exited and its outputs are being
// collected.
//
// It names the dispatch as an answer does, by its key and its row, and comes from the runner that
// published it, which the bus vouches for as it does for an answer.
type Progress struct {
	Task  agk.TaskID
	Row   string
	State agk.TaskState

	// Runner is the runner that published it, and who it is taken from: the runner the
	// dispatch was bound to at its redemption, and no other.
	Runner string
}

// Progress writes one task's progress on the row of the dispatch it names.
//
// "running: The container is running." Only the runner holding a task sees it start, and until it
// says so the run detail shows the task dispatched until it ends. So the runner says so, on its
// results subject, and this writes it where the run detail reads it, and nowhere else: the
// evaluator decides nothing by it, so the run is not decided again and its document is not
// touched.
//
// Its rules are an answer's, as far as they go. It is taken from the runner the dispatch was bound
// to and from no other, refused with ErrNotTheHolder otherwise, and wrapped with ErrNotAResult,
// since no delivery would change that. And it is never news after an ending: db.Wide.Progress moves
// a row only forwards and never over an ending, so a copy delivered late, twice, or after the
// result it preceded changes nothing and is not an error, which is what the bus delivering at
// least once needs. A binding made by an ending that never reached a container is over already,
// so it moves nothing either.
func (co *Core) Progress(ctx context.Context, p Progress) error {
	run, _, _, _, err := agk.ParseTaskID(string(p.Task))
	if err != nil {
		return fmt.Errorf("%w: it names no task: %w", ErrNotAResult, err)
	}
	switch {
	case p.State != agk.TaskRunning && p.State != agk.TaskPublishing:
		return fmt.Errorf("%w: %s is %s, and progress is running or publishing, the ending being a result's to report", ErrNotAResult, p.Task, p.State)
	case p.Row == "":
		return fmt.Errorf("%w: the progress of %s names no dispatch, and a requeue keeps the key, so the key alone cannot say which dispatch moved", ErrNotAResult, p.Task)
	case p.Runner == "":
		return fmt.Errorf("%w: the progress of %s names no runner, and it is taken from the runner its task is bound to and from no other", ErrNotAResult, p.Task)
	}
	err = co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		namespace, _, err := w.Locate(ctx, run)
		if err != nil {
			return err
		}
		_, err = w.Progress(ctx, namespace, p.Task, p.Row, p.Runner, p.State)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNotHeld):
		return fmt.Errorf("%w: %w: %s reported %s for dispatch %s of %s: %w", ErrNotAResult, ErrNotTheHolder, p.Runner, p.State, p.Row, p.Task, err)
	case errors.Is(err, db.ErrNoRun):
		// Rows are written before a message leaves, so progress naming no run is not early.
		return fmt.Errorf("%w: %w", ErrNotAResult, err)
	}
	return err
}
