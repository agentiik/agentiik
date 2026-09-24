package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
// guessing: it presented a valid grant for a real task, and what it needs to know is that the work
// is somebody else's, or over, and that it acknowledges the message and starts nothing rather than
// retrying or putting the message back for the next runner to be refused in its turn.
var ErrTaskHeld = errors.New("db: that task is held by another runner")

// ErrRunnerNotTaking is a redemption by a runner that is draining or revoked of a task it does not
// hold yet, since neither takes anything new.
//
// Separate from ErrTaskHeld because the runner does the opposite with the message: the task is
// nobody's yet and some other runner of the pool should have it, so it is put back rather than
// acknowledged.
var ErrRunnerNotTaking = errors.New("db: that runner is draining or revoked, and takes no new task")

// ErrPoolRefusesNamespace is a redemption of a task whose namespace the redeeming runner's pool does
// not accept.
//
// No runner of that pool may ever run it, since what a pool accepts is the pool's and every one of
// its runners carries it, so it is not put back for the next one: the redemption can never be
// answered, and the runner reports that no container ran. The controller refuses to publish such a
// task in the first place, so this is a pool whose policy changed while the task waited, or a grant
// presented by a runner of a pool that does not run the namespace. It is the runner's pool that is
// read and not the one the task was published to, which neither the task nor its grant records.
var ErrPoolRefusesNamespace = errors.New("db: that runner's pool does not accept the task's namespace")

// ErrRunnerNarrowed is a redemption of a task whose namespace the redeeming runner's own narrowing,
// AGK_RUNNER_NAMESPACES, leaves out.
//
// Separate from ErrPoolRefusesNamespace because the runner does the opposite with the message: the
// pool accepts the namespace, so another runner of the pool may take the task, and it is put back
// rather than reported. The runner checks its narrowing before it redeems; this is the API holding
// it to the same list, sent at join, rather than trusting it to.
var ErrRunnerNarrowed = errors.New("db: that runner narrows itself to namespaces that leave out the task's")

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
// delivered twice and to two machines; the second one to redeem is told the work is somebody else's
// rather than starting a container for it. A runner redeems before it acknowledges the message, so
// this, and not the bus, is what decides whose a task is: the bus decides only who is handed it.
// The binding is never released, because a task whose runner was lost is moved to lost and requeued
// as a new row under the same key, with a grant of its own.
//
// A key may therefore have several rows, one per dispatch, and the rule is about the key: "the
// runner refuses to start a container for a key that has already completed". The row is what is
// read, and it answers for the key because of the uniqueness rule on tasks: at most one row of a
// key is anything but lost, so a key that completed on one dispatch has every row over, and a key
// with a row still going has completed on none. A lost row is refused like any row that is over,
// since its dispatch was superseded by the requeue and the grant it carried opens nothing.
func (w *Wide) Redeem(ctx context.Context, clear string, task agk.TaskID, runner string, now time.Time) (Redeemed, error) {
	got, err := w.redeemable(ctx, clear, task, runner, now, true)
	if err != nil {
		return Redeemed{}, err
	}
	if _, err := w.tx.Exec(ctx,
		`update task_grants set redeemed_at = $4 where namespace = $1 and task_id = $2 and hash = $3`,
		got.Namespace, got.Row, token.Hash(clear), now); err != nil {
		return Redeemed{}, fmt.Errorf("db: the redemption could not be recorded: %w", err)
	}
	if _, err := w.tx.Exec(ctx,
		`update tasks set runner = $3 where namespace = $1 and id = $2`,
		got.Namespace, got.Row, runner); err != nil {
		return Redeemed{}, fmt.Errorf("db: the task could not be bound to its runner: %w", err)
	}
	return got, nil
}

// Redeemable is Redeem without the binding: every check Redeem makes, refused the same way, and
// nothing written.
//
// It is the first half of a redemption and Redeem the second. What the grant is for is read
// between the two, the secret values last, so that a value is read only for a grant that would
// redeem and a task is bound only once there is an answer to give the runner. One transaction for
// both would hold the task's row, and a connection, across a round trip to whichever store keeps
// the value. Redeem checks again under the lock, so a task bound, ended or expired in between is
// refused there and nothing is answered.
func (w *Wide) Redeemable(ctx context.Context, clear string, task agk.TaskID, runner string, now time.Time) (Redeemed, error) {
	return w.redeemable(ctx, clear, task, runner, now, false)
}

// redeemable is the checks Redeem and Redeemable share, the task's row locked for Redeem to bind.
func (w *Wide) redeemable(ctx context.Context, clear string, task agk.TaskID, runner string, now time.Time, lock bool) (Redeemed, error) {
	id, ok := token.TaskOf(clear)
	if !ok {
		return Redeemed{}, ErrNoGrant
	}

	// Found by its hash as well as its task, because a task may have been issued several, one
	// for every message prepared for it, and each redeems.
	query := `
		select g.namespace, g.expires_at, g.scope, t.idempotency_key, t.state, t.runner
		from task_grants g join tasks t on t.namespace = g.namespace and t.id = g.task_id
		where g.task_id = $1 and g.hash = $2`
	if lock {
		query += ` for update of t`
	}
	var namespace string
	var scope GrantScope
	var expires time.Time
	var key agk.TaskID
	var state string
	var holder *string
	err := w.tx.QueryRow(ctx, query, id, token.Hash(clear)).
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

	// "A redemption by a draining or revoked runner gets 403, binds nothing", since both take
	// nothing new, and so does one whose pool does not accept the run's namespace (422) or whose
	// own narrowing leaves it out (403). What a runner already holds is not new: the holder asking
	// again, after a lost answer or a restart, is finishing what it holds, which is what a drain
	// and a grace are for, and refused it would put back a message nobody else may redeem and
	// leave its task to be declared lost. It passed these checks when it bound the task. So only a
	// redemption that would bind is refused. They are read here, where the binding reads them
	// again under the lock, so that a drain or a revocation that commits while a redemption is
	// under way is obeyed and the secrets read in between go nowhere. The runner is read by what
	// its row says of it, and the names this package is handed carry no promise of a row: the API
	// hands it the runner a credential opened, which has one.
	if holder == nil {
		if err := w.mayTake(ctx, runner, namespace); err != nil {
			return Redeemed{}, err
		}
	}
	return Redeemed{Namespace: namespace, Row: id, Task: key, Scope: scope, ExpiresAt: expires}, nil
}

// mayTake holds a runner that would bind a task to what it may take: its standing first, then its
// pool, then its own narrowing.
//
// In that order for a reason each. A draining or revoked runner settles nothing new, and a 422 would
// have it report the task, which binds it. A namespace the pool refuses is one every runner of the
// pool refuses, since a host's narrowing may only name namespaces its pool accepts, so it is
// answered as never answerable rather than put back to go round the pool until its deadline.
func (w *Wide) mayTake(ctx context.Context, runner, namespace string) error {
	var state string
	var accepted, narrowed []string
	err := w.tx.QueryRow(ctx, `
		select r.state, p.accepted_namespaces::text[], r.accepted_namespaces::text[]
		from runners r join runner_pools p on p.name = r.pool
		where r.id = $1`, runner).Scan(&state, &accepted, &narrowed)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("db: the runner redeeming the grant could not be read: %w", err)
	case state != "ready":
		return ErrRunnerNotTaking
	case !(RunnerPool{AcceptedNamespaces: accepted}).Accepts(namespace):
		return ErrPoolRefusesNamespace
	case narrowed != nil && !slices.Contains(narrowed, namespace):
		return ErrRunnerNarrowed
	}
	return nil
}
