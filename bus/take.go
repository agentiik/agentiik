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
// It says them at once. A runner either writes the task down and holds it, or puts it back, and
// the package documentation says why the first is said on take rather than when the container is
// over: from then on the task is the host's to answer for, through its heartbeat, and nothing is
// left to tell the bus while the container runs.
type Taken struct {
	Task TaskMessage

	msg jetstream.Msg
}

// Held says the task is written down on this host, and takes it off the queue.
//
// Under WorkQueue retention acknowledging is what removes a message from the stream: "a message
// is removed as soon as it has been consumed". So it is said after the key is recorded under the
// work root, driver.Docker.Hold, and never before. The other order leaves a moment in which the
// task is off the queue and on no host's record, and a runner that died in it would leave the
// task for the heartbeat's sweep to find lost, where one that dies before acknowledging has it
// handed to the next runner of the pool a minute later.
func (t Taken) Held() error {
	if t.msg == nil {
		return errors.New("bus: acknowledging a task that came from nowhere")
	}
	return t.msg.Ack()
}

// Again puts it back for somebody else, which is what a runner says when it took a task it
// cannot run: its labels changed, it is draining, it has no room after all, or the task could
// not be written down.
func (t Taken) Again() error {
	if t.msg == nil {
		return errors.New("bus: returning a task that came from nowhere")
	}
	return t.msg.Nak()
}

// Take pulls up to batch tasks for one pool, waiting up to wait for them.
//
// From the one durable consumer the control plane created for the pool, which this binds to and
// never creates. Several runners of one pool are one consumer with many clients, so a task goes
// to whichever asks first. It is a pull consumer because "a runner asks for a batch of tasks when
// it has room, which makes distribution naturally proportional to each host's real capacity
// without the controller having to model load".
//
// Creating one here would fail twice over. A runner's credential reaches this consumer and no
// other and creates nothing, for the reason Consumer gives, and a WorkQueue stream refuses a
// second consumer on a subject one already filters on. So AckWait and MaxDeliver are what
// Consumer set, and nothing on this side restates them.
func (b *Bus) Take(ctx context.Context, pool string, batch int, wait time.Duration) ([]Taken, error) {
	if err := validPool(pool); err != nil {
		return nil, fmt.Errorf("bus: %w", err)
	}
	if batch < 1 {
		return nil, fmt.Errorf("bus: a runner asking for %d tasks", batch)
	}
	consumer, err := b.js.Consumer(ctx, Stream, Durable(pool))
	if errors.Is(err, jetstream.ErrConsumerNotFound) || errors.Is(err, jetstream.ErrStreamNotFound) {
		return nil, fmt.Errorf("bus: pool %s has no consumer to take work from: the control plane creates it when a runner of the pool asks for its bus credential, and a runner creates none", pool)
	}
	if err != nil {
		return nil, fmt.Errorf("bus: the consumer of pool %s could not be reached: %w", pool, err)
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
// is why the task state it carries is terminal. The task message it answers was acknowledged
// long before, on take, so a result that could not be published is the runner's to publish
// again and not the bus's to recover by redelivering the task: redelivery would run the brick a
// second time to recover an answer that already exists.
func (b *Bus) Report(ctx context.Context, a controller.Answer) error {
	body, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("bus: the result of %s could not be written: %w", a.Result.Task, err)
	}
	msg := &nats.Msg{
		Subject: ResultSubject,
		Data:    body,
		Header: nats.Header{
			jetstream.MsgIDHeader: []string{"result-" + string(a.Result.Task) + "-" + a.Result.State.String()},
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
	if b.results == nil {
		return errors.New("bus: a runner's connection takes no results back: the controller opens the bus with Open, which is what makes sure the result stream is there")
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
