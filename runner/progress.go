package runner

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/driver"
)

// Saying how a task is getting on.
//
// "A task's running and publishing transitions travel as a progress message on the runner's own
// results subject", so that a run read through the API shows a step running while its container
// runs rather than dispatched until it ends. The driver tells of each transition on the goroutine
// running the task and must not be held up, and nothing waits on a progress message: the
// controller writes one only forwards and never over an ending, so one lost is a step that reads
// dispatched a little longer. So a transition is handed to a goroutine of its own and published
// from there, once, and one that could not be published is said and not tried again, since by
// then the task has moved on.

// ProgressPublisher is what takes a progress message to the bus, which is bus.Bus.
type ProgressPublisher interface {
	Progress(ctx context.Context, p bus.TaskProgress) error
}

// Progress is the observer that publishes each task's running and publishing transitions. It is
// the Next of the driver's Endings.
type Progress struct {
	runner string
	bus    ProgressPublisher
	say    func(string)

	mu    sync.Mutex
	tasks map[agk.TaskID]bus.TaskMessage

	queue chan bus.TaskProgress
}

// progressQueued is how many transitions wait to be published before one more is dropped. Each
// task makes two, so this is a host of many tasks whose bus stopped answering for a while, and a
// transition dropped then is one the controller would have taken late or not at all.
const progressQueued = 256

// progressTimeout bounds one publication, so that a bus that does not answer holds up the next
// transition by that much and no more.
const progressTimeout = 10 * time.Second

// NewProgress is the observer of one runner's transitions, publishing on p.
func NewProgress(runner string, p ProgressPublisher, say func(string)) *Progress {
	if say == nil {
		say = func(string) {}
	}
	return &Progress{runner: runner, bus: p, say: say, tasks: map[agk.TaskID]bus.TaskMessage{}, queue: make(chan bus.TaskProgress, progressQueued)}
}

// Observe hands a running or publishing transition of a task this runner is carrying to the
// goroutine that publishes it, and drops anything else. It never blocks.
func (p *Progress) Observe(_ context.Context, ev driver.Event) {
	if ev.State != agk.TaskRunning && ev.State != agk.TaskPublishing {
		return
	}
	p.mu.Lock()
	m, ok := p.tasks[ev.Task]
	p.mu.Unlock()
	if !ok {
		return
	}
	select {
	case p.queue <- bus.TaskProgress{TaskID: m.TaskID, IdempotencyKey: m.IdempotencyKey, Runner: p.runner, Progress: ev.State}:
	default:
		p.say(fmt.Sprintf("the progress of task %s (%s) to %s is dropped, since %d transitions are already waiting on the bus", m.TaskID, m.IdempotencyKey, ev.State, progressQueued))
	}
}

// carrying says which message the transitions of its key belong to, until the function it answers
// is called. A key is carried by one delivery at a time on a host, since the driver refuses a
// second, so the dispatch a transition names is the one being run.
func (p *Progress) carrying(m bus.TaskMessage) func() {
	id := agk.TaskID(m.IdempotencyKey)
	p.mu.Lock()
	p.tasks[id] = m
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		if p.tasks[id].TaskID == m.TaskID {
			delete(p.tasks, id)
		}
		p.mu.Unlock()
	}
}

// Run publishes transitions as they are handed over, until ctx ends.
func (p *Progress) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case pr := <-p.queue:
			publish, cancel := context.WithTimeout(ctx, progressTimeout)
			if err := p.bus.Progress(publish, pr); err != nil && ctx.Err() == nil {
				p.say(err.Error())
			}
			cancel()
		}
	}
}
