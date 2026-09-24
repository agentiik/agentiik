package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"

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
// log, the usage and the exit of a container a Result cannot carry.

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

	// Logs says the driver was given somewhere to write each task's log, which is what lets a
	// result address one: a log written nowhere is not one to point at.
	Logs bool

	// Log is where the agent writes a line.
	Log func(string)
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
	_, err := c.Driver.Run(a.Context(ctx), a.Task)
	told, ended := c.Endings.take(a.Task.ID)
	if errors.Is(err, driver.ErrTaskInFlight) && !ended {
		// Another delivery on this host is running the key, in a container bound to a
		// tree of the key, and Remove takes every tree of the key: this delivery's goes
		// with that one's, once it ends.
		return fmt.Errorf("%w: task %s is carried by another delivery on this host: %w", ErrNotReported, m.TaskID, err)
	}
	if rerr := a.Remove(); rerr != nil {
		// A tree left behind is disk and not a secret, since the values are the working
		// directory's and Run took that. It is said rather than reported, and the result is
		// no less the result.
		say(rerr.Error())
	}

	var r bus.TaskResult
	var completed *driver.Completed
	switch {
	case ended:
		r = resultOf(m, c.Runner, told, c.Logs)
	case errors.As(err, &completed):
		// A key this host had ended between Hold and Run, which is a second delivery of
		// the key that got past Hold before the first wrote its ending. That ending is
		// the answer, as it is where Hold finds it.
		if r, err = EndingOf(m, c.Runner, completed.Ending); err != nil {
			return err
		}
	case err != nil && ctx.Err() != nil:
		// The agent is stopping, and the task did not fail: nothing is said of it, the
		// agent stops naming its key, and the heartbeat's sweep declares it lost, which is
		// what a runner the control plane stopped hearing from is, requeued where the step
		// allows it.
		return fmt.Errorf("%w: task %s was stopped with the agent: %w", ErrNotReported, m.TaskID, err)
	case err != nil:
		say(fmt.Sprintf("runner: task %s (%s) ran no container: %s", m.TaskID, m.IdempotencyKey, err))
		r = unreached(m, c.Runner)
	default:
		return fmt.Errorf("runner: task %s ended and the driver told nothing of its ending, so the driver was not given this carrier's Endings as its observer", m.TaskID)
	}

	if cerr := r.Check(); cerr != nil {
		// What the driver told cannot be written as a result, which leaves the dispatch with
		// no ending at all unless something is said. What can always be said is that the
		// runner could not report what ran, charged to the platform.
		say(fmt.Sprintf("runner: task %s: its ending cannot be reported as it is, so it is reported as a failure that ran no container: %s", m.TaskID, cerr))
		r = unreached(m, c.Runner)
	}
	return c.Results.Report(ctx, r)
}

// resultOf is the result of dispatch m, from the ending the driver told of it.
//
// A container that started is described whole. Its exit code and its span, wherever the daemon gave
// them. Its ports, every one a success published, and an empty list wherever it did not succeed,
// since a task that ran and published nothing says so, as the documentation's own failed result
// does. Its artifacts, once each, since a digest is one object however many items name it. Its
// usage, the two sampled figures only where a sample was read. A task that never reached a
// container, stopped before its container started, carries none of it.
func resultOf(m bus.TaskMessage, runner string, e driver.Event, logs bool) bus.TaskResult {
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
	if logs {
		r.Log = logOf(m, e.Log.Lines, e.Log.Truncated)
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

// logOf is where a task's log is addressed from: its key's, as agk.NewLogURI addresses it, and not
// the sink's, which knows where its bytes went on this host and nowhere else.
func logOf(m bus.TaskMessage, lines int, truncated bool) *bus.Log {
	uri, err := agk.NewLogURI(agk.TaskID(m.IdempotencyKey))
	if err != nil {
		return nil
	}
	return &bus.Log{URI: uri.String(), Lines: lines, Truncated: truncated}
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
