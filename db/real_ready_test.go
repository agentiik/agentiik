package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// A task is ready when its row is first written, or when a retry's backoff ends where it waits one
// out, and that moment is written once: a later decision writing the same task, dispatched by then
// and waiting on nothing, leaves it where it was, since it is what the queue wait is read from.
func TestATaskIsReadyOnceAndStaysSo(t *testing.T) {
	pool, super := created(t)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	backoff := now.Add(30 * time.Second)
	normalize := agk.NewTaskID(theRun, "normalize", 1, agk.Shard{})
	invoice := agk.NewTaskID(theRun, "invoice", 2, agk.Shard{})

	before := time.Now()
	decidedAs(t, pool, agk.Running, now,
		TaskRow{ID: normalize, Step: "normalize", Attempt: 1, State: agk.TaskPending},
		TaskRow{ID: invoice, Step: "invoice", Attempt: 2, State: agk.TaskPending, ReadyAt: backoff},
	)
	after := time.Now()

	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	readyAt := func() map[string]time.Time {
		t.Helper()
		rows, err := conn.Query(t.Context(), `select step, ready_at from tasks where run_id = $1`, string(theRun))
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]time.Time{}
		for rows.Next() {
			var step string
			var at time.Time
			if err := rows.Scan(&step, &at); err != nil {
				t.Fatal(err)
			}
			out[step] = at
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := readyAt()
	// Read by the database's clock where nothing was waited out, which is this host's here.
	if at := first["normalize"]; at.Before(before.Add(-time.Second)) || at.After(after.Add(time.Second)) {
		t.Errorf("a task written with nothing to wait out was ready at %s, and it was written between %s and %s", at, before, after)
	}
	if at := first["invoice"]; !at.Equal(backoff) {
		t.Errorf("an attempt waiting out its backoff until %s was ready at %s", backoff, at)
	}

	// Dispatched by the next decision, which carries no backoff any more.
	later := now.Add(time.Minute)
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		return w.SaveDecision(ctx, Decision{
			Namespace: "finance", Run: theRun, Was: 1, Seq: 2,
			Document: json.RawMessage(`{"version":1}`), State: agk.Running, StartedAt: now, WakeAt: later.Add(time.Hour),
			Tasks: []TaskRow{
				{ID: normalize, Step: "normalize", Attempt: 1, State: agk.TaskDispatched, DispatchedAt: later},
				{ID: invoice, Step: "invoice", Attempt: 2, State: agk.TaskDispatched, DispatchedAt: later, ReadyAt: later},
			},
		})
	}); err != nil {
		t.Fatal(err)
	}
	for step, at := range readyAt() {
		if !at.Equal(first[step]) {
			t.Errorf("%s was ready at %s and, dispatched, reads %s", step, first[step], at)
		}
	}
}
