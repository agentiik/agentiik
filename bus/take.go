package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// The two ends that are not the controller publishing: a runner taking work, and a result coming
// back.

// Results is the stream a task result travels back on, and the progress that precedes it.
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

// Taken is one task message a runner pulled, and what it can say about it afterwards.
//
// It says one thing, and soon. A runner writes the key down and redeems the grant, and then says
// Held where the task is now its own, or Refused where the redemption refused it the task. It says
// Again where it cannot take the task at all, which is only ever before the redemption, and Ended
// where its host had already carried the key to an ending, which writing the key down finds before
// anything is redeemed. Where writing the key down finds it still in flight on its host,
// driver.ErrTaskInFlight, it redeems nothing and says nothing, and the message comes round once
// AckWait has passed, to be answered with Ended once the key has ended there. Where the redemption
// got no answer saying whose the task is, it keeps the key and redeems again until one does, and
// says nothing meanwhile, as Refused says. The package documentation says why the acknowledgement
// follows the redemption and never the container: from the redemption on, the task is bound to one
// runner and is that runner's to answer for, through its heartbeat, and nothing is left to tell the
// bus while the container runs.
type Taken struct {
	Task TaskMessage

	msg jetstream.Msg
}

// Held says the task is this runner's, and takes it off the queue.
//
// It is said once the grant is redeemed, which is what binds the task to this runner, and before
// the image is pulled or anything is created. Under WorkQueue retention acknowledging is what
// removes a message from the stream: "a message is removed as soon as it has been consumed". Said
// before the redemption, it would leave a moment in which the task was off the queue and bound to
// nobody, and a runner that died in that moment would leave a task nothing hands out again and no
// sweep finds, since a task nobody redeemed is one db.Wide.Lost reads as still waiting on the
// queue. Said after it, a runner that dies before redeeming has its task handed to the next runner
// of the pool once AckWait has passed, and one that dies after redeeming is bound and silent, which
// is what the heartbeat's sweep declares lost.
//
// It answers once the server says the acknowledgement arrived, and not once it has left this side:
// the client keeps what it sends while its link is down and answers nil for it. What it answers
// does not decide whether the task runs, though, because the redemption decided that. A message
// whose acknowledgement never arrived comes round again once AckWait has passed. Any other runner
// that takes it is refused it at the redemption, the task being bound to this one, and says Refused
// and starts nothing. This runner, writing the key down again, finds it ended on its host,
// driver.Completed, and answers with Ended, or finds it still in flight there,
// driver.ErrTaskInFlight, and leaves the message to come round until the key has ended; either way
// before redeeming anything. So a runner goes on with a task whose Held answered an error, and says
// so. A ctx with no deadline waits as long as JetStream's own default.
func (t Taken) Held(ctx context.Context) error {
	if t.msg == nil {
		return errors.New("bus: acknowledging a task that came from nowhere")
	}
	if err := t.msg.DoubleAck(ctx); err != nil {
		return fmt.Errorf("bus: task %s: the server did not confirm the acknowledgement, so the message may come round again, to be refused at the redemption by every runner but this one: %w", t.Task.IdempotencyKey, err)
	}
	return nil
}

// Refused takes off the queue a task whose redemption refused this runner, and nothing is started
// for it.
//
// A redemption refuses a runner the task where another runner holds it or where it is over, a lost
// dispatch among them, which is a 409, and neither is this runner's to answer for: the holder
// answers for its task through its heartbeat, the heartbeat's sweep for a holder that went quiet,
// and the requeue of a lost task goes out as a message of its own. Put back, the message would go
// to the next runner of the pool to be refused in its turn, for as long as the stream kept it, and
// left alone it would come round again every AckWait. So it is acknowledged, and nothing more is
// said of it.
//
// A redemption refused because the installation has nothing to give the task, no tree or a secret
// it does not hold, is the one where the runner speaks first. It reports that no container ran,
// which is what ends the dispatch and binds the runner to it, and says Refused once the report is
// published. The other order would leave a runner that died between the two with a task taken off
// the queue and ended by nobody, where this one hands the message to the next runner of the pool,
// to be refused the same way, or to find the dispatch over.
//
// Nothing else is a refusal. A redemption that got no answer, or an answer about the runner rather
// than the task, its own credential refused or the API failing on its side, has said nothing of
// whose the task is. Acknowledged, the message would leave the queue with the task perhaps held by
// nobody, where no sweep finds it. Which answer is which is not always in the status alone, since a
// 401 answers a grant that opens nothing and a runner credential that opens nothing alike, and a
// 500 an installation that will never have the task's tree and one that could not read it this
// time.
//
// Nor does the runner let go of the key. A redemption that got no answer may have bound the task
// all the same: a client that gave up on a slow API, or a connection that dropped once the binding
// had committed. The heartbeat's sweep counts a bound task from its redemption, so a runner that let
// go would stop naming it, and the task would be declared lost three heartbeat intervals on, before
// AckWait brought the message round to be refused even to its holder: a requeue of max_requeues
// spent on a host that was never lost, or a step that does not requeue failed for it. So the
// runner keeps the key, which its heartbeat goes on naming, and redeems again as the holder it may
// already be, which answers as the first time would have and counts as hearing from it, until an
// answer says whose the task is, or the task's deadline, which the grant expires with, has passed.
// It says nothing to the bus meanwhile, so a runner that dies meanwhile leaves the message to come
// round once AckWait has passed, as one that died before redeeming does.
//
// A runner that says Refused lets go of the key it held, which is driver.Docker.Release.
func (t Taken) Refused(ctx context.Context) error {
	if t.msg == nil {
		return errors.New("bus: acknowledging a task that came from nowhere")
	}
	if err := t.msg.DoubleAck(ctx); err != nil {
		return fmt.Errorf("bus: task %s: the server did not confirm the acknowledgement, so the message may come round again, to be refused again: %w", t.Task.IdempotencyKey, err)
	}
	return nil
}

// Again puts it back for somebody else, which is what a runner says when it took a task it cannot
// run: its labels changed, it is draining, it has no room after all, or the task could not be
// written down. Only before the redemption: once redeemed, the task is bound to this runner, and a
// message put back would go round runners that are each refused it while the task waited on a
// runner that had given it up, until the heartbeat's sweep declared it lost. Not a task whose key
// this host has already ended, which Ended answers. A runner that held the key lets go of it, which
// is driver.Docker.Release.
func (t Taken) Again() error {
	if t.msg == nil {
		return errors.New("bus: returning a task that came from nowhere")
	}
	return t.msg.Nak()
}

// AgainAfter puts it back as Again does, to be handed out again once d has passed rather than at
// once. A runner that would be refused the same message again, being draining or unable to write
// the key down, says this: put back at once, the message would come straight back to its next free
// slot, and a pool with no other runner would spin on it as fast as the API answers.
func (t Taken) AgainAfter(d time.Duration) error {
	if t.msg == nil {
		return errors.New("bus: returning a task that came from nowhere")
	}
	return t.msg.NakWithDelay(d)
}

// Ended answers a task this host took whose key it had already carried to an ending, with that
// ending.
//
// That is the requeue of a task the heartbeat declared lost while its host was only cut off: the
// host ran it to its end and reported into the same silence, and the requeue is likeliest to come
// back to it, and certain to where it is its pool's only runner. The host's record refuses to run
// the key again, which is driver.Completed, and putting the message back would hand it to a runner
// of the pool with no record of the key, which would run it. So it is answered, and without a
// redemption: the record answers it, and redeeming would bind this runner to the requeue and read
// its secret values for a brick that is not going to run. The ending the record holds is reported
// under the task_id this message carries, and the controller takes it from the runner that redeemed
// the dispatch the host ended, as the requeue's answer, binding that runner to the requeue as it
// writes the ending. The brick never runs twice. The controller reads the envelopes back by the
// digests the ending names, and the store holds them: the host wrote each there before it wrote the
// ending down.
//
// The ending is published first, and the message acknowledged only once it is, for the reason Held
// follows the redemption: the bus lets go of a task only once something else answers for it, and
// nothing binds a requeue answered from the record until the controller writes that ending.
// Acknowledged first, a host that died before its report went out would leave the requeue off the
// queue and bound to nobody, out of reach of the heartbeat's sweep, for the run to wait on until
// its timeout. Published first, the ending is on the result stream, which keeps it until the
// controller has recorded it, before the message leaves the queue. A host that dies between the
// two, or whose report did not go out, leaves the message to come round once AckWait has passed. On
// this host the record answers it again, which the result stream drops as the copy it is or the
// controller reads as no news. On a host with no record of the key it is refused at the redemption,
// once the controller has written the ending this reports. A controller that has not written it by
// then lets that host redeem the requeue and run the key, which only a requeue can bring about and
// so only for an idempotent step, and the ending this reports is then refused as another runner's
// word on the requeue.
//
// The ending is the record's and only the dispatch is this message's, so an ending of another key
// is refused before anything is said: a result under a task_id is about that task_id's key, and the
// two travel as separate fields.
func (b *Bus) Ended(ctx context.Context, t Taken, ending TaskResult) error {
	if t.msg == nil {
		return errors.New("bus: answering a task that came from nowhere")
	}
	if ending.IdempotencyKey != t.Task.IdempotencyKey {
		return fmt.Errorf("bus: task %s is %s, and the ending of %s is no answer to it", t.Task.TaskID, t.Task.IdempotencyKey, ending.IdempotencyKey)
	}
	ending.TaskID = t.Task.TaskID
	if err := b.Report(ctx, ending); err != nil {
		return fmt.Errorf("%w, so task %s is left on the queue, to be answered again once AckWait has passed", err, t.Task.TaskID)
	}
	if err := t.msg.DoubleAck(ctx); err != nil {
		return fmt.Errorf("bus: task %s: its ending was reported and the server did not confirm the acknowledgement, so the message may come round again, to be answered again from the record: %w", t.Task.IdempotencyKey, err)
	}
	return nil
}

// Take pulls up to batch tasks for one pool, waiting up to wait for them.
//
// From the one durable consumer the control plane created for the pool, which this binds to and
// never creates. Several runners of one pool are one consumer with many clients, so a task goes
// to whichever asks first. It is a pull consumer because "a runner asks for a batch of tasks when
// it has room, which makes distribution naturally proportional to each host's real capacity
// without the controller having to model load".
//
// A ctx that ends ends the wait as well, so that an agent being stopped is not held for the rest of
// a long poll. A message the server handed out as the wait was given up is not lost: nobody
// acknowledged it, so it comes round once AckWait has passed.
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
		return nil, fmt.Errorf("bus: pool %s has no consumer to take work from: the API creates it when the pool is created and again as it starts, and a runner creates none", pool)
	}
	if err != nil {
		return nil, fmt.Errorf("bus: the consumer of pool %s could not be reached: %w", pool, err)
	}

	if wait <= 0 {
		return nil, fmt.Errorf("bus: a runner waiting %s for work", wait)
	}
	waiting, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	// One message is waited for, and the rest of the batch is only what is already there. A pull
	// for the whole batch would hold the first message until the batch filled or the wait ran
	// out, unacknowledged and handed to nobody else, while a host with room for several tasks
	// waited on a queue holding one.
	first, err := consumer.Fetch(1, jetstream.FetchContext(waiting))
	if err != nil {
		return nil, fmt.Errorf("bus: pool %s could not be asked for work: %w", pool, err)
	}
	out, arrived := b.taken(pool, first)
	// The wait running out is a take that found nothing more, which is no failure, and the
	// server says as much just before it runs out. The caller's ctx ending is, whatever the
	// server said, and what was taken by then is answered with it.
	if err := ctx.Err(); err != nil {
		return out, fmt.Errorf("bus: pool %s was asked for work and the wait was given up: %w", pool, err)
	}
	if err := first.Error(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return out, fmt.Errorf("bus: pool %s was asked for work and answered: %w", pool, err)
	}
	if batch == 1 || !arrived {
		return out, nil
	}
	rest, err := consumer.FetchNoWait(batch - 1)
	if err != nil {
		return out, fmt.Errorf("bus: pool %s could not be asked for more work: %w", pool, err)
	}
	more, _ := b.taken(pool, rest)
	out = append(out, more...)
	if err := rest.Error(); err != nil {
		return out, fmt.Errorf("bus: pool %s was asked for more work and answered: %w", pool, err)
	}
	return out, nil
}

// taken reads every message of one fetch as a task, and says whether any message arrived at all.
func (b *Bus) taken(pool string, msgs jetstream.MessageBatch) ([]Taken, bool) {
	var out []Taken
	arrived := false
	for msg := range msgs.Messages() {
		arrived = true
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
	return out, arrived
}

// Report sends one result back.
//
// Called by the runner when the container is over and everything it produced is uploaded, which
// is why the task state it carries is terminal. The task message it answers was acknowledged
// long before, once its grant was redeemed, so a result that could not be published is the
// runner's to publish again and not the bus's to recover by redelivering the task: every runner
// but this one is refused the task at its redemption, so a redelivery would recover nothing.
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

// Progress says a task this runner holds has moved on without ending: running once its container
// has started, publishing once the container has exited and its outputs are being collected.
//
// On the runner's own results subject, for the reason ResultSubject gives: the subject is who sent
// it, and the controller writes it on the dispatch's row only where that dispatch is bound to the
// same runner. A runner's credential, and the narrower one a revoked runner finishes its grace with,
// both publish there already, so saying how a task is getting on needs nothing a result does not.
//
// It is said for a person reading the run, and nothing waits on it. The controller writes it only
// forwards and never over an ending, so one published late, twice, out of order or after the result
// changes nothing, and one that never arrives leaves the task reading dispatched until its ending,
// as it read before there was such a message. So a runner does not hold a container back for it,
// and one that could not be published is not worth publishing again once the task has moved on.
//
// The stream deduplicates it on the runner, the dispatch and the state, which is what a runner
// publishing again after an answer it never heard sends, as Report says of a result; the prefix
// keeps it apart from a result's identifier, though no state is both.
func (b *Bus) Progress(ctx context.Context, p TaskProgress) error {
	body, err := p.encode()
	if err != nil {
		return fmt.Errorf("bus: %w", err)
	}
	msg := &nats.Msg{
		Subject: ResultSubject(p.Runner),
		Data:    body,
		Header: nats.Header{
			jetstream.MsgIDHeader: []string{"progress-" + p.Runner + "-" + p.TaskID + "-" + p.Progress.String()},
		},
	}
	if _, err := b.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("bus: the progress of %s could not be published: %w", p.IdempotencyKey, err)
	}
	return nil
}

// Reports hands every result to fn, and every progress message to progress, until ctx is done, with
// the runner whose subject it arrived on.
//
// It is the other end of Report and Progress, and the control plane's: package bus/control is what
// calls it, and what turns each result into the answer the controller takes. One durable consumer,
// because there is one active controller. fn is called before the message is acknowledged and never
// after, so a controller dying in the middle gets the result again rather than losing it, and fn
// returning an error leaves the message for a later delivery, timed by again, unless the error is
// one Drop made, which no delivery would change. Nothing deduplicates: "the same result delivered
// twice writes the same thing" is the controller's promise, made good by the evaluator answering a
// duplicate with no decision.
//
// A result is read as the wire describes it, with its outputs as digests. One the reader refuses is
// taken off the queue and said out loud, as one nobody can decode is: a result that is not an
// ending, or that says what no container could, reads the same on every delivery. The reader is not
// the schema, and readResult and check say where the two part.
//
// A progress message is told from a result by its keyword, as isProgress reads it, and goes the
// same way: read closed, handed to progress before it is acknowledged, taken off the queue and said
// out loud where it cannot be read or progress answers with Drop, and left for a later delivery
// where progress answers any other error. A progress message left for later is overtaken by the
// result it preceded, which is why the controller writes one only forwards and never over an
// ending.
//
// The runner is handed on beside the result rather than held to it here. The subject is who sent
// it, for the reason ResultSubject gives, and a result naming anybody else is a result from a
// runner that does not hold the task, which is the controller's to name, as
// controller.ErrNotTheHolder. Naming it here would link the controller into every runner, so
// package bus/control compares the two and drops such a result.
func (b *Bus) Reports(ctx context.Context, fn func(ctx context.Context, sender string, r TaskResult) error, progress func(ctx context.Context, sender string, p TaskProgress) error) error {
	if fn == nil {
		return errors.New("bus: consuming results with nothing to hand them to")
	}
	if progress == nil {
		// Acknowledged unread, a task's progress would be lost without anybody hearing of
		// it, and left on the queue it would be delivered for ever.
		return errors.New("bus: consuming results with nothing to hand progress to")
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
			sender := strings.TrimPrefix(msg.Subject(), resultPrefix)
			var handled error
			if isProgress(msg.Data()) {
				p, err := readProgress(msg.Data())
				if err != nil {
					b.report(msg.Subject(), fmt.Errorf("a progress message could not be read: %w", err))
					msg.Term()
					continue
				}
				handled = progress(ctx, sender, p)
			} else {
				r, err := readResult(msg.Data())
				if err != nil {
					b.report(msg.Subject(), fmt.Errorf("a result could not be read: %w", err))
					msg.Term()
					continue
				}
				handled = fn(ctx, sender, r)
			}
			if handled != nil {
				var drop *dropped
				if errors.As(handled, &drop) {
					// Readable, and still nothing a controller could ever
					// record: it would be the same on every delivery, and
					// this consumer delivers without limit. So it goes the
					// way of a message nobody can read, off the queue and
					// said out loud.
					b.report(msg.Subject(), drop.err)
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

// Drop is what a function Reports hands a result or a progress message to answers for one no
// delivery would change, err saying why. Reports takes that message off the queue and says err
// through Trouble, where it leaves one answered with any other error for a later delivery.
//
// Which results no controller could ever record is for the controller's rules to say, and this
// package links no controller, so the function says it: package bus/control drops a result the
// controller refused with controller.ErrNotAResult, and one naming a runner other than its sender.
func Drop(err error) error {
	if err == nil {
		err = errors.New("a result taken off the queue with no reason given")
	}
	return &dropped{err: err}
}

// dropped is an error Drop made. It reads as the error it carries and unwraps to it, so Trouble is
// told what the function said and errors.Is still finds whatever that wraps.
type dropped struct{ err error }

func (d *dropped) Error() string { return d.err.Error() }
func (d *dropped) Unwrap() error { return d.err }

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
