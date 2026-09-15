package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/ulid"
)

// The events the API turns into push messages.
//
// "Emits the notification events that the API turns into push messages: failure, approval
// requested, completion of a run the recipient started." The controller writes them and the API
// reads them; the two "share the database and nothing else".
//
// An event carries an identifier and a state, which is the rule the push message itself is held
// to and the reason it is held to it: "A push message carries an identifier and a state, never a
// payload, a log line or a workflow name from a namespace the device may have lost access to.
// The application fetches the detail over the API after the notification is tapped, so that a
// revoked grant takes effect between the alert and what is shown."

// Kind is what happened.
type Kind string

const (
	// Failure is a run that did not work. Not a run somebody cancelled: that is a
	// completion, because somebody meant it.
	Failure Kind = "failure"

	// ApprovalRequested is a run suspended on a human decision. Nothing emits it yet,
	// because nothing in the language can suspend a run; the kind exists so that the group
	// that teaches it to does not also have to widen a check constraint.
	ApprovalRequested Kind = "approval_requested"

	// Completion is a run that ended, however it ended. The page scopes it to "a run the
	// recipient started", which the API resolves from StartedBy: who wants it is a
	// preference and a grant, and neither is the controller's.
	Completion Kind = "completion"
)

// Event is one thing worth waking somebody for.
type Event struct {
	Namespace string
	ID        string
	Kind      Kind

	Run   agk.RunID
	State agk.RunState

	// StartedBy is the principal the run was attributed to, and is empty where a schedule
	// or an event started it: "Runs started by a schedule, a webhook or an event are
	// attributed to the namespace service identity, not to the person who last edited the
	// workflow."
	StartedBy string

	CreatedAt time.Time
}

// emit records the events a decision produced.
//
// It is called from inside SaveDecision and from nowhere else, which is what makes emitting
// exactly once a property of the design rather than a discipline: a run reaches a terminal state
// in one committed decision, and the decision is what carries the event. The unique index is a
// belt on top of that.
func (w *Wide) emit(ctx context.Context, namespace string, run agk.RunID, was, now agk.RunState, startedBy string) error {
	if !now.Terminal() || was.Terminal() {
		return nil
	}

	kinds := []Kind{Completion}
	// A run that failed and a run that ran out of time are both worth telling somebody
	// about, and a run somebody cancelled is not: they know. Both are completions as well,
	// because "completion of a run the recipient started" is about who asked rather than
	// about how it went.
	if now == agk.Failed || now == agk.TimedOut {
		kinds = append([]Kind{Failure}, kinds...)
	}

	for _, kind := range kinds {
		if _, err := w.tx.Exec(ctx,
			`insert into notification_events (namespace, id, kind, run_id, run_state, started_by)
			 values ($1, $2, $3, $4, $5, $6)
			 on conflict (namespace, run_id, kind) do nothing`,
			namespace, ulid.New(), string(kind), string(run), now.String(), nilIfEmpty(startedBy)); err != nil {
			return fmt.Errorf("db: the %s of run %s could not be recorded: %w", kind, run, err)
		}
	}
	return nil
}

// Undelivered names the events nobody has been told about.
//
// In the order they happened, because a person told that a run completed before being told it
// failed has been told two true things in the order that makes the second one confusing.
func (w *Wide) Undelivered(ctx context.Context, batch int) ([]Event, error) {
	batch, err := batchOf(batch)
	if err != nil {
		return nil, err
	}
	rows, err := w.tx.Query(ctx, `
		select namespace, id, kind, run_id, run_state, started_by, created_at
		from notification_events
		where delivered_at is null
		order by created_at, id
		limit $1`, batch)
	if err != nil {
		return nil, fmt.Errorf("db: the undelivered events could not be read: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var kind, state string
		var by *string
		if err := rows.Scan(&e.Namespace, &e.ID, &kind, &e.Run, &state, &by, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: the undelivered events could not be read: %w", err)
		}
		e.Kind = Kind(kind)
		if err := e.State.UnmarshalText([]byte(state)); err != nil {
			return nil, fmt.Errorf("db: event %s says the run is %q: %w", e.ID, state, err)
		}
		if by != nil {
			e.StartedBy = *by
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Delivered records that those events have been handed to whoever wanted them.
//
// Stamped after the push went out and never before, for the same reason a task's message is
// stamped after the bus took it: a push that never went and is marked as gone is a failure
// nobody hears about.
func (w *Wide) Delivered(ctx context.Context, events []Event, at time.Time) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	namespaces := make([]string, len(events))
	ids := make([]string, len(events))
	for i, e := range events {
		if e.Namespace == "" || e.ID == "" {
			return 0, errors.New("db: an event with no namespace or no identifier")
		}
		namespaces[i], ids[i] = e.Namespace, e.ID
	}
	tag, err := w.tx.Exec(ctx, `
		update notification_events e set delivered_at = $3
		from unnest($1::text[], $2::text[]) as g(namespace, id)
		where e.namespace = g.namespace and e.id = g.id and e.delivered_at is null`,
		namespaces, ids, at)
	if err != nil {
		return 0, fmt.Errorf("db: the delivered events could not be recorded: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
