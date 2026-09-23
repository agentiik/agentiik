package driver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
)

// The evaluator states the interface and calls neither of its methods. This is the half
// that fills it, and the assertion lives next to New so that a reader of either side
// finds it.
var _ graph.Driver = (*Docker)(nil)

// Docker runs containers on one daemon. One value serves many tasks at once.
//
// What it holds is what is about the daemon rather than about a task: the negotiated
// handle, the answer to the userns floor, the manifest cache keyed by image digest, the
// registry of tasks in flight, and one goroutine following the daemon's event stream for
// all of them. A second event stream per task would be one long poll per container,
// which is the thing the label filter exists to avoid.
type Docker struct {
	cfg   Config
	cli   *docker.Client
	floor *usernsFloor
	cache *manifests

	// ctx bounds the event goroutine, which outlives any one task and ends with
	// Close.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	inflight map[agk.TaskID]*held

	// keys is the record under the work root of what this host has completed, which
	// outlives this process and is what a restarted one reads.
	keys *keys
}

// held is one task this process is running.
//
// It exists so that a stop is fast, and never so that a stop is possible: the container
// is resolved by its dev.agentiik.task label when it is not here, which is what lets
// Stop reach a container this process did not start.
//
// A task is held from the moment Run takes it and not from the moment its container
// exists, because the window between the two is the image pull and it is minutes wide on
// a cold registry. A stop that arrived in that window and changed nothing would leave the
// container to be created afterwards and to run to its deadline, which is the shard
// fail_fast existed to stop. So a stop that finds no watch is recorded here, and the
// watch takes it as soon as there is a container to watch.
type held struct {
	mu        sync.Mutex
	container string
	watch     *watch

	// stopped records a stop that landed and state what it made of the task, for the
	// case where there was no container to send a signal to yet.
	stopped bool
	state   agk.TaskState
}

// stopping records a stop against the task and answers with the watch to send it to,
// which is nothing where the container does not exist yet.
func (h *held) stopping(state agk.TaskState) *watch {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	h.state = state
	if h.watch != nil {
		h.watch.stopping(state)
	}
	return h.watch
}

// watching answers with the watch of this task's container, or nothing where there is no
// container yet. It is a method rather than a field read because the event goroutine
// reads it while the goroutine running the task is still writing it.
func (h *held) watching() *watch {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.watch
}

// join attaches the watch of a container that now exists, and answers with the stop that
// landed while there was none.
//
// The recorded stop is handed to the watch here, so that the one place deciding what a
// stopped task reports stays the watch and not this.
func (h *held) join(container string, w *watch) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.container, h.watch = container, w
	if h.stopped {
		w.stopping(h.state)
	}
	return h.stopped
}

// New opens a driver on a daemon.
//
// Three things happen once, here, rather than once per task: the API version is
// negotiated, the userns floor is read and the machine says what it gives up. Each is a
// fact about the daemon and the policy, and a task that re-read them would be a task
// that could answer differently from the one beside it.
func New(cfg Config) (*Docker, error) {
	cli, err := docker.Dial(cfg.Socket)
	if err != nil {
		if docker.IsUnreachable(err) {
			return nil, fmt.Errorf("driver: %w: %v", ErrDaemonUnreachable, err)
		}
		return nil, fmt.Errorf("driver: %w", err)
	}

	info, err := cli.Info(context.Background())
	if err != nil {
		cli.Close()
		return nil, fmt.Errorf("driver: the daemon at %s could not be asked what it is: %w", cli.Socket(), err)
	}
	floor, err := readUsernsFloor(info, cfg.Policy)
	if err != nil {
		cli.Close()
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	d := &Docker{
		cfg: cfg, cli: cli, floor: floor, cache: newManifests(),
		ctx: ctx, cancel: cancel,
		inflight: map[agk.TaskID]*held{},
		keys:     &keys{root: cfg.WorkRoot},
	}
	floor.announce(cfg.Policy, d.say)

	d.wg.Add(1)
	go d.follow()
	return d, nil
}

// Close releases the daemon handle and ends the event goroutine. It does not stop
// anything that is running: a container outlives the process that started it, which is
// the property adoption depends on.
func (d *Docker) Close() error {
	d.cancel()
	d.wg.Wait()
	return d.cli.Close()
}

// APIVersion is what is being spoken to the daemon, which is min(the ceiling, what it
// offers) and not what was compiled in.
func (d *Docker) APIVersion() string { return d.cli.APIVersion() }

// follow reads the daemon's event stream for every task at once, and hands each event to
// the task it belongs to.
//
// One stream and not one per container: the filter is the task label, which every
// container this driver creates carries, so the daemon does the matching and this side
// does a map lookup. The stream reconnects itself with since set to the last event seen,
// so a daemon that restarted replays the gap rather than losing a die that fell in it.
func (d *Docker) follow() {
	defer d.wg.Done()

	events, failures := d.cli.Events(d.ctx, d.now(), docker.Filters{}.
		Add("type", docker.EventTypeContainer).
		Add("label", LabelTask))

	for {
		select {
		case <-d.ctx.Done():
			return
		case e, open := <-events:
			if !open {
				return
			}
			d.dispatch(e)
		case err, open := <-failures:
			if !open {
				return
			}
			// The stream is a latency optimisation and the inspect under it is
			// the correctness guarantee, so a stream that dropped is worth
			// saying and is not worth failing anything for.
			if err != nil {
				d.say("the Docker event stream dropped and is being resumed: " + err.Error())
			}
		}
	}
}

// dispatch hands one event to the task whose container it is about.
func (d *Docker) dispatch(e docker.Event) {
	id := agk.TaskID(e.Actor.Attributes[LabelTask])
	if id == "" {
		return
	}
	d.mu.Lock()
	h := d.inflight[id]
	d.mu.Unlock()
	if h == nil {
		return
	}
	if w := h.watching(); w != nil {
		w.event(e)
	}
}

// ErrTaskInFlight is a second Run for a task this driver is already running.
//
// At-least-once delivery can hand one task to one host twice while the first delivery is
// still in hand: a message not acknowledged inside its window is delivered again, and the
// window can pass during a cold image pull. The second is refused rather than run beside
// the first, because two Runs carrying one container would each collect it and each remove
// it, and the one that removed it first would take it away under the other. The first
// delivery is the one that reports.
var ErrTaskInFlight = errors.New("the task is already in flight on this runner, and a second delivery of it is refused rather than run beside the first")

// register records a task as being in flight, and answers with what to call when it is
// not. A task already in flight is refused and nothing is recorded, which leaves the
// first delivery holding it: a stop still reaches it through the registry, and its own
// done is what lets it go.
func (d *Docker) register(id agk.TaskID, h *held) (func(), bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, taken := d.inflight[id]; taken {
		return nil, false
	}
	d.inflight[id] = h
	return func() {
		d.mu.Lock()
		delete(d.inflight, id)
		d.mu.Unlock()
	}, true
}

// lookup answers with the task in flight, where this process is holding it.
func (d *Docker) lookup(id agk.TaskID) *held {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.inflight[id]
}

// Stop stops one task in flight.
//
// The container is resolved by its dev.agentiik.task label rather than out of the
// registry, because a stop may arrive for a container this process did not start: a
// runner that was restarted, or a second runner holding the same task. The registry is
// consulted first because it is faster, never because it is the truth.
//
// The stop is the daemon's own, with t set to the policy grace, so that the SIGTERM then
// SIGKILL escalation belongs to the daemon and survives this process dying between the
// two signals. Where a Run is blocked on the container, the stop is issued and this
// returns at once: the Run is the one that reports, coming back cancelled, or timed out
// where the reason was the deadline, which is how one stop produces one Result without
// Stop having to return one.
//
// Stopping a task this driver does not hold is not an error. At-least-once delivery
// means a stop can arrive for a task that already finished, and a driver that failed
// there would fail on a duplicate.
func (d *Docker) Stop(ctx context.Context, s graph.Stop) error {
	grace := d.cfg.Policy.StopGrace
	if grace <= 0 {
		grace = DefaultPolicy().StopGrace
	}

	if h := d.lookup(s.Task); h != nil {
		if w := h.stopping(stopState(s.Reason)); w != nil {
			w.sendStop(ctx)
			return nil
		}
		// The task is held and its container does not exist yet, so the stop is
		// recorded rather than sent: there is nothing to signal, and the Run that is
		// preparing it will not start what has been called off. The label lookup
		// below still runs, because a container created a moment ago and not yet
		// joined carries the label whether or not this side has reached it.
	}

	found, err := d.containerOf(ctx, s.Task)
	if err != nil {
		return err
	}
	if found == "" {
		// Nothing here holds it and nothing on this daemon carries its label.
		// A stop for a task that already finished is the ordinary consequence of
		// at-least-once delivery.
		return nil
	}
	if err := d.cli.ContainerStop(ctx, found, grace); err != nil {
		if docker.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("driver: task %s: stopping container %s: %w", s.Task, found, err)
	}
	return nil
}

// stopState is what a stop makes of the task that was running. The three reasons that
// are not the deadline all end the task the same way, because cancelled is what the run
// states have for work called off, and the reason itself travels on the stop.
func stopState(r graph.StopReason) agk.TaskState {
	if r == graph.StopDeadline {
		return agk.TaskTimedOut
	}
	return agk.TaskCancelled
}

// containerOf finds the container of one task by its label, which is the identifier
// spelled as the label a create carried.
//
// The task identifier is derived from what makes the task that task and never minted, so
// the label of a redelivered task is the label of the first delivery. That is what makes
// both adoption and a stop across processes work off the same lookup.
func (d *Docker) containerOf(ctx context.Context, id agk.TaskID) (string, error) {
	found, err := d.cli.ContainerList(ctx, docker.Filters{}.Add("label", LabelTask+"="+string(id)))
	if err != nil {
		if docker.IsUnreachable(err) {
			return "", fmt.Errorf("driver: task %s: %w: %v", id, ErrDaemonUnreachable, err)
		}
		return "", fmt.Errorf("driver: task %s: looking for its container: %w", id, err)
	}
	if len(found) == 0 {
		return "", nil
	}
	// More than one container under one task label is a daemon somebody put two
	// runners on the same task on. The first is taken, because the alternative is
	// refusing to stop either.
	return found[0].ID, nil
}

// deadlineOf is when this attempt must be over.
//
// The Task carries the moment where the evaluator fixed one. Where it carries only a
// timeout, the moment is the dispatch plus the timeout, and the dispatch is the moment
// the container was created: "recording the dispatch is what fixes the task's deadline,
// because the deadline runs from the moment the work became somebody's".
func deadlineOf(t graph.Task, dispatched time.Time) time.Time {
	if !t.Deadline.IsZero() {
		return t.Deadline
	}
	if t.Timeout > 0 {
		return dispatched.Add(time.Duration(t.Timeout))
	}
	return time.Time{}
}

// unreachable says whether a failure was the daemon not being there, which is the one
// question that has to be asked before anything is charged to a brick.
func unreachable(err error) bool {
	var f *Fault
	if errors.As(err, &f) {
		return errors.Is(f, ErrDaemonUnreachable)
	}
	return docker.IsUnreachable(err)
}
