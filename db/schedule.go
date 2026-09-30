package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// Schedules that fire with nobody present: what the leading controller reads to fire them, and how
// a firing moves a schedule on. "Compute the next occurrence from the schedule state and last
// firing on the trigger row, never from memory": what a schedule is due for next is on its row, so a
// controller that takes over carries on from where the last one wrote, and a firing and the move of
// its row are one transaction, fenced, so that a controller that lost the lead neither starts a run
// nor moves a schedule on.

// Due is a schedule whose occurrence is due to fire.
type Due struct {
	Namespace, Workflow string
	Position            int
	Commit              string
	Declared            json.RawMessage

	// DueAt is the occurrence it is due for, and FireAt when it fires, its jitter drawn.
	DueAt, FireAt time.Time
}

// DueSchedules are the schedules, across every namespace, whose occurrence is due to fire at now,
// soonest first, at most batch of them.
func (w *Wide) DueSchedules(ctx context.Context, now time.Time, batch int) ([]Due, error) {
	batch, err := batchOf(batch)
	if err != nil {
		return nil, err
	}
	rows, err := w.tx.Query(ctx,
		`select namespace, workflow, position, commit, declared, due_at, fire_at from triggers
		 where kind = 'schedule' and fire_at <= $1
		 order by fire_at, namespace, workflow, position limit $2`, now, batch)
	if err != nil {
		return nil, fmt.Errorf("db: the schedules due could not be read: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Due, error) {
		var d Due
		err := row.Scan(&d.Namespace, &d.Workflow, &d.Position, &d.Commit, &d.Declared, &d.DueAt, &d.FireAt)
		if err == nil {
			d.Declared, err = canonical(d.Declared)
		}
		return d, err
	})
}

// ErrScheduleMoved is a schedule that is no longer where its firing found it: another firing moved
// it on, or a push armed it anew. The firing does nothing, and the next pass reads the row again.
var ErrScheduleMoved = errors.New("db: the schedule moved on since it was read")

// Firing is what became of one occurrence of a schedule, and when it comes round next.
type Firing struct {
	// For is the occurrence, the instant trigger.scheduled_for reads, and At when it fired.
	For, At time.Time
	// Run is the run it started, or Skipped says why it started none.
	Run     agk.RunID
	Skipped string

	// Next is the occurrence the schedule is due for from now on, and FireAt when it fires.
	Next, FireAt time.Time
}

// Held reads a schedule's row under a lock, where it is still due for the occurrence d names, and
// ErrScheduleMoved where it is not: the first thing a firing does in its transaction, so that two
// passes firing one occurrence take turns and the second finds it moved on.
func (n *NS) Held(ctx context.Context, d Due) error {
	var due time.Time
	err := n.tx.QueryRow(ctx,
		`select due_at from triggers
		 where namespace = $1 and workflow = $2 and kind = 'schedule' and position = $3 and commit = $4
		 for update`, n.namespace, d.Workflow, d.Position, d.Commit).Scan(&due)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrScheduleMoved
	}
	if err != nil {
		return fmt.Errorf("db: the schedule %d of %s could not be read: %w", d.Position, d.Workflow, err)
	}
	if !due.Equal(d.DueAt) {
		return ErrScheduleMoved
	}
	return nil
}

// Fired records what became of the occurrence d was due for and moves the schedule on to the next,
// in the transaction Held locked its row in.
func (n *NS) Fired(ctx context.Context, d Due, f Firing) error {
	tag, err := n.tx.Exec(ctx,
		`update triggers set fired_at = $6, fired_for = $7, fired_run = nullif($8, ''), skipped = nullif($9, ''),
		   due_at = $10, fire_at = $11
		 where namespace = $1 and workflow = $2 and kind = 'schedule' and position = $3 and commit = $4 and due_at = $5`,
		n.namespace, d.Workflow, d.Position, d.Commit, d.DueAt, f.At, f.For, string(f.Run), f.Skipped, f.Next, f.FireAt)
	if err != nil {
		return fmt.Errorf("db: the schedule %d of %s could not be moved on: %w", d.Position, d.Workflow, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrScheduleMoved
	}
	return nil
}
