package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// The two things the controller needs from the database that are not state: the term it holds
// the installation by, and the channel the API wakes it on.
//
// The election itself is not here. It is a property of a connection, held through Session,
// and package controller is where a controller is written.

// RunChannel is the channel the API signals a new run on.
//
// "When the API creates a run it commits the row and issues a NOTIFY carrying the run
// identifier. PostgreSQL delivers such an event only once the transaction commits, and only
// to sessions currently listening." One channel for the installation, because the controller
// that listens is the installation's.
const RunChannel = "agentiik_run"

// ErrFenced is a write by a controller that is no longer the active one.
//
// "Because a partitioned former holder may not yet know it has lost, every controller write
// carries the lock's acquisition counter as a fencing token and a write bearing an older
// counter is refused. Election alone would not be safe; the fencing token is what makes it
// so." A controller seeing this has lost the lock and has not noticed yet, and the right
// answer is to stop being a controller rather than to retry.
var ErrFenced = errors.New("db: this controller's term has ended and another holds the lock")

// Term is one controller's period as the active one.
type Term struct {
	// Token is the acquisition counter, raised by whoever takes the lock. It only goes
	// up, so a write carrying an older one is a write from a former holder.
	Token int64

	// Holder is the name that controller was given, and AcquiredAt when it took the lock.
	// Neither decides anything: they are what a person reads during an incident.
	Holder     string
	AcquiredAt time.Time
}

// BeginTerm raises the acquisition counter and answers the term that starts.
//
// It is called once, by whoever has just taken the advisory lock, and never by a standby. The
// counter is a column and not the lock's own, because PostgreSQL does not hand one out for an
// advisory lock.
func (p *Pool) BeginTerm(ctx context.Context, holder string) (Term, error) {
	if holder == "" {
		return Term{}, errors.New("db: a controller term with no holder: a name is what an operator reads when two processes each believe they are the active one")
	}
	var t Term
	err := p.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
		return w.tx.QueryRow(ctx,
			`update controller_term set token = token + 1, holder = $1, acquired_at = now()
			 where sole returning token, holder, acquired_at`,
			holder).Scan(&t.Token, &t.Holder, &t.AcquiredAt)
	})
	if err != nil {
		return Term{}, fmt.Errorf("db: the controller term could not be taken: %w", err)
	}
	return t, nil
}

// Fence refuses this transaction if the term has moved on.
//
// Called at the top of every controller write, before anything is read or written, so that a
// former holder that has not noticed cannot act on state it no longer owns. The row is locked
// for the life of the transaction, so a controller taking the term waits for an in-flight
// write to finish rather than overlapping it, and everything that starts afterwards is
// refused.
func (w *Wide) Fence(ctx context.Context, token int64) error {
	var held int64
	var holder *string
	err := w.tx.QueryRow(ctx,
		`select token, holder from controller_term where sole for share`).Scan(&held, &holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("db: there is no controller term row, so nothing can be fenced: the schema is older than the controller")
	}
	if err != nil {
		return fmt.Errorf("db: the controller term could not be read: %w", err)
	}
	if held != token {
		name := "another instance"
		if holder != nil {
			name = *holder
		}
		return fmt.Errorf("%w: this write carries term %d and the term is %d, held by %s", ErrFenced, token, held, name)
	}
	return nil
}

// NotifyRun tells the controller a run is worth looking at.
//
// The payload is the identifier and nothing else, which keeps it far below the documented
// eight thousand byte limit and keeps the notification what it is: a latency optimisation. The
// controller's sweep is the correctness guarantee, so a notification that is never delivered,
// to a controller that was restarting, costs latency and not a run.
//
// It is on the namespaced door because the API sends it in the same transaction that writes
// the run, which is what makes the two one fact: PostgreSQL delivers the event only when that
// transaction commits.
func (n *NS) NotifyRun(ctx context.Context, run agk.RunID) error {
	if err := run.Validate(); err != nil {
		return fmt.Errorf("db: the run to notify about: %w", err)
	}
	if _, err := n.tx.Exec(ctx, `select pg_notify($1, $2)`, RunChannel, string(run)); err != nil {
		return fmt.Errorf("db: the controller could not be notified about run %s: %w", run, err)
	}
	return nil
}
