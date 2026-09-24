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

// Wake is why the controller woke up.
type Wake struct {
	// Run is the run the API named, and is empty on a sweep. A caller that treats the two
	// the same is correct and slower, which is the right way round: the notification is a
	// latency optimisation and the sweep is the correctness guarantee, so the sweep has to
	// find everything a notification would have.
	Run agk.RunID

	// Swept is true where nobody asked and the interval came round.
	Swept bool
}

// Watch calls on for every run the API notifies about, and on every sweep.
//
// It blocks until ctx is done or on returns an error, and it takes a session of its own: LISTEN
// is a property of a connection, and a pooled connection handed back between transactions would
// carry the listen to whoever got it next.
//
// The first thing it does is sweep. A controller that has just taken the term has by definition
// been listening to nothing, so everything the API notified while it was starting is a
// notification it did not hear, and the sweep is what makes that cost latency instead of a run.
func (c *Controller) Watch(ctx context.Context, on func(context.Context, Wake) error) error {
	if on == nil {
		return errors.New("controller: Watch with nothing to call")
	}
	sweep := c.Sweep
	if sweep <= 0 {
		sweep = 10 * time.Second
	}

	return c.pool.Session(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		if _, err := conn.Exec(ctx, `listen `+db.RunChannel); err != nil {
			return fmt.Errorf("controller: the run channel could not be listened to: %w", err)
		}
		defer func() {
			unlisten, stop := context.WithTimeout(context.WithoutCancel(ctx), cleanupWithin)
			defer stop()
			conn.Exec(unlisten, `unlisten `+db.RunChannel)
		}()

		if err := on(ctx, Wake{Swept: true}); err != nil {
			return err
		}

		for {
			// The wait is bounded by the sweep, so a quiet installation still sweeps
			// and a busy one still sweeps on time. A notification arriving inside the
			// window ends the wait early, which is the whole of what the channel buys.
			waiting, stop := context.WithTimeout(ctx, sweep)
			note, err := conn.Conn().WaitForNotification(waiting)
			stop()

			switch {
			case err == nil:
				run := agk.RunID(note.Payload)
				if err := run.Validate(); err != nil {
					// A payload that is not a run identifier is not worth
					// stopping for: the sweep finds the work anyway. It is
					// worth reporting as a sweep so the caller does something.
					if err := on(ctx, Wake{Swept: true}); err != nil {
						return err
					}
					continue
				}
				if err := on(ctx, Wake{Run: run}); err != nil {
					return err
				}
			case ctx.Err() != nil:
				return ctx.Err()
			case errors.Is(err, context.DeadlineExceeded):
				if err := on(ctx, Wake{Swept: true}); err != nil {
					return err
				}
			default:
				return fmt.Errorf("controller: the listening connection failed: %w", err)
			}
		}
	})
}
