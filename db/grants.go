package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/token"
	"github.com/jackc/pgx/v5"
)

// The per-task grant.
//
// What is kept is a hash and an expiry. The clear value exists once, in the task message, and is
// not recoverable from here: "stored hashed, shown once at creation".

// ErrNoGrant is nothing of that value, for that task, that has not expired. One error for all
// three, because telling a caller which of them it was is telling somebody guessing whether they
// had the right task, the right value, or merely the wrong moment.
var ErrNoGrant = errors.New("db: no grant of that value for that task")

// Granted is a grant as it was issued: the clear value, once, and when it stops working.
type Granted struct {
	Task      agk.TaskID
	Clear     string
	ExpiresAt time.Time
}

// IssueGrant mints the grant for one task and records what it takes to check it.
//
// The row is written by the controller inside the decision that planned the task, so a task that
// exists has a grant and a grant belongs to a task that exists. The clear value is answered here
// and nowhere else: it goes into the message and is then unrecoverable.
func (w *Wide) IssueGrant(ctx context.Context, namespace string, task agk.TaskID, id string, until time.Time) (Granted, error) {
	if namespace == "" {
		return Granted{}, errors.New("db: a grant with no namespace")
	}
	if id == "" {
		return Granted{}, fmt.Errorf("db: the grant for %s names no task row", task)
	}
	if until.IsZero() {
		return Granted{}, fmt.Errorf("db: the grant for %s would never expire, and a grant expires with its task", task)
	}

	clear, hashed, err := token.New(token.Grant, id)
	if err != nil {
		return Granted{}, fmt.Errorf("db: %w", err)
	}
	if _, err := w.tx.Exec(ctx,
		`insert into task_grants (namespace, task_id, hash, expires_at)
		 values ($1, $2, $3, $4)
		 on conflict (namespace, task_id) do update
		 set hash = excluded.hash, expires_at = excluded.expires_at, redeemed_at = null`,
		namespace, id, hashed, until); err != nil {
		return Granted{}, fmt.Errorf("db: the grant for %s could not be recorded: %w", task, err)
	}
	return Granted{Task: task, Clear: clear, ExpiresAt: until}, nil
}

// Redeem checks a grant and answers what it is for.
//
// The value names the task inside its own text and the caller names one too, and both are checked
// against the row: "the API refuses a redemption where the two disagree rather than believing
// either alone". A grant past its expiry is refused like one that never existed, because a
// credential that says how it failed is a credential that helps somebody find the next one.
func (w *Wide) Redeem(ctx context.Context, clear string, task agk.TaskID, now time.Time) (string, error) {
	id, ok := token.TaskOf(clear)
	if !ok {
		return "", ErrNoGrant
	}

	var namespace, hashed string
	var expires time.Time
	var key agk.TaskID
	err := w.tx.QueryRow(ctx, `
		select g.namespace, g.hash, g.expires_at, t.idempotency_key
		from task_grants g join tasks t on t.namespace = g.namespace and t.id = g.task_id
		where g.task_id = $1`, id).Scan(&namespace, &hashed, &expires, &key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoGrant
	}
	if err != nil {
		return "", fmt.Errorf("db: the grant could not be read: %w", err)
	}

	switch {
	case !token.Same(clear, hashed):
		return "", ErrNoGrant
	case !now.Before(expires):
		return "", ErrNoGrant
	case task != "" && task != key:
		// The body named one task and the grant names another, which is a request
		// somebody assembled out of two and neither half is evidence about the other.
		return "", ErrNoGrant
	}

	if _, err := w.tx.Exec(ctx,
		`update task_grants set redeemed_at = $3 where namespace = $1 and task_id = $2`,
		namespace, id, now); err != nil {
		return "", fmt.Errorf("db: the redemption could not be recorded: %w", err)
	}
	return namespace, nil
}
