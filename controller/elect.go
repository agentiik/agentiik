package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"

	"github.com/agentiik/agentiik/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lockKey is the advisory lock the active controller is the holder of.
//
// One key for the installation, since there is one active controller. The number itself is
// arbitrary and its only requirement is that nothing else in the installation picks it: it is
// the low sixty-three bits of "agentiik" read as bytes, written as a literal so that a person
// finding it in pg_locks can search for it.
const lockKey int64 = 0x6167656e7469696b

// Controller is one instance, active or waiting to be.
type Controller struct {
	pool *db.Pool
	name string

	// Poll is how often a standby tries the lock, and how often the holder asks whether it
	// still holds it. It is a latency and not a correctness setting: whatever it is, the
	// standby takes over the instant a try succeeds, the fence refuses a former holder's
	// writes, and the only thing a shorter one buys is the time between the holder going and
	// that, or between the holder losing the lock and stopping.
	Poll time.Duration

	// Sweep is how often the active controller looks for work nobody told it about.
	Sweep time.Duration

	// Trouble is where something worth saying goes: a run that could not be decided, a
	// message the bus refused, a stop nobody took, a sweep that could not look for lost
	// tasks, which names no run. None of them is fatal to the controller and all of them are
	// worth a person seeing, so an installation says where they go and a controller with
	// nowhere to put them drops them rather than choosing for it.
	//
	// It is a field rather than a package level logger because a controller is a value a
	// test builds, and a test that had to read standard error to find out what happened is
	// a test nobody writes.
	Trouble func(run agk.RunID, err error)
}

// report says one thing, through whatever Trouble was given.
func (c *Controller) report(run agk.RunID, err error) {
	if c.Trouble == nil {
		return
	}
	c.Trouble(run, err)
}

// New builds a controller. name is what an operator calls this process, and is written on the
// term so that a person reading the table during an incident can tell two instances apart.
func New(pool *db.Pool, name string) (*Controller, error) {
	if pool == nil {
		return nil, errors.New("controller: no database, and the state lives in the database")
	}
	if name == "" {
		return nil, errors.New("controller: a controller with no name: two instances each believing they are active is exactly the moment somebody needs to tell them apart")
	}
	return &Controller{pool: pool, name: name, Poll: 2 * time.Second, Sweep: 10 * time.Second}, nil
}

// Name is what this instance is called.
func (c *Controller) Name() string { return c.name }

// ErrLockLost is why a term ended where the session holding the lock stopped answering, or
// answered that it no longer holds it.
var ErrLockLost = errors.New("controller: the session holding the lock stopped holding it, so this instance no longer leads")

// cleanupWithin bounds what is sent to the database on the way out of a term or a watch, where
// the connection it goes on may be one that no longer answers: unlocking or unlistening on a
// connection cut off without a reset would otherwise wait for TCP to give up, which is hours, and
// a process asked to stop would not.
const cleanupWithin = 5 * time.Second

// Lead waits for the lock, then runs fn for as long as this instance holds it.
//
// It blocks: a standby is a process sitting in here, trying the non-blocking variant on a loop
// and taking over the instant it succeeds. fn is called once, with the term that has just
// started, and Lead returns what fn returns. The lock is released when fn returns, when ctx is
// done, and by the database itself if this process dies without either.
//
// And fn is stopped when the lock is gone. The lock is a property of one session, which nothing
// in fn uses, so nothing in fn would notice it end: a backend terminated, or a connection a
// middlebox dropped without a reset, leaves a controller deciding with no lock held, and in the
// second case with every statement waiting on a network that does not answer. So the session is
// asked on every poll whether it still holds the lock, within the poll's own bound, and a session
// that fails to say so ends the term with ErrLockLost. Asking also keeps the session from ever
// looking idle to whatever closes idle connections.
func (c *Controller) Lead(ctx context.Context, fn func(context.Context, db.Term) error) error {
	return c.pool.Session(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		if err := c.waitForTheLock(ctx, conn); err != nil {
			return err
		}
		// Released explicitly rather than left to the session, because a Session hands
		// its connection back to the pool and a lock still held would travel with it.
		defer func() {
			unlock, stop := context.WithTimeout(context.WithoutCancel(ctx), cleanupWithin)
			defer stop()
			conn.Exec(unlock, `select pg_advisory_unlock($1)`, lockKey)
		}()

		term, err := c.pool.BeginTerm(ctx, c.name)
		if err != nil {
			return err
		}

		leading, lost := context.WithCancelCause(ctx)
		defer lost(nil)
		held := make(chan struct{})
		go func() {
			defer close(held)
			c.holdTheLock(leading, conn, lost)
		}()
		err = fn(leading, term)
		lost(nil)
		// The connection is the unlock's once this returns, and is not shared.
		<-held
		if cause := context.Cause(leading); errors.Is(cause, ErrLockLost) {
			return cause
		}
		return err
	})
}

// holdTheLock asks the session on every poll whether it still holds the lock, until ctx is done,
// and ends the term through lost the first time it cannot say it does.
func (c *Controller) holdTheLock(ctx context.Context, conn *pgxpool.Conn, lost context.CancelCauseFunc) {
	poll := c.poll()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
		// Bounded by the poll and never less than a second, so that a database slow for a
		// moment is not mistaken for one that is gone.
		asking, stop := context.WithTimeout(ctx, max(poll, time.Second))
		var held bool
		err := conn.QueryRow(asking,
			`select exists (select 1 from pg_locks where locktype = 'advisory' and granted
			   and pid = pg_backend_pid() and objsubid = 1
			   and (classid::bigint << 32 | objid::bigint) = $1)`, lockKey).Scan(&held)
		stop()
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			lost(fmt.Errorf("%w: %w", ErrLockLost, err))
			return
		case !held:
			lost(ErrLockLost)
			return
		}
	}
}

// poll is how often a standby tries the lock and a leader asks whether it still holds it.
func (c *Controller) poll() time.Duration {
	if c.Poll <= 0 {
		return time.Second
	}
	return c.Poll
}

// waitForTheLock tries the non-blocking variant until it succeeds or ctx is done.
//
// The blocking variant would be shorter and is wrong here: it waits inside the database, so a
// standby holding one would be a backend that cannot be told to stop except by closing the
// connection, and a controller that cannot be shut down cleanly is a controller that leaves a
// lock to be noticed rather than released.
func (c *Controller) waitForTheLock(ctx context.Context, conn *pgxpool.Conn) error {
	poll := c.poll()
	for {
		var got bool
		if err := conn.QueryRow(ctx, `select pg_try_advisory_lock($1)`, lockKey).Scan(&got); err != nil {
			return fmt.Errorf("controller: the lock could not be tried for: %w", err)
		}
		if got {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Fenced is the one place this package reaches the database, and everything it does is fenced.
//
// The token is checked before fn reads or writes anything, inside the same transaction, so a
// former holder is refused rather than applied late. A caller that finds db.ErrFenced has lost
// the term and should stop being a controller rather than retry: retrying is precisely what a
// partitioned former holder would do.
//
// Reads go through it too, and not as an oversight. A controller reads in order to decide, so a
// former holder reading state is as wrong as one writing it: what it would do with what it read
// is take a decision it has no right to take. Making the door one door also makes the rule one
// a test can count, which is the whole of why the rule holds.
//
// The reason is named here and not taken as an argument, because a controller touching state
// has exactly one why: it is the decider, acting across every namespace at once. The three
// purges and the collector are sweeps on the pool with reasons of their own.
func (c *Controller) Fenced(ctx context.Context, term db.Term, fn func(context.Context, *db.Wide) error) error {
	return c.pool.Installation(ctx, db.ControllerSweep, func(ctx context.Context, w *db.Wide) error {
		if err := w.Fence(ctx, term.Token); err != nil {
			return err
		}
		return fn(ctx, w)
	})
}
