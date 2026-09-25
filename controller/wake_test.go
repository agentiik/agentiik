package controller

import (
	"context"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// The wake-up half: a notification is a latency optimisation and the sweep is the correctness
// guarantee, so what is tested is that the sweep happens whether or not anything was notified.

// watching starts a controller watching and answers the channel its wakes arrive on.
func watching(t *testing.T, pool *db.Pool, sweep time.Duration) (<-chan Wake, context.CancelFunc) {
	t.Helper()
	c, err := New(pool, "watcher")
	if err != nil {
		t.Fatal(err)
	}
	c.Sweep = sweep

	ctx, stop := context.WithCancel(t.Context())
	wakes := make(chan Wake, 64)
	go func() {
		err := c.Watch(ctx, func(ctx context.Context, w Wake) error {
			select {
			case wakes <- w:
			default:
			}
			return nil
		})
		if err != nil && ctx.Err() == nil {
			t.Errorf("watching: %s", err)
		}
	}()
	return wakes, stop
}

func next(t *testing.T, wakes <-chan Wake, within time.Duration, what string) Wake {
	t.Helper()
	select {
	case w := <-wakes:
		return w
	case <-time.After(within):
		t.Fatalf("nothing woke the controller within %s, waiting for %s", within, what)
		return Wake{}
	}
}

// "A controller that was restarting therefore misses notifications, so it also sweeps for
// actionable work on a fixed interval." The first thing one does is sweep, because everything
// notified while it was starting is a notification it did not hear.
func TestAControllerSweepsBeforeItListens(t *testing.T) {
	pool, _ := dbtest.Open(t)
	wakes, stop := watching(t, pool, time.Hour)
	defer stop()

	first := next(t, wakes, 10*time.Second, "the first sweep")
	if !first.Swept || first.Run != "" {
		t.Fatalf("the first wake is %+v, and a controller that has just taken the term has been listening to nothing", first)
	}
}

// The notification carries a run identifier and nothing else.
func TestANotificationCarriesTheRun(t *testing.T) {
	pool, _ := dbtest.Open(t)
	wakes, stop := watching(t, pool, time.Hour)
	defer stop()
	next(t, wakes, 10*time.Second, "the first sweep")

	// The listen is issued before the first sweep, so by the time that sweep has been
	// seen the connection is listening.
	run := agk.RunID("01JMZ8V1P9C4XQ7K2N4D6F8H0A")
	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.NotifyRun(ctx, run)
	})
	if err != nil {
		t.Fatal(err)
	}

	w := next(t, wakes, 10*time.Second, "the notification")
	if w.Run != run || w.Swept {
		t.Fatalf("the notification woke the controller as %+v", w)
	}
}

// A notification is delivered only when the transaction that issued it commits, which is what
// makes the run row and the wake-up one fact rather than two.
func TestANotificationArrivesOnlyWhenItsTransactionCommits(t *testing.T) {
	pool, _ := dbtest.Open(t)
	wakes, stop := watching(t, pool, time.Hour)
	defer stop()
	next(t, wakes, 10*time.Second, "the first sweep")

	run := agk.RunID("01M2AAZ9G62NQXFAFCXKRPJEH5")
	rolledBack := make(chan struct{})
	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		if err := ns.NotifyRun(ctx, run); err != nil {
			return err
		}
		close(rolledBack)
		// Returning an error rolls the transaction back, which is the case this is
		// about: nothing was written, so nothing should have been announced.
		return context.Canceled
	})
	if err == nil {
		t.Fatal("the transaction committed")
	}
	<-rolledBack

	select {
	case w := <-wakes:
		t.Fatalf("a rolled back transaction woke the controller with %+v", w)
	case <-time.After(500 * time.Millisecond):
	}
}

// And the sweep comes round on its own, with nobody notifying anything, which is the half that
// has to work for a controller that missed everything while it was restarting.
func TestTheSweepComesRoundWithNobodyAsking(t *testing.T) {
	pool, _ := dbtest.Open(t)
	wakes, stop := watching(t, pool, 150*time.Millisecond)
	defer stop()

	for i := range 3 {
		w := next(t, wakes, 10*time.Second, "a sweep")
		if !w.Swept {
			t.Fatalf("wake %d is %+v and nothing was notified", i+1, w)
		}
	}
}

// And it comes round however busy the channel is. A sweep that waited for a quiet spell would
// never come where some run is written every few seconds, and a notification missed there would
// wait on it for ever: "the notification is a latency optimisation; the sweep is the correctness
// guarantee."
func TestTheSweepComesRoundWhileNotificationsKeepComing(t *testing.T) {
	pool, _ := dbtest.Open(t)
	wakes, stop := watching(t, pool, 300*time.Millisecond)
	defer stop()
	next(t, wakes, 10*time.Second, "the first sweep")

	busy, quiet := context.WithCancel(t.Context())
	still := make(chan struct{})
	defer func() {
		quiet()
		<-still
	}()
	go func() {
		defer close(still)
		run := agk.RunID("01JMZ8V1P9C4XQ7K2N4D6F8H0A")
		for busy.Err() == nil {
			pool.In(busy, "finance", func(ctx context.Context, ns *db.NS) error {
				return ns.NotifyRun(ctx, run)
			})
			time.Sleep(50 * time.Millisecond)
		}
	}()

	notified := 0
	deadline := time.After(10 * time.Second)
	for {
		select {
		case w := <-wakes:
			// A sweep before any notification proves nothing, and a loaded machine can
			// be slow to send the first one, so the test waits for the next.
			if w.Swept && notified > 0 {
				return
			}
			if !w.Swept {
				notified++
			}
		case <-deadline:
			t.Fatalf("%d notifications in 10s and no sweep, on a sweep of 300ms", notified)
		}
	}
}

// A payload that is not a run identifier is not worth stopping for: the sweep finds the work
// anyway, so it is reported as one.
func TestAPayloadThatIsNotARunIsSweptInstead(t *testing.T) {
	pool, super := dbtest.Open(t)
	wakes, stop := watching(t, pool, time.Hour)
	defer stop()
	next(t, wakes, 10*time.Second, "the first sweep")

	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(),
		`select pg_notify($1, $2)`, db.RunChannel, "not a run identifier"); err != nil {
		t.Fatal(err)
	}

	w := next(t, wakes, 10*time.Second, "the wake the bad payload causes")
	if !w.Swept || w.Run != "" {
		t.Fatalf("a payload that is not a run identifier woke the controller as %+v", w)
	}
}

// Watch stops when its context does, and releases the listen with it.
func TestWatchStopsWithItsContext(t *testing.T) {
	pool, _ := dbtest.Open(t)
	c, err := New(pool, "watcher")
	if err != nil {
		t.Fatal(err)
	}
	c.Sweep = 50 * time.Millisecond

	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Watch(ctx, func(context.Context, Wake) error { return nil }) }()

	time.Sleep(200 * time.Millisecond)
	stop()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Watch answered nothing when its context was cancelled")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Watch did not stop with its context")
	}
}
