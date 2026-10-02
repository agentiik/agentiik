package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// Events published to a namespace: "CloudEvents 1.0 published to POST /api/v1/{ns}/events", matched
// in the request against the event triggers armed in the namespace published into, and against
// those of another namespace naming it "where it granted the workflow's namespace read access".

// Listening is an event trigger armed to hear a namespace: where it is armed, the version that
// armed it and what it declares.
type Listening struct {
	Namespace, Workflow string
	Position            int
	Commit              string
	Declared            json.RawMessage
}

// Listening are the event triggers, across every namespace, that hear the namespace published into
// and whose type and source, each where it names one, are the event's: in the namespace published
// into first, then by namespace, workflow and position. Whether one armed in another namespace may
// hear it is the authorizer's to say, and its filter the graph's.
//
// A trigger hears a namespace by any name it answers to, its own or one it held before a rename,
// since a workflow of another namespace keeps the name it was written with until somebody changes
// it, and "a workflow naming the old name keeps working". The rename writes the triggers armed then
// under the new name; one armed since from a file naming the old one names it still.
func (w *Wide) Listening(ctx context.Context, published, eventType, source string) ([]Listening, error) {
	rows, err := w.tx.Query(ctx,
		`select namespace, workflow, position, commit, declared from triggers
		 where kind = 'event'
		   and (hears = $1 or hears = any (select unnest(n.former_names) from namespaces n where n.name = $1))
		   and (type is null or type = $2) and (source is null or source = $3)
		 order by namespace <> $1, namespace, workflow, position`, published, eventType, source)
	if err != nil {
		return nil, fmt.Errorf("db: the event triggers hearing %s could not be read: %w", published, err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Listening, error) {
		var l Listening
		err := row.Scan(&l.Namespace, &l.Workflow, &l.Position, &l.Commit, &l.Declared)
		return l, err
	})
}

// EventTaken takes the event source and id name as published into this namespace at at, by
// publisher, and answers false; or, where it was taken already within forget, how many runs it
// started, and true. Those taken longer ago are let go first, so that the table holds a day of
// events rather than every event ever published.
func (n *NS) EventTaken(ctx context.Context, source, id, publisher string, at time.Time, forget time.Duration) (int, bool, error) {
	if _, err := n.tx.Exec(ctx,
		`delete from event_deliveries where namespace = $1 and taken_at < $2`,
		n.namespace, at.Add(-forget)); err != nil {
		return 0, false, fmt.Errorf("db: the events past their day could not be let go: %w", err)
	}
	tag, err := n.tx.Exec(ctx,
		`insert into event_deliveries (namespace, source, id, taken_at, publisher) values ($1, $2, $3, $4, $5)
		 on conflict (namespace, source, id) do nothing`,
		n.namespace, source, id, at, publisher)
	if err != nil {
		return 0, false, fmt.Errorf("db: the event %s from %s could not be taken: %w", id, source, err)
	}
	if tag.RowsAffected() == 1 {
		return 0, false, nil
	}
	var runs int
	if err := n.tx.QueryRow(ctx,
		`select runs from event_deliveries where namespace = $1 and source = $2 and id = $3`,
		n.namespace, source, id).Scan(&runs); err != nil {
		return 0, false, fmt.Errorf("db: the event %s from %s could not be read: %w", id, source, err)
	}
	return runs, true, nil
}

// EventRuns records how many runs the event source and id name started, what one published again
// is answered with.
func (n *NS) EventRuns(ctx context.Context, source, id string, runs int) error {
	if _, err := n.tx.Exec(ctx,
		`update event_deliveries set runs = $4 where namespace = $1 and source = $2 and id = $3`,
		n.namespace, source, id, runs); err != nil {
		return fmt.Errorf("db: the runs of the event %s from %s could not be recorded: %w", id, source, err)
	}
	return nil
}

// EventFired records on the event trigger l names what an event it matched became: the run it
// started, or why it started none, a skipped firing, "as a schedule's is". A trigger armed anew
// since it was read records nothing, since what it now declares is not what matched.
func (n *NS) EventFired(ctx context.Context, l Listening, at time.Time, run agk.RunID, skipped string) error {
	if _, err := n.tx.Exec(ctx,
		`update triggers set fired_at = $5, fired_run = nullif($6, ''), skipped = nullif($7, '')
		 where namespace = $1 and workflow = $2 and kind = 'event' and position = $3 and commit = $4`,
		n.namespace, l.Workflow, l.Position, l.Commit, at, string(run), skipped); err != nil {
		return fmt.Errorf("db: the event trigger %d of %s could not record its firing: %w", l.Position, l.Workflow, err)
	}
	return nil
}
