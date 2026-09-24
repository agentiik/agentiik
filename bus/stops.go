package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/nats-io/nats.go"
)

// The two ends of a stop: the control plane asking for a task in flight to be stopped, and the
// runner holding it hearing so.

// StopSubject is where a stop goes. Not a stream: a stop is worth nothing to a runner that was
// not holding the task, and worth nothing later.
const StopSubject = "agentiik.stops"

// stopMessage is a stop as it travels, wire.schema.json $defs/stop: the key of the task and why.
//
// Written out here rather than left to graph.Stop's own tags, so that a field added to the
// evaluator's value does not become a field on the wire without anybody deciding it should. The
// pointers are what tell a member that is missing from one that is there: a stop with no reason
// read into a graph.StopReason would read as superseded, which is the first of the four and was
// said by nobody.
type stopMessage struct {
	Task   *agk.TaskID       `json:"task"`
	Reason *graph.StopReason `json:"reason"`
}

// encodeStop writes a stop the way it travels, once it is one a runner could read.
//
// A key that is not one, or a reason with no spelling, is refused here rather than published,
// since a stop the runner cannot read is a container that runs to its deadline while the control
// plane believes it asked.
func encodeStop(s graph.Stop) ([]byte, error) {
	if err := s.Task.Validate(); err != nil {
		return nil, fmt.Errorf("a stop names a task by its key: %w", err)
	}
	return json.Marshal(stopMessage{Task: &s.Task, Reason: &s.Reason})
}

// readStop reads one stop off the bus, as the wire describes it.
//
// Closed, as the document is and as readResult is, for the same reason: a stop saying more than
// the wire describes comes from something written against another wire. Closed further than
// readResult, down to the spelling of the two names: encoding/json reads "Reason" as reason, and
// a stop carrying both, one cancelled and one deadline, would be read as whichever came last,
// which decides whether the task ends cancelled or timed_out. The schema refuses such a stop,
// and so does this.
func readStop(body []byte) (graph.Stop, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil {
		return graph.Stop{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return graph.Stop{}, errors.New("a stop is one document, and this message carries more after it")
	}
	for name := range fields {
		if name != "task" && name != "reason" {
			return graph.Stop{}, fmt.Errorf("a stop carries task and reason, spelled so, and this one carries %q", name)
		}
	}
	var m stopMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return graph.Stop{}, err
	}
	if m.Task == nil {
		return graph.Stop{}, errors.New("a stop names no task")
	}
	if err := m.Task.Validate(); err != nil {
		return graph.Stop{}, fmt.Errorf("task: %w", err)
	}
	if m.Reason == nil {
		return graph.Stop{}, fmt.Errorf("the stop of %s gives no reason, and the reason decides whether the task ends timed_out or cancelled", *m.Task)
	}
	return graph.Stop{Task: *m.Task, Reason: *m.Reason}, nil
}

// Stop asks for a task in flight to be stopped.
//
// A stop is not a queue message. The task it names is held by a runner that already took it, so
// putting a stop on the work queue would be putting it where nobody holding that task is looking
// and where a runner with room would take it as work. It goes out as a plain subject a runner
// hears for as long as it holds anything, which Stops is, and which is the one thing the bus does
// that is not work distribution.
func (b *Bus) Stop(ctx context.Context, s graph.Stop) error {
	body, err := encodeStop(s)
	if err != nil {
		return fmt.Errorf("bus: the stop for %s could not be written: %w", s.Task, err)
	}
	if err := b.conn.Publish(StopSubject, body); err != nil {
		return fmt.Errorf("bus: the stop for %s could not be published: %w", s.Task, err)
	}
	// Flushed, because a plain publish is fire and forget and a stop that never left the
	// buffer is a container that runs to its deadline. The flush is given a bound of its
	// own: the client refuses a context with no deadline, and a caller passing one that has
	// none is asking for a stop rather than asking to wait for ever.
	flush, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	if err := b.conn.FlushWithContext(flush); err != nil {
		return fmt.Errorf("bus: the stop for %s was published and not flushed: %w", s.Task, err)
	}
	return nil
}

// Stops hands fn every stop published from now until ctx is done, over this bus's own connection.
//
// It is Stop's other end, and a runner's: the connection OpenRunner opened is the one its
// credential allows to hear StopSubject, and a runner has no other. Every runner hears every stop,
// and fn is what finds out whether this one holds the task. A stop names a key and no runner,
// because the evaluator that decides it knows nothing of runners, and a dispatch taken and not yet
// redeemed is bound to none, so the one place its holder is sure to be listening is a subject every
// runner hears.
//
// It answers once the server has the subscription, and not before, so a stop published after it
// returns is one fn is handed. A runner therefore calls it before it takes anything: a task taken
// first could be stopped in the moment before anybody was listening. A credential that may not
// hear stops is answered with an error rather than a subscription that hears nothing, since a
// runner that believed it was listening would run every stopped container to its deadline.
//
// The subject keeps nothing, and neither does this. A stop published while the connection was
// down is not handed over when it comes back, and neither is one the client dropped because fn
// fell behind, which is said through Trouble: the client resubscribes on its own, and the
// heartbeat's cancel list is the backstop for the gap, carrying the same meaning. So fn is handed
// stops one at a time, in the order they arrived, and one that blocks holds back those behind it:
// it should hand the stop on rather than wait out a container's grace. A stop nobody can read is
// said through Trouble and nothing is handed over for it, where handing over a guess would stop a
// container for a reason nobody gave, or the wrong one.
func (b *Bus) Stops(ctx context.Context, fn func(graph.Stop)) error {
	if fn == nil {
		return errors.New("bus: hearing stops with nothing to hand them to")
	}
	// A subscription read by hand rather than through a handler, because it is the one kind
	// the client keeps the server's refusal on, which is what the connection was opened
	// with PermissionErrOnSubscribe for. The connection's last error is no place to look: any
	// later refusal on the same connection, a result published under another runner's name,
	// overwrites it, and a refusal to publish a stop reads much like a refusal to hear one.
	sub, err := b.conn.SubscribeSync(StopSubject)
	if err != nil {
		return fmt.Errorf("bus: stops could not be listened for: %w", err)
	}

	// The server answers a subscription it refuses with an error of its own, and it answers
	// before the flush's round trip comes back, so once the flush is through the client has
	// recorded a refusal against the subscription. Bounded for the reason Stop gives.
	flush, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	if err := b.conn.FlushWithContext(flush); err != nil {
		sub.Unsubscribe()
		return fmt.Errorf("bus: the server did not confirm it had the subscription to stops: %w", err)
	}
	// Reading with no wait answers the recorded refusal before it looks for a message, and a
	// stop that already arrived is kept for fn rather than lost to the look.
	first, err := sub.NextMsg(0)
	switch {
	case errors.Is(err, nats.ErrPermissionViolation):
		sub.Unsubscribe()
		return fmt.Errorf("bus: this credential may not hear stops, and a runner that cannot would run every stopped container to its deadline: %w", err)
	case err != nil && !errors.Is(err, nats.ErrTimeout):
		sub.Unsubscribe()
		return fmt.Errorf("bus: stops could not be listened for: %w", err)
	}
	go b.hear(ctx, sub, first, fn)
	return nil
}

// hear hands fn each stop sub holds, first included where there is one, until ctx is done or the
// connection is closed.
func (b *Bus) hear(ctx context.Context, sub *nats.Subscription, first *nats.Msg, fn func(graph.Stop)) {
	defer sub.Unsubscribe()
	// said is what was last said through Trouble about the subscription, so that one that
	// keeps failing, refused again after a reconnect, is said once rather than every second.
	var said string
	msg := first
	for {
		if msg != nil {
			said = ""
			s, err := readStop(msg.Data)
			if err != nil {
				b.report(StopSubject, fmt.Errorf("a stop could not be read: %w", err))
			} else {
				fn(s)
			}
		}
		// Asked of ctx before anything is taken, so a stop that arrived while fn was busy
		// and after ctx ended is not handed over: the caller said it had stopped listening.
		var err error
		msg, err = sub.NextMsgWithContext(ctx)
		switch {
		case err == nil:
		case ctx.Err() != nil, errors.Is(err, nats.ErrConnectionClosed), errors.Is(err, nats.ErrBadSubscription):
			return
		case errors.Is(err, nats.ErrSlowConsumer):
			b.report(StopSubject, errors.New("stops were dropped because they were handed over more slowly than they arrived, and the heartbeat's cancel list is all that will carry them"))
		default:
			// Nothing else is expected, and nothing else goes away by asking again at
			// once, so it is said and asked about again a second later.
			if err.Error() != said {
				said = err.Error()
				b.report(StopSubject, fmt.Errorf("stops are not being heard: %w", err))
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}
