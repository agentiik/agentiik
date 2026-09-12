package local

import (
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
)

// narrator turns the state into Events by saying what changed since the last time it looked.
//
// The state is the truth and the narration is a diff of it, which settles two things that
// are otherwise awkward. A step verdict is settled by Next and not by Record, so an Event
// composed at the moment a Result is recorded would carry the verdict the step had before it
// ended; reading the state after the pass carries the verdict it reached. And a step that
// skipped has no task to report at all, so a narration made only of task transitions would
// never mention it, while a diff of the state has its verdict like any other.
//
// The order is the graph's: steps in the order no edge points backwards through, and within
// a step its shards, then the step itself. A shard that finished and a step that ended in
// the same pass therefore read as the shard ending and then the step.
type narrator struct {
	g       *graph.Graph
	emit    func(Event)
	started time.Time

	// verdicts and shards are what has already been said, so that nothing is said
	// twice. A shard is keyed by its index alone, which is what distinguishes it inside
	// its step, and its value carries the attempt so that a retry on the same index is a
	// change rather than a repetition.
	verdicts map[agk.Step]agk.Verdict
	shards   map[shardKey]shardSaid
}

type shardKey struct {
	step  agk.Step
	index int
}

type shardSaid struct {
	attempt int
	state   agk.TaskState
}

// newNarrator starts one, with nothing said yet.
func newNarrator(g *graph.Graph, emit func(Event)) *narrator {
	return &narrator{
		g:        g,
		emit:     emit,
		verdicts: map[agk.Step]agk.Verdict{},
		shards:   map[shardKey]shardSaid{},
	}
}

// narrate says everything that has become true since the last pass.
func (n *narrator) narrate(state *graph.State, now time.Time) {
	if n == nil || n.emit == nil || state == nil {
		return
	}
	if n.started.IsZero() {
		n.started = state.Run.StartedAt
	}
	for _, name := range n.order() {
		ss, known := state.Steps[name]
		if !known {
			continue
		}
		for _, sh := range ss.Shards {
			key := shardKey{step: name, index: sh.Shard.Index}
			said, seen := n.shards[key]
			if seen && said.attempt == sh.Attempt && said.state == sh.Task {
				continue
			}
			n.shards[key] = shardSaid{attempt: sh.Attempt, state: sh.Task}
			// A shard that has only just been planned is not news. It becomes
			// news when it is handed out, which is the transition this loop
			// records itself.
			if sh.Task == agk.TaskPending && !seen {
				continue
			}
			n.emit(Event{
				At:            n.since(now),
				Step:          name,
				Shard:         sh.Shard,
				Shards:        len(ss.Shards),
				Attempt:       sh.Attempt,
				State:         sh.Task,
				Verdict:       ss.Verdict,
				Ports:         counts(sh.Ports),
				ExitCode:      sh.ExitCode,
				NextAttemptAt: sh.NextAttemptAt,
			})
		}
		if verdict, seen := n.verdicts[name]; !seen || verdict != ss.Verdict {
			n.verdicts[name] = ss.Verdict
			if ss.Verdict == agk.VerdictPending {
				// Pending is where every step starts, so reporting it would be
				// a line per step before anything has happened.
				continue
			}
			// The step itself and not one of its shards, which is what an attempt
			// of zero says: an attempt is numbered from one everywhere in this
			// module, so zero cannot name a task.
			n.emit(Event{
				At:      n.since(now),
				Step:    name,
				Shards:  len(ss.Shards),
				Verdict: ss.Verdict,
				Ports:   counts(ss.Ports),
			})
		}
	}
}

// observed says what the driver reported about a task in flight.
//
// These never enter the evaluator: running and publishing are news about a task the evaluator
// already knows is in flight, and it has no word for either.
//
// Two of the driver's own transitions are dropped. A terminal one, because the Result is what
// reports a task that ended and two reports of one ending would read as two endings. And
// dispatched, because this loop records the dispatch itself and the state it wrote is narrated
// already: the driver's dispatched is the moment its container was created, this one is the
// moment the work became somebody's, and a narration saying dispatched twice would be reporting
// the difference between the two to a person who cannot act on it.
func (n *narrator) observed(state *graph.State, e driver.Event, now time.Time) {
	if n == nil || n.emit == nil || e.State.Terminal() || e.State == agk.TaskDispatched {
		return
	}
	run, step, attempt, shard, err := agk.ParseTaskID(string(e.Task))
	if err != nil || state == nil || run != state.Run.ID {
		return
	}
	ss := state.Steps[step]
	n.emit(Event{
		At:      n.since(now),
		Step:    step,
		Shard:   shard,
		Shards:  len(ss.Shards),
		Attempt: attempt,
		State:   e.State,
		Verdict: ss.Verdict,
	})
}

// order is the graph's own order, so that two runs narrate one file the same way.
func (n *narrator) order() []agk.Step {
	if n.g == nil {
		return nil
	}
	return n.g.Order()
}

// since is how long into the run this is. It is a duration and not a moment because a
// narration is read as a sequence, and the moment is in the state beside it.
func (n *narrator) since(now time.Time) time.Duration {
	if n.started.IsZero() {
		return 0
	}
	if d := now.Sub(n.started); d > 0 {
		return d
	}
	return 0
}
