package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// The reconnection backoff. A daemon that closed the stream is usually a daemon that
// restarted, and the first attempt after one should be soon rather than immediate.
const (
	reconnectFirst = 100 * time.Millisecond
	reconnectMax   = 5 * time.Second
)

// Events follows the daemon's event stream and reconnects on its own.
//
// The stream is a latency optimisation and never the correctness guarantee. The wait is
// the fast path for an exit this driver caused, and this catches one it did not: a
// container killed from outside, or an out-of-memory kill, which arrives as an oom
// before the die and which no wait reports on its own. The backstop under both is an
// inspect, so a stream that is down makes a task slow and never wrong.
//
// It reconnects with since set to the last event seen, which replays the gap rather than
// losing what fell in it. A replay is the deliberate direction: an event seen twice is
// the same exit recorded twice, and a die that was never seen is a task that hangs until
// its deadline.
//
// Both channels are closed when the context is done. The error channel carries each
// connection failure so that a caller can say the stream is down; it is not a terminal
// error, because the stream comes back.
func (c *Client) Events(ctx context.Context, since time.Time, f Filters) (<-chan Event, <-chan error) {
	events := make(chan Event)
	errs := make(chan error, 1)

	go func() {
		defer close(events)
		defer close(errs)

		backoff := reconnectFirst
		for ctx.Err() == nil {
			last, err := c.readEvents(ctx, since, f, events)
			if !last.IsZero() {
				since = last
				// A stream that delivered something was a working
				// stream, so the next failure starts its backoff over.
				backoff = reconnectFirst
			}
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				select {
				case errs <- err:
				default:
					// Nobody is reading the failures. The stream is
					// what matters and it is coming back anyway.
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > reconnectMax {
				backoff = reconnectMax
			}
		}
	}()

	return events, errs
}

// readEvents holds one connection open, answering with the moment of the last event it
// delivered so that the next connection resumes from it.
func (c *Client) readEvents(ctx context.Context, since time.Time, f Filters, out chan<- Event) (time.Time, error) {
	q := url.Values{}
	if !since.IsZero() {
		q.Set("since", timestamp(since))
	}
	f.encode(q)

	resp, err := c.do(ctx, http.MethodGet, "/events", q, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()

	var last time.Time
	dec := json.NewDecoder(resp.Body)
	for {
		var e Event
		if err := dec.Decode(&e); err != nil {
			if ctx.Err() != nil {
				return last, nil
			}
			return last, fmt.Errorf("reading the daemon event stream: %w", err)
		}
		if at := e.At(); !at.IsZero() {
			last = at
		}
		select {
		case out <- e:
		case <-ctx.Done():
			return last, nil
		}
	}
}

// timestamp writes a moment the way the events query reads one, seconds and nanoseconds
// with a dot between them.
func timestamp(t time.Time) string {
	return fmt.Sprintf("%d.%09d", t.Unix(), t.Nanosecond())
}
