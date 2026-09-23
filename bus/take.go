package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// ResultSubject is where one runner's results go.
//
// A subject per runner, because a subject is the one thing on this bus a publisher cannot choose
// for itself: a runner's credential may publish on its own and on no other, so the runner a result
// arrives under is the runner that sent it, whatever the result says. That is what the controller
// holds against the runner the task was bound to at redemption. On one subject shared by the pool
// the runner field would be a claim, and a machine of the pool could settle another machine's task
// by writing the other machine's name in it.
func ResultSubject(runner string) string { return resultPrefix + runner }

const resultPrefix = "agentiik.results."

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
//
// It answers once the server says the acknowledgement arrived, and not once it has left this
// side. The client keeps what it sends while its link is down and answers nil for it, and a
// server that never received the acknowledgement hands the task to another runner of the pool
// when the consumer's AckWait runs out. So a runner starts nothing for a task whose Held did not
// answer nil, and does not name it in its heartbeat. The key stays recorded as taken and not
// ended. Where the acknowledgement was lost, the bus delivers the task again and it runs once.
// Where only the confirmation was, nothing comes next: the server has taken the message off the
// queue, and the heartbeat does not find the task either, because nobody redeemed it and a task
// nobody redeemed is one db.Pool.Lost reads as waiting on the queue. It stays dispatched until the
// run's own timeout ends it, and a run with none waits for ever. That is not run twice, and a step
// that is not idempotent is not run at all, which is the side to err on, but it is a task left
// hanging; nothing can find it until the database records who took a message as well as who
// redeemed it. A ctx with no deadline waits as long as JetStream's own default.
func (t Taken) Held(ctx context.Context) error {
	if t.msg == nil {
		return errors.New("bus: acknowledging a task that came from nowhere")
	}
	if err := t.msg.DoubleAck(ctx); err != nil {
		return fmt.Errorf("bus: task %s: the server did not confirm the acknowledgement, so the task is not held and nothing is to be started for it: %w", t.Task.IdempotencyKey, err)
	}
	return nil
}

// Again puts it back for somebody else, which is what a runner says when it took a task it
// cannot run: its labels changed, it is draining, it has no room after all, or the task could
// not be written down. Not a task whose key this host has already ended, which Ended answers.
func (t Taken) Again() error {
	if t.msg == nil {
		return errors.New("bus: returning a task that came from nowhere")
	}
	return t.msg.Nak()
}

// Ended answers a task this host took whose key it had already carried to an ending, with that
// ending.
//
// That is the requeue of a task the heartbeat declared lost while its host was only cut off: the
// host ran it to its end and reported into the same silence, and the requeue is likeliest to come
// back to it, and certain to where it is its pool's only runner. The host's record refuses to run
// the key again, which is driver.Completed, and putting the message back would hand it to a runner
// of the pool with no record of the key, which would run it. So it is acknowledged, as every take
// is, and the ending the record holds is reported under the task_id this message carries. The
// controller takes it from the runner that redeemed the dispatch the host ended, as the requeue's
// answer, and the brick never runs twice. It reads the envelopes back by the digests the ending
// names, and the store holds them: the host wrote each there before it wrote the ending down.
//
// The ending is the record's and only the dispatch is this message's, so an ending of another key
// is refused before anything is said: a result under a task_id is about that task_id's key, and
// the two travel as separate fields. It is reported whether or not the acknowledgement was
// confirmed. Nothing is started either way, and a message the bus hands out again is refused and
// answered again, which the result stream drops as the copy it is, or the controller reads as no
// news.
func (b *Bus) Ended(ctx context.Context, t Taken, ending TaskResult) error {
	if ending.IdempotencyKey != t.Task.IdempotencyKey {
		return fmt.Errorf("bus: task %s is %s, and the ending of %s is no answer to it", t.Task.TaskID, t.Task.IdempotencyKey, ending.IdempotencyKey)
	}
	ending.TaskID = t.Task.TaskID
	held := t.Held(ctx)
	if err := b.Report(ctx, ending); err != nil {
		return errors.Join(held, err)
	}
	return held
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
//
// A result the controller would refuse is refused here rather than on the queue, where the one
// thing left to do with it is take it off and say so. One naming no dispatch is among them: a
// requeue keeps the key, so the key alone cannot say which dispatch ended, and task_id is what
// does.
//
// The stream deduplicates it on the runner, the dispatch and the ending, which is what a runner
// publishing again after an answer it never heard sends. Not on the key: a requeue after loss
// keeps the key, and the ending of the requeue could then follow a late one of the dispatch it
// replaced inside the duplicate window, so the stream would answer that it was already there, and
// the one ending the controller was waiting for would go nowhere while the one it throws away
// went through. Nor on the dispatch and the ending alone, because JetStream deduplicates across
// the stream and not per subject, and one dispatch may be reported by two runners: a message
// delivered to two machines is redeemed by one, and the other may report the unreached failure
// the controller refuses from it. Deduplicated on the dispatch alone, that refused report would
// swallow the holder's own failure for two minutes, while the holder was told it had been
// published.
//
// The identifier is the publisher's to write, and a runner's credential does not hold it to its
// subject as it holds the subject. A compromised machine that wrote another's identifier would
// withhold that machine's ending until the heartbeat found the task lost, which is no more than
// it can do already by taking the pool's tasks off the queue and running none of them.
func (b *Bus) Report(ctx context.Context, r TaskResult) error {
	body, err := r.encode()
	if err != nil {
		return fmt.Errorf("bus: %w", err)
	}
	msg := &nats.Msg{
		Subject: ResultSubject(r.Runner),
		Data:    body,
		Header: nats.Header{
			jetstream.MsgIDHeader: []string{"result-" + r.Runner + "-" + r.TaskID + "-" + r.State.String()},
		},
	}
	if _, err := b.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("bus: the result of %s could not be published: %w", r.IdempotencyKey, err)
	}
	return nil
}

// Answers hands every result to fn until ctx is done.
//
// One durable consumer, because there is one active controller. fn is called before the message
// is acknowledged and never after, so a controller dying in the middle gets the result again
// rather than losing it, and fn returning an error leaves the message for a later delivery, timed
// by again, unless the error is controller.ErrNotAResult, which no delivery would change. Nothing
// deduplicates: "the same result delivered twice writes the same thing" is the controller's
// promise, made good by the evaluator answering a duplicate with no decision.
//
// A result is read as the wire describes it, and handed on with its outputs as digests. One the
// reader refuses is taken off the queue and said out loud, as one nobody can decode is: a result
// that is not an ending, or that says what no container could, reads the same on every delivery.
// So is one naming a runner other than the one whose subject it came on, for the reason
// ResultSubject gives. The reader is not the schema, and readResult and check say where the two
// part.
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
			a, err := readResult(msg.Data())
			if err != nil {
				b.report(msg.Subject(), fmt.Errorf("a result could not be read: %w", err))
				msg.Term()
				continue
			}
			// The subject is who sent it, and the result is taken as that
			// runner's word or not at all. One naming somebody else is a
			// machine of the pool speaking for another, the same on every
			// delivery, and the controller is not shown it.
			if sender := strings.TrimPrefix(msg.Subject(), resultPrefix); sender != a.Runner {
				b.report(msg.Subject(), fmt.Errorf("%w: %w: the result of %s names %s and was published by %s", controller.ErrNotAResult, controller.ErrNotTheHolder, a.Result.Task, a.Runner, sender))
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
					b.report(msg.Subject(), err)
					msg.Term()
					continue
				}
				// Left for the next delivery, which is the whole of what
				// at-least-once buys: a controller that could not record a
				// result gets it again rather than losing it. Not at once,
				// for the reason again gives.
				msg.NakWithDelay(again(msg))
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

// again is how long a result the controller could not record waits before it is delivered again.
//
// Not at once. What leaves a result unrecorded is a database or a store that did not answer, or an
// envelope the store does not hold, and asking again a millisecond later changes none of them. The
// consumer delivers without limit, so a redelivery with no pause is a loop that holds the
// controller and the store for as long as the cause lasts. A second, doubling up to a minute,
// keeps the first retry quick and the hundredth cheap.
//
// It never gives up, because a result dropped while the database was down is an ending nobody
// records, and it has no need to: a result for a run that has ended is acknowledged without being
// read, so the run ending is what stops one that never records.
func again(msg jetstream.Msg) time.Duration {
	delay := time.Second
	meta, err := msg.Metadata()
	if err != nil {
		return delay
	}
	for n := uint64(1); n < meta.NumDelivered && delay < time.Minute; n++ {
		delay *= 2
	}
	return min(delay, time.Minute)
}
