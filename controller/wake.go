package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/jackc/pgx/v5/pgconn"
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

	// Due is true where a run's clock came round before the sweep did: a retry's backoff or a
	// root timeout, which nothing notifies, since nothing happens at that instant but the
	// instant itself. What is due is every run whose clock has come round, so a caller treats
	// it as a sweep that has nothing to notice but the clocks.
	Due bool
}

// duePause is the least time between the starts of two passes over the runs due.
//
// A pass decides every run due by the moment it looks, so passes closer together than this would
// each find one run where one pass finds them all, and a decision that left its run due again at
// once, which would be a fault, costs ten passes a second rather than every pass the database can
// answer. A tenth of a second is late by nothing anybody waiting on a retry would notice.
const duePause = 100 * time.Millisecond

// Watch calls on for every run the API notifies about, on every sweep, and whenever the clock of a
// run comes round before the next sweep does.
//
// due answers the earliest moment a run's clock comes round after the moment it is given, zero
// where none does, and is asked again after every call to on and whenever a decision of this
// process sets a clock, since a decision may set an earlier one than the one waited for: a result
// is decided on a goroutine of its own and notifies nothing. The moment given is the start of the
// last pass over everything due, so that a run that pass could not move is left to the sweep
// rather than answered again at once. A nil due is a watch that waits on the sweep alone.
//
// It blocks until ctx is done or on or due returns an error, and it takes a session of its own:
// LISTEN is a property of a connection, and a pooled connection handed back between transactions
// would carry the listen to whoever got it next.
//
// The first thing it does is sweep. A controller that has just taken the term has by definition
// been listening to nothing, so everything the API notified while it was starting is a
// notification it did not hear, and the sweep is what makes that cost latency instead of a run.
// The clocks need nothing of the kind: they are rows, and the first asking after the first sweep
// reads them whoever wrote them, this term or the last.
func (c *Controller) Watch(ctx context.Context, on func(context.Context, Wake) error, due func(context.Context, time.Time) (time.Time, error)) error {
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

		// The sweep keeps a clock of its own, and neither a notification nor a run due moves
		// it. A wait that started over on every notification would sweep only after a quiet
		// spell, and on an installation where some run is written every few seconds that
		// spell never comes: a notification missed there would wait on it for ever, and the
		// sweep is the correctness guarantee only if it happens whatever else is heard.
		// passed is when the last pass over everything due began, a sweep's or a clock's,
		// which is the moment due is asked from.
		var next, passed time.Time
		pass := func(w Wake) error {
			passed = time.Now()
			if err := on(ctx, w); err != nil {
				return err
			}
			if w.Swept {
				next = time.Now().Add(sweep)
			}
			return nil
		}
		if err := pass(Wake{Swept: true}); err != nil {
			return err
		}

		for {
			// Drained before asking, so that a clock set while on ran, which the answer
			// already holds, does not cut the wait short for nothing. One set after the
			// asking is still there to cut it.
			select {
			case <-c.clocks:
			default:
			}
			var wake time.Time
			if due != nil {
				at, err := due(ctx, passed)
				if err != nil {
					return err
				}
				if !at.IsZero() {
					wake = later(at, passed.Add(duePause))
				}
			}
			until := next
			if !wake.IsZero() && wake.Before(until) {
				until = wake
			}

			note, err := c.wait(ctx, conn, until)
			swept := false
			switch {
			case err == nil:
				run := agk.RunID(note.Payload)
				if err := run.Validate(); err != nil {
					// A payload that is not a run identifier is not worth
					// stopping for: the sweep finds the work anyway. It is
					// worth reporting as a sweep so the caller does something.
					if err := pass(Wake{Swept: true}); err != nil {
						return err
					}
					swept = true
					break
				}
				if err := on(ctx, Wake{Run: run}); err != nil {
					return err
				}
			case ctx.Err() != nil:
				return ctx.Err()
			case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
				// The wait came to its end, or a decision set a clock and cut it short
				// to look again.
			default:
				return fmt.Errorf("controller: the listening connection failed: %w", err)
			}
			now := time.Now()
			switch {
			case swept:
			case !now.Before(next):
				if err := pass(Wake{Swept: true}); err != nil {
					return err
				}
			case !wake.IsZero() && !now.Before(wake):
				if err := pass(Wake{Due: true}); err != nil {
					return err
				}
			}
		}
	})
}

// wait waits on conn for a notification until until, and is cut short by a decision setting a
// clock, which answers context.Canceled.
//
// The watcher is gone by the time this returns, so that it cannot take a signal meant for the next
// wait. A signal it took after the wait had ended is lost, and nothing is lost with it: it was sent
// after its decision committed, and the asking that follows every wait reads that decision.
func (c *Controller) wait(ctx context.Context, conn *pgxpool.Conn, until time.Time) (*pgconn.Notification, error) {
	waiting, stop := context.WithDeadline(ctx, until)
	defer stop()
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-c.clocks:
			stop()
		case <-waiting.Done():
		}
	}()
	note, err := conn.Conn().WaitForNotification(waiting)
	stop()
	<-watched
	return note, err
}

// clockSet tells Watch that a decision has set a run's clock at at, once it is written, so that it
// asks again for the earliest one. Nothing is told of a run whose clock waits on nothing, and
// nothing blocks: one signal waiting is as good as many, since what Watch does with it is ask.
func (c *Controller) clockSet(at time.Time) {
	if at.IsZero() {
		return
	}
	select {
	case c.clocks <- struct{}{}:
	default:
	}
}

// later is the later of two moments.
func later(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}
