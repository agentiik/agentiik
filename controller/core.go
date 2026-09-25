package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	ceiling  time.Duration
	requeues int
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

	// Ceiling is how long a task may run when nothing bounded it.
	//
	// It is an installation's answer to a gap rather than a preference. A task message
	// requires a deadline and a grant expires with its task, so a task with neither is a
	// message that cannot be published and a credential that never stops working. The
	// evaluator computes a deadline from the step's timeout or the run's root timeout,
	// and a workflow that declares neither leaves it zero: nothing on the page fixes a
	// default for that case, and refusing to run such a workflow would be inventing a
	// rule rather than filling a gap. The zero value is an hour.
	Ceiling time.Duration

	// MaxRequeues is the installation's max_requeues: how many times one key is handed
	// out again after a loss before the loss stands and fails its step.
	//
	// Nothing else bounds a requeue. A loss uses up no retry.max attempt, since it is
	// charged to the infrastructure, so a step whose container takes down every host it
	// lands on would otherwise be requeued until the run's timeout, and for ever where
	// there is none. It reaches the evaluator as an argument on every pass, as the size
	// rules do, and the evaluator counts it against the key. Nil is
	// graph.DefaultMaxRequeues, three, and zero requeues nothing, as max_requeues: 0 reads
	// wherever the setting is written; graph.Options says why it is a pointer. A negative
	// number is refused.
	//
	// Only a loss counts, and only a dispatch a runner redeemed can be lost: the heartbeat's
	// sweep declares it once that runner goes quiet, or the runner reports it. A message the
	// bus hands to another runner because the first died before redeeming it is the same
	// dispatch delivered again, under its row and its grant, and a task waiting on the queue
	// of a full pool is not lost however long it waits. Neither is handed out again, so
	// neither spends a requeue, and a pool slow to take its work never fails a step for it.
	//
	// The runner's order is what keeps a loss to a runner that went quiet, and package bus
	// sets it out. A runner that never heard its redemption answered keeps the key, names it
	// in its heartbeat and redeems again, rather than letting it go and leaving a task the
	// lost answer had bound to be declared lost before the message came round. A host still
	// running a key redeems nothing of its requeue until the key has ended there, and then
	// answers it from its record, rather than binding a requeue its one ending would never
	// answer. So what spends a requeue is a host the control plane stopped hearing from,
	// which is a host lost as far as it can tell, one only cut off included, and one cut
	// costs its key one requeue however the host comes back.
	MaxRequeues *int
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
	case o.MaxRequeues != nil && *o.MaxRequeues < 0:
		return nil, fmt.Errorf("controller: max_requeues is how many times one key is handed out again after a loss, and %d is no number of times: zero is what requeues nothing", *o.MaxRequeues)
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.Limits == (agk.Limits{}) {
		o.Limits = agk.DefaultLimits()
	}
	if o.Ceiling <= 0 {
		o.Ceiling = time.Hour
	}
	requeues := graph.DefaultMaxRequeues
	if o.MaxRequeues != nil {
		requeues = *o.MaxRequeues
	}
	return &Core{
		controller: c, term: term,
		queue: o.Queue, versions: o.Versions, objects: o.Objects,
		limits: o.Limits, ceiling: o.Ceiling, requeues: requeues, now: o.Now,
	}, nil
}

// Wake is what Watch calls: one run the API named, or a sweep.
//
// A notification decides one run and a sweep decides everything actionable, which is the
// asymmetry the page fixes: "The notification is a latency optimisation; the sweep is the
// correctness guarantee." So a sweep has to reach everything a notification would have, and a
// notification is only ever a shortcut to one of them.
//
// A sweep is also the one thing that notices a silence, since nothing notifies one. So it moves
// to lost, first, every task in flight whose runner has said nothing of it in three heartbeat
// intervals. First, so that the runs it wakes are among those this sweep decides, and the loss is
// heard and requeued on this pass rather than the next.
//
// In a transaction of its own, and a failure of it is reported rather than returned. The runs a
// sweep decides do not depend on it: a loss it could not write is found by the next sweep, and a
// sweep that stopped at it would leave every run of the installation waiting on one statement.
func (co *Core) Wake(ctx context.Context, w Wake) error {
	if !w.Swept {
		return co.Decide(ctx, w.Run)
	}
	now := co.now().UTC()
	if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, wide *db.Wide) error {
		_, err := wide.Lost(ctx, now, 0)
		return err
	}); err != nil {
		if errors.Is(err, db.ErrFenced) || ctx.Err() != nil {
			return err
		}
		co.controller.report("", fmt.Errorf("controller: the sweep could not look for lost tasks: %w", err))
	}
	var runs []agk.RunID
	if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, wide *db.Wide) error {
		var err error
		runs, err = wide.Actionable(ctx, now, 0)
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
	var losses []db.Loss
	var pools []db.RunnerPool
	if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		if e, err = w.Run(ctx, run); err != nil {
			return err
		}
		if losses, err = w.Losses(ctx, e.Namespace, run); err != nil {
			return err
		}
		pools, err = w.RunnerPools(ctx)
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
	if !e.CancelRequestedAt.IsZero() {
		// Somebody holding workflow:run asked, through the API, which wrote the request on
		// the run and nothing else: ending a run and stopping what it holds is one decision,
		// and it is this loop's. Before admission, so that a run queued behind a group is
		// not let in only to be called off.
		return co.Cancel(ctx, run)
	}

	g, err := co.versions.Graph(ctx, e.Namespace, e.Workflow, e.Commit)
	if err != nil {
		return fmt.Errorf("controller: the graph of run %s could not be resolved: %w", run, err)
	}

	// Admission comes before the evaluator does, and it has to: graph.Start stamps the run
	// as started and the root timeout runs from there, so a run admitted late would be a run
	// whose deadline had been running while it queued.
	admit, err := co.admitted(ctx, e, g)
	if err != nil {
		return err
	}
	if !admit {
		return nil
	}

	now := co.now().UTC()
	ev, err := co.resume(ctx, e, g, now)
	if err != nil {
		return err
	}

	// The losses the sweep declared are heard here, before anything is decided. "Three
	// missed intervals move a task to lost", and that is written beside the task state where
	// liveness lives, by whatever noticed; whether the task is then requeued is a decision,
	// and deciding is this loop's. A loss already heard, or one of a dispatch requeued past,
	// is not news, and the evaluator says so by not counting a decision.
	for _, l := range losses {
		if err := ev.Record(graph.Result{
			Task: l.Task, State: agk.TaskLost, Requeue: l.Requeue, FinishedAt: l.At,
		}, now); err != nil {
			return fmt.Errorf("controller: the loss of %s could not be recorded: %w", l.Task, err)
		}
	}

	plan, err := ev.Next(now)
	if err != nil {
		return fmt.Errorf("controller: run %s could not be evaluated: %w", run, err)
	}

	// A step whose pool will not run the namespace ends before anything is counted against
	// the quota, since it will never hold a slot, and before the decision is written, so that
	// the pass that finds it is the pass that fails it.
	if plan, err = refuseUnpooled(ev, e.Namespace, pools, plan, now); err != nil {
		return fmt.Errorf("controller: run %s could not be evaluated: %w", run, err)
	}

	// A stop a rule of the language calls for while the run goes on ends its task here, before
	// the decision is written, so that the row reads cancelled from the moment the stop goes
	// out and the heartbeat repeats it to a runner that missed it on agentiik.stops.
	plan, stopped, err := endStopped(ev, e.Namespace, pools, plan, now)
	if err != nil {
		return fmt.Errorf("controller: run %s could not be evaluated: %w", run, err)
	}

	// What a namespace may hold at once bounds what leaves here, and it bounds it before the
	// decision is written rather than after, so that the row says what was handed out. A task
	// held back is not refused: it stays pending in the evaluator's state, which is what
	// makes the next pass hand it out again. A task this pass stopped holds no slot, though its
	// row reads in flight until the decision is written.
	within, err := co.withinTheQuota(ctx, e.Namespace, plan.Start, stopped)
	if err != nil {
		return err
	}
	plan.Start = within

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
	var held []agk.TaskID
	if state.Seq != saved {
		if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
			if err := w.SaveDecision(ctx, decision); err != nil {
				return err
			}
			if !state.Run.State.Terminal() {
				return nil
			}
			// A run that has just ended ends its tasks here, in the same transaction, as a
			// cancelled run does, whatever its verdict: the evaluator ends the run and
			// leaves its tasks as they were, and a row left in flight would redeem, hold a
			// slot of max_concurrent_tasks for good, and keep the stop out of the
			// heartbeat's cancel, the one place a runner that missed it on agentiik.stops
			// hears it again. A run that reached its deadline is the obvious case, and one
			// that succeeded or failed with a task a merge: first superseded still in flight,
			// whose dispatch the document never saw recorded, is the other.
			var err error
			held, err = w.EndTasks(ctx, e.Namespace, run, now)
			return err
		}); err != nil {
			return err
		}
		saved = state.Seq
	}
	// And the rows name what a runner holds that the document may not, as they do for a
	// cancellation: a pass that published a task and died before recording the dispatch.
	for _, key := range held {
		if !slices.ContainsFunc(plan.Stop, func(s graph.Stop) bool { return s.Task == key }) {
			plan.Stop = append(plan.Stop, graph.Stop{Task: key, Reason: stopOf(state.Run.State)})
		}
	}

	// Committed. Only now does anything leave this process, and everything that does is
	// repeatable: a stop that arrives twice stops a task that is already stopping, and a
	// message that arrives twice carries a key a runner has already seen.
	sent := co.hand(ctx, e.Namespace, run, plan)
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

// endStopped ends as cancelled every task in flight that the plan stops as superseded or
// sibling_failed, asks the evaluator again, and answers the plan with every stop it named on the
// way, until a round ends nothing more, and the keys it ended.
//
// "The task ends: cancelled", says the table of stops, and it ends when the stop goes out rather
// than when its runner reports. The heartbeat's cancel is read off the row, "each key the request
// named whose dispatch, bound to this runner, the controller has ended as cancelled or timed_out",
// and it is the backstop for agentiik.stops, which keeps nothing: a row left in flight until the
// report would keep the stop out of it, and a runner that missed the stop would run the container
// to its deadline, a fail_fast step waiting on it all that time. So the ending is recorded here, as
// a run's own ending records its tasks' in the pass that ends it, and the report that follows adds
// the exit code, the log and the usage, and nothing else. The evaluator keeps a stopped task in flight until a driver
// reports the stop, which is right for a local run, whose driver stops the container itself and
// always reports; a server's stop crosses a bus that may lose it.
//
// Ending a task may end its step, and what follows the step may then start or be stopped in turn,
// so the evaluator is asked again, and its plan is held to the pools as the first one was. Each
// round ends at least one task for good, so there are at most as many rounds as tasks in flight.
func endStopped(ev *graph.Evaluator, namespace string, pools []db.RunnerPool, plan graph.Plan, now time.Time) (graph.Plan, []agk.TaskID, error) {
	var stops []graph.Stop
	var ended []agk.TaskID
	for {
		var ending []graph.Result
		for _, s := range plan.Stop {
			if !slices.Contains(stops, s) {
				stops = append(stops, s)
			}
			if s.Reason != graph.StopSuperseded && s.Reason != graph.StopSiblingFailed {
				continue
			}
			_, step, attempt, shard, err := agk.ParseTaskID(string(s.Task))
			if err != nil {
				return graph.Plan{}, nil, fmt.Errorf("the stop of %s names no task: %w", s.Task, err)
			}
			sh, ok := shardOf(ev.State(), step, shard)
			if !ok || sh.Attempt != attempt || sh.Task == agk.TaskPending || sh.Task.Terminal() {
				continue
			}
			ending = append(ending, graph.Result{
				Task: s.Task, State: agk.TaskCancelled, Requeue: sh.Requeue, NoExitCode: true, FinishedAt: now,
			})
		}
		if len(ending) == 0 {
			plan.Stop = stops
			return plan, ended, nil
		}
		for _, r := range ending {
			if err := ev.Record(r, now); err != nil {
				return graph.Plan{}, nil, fmt.Errorf("the stop of %s could not be recorded: %w", r.Task, err)
			}
			ended = append(ended, r.Task)
		}
		next, err := ev.Next(now)
		if err != nil {
			return graph.Plan{}, nil, err
		}
		if plan, err = refuseUnpooled(ev, namespace, pools, next, now); err != nil {
			return graph.Plan{}, nil, err
		}
	}
}

// stopOf is the stop a run's ending sends to a runner still holding one of its tasks, as the
// documentation's table of stops names it: deadline for a run past its root timeout, cancelled
// for a run called off, and superseded for a run that succeeded or failed. Such a run has ended
// every step, and the only one whose tasks can still be in flight is a step a merge: first
// cancelled when its barrier lifted on another edge, which is what superseded says: a task whose
// dispatch the document never saw recorded, since endStopped ends every one it did see when its
// stop goes out. It is never sibling_failed, since a fail_fast step keeps running until every
// shard of it has ended.
func stopOf(run agk.RunState) graph.StopReason {
	switch run {
	case agk.TimedOut:
		return graph.StopDeadline
	case agk.Cancelled:
		return graph.StopCancelled
	}
	return graph.StopSuperseded
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
		}, graph.Options{Inputs: e.Inputs, Limits: co.limits, MaxRequeues: new(co.requeues)}, now)
	}

	var doc Document
	if err := json.Unmarshal(e.Document, &doc); err != nil {
		return nil, fmt.Errorf("controller: the document of run %s could not be read: %w", e.Run, err)
	}
	state, err := Rehydrate(ctx, doc, e.Namespace, co.objects, co.limits)
	if err != nil {
		return nil, err
	}
	ev, err := graph.New(g, state, co.limits, co.requeues)
	if err != nil {
		return nil, fmt.Errorf("controller: run %s could not be resumed: %w", e.Run, err)
	}
	return ev, nil
}

// hand publishes what was planned, asks for what should stop, and answers what actually went.
//
// A failure here is not a failure of the decision: the decision is committed, and what is left
// is a courier's job. So it is reported, the pass is not unwound, and what did not go stays
// pending in the state, which is what makes the next pass send it again. That includes a task
// its pool refuses here, which the pass refused none of when it read the pools: the next pass
// reads them again and ends the step.
func (co *Core) hand(ctx context.Context, namespace string, run agk.RunID, plan graph.Plan) []agk.TaskID {
	for _, s := range plan.Stop {
		if err := co.queue.Stop(ctx, s); err != nil {
			co.controller.report(run, fmt.Errorf("stopping %s: %w", s.Task, err))
		}
	}

	var sent []agk.TaskID
	for _, t := range plan.Start {
		d, err := co.dispatchOf(ctx, namespace, t)
		if err != nil {
			co.controller.report(run, fmt.Errorf("preparing %s: %w", t.ID, err))
			continue
		}
		if err := co.queue.Publish(ctx, d); err != nil {
			co.controller.report(run, fmt.Errorf("publishing %s: %w", t.ID, err))
			continue
		}
		sent = append(sent, t.ID)
	}
	return sent
}

// dispatchOf turns a task the evaluator decided into everything that leaves this process.
//
// Three things are added here because only the controller can add them. The input envelopes are
// written to the object store and named by digest, so that the message can carry a name where
// graph.Task carries the items. The grant is minted and recorded, so that those names can be
// turned back into values by whoever holds it and by nobody else. And the row is looked up,
// because a grant and a log are addressed by the task's own identifier rather than by the key
// that says which unit of work it is. A key requeued after a loss has a row per dispatch, and the
// one looked up is the one the decision before this has just written, which is how a requeue
// takes a new task_id and a grant of its own.
func (co *Core) dispatchOf(ctx context.Context, namespace string, t graph.Task) (Dispatch, error) {
	// A task nothing bounded gets the installation's ceiling, because a message requires a
	// deadline and a grant expires with its task. It is a fallback and never an override:
	// where the evaluator computed one, that one stands.
	if t.Deadline.IsZero() {
		t.Deadline = co.now().UTC().Add(co.ceiling)
	}
	d := Dispatch{Task: t, Inputs: map[agk.Port]InputRef{}}

	// The bytes first. Content addressed, so a task republished after a refused publish
	// writes nothing, and a shard whose inputs are the step's whole envelope shares the
	// object the publication already put there.
	for port, e := range t.Inputs {
		ref, err := put(ctx, namespace, co.objects, e)
		if err != nil {
			return Dispatch{}, fmt.Errorf("the input on %s could not be written: %w", port, err)
		}
		d.Inputs[port] = InputRef{Digest: ref.Digest, Items: e.Meta.Count}
	}

	err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		// The pool's policy before the grant, so that a task no runner may be handed is
		// given no credential either, and read in the transaction that issues it.
		pool, resources, err := policed(ctx, w, namespace, t)
		if err != nil {
			return err
		}
		d.Pool, d.Task.Resources = pool, resources
		row, err := w.TaskRow(ctx, namespace, t.ID)
		if err != nil {
			return err
		}
		d.Row = row
		granted, err := w.IssueGrant(ctx, namespace, t.ID, row, scopeOf(d.Task, d.Inputs), t.Deadline)
		if err != nil {
			return err
		}
		d.Grant = granted.Clear
		return nil
	})
	if err != nil {
		return Dispatch{}, err
	}
	return d, nil
}

// scopeOf is what the grant may be turned into, which only the controller can say.
//
// "the controller names which secret a task may have and never sees its value", and the same is
// true of every other thing a task is allowed to fetch: what is written here is the whole of what
// the redemption will answer, so a runner holding a grant reaches the files of the commit this
// run pinned, the envelopes on this task's input ports, the secrets this step declared, and
// nothing else in the namespace. The commit is written here for the reason the rest is: "the
// controller resolves a commit to a tree", and a runner that named its own would reach every
// version in the namespace.
func scopeOf(t graph.Task, inputs map[agk.Port]InputRef) db.GrantScope {
	scope := db.GrantScope{Run: t.Run, Step: t.Step, Workflow: t.Workflow, Commit: t.Commit}
	ports := make([]agk.Port, 0, len(inputs))
	for port := range inputs {
		ports = append(ports, port)
	}
	// Ordered, because the scope is written as a document and two dispatches of one task
	// that differ only in map iteration order would be two different documents.
	slices.Sort(ports)
	for _, port := range ports {
		ref := inputs[port]
		scope.Inputs = append(scope.Inputs, db.GrantInput{
			Port: port, Digest: ref.Digest, Items: ref.Items,
		})
	}
	// Each with the mount the evaluator resolved from the manifest, which is the one the task
	// message carries: a redemption that derived the path from the name would put a value
	// where the brick is not looking for it.
	for _, m := range t.Secrets {
		scope.Secrets = append(scope.Secrets, db.GrantSecret{Name: m.Name, Mount: m.Mount})
	}
	return scope
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
		Requeue: sh.Requeue,

		DispatchedAt: sh.DispatchedAt,
		StartedAt:    sh.StartedAt,
		FinishedAt:   sh.FinishedAt,
	}
	// A code is written for every ending that carries one. The evaluator reads a code only for a
	// task that succeeded or failed, since a stop and not the code decided the verdict of one
	// stopped, but a stopped container exits too: "a timed_out or cancelled task carries an
	// exit code wherever a container ran", 143 where it obeyed SIGTERM and 137 where it was
	// killed after the grace, and that is what a person reading the run is owed. A lost task
	// carries none, because nothing came back. Nor does an ending that never reached a
	// container, which a runner reports with no exit code at all and the shard holds as 0, the
	// code of success, nor a stopped container its runner reported no code for. So a code is
	// written where a container started and reported one, and where the evaluator gave one to a
	// task it could not build, which is 120 and never 0.
	switch sh.Task {
	case agk.TaskSucceeded, agk.TaskFailed, agk.TaskTimedOut, agk.TaskCancelled:
		if (!sh.StartedAt.IsZero() && !sh.NoExitCode) || sh.ExitCode != 0 {
			code := sh.ExitCode
			t.ExitCode = &code
		}
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
