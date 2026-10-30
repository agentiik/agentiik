package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// The wake-up half: a notification is a latency optimisation and the sweep is the correctness
// guarantee, so what is tested is that the sweep happens whether or not anything was notified.

// watching starts a controller watching, with no clock to wait on, and answers the channel its
// wakes arrive on.
func watching(t *testing.T, pool *db.Pool, sweep time.Duration) (<-chan Wake, context.CancelFunc) {
	t.Helper()
	_, wakes, stop := watchingClocks(t, pool, sweep, nil)
	return wakes, stop
}

// watchingClocks starts a controller watching, asking due for the next clock, and answers the
// controller and the channel its wakes arrive on.
func watchingClocks(t *testing.T, pool *db.Pool, sweep time.Duration, due func(context.Context, time.Time) (time.Time, error)) (*Controller, <-chan Wake, context.CancelFunc) {
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
		}, due)
		if err != nil && ctx.Err() == nil {
			t.Errorf("watching: %s", err)
		}
	}()
	return c, wakes, stop
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
	go func() { done <- c.Watch(ctx, func(context.Context, Wake) error { return nil }, nil) }()

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

// The clock half: a retry's backoff and a root timeout are moments nothing notifies, and the
// controller wakes at them rather than at the sweep after them, which comes round every ten seconds
// and once read a backoff of two seconds as anything up to twelve.

// at is a due that answers one moment, while it lies after the moment it is asked from, as the
// database answers the earliest clock past the last pass.
func at(when func() time.Time) func(context.Context, time.Time) (time.Time, error) {
	return func(_ context.Context, after time.Time) (time.Time, error) {
		if w := when(); w.After(after) {
			return w, nil
		}
		return time.Time{}, nil
	}
}

func TestARunDueIsWokenAtItsInstantRatherThanAtTheSweep(t *testing.T) {
	pool, _ := dbtest.Open(t)
	due := time.Now().Add(1500 * time.Millisecond)
	_, wakes, stop := watchingClocks(t, pool, 10*time.Second, at(func() time.Time { return due }))
	defer stop()
	next(t, wakes, 10*time.Second, "the first sweep")

	w := next(t, wakes, 10*time.Second, "the run due")
	woke := time.Now()
	if !w.Due || w.Swept || w.Run != "" {
		t.Fatalf("the controller woke as %+v, and what came round was a run's clock", w)
	}
	if woke.Before(due) {
		t.Fatalf("the controller woke %s before the clock came round", due.Sub(woke))
	}
	if late := woke.Sub(due); late > 1500*time.Millisecond {
		t.Errorf("the controller woke %s after the clock came round, on a sweep of 10s", late)
	}
}

// A result is decided on a goroutine of its own and notifies nothing, so a decision that sets a
// clock says so, and the watch asks again rather than wait out the sweep it was waiting for.
func TestAClockADecisionSetsIsWaitedFor(t *testing.T) {
	pool, _ := dbtest.Open(t)
	var mu sync.Mutex
	var due time.Time
	c, wakes, stop := watchingClocks(t, pool, time.Hour, at(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return due
	}))
	defer stop()
	next(t, wakes, 10*time.Second, "the first sweep")

	mu.Lock()
	due = time.Now().Add(300 * time.Millisecond)
	set := due
	mu.Unlock()
	c.clockSet(set)

	w := next(t, wakes, 10*time.Second, "the clock the decision set")
	if !w.Due {
		t.Fatalf("the controller woke as %+v, waiting on a sweep of an hour and a clock set at %s", w, set)
	}
	if late := time.Since(set); late < 0 || late > 1500*time.Millisecond {
		t.Errorf("the controller woke %s after the clock came round", late)
	}
}

// A run whose clock came round and stayed there, one a pass could not decide, is asked for from
// after the pass that looked at it, so it is not answered again at once: one pass, and then the
// sweep, rather than a loop on one run.
func TestAClockThatStaysDueIsLeftToTheSweep(t *testing.T) {
	pool, _ := dbtest.Open(t)
	var mu sync.Mutex
	var stuck time.Time
	c, wakes, stop := watchingClocks(t, pool, time.Hour, at(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return stuck
	}))
	defer stop()
	next(t, wakes, 10*time.Second, "the first sweep")
	// Set once the first sweep has begun, so that the clock lies after it however long the
	// watch took to start.
	mu.Lock()
	stuck = time.Now().Add(200 * time.Millisecond)
	set := stuck
	mu.Unlock()
	c.clockSet(set)

	if w := next(t, wakes, 10*time.Second, "the run due"); !w.Due {
		t.Fatalf("the controller woke as %+v", w)
	}
	select {
	case w := <-wakes:
		t.Fatalf("a clock the last pass already looked at woke the controller again, as %+v", w)
	case <-time.After(time.Second):
	}
}

// And a due that answered the same moment again and again, which would be a fault, costs ten
// passes a second and not every pass the database can answer.
func TestPassesOverTheRunsDueAreATenthOfASecondApart(t *testing.T) {
	pool, _ := dbtest.Open(t)
	past := time.Now().Add(-time.Minute)
	_, wakes, stop := watchingClocks(t, pool, time.Hour, func(context.Context, time.Time) (time.Time, error) {
		return past, nil
	})
	defer stop()
	next(t, wakes, 10*time.Second, "the first sweep")

	passes := 0
	for over := time.After(time.Second); ; {
		select {
		case w := <-wakes:
			if !w.Due {
				t.Fatalf("the controller woke as %+v", w)
			}
			passes++
			continue
		case <-over:
		}
		break
	}
	if passes == 0 || passes > 12 {
		t.Errorf("%d passes in a second over a clock answered again and again, want about 10", passes)
	}
}

// retryingSoon retries normalize once after a failure, a second and a half later, which is well
// inside the sweep's ten seconds.
const retryingSoon = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: normalize, port: ok } }
steps:
  normalize:
    image: ` + theImage + `
    retry:
      max: 1
      on: [failed]
      backoff: { type: exponential, base: 1500ms, max: 1500ms }
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
`

// The whole of it, on a real clock: a failure answered on a goroutine other than the watch's, a
// backoff of a second and a half counted from the failed attempt's end, and the retry handed out
// then, on the sweep of ten seconds a controller runs with.
func TestARetryGoesOutAtTheEndOfItsBackoff(t *testing.T) {
	core, q, pool, _ := decidingOn(t, retryingSoon)
	core.now = func() time.Time { return time.Now().UTC() }
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.taken()
	if len(first) != 1 || first[0].Attempt != 1 {
		t.Fatalf("the first pass published %+v", first)
	}
	if core.controller.Sweep != 10*time.Second {
		t.Fatalf("the controller sweeps every %s, and this is about a backoff shorter than the sweep a controller runs with", core.controller.Sweep)
	}

	ctx, stop := context.WithCancel(t.Context())
	watched := make(chan error, 1)
	swept := make(chan struct{})
	var once sync.Once
	go func() {
		watched <- core.controller.Watch(ctx, func(ctx context.Context, w Wake) error {
			defer once.Do(func() { close(swept) })
			return core.Wake(ctx, w)
		}, core.NextWake)
	}()
	defer func() {
		stop()
		<-watched
	}()
	// The failure comes once the watch has swept and is waiting, as a result comes to a
	// controller that has led for a while, so that what wakes it is the clock and not the
	// sweep it starts with.
	select {
	case <-swept:
	case <-time.After(10 * time.Second):
		t.Fatal("the watch did not sweep")
	}

	failedAt := time.Now().UTC()
	core.answer(t, failed(first[0], 1, failedAt))
	for give := time.Now().Add(8 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if retry := q.taken(); len(retry) > 0 {
			took := time.Since(failedAt)
			if len(retry) != 1 || retry[0].Attempt != 2 {
				t.Fatalf("the retry went out as %+v", retry)
			}
			if took < 1500*time.Millisecond-time.Microsecond || took > 3*time.Second {
				t.Errorf("the retry of a backoff of 1.5s went out %s after the failure", took)
			}
			return
		}
		if time.Now().After(give) {
			t.Fatal("no retry 8s after a failure retried after 1.5s")
		}
	}
}
