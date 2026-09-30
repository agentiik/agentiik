package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// Calling a workflow from a workflow, as the controller sees it: the run a call started, found by
// the dispatch that made it, and what a caller is woken and a called run cancelled by.

// ErrCalledAlready is a call whose dispatch has started a run already: the pass that made it died
// before writing it down, and the next finds that run rather than starting a second.
var ErrCalledAlready = errors.New("db: that dispatch has called a run already")

// Called is a run a call started, as the step that made the call reads it.
type Called struct {
	Namespace string
	Run       agk.RunID
	Workflow  string
	Commit    string
	State     agk.RunState
	Reason    string
}

// ErrNotCalled is a dispatch that has started no run.
var ErrNotCalled = errors.New("db: that dispatch has called no run")

// CalledBy is the run the dispatch task called, in whichever namespace it is.
func (w *Wide) CalledBy(ctx context.Context, task agk.TaskID) (Called, error) {
	var c Called
	var state string
	err := w.tx.QueryRow(ctx,
		`select namespace, id, workflow, commit, state, coalesce(reason, '') from runs where caller_task = $1`,
		string(task)).Scan(&c.Namespace, &c.Run, &c.Workflow, &c.Commit, &state, &c.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return Called{}, ErrNotCalled
	}
	if err != nil {
		return Called{}, fmt.Errorf("db: the run task %s called could not be read: %w", task, err)
	}
	if err := c.State.UnmarshalText([]byte(state)); err != nil {
		return Called{}, fmt.Errorf("db: run %s is in state %q: %w", c.Run, state, err)
	}
	return c, nil
}

// WakeCaller wakes the run whose call started run, where one did and it has not ended: the run it
// called has ended, and the step waiting on it has its answer. In the transaction that writes the
// ending, so that the caller is told of an ending that is written and of no other.
func (w *Wide) WakeCaller(ctx context.Context, run agk.RunID, at time.Time) error {
	var caller *string
	err := w.tx.QueryRow(ctx,
		`update runs c set wake_at = $2
		 from runs r
		 where r.id = $1 and c.id = r.caller_run and c.state in ('queued', 'running', 'waiting')
		 returning c.id`, string(run), at).Scan(&caller)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("db: the caller of run %s could not be woken: %w", run, err)
	}
	if _, err := w.tx.Exec(ctx, `select pg_notify($1, $2)`, RunChannel, *caller); err != nil {
		return fmt.Errorf("db: the caller of run %s could not be notified: %w", run, err)
	}
	return nil
}

// CancelCalled asks every run the calls of run started, or the one the dispatch task started where
// task is not empty, to be cancelled, as a person cancelling it asks: "propagate cancellation and
// timeout down to child runs". Each is decided by the controller, which reads the request there,
// and is told to. A run that has ended, or was asked already, is left as it is.
func (w *Wide) CancelCalled(ctx context.Context, run agk.RunID, task agk.TaskID, at time.Time) error {
	rows, err := w.tx.Query(ctx,
		`update runs set cancel_requested_at = $3
		 where caller_run = $1 and ($2 = '' or caller_task = $2)
		   and state in ('queued', 'running', 'waiting') and cancel_requested_at is null
		 returning id`, string(run), string(task), at)
	if err != nil {
		return fmt.Errorf("db: the runs run %s called could not be cancelled: %w", run, err)
	}
	called, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("db: the runs run %s called could not be cancelled: %w", run, err)
	}
	for _, id := range called {
		if _, err := w.tx.Exec(ctx, `select pg_notify($1, $2)`, RunChannel, id); err != nil {
			return fmt.Errorf("db: run %s could not be notified: %w", id, err)
		}
	}
	return nil
}
