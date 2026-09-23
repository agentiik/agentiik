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

// GrantScope is what one grant may be turned into, and the whole of it.
//
// It is what the task was dispatched with, written by the controller at the moment it decided:
// the version whose tree the task sees under /agk/repo, the envelopes on each input port, named by
// digest, and the secrets the step asked for, named with where they go and never valued. A
// redemption answers from this and from nothing else, which is what makes "refusing anything the
// task does not name" a comparison rather than a promise.
type GrantScope struct {
	Run  agk.RunID `json:"run,omitempty"`
	Step agk.Step  `json:"step,omitempty"`

	// Workflow and Commit are the version the task runs, which is what names its tree: "the
	// controller resolves a commit to a tree and the runner fetches content-addressed objects
	// with the task's grant". Written here rather than read off the run at redemption, so that
	// what a runner is handed is what the controller decided and not what a second lookup
	// found, and so that a runner holding a grant can reach one version's files and no other.
	Workflow string `json:"workflow,omitempty"`
	Commit   string `json:"commit,omitempty"`

	Inputs  []GrantInput  `json:"inputs,omitempty"`
	Secrets []GrantSecret `json:"secrets,omitempty"`
}

// GrantInput is one input port's envelope, named by digest.
type GrantInput struct {
	Port   agk.Port `json:"port"`
	Digest string   `json:"digest"`
	Items  int      `json:"items"`
}

// GrantSecret is one secret the step asked for, by name, and the path the value is written at.
//
// The mount travels with the name because it is not always /agk/secrets/<name>: a brick manifest
// may ask for a secret somewhere else under /agk/secrets/, and the value and the path it belongs at
// have to reach the runner as one entry. Only the controller read the manifest, so only the
// controller can write it down.
type GrantSecret struct {
	Name  string `json:"name"`
	Mount string `json:"mount"`
}

// Redeemed is what a grant turned out to be for.
type Redeemed struct {
	Namespace string

	// Row is the task's own identifier, the one the grant names inside its text, and Task is
	// the idempotency key that says which unit of work it is. A redemption is asked with both
	// and answers the row.
	Row   string
	Task  agk.TaskID
	Scope GrantScope

	// ExpiresAt is the grant's own expiry, which is the task's deadline. Anything the
	// redemption hands out is minted to end with it: a URL that outlived the task it was
	// fetched for would be the standing credential the grant exists to avoid.
	ExpiresAt time.Time
}

// ErrTaskHeld is a task another runner is already working on.
//
// Separate from ErrNoGrant because the caller is an authenticated runner rather than somebody
// guessing: it presented a valid grant for a real task, and what it needs to know is that the
// work is somebody else's and it should stop rather than retry.
var ErrTaskHeld = errors.New("db: that task is held by another runner")

// IssueGrant mints the grant for one task and records what it takes to check it.
//
// The row is written by the controller inside the decision that planned the task, so a task that
// exists has a grant and a grant belongs to a task that exists. The clear value is answered here
// and nowhere else: it goes into the message and is then unrecoverable.
//
// Each call adds a grant beside those the row already has, and replaces none of them. A task is
// issued one again when a pass published it and could not record the dispatch, so the next pass
// plans it and publishes it again, and the message that went first may be the only one on the
// queue: the stream deduplicates a task on its row. Replaced, the grant that message carries would
// open nothing, and the runner taking it would be left to report a task that never reached a
// container, which ends it. Kept, whichever message a runner takes redeems, and the first
// redemption binds the task for all of them.
func (w *Wide) IssueGrant(ctx context.Context, namespace string, task agk.TaskID, id string, scope GrantScope, until time.Time) (Granted, error) {
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
		`insert into task_grants (namespace, task_id, hash, expires_at, scope)
		 values ($1, $2, $3, $4, $5)`,
		namespace, id, hashed, until, scope); err != nil {
		return Granted{}, fmt.Errorf("db: the grant for %s could not be recorded: %w", task, err)
	}
	return Granted{Task: task, Clear: clear, ExpiresAt: until}, nil
}

// Redeem checks a grant, binds the task to the runner redeeming it, and answers what it is for.
//
// The value names the task inside its own text and the caller names one too, and both are checked
// against the row: "the API refuses a redemption where the two disagree rather than believing
// either alone". A grant past its expiry is refused like one that never existed, because a
// credential that says how it failed is a credential that helps somebody find the next one.
//
// Binding the task here is what makes at-least-once delivery safe on the way in. A message may be
// delivered twice and to two machines; the second one to redeem is told the work is somebody
// else's rather than starting a container for it. The binding is never released, because a task
// whose runner was lost is moved to lost and requeued as a new row under the same key, with a
// grant of its own.
//
// A key may therefore have several rows, one per dispatch, and the rule is about the key: "the
// runner refuses to start a container for a key that has already completed". The row is what is
// read, and it answers for the key because of the uniqueness rule on tasks: at most one row of a
// key is anything but lost, so a key that completed on one dispatch has every row over, and a key
// with a row still going has completed on none. A lost row is refused like any row that is over,
// since its dispatch was superseded by the requeue and the grant it carried opens nothing.
func (w *Wide) Redeem(ctx context.Context, clear string, task agk.TaskID, runner string, now time.Time) (Redeemed, error) {
	id, ok := token.TaskOf(clear)
	if !ok {
		return Redeemed{}, ErrNoGrant
	}

	// Found by its hash as well as its task, because a task may have been issued several, one
	// for every message prepared for it, and each redeems.
	hashed := token.Hash(clear)
	var namespace string
	var scope GrantScope
	var expires time.Time
	var key agk.TaskID
	var state string
	var holder *string
	err := w.tx.QueryRow(ctx, `
		select g.namespace, g.expires_at, g.scope, t.idempotency_key, t.state, t.runner
		from task_grants g join tasks t on t.namespace = g.namespace and t.id = g.task_id
		where g.task_id = $1 and g.hash = $2 for update of t`, id, hashed).
		Scan(&namespace, &expires, &scope, &key, &state, &holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return Redeemed{}, ErrNoGrant
	}
	if err != nil {
		return Redeemed{}, fmt.Errorf("db: the grant could not be read: %w", err)
	}

	switch {
	case !now.Before(expires):
		return Redeemed{}, ErrNoGrant
	case task != "" && task != key:
		// The body named one task and the grant names another, which is a request
		// somebody assembled out of two and neither half is evidence about the other.
		return Redeemed{}, ErrNoGrant
	}

	var where agk.TaskState
	if err := where.UnmarshalText([]byte(state)); err != nil {
		return Redeemed{}, fmt.Errorf("db: the task is in state %q: %w", state, err)
	}

	switch {
	case runner == "":
		return Redeemed{}, errors.New("db: a redemption by no runner, and a task is held by the machine that redeemed it")
	case holder != nil && *holder != runner:
		return Redeemed{}, ErrTaskHeld
	case where.Terminal():
		// "the runner refuses to start a container for a key that has already
		// completed", and refusing the grant is the same rule applied where it cannot
		// be forgotten.
		return Redeemed{}, ErrTaskHeld
	}

	if _, err := w.tx.Exec(ctx,
		`update task_grants set redeemed_at = $4 where namespace = $1 and task_id = $2 and hash = $3`,
		namespace, id, hashed, now); err != nil {
		return Redeemed{}, fmt.Errorf("db: the redemption could not be recorded: %w", err)
	}
	if _, err := w.tx.Exec(ctx,
		`update tasks set runner = $3 where namespace = $1 and id = $2`,
		namespace, id, runner); err != nil {
		return Redeemed{}, fmt.Errorf("db: the task could not be bound to its runner: %w", err)
	}
	return Redeemed{Namespace: namespace, Row: id, Task: key, Scope: scope, ExpiresAt: expires}, nil
}
