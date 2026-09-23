package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// The grant's half of at-least-once delivery, against a real PostgreSQL: a redemption binds
// the task to the runner that made it, and a task that has ended is handed to nobody again.

// "the runner refuses to start a container for a key that has already completed", and the
// grant refuses it too, even to the runner that holds the task: a message delivered again
// after its result was recorded must not start the work a second time. Every one of the five
// endings counts, because each is a task nothing more is expected of.
func TestAnEndedTaskIsNotRedeemedEvenByItsRunner(t *testing.T) {
	super, app := database(t)
	seed(t, super)
	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx,
		`insert into steps (namespace, run_id, step) values ('finance', $1, 'render')`, financeRun); err != nil {
		t.Fatalf("seeding: %s", err)
	}

	now := time.Now().UTC()
	// dispatched writes one attempt of render the way the controller dispatches it: a row,
	// and the grant that goes into its message.
	dispatched := func(attempt int) (row string, key agk.TaskID, clear string) {
		row = "01M2G" + string(rune('A'+attempt)) + "AAAAAAAAAAAAAAAAAAAA"
		key = agk.NewTaskID(financeRun, "render", attempt, agk.Shard{})
		if _, err := conn.Exec(ctx, `
			insert into tasks (namespace, id, run_id, step, attempt, state)
			values ('finance', $1, $2, 'render', $3, 'dispatched')`, row, financeRun, attempt); err != nil {
			t.Fatalf("seeding attempt %d: %s", attempt, err)
		}
		err := pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
			granted, err := w.IssueGrant(ctx, "finance", key, row,
				GrantScope{Run: financeRun, Step: "render"}, now.Add(time.Hour))
			clear = granted.Clear
			return err
		})
		if err != nil {
			t.Fatalf("issuing the grant of attempt %d: %s", attempt, err)
		}
		return row, key, clear
	}
	redeem := func(clear string, key agk.TaskID) error {
		return pool.Installation(ctx, Redemption, func(ctx context.Context, w *Wide) error {
			_, err := w.Redeem(ctx, clear, key, "runner-1", now)
			return err
		})
	}

	// A task still dispatched redeems again for the runner that holds it, which a runner
	// that lost the answer to its first redemption depends on. It is also what makes the
	// refusals below about the state and not about the holder.
	_, key, clear := dispatched(1)
	for i := range 2 {
		if err := redeem(clear, key); err != nil {
			t.Fatalf("redemption %d of a task still dispatched, by the runner that holds it: %s", i+1, err)
		}
	}

	for i, ending := range []agk.TaskState{agk.TaskSucceeded, agk.TaskFailed, agk.TaskLost, agk.TaskTimedOut, agk.TaskCancelled} {
		row, key, clear := dispatched(i + 2)
		if err := redeem(clear, key); err != nil {
			t.Fatalf("the first redemption of the task that ends %s: %s", ending, err)
		}
		// The result is recorded, which moves the row to its ending and leaves it
		// bound to the runner that ran it.
		if _, err := conn.Exec(ctx,
			`update tasks set state = $2 where namespace = 'finance' and id = $1`, row, ending.String()); err != nil {
			t.Fatal(err)
		}
		if err := redeem(clear, key); !errors.Is(err, ErrTaskHeld) {
			t.Errorf("a task in state %s was redeemed again by its own runner, answering %v", ending, err)
		}
	}
}
