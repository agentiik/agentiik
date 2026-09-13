package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

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

	// Poll is how often a standby tries the lock. It is a latency and not a correctness
	// setting: whatever it is, the standby takes over the instant a try succeeds, and the
	// only thing a shorter one buys is the time between the holder going and that.
	Poll time.Duration

	// Sweep is how often the active controller looks for work nobody told it about.
	Sweep time.Duration
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

// Lead waits for the lock, then runs fn for as long as this instance holds it.
//
// It blocks: a standby is a process sitting in here, trying the non-blocking variant on a loop
// and taking over the instant it succeeds. fn is called once, with the term that has just
// started, and Lead returns what fn returns. The lock is released when fn returns, when ctx is
// done, and by the database itself if this process dies without either.
func (c *Controller) Lead(ctx context.Context, fn func(context.Context, db.Term) error) error {
	return c.pool.Session(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		if err := c.waitForTheLock(ctx, conn); err != nil {
			return err
		}
		// Released explicitly rather than left to the session, because a Session hands
		// its connection back to the pool and a lock still held would travel with it.
		defer conn.Exec(context.WithoutCancel(ctx), `select pg_advisory_unlock($1)`, lockKey)

		term, err := c.pool.BeginTerm(ctx, c.name)
		if err != nil {
			return err
		}
		return fn(ctx, term)
	})
}

// waitForTheLock tries the non-blocking variant until it succeeds or ctx is done.
//
// The blocking variant would be shorter and is wrong here: it waits inside the database, so a
// standby holding one would be a backend that cannot be told to stop except by closing the
// connection, and a controller that cannot be shut down cleanly is a controller that leaves a
// lock to be noticed rather than released.
func (c *Controller) waitForTheLock(ctx context.Context, conn *pgxpool.Conn) error {
	poll := c.Poll
	if poll <= 0 {
		poll = time.Second
	}
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

// Write is the one place this package writes state, and everything it writes is fenced.
//
// The token is checked before fn reads or writes anything, inside the same transaction, so a
// former holder is refused rather than applied late. A caller that finds db.ErrFenced has lost
// the term and should stop being a controller rather than retry: retrying is precisely what a
// partitioned former holder would do.
//
// The reason is named here and not taken as an argument, because a controller writing state
// has exactly one why: it is the decider, acting across every namespace at once. The three
// purges and the collector are sweeps on the pool with reasons of their own.
func (c *Controller) Write(ctx context.Context, term db.Term, fn func(context.Context, *db.Wide) error) error {
	return c.pool.Installation(ctx, db.ControllerSweep, func(ctx context.Context, w *db.Wide) error {
		if err := w.Fence(ctx, term.Token); err != nil {
			return err
		}
		return fn(ctx, w)
	})
}
