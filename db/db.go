package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is the connection pool, and nothing a statement can be issued on.
//
// It has no Query, no Exec and no Row. The only thing it hands out is a handle that has
// already bound a namespace, or a handle whose name says why it has not. That is the whole
// of how "every query path carries the namespace" is made true here: not by asking callers
// to remember, but by giving them nothing to forget with.
type Pool struct {
	pool *pgxpool.Pool
}

// Open connects, and refuses a connection that would make the policy decorative.
//
// The check is not ceremony. Row level security is skipped entirely for a superuser and
// for a role holding BYPASSRLS, so an installation that connected as one would enforce the
// namespace nowhere while every test still passed: the tests bind a namespace, and the
// thing being tested is what happens when somebody does not. Refusing at startup is the
// only moment this can be caught before it matters.
func Open(ctx context.Context, url string) (*Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("db: the database at that address could not be reached: %w", err)
	}
	if err := checkTheRoleCannotBypass(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Pool{pool: pool}, nil
}

// checkTheRoleCannotBypass refuses a role that walks through the policies.
func checkTheRoleCannotBypass(ctx context.Context, pool *pgxpool.Pool) error {
	var super, bypass bool
	var role string
	err := pool.QueryRow(ctx,
		`select current_user, rolsuper, rolbypassrls from pg_roles where rolname = current_user`,
	).Scan(&role, &super, &bypass)
	if err != nil {
		return fmt.Errorf("db: the connection could not be asked what it is: %w", err)
	}
	if super || bypass {
		which := "is a superuser"
		if !super {
			which = "holds BYPASSRLS"
		}
		return fmt.Errorf(
			"db: this connection is %s and %s, so row level security does not apply to it and the namespace on every query path would be enforced nowhere: connect as a role created NOSUPERUSER NOBYPASSRLS, which is the role Provision creates beside the migrations. A superuser is for migrations and for nothing else",
			role, which)
	}
	return nil
}

// Close releases the pool.
func (p *Pool) Close() { p.pool.Close() }

// NS is a handle on one namespace, inside one transaction.
//
// Every statement it issues runs with agentiik.namespace bound, so the policies see the
// namespace whether or not the statement mentions it. A statement that forgets the column
// reads that namespace's rows rather than everybody's, and a statement that writes another
// namespace's name is refused.
type NS struct {
	tx        pgx.Tx
	namespace string
}

// Namespace is the namespace this handle is bound to.
func (n *NS) Namespace() string { return n.namespace }

// In runs fn inside one transaction, in one namespace.
//
// The namespace is bound with SET LOCAL, so it lasts exactly as long as the transaction
// and cannot leak into the next caller of a pooled connection. fn returning an error rolls
// back; fn returning nil commits.
func (p *Pool) In(ctx context.Context, namespace string, fn func(context.Context, *NS) error) error {
	if namespace == "" {
		return errors.New("db: In was given no namespace, and a handle bound to nothing would read nothing: the namespace comes from the authorisation decision, never from what a request said about itself")
	}
	return p.transaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// set_config rather than SET LOCAL, because the value is a parameter here: a
		// namespace interpolated into a SET statement would be the one place in this
		// package where a name becomes SQL.
		if _, err := tx.Exec(ctx, `select set_config('agentiik.namespace', $1, true)`, namespace); err != nil {
			return fmt.Errorf("db: the namespace could not be bound: %w", err)
		}
		return fn(ctx, &NS{tx: tx, namespace: namespace})
	})
}

// Reason is why a piece of work legitimately has no namespace.
//
// It is an enumeration and not a string so that the set stays small and countable: every
// escape from the namespace rule is one of these, a test counts the call sites, and adding
// a new one is a line somebody writes here and a reviewer reads.
type Reason string

const (
	// ControllerSweep is the controller looking for actionable work across the
	// installation, which the documentation calls the correctness guarantee of the
	// scheduler, the notification being only a latency optimisation.
	ControllerSweep Reason = "the controller's sweep"

	// Purge is one of the three sweeps, over envelopes, artifacts or logs, each
	// against its declared retention.
	Purge Reason = "a retention purge"

	// Collect is the garbage collector, over objects whose reference count reached
	// zero.
	Collect Reason = "collecting objects nothing references"

	// RunnerInventory is the runner table, which belongs to the installation: a
	// runner serves several namespaces and its inventory is administrator only.
	RunnerInventory Reason = "the runner inventory"

	// Heartbeat is one request covering every in-flight task on one host, which is
	// one host across however many namespaces it is working for.
	Heartbeat Reason = "a runner's heartbeat"

	// Redemption is a runner turning a grant into what the task it names was given.
	// The namespace is what the grant answers rather than what the caller claims: a
	// runner works for several and knows which one this task belongs to only because
	// the grant said so, and a request that carried a namespace of its own would be a
	// request choosing the scope its own credential is checked in. What it reads is the
	// grant, the task it names, and the tree of the one version the grant's scope names.
	Redemption Reason = "a grant redemption"

	// LogShipment is a runner shipping a chunk of a task's log, which it is authorised for
	// the way it redeems: by the task's grant, whose namespace is found from the grant and
	// never taken from the request, and by the task being bound to it. What it reads and
	// writes is the grant, the task it names, and the index to that task's log.
	LogShipment Reason = "a runner shipping a task's log"

	// RunRoute is a route about one run, whose path names the run and not the workflow it is
	// of, and often not its namespace: "Agentiik sends the push service an identifier and a
	// state", and the application a notification opens holds that identifier and nothing
	// else. What it reads is which namespace and workflow the run is of, and that goes to the
	// router and the authorizer and nowhere else; the route itself reads and writes through
	// In, in the namespace it was authorised in.
	RunRoute Reason = "a route that names a run and not its namespace"

	// RunListing is GET /api/v1/runs, "across every namespace the caller can read", and GET
	// /api/v1/{ns}/runs, the same listing within one. What it reads is which workflows there
	// are, for the authorizer to be asked about each, and then the runs of the ones it allowed
	// and of no others: the namespaces a listing reaches are the ones the authorisation
	// decision named, as In's always are. One namespace's listing steps past In too, rather
	// than reading its runs under the namespace's policy, because that policy admits every
	// workflow of the namespace, and "a deny wins at any scope" only where the workflow is in
	// the question.
	RunListing Reason = "a listing of runs across the namespaces its caller can read"

	// SchemaUpgrade is the schema itself. Named for the act rather than for the file,
	// since Migration is the file.
	SchemaUpgrade Reason = "a schema upgrade"
)

// Wide is a handle on the installation, inside one transaction.
//
// It sees every namespace, which is why it exists and why it is named rather than
// reached. Everything it is legitimately for is in Reason.
type Wide struct {
	tx     pgx.Tx
	reason Reason
}

// Installation runs fn across every namespace, for a named reason.
func (p *Pool) Installation(ctx context.Context, reason Reason, fn func(context.Context, *Wide) error) error {
	if reason == "" {
		return errors.New("db: Installation was given no reason, and stepping past the namespace without saying why is the one thing this door exists to prevent")
	}
	return p.transaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select set_config('agentiik.scope', 'installation', true)`); err != nil {
			return fmt.Errorf("db: the installation scope could not be bound: %w", err)
		}
		return fn(ctx, &Wide{tx: tx, reason: reason})
	})
}

// Reason is why this handle was opened.
func (w *Wide) Reason() Reason { return w.reason }

// Session is one connection, pinned, for as long as fn runs.
//
// The advisory lock the controller is elected by is held "until explicitly released or the
// session ends", and LISTEN is a property of a connection: both outlive a transaction and
// neither survives a connection going back to the pool. Nothing namespaced is reachable
// through it, because a session held for minutes is not a place to be reading a tenant's
// rows from.
func (p *Pool) Session(ctx context.Context, fn func(context.Context, *pgxpool.Conn) error) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("db: a connection could not be pinned: %w", err)
	}
	defer conn.Release()
	return fn(ctx, conn)
}

// rollbackWithin bounds the rollback on the error path, which cannot be bounded by ctx.
//
// The connection it goes on may be one that no longer answers: a network cut off without a
// reset leaves it open and silent, and a rollback with no bound of its own waits there for as
// long as TCP does, which is hours. That is where a controller asked to stop in the middle of a
// sweep while cut off from its database stayed. Past the bound pgx closes the connection and the
// pool drops it, and PostgreSQL rolls the transaction back itself when the session ends, so
// nothing fn did is kept either way. A rollback that is answered at all is answered in
// milliseconds, so the bound only ever meets a connection that is gone.
const rollbackWithin = 5 * time.Second

// transaction is the one place a transaction is begun, committed and rolled back.
//
// A rollback on the error path uses a context of its own, because the usual reason fn
// failed is that ctx was cancelled and a rollback on a cancelled context does not run,
// which would leave the transaction open until the connection was recycled. Its own is
// bounded by rollbackWithin rather than by nothing.
func (p *Pool) transaction(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: a transaction could not be begun: %w", err)
	}
	if err := fn(ctx, tx); err != nil {
		rollback, stop := context.WithTimeout(context.WithoutCancel(ctx), rollbackWithin)
		defer stop()
		tx.Rollback(rollback)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: the transaction could not be committed: %w", err)
	}
	return nil
}
