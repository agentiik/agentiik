package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/agentiik/agentiik/bus"
)

// Reaching the task bus.
//
// "The runner asks the API for a short-lived bus token whenever the previous one nears expiry. Bus
// credentials are therefore never at rest on a runner host." So nothing on the host names the bus:
// the API answers where it is and what to present to it, for the pool the runner credential
// belongs to, and the agent holds the answer in memory and nowhere else.

// busTokenPath is the route, as the router spells it.
const busTokenPath = "/api/v1/bus/token"

// busToken is the API's answer: a credential of the kind bus.Kind names, the consumer of the pool
// the runner joined and the stream it reads from, and when the credential stops working.
type busToken struct {
	Kind      string `json:"kind"`
	URL       string `json:"url"`
	JWT       string `json:"jwt"`
	Seed      string `json:"seed"`
	Consumer  string `json:"consumer"`
	Stream    string `json:"stream"`
	ExpiresAt string `json:"expires_at"`
}

// BusCredentials asks the API for this runner's bus credential.
//
// It is held to the runner that asks: of the kind this runner speaks, for the stream it takes work
// from and the consumer of the pool it joined, and not yet expired. A pool other than the one join
// wrote is refused rather than followed, since the API answers the pool the credential opened and
// runner.env the pool join was answered, and two that disagree are an installation to look at
// rather than one to take work from.
func (c *Client) BusCredentials(ctx context.Context, pool string) (bus.Credentials, error) {
	var t busToken
	if err := c.Do(ctx, http.MethodPost, busTokenPath, nil, &t); err != nil {
		return bus.Credentials{}, err
	}
	expires, err := time.Parse(time.RFC3339Nano, t.ExpiresAt)
	switch {
	case t.Kind != bus.Kind:
		return bus.Credentials{}, fmt.Errorf("runner: POST %s: the bus credential is of the kind %q, and this runner speaks %s", busTokenPath, t.Kind, bus.Kind)
	case t.URL == "" || t.JWT == "" || t.Seed == "":
		return bus.Credentials{}, fmt.Errorf("runner: POST %s: the bus credential names no bus, or nothing to present to it", busTokenPath)
	case bus.CheckURL(t.URL) != nil:
		// Refused here rather than when the connection is opened, which would be taken for
		// a bus not reached yet and asked again for ever: the API answers the same address
		// every time.
		return bus.Credentials{}, fmt.Errorf("runner: POST %s: %w", busTokenPath, bus.CheckURL(t.URL))
	case t.Stream != bus.Stream:
		return bus.Credentials{}, fmt.Errorf("runner: POST %s: the bus credential is for the stream %q, and a runner takes work from %s", busTokenPath, t.Stream, bus.Stream)
	case t.Consumer != bus.Durable(pool):
		return bus.Credentials{}, fmt.Errorf("runner: POST %s: the bus credential takes from the consumer %q, and this runner joined pool %s", busTokenPath, t.Consumer, pool)
	case err != nil:
		return bus.Credentials{}, fmt.Errorf("runner: POST %s: the bus credential expires at %q, which is not an RFC 3339 instant", busTokenPath, t.ExpiresAt)
	case !expires.After(time.Now()):
		return bus.Credentials{}, fmt.Errorf("runner: POST %s: the bus credential expired at %s, before it arrived", busTokenPath, t.ExpiresAt)
	}
	return bus.Credentials{Kind: t.Kind, URL: t.URL, JWT: t.JWT, Seed: t.Seed, ExpiresAt: expires}, nil
}

// OpenBus asks for this runner's bus credential and connects with it, asking again for as long as
// the API does not answer, and answers the connection and when its credential expires.
//
// An API that does not answer is one that may, and an agent started before it, as a host booting
// beside the control plane is, waits for it rather than failing its start into a restart loop. An
// answer that refuses, the credential opening nothing or a bus of another kind, is the same on
// every try and ends the wait. A context that ends answers nil and no error: the agent was
// stopped, and that is not a failure to say.
func OpenBus(ctx context.Context, c *Client, cfg Config, say func(string)) (*bus.Bus, time.Time, error) {
	wait := retryFirst
	for {
		b, expires, err := dialBus(ctx, c, cfg)
		if err == nil {
			return b, expires, nil
		}
		if ctx.Err() != nil {
			return nil, time.Time{}, nil
		}
		if !errors.Is(err, ErrUnavailable) {
			return nil, time.Time{}, err
		}
		say(fmt.Sprintf("the task bus is not reached yet, and is tried again in %s: %s", wait, err))
		if !sleep(ctx, wait) {
			return nil, time.Time{}, nil
		}
		wait = min(2*wait, retryMost)
	}
}

// dialBus asks for this runner's bus credential once and connects with it.
func dialBus(ctx context.Context, c *Client, cfg Config) (*bus.Bus, time.Time, error) {
	credentials, err := c.BusCredentials(ctx, cfg.Pool)
	if err != nil {
		return nil, time.Time{}, err
	}
	// A bus that cannot be reached now is one that may be, as an API that does not answer is,
	// and the credential is good for an hour.
	b, err := bus.OpenRunner(bus.Options{URL: credentials.URL, Name: cfg.Runner, Credentials: &credentials})
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("runner: %w: %w", ErrUnavailable, err)
	}
	return b, credentials.ExpiresAt, nil
}

// Renewing the bus credential.
//
// A bus credential runs out, within the hour and sooner where the runner credential it was asked
// with or a revocation's grace ends first, and a NATS client whose credential ran out asks the
// server again for ever with the one it holds. The client resubscribes on its own after a
// reconnect, so what has to be replaced is the connection: at three quarters of the credential's
// life the agent asks for a new one, opens a second connection with it and listens for stops on
// it, and only then moves what it takes, publishes and says onto that one. The connection it
// replaces is closed once nothing it handed over can still need it, so the two overlap and no
// moment passes with nobody listening for stops, taking work or able to answer for it.

// busConn is one connection to the task bus, which is bus.Bus.
type busConn interface {
	Queue
	Publisher
	ProgressPublisher
	Close()
}

// dialer asks for a bus credential and connects with it, listening for stops on the connection
// before it answers, and answers when the credential expires.
type dialer func(ctx context.Context) (busConn, time.Time, error)

// taskBus is the task bus as the agent holds it: one connection at a time, replaced ahead of its
// credential's expiry. It is the loop's Queue, the kept results' Publisher and the progress's
// ProgressPublisher, so that each of them reaches whichever connection is current.
type taskBus struct {
	dial dialer
	say  func(string)
	now  func() time.Time

	// linger is how long a replaced connection is kept past the last moment a take on it could
	// still hand a message over, bus.AckWait where it is zero.
	linger time.Duration

	mu       sync.Mutex
	current  *link
	closed   bool
	closing  chan struct{}
	retiring sync.WaitGroup
}

// link is one connection, and what the agent knows of it.
type link struct {
	conn busConn

	// received is when its credential arrived, by the agent's clock, and expires when the
	// credential runs out, by the API's.
	received, expires time.Time

	// busyUntil is the last moment a take made on it may still hand a message over.
	busyUntil time.Time
}

// renewAt is when the connection's credential is renewed: at three quarters of its life, which
// leaves a quarter of an hour of the usual hour for an API that does not answer at once, and not
// sooner than a second after it arrived, so that a credential cut short by a rotate_by or a grace
// about to end is not asked for again in a loop.
func (l *link) renewAt() time.Time {
	at := l.received.Add(3 * l.expires.Sub(l.received) / 4)
	if soonest := l.received.Add(retryFirst); at.Before(soonest) {
		return soonest
	}
	return at
}

func newTaskBus(conn busConn, expires time.Time, dial dialer, say func(string)) *taskBus {
	if say == nil {
		say = func(string) {}
	}
	t := &taskBus{dial: dial, say: say, now: time.Now, closing: make(chan struct{})}
	t.current = &link{conn: conn, received: t.now(), expires: expires}
	return t
}

// on is the current connection.
func (t *taskBus) on() busConn {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.current.conn
}

// Take takes from the current connection, and notes how long the take may go on handing messages
// over on it.
func (t *taskBus) Take(ctx context.Context, pool string, batch int, wait time.Duration) ([]bus.Taken, error) {
	t.mu.Lock()
	l := t.current
	if until := t.now().Add(wait); until.After(l.busyUntil) {
		l.busyUntil = until
	}
	t.mu.Unlock()
	return l.conn.Take(ctx, pool, batch, wait)
}

// Ended publishes the ending on the current connection. The message is acknowledged on the one it
// was taken on, which is kept open for that.
func (t *taskBus) Ended(ctx context.Context, taken bus.Taken, ending bus.TaskResult) error {
	return t.on().Ended(ctx, taken, ending)
}

func (t *taskBus) Report(ctx context.Context, r bus.TaskResult) error {
	return t.on().Report(ctx, r)
}

func (t *taskBus) Progress(ctx context.Context, p bus.TaskProgress) error {
	return t.on().Progress(ctx, p)
}

// Keep renews the connection ahead of its credential's expiry until ctx ends, and answers nil then.
//
// A renewal that fails is asked again, from a second doubling to thirty, for as long as it takes:
// the connection it would replace goes on working until its credential runs out, and after that
// nothing is taken or published until a renewal succeeds, which a result kept under the work root
// and a take asked again both wait out. A bus credential refused 401 is the runner credential
// refused, and ends Keep with an error saying to join again, as the heartbeat does.
func (t *taskBus) Keep(ctx context.Context) error {
	wait := retryFirst
	for {
		t.mu.Lock()
		l := t.current
		t.mu.Unlock()
		if !sleep(ctx, l.renewAt().Sub(t.now())) {
			return nil
		}
		conn, expires, err := t.dial(ctx)
		switch {
		case err == nil:
			t.replace(conn, expires)
			wait = retryFirst
			continue
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, ErrCredentialRefused):
			return fmt.Errorf("runner: %s: %w", joinAgain, err)
		}
		t.say(fmt.Sprintf("the bus credential could not be renewed, and is asked for again in %s; the one held runs out at %s: %s", wait, l.expires.UTC().Format(time.RFC3339), err))
		if !sleep(ctx, wait) {
			return nil
		}
		wait = min(2*wait, retryMost)
	}
}

// replace makes conn the current connection, and closes the one it replaces once nothing can
// still need it: bus.AckWait past the last moment a take on it could hand a message over, since a
// message taken on a connection is acknowledged on that connection, and one not acknowledged
// within AckWait has gone round the pool again whatever it is answered. No later than its
// credential runs out, when the server drops it anyway.
func (t *taskBus) replace(conn busConn, expires time.Time) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		conn.Close()
		return
	}
	old := t.current
	t.current = &link{conn: conn, received: t.now(), expires: expires}
	linger := t.linger
	if linger <= 0 {
		linger = bus.AckWait
	}
	until := old.busyUntil.Add(linger)
	if old.expires.Before(until) {
		until = old.expires
	}
	t.retiring.Add(1)
	t.mu.Unlock()

	go func() {
		defer t.retiring.Done()
		timer := time.NewTimer(until.Sub(t.now()))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-t.closing:
		}
		old.conn.Close()
	}()
}

// Close closes the current connection and every one still kept for what it handed over, and
// returns once they are closed.
func (t *taskBus) Close() {
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		close(t.closing)
		t.current.conn.Close()
	}
	t.mu.Unlock()
	t.retiring.Wait()
}
