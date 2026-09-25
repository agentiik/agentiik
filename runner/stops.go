package runner

import (
	"context"
	"fmt"
	"sync"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
)

// Stopping a task this host holds.
//
// A stop reaches a runner by two channels with one meaning. agentiik.stops is the fast path, heard
// over the runner's own connection for as long as the agent runs; it keeps nothing, so a stop
// published while that connection was down is never heard. The heartbeat's cancel is the backstop:
// each key the request named whose dispatch, bound to this runner, the controller has ended as
// cancelled or timed_out. The controller also repeats a stop there while the run goes on, and a
// replaced connection overlaps the one it replaces, so the same key can arrive several times and by
// both. All of them end here, and the driver is asked to stop a key once: a second SIGTERM is one
// more signal a brick may read as being told to hurry, and a second stop with another reason would
// rewrite what the first made of the task, cancelled where the bus said the deadline.

// StopSource is where stops are heard, which is bus.Bus.
type StopSource interface {
	Stops(ctx context.Context, fn func(graph.Stop)) error
}

// Stops hands each stop for a key this host holds to the driver, once per key, whichever channel
// it came by. It is the heartbeat's Stopper, and the subscription's listener.
type Stops struct {
	// Stopper is the driver.
	Stopper Stopper

	// Holding answers the keys this host holds a task for: the loop's, from the moment a key is
	// written down until its task is answered, and those an earlier agent took and never ended,
	// whose containers may still be running on the daemon. Every runner hears every stop, and
	// one for a key held elsewhere is passed over here rather than costing a lookup on the
	// daemon, which is what the driver makes of a key it does not hold in memory.
	Holding func() []string

	// Log is where the agent writes a line.
	Log func(string)

	mu      sync.Mutex
	stopped map[agk.TaskID]bool
	going   sync.WaitGroup
}

// Hear listens for stops on from until ctx ends or from's connection is closed, and answers once
// the server holds the subscription, so a stop published after it returns is one this hears. The
// agent calls it before it takes anything, as package bus asks.
//
// It may be called again with another connection, which is how the subscription survives the bus
// connection being replaced: the replacement is heard from before the one it replaces is closed,
// so that no moment passes with nobody listening, and a stop heard on both in the overlap is sent
// once. The subscription on a connection that is closed ends with it.
func (s *Stops) Hear(ctx context.Context, from StopSource) error {
	return from.Stops(ctx, func(st graph.Stop) { s.heard(ctx, st) })
}

// heard hands a stop heard on the bus to the driver, on a goroutine of its own. The subscription
// hands stops over one at a time, and the driver's stop of a container this process did not start
// waits out the grace, which would hold back every stop behind it.
func (s *Stops) heard(ctx context.Context, st graph.Stop) {
	if !s.claim(st.Task) {
		return
	}
	// Carried through past the agent's own stop, since a container the control plane asked to
	// stop is to be stopped whether or not this agent is winding down, and bounded as one
	// heartbeat's cancel is, after which the next heartbeat names the key again.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), HeartbeatInterval)
	s.going.Add(1)
	go func() {
		defer s.going.Done()
		defer cancel()
		if err := s.send(ctx, st); err != nil {
			s.say(fmt.Sprintf("task %s, which a stop on the bus names (%s), could not be stopped, and is stopped when the heartbeat's answer cancels it: %s", st.Task, st.Reason, err))
		}
	}()
}

// Stop stops a task the heartbeat's answer cancels, where no stop has reached it yet.
func (s *Stops) Stop(ctx context.Context, st graph.Stop) error {
	if !s.claim(st.Task) {
		return nil
	}
	return s.send(ctx, st)
}

// Wait returns once every stop heard on the bus has been sent or given up.
func (s *Stops) Wait() { s.going.Wait() }

// send asks the driver, and lets go of the key where the driver could not stop it, so that the
// next stop for it, the heartbeat's above all, is sent rather than passed over. The driver answers
// an error only for a container it found by its label; one it watches is handed the stop at once,
// and the driver sends it again itself for as long as the daemon refuses it.
func (s *Stops) send(ctx context.Context, st graph.Stop) error {
	err := s.Stopper.Stop(ctx, st)
	if err != nil {
		s.mu.Lock()
		delete(s.stopped, st.Task)
		s.mu.Unlock()
	}
	return err
}

// claim answers whether a stop for key is the one to send: the key is held here and no stop has
// been sent for it. A key no longer held is forgotten on the way, so that what is kept is never
// more than what is held.
func (s *Stops) claim(key agk.TaskID) bool {
	var held map[agk.TaskID]bool
	if s.Holding != nil {
		keys := s.Holding()
		held = make(map[agk.TaskID]bool, len(keys))
		for _, k := range keys {
			held[agk.TaskID(k)] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.stopped {
		if !held[k] {
			delete(s.stopped, k)
		}
	}
	if !held[key] || s.stopped[key] || s.Stopper == nil {
		return false
	}
	if s.stopped == nil {
		s.stopped = map[agk.TaskID]bool{}
	}
	s.stopped[key] = true
	return true
}

func (s *Stops) say(line string) {
	if s.Log != nil {
		s.Log(line)
	}
}
