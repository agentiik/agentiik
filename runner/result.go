package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
)

// Reporting a task's ending as the wire's task result, $defs/taskResult.
//
// The result is the last thing a runner does with a task. "It posts the outputs under the upload
// policy, and records the key's ending", then "destroys the container, its working directory and
// every tree laid out for its key, and publishes the result on its own results subject". driver.Run
// is everything up to the removal of the container and its working directory, since those go in its
// defers, so the result is assembled once Run has returned and the trees are gone, and never from
// the observer's terminal event as it fires: that event comes before the ending is written down and
// before anything is removed, and a result published there would tell the controller a task was over
// while its container and its secrets were still on the host. What the result says comes from that
// event all the same, kept until Run returns, because it is what names the envelopes by digest, the
// usage and the exit of a container a Result cannot carry. The log is the API's last answer to its
// shipment, which logs.go closes before the result goes out.

// Endings keeps the terminal event of every task the driver carries until the task's result is
// assembled from it. It is the driver's Observer, or the first of them.
//
// The driver is given one observer, and an agent has more than one thing to tell: Next is told every
// event after this has kept what it keeps, which is how the others are composed with it.
type Endings struct {
	Next driver.Observer

	mu   sync.Mutex
	told map[agk.TaskID]driver.Event
}

// Observe keeps a terminal event and hands every event on.
func (e *Endings) Observe(ctx context.Context, ev driver.Event) {
	if ev.State.Terminal() {
		e.mu.Lock()
		if e.told == nil {
			e.told = map[agk.TaskID]driver.Event{}
		}
		e.told[ev.Task] = ev
		e.mu.Unlock()
	}
	if e.Next != nil {
		e.Next.Observe(ctx, ev)
	}
}

// take answers with the terminal event of one task and forgets it, since a task's ending is
// reported once and a key kept here past that would be kept for the life of the agent.
func (e *Endings) take(id agk.TaskID) (driver.Event, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ev, ok := e.told[id]
	delete(e.told, id)
	return ev, ok
}

// Carrier runs a task the runner holds and reports its ending, in the order the runner owes.
type Carrier struct {
	// Runner is the name this runner joined under, which a result carries and which names the
	// subject it is published on.
	Runner string

	// Driver runs the task. It is the driver.Docker whose Observer Endings is, and the two are
	// one: a Run whose ending Endings was never told of has nothing to report but its error.
	Driver  graph.Driver
	Endings *Endings

	// Results is where the result goes, kept under the work root until the bus has taken it.
	Results *Results

	// Logs is where each task's log is shipped, which is Client. It goes with a driver opened with
	// TaskLogs, which refuses a task its carrier ships no log for, and nil with a driver given
	// nowhere to write a log, whose results address none: a log written nowhere is not one to
	// point at.
	Logs LogShipper

	// Log is where the agent writes a line.
	Log func(string)

	// shipEvery and closeWithin are how often a running task's log is shipped and how long its
	// closing chunk is waited for, zero being the constants of those names, which a test shortens.
	shipEvery, closeWithin time.Duration

	// atPoint is told each point of Carry an agent can stop at with something on disk to recover
	// from, which is where a test takes what the disk holds. Nil tells nobody.
	atPoint func(point string)
}

func (c *Carrier) at(point string) {
	if c.atPoint != nil {
		c.atPoint(point)
	}
}

// ErrNotReported is a task Carry ran and reported nothing for, because nothing about it is this
// delivery's to report: another delivery of the key is carrying it on this host, or the runner
// stopped before any container ended and the task is still where a restarted agent finds it.
var ErrNotReported = errors.New("runner: the task was not reported, since nothing about it is this delivery's to say")

// Carry runs one assembled task and reports its ending, which is the whole of what follows the
// acknowledgement.
//
// Run removes the container and the working directory on its way out, and Remove takes the trees
// laid out for the key, so by the time the result is assembled nothing of the task is left on the
// host but the record of its key and, where the bus does not take the result at once, the result
// itself under the work root. What the result says is what the driver told Endings of the ending,
// or, where Run refused a key this host had already ended, what the record says of that ending. A
// Run that ended in an error and told of no ending ran no container, and is reported failed with
// none, which the controller charges to the platform: that is a task this runner could not carry,
// and a result is what ends the dispatch rather than leaving it to be declared lost three heartbeat
// intervals on and requeued. The two exceptions are ErrNotReported, and neither takes the trees
// away where another delivery of the key may still be using them.
//
// A result that is published answers nil. One the bus did not take is kept and published again by
// Results, and Carry answers the error that says so.
func (c *Carrier) Carry(ctx context.Context, m bus.TaskMessage, a *Assembled) error {
	say := c.Log
	if say == nil {
		say = func(string) {}
	}
	run := a.Context(ctx)
	var log *shipment
	if c.Logs != nil {
		log = newShipment(ctx, c.Logs, m, say, c.shipEvery, c.closeWithin)
		defer log.abandon()
		run = withShipment(run, log)
	}
	// The dispatch is owed its result from before the ending can be written, since the record
	// stops naming the key as taken the moment it is, and the result names it only once kept.
	owing, err := c.Results.owe(m, c.Runner)
	if err != nil {
		say(err.Error())
	}
	c.at("run")
	_, err = c.Driver.Run(run, a.Task)
	c.at("ran")

	// Endings is keyed by the task, which is the key, and two deliveries of one key are two
	// Carries. A Run that refused this delivery, for a key another delivery is running or has
	// just ended, ran nothing of its own, so it takes no ending: the one there is the other
	// delivery's, which that delivery reports.
	var completed *driver.Completed
	if errors.Is(err, driver.ErrTaskInFlight) {
		// Another delivery on this host is running the key, in a container bound to a
		// tree of the key, and Remove takes every tree of the key: this delivery's goes
		// with that one's, once it ends. What it owes is its own to settle, where it is not
		// what the delivery running the key owes under the same task_id.
		if owing {
			c.Results.settle(m.TaskID)
		}
		return fmt.Errorf("%w: task %s is carried by another delivery on this host: %w", ErrNotReported, m.TaskID, err)
	}
	if rerr := a.Remove(); rerr != nil {
		// A tree left behind is disk and not a secret, since the values are the working
		// directory's and Run took that. It is said rather than reported, and the result is
		// no less the result.
		say(rerr.Error())
	}
	if errors.As(err, &completed) {
		// A key this host had ended between Hold and Run, which is a second delivery of
		// the key that got past Hold before the first wrote its ending. That ending is
		// the answer, as it is where Hold finds it.
		r, err := EndingOf(m, c.Runner, completed.Ending)
		if err != nil {
			return err
		}
		return c.Results.Report(ctx, r)
	}

	var r bus.TaskResult
	told, ended := c.Endings.take(a.Task.ID)
	switch {
	case ended:
		// The closing chunk goes before the result, "so a finished task's log is whole by the
		// time its step is judged", and the result's log is what the API answered it.
		//
		// The record of the key says what the result says of the log, since a later report
		// is made from it: nothing while the close is waited for, since what the driver
		// counted is not what the store holds, and the API's answer once it is known.
		r = resultOf(m, c.Runner, told)
		if log != nil {
			c.logged(a.Task.ID, nil)
			c.at("closing")
			r.Log = log.finish(told.Log.Truncated)
			c.logged(a.Task.ID, r.Log)
			c.at("logged")
		}
	case err != nil && ctx.Err() != nil:
		// The agent is stopping, and the task did not fail: nothing is said of it, the
		// agent stops naming its key, and the heartbeat's sweep declares it lost, which is
		// what a runner the control plane stopped hearing from is, requeued where the step
		// allows it. The result stays owed: the agent that comes back keeps one where the
		// record holds an ending after all, and owes nothing where it holds none.
		return fmt.Errorf("%w: task %s was stopped with the agent: %w", ErrNotReported, m.TaskID, err)
	case err != nil:
		say(fmt.Sprintf("runner: task %s (%s) ran no container: %s", m.TaskID, m.IdempotencyKey, err))
		r = unreached(m, c.Runner)
		// A log the driver opened before it failed is closed all the same, rather than left
		// open for its readers to wait on.
		r.Log = log.finish(false)
	default:
		return fmt.Errorf("runner: task %s ended and the driver told nothing of its ending, so the driver was not given this carrier's Endings as its observer", m.TaskID)
	}

	if cerr := r.Check(); cerr != nil {
		// What the driver told cannot be written as a result, which leaves the dispatch with
		// no ending at all unless something is said. What can always be said is that the
		// runner could not report what ran, charged to the platform.
		say(fmt.Sprintf("runner: task %s: its ending cannot be reported as it is, so it is reported as a failure that ran no container: %s", m.TaskID, cerr))
		r = unreached(m, c.Runner)
		r.Log = log.finish(false)
	}
	return c.Results.Report(ctx, r)
}

// logRecorder is the host's record of the keys it ended, which is driver.Docker.
type logRecorder interface {
	Logged(id agk.TaskID, log *driver.EndedLog) error
}

// logged records what the result of a key says of its log in the host's record of its ending, where
// the driver keeps one.
func (c *Carrier) logged(id agk.TaskID, l *bus.Log) {
	rec, ok := c.Driver.(logRecorder)
	if !ok {
		return
	}
	var ended *driver.EndedLog
	if l != nil {
		uri, err := agk.ParseLogURI(l.URI)
		if err != nil {
			return
		}
		ended = &driver.EndedLog{URI: uri, Lines: l.Lines, Truncated: l.Truncated}
	}
	if err := rec.Logged(id, ended); err != nil && c.Log != nil {
		c.Log(err.Error() + ": a later report of the key from the record may not say of its log what its result said")
	}
}

// endingReader is the host's record read for how one key ended, which is driver.Docker.
type endingReader interface {
	Ended(id agk.TaskID) (driver.Ending, bool, error)
}

// Recover keeps a result for every dispatch an earlier agent on this host owed one to and never kept
// one for, from what the record says of its key's ending. It is called as the agent starts, before
// its first heartbeat, so that Keys names each key from that heartbeat on, and the Flush that
// follows publishes each result.
//
// A dispatch whose key the record holds no ending of is owed nothing: the task never reached its
// ending, the record names the key as taken where it was, and Dispatched lists it for the heartbeat
// as it lists every key an earlier agent held.
//
// Recovered, a result says what EndingOf says of a requeue answered from the record, and of the log,
// which was being closed when the agent stopped, only what can be said without its closing chunk's
// answer: where it is, truncated, since what the store holds of it is not known to be whole, and
// no lines. The wire's lines are what the API holds, and the record counts something else at every
// point but the last: the driver's own lines until the carrier clears them, none while the close is
// waited for. Zero never claims a line the store cannot show. The record is then given the same log, so that a requeue of the key
// answered from it later says what this result said.
//
// A record that could not be read leaves the dispatch owed, for the agent after this one, and the
// error says so.
func (c *Carrier) Recover() error {
	rec, ok := c.Driver.(endingReader)
	if !ok {
		return nil
	}
	var failed []error
	for _, o := range c.Results.Owed() {
		key := agk.TaskID(o.IdempotencyKey)
		e, ended, err := rec.Ended(key)
		if err != nil {
			failed = append(failed, fmt.Errorf("runner: the result owed to %s is still owed, since the record of %s could not be read: %w", o.TaskID, key, err))
			continue
		}
		if !ended {
			c.Results.settle(o.TaskID)
			continue
		}
		r, err := EndingOf(bus.TaskMessage{TaskID: o.TaskID, IdempotencyKey: o.IdempotencyKey}, c.Runner, e)
		// A log wherever the record names one, a pull that ended the task included, since the
		// driver writes there why no container ran, and wherever a container started, whose log
		// the carrier cleared from the record while it was being closed. An ending that names
		// neither is one the driver opened no log for.
		if err == nil && c.Logs != nil && (e.Log != nil || !e.StartedAt.IsZero()) {
			var uri agk.LogURI
			if uri, err = agk.NewLogURI(key); err == nil {
				r.Log = &bus.Log{URI: uri.String(), Truncated: true}
			}
		}
		if err != nil {
			failed = append(failed, fmt.Errorf("runner: the result owed to %s is still owed, since the record of %s says nothing a result can: %w", o.TaskID, key, err))
			continue
		}
		// A result Keep could not write down is still held, and published by the next Flush.
		if err := c.Results.Keep(r); err != nil {
			failed = append(failed, err)
		}
		if r.Log != nil {
			c.logged(key, r.Log)
		}
	}
	return errors.Join(failed...)
}

// resultOf is the result of dispatch m, from the ending the driver told of it.
//
// A container that started is described whole. Its exit code and its span, wherever the daemon gave
// them. Its ports, every one a success published, and an empty list wherever it did not succeed,
// since a task that ran and published nothing says so, as the documentation's own failed result
// does. Its artifacts, once each, since a digest is one object however many items name it. Its
// usage, the two sampled figures only where a sample was read. A task that never reached a
// container, stopped before its container started, carries none of it.
func resultOf(m bus.TaskMessage, runner string, e driver.Event) bus.TaskResult {
	r := bus.TaskResult{TaskID: m.TaskID, IdempotencyKey: m.IdempotencyKey, Runner: runner, State: e.State}
	if !e.StartedAt.IsZero() {
		r.StartedAt = e.StartedAt.UTC()
		if e.ExitCode != nil && !e.FinishedAt.IsZero() {
			code := *e.ExitCode
			r.ExitCode, r.FinishedAt = &code, e.FinishedAt.UTC()
		}
		r.Outputs = []bus.Output{}
		for _, o := range e.Outputs {
			r.Outputs = append(r.Outputs, bus.Output{Port: string(o.Port), Digest: o.Digest, Items: o.Items})
		}
		r.Artifacts = []bus.Artifact{}
		for _, f := range e.Artifacts {
			r.Artifacts = appendArtifact(r.Artifacts, bus.Artifact{SHA256: f.SHA256, Bytes: f.Size})
		}
		u := &bus.Usage{ImagePullMS: e.Usage.ImagePullMS}
		if e.Usage.Sampled {
			cpu, rss := e.Usage.CPUSeconds, e.Usage.MaxRSSBytes
			u.CPUSeconds, u.MaxRSSBytes = &cpu, &rss
		}
		r.Usage = u
	}
	return r
}

// EndingOf is the result of dispatch m, from what the record of its key says of an ending, which is
// how a host answers a requeue of a key it has already carried to one: with Bus.Ended, whose ending
// this is.
//
// It says what the record kept, which is everything a result names but the usage: the record keeps
// references and nothing measured, and the first report of the ending carried the measure.
func EndingOf(m bus.TaskMessage, runner string, e driver.Ending) (bus.TaskResult, error) {
	if string(e.Key) != m.IdempotencyKey {
		return bus.TaskResult{}, fmt.Errorf("runner: task %s is %s, and the ending of %s is no answer to it", m.TaskID, m.IdempotencyKey, e.Key)
	}
	if !e.State.Terminal() {
		return bus.TaskResult{}, fmt.Errorf("runner: the record holds %s as %s, which is no ending to report", e.Key, e.State)
	}
	r := bus.TaskResult{TaskID: m.TaskID, IdempotencyKey: m.IdempotencyKey, Runner: runner, State: e.State}
	if !e.StartedAt.IsZero() {
		r.StartedAt = e.StartedAt.UTC()
		if e.ExitCode != nil && !e.FinishedAt.IsZero() {
			code := *e.ExitCode
			r.ExitCode, r.FinishedAt = &code, e.FinishedAt.UTC()
		}
		r.Outputs = []bus.Output{}
		for _, o := range e.Outputs {
			r.Outputs = append(r.Outputs, bus.Output{Port: string(o.Port), Digest: o.Digest, Items: o.Items})
		}
		r.Artifacts = []bus.Artifact{}
		for _, a := range e.Artifacts {
			r.Artifacts = appendArtifact(r.Artifacts, bus.Artifact{SHA256: a.SHA256, Bytes: a.Bytes})
		}
	}
	if e.Log != nil {
		r.Log = &bus.Log{URI: e.Log.URI.String(), Lines: e.Log.Lines, Truncated: e.Log.Truncated}
	}
	return r, nil
}

// unreached is the result of a dispatch this runner could not carry to a container: failed, and
// nothing else, which the controller reads as the platform's failure and never the brick's.
func unreached(m bus.TaskMessage, runner string) bus.TaskResult {
	return bus.TaskResult{TaskID: m.TaskID, IdempotencyKey: m.IdempotencyKey, Runner: runner, State: agk.TaskFailed}
}

// appendArtifact adds an artifact unless the list already names its digest. The wire lists each
// object once, and a file two items share is one object.
func appendArtifact(list []bus.Artifact, a bus.Artifact) []bus.Artifact {
	for _, had := range list {
		if had.SHA256 == a.SHA256 {
			return list
		}
	}
	return append(list, a)
}
