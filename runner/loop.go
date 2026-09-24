package runner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/driver"
)

// Taking work, and answering for every message taken.
//
// "A runner asks for a batch of tasks when it has room", and handles each message in the order the
// page's Runner table sets out: it writes the key down, redeems the grant, and only then
// acknowledges the message, before it pulls or starts anything. Every message ends in one of the
// answers package bus gives a runner, and which one is decided by the host's record of the key and
// then by the redemption's answer, which NextAfter reads as the page's table does:
//
//	an image not named by digest        report that no container ran, then Refused, before
//	                                    anything is written down or redeemed
//	the record holds the key's ending   Bus.Ended, with that ending, and nothing redeemed
//	the record holds the key in flight  nothing, and the message comes round after AckWait
//	the key could not be written down   AgainAfter, for another runner of the pool
//	runs_on names a label not claimed   Release and AgainAfter, before anything is redeemed
//	200                                 Held, then assemble, run and report
//	403                                 Release and AgainAfter, for another runner of the pool
//	409                                 Refused and Release, and nothing reported
//	422, or a 200 it cannot run on      report that no container ran, then Refused and Release
//	no answer, a 401 or another 5xx     keep the key and redeem again until the deadline, then
//	                                    report timed_out with no container ran, Refused, Release
//
// Nothing is put back once a redemption may have bound the task: a message is put back only before
// the redemption and after a 403, which binds nothing, and held back a moment from every runner.

// Queue is the task bus as the loop uses it, which is bus.Bus.
type Queue interface {
	Take(ctx context.Context, pool string, batch int, wait time.Duration) ([]bus.Taken, error)
	Ended(ctx context.Context, t bus.Taken, ending bus.TaskResult) error
}

// Holder is the host's record of the keys it took, which is driver.Docker.
type Holder interface {
	Hold(id agk.TaskID) error
	Release(id agk.TaskID)
}

// Redeemer redeems a task's grant, which is Client.
type Redeemer interface {
	Redeem(ctx context.Context, m bus.TaskMessage) (Redemption, error)
}

// Loop takes work for one runner, and carries each task it takes to an answer.
type Loop struct {
	// Runner and Pool are who this runner is and where it takes work from.
	Runner string
	Pool   string

	// Concurrency is how many tasks this host holds at once, AGK_RUNNER_CONCURRENCY.
	Concurrency int

	// Labels are the labels this runner claims, AGK_RUNNER_LABELS, which its join token allowed.
	// A task whose runs_on names a label outside them is put back before anything is written
	// down: a token may allow fewer labels than its pool carries, so a runner of the pool a task
	// was published to is not for that reason a runner the task may run on.
	Labels []string

	Queue    Queue
	Redeemer Redeemer
	Holder   Holder

	// Carrier runs each task and reports its ending, and its Results is where the loop
	// reports a task that reached no container.
	Carrier *Carrier

	// Assembly is what a task is assembled with once its grant is redeemed.
	Assembly Assembly

	// Progress is told which message each key it hears of belongs to. Nil says nothing of a
	// task's progress.
	Progress *Progress

	// Log is where the agent writes a line.
	Log func(string)

	// Wait is how long one take waits for work. Retry is the first wait before asking again after
	// an answer that may change, and how long a message put back is held back and the loop takes
	// nothing more. Zero is takeWait and retryFirst.
	Wait  time.Duration
	Retry time.Duration

	// Now is the clock the deadlines are read against. Nil is the time of day.
	Now func() time.Time

	mu    sync.Mutex
	held  map[string]int
	quiet time.Time
}

const (
	// takeWait is how long one take waits for work when there is none. It is a long poll: the
	// server answers as soon as a message is there, so this bounds the request and not the
	// wait for a task, and a longer one would only be more time between one stop and the next
	// request being given up.
	takeWait = 20 * time.Second

	// retryFirst and retryMost bound the wait before asking again, doubling from the first to
	// the most: soon after the first failure, since most pass in a moment, and never so long
	// apart that a task waits much past the API coming back.
	retryFirst = time.Second
	retryMost  = 30 * time.Second

	// flushEvery is how often a result the bus did not take is published again.
	flushEvery = 10 * time.Second
)

// Held are the idempotency keys this loop holds: written down and not yet answered, a redemption
// asked again or a container running among them. The heartbeat names them, with the keys of the
// results still to publish, since the controller counts a bound task as held only as long as its
// runner goes on naming it.
func (l *Loop) Held() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	keys := make([]string, 0, len(l.held))
	for key := range l.held {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func (l *Loop) holding(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held == nil {
		l.held = map[string]int{}
	}
	l.held[key]++
}

func (l *Loop) letGo(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[key]--; l.held[key] <= 0 {
		delete(l.held, key)
	}
}

func (l *Loop) say(s string) {
	if l.Log != nil {
		l.Log(s)
	}
}

func (l *Loop) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Run takes work until ctx ends, and returns once every task it took has been answered or given
// up with the agent.
//
// One goroutine takes, and one goroutine per task carries. A take is made only when there is room
// and asks for as many tasks as there is room for, so a full host asks for nothing and a message it
// is not asked for stays on the queue for a runner with room: "distribution naturally proportional
// to each host's real capacity without the controller having to model load". A slot is held from
// the take until the task is answered, which for a task whose redemption got no answer is until it
// gets one or its deadline passes, since the task may be bound here all along.
func (l *Loop) Run(ctx context.Context) error {
	switch {
	case l.Concurrency < 1:
		return fmt.Errorf("runner: a loop holding %d tasks at once takes nothing", l.Concurrency)
	case l.Queue == nil, l.Redeemer == nil, l.Holder == nil, l.Carrier == nil || l.Carrier.Results == nil:
		return errors.New("runner: a loop needs a bus, a client, the host's record and a carrier with its results")
	}
	wait := l.Wait
	if wait <= 0 {
		wait = takeWait
	}

	var carrying sync.WaitGroup
	defer carrying.Wait()
	carrying.Add(1)
	go func() {
		defer carrying.Done()
		l.flush(ctx)
	}()

	// slots holds one token for each task in hand, so a send blocks while the host is full.
	slots := make(chan struct{}, l.Concurrency)
	free := func(n int) {
		for range n {
			<-slots
		}
	}
	backoff := l.retryFirst()
	for {
		// A message was just put back, and the next ones on the queue are likely to be
		// refused the same way.
		if d := l.quietFor(); d > 0 && !sleep(ctx, d) {
			return nil
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		room := 1
	fill:
		for room < l.Concurrency {
			select {
			case slots <- struct{}{}:
				room++
			default:
				break fill
			}
		}

		taken, err := l.Queue.Take(ctx, l.Pool, room, wait)
		if ctx.Err() != nil {
			// Nothing of these was written down, so another runner may have them at
			// once rather than after AckWait.
			for _, t := range taken {
				t.Again()
			}
			free(room)
			return nil
		}
		free(room - len(taken))
		for _, t := range taken {
			carrying.Add(1)
			go func() {
				defer carrying.Done()
				defer free(1)
				l.carry(ctx, t)
			}()
		}
		if err == nil {
			backoff = l.retryFirst()
			continue
		}
		l.say(fmt.Sprintf("work could not be taken from pool %s, and is asked for again in %s: %s", l.Pool, backoff, err))
		if !sleep(ctx, backoff) {
			return nil
		}
		backoff = min(2*backoff, retryMost)
	}
}

func (l *Loop) retryFirst() time.Duration {
	if l.Retry > 0 {
		return l.Retry
	}
	return retryFirst
}

// flush publishes again every result the bus did not take, as long as the loop runs.
func (l *Loop) flush(ctx context.Context) {
	for {
		if err := l.Carrier.Results.Flush(ctx); err != nil && ctx.Err() == nil {
			l.say(err.Error())
		}
		if !sleep(ctx, flushEvery) {
			return
		}
	}
}

// carry answers one message, in the order the table at the top of this file sets out.
func (l *Loop) carry(ctx context.Context, t bus.Taken) {
	m := t.Task
	id := agk.TaskID(m.IdempotencyKey)
	if err := id.Validate(); err != nil {
		// No runner can write this key down or report on it, so the message is taken
		// off the queue rather than handed round the pool for ever.
		l.say(fmt.Sprintf("task %s is dropped, since its message names no task a runner can hold: %s", m.TaskID, err))
		if err := t.Refused(ctx); err != nil {
			l.say(err.Error())
		}
		return
	}

	// "A message whose image is not name@sha256 is reported as no container ran, on the
	// platform's account, then acknowledged, since no runner of any pool could ever run it."
	// Before anything is written down and before any redemption: redeeming would bind the
	// task and read its secrets for a container that is never created, and put back, the
	// message would go round the pool for ever. The driver refuses the same image under
	// Policy.RequireDigest, which is the same rule where a message did not come from here.
	if !agk.ImageByDigest(m.Image) {
		l.say(fmt.Sprintf("task %s (%s) is reported as having reached no container, since it names the image %q, and a runner runs only an image named by digest", m.TaskID, m.IdempotencyKey, m.Image))
		l.reportThenRefuse(ctx, t, unreached(m, l.Runner))
		return
	}

	err := l.Holder.Hold(id)
	var completed *driver.Completed
	switch {
	case errors.As(err, &completed):
		l.answerFromRecord(ctx, t, completed.Ending)
		return
	case errors.Is(err, driver.ErrTaskInFlight):
		// The requeue of a key this host is still running, or a second delivery of one
		// it holds. Nothing is redeemed, so nothing is bound, and the message comes round
		// once AckWait has passed, to whichever runner takes it then.
		l.say(fmt.Sprintf("task %s (%s) is left on the queue, since this host has its key in flight: %s", m.TaskID, m.IdempotencyKey, err))
		return
	case err != nil:
		l.putBack(t, fmt.Sprintf("task %s (%s) is put back, since its key could not be written down: %s", m.TaskID, m.IdempotencyKey, err))
		return
	}
	// After the record, which answers a key this host ended or still has in flight whatever it
	// claims now, and before anything is redeemed. What Hold wrote down is let go of, and the
	// record keeps the key only as taken, which refuses nothing when the message comes round.
	if missing := uncovered(m.RunsOn, l.Labels); len(missing) > 0 {
		l.Holder.Release(id)
		l.putBack(t, fmt.Sprintf("task %s (%s) is put back for another runner of the pool, since it runs on %s and this runner does not claim it", m.TaskID, m.IdempotencyKey, strings.Join(missing, ", ")))
		return
	}
	l.holding(m.IdempotencyKey)
	defer l.letGo(m.IdempotencyKey)

	r, err := l.redeem(ctx, m)
	if ctx.Err() != nil {
		// The agent is stopping. The key is kept on the host as taken, the message goes
		// unacknowledged and comes round after AckWait, and a redemption that did bind
		// the task is answered again to its holder, this host once restarted among them.
		return
	}
	switch next := NextAfter(err); {
	case next == RedeemRun:
		l.run(ctx, t, r)
	case next == RedeemPutBack:
		l.Holder.Release(id)
		l.putBack(t, fmt.Sprintf("task %s (%s) is put back for another runner of the pool: %s", m.TaskID, m.IdempotencyKey, err))
	case next == RedeemLetGo:
		if err := t.Refused(ctx); err != nil {
			l.say(err.Error())
		}
		l.Holder.Release(id)
	case next == RedeemReport:
		l.say(fmt.Sprintf("task %s (%s) is reported as having reached no container: %s", m.TaskID, m.IdempotencyKey, err))
		l.reportThenRefuse(ctx, t, unreached(m, l.Runner))
		l.Holder.Release(id)
	default:
		// Asked again until the deadline, when the grant expired with it and no answer
		// can come.
		l.say(fmt.Sprintf("task %s (%s) is reported timed_out, since its deadline %q passed, or does not read as an instant, before its grant was redeemed: %s", m.TaskID, m.IdempotencyKey, m.Deadline, err))
		l.reportThenRefuse(ctx, t, timedOut(m, l.Runner))
		l.Holder.Release(id)
	}
}

// uncovered are the labels a task runs on that a runner does not claim.
func uncovered(runsOn, claimed []string) []string {
	var missing []string
	for _, label := range runsOn {
		if !slices.Contains(claimed, label) {
			missing = append(missing, label)
		}
	}
	return missing
}

// putBack puts a message back for another runner of the pool, held back a moment from every
// runner, and keeps this loop from taking anything more for as long.
//
// This runner would be refused the same message again for the same reason, and likely the next
// ones too: its credential draining, its disk full, labels it does not claim. Put back at once, the
// message would come straight back to its next free slot, and freed at once, the slot would take
// the next message on the queue to be refused in its turn, so a pool with no other runner would
// spin through its queue, writing keys down and redeeming grants as fast as the API answers. Held
// back and paused, a runner refused everything asks for at most its free slots every Retry.
func (l *Loop) putBack(t bus.Taken, why string) {
	l.say(why)
	if err := t.AgainAfter(l.retryFirst()); err != nil {
		l.say(err.Error())
	}
	l.mu.Lock()
	l.quiet = l.now().Add(l.retryFirst())
	l.mu.Unlock()
}

// quietFor is how long the loop takes nothing more, after a message was put back.
func (l *Loop) quietFor() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.quiet.Sub(l.now())
}

// redeem redeems a task's grant, and asks again, as the holder it may already be, for as long as
// the answer says nothing of whose the task is and the deadline has not passed. It answers with
// the last answer, which is RedeemAgain's only once the deadline has passed or ctx has ended.
func (l *Loop) redeem(ctx context.Context, m bus.TaskMessage) (Redemption, error) {
	deadline, derr := time.Parse(time.RFC3339Nano, m.Deadline)
	wait := l.retryFirst()
	for {
		r, err := l.Redeemer.Redeem(ctx, m)
		if NextAfter(err) != RedeemAgain || ctx.Err() != nil {
			return r, err
		}
		left := deadline.Sub(l.now())
		if derr != nil || left <= 0 {
			return r, err
		}
		l.say(fmt.Sprintf("the grant of task %s (%s) got no answer saying whose the task is, and is redeemed again in %s: %s", m.TaskID, m.IdempotencyKey, min(wait, left), err))
		if !sleep(ctx, min(wait, left)) {
			return r, ctx.Err()
		}
		wait = min(2*wait, retryMost)
	}
}

// run carries a task whose redemption bound it to this runner: acknowledged, assembled, run and
// reported.
func (l *Loop) run(ctx context.Context, t bus.Taken, r Redemption) {
	m := t.Task
	// What the acknowledgement answers does not decide whether the task runs, since the
	// redemption did: a message whose acknowledgement was lost comes round, and is refused to
	// every other runner and found in flight or ended here.
	if err := t.Held(ctx); err != nil {
		l.say(err.Error())
	}

	a, err := l.assemble(ctx, m, r)
	if err != nil {
		l.Holder.Release(agk.TaskID(m.IdempotencyKey))
		switch {
		case ctx.Err() != nil:
			// Stopped with the agent: nothing is said, and the heartbeat's sweep
			// declares the task lost once its key is named no more.
		case refusedForGood(err):
			l.say(fmt.Sprintf("task %s (%s) is reported as having reached no container: %s", m.TaskID, m.IdempotencyKey, err))
			l.report(ctx, unreached(m, l.Runner))
		default:
			l.say(fmt.Sprintf("task %s (%s) is reported timed_out, since what it names could not be fetched before its deadline: %s", m.TaskID, m.IdempotencyKey, err))
			l.report(ctx, timedOut(m, l.Runner))
		}
		return
	}

	if l.Progress != nil {
		defer l.Progress.carrying(m)()
	}
	if err := l.Carrier.Carry(ctx, m, a); err != nil {
		l.say(err.Error())
	}
}

// assemble fetches what the redemption names, and fetches again while a fetch fails in a way
// that may pass, until the deadline, with the same redemption: its URLs hold until then, and
// asking the API again would read the task's secrets again.
func (l *Loop) assemble(ctx context.Context, m bus.TaskMessage, r Redemption) (*Assembled, error) {
	wait := l.retryFirst()
	for {
		a, err := Assemble(ctx, m, r, l.Assembly)
		if err == nil || ctx.Err() != nil || refusedForGood(err) {
			return a, err
		}
		deadline, derr := time.Parse(time.RFC3339Nano, m.Deadline)
		left := deadline.Sub(l.now())
		if derr != nil || left <= 0 {
			return nil, err
		}
		l.say(fmt.Sprintf("task %s (%s) could not be assembled, and is tried again in %s: %s", m.TaskID, m.IdempotencyKey, min(wait, left), err))
		if !sleep(ctx, min(wait, left)) {
			return nil, ctx.Err()
		}
		wait = min(2*wait, retryMost)
	}
}

// refusedForGood says an assembly was refused in a way that assembling again cannot change: the
// redemption, what it names or the message itself is not something a task can be run on.
func refusedForGood(err error) bool {
	return errors.Is(err, ErrAnswerUnusable) || errors.Is(err, ErrNotAsNamed) || errors.Is(err, ErrNotRunnable)
}

// answerFromRecord answers a message whose key this host already carried to an ending with that
// ending, under the message's own task_id, and redeems nothing.
func (l *Loop) answerFromRecord(ctx context.Context, t bus.Taken, e driver.Ending) {
	r, err := EndingOf(t.Task, l.Runner, e)
	if err == nil {
		err = l.Queue.Ended(ctx, t, r)
	}
	if err != nil {
		// The message is left on the queue and comes round once AckWait has passed, to
		// be answered from the record again.
		l.say(fmt.Sprintf("task %s (%s) was not answered from the record of its ending: %s", t.Task.TaskID, t.Task.IdempotencyKey, err))
	}
}

// reportThenRefuse reports an ending the runner speaks first for, and takes the message off the
// queue once the report is out. A report the bus did not take leaves the message to come round, and
// the kept result goes out with a later flush.
func (l *Loop) reportThenRefuse(ctx context.Context, t bus.Taken, r bus.TaskResult) {
	if !l.report(ctx, r) {
		return
	}
	if err := t.Refused(ctx); err != nil {
		l.say(err.Error())
	}
}

// report publishes a result through the results kept under the work root, and answers whether the
// bus took it.
func (l *Loop) report(ctx context.Context, r bus.TaskResult) bool {
	if err := l.Carrier.Results.Report(ctx, r); err != nil {
		l.say(err.Error())
		return false
	}
	return true
}

// timedOut is the result of a dispatch whose deadline passed before any container could start
// for it: timed_out, and nothing else.
func timedOut(m bus.TaskMessage, runner string) bus.TaskResult {
	return bus.TaskResult{TaskID: m.TaskID, IdempotencyKey: m.IdempotencyKey, Runner: runner, State: agk.TaskTimedOut}
}

// sleep waits d or until ctx ends, and answers whether it waited the whole of d.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
