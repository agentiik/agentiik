package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/expr"
)

// The run, as a state a Plan comes out of and a Result goes back into. Everything below
// this line is the rules of the other files read in one order: the barrier says whether a
// step may start, the merge strategies say what its ports carry, the fan-out says how many
// containers that is, the exit-code table says what became of each one, the retry policy
// says whether there is another, and the reduction says what the run itself is.
//
// Nothing here runs a container. Next says which tasks have become ready and which of the
// tasks in flight should be stopped; Record takes back what happened to one. The loop
// between them belongs to cmd/agk and to the controller.
//
// Next advances the state and returns the decision, and that is not the same as a decision
// that has happened. The state is where progress lives, which is what lets a second
// process pick a run up, and a Plan names tasks that are still pending and tasks that are
// still in flight, so calling Next twice with the same moment returns the same Plan: the
// second call finds the same shards waiting to be handed out and the same containers
// waiting to be stopped. What the first call settled, a step that skipped or a step that
// ended, the second call finds already settled and settles again the same way.
//
// Four readings the documentation does not state are taken in this file, each recorded
// again beside the rule that applies it: what a port the inputs keyword feeds carries and
// how its items are identified, what run.attempt is, and what becomes of a step that broke
// a rule of the language while the run was already under way. doc.go lists them beside
// the others, because whoever writes the driver reads that list and not this one.

// Options is everything a run carries that the workflow file does not.
type Options struct {
	// Inputs are the workflow inputs the trigger supplied, already held to their
	// declared schemas with required and default applied. That validation is package
	// schema's and it happens before a run exists, so what arrives here is what the
	// run holds.
	Inputs map[string]any

	// Vars are the workflow and namespace variables, already merged, because which of
	// the two a name came from is the API's business and not an expression's.
	Vars map[string]any

	// Trigger is body, headers, query and scheduled_for. Event is the CloudEvents 1.0
	// document an event trigger matched.
	Trigger map[string]any
	Event   map[string]any

	// Limits are the namespace's four size rules, applied wherever an envelope is
	// built here: a merge, a publication and a port the inputs keyword feeds. The zero
	// value is the documented defaults, because a caller that says nothing about them
	// means the rules and not their absence.
	Limits agk.Limits

	// MaxRequeues is the installation's max_requeues: how many times one key is handed
	// out again after a loss. Nil is DefaultMaxRequeues, for the reason the zero Limits
	// are the defaults, and zero is an installation that requeues nothing, which is what
	// max_requeues: 0 says. A pointer, because the two are different settings, as a hint
	// a file does not write is not one written false: read as an int, the zero a caller
	// left unset and the zero an installation wrote would be one value, and one of them
	// would be read as the other.
	MaxRequeues *int
}

// Evaluator is a handle over a Graph and a State. It holds no progress of its own: the
// graph is fixed at validation and the state is the run, so a second process given the
// same two answers the same way.
type Evaluator struct {
	g      *Graph
	s      *State
	limits agk.Limits

	// maxRequeues is the bound New was given, and zero is an installation that asked for
	// no requeue at all.
	maxRequeues int

	// templates is a memo and not progress. The same expression is read once per shard
	// and again on every attempt, and compiling CEL is the expensive half of evaluating
	// it; what it holds is a function of the file alone, so losing it costs time and
	// changes no answer.
	templates map[templateKey]*expr.Template
}

// templateKey names one compiled value: the text, and the position it was compiled for.
type templateKey struct {
	scope  expr.Scope
	source string
}

// Start begins a run of a graph.
//
// Every step of the graph gets an entry at once, so that a step nobody has reached is a
// pending entry rather than a missing one and the reduction to a run verdict can tell a
// state that has not been built from a graph with nothing in it.
func Start(g *Graph, run agk.Run, o Options, at time.Time) (*Evaluator, error) {
	if g == nil {
		return nil, fmt.Errorf("graph: there is no graph to start a run of")
	}
	if err := run.ID.Validate(); err != nil {
		return nil, fmt.Errorf("graph: the run cannot be started: %w", err)
	}
	at = at.UTC()
	if run.StartedAt.IsZero() {
		run.StartedAt = at
	}
	s := &State{
		Version: StateVersion,
		Run:     run,
		Inputs:  o.Inputs,
		Vars:    o.Vars,
		Trigger: o.Trigger,
		Event:   o.Event,
		Steps:   make(map[agk.Step]StepState, len(g.Steps())),
	}
	for _, name := range g.Steps() {
		s.Steps[name] = StepState{}
	}
	most := DefaultMaxRequeues
	if o.MaxRequeues != nil {
		most = *o.MaxRequeues
	}
	return New(g, s, o.Limits, most)
}

// New resumes a run from a state. "Failover is a state resume and never a rebuild": load
// the State, call Next, get the Plan the instance that died would have got.
//
// The size rules and max_requeues are arguments and not state, because neither is the
// run's: they are the namespace's and the installation's, and a pass is decided under the
// ones that hold when it is taken. The size rules are read as Options reads them. The bound
// is the number itself, zero requeuing nothing, since a caller resuming a run has already
// read the installation's setting and has nothing left unset to fill in. A negative bound
// is refused: it counts no number of times, and reading it as none or as the default would
// be guessing which the installation meant.
func New(g *Graph, s *State, l agk.Limits, maxRequeues int) (*Evaluator, error) {
	switch {
	case g == nil:
		return nil, fmt.Errorf("graph: there is no graph to evaluate the run against")
	case s == nil:
		return nil, fmt.Errorf("graph: there is no state to resume")
	case maxRequeues < 0:
		return nil, fmt.Errorf("graph: max_requeues is how many times one key is handed out again after a loss, and %d is no number of times: zero is what requeues nothing", maxRequeues)
	case s.Version != StateVersion:
		return nil, fmt.Errorf("graph: the state was written at version %d and this package reads version %d: a state is resumed and never guessed at, so a field whose meaning has moved is refused rather than read", s.Version, StateVersion)
	}
	if s.Steps == nil {
		s.Steps = map[agk.Step]StepState{}
	}
	if l == (agk.Limits{}) {
		l = agk.DefaultLimits()
	}
	return &Evaluator{g: g, s: s, limits: l, maxRequeues: maxRequeues, templates: map[templateKey]*expr.Template{}}, nil
}

// State is the run as a value: what a caller persists, and what New takes back.
func (e *Evaluator) State() *State { return e.s }

// Graph is the graph the run is evaluated against.
func (e *Evaluator) Graph() *Graph { return e.g }

// Next says what should happen now: the tasks that have become ready, the tasks in
// flight that should be stopped, and the moment to ask again.
//
// The moment is an argument rather than a clock so that evaluation is pure and a replay
// is exact. A caller that never comes back at Wake does not get a wrong Plan; it gets the
// right one late.
func (e *Evaluator) Next(now time.Time) (Plan, error) {
	now = now.UTC()

	// A run that has ended still has containers to call off: the moment a run ends is
	// not the moment the work stops, which is what Stop is for.
	switch {
	case e.s.Run.State == agk.Cancelled:
		return e.stopEverything(StopCancelled), nil
	case e.s.Run.State == agk.TimedOut:
		return e.stopEverything(StopDeadline), nil
	case e.s.Run.State.Terminal():
		return Plan{}, nil
	}

	// "It is the deadline that makes timed_out a state a run can actually reach: once
	// it passes, the tasks still running are stopped and the run ends there rather
	// than waiting on a graph that is no longer going anywhere."
	if deadline, ok := e.deadline(); ok && !now.Before(deadline) {
		e.s.Run.State = agk.TimedOut
		e.s.Run.FinishedAt = now
		e.s.Seq++
		return e.stopEverything(StopDeadline), nil
	}

	// Deciding is a pass over the graph in an order no edge points backwards through,
	// so that a step publishing in this pass is already published when the step below
	// it reads its barrier.
	//
	// It is taken again when building a task failed, because a shard that failed that
	// way ends its step, and a run whose last step ended inside dispatch would be left
	// at running with nothing in the plan to bring anybody back.
	var plan Plan
	for {
		for _, name := range e.g.Order() {
			if err := e.advance(name, now); err != nil {
				return Plan{}, err
			}
		}
		plan = Plan{}
		broke, err := e.dispatch(&plan, now)
		if err != nil {
			return Plan{}, err
		}
		if !broke {
			break
		}
	}
	e.stops(&plan)
	plan.Wake = e.wake(now)

	e.s.Run.State = runVerdict(e.s, e.tolerates)
	if e.s.Run.State.Terminal() && e.s.Run.FinishedAt.IsZero() {
		e.s.Run.FinishedAt = now
	}
	e.s.Seq++
	return plan, nil
}

// Record takes back what became of one task.
//
// It is the only thing that enters the evaluator after a run has started, which is what
// makes "a run replays by replaying its Results" true. A result for an attempt that is
// over changes nothing: the task identifier is the idempotency key, a bus is allowed to
// deliver twice, and a late answer about an attempt already judged is a duplicate rather
// than news. Nor does an ending of a dispatch the shard has been requeued past, since the
// requeue kept the key and only the dispatch tells the two apart: that dispatch was
// judged lost and handed out again, and the attempt now waits on the requeue.
func (e *Evaluator) Record(r Result, now time.Time) error {
	now = now.UTC()
	run, name, attempt, shard, err := agk.ParseTaskID(string(r.Task))
	if err != nil {
		return fmt.Errorf("graph: %w", err)
	}
	if run != e.s.Run.ID {
		return fmt.Errorf("graph: the result names run %s and this is run %s: a result belongs to the run that decided the task", run, e.s.Run.ID)
	}
	ss, known := e.s.Steps[name]
	if !known {
		return fmt.Errorf("graph: step %s: the run holds no step of that name", name)
	}
	at := slices.IndexFunc(ss.Shards, func(sh ShardState) bool { return sh.Shard == shard })
	if at < 0 {
		return fmt.Errorf("graph: step %s: the run holds no shard %s: a result names the task the evaluator decided on", name, shardName(shard))
	}

	sh := ss.Shards[at]
	if sh.Attempt != attempt || sh.Task.Terminal() {
		return nil
	}
	if r.State.Terminal() && r.Requeue != sh.Requeue {
		return nil
	}

	// The attempt is under way, so the moment it was waiting for is spent. A shard that
	// kept it would read as a shard with another attempt coming for the rest of the
	// run, and the step it belongs to would never end.
	sh.NextAttemptAt = time.Time{}
	sh.Task = r.State
	sh.ExitCode = r.ExitCode
	if !r.DispatchedAt.IsZero() {
		sh.DispatchedAt = r.DispatchedAt.UTC()
	}
	if !r.StartedAt.IsZero() {
		sh.StartedAt = r.StartedAt.UTC()
	}
	if !r.FinishedAt.IsZero() {
		sh.FinishedAt = r.FinishedAt.UTC()
	}
	// The envelopes are kept for a shard that produced them, which is exit 0 and
	// nothing else: "output envelopes are published" is the success row of the table,
	// and a failed shard's ports are the empty envelopes the step publishes for it.
	if shardVerdict(r.State, r.ExitCode) == agk.VerdictSucceeded {
		sh.Ports = r.Outputs
	}

	if r.State.Terminal() && shardVerdict(r.State, r.ExitCode) == agk.VerdictFailed {
		if st, ok := e.g.Step(name); ok {
			var when time.Time
			again := requeued(st.Retry, st.Idempotent, sh, e.maxRequeues)
			if again {
				// A requeue is the same attempt handed out again, so the
				// attempt number and the key built from it stay as they were
				// and the dispatch is what moves. It is due at once.
				sh.Requeue++
			} else if requeueable(st.Retry, st.Idempotent, sh) {
				// The file asked for a requeue and the installation's bound
				// refused it. Nothing in the file explains a step failing on a
				// loss it said to requeue, so the step says why. Only a step
				// still running fails on it: one a merge: first cancelled can
				// still lose a task in flight, since a stop is a request, and
				// it keeps the reason that fixed its verdict.
				if ss.Verdict == agk.VerdictRunning {
					ss.Reason = fmt.Sprintf("%s was lost on dispatch %d of its key, and max_requeues hands one key out again after a loss at most %d times: the loss stands, and the step fails on the infrastructure's account rather than the brick's", e.taskID(name, sh), sh.Requeue+1, e.maxRequeues)
				}
			} else if when, again = nextAttempt(st.Retry, sh); again {
				// A further attempt is a new key, handed out for the first
				// time.
				sh.Attempt++
				sh.Requeue = 0
			}
			if again {
				// A shard with more to come has not finished: it goes back to
				// pending, and what it carried from the dispatch that ended goes
				// with it.
				sh.Task = agk.TaskPending
				sh.ExitCode = 0
				sh.Ports = nil
				sh.NextAttemptAt = when
				sh.DispatchedAt, sh.StartedAt, sh.FinishedAt = time.Time{}, time.Time{}, time.Time{}
			} else if r.Reason != "" && ss.Reason == "" && ss.Verdict == agk.VerdictRunning {
				// A failure that stands, and says why where no container could.
				ss.Reason = r.Reason
			}
		}
	}

	ss.Shards[at] = sh
	e.s.Steps[name] = ss
	e.s.Seq++
	return nil
}

// Cancel ends the run the way a principal holding workflow:run or a concurrency group
// ends one. The tasks in flight are named by the next Plan, because stopping a container
// is the driver's and saying which ones is this package's.
func (e *Evaluator) Cancel(now time.Time) {
	if e.s.Run.State.Terminal() {
		return
	}
	e.s.Run.State = agk.Cancelled
	e.s.Run.FinishedAt = now.UTC()
	e.s.Seq++
}

// Outputs are the envelopes the workflow's declared outputs name, which is what a
// finished run hands back. An output is a view of one step port, so it is available once
// that step has ended and not before.
func (e *Evaluator) Outputs() (map[string]agk.Envelope, error) {
	declared := e.g.Workflow().Outputs
	out := make(map[string]agk.Envelope, len(declared))
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		from := declared[name].From
		envelope, err := outputOf(e.s, from.Step, from.Port)
		if err != nil {
			return nil, fmt.Errorf("graph: the workflow output %s: %w", name, err)
		}
		out[name] = envelope
	}
	return out, nil
}

// advance moves one step as far as the state allows: a pending step through its barrier,
// a running step to its end once every shard has finished.
func (e *Evaluator) advance(name agk.Step, now time.Time) error {
	switch e.s.Steps[name].Verdict {
	case agk.VerdictPending:
		return e.begin(name, now)
	case agk.VerdictRunning:
		return e.settle(name, now)
	}
	return nil
}

// begin reads a pending step's barrier and, when it lifts, its condition and its fan-out.
//
// The order is the language's. The barrier first, because nothing about a step is decided
// while an edge has not resolved. Then the edges a merge: first abandoned, because the
// steps behind them are cancelled by this step starting. Then if, which is evaluated
// against the ports the barrier handed over, since that is what lets
// ${{ inputs.in.count > 0 }} be written at all. Then the fan-out, which is how many
// containers this is.
func (e *Evaluator) begin(name agk.Step, now time.Time) error {
	st, ok := e.g.Step(name)
	if !ok {
		return fmt.Errorf("graph: step %s: the run holds a step the graph does not: the state and the graph are of one version", name)
	}

	gate, arrived, reason, err := e.read(name, st)
	if err != nil {
		return e.refused(name, st, err, now)
	}
	switch gate {
	case gateWait:
		return nil
	case gateSkip:
		return e.skip(name, st, reason, now)
	}

	// "The other edges are abandoned and their steps cancelled if no other consumer
	// needs them."
	for _, abandoned := range superseded(e.g, name, arrived.Abandoned, e.s.Steps) {
		e.cancel(abandoned, fmt.Sprintf("the barrier of %s lifted on another edge under merge: first, and no other consumer needs this one", name), now)
	}

	if st.If != "" {
		run, err := e.condition(name, st, arrived)
		if err != nil {
			return e.refused(name, st, err, now)
		}
		if !run {
			// "A step whose if condition is false moves to skipped and publishes
			// empty envelopes on all its ports."
			return e.skip(name, st, fmt.Sprintf("if %s answered false", st.If), now)
		}
	}

	shards, err := fanOut(name, st, arrived.Inputs)
	if err != nil {
		return e.refused(name, st, err, now)
	}
	if len(shards) == 0 {
		// A fan-out over an empty batch starts no containers. The step has run
		// nothing and failed at nothing, and its ports publish empty envelopes like
		// any other step that wrote none.
		return e.end(name, st, StepState{Verdict: agk.VerdictSucceeded}, arrived.Unmatched, now)
	}

	ss := StepState{Verdict: agk.VerdictRunning, Since: now, Shards: make([]ShardState, 0, len(shards))}
	for _, sh := range shards {
		ss.Shards = append(ss.Shards, ShardState{Shard: sh.Shard, Matrix: sh.Matrix, Attempt: 1})
	}
	e.s.Steps[name] = ss
	return nil
}

// settle ends a running step once every shard of it has finished.
func (e *Evaluator) settle(name agk.Step, now time.Time) error {
	ss := e.s.Steps[name]
	verdict := stepVerdict(ss.Shards)
	if !verdict.Terminal() {
		return nil
	}
	st, ok := e.g.Step(name)
	if !ok {
		return fmt.Errorf("graph: step %s: the run holds a step the graph does not", name)
	}
	if verdict == agk.VerdictCancelled {
		// "A cancelled step will not publish at all": it was stopped before it could
		// finish, which is what separates it from a skipped one.
		ss.Verdict, ss.Since = verdict, now
		e.s.Steps[name] = ss
		return nil
	}

	// The items a key join could not match are the engine's to publish, so they are
	// read back out of the arrival that lifted the barrier. Nothing else needs it,
	// which is why it is recomputed here and not held in the state.
	var unmatched []agk.Item
	if st.Merge == MergeJoin {
		_, arrived, _, err := e.read(name, st)
		if err != nil {
			return err
		}
		if arrived != nil {
			unmatched = arrived.Unmatched
		}
	}

	ss.Verdict = verdict
	return e.end(name, st, ss, unmatched, now)
}

// read asks the barrier about one step.
//
// The moment it is read at is the moment the run started, and never the moment somebody
// asked. The only thing the barrier takes a moment for is the stamp on the empty envelope
// an upstream that published nothing arrives as, and a stamp that moved with every
// evaluation would move the cache key of every step below it: the same run has to read
// the same barrier however many times it is asked.
func (e *Evaluator) read(name agk.Step, st *Step) (gate, *arrival, string, error) {
	fed, err := e.fed(name, st)
	if err != nil {
		return gateWait, nil, "", err
	}
	return barrier(name, st, e.g.Edges(name), e.s.Steps, fed, e.s.Run.ID, e.limits, e.s.Run.StartedAt)
}

// shards recomputes the shards a running step was divided into.
//
// They are recomputed rather than stored because they are a function of what the steps
// above published, which the state already holds; storing them would put a second copy of
// every envelope in the run beside the first.
func (e *Evaluator) shards(name agk.Step, st *Step) ([]shard, error) {
	gate, arrived, _, err := e.read(name, st)
	if err != nil {
		return nil, err
	}
	if gate != gateStart {
		return nil, fmt.Errorf("graph: step %s: the step is running and its barrier no longer lifts, which no history of this run can produce", name)
	}
	return fanOut(name, st, arrived.Inputs)
}

// end publishes a step's ports and fixes its verdict. "A port produces exactly one
// envelope, once, when the emitting step ends", whatever that verdict is.
func (e *Evaluator) end(name agk.Step, st *Step, ss StepState, unmatched []agk.Item, now time.Time) error {
	ports, err := publish(name, st, ss.Shards, e.s.Run.ID, now, e.limits)
	if err != nil {
		return err
	}
	if st.Merge == MergeJoin && unmatched != nil {
		envelope, declared, err := unmatchedEnvelope(name, st, unmatched, e.s.Run.ID, now, e.limits)
		if err != nil {
			return err
		}
		if declared {
			ports[unmatchedPort] = envelope
		}
	}
	ss.Ports = ports
	ss.Since = now
	e.s.Steps[name] = ss
	return nil
}

// skip moves a step to skipped and publishes the empty envelopes that says. The reason is
// kept in the words of the rule that decided it, so that a run detail says why a step did
// not run rather than only that it did not.
func (e *Evaluator) skip(name agk.Step, st *Step, reason string, now time.Time) error {
	return e.end(name, st, StepState{Verdict: agk.VerdictSkipped, Reason: reason}, nil, now)
}

// refused fails a step on a rule it broke before a container was ever started: zip on
// envelopes of differing lengths, a batch too large to travel, a condition that does not
// evaluate, a port fed something that is not a batch.
//
// A reading the documentation does not state. Every one of these is the step not being
// given what it asked for, so the step failed, which is a verdict the run states already
// have an answer for: continue_on_error decides whether the run hears about it, and the
// steps below decide their own fate through when. Returning it to the caller instead
// would leave a run at running with nothing to record and no terminal state to reach,
// and the run states have no word for a run whose evaluation stopped.
func (e *Evaluator) refused(name agk.Step, st *Step, cause error, now time.Time) error {
	return e.end(name, st, StepState{Verdict: agk.VerdictFailed, Reason: cause.Error()}, nil, now)
}

// cancel calls a step off. A cancelled step publishes nothing at all, which is what
// separates it from a skipped one: it was stopped before it could finish.
func (e *Evaluator) cancel(name agk.Step, reason string, now time.Time) {
	ss := e.s.Steps[name]
	if ss.Verdict.Terminal() {
		return
	}
	ss.Verdict = agk.VerdictCancelled
	ss.Reason = reason
	ss.Since = now
	e.s.Steps[name] = ss
}

// dispatch names the shards that may be handed out now, under max_parallel. The first
// result says whether a task could not be built, which fails the shard it was for and
// leaves the graph with something more to settle.
func (e *Evaluator) dispatch(plan *Plan, now time.Time) (bool, error) {
	broke := false
	for _, name := range e.g.Order() {
		ss := e.s.Steps[name]
		if ss.Verdict != agk.VerdictRunning {
			continue
		}
		st, ok := e.g.Step(name)
		if !ok {
			continue
		}
		slots := slotsFree(st, ss)
		if slots <= 0 {
			continue
		}

		var divided []shard
		for i := range ss.Shards {
			if slots <= 0 {
				break
			}
			sh := ss.Shards[i]
			if sh.Task != agk.TaskPending || (!sh.NextAttemptAt.IsZero() && now.Before(sh.NextAttemptAt)) {
				continue
			}
			// The fan-out is recomputed once per step and read once per shard,
			// and only for a step that has something to hand out.
			if divided == nil {
				var err error
				if divided, err = e.shards(name, st); err != nil {
					return broke, err
				}
			}
			at := slices.IndexFunc(divided, func(d shard) bool { return d.Shard == sh.Shard })
			if at < 0 {
				return broke, fmt.Errorf("graph: step %s: the run holds the shard %s and the fan-out no longer produces it", name, shardName(sh.Shard))
			}
			task, err := e.task(name, st, divided[at], sh, now)
			if err != nil {
				// A parameter that does not satisfy the manifest, or an
				// expression that does not evaluate, is invalid input: exit
				// 120, "a permanent failure, never retried, whatever retry
				// says". The container never ran, and there is no other word
				// in the vocabulary for a task that could not be built.
				ss.Shards[i].Task = agk.TaskFailed
				ss.Shards[i].ExitCode = 120
				ss.Shards[i].FinishedAt = now
				ss.Reason = err.Error()
				e.s.Steps[name] = ss
				broke = true
				continue
			}
			plan.Start = append(plan.Start, task)
			slots--
		}
	}
	return broke, nil
}

// stops names the tasks in flight that a rule of the language calls off. There are three
// here and a fourth, the run's own deadline and cancellation, is answered before any of
// this is reached.
func (e *Evaluator) stops(plan *Plan) {
	for _, name := range e.g.Order() {
		ss := e.s.Steps[name]
		st, known := e.g.Step(name)

		if ss.Verdict == agk.VerdictCancelled {
			for _, sh := range ss.Shards {
				if holdsARunner(sh) {
					plan.Stop = append(plan.Stop, Stop{Task: e.taskID(name, sh), Reason: StopSuperseded})
				}
			}
			continue
		}

		// "fail_fast: the first shard to fail stops the shards still running beside
		// it." A shard with another attempt coming has not failed yet.
		if ss.Verdict != agk.VerdictRunning || !known || !failFast(st) {
			continue
		}
		for _, sh := range ss.Shards {
			if !sh.NextAttemptAt.IsZero() || !sh.Task.Terminal() {
				continue
			}
			if shardVerdict(sh.Task, sh.ExitCode) != agk.VerdictFailed {
				continue
			}
			for _, sibling := range siblingsInFlight(ss, sh.Shard) {
				plan.Stop = append(plan.Stop, Stop{Task: e.taskID(name, sibling), Reason: StopSiblingFailed})
			}
			break
		}
	}
}

// stopEverything names every task still in flight, for a run that has ended under it.
func (e *Evaluator) stopEverything(reason StopReason) Plan {
	var plan Plan
	for _, name := range e.g.Order() {
		for _, sh := range e.s.Steps[name].Shards {
			if holdsARunner(sh) {
				plan.Stop = append(plan.Stop, Stop{Task: e.taskID(name, sh), Reason: reason})
			}
		}
	}
	return plan
}

// wake is the moment to ask again, and it is zero when nothing waits on the clock. Two
// things do: a retry backoff, which places an attempt in the future, and the root
// timeout, which places the end of the run there.
func (e *Evaluator) wake(now time.Time) time.Time {
	var at time.Time
	for _, name := range e.g.Order() {
		for _, sh := range e.s.Steps[name].Shards {
			if sh.Task != agk.TaskPending || !sh.NextAttemptAt.After(now) {
				continue
			}
			if at.IsZero() || sh.NextAttemptAt.Before(at) {
				at = sh.NextAttemptAt
			}
		}
	}
	if deadline, ok := e.deadline(); ok && deadline.After(now) && (at.IsZero() || deadline.Before(at)) {
		at = deadline
	}
	return at
}

// deadline is the moment the root timeout of the entry point places the end of the run
// at. "timeout at the root bounds the whole run", and a workflow that writes none is
// bounded by the namespace quota instead, which is not this package's to hold.
func (e *Evaluator) deadline() (time.Time, bool) {
	d := time.Duration(e.g.Workflow().Timeout)
	if d <= 0 {
		return time.Time{}, false
	}
	return e.s.Run.StartedAt.Add(d), true
}

// tolerates says whether a step carries continue_on_error, which is the only thing that
// keeps a failed step from failing the run.
func (e *Evaluator) tolerates(name agk.Step) bool {
	st, ok := e.g.Step(name)
	return ok && st.ContinueOnError
}

// taskID composes the identifier of the task a shard is on: the idempotency key, derived
// from what makes the task that task and never minted.
func (e *Evaluator) taskID(name agk.Step, sh ShardState) agk.TaskID {
	return agk.NewTaskID(e.s.Run.ID, name, sh.Attempt, sh.Shard)
}

// task builds one task: one shard of one attempt of one step, carrying everything a
// driver needs and nothing it has to look up.
func (e *Evaluator) task(name agk.Step, st *Step, sh shard, state ShardState, now time.Time) (Task, error) {
	params, err := e.params(name, st, sh, state)
	if err != nil {
		return Task{}, err
	}

	t := Task{
		ID:          e.taskID(name, state),
		Run:         e.s.Run.ID,
		Workflow:    e.s.Run.Workflow,
		Namespace:   e.s.Run.Namespace,
		Commit:      e.s.Run.Commit,
		Step:        name,
		Attempt:     state.Attempt,
		Shard:       sh.Shard,
		Image:       st.Image,
		Call:        st.Call,
		Script:      st.Script,
		Shell:       st.Shell,
		Params:      params,
		Secrets:     e.mounts(name, st),
		Inputs:      sh.Inputs,
		Outputs:     e.collected(name, st),
		Files:       st.Files,
		Resources:   st.Resources,
		Network:     st.Network,
		EgressAllow: st.EgressAllow,
		RunsOn:      st.RunsOn,
		Timeout:     st.Timeout,
		Idempotent:  st.Idempotent,
	}
	if len(st.Script) > 0 {
		t.BeforeScript, t.AfterScript = st.BeforeScript, st.AfterScript
	}

	// "AGK_DEADLINE: the timestamp past which the container will be stopped." The
	// deadline runs from the moment the work became somebody's, so it is measured from
	// the dispatch where the driver has reported one and from this decision where it
	// has not.
	if d := time.Duration(st.Timeout); d > 0 {
		from := state.DispatchedAt
		if from.IsZero() {
			from = now
		}
		t.Deadline = from.Add(d)
	}

	// "Only an idempotent step can be cached", and the key "combines the image digest,
	// the resolved parameters and the digests of the input envelopes", prefixed by the
	// namespace so that it never crosses a boundary.
	if st.Cache && st.Idempotent {
		key, err := cacheKey(e.s.Run.Namespace, st.Image, params, sh.Inputs)
		if err != nil {
			return Task{}, fmt.Errorf("graph: step %s: the cache key: %w", name, err)
		}
		t.Cache, t.CacheKey = true, key
	}
	return t, nil
}

// collected names the ports brick.Collect is asked for.
//
// It is the step's outputs, with one exception the engine makes for itself: the unmatched
// port of a key join is written here and never by the container, so asking a brick for a
// port it has never heard of would be asking for a directory nothing writes. A brick that
// declares one of its own keeps it.
func (e *Evaluator) collected(name agk.Step, st *Step) []agk.Port {
	if st.Merge != MergeJoin || !slices.Contains(st.Outputs, unmatchedPort) {
		return st.Outputs
	}
	if m, ok := e.g.manifest(name); ok && slices.Contains(m.OutputPorts(), unmatchedPort) {
		return st.Outputs
	}
	out := make([]agk.Port, 0, len(st.Outputs))
	for _, port := range st.Outputs {
		if port != unmatchedPort {
			out = append(out, port)
		}
	}
	return out
}

// mounts names the secrets a task is given: the name the workflow knows each by, and the
// path under /agk/secrets/ the brick manifest asks for it at. It carries no value and
// never will.
func (e *Evaluator) mounts(name agk.Step, st *Step) []SecretMount {
	if len(st.Secrets) == 0 {
		return nil
	}
	asked := map[string]string{}
	if m, ok := e.g.manifest(name); ok {
		for _, s := range m.Spec.Secrets {
			asked[s.Name] = s.Mount
		}
	}
	out := make([]SecretMount, 0, len(st.Secrets))
	for _, secret := range st.Secrets {
		// A manifest that declares no mount, and a script step, which has no
		// manifest at all, both get the path the contract names the directory by.
		mount := asked[secret]
		if mount == "" {
			mount = "/agk/secrets/" + secret
		}
		out = append(out, SecretMount{Name: secret, Mount: mount})
	}
	return out
}

// params resolves a step's parameters for one shard and holds the result to the manifest.
//
// "params: brick parameters, validated against the manifest schema once expressions are
// resolved." Check held what the file already settles; this is the other end of the same
// rule, where every expression has a value.
//
// The combination is injected beside them, because a matrix is "every combination is a
// shard, with its variables injected into params". A variable the step also writes itself
// leaves the step's own value standing, since the step is the innermost writer of
// everything else it says, and an injected one is not held to the manifest: it is the
// engine's doing rather than the step's, and a brick that knows nothing of a matrix cannot
// have declared it.
func (e *Evaluator) params(name agk.Step, st *Step, sh shard, state ShardState) (map[string]any, error) {
	scope := paramsScope(e.g.Workflow(), *st)
	context := e.context(sh.Inputs, &sh, state)

	out := make(map[string]any, len(st.Params)+len(sh.Matrix))
	for _, key := range slices.Sorted(maps.Keys(st.Params)) {
		v, err := e.resolve(scope, context, st.Params[key])
		if err != nil {
			return nil, fmt.Errorf("graph: step %s: params.%s: %w", name, key, err)
		}
		out[key] = v
	}
	if m, ok := e.g.manifest(name); ok {
		if err := paramsAgainstManifest(name, *st, m, out, true); err != nil {
			return nil, err
		}
	}
	for _, key := range slices.Sorted(maps.Keys(sh.Matrix)) {
		if _, written := out[key]; !written {
			out[key] = sh.Matrix[key]
		}
	}
	return out, nil
}

// condition evaluates a step's if against the ports its barrier handed over.
func (e *Evaluator) condition(name agk.Step, st *Step, arrived *arrival) (bool, error) {
	t, err := e.template(expr.ScopeStep, st.If)
	if err != nil {
		return false, fmt.Errorf("graph: step %s: if: %w", name, err)
	}
	v, err := expr.Evaluate(t, e.context(arrived.Inputs, nil, ShardState{Attempt: 1}))
	if err != nil {
		return false, fmt.Errorf("graph: step %s: if: %w", name, err)
	}
	return truth(name, st.If, v)
}

// fed resolves the ports a step feeds itself. "inputs feeds a port from a workflow input
// or an expression, with no dependency on another step", so the value is resolved here
// and the envelope that port carries is built out of it.
func (e *Evaluator) fed(name agk.Step, st *Step) (map[agk.Port]agk.Envelope, error) {
	if len(st.Inputs) == 0 {
		return nil, nil
	}
	// The step's own input ports are what is being fed, so there is no port metadata
	// to read yet and the context carries none. Everything else a step reads is here.
	context := e.context(nil, nil, ShardState{Attempt: 1})

	out := make(map[agk.Port]agk.Envelope, len(st.Inputs))
	for _, port := range slices.Sorted(maps.Keys(st.Inputs)) {
		v, err := e.resolve(expr.ScopeStep, context, st.Inputs[port])
		if err != nil {
			return nil, fmt.Errorf("graph: step %s: inputs.%s: %w", name, port, err)
		}
		envelope, err := envelopeOf(e.s.Run.ID, name, port, v, e.s.Run.StartedAt, e.limits)
		if err != nil {
			return nil, err
		}
		out[port] = envelope
	}
	return out, nil
}

// resolve evaluates the expressions one value carries. "An expression that fills the
// whole value keeps its type; an expression embedded in a string is converted to text",
// which is package expr's rule and is applied by Evaluate; what is done here is reaching
// the strings, because a parameter that is a map or a list carries them in its leaves.
func (e *Evaluator) resolve(scope expr.Scope, context expr.Context, v any) (any, error) {
	switch value := v.(type) {
	case string:
		if !strings.Contains(value, "${{") {
			return value, nil
		}
		t, err := e.template(scope, value)
		if err != nil {
			return nil, err
		}
		resolved, err := expr.Evaluate(t, context)
		if err != nil {
			return nil, err
		}
		// A secret filling a whole value travels as the reference it is. The value
		// is not in this process and never will be: "the task message names the
		// secrets it needs and carries none of them".
		if secret, ok := resolved.(expr.Secret); ok {
			return SecretParam{Secret: secret.Name}, nil
		}
		return resolved, nil

	case map[string]any:
		out := make(map[string]any, len(value))
		for _, key := range slices.Sorted(maps.Keys(value)) {
			resolved, err := e.resolve(scope, context, value[key])
			if err != nil {
				return nil, err
			}
			out[key] = resolved
		}
		return out, nil

	case []any:
		out := make([]any, 0, len(value))
		for _, elem := range value {
			resolved, err := e.resolve(scope, context, elem)
			if err != nil {
				return nil, err
			}
			out = append(out, resolved)
		}
		return out, nil
	}
	return v, nil
}

// template compiles one value for one position, once.
func (e *Evaluator) template(scope expr.Scope, source string) (*expr.Template, error) {
	key := templateKey{scope: scope, source: source}
	if t, ok := e.templates[key]; ok {
		return t, nil
	}
	t, err := expr.Interpolate(scope, source)
	if err != nil {
		return nil, err
	}
	e.templates[key] = t
	return t, nil
}

// context fills the roots of the exposed-context table. It fills every one of them and
// the position decides which are declared, so a root a keyword may not read is not in the
// activation the expression is evaluated against however much is passed here.
func (e *Evaluator) context(in map[agk.Port]agk.Envelope, sh *shard, state ShardState) expr.Context {
	wf := e.g.Workflow()
	c := expr.Context{
		Workflow: map[string]any{
			"name":      wf.Metadata.Name,
			"namespace": wf.Metadata.Namespace,
			// "The commit that carries it is the version." There is no second
			// number a workflow is versioned by.
			"version": e.s.Run.Commit,
			"inputs":  e.s.Inputs,
		},
		Run: map[string]any{
			"id":         string(e.s.Run.ID),
			"started_at": e.s.Run.StartedAt,
			// A run has one attempt: a replay is a new run, and the attempt a
			// container is on is the task's, which AGK_ATTEMPT carries.
			"attempt":      1,
			"trigger_kind": e.s.Run.Trigger.String(),
			"triggered_by": e.s.Run.TriggeredBy,
		},
		Trigger: e.s.Trigger,
		Event:   e.s.Event,
		Vars:    e.s.Vars,
		Inputs:  portsMeta(in),
		Steps:   e.stepsMeta(),
		Secrets: e.secrets(),
	}
	if sh != nil {
		c.Matrix = sh.Matrix
		if sh.Item != nil {
			c.Item = itemValue(*sh.Item)
		}
	}
	return c
}

// portsMeta is what an expression may see of the current step's input ports: "count,
// empty, bytes", and never the contents. "The controller therefore never loads whole
// envelopes into memory in order to schedule."
func portsMeta(in map[agk.Port]agk.Envelope) map[string]expr.PortMeta {
	out := make(map[string]expr.PortMeta, len(in))
	for port, envelope := range in {
		out[string(port)] = metaOf(envelope)
	}
	return out
}

// stepsMeta is steps.<id>.status and steps.<id>.outputs.<port>, for the steps the run has
// decided so far.
func (e *Evaluator) stepsMeta() map[string]expr.StepMeta {
	out := make(map[string]expr.StepMeta, len(e.s.Steps))
	for name, ss := range e.s.Steps {
		meta := expr.StepMeta{Status: ss.Verdict.String(), Outputs: make(map[string]expr.PortMeta, len(ss.Ports))}
		for port, envelope := range ss.Ports {
			meta.Outputs[string(port)] = metaOf(envelope)
		}
		out[string(name)] = meta
	}
	return out
}

// secrets are the secrets the workflow names, as opaque references. A Secret carries the
// name and never the value behind it, because nothing in this process has the value.
func (e *Evaluator) secrets() map[string]expr.Secret {
	named := e.g.Workflow().Secrets
	out := make(map[string]expr.Secret, len(named))
	for _, name := range named {
		out[name] = expr.Secret{Name: name}
	}
	return out
}

// metaOf measures one port the way the table exposes it. The bytes are the serialised
// envelope, which is the size the size rules themselves are measured on.
func metaOf(envelope agk.Envelope) expr.PortMeta {
	var bytes int64
	if doc, err := agk.EncodeValue(envelope); err == nil {
		bytes = int64(len(doc))
	}
	return expr.PortMeta{Count: envelope.Meta.Count, Empty: envelope.Meta.Count == 0, Bytes: bytes}
}

// itemValue presents the current item the way an expression reads it, as the document an
// item is: ${{ item.data.customer_id }} is the member of its data.
func itemValue(it agk.Item) any {
	return map[string]any{
		"id":    it.ID,
		"data":  map[string]any(it.Data),
		"files": filesValue(it.Files),
	}
}

// SecretParam is a step parameter whose value is a secret: a reference the runner
// redeems, and never the secret itself.
//
// It exists because "sensitive is the only route by which a secret reaches a brick as a
// parameter rather than as a mounted file", and because a task message "names everything
// the runner needs and contains nothing sensitive". So the parameter travels as the name
// the workflow declared, and the value is put in /agk/params.json by the runner that has
// redeemed its grant, in the container and nowhere else.
type SecretParam struct {
	Secret string `json:"secret"`
}

// envelopeOf turns the value a step feeds one of its own ports into the envelope that
// port carries.
//
// A port carries a batch, so a list is a batch of items and a single object is a batch of
// one: that is what a workflow input declared with an array schema and one declared with
// an object schema each mean on a port. A scalar is refused rather than wrapped, because
// wrapping one would invent the key it sits under, and an item's data is an object.
//
// The identifiers are derived and never minted. The evaluator has no identifier
// generator, and a derived identifier is also what makes a replay exact: the same run fed
// the same value builds the same items, so the cache key of every step below it stays
// where it was.
func envelopeOf(run agk.RunID, step agk.Step, port agk.Port, v any, at time.Time, l agk.Limits) (agk.Envelope, error) {
	var items []agk.Item
	switch value := v.(type) {
	case nil:
		items = []agk.Item{}
	case []any:
		items = make([]agk.Item, 0, len(value))
		for i, elem := range value {
			data, ok := elem.(map[string]any)
			if !ok {
				return agk.Envelope{}, fmt.Errorf("graph: step %s: port %s: inputs feeds the port a list whose entry %d is a %s: an item carries id, data and files, and data is an object", step, port, i, kindOf(elem))
			}
			items = append(items, agk.Item{ID: fedItemID(step, port, i), Data: data, Files: []agk.File{}})
		}
	case map[string]any:
		items = []agk.Item{{ID: fedItemID(step, port, 0), Data: value, Files: []agk.File{}}}
	default:
		return agk.Envelope{}, fmt.Errorf("graph: step %s: port %s: inputs feeds the port a %s: a port carries a batch of items, so the value is an object or a list of objects", step, port, kindOf(v))
	}

	envelope := agk.Envelope{
		Meta: agk.Meta{
			RunID:      run,
			Step:       step,
			Port:       port,
			Attempt:    1,
			Count:      len(items),
			ProducedAt: at.UTC(),
		},
		Items: items,
	}
	if err := envelope.Validate(l); err != nil {
		return agk.Envelope{}, fmt.Errorf("graph: step %s: port %s: %w", step, port, err)
	}
	return envelope, nil
}

// fedItemID is the identity of one item of a port the inputs keyword fed. A port
// publishes once, so the step, the port and the rank name one element of one run.
func fedItemID(step agk.Step, port agk.Port, at int) string {
	return fmt.Sprintf("%s.%s.%d", step, port, at)
}

// cacheKey is the memoisation key: "the digest of the image digest, the resolved
// parameters and the digests of the input envelopes concatenated, prefixed by the
// namespace".
//
// It is computed here and looked up nowhere: "a cache entry is invalidated when an
// artifact it would hand back has expired", and expiry is the store's knowledge.
func cacheKey(namespace, image string, params map[string]any, in map[agk.Port]agk.Envelope) (string, error) {
	key := sha256.New()
	write := func(s string) {
		key.Write([]byte(s))
		key.Write([]byte{0})
	}
	write(image)

	resolved, err := agk.EncodeValue(params)
	if err != nil {
		return "", fmt.Errorf("the resolved parameters: %w", err)
	}
	write(string(resolved))

	// Port by port, in name order, so that one set of inputs has one key.
	for _, port := range slices.Sorted(maps.Keys(in)) {
		doc, err := agk.EncodeValue(in[port])
		if err != nil {
			return "", fmt.Errorf("the envelope on port %s: %w", port, err)
		}
		digest := sha256.Sum256(doc)
		write(string(port))
		write(hex.EncodeToString(digest[:]))
	}
	return namespace + "/sha256/" + hex.EncodeToString(key.Sum(nil)), nil
}

// shardName writes a shard the way AGK_SHARD carries it, and says so where there is none.
func shardName(sh agk.Shard) string {
	if sh.IsZero() {
		return "of a step with no fan-out"
	}
	return sh.String()
}
