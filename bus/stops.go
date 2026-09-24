package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
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
// the wire describes comes from something written against another wire.
func readStop(body []byte) (graph.Stop, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var m stopMessage
	if err := dec.Decode(&m); err != nil {
		return graph.Stop{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return graph.Stop{}, errors.New("a stop is one document, and this message carries more after it")
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
// and fn is what finds out whether this one holds the task, since a stop is published without
// knowing where the task runs: the controller "does not choose a machine", so it cannot address
// one either.
//
// It answers once the server has the subscription, and not before, so a stop published after it
// returns is one fn is handed. A runner therefore calls it before it takes anything: a task taken
// first could be stopped in the moment before anybody was listening. A credential that may not
// hear stops is answered with an error rather than a subscription that hears nothing, since a
// runner that believed it was listening would run every stopped container to its deadline.
//
// The subject keeps nothing, and neither does this. A stop published while the connection was
// down is not handed over when it comes back, and neither is one the client dropped because fn
// fell behind: the client resubscribes on its own, and the heartbeat's cancel list is the
// backstop for the gap, carrying the same meaning. So fn is handed stops one at a time, in the
// order they arrived, and one that blocks holds back those behind it: it should hand the stop on
// rather than wait out a container's grace. A stop nobody can read is said through Trouble and
// nothing is handed over for it, where handing over a guess would stop a container for a reason
// nobody gave, or the wrong one.
func (b *Bus) Stops(ctx context.Context, fn func(graph.Stop)) error {
	if fn == nil {
		return errors.New("bus: hearing stops with nothing to hand them to")
	}
	sub, err := b.conn.Subscribe(StopSubject, func(msg *nats.Msg) {
		// A message already on its way when ctx ended is not handed over: the caller
		// said it had stopped listening.
		if ctx.Err() != nil {
			return
		}
		s, err := readStop(msg.Data)
		if err != nil {
			b.report(StopSubject, fmt.Errorf("a stop could not be read: %w", err))
			return
		}
		fn(s)
	})
	if err != nil {
		return fmt.Errorf("bus: stops could not be listened for: %w", err)
	}

	// The server answers a subscription it refuses with an error of its own, which the
	// client records rather than returns, and it answers before the flush's round trip
	// comes back. So once the flush is through, a refusal is there to read. Bounded for the
	// reason Stop gives.
	flush, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	if err := b.conn.FlushWithContext(flush); err != nil {
		sub.Unsubscribe()
		return fmt.Errorf("bus: the server did not confirm it had the subscription to stops: %w", err)
	}
	if err := b.conn.LastError(); errors.Is(err, nats.ErrPermissionViolation) && strings.Contains(err.Error(), `"`+StopSubject+`"`) {
		sub.Unsubscribe()
		return fmt.Errorf("bus: this credential may not hear stops, and a runner that cannot would run every stopped container to its deadline: %w", err)
	}
	context.AfterFunc(ctx, func() { sub.Unsubscribe() })
	return nil
}
