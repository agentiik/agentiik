package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/controller"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// The two ends that are not the controller publishing: a runner taking work, and a result coming
// back.

// Results is the stream a task result travels back on.
//
// A second stream rather than a second subject on the first, because the two have different
// consumers and different lifetimes: tasks are taken by many runners filtered by pool, results
// are taken by the one active controller. WorkQueue on both, for the same reason.
const Results = "AGENTIIK_RESULTS"

// ResultSubject is where a result goes.
const ResultSubject = "agentiik.results"

// Taken is one task message a runner pulled, and the two things it can say about it afterwards.
//
// Acknowledging is what removes it from the queue, and under WorkQueue retention that is what
// removes it from the stream: "a message is removed as soon as it has been consumed". So a
// runner acknowledges when the work is over rather than when it arrives, and a runner that dies
// holding one has the task redelivered, which is what at-least-once means and what the
// idempotency key makes survivable.
type Taken struct {
	Task TaskMessage

	msg jetstream.Msg
}

// Done removes the task from the queue.
func (t Taken) Done() error {
	if t.msg == nil {
		return errors.New("bus: acknowledging a task that came from nowhere")
	}
	return t.msg.Ack()
}

// Again puts it back for somebody else, which is what a runner says when it took a task it
// cannot run: its labels changed, it is draining, or it has no room after all.
func (t Taken) Again() error {
	if t.msg == nil {
		return errors.New("bus: returning a task that came from nowhere")
	}
	return t.msg.Nak()
}

// Working says the task is still in hand, which holds off redelivery for another interval.
//
// It is not the heartbeat. The heartbeat is a request to the API "listing the idempotency keys
// it currently holds" and is what liveness is read from; this only tells the bus not to hand the
// same message to somebody else while a container is legitimately still running.
func (t Taken) Working() error {
	if t.msg == nil {
		return errors.New("bus: reporting on a task that came from nowhere")
	}
	return t.msg.InProgress()
}

// Take pulls up to batch tasks for one pool, waiting up to wait for them.
//
// The consumer is durable and named after the pool, which is what makes several runners of one
// pool share the work: they are one consumer with many clients, so a task goes to whichever asks
// first. It is a pull consumer because "a runner asks for a batch of tasks when it has room,
// which makes distribution naturally proportional to each host's real capacity without the
// controller having to model load".
func (b *Bus) Take(ctx context.Context, pool string, batch int, wait time.Duration) ([]Taken, error) {
	if err := validPool(pool); err != nil {
		return nil, fmt.Errorf("bus: %w", err)
	}
	if batch < 1 {
		return nil, fmt.Errorf("bus: a runner asking for %d tasks", batch)
	}
	consumer, err := b.stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "pool-" + pool,
		Description:   "Every runner of the " + pool + " pool, sharing one queue.",
		FilterSubject: Subject(pool),
		// Explicit, because acknowledging is what says the work is over rather than
		// what says it arrived.
		AckPolicy: jetstream.AckExplicitPolicy,
		// How long a task may be held before the bus decides the runner holding it is
		// gone. A container runs for as long as its step's timeout allows, so this is
		// held off by Working rather than set to the longest a step may take.
		AckWait:    time.Minute,
		MaxDeliver: -1,
	})
	if err != nil {
		return nil, fmt.Errorf("bus: the consumer for pool %s could not be created: %w", pool, err)
	}

	msgs, err := consumer.Fetch(batch, jetstream.FetchMaxWait(wait))
	if err != nil {
		return nil, fmt.Errorf("bus: pool %s could not be asked for work: %w", pool, err)
	}

	var out []Taken
	for msg := range msgs.Messages() {
		var t TaskMessage
		if err := json.Unmarshal(msg.Data(), &t); err != nil {
			// A message nobody can read is not work and will never become work, so it
			// is taken off the queue rather than redelivered for ever. Losing it costs
			// one task, which the run's own deadline already accounts for; leaving it
			// costs the pool, which nothing accounts for. It is reported, because a
			// wire that stopped matching is otherwise a queue that swallows everything.
			b.report(Subject(pool), fmt.Errorf("a task message could not be read: %w", err))
			msg.Term()
			continue
		}
		out = append(out, Taken{Task: t, msg: msg})
	}
	if err := msgs.Error(); err != nil {
		return out, fmt.Errorf("bus: pool %s was asked for work and answered: %w", pool, err)
	}
	return out, nil
}

// Report sends one result back.
//
// Called by the runner when the container is over and everything it produced is uploaded, which
// is why the task state it carries is terminal and why the runner acknowledges the task message
// after this rather than before: a result that never went and a task already off the queue is a
// task nothing will ever answer for.
//
// The stream deduplicates it on the dispatch and the ending, and not on the key. A requeue after
// loss keeps the key, and the ending of the requeue could then follow a late one of the dispatch
// it replaced inside the duplicate window: the stream would answer that it was already there, and
// the one ending the controller was waiting for would go nowhere while the one it throws away went
// through.
func (b *Bus) Report(ctx context.Context, a controller.Answer) error {
	if a.Row == "" {
		return fmt.Errorf("bus: the result of %s names no dispatch, and a requeue keeps the key, so the key alone cannot say which one ended", a.Result.Task)
	}
	body, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("bus: the result of %s could not be written: %w", a.Result.Task, err)
	}
	msg := &nats.Msg{
		Subject: ResultSubject,
		Data:    body,
		Header: nats.Header{
			jetstream.MsgIDHeader: []string{"result-" + a.Row + "-" + a.Result.State.String()},
		},
	}
	if _, err := b.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("bus: the result of %s could not be published: %w", a.Result.Task, err)
	}
	return nil
}

// Answers hands every result to fn until ctx is done.
//
// One durable consumer, because there is one active controller. fn is called before the message
// is acknowledged and never after, so a controller dying in the middle gets the result again
// rather than losing it, and fn returning an error leaves the message for the next delivery,
// unless the error is controller.ErrNotAResult, which no delivery would change. Nothing
// deduplicates: "the same result delivered twice writes the same thing" is the controller's
// promise, made good by the evaluator answering a duplicate with no decision.
func (b *Bus) Answers(ctx context.Context, fn func(context.Context, controller.Answer) error) error {
	if fn == nil {
		return errors.New("bus: consuming results with nothing to hand them to")
	}
	consumer, err := b.results.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:     "controller",
		Description: "The active controller, taking results back.",
		AckPolicy:   jetstream.AckExplicitPolicy,
		AckWait:     time.Minute,
		MaxDeliver:  -1,
	})
	if err != nil {
		return fmt.Errorf("bus: the result consumer could not be created: %w", err)
	}

	for ctx.Err() == nil {
		msgs, err := consumer.Fetch(16, jetstream.FetchMaxWait(time.Second))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("bus: results could not be taken: %w", err)
		}
		for msg := range msgs.Messages() {
			var a controller.Answer
			if err := json.Unmarshal(msg.Data(), &a); err != nil {
				b.report(ResultSubject, fmt.Errorf("a result could not be read: %w", err))
				msg.Term()
				continue
			}
			if err := fn(ctx, a); err != nil {
				if errors.Is(err, controller.ErrNotAResult) {
					// Readable, and still nothing a controller could ever
					// record: it would be the same on every delivery, and
					// this consumer delivers without limit. So it goes the
					// way of a message nobody can read, off the queue and
					// said out loud.
					b.report(ResultSubject, err)
					msg.Term()
					continue
				}
				// Left for the next delivery, which is the whole of what
				// at-least-once buys: a controller that could not record a
				// result gets it again rather than losing it.
				msg.Nak()
				continue
			}
			if err := msg.Ack(); err != nil {
				return fmt.Errorf("bus: a result could not be acknowledged: %w", err)
			}
		}
		if err := msgs.Error(); err != nil && ctx.Err() == nil {
			return fmt.Errorf("bus: taking results answered: %w", err)
		}
	}
	return ctx.Err()
}
