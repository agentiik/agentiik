package driver

import (
	"context"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
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

// sweepInterval is how often a container that has been silent past its deadline is
// inspected.
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
// The inspect is asked only when a task has been silent past its deadline, and it is the
// one that cannot be wrong.
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
		events: make(chan docker.Event, 16),
	}
}

// event hands the watch one event from the daemon's stream, without ever blocking the
// goroutine that follows it.
//
// A watch that is not keeping up drops the event rather than holding up every other
// task, and loses nothing by it: the die is also on the wait, and where it is not, the
// sweep's inspect finds the same exit a moment later.
func (w *watch) event(e docker.Event) {
	select {
	case w.events <- e:
	default:
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
