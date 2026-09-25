package driver

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
)

// killSlack is how long the daemon is given past the grace before this side sends the
// kill itself.
//
// The escalation belongs to the daemon: a stop with t set carries SIGTERM, then SIGKILL
// after t, and doing it that way is what makes it survive this process dying between the
// two signals. The timer here is a backstop for the daemon that did not, not a second
// implementation of the rule, which is why it fires after the grace and not at it.
const killSlack = 5 * time.Second

// sweepInterval is how often a container is inspected once it has been silent past its
// deadline, its wait has ended with nothing, or an event about it was dropped.
//
// The wait is the fast path, the event stream catches an exit this driver did not cause,
// and the inspect is the correctness guarantee under both. That is the same shape the
// controller uses for its own notifications and sweep: the stream is a latency
// optimisation, so a stream that is down makes a task slow and never wrong.
const sweepInterval = 2 * time.Second

// exit is how a container ended and who noticed.
type exit struct {
	Code int

	// TimedOut says the deadline fired and this container was stopped for it,
	// which is the difference between agk.TaskTimedOut and agk.TaskFailed on the
	// same exit code.
	TimedOut bool

	// Stopped says a graph.Stop landed, and why.
	Stopped bool

	// OOM says the kernel took it for its memory, which is on the event stream and
	// on an inspect and never on the wait.
	OOM bool

	// Source names which of the three saw it, for the log line and for nothing
	// else.
	Source string
}

// watch is one container being waited on: its exit, however it arrives.
//
// Three things can report it and they are not equivalent. The wait was opened before the
// start and is the ordinary answer. The event stream carries a die this driver did not
// cause, an out-of-memory kill included, which is the case the wait can miss entirely.
// The inspect is asked only when a task has been silent past its deadline, when its wait
// ended with nothing, or when an event about it was dropped, and it is the one that cannot
// be wrong.
type watch struct {
	cli  *docker.Client
	id   string
	step agk.Step
	log  *taskLog

	// deadline is when the attempt must be over. A zero deadline is a task with no
	// timeout, which nothing here stops.
	deadline time.Time
	grace    time.Duration
	now      func() time.Time

	mu      sync.Mutex
	fired   bool
	stopped bool
	oom     bool
	events  chan docker.Event

	// dropped says an event was dropped, which arms the sweep: the dropped one may be
	// the die of a container whose wait never answers.
	dropped chan struct{}
}

// newWatch is the watch of one container.
func newWatch(cli *docker.Client, id string, step agk.Step, l *taskLog, deadline time.Time, grace time.Duration, now func() time.Time) *watch {
	if now == nil {
		now = time.Now
	}
	if grace <= 0 {
		grace = DefaultPolicy().StopGrace
	}
	return &watch{
		cli: cli, id: id, step: step, log: l,
		deadline: deadline, grace: grace, now: now,
		events:  make(chan docker.Event, 16),
		dropped: make(chan struct{}, 1),
	}
}

// event hands the watch one event from the daemon's stream, without ever blocking the
// goroutine that follows it.
//
// A watch that is not keeping up drops the event rather than holding up every other
// task, and loses nothing by it: the die is also on the wait, and where it is not, the
// drop arms the sweep, whose inspect finds the same exit a moment later rather than at
// the deadline, where it would read as timed_out.
func (w *watch) event(e docker.Event) {
	select {
	case w.events <- e:
	default:
		select {
		case w.dropped <- struct{}{}:
		default:
		}
	}
}

// stopping records that a stop landed, so that the exit reads as cancelled rather than
// as the code the kill left behind.
func (w *watch) stopping(reason agk.TaskState) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	if reason == agk.TaskTimedOut {
		w.fired = true
	}
}

// resendStop sends the stop again where one landed before the container was running.
//
// A task is registered before its container is started, so that a stop can reach it at
// all, and the daemon answers a stop on a container it has not started yet with 304 Not
// Modified: nothing is signalled and nothing is remembered, so the start that follows
// runs the container as though no stop had ever arrived. A cancelled run would then keep
// a container for the rest of its natural life, which is the one thing Stop exists to
// prevent. This is called once the container is running, and it is a no-op for every
// task nobody stopped.
func (w *watch) resendStop(ctx context.Context) {
	w.mu.Lock()
	stopped := w.stopped
	w.mu.Unlock()
	if !stopped {
		return
	}
	w.log.note("a stop landed before the container was running, which the daemon answered without signalling anything, so it is sent again now that there is a process to signal")
	w.sendStop(ctx)
}

// await waits for the container to end, enforcing the deadline while it does.
//
// The deadline is enforced with the daemon's own stop, which is SIGTERM and then SIGKILL
// after the grace, and with a kill from this side only if the daemon did not carry it
// through. The three sources are watched at once, and the first one with an answer is
// the answer.
func (w *watch) await(ctx context.Context, waited <-chan docker.Waited) (exit, error) {
	var timer <-chan time.Time
	if !w.deadline.IsZero() {
		timer = time.After(w.deadline.Sub(w.now()))
	}
	var killer <-chan time.Time
	var sweep <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return exit{}, ctx.Err()

		case got, open := <-waited:
			if !open {
				// The wait ended with nothing, which is the daemon
				// closing the connection. The sweep answers instead.
				waited = nil
				if sweep == nil {
					sweep = time.After(sweepInterval)
				}
				continue
			}
			if got.Err != nil {
				w.log.note("the wait on the container ended before it did: %v", got.Err)
				waited = nil
				if sweep == nil {
					sweep = time.After(sweepInterval)
				}
				continue
			}
			return w.finish(got.StatusCode, false, "the wait"), nil

		case <-w.dropped:
			if sweep == nil {
				sweep = time.After(sweepInterval)
			}

		case e := <-w.events:
			if e.Actor.ID != w.id || e.Type != docker.EventTypeContainer {
				continue
			}
			switch e.Action {
			case docker.ActionOOM:
				// The oom arrives before the die and is the only place
				// the reason is written, so it is remembered here: the
				// die that follows carries the code and not the reason.
				w.log.note("the container was killed for its memory")
				w.mu.Lock()
				w.oom = true
				w.mu.Unlock()
			case docker.ActionDie:
				code, ok := exitCodeOf(e)
				if !ok {
					// A die with no code on it is still a die. The
					// inspect reads the code.
					if sweep == nil {
						sweep = time.After(0)
					}
					continue
				}
				return w.finish(code, oomOf(e), "the daemon event stream"), nil
			}

		case <-timer:
			timer = nil
			w.mu.Lock()
			w.fired = true
			w.mu.Unlock()
			w.log.note("the step's timeout passed, so the container is being stopped: SIGTERM, then SIGKILL after %s", w.grace)
			w.sendStop(ctx)
			killer = time.After(w.grace + killSlack)
			if sweep == nil {
				sweep = time.After(w.grace + killSlack + sweepInterval)
			}

		case <-killer:
			killer = nil
			// The daemon was given the grace and did not carry the
			// escalation through. This is the backstop and not the rule.
			w.log.note("the container was still running %s after the timeout, so it was killed", w.grace)
			w.cli.ContainerKill(ctx, w.id, "SIGKILL")
			if sweep == nil {
				sweep = time.After(sweepInterval)
			}

		case <-sweep:
			sweep = nil
			in, err := w.cli.ContainerInspect(ctx, w.id)
			if err != nil {
				if docker.IsNotFound(err) {
					// The container is gone and took its exit code
					// with it. Nothing can be read, and inventing a
					// code here would charge this side's trouble to
					// the brick.
					return exit{}, fault(w.step, ErrDaemonUnreachable, ChargePlatform,
						"the container is gone and its exit code with it, so there is none to read")
				}
				w.log.note("the container could not be inspected: %v", err)
				sweep = time.After(sweepInterval)
				continue
			}
			if in.State.Running || in.State.Restarting {
				sweep = time.After(sweepInterval)
				continue
			}
			got := w.finish(in.State.ExitCode, in.State.OOMKilled, "an inspect")
			return got, nil
		}
	}
}

// stoppedAtDeadline says whether a container that ended at finished, with nobody watching
// it any more, ended the way the stop at its deadline ends one.
//
// That is an end at the deadline or after it, and no later than a watch lets a container
// outlive its deadline: the grace the daemon's stop gives it, the slack before the kill
// that backs it, and the inspect after that. Inside that window the watch would have
// fired, whatever ended the container, so the end reads as timed_out as it would have read
// to the delivery that died watching it, and a redelivery reports the same state whether
// it reached the container a moment before its end or a moment after. An end past the
// window is a container nobody stopped, and it is read by its code.
func stoppedAtDeadline(deadline, finished time.Time, grace time.Duration) bool {
	if deadline.IsZero() || finished.Before(deadline) {
		return false
	}
	if grace <= 0 {
		grace = DefaultPolicy().StopGrace
	}
	return !finished.After(deadline.Add(grace + killSlack + sweepInterval))
}

// finish reads the outcome, with what this side knows about why.
func (w *watch) finish(code int, oom bool, source string) exit {
	w.mu.Lock()
	defer w.mu.Unlock()
	return exit{Code: code, TimedOut: w.fired, Stopped: w.stopped, OOM: oom || w.oom, Source: source}
}

// sendStop asks the daemon to stop the container: SIGTERM, then SIGKILL after the grace.
//
// The stop is issued on a context that outlives the caller's, because a stop that is
// cancelled half way is a container nobody is going to stop afterwards.
func (w *watch) sendStop(ctx context.Context) {
	stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.grace+killSlack)
	go func() {
		defer cancel()
		if err := w.cli.ContainerStop(stop, w.id, w.grace); err != nil && !docker.IsNotFound(err) {
			w.log.note("the container could not be stopped: %v", err)
		}
	}()
}

// exitCodeOf reads the code a die event carries.
func exitCodeOf(e docker.Event) (int, bool) {
	raw, ok := e.Actor.Attributes["exitCode"]
	if !ok || raw == "" || raw == "-" {
		return 0, false
	}
	code := 0
	negative := false
	for i := 0; i < len(raw); i++ {
		switch {
		case i == 0 && raw[i] == '-':
			negative = true
		case raw[i] >= '0' && raw[i] <= '9':
			code = code*10 + int(raw[i]-'0')
		default:
			return 0, false
		}
	}
	if negative {
		code = -code
	}
	return code, true
}

// oomOf says whether a die event was an out-of-memory kill. The attribute is not always
// there, which is why the oom event before it is followed as well.
func oomOf(e docker.Event) bool {
	return e.Actor.Attributes["oomKilled"] == "true"
}

// state reads the exit as a task state, through the three things that are true of it
// before the exit code table is consulted at all.
//
// A deadline that fired is agk.TaskTimedOut and a stop that landed is
// agk.TaskCancelled, whatever code the kill left behind, because the code of a killed
// container says how it was killed and not why. Everything else is the table's, read
// through agk.Band and nowhere else. Never agk.TaskLost: that is the heartbeat's word
// for a runner that stopped reporting, and a container that exited 137 reported.
func (e exit) state() agk.TaskState {
	switch {
	case e.TimedOut:
		return agk.TaskTimedOut
	case e.Stopped:
		return agk.TaskCancelled
	default:
		return exitState(e.Code)
	}
}

// pastDeadline is a task whose deadline passed while its image was being resolved, before
// any container was created for it.
type pastDeadline struct {
	deadline time.Time
	ref      string

	// pulled is how long the pull ran before the deadline cut it short, and zero where
	// none had begun.
	pulled int64

	// err is what the resolution answered once its context was done.
	err error

	// stopped is what a stop that had landed by the moment the pull came back made of the
	// task, and stop says one had. It is read then and not once the ending is written,
	// because a stop that lands after the deadline cut the pull came second.
	stopped agk.TaskState
	stop    bool
}

func (p *pastDeadline) Error() string {
	return fmt.Sprintf("the deadline %s passed while %s was being pulled: %v", p.deadline.UTC().Format(time.RFC3339), p.ref, p.err)
}

func (p *pastDeadline) Unwrap() error { return p.err }

// timedOutPulling ends a task whose deadline passed during its pull: timed_out, with no
// container, as a task whose deadline passed before its grant could be redeemed ends, or
// cancelled where a stop landed during the pull before the deadline did.
//
// It is an ending and not an error, unlike every other way a task reaches no container,
// because the reason is the task's own clock and not a failure of anything: the step's
// retry reads it as a timeout, as it would a container stopped at the same moment, and a
// retry that finds the image already pulled starts at once. The log says why, since a
// person reading a timed_out step with no container would otherwise look for one, and how
// long the pull ran, since a result that ran no container carries no usage: the observer's
// Event has it as well, for a caller that is not a runner.
func (d *Docker) timedOutPulling(ctx context.Context, t graph.Task, p *pastDeadline) (graph.Result, error) {
	sink, closeSink, err := d.openLog(ctx, t)
	if err != nil {
		return graph.Result{}, err
	}
	defer closeSink()
	log := newLog(sink, newMasker(), d.cfg.Now, d.cfg.Policy.LogMaxBytes, d.cfg.Policy.LogMaxLines)
	state := agk.TaskTimedOut
	if p.stop {
		// A stop that landed during the pull found no container to signal and was
		// recorded, and the work was called off before its deadline came: that is
		// what the task ended as, and a cancelled step is not retried as a timeout.
		state = p.stopped
		log.note("the task was stopped while its image %s was being pulled, so no container was created for it", p.ref)
	}
	if state == agk.TaskTimedOut {
		log.note("the step's deadline passed while its image %s was being pulled, %s, so no container was created for it: %v", p.ref, pullRan(p.pulled), p.err)
	}
	ref, _ := log.finish()
	d.observe(ctx, Event{Task: t.ID, State: state, Log: ref, Usage: Usage{ImagePullMS: p.pulled}})
	return graph.Result{Task: t.ID, State: state}, nil
}

// pullRan says how long a pull the deadline cut short had run, for the log of its ending.
func pullRan(ms int64) string {
	if ms == 0 {
		return "before the pull had begun"
	}
	return fmt.Sprintf("after %d ms of pulling", ms)
}
