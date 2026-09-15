package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
)

// The loop, which is the whole of what a controller does.
//
// "The controller evaluates the graph at that commit and finds the steps whose every declared
// input port is already satisfied. For each of them it creates one task per shard and publishes
// those tasks on the queue that matches the step's runs_on labels." Every rule behind that
// sentence, the barrier, the fan-out, the merge strategies, retry, fail_fast, the run verdict,
// belongs to package graph and is already written. Nothing here reimplements any of it: the
// loop reads a state, hands it to the evaluator, and writes down what came back.
//
// That is the point rather than an economy. "The graph evaluator and the container driver stay
// importable libraries with no server, no bus and no database behind them ... it is what makes
// agk run --local the same code path as a server run rather than a second implementation that
// drifts." A controller with a scheduler of its own inside it would be that second
// implementation.

// Core is the controller deciding.
//
// It holds nothing a restart could not read back. The evaluator it builds per pass is thrown
// away at the end of it, which is what makes "failover is a state resume, never a rebuild" a
// property of the design rather than a promise about shutdown.
type Core struct {
	controller *Controller
	term       db.Term

	queue    Queue
	versions Versions
	objects  artifact.Objects
	limits   agk.Limits
	now      func() time.Time
}

// Options are what a Core is given. Everything in it is somebody else's work: the bus, the
// object store, and whoever can resolve a commit to a graph.
type Options struct {
	Queue    Queue
	Versions Versions
	Objects  artifact.Objects

	// Limits are the size rules. The zero value is agk.DefaultLimits().
	Limits agk.Limits

	// Now is the clock, an argument so that a test has one and so that a pass is taken at
	// one instant rather than at several: the evaluator's Next "is an argument rather than
	// a clock so that evaluation is pure and a replay is exact".
	Now func() time.Time
}

// NewCore builds the deciding half of a controller, for the term it holds.
func NewCore(c *Controller, term db.Term, o Options) (*Core, error) {
	switch {
	case c == nil:
		return nil, errors.New("controller: a core with no controller behind it")
	case o.Queue == nil:
		return nil, errors.New("controller: a core with no queue, and a decided task that goes nowhere is a run that never moves")
	case o.Versions == nil:
		return nil, errors.New("controller: a core with no way to resolve a commit to a graph")
	case o.Objects == nil:
		return nil, errors.New("controller: a core with no object store, and the envelopes live there")
	case term.Token < 1:
		return nil, errors.New("controller: a core outside a term: deciding is what the election decides who may do")
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.Limits == (agk.Limits{}) {
		o.Limits = agk.DefaultLimits()
	}
	return &Core{
		controller: c, term: term,
		queue: o.Queue, versions: o.Versions, objects: o.Objects,
		limits: o.Limits, now: o.Now,
	}, nil
}

// Wake is what Watch calls: one run the API named, or a sweep.
//
// A notification decides one run and a sweep decides everything actionable, which is the
// asymmetry the page fixes: "The notification is a latency optimisation; the sweep is the
// correctness guarantee." So a sweep has to reach everything a notification would have, and a
// notification is only ever a shortcut to one of them.
func (co *Core) Wake(ctx context.Context, w Wake) error {
	if !w.Swept {
		return co.Decide(ctx, w.Run)
	}
	var runs []agk.RunID
	if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, wide *db.Wide) error {
		var err error
		runs, err = wide.Actionable(ctx, co.now(), 0)
		return err
	}); err != nil {
		return err
	}
	for _, run := range runs {
		if err := co.Decide(ctx, run); err != nil {
			// One run that cannot be decided does not stop the sweep, because the
			// sweep is what the other runs depend on. A run that fails every pass
			// fails visibly through its own state rather than by taking the
			// installation down with it.
			if errors.Is(err, db.ErrFenced) || ctx.Err() != nil {
				return err
			}
			co.controller.report(run, err)
		}
	}
	return nil
}

// Decide takes one pass over one run.
//
// Read, evaluate, write, publish, in that order and for that reason. The read and the write are
// separate transactions because what happens between them reaches the object store, and a
// transaction held open across a fetch of up to envelope_max_bytes holds a row lock for as long
// as somebody else's disk takes. The sequence the document was read at is what closes that gap:
// a pass written against a state that has moved is refused whole.
func (co *Core) Decide(ctx context.Context, run agk.RunID) error {
	var e db.Evaluation
	if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		e, err = w.Run(ctx, run)
		return err
	}); err != nil {
		return err
	}
	if e.State.Terminal() {
		// Nothing to decide, and nothing to repair either: a finished run's tasks are
		// finished. The sweep does not offer these, so this is the notification arriving
		// after the fact.
		return nil
	}

	g, err := co.versions.Graph(ctx, e.Namespace, e.Workflow, e.Commit)
	if err != nil {
		return fmt.Errorf("controller: the graph of run %s could not be resolved: %w", run, err)
	}

	now := co.now().UTC()
	ev, err := co.resume(ctx, e, g, now)
	if err != nil {
		return err
	}

	plan, err := ev.Next(now)
	if err != nil {
		return fmt.Errorf("controller: run %s could not be evaluated: %w", run, err)
	}

	state := ev.State()
	doc, err := Elide(ctx, state, e.Namespace, co.objects)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("controller: the document of run %s could not be written: %w", run, err)
	}

	steps, tasks := project(state)
	stampDeadlines(tasks, plan)
	decision := db.Decision{
		Namespace: e.Namespace, Run: run,
		Was: e.Seq, Seq: state.Seq,
		Document:   encoded,
		State:      state.Run.State,
		StartedAt:  state.Run.StartedAt,
		FinishedAt: state.Run.FinishedAt,
		WakeAt:     plan.Wake,
		Steps:      steps, Tasks: tasks,
		Envelopes: referencesOf(doc),
		Artifacts: artifactsOf(g, state),
	}
	if state.Run.State.Terminal() {
		if outputs, err := ev.Outputs(); err == nil {
			decision.Outputs = digestsOf(outputs)
		}
	}
	// A pass that decided nothing writes nothing. The evaluator counts decisions, so a
	// sequence that has not moved is the honest statement that this pass was a no-op:
	// asking again at the same instant is idempotent by design, and the commonest case is
	// a sweep reaching a run that is simply waiting.
	saved := e.Seq
	if state.Seq != saved {
		if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
			return w.SaveDecision(ctx, decision)
		}); err != nil {
			return err
		}
		saved = state.Seq
	}

	// Committed. Only now does anything leave this process, and everything that does is
	// repeatable: a stop that arrives twice stops a task that is already stopping, and a
	// message that arrives twice carries a key a runner has already seen.
	sent := co.hand(ctx, run, plan)
	if len(sent) == 0 {
		return nil
	}

	// And the dispatch is recorded once the message has gone, not when the task was
	// planned. That is what the state means: "dispatched: Handed out. The task's deadline
	// runs from here, because that is when the work became somebody's", and a task whose
	// message the bus refused was handed out to nobody.
	//
	// It is also what closes the outbox. A task the evaluator has not seen dispatched is
	// one it plans again on the next pass, so a message that never went is republished by
	// the thing that decided it rather than by a second mechanism that would have to
	// rebuild a task message from rows.
	for _, t := range plan.Start {
		if !contains(sent, t.ID) {
			continue
		}
		if err := ev.Record(graph.Result{
			Task: t.ID, State: agk.TaskDispatched, DispatchedAt: now,
		}, now); err != nil {
			return fmt.Errorf("controller: the dispatch of %s could not be recorded: %w", t.ID, err)
		}
	}
	if state.Seq == saved {
		// The messages went and the evaluator learned nothing from it, which happens
		// only when every one of them was a task it had already seen dispatched.
		return co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
			_, err := w.Published(ctx, e.Namespace, sent, co.now().UTC())
			return err
		})
	}
	dispatched, err := Elide(ctx, state, e.Namespace, co.objects)
	if err != nil {
		return err
	}
	encoded, err = json.Marshal(dispatched)
	if err != nil {
		return fmt.Errorf("controller: the document of run %s could not be written: %w", run, err)
	}
	steps, tasks = project(state)
	stampDeadlines(tasks, plan)
	return co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		if err := w.SaveDecision(ctx, db.Decision{
			Namespace: e.Namespace, Run: run,
			Was: saved, Seq: state.Seq,
			Document:   encoded,
			State:      state.Run.State,
			StartedAt:  state.Run.StartedAt,
			FinishedAt: state.Run.FinishedAt,
			WakeAt:     plan.Wake,
			Steps:      steps, Tasks: tasks,
			Envelopes: referencesOf(dispatched),
			Artifacts: artifactsOf(g, state),
		}); err != nil {
			return err
		}
		_, err := w.Published(ctx, e.Namespace, sent, co.now().UTC())
		return err
	})
}

// contains says whether an identifier is in a list, which two places here need.
func contains(ids []agk.TaskID, want agk.TaskID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// resume builds the evaluator for a run, starting it where nothing has decided yet.
func (co *Core) resume(ctx context.Context, e db.Evaluation, g *graph.Graph, now time.Time) (*graph.Evaluator, error) {
	if len(e.Document) == 0 {
		// Nothing has decided this run, so it starts here. Admission, which is what
		// queued is waiting on, is the concurrency group's and arrives with it; until
		// then a run starts the moment the controller reaches it.
		return graph.Start(g, agk.Run{
			ID: e.Run, Workflow: e.Workflow, Namespace: e.Namespace, Commit: e.Commit,
			Trigger: e.Trigger,
		}, graph.Options{Inputs: e.Inputs, Limits: co.limits}, now)
	}

	var doc Document
	if err := json.Unmarshal(e.Document, &doc); err != nil {
		return nil, fmt.Errorf("controller: the document of run %s could not be read: %w", e.Run, err)
	}
	state, err := Rehydrate(ctx, doc, e.Namespace, co.objects, co.limits)
	if err != nil {
		return nil, err
	}
	ev, err := graph.New(g, state, co.limits)
	if err != nil {
		return nil, fmt.Errorf("controller: run %s could not be resumed: %w", e.Run, err)
	}
	return ev, nil
}

// hand publishes what was planned, asks for what should stop, and answers what actually went.
//
// A failure here is not a failure of the decision: the decision is committed, and what is left
// is a courier's job. So it is reported, the pass is not unwound, and what did not go stays
// pending in the state, which is what makes the next pass send it again.
func (co *Core) hand(ctx context.Context, run agk.RunID, plan graph.Plan) []agk.TaskID {
	for _, s := range plan.Stop {
		if err := co.queue.Stop(ctx, s); err != nil {
			co.controller.report(run, fmt.Errorf("stopping %s: %w", s.Task, err))
		}
	}

	var sent []agk.TaskID
	for _, t := range plan.Start {
		if err := co.queue.Publish(ctx, t); err != nil {
			co.controller.report(run, fmt.Errorf("publishing %s: %w", t.ID, err))
			continue
		}
		sent = append(sent, t.ID)
	}
	return sent
}

// project turns a state into the rows everything that queries reads.
//
// It is a projection and not the state: what is authoritative is the document, and these rows
// exist so that a console, an API and a purge can ask questions of a run without understanding
// the evaluator.
func project(s *graph.State) ([]db.StepRow, []db.TaskRow) {
	steps := make([]db.StepRow, 0, len(s.Steps))
	var tasks []db.TaskRow

	names := make([]agk.Step, 0, len(s.Steps))
	for name := range s.Steps {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })

	for _, name := range names {
		st := s.Steps[name]
		row := db.StepRow{Step: name, Verdict: st.Verdict}
		if st.Verdict.Terminal() {
			row.FinishedAt = st.Since
		}
		for _, sh := range st.Shards {
			// The highest attempt any shard reached, which is what one number is
			// worth to a person reading a run.
			if sh.Attempt > row.Attempts {
				row.Attempts = sh.Attempt
			}
			if !sh.DispatchedAt.IsZero() && (row.StartedAt.IsZero() || sh.DispatchedAt.Before(row.StartedAt)) {
				row.StartedAt = sh.DispatchedAt
			}
			tasks = append(tasks, taskOf(s.Run.ID, name, sh))
		}
		steps = append(steps, row)
	}
	return steps, tasks
}

// stampDeadlines writes each planned task's deadline onto its row.
//
// The deadline is not in the state and cannot be: it is computed from the step's timeout and the
// moment the work became somebody's, so the evaluator puts it on the task it hands out rather
// than on the shard it hands out from. The row wants it all the same, because it is what a
// person reading a stuck run looks at and what a lost-task sweep will compare against.
func stampDeadlines(tasks []db.TaskRow, plan graph.Plan) {
	if len(plan.Start) == 0 {
		return
	}
	by := make(map[agk.TaskID]time.Time, len(plan.Start))
	for _, t := range plan.Start {
		by[t.ID] = t.Deadline
	}
	for i := range tasks {
		if at, ok := by[tasks[i].ID]; ok {
			tasks[i].Deadline = at
		}
	}
}

// taskOf is one shard, as the projection holds it.
func taskOf(run agk.RunID, step agk.Step, sh graph.ShardState) db.TaskRow {
	t := db.TaskRow{
		ID:      agk.NewTaskID(run, step, sh.Attempt, sh.Shard),
		Step:    step,
		State:   sh.Task,
		Attempt: sh.Attempt,
		Shard:   sh.Shard,

		DispatchedAt: sh.DispatchedAt,
		StartedAt:    sh.StartedAt,
		FinishedAt:   sh.FinishedAt,
	}
	// "ExitCode is read for a task that succeeded or failed and for no other state", which
	// the column says too: a task stopped at its deadline or by a cancellation decided
	// nothing and has no code of its own to carry.
	if sh.Task == agk.TaskSucceeded || sh.Task == agk.TaskFailed {
		code := sh.ExitCode
		t.ExitCode = &code
	}
	return t
}

// digestsOf reduces a run's outputs to what the database is allowed to hold.
//
// "The outputs are digests and not envelopes for the reason the chapter gives: the database
// keeps the digest and the URI, and the bytes live in the object store."
func digestsOf(outputs map[string]agk.Envelope) map[string]any {
	out := make(map[string]any, len(outputs))
	for name, e := range outputs {
		out[name] = map[string]any{
			"step":  string(e.Meta.Step),
			"port":  string(e.Meta.Port),
			"count": e.Meta.Count,
		}
	}
	return out
}
