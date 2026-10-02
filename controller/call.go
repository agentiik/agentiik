package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/schema"
	"github.com/agentiik/agentiik/trigger"
	"github.com/agentiik/agentiik/version"
)

// Calling a workflow from a workflow.
//
// A workflow: step runs no container. Its dispatch is a call: the controller starts a run of the
// workflow it names, by the one path every run takes, "with trigger_kind: workflow and a from naming
// its caller", attributed to the principal the calling run is, charged to the called workflow's
// namespace and held to its concurrency group like any run; and the step waits on that run in place
// of a container, holding no slot, since nothing runs for it. When the run it called ends, its
// caller is woken, and the step ends as the run did: succeeded with the run's outputs as its ports,
// "its ports are the declared outputs of the callee", or failed, so that when and continue_on_error
// act on it as they act on a brick's. A caller cancelled, or past its deadline, cancels the runs it
// called, and a call stopped cancels the run it made.
//
// A call that cannot be made fails the step, and not the run: a call past the depth a chain of calls
// may reach, a workflow that does not exist or that the calling run's principal may not run, which
// are one refusal so that a call learns nothing of a namespace it may not see, inputs the called
// workflow refuses, and a namespace past its max_runs_per_hour.

// callFailure is the exit code a call ends with where it failed, the called run having failed, timed
// out or been cancelled: a failure of the step's own band, which a retry policy may name, as a brick
// exiting 1 is, since the step asked for work and the work failed.
const callFailure = 1

// calls makes the call of each task of the plan that calls a workflow, and answers the plan the
// evaluator makes once each is recorded: dispatched with the run it started, or failed with why it
// could not be made. A call's task is never handed to the bus.
func (co *Core) calls(ctx context.Context, ev *graph.Evaluator, e db.Evaluation, plan graph.Plan, now time.Time) (graph.Plan, error) {
	for {
		var made []graph.Result
		var rest []graph.Task
		for _, t := range plan.Start {
			if t.Call == nil {
				rest = append(rest, t)
				continue
			}
			called, why, err := co.call(ctx, e, t, now)
			if err != nil {
				return graph.Plan{}, err
			}
			requeue := 0
			for _, sh := range ev.State().Steps[t.Step].Shards {
				if sh.Shard == t.Shard {
					requeue = sh.Requeue
				}
			}
			if why != "" {
				made = append(made, graph.Result{
					Task: t.ID, State: agk.TaskFailed, ExitCode: platformFailure, Requeue: requeue,
					FinishedAt: now, Reason: why,
				})
				continue
			}
			made = append(made, graph.Result{Task: t.ID, State: agk.TaskDispatched, DispatchedAt: now, Called: called})
		}
		if len(made) == 0 {
			plan.Start = rest
			return plan, nil
		}
		for _, r := range made {
			if err := ev.Record(r, now); err != nil {
				return graph.Plan{}, fmt.Errorf("the call of %s could not be recorded: %w", r.Task, err)
			}
		}
		stops := plan.Stop
		next, err := ev.Next(now)
		if err != nil {
			return graph.Plan{}, err
		}
		for _, s := range stops {
			if !slices.Contains(next.Stop, s) {
				next.Stop = append(next.Stop, s)
			}
		}
		plan = next
	}
}

// call makes the call one task names, and answers the run it started, or why it could not be made.
// A dispatch that has called a run already, on a pass that died before writing it down, finds that
// run rather than starting a second.
func (co *Core) call(ctx context.Context, e db.Evaluation, t graph.Task, now time.Time) (agk.RunID, string, error) {
	var called db.Called
	err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		called, err = w.CalledBy(ctx, t.ID)
		return err
	})
	switch {
	case err == nil:
		return called.Run, "", nil
	case !errors.Is(err, db.ErrNotCalled):
		return "", "", err
	}
	if co.starter == nil {
		return "", "the step calls a workflow, and this controller was given no way to start a run", nil
	}

	namespace, name, _ := strings.Cut(t.Call.Workflow, "/")
	// A call names the namespace it calls into as its file was written, which may be a name the
	// namespace held before a rename: it calls the namespace that answers to it now.
	if co.controller != nil && co.controller.pool != nil {
		current, err := co.controller.pool.CurrentName(ctx, namespace)
		if err != nil {
			return "", "", err
		}
		namespace = current
	}
	target := t.Call.Workflow
	if t.Call.Ref != "" {
		target += "@" + t.Call.Ref
	}
	depth := e.Depth + 1
	if depth > co.maxCallDepth {
		return "", fmt.Sprintf("the call of %s would be %d calls deep, and a chain of calls is at most %d deep: the call past the limit fails, and not the run that made it", target, depth, co.maxCallDepth), nil
	}
	// "Across namespaces, needs workflow:run on the callee", and within one the run it starts is
	// asked the same when it is let in: asked here, so that the call fails at the call. A
	// workflow that does not exist and one the principal may not run are one refusal, so that a
	// call learns nothing of a namespace it may not see.
	unknown := fmt.Sprintf("%s names no workflow that %s may run: a call needs %s on the workflow it calls", target, e.TriggeredBy, access.WorkflowRun)
	why, err := co.refusal(ctx, db.Evaluation{
		Namespace: namespace, Workflow: name, TriggeredBy: e.TriggeredBy, Trigger: agk.TriggerWorkflow, CreatedAt: now,
	})
	if err != nil {
		return "", "", err
	}
	if why.reason != "" {
		return "", unknown, nil
	}

	inputs, err := supplied(t.CallInputs)
	if err != nil {
		return "", fmt.Sprintf("the inputs the call hands %s could not be written: %v", target, err), nil
	}
	caller := &db.Caller{Run: e.Run, Step: t.Step, Task: t.ID}
	prepared, err := co.starter.Prepare(ctx, trigger.Request{
		Namespace: namespace, Workflow: name, Kind: agk.TriggerWorkflow, By: e.TriggeredBy,
		Ref: t.Call.Ref, Inputs: inputs, Caller: caller, Depth: depth,
	})
	if err != nil {
		if why := callRefusal(target, unknown, err); why != "" {
			return "", why, nil
		}
		return "", "", fmt.Errorf("the call of %s by %s could not be prepared: %w", target, t.ID, err)
	}

	var run agk.RunID
	var reached *db.RunsPerHourReached
	err = co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		return w.Within(ctx, namespace, func(ctx context.Context, ns *db.NS) error {
			var err error
			run, err = prepared.Create(ctx, ns)
			if errors.As(err, &reached) {
				// Committed rather than rolled back, so that the refusal counted where it was
				// decided is kept for the chart of the namespace against its quotas.
				return nil
			}
			return err
		})
	})
	if err == nil && reached != nil {
		err = reached
	}
	if errors.Is(err, db.ErrCalledAlready) {
		// Made by another pass since this one looked.
		return co.call(ctx, e, t, now)
	}
	if err != nil {
		if why := callRefusal(target, unknown, err); why != "" {
			return "", why, nil
		}
		return "", "", fmt.Errorf("the call of %s by %s could not be made: %w", target, t.ID, err)
	}
	return run, "", nil
}

// called says whether a task of the run is a call that made a run.
func called(state *graph.State, task agk.TaskID) bool {
	_, step, attempt, shard, err := agk.ParseTaskID(string(task))
	if err != nil {
		return false
	}
	for _, sh := range state.Steps[step].Shards {
		if sh.Shard == shard && sh.Attempt == attempt {
			return sh.Called != ""
		}
	}
	return false
}

// callRefusal is why a call could not be made, where what refused it is the call's rather than the
// installation's, and empty where it is not.
func callRefusal(target, unknown string, err error) string {
	var input *schema.InputRefusal
	var large *trigger.InputsTooLarge
	var ref *db.RefUnresolved
	var reached *db.RunsPerHourReached
	switch {
	case errors.Is(err, db.ErrNoWorkflow), errors.Is(err, db.ErrNoVersion), errors.As(err, &ref):
		return unknown
	case errors.Is(err, version.ErrLibrary):
		return fmt.Sprintf("%s is a library, which other workflows include and nothing runs or calls", target)
	case errors.As(err, &input):
		return fmt.Sprintf("%s refuses the inputs the call hands it: %s", target, input.Error())
	case errors.As(err, &large):
		return fmt.Sprintf("the inputs the call hands %s are too large: %s", target, large.Error())
	case errors.Is(err, trigger.ErrDeclarationRefused):
		return fmt.Sprintf("%s declares inputs that do not compile, and no call can fill them", target)
	case errors.As(err, &reached):
		return fmt.Sprintf("the call of %s starts no run: %s", target, reached.Reason())
	case errors.Is(err, db.ErrWorkflowMoving):
		return fmt.Sprintf("%s is being moved to another namespace, and starts no run until it is", target)
	}
	return ""
}

// supplied writes the values a call hands over as a run's inputs are supplied: each number a
// json.Number as written, and every value one JSON holds.
func supplied(values map[string]any) (map[string]any, error) {
	if len(values) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var out map[string]any
	return out, d.Decode(&out)
}

// returned records the ending of each call of the run whose called run has ended: succeeded with
// the called run's outputs as the step's ports, or failed with why, as a brick's task ends. It
// answers whether it recorded any.
func (co *Core) returned(ctx context.Context, ev *graph.Evaluator, e db.Evaluation, now time.Time) (bool, error) {
	state := ev.State()
	recorded := false
	for _, step := range slices.Sorted(func(yield func(agk.Step) bool) {
		for name := range state.Steps {
			if !yield(name) {
				return
			}
		}
	}) {
		for _, sh := range state.Steps[step].Shards {
			if sh.Called == "" || sh.Task.Terminal() {
				continue
			}
			task := agk.NewTaskID(state.Run.ID, step, sh.Attempt, sh.Shard)
			var called db.Called
			err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
				var err error
				called, err = w.CalledBy(ctx, task)
				return err
			})
			if errors.Is(err, db.ErrNotCalled) {
				continue
			}
			if err != nil {
				return recorded, err
			}
			if !called.State.Terminal() {
				continue
			}
			r, err := co.answerOfCall(ctx, called, e.Namespace, state.Run.ID, task, step, sh, now)
			if err != nil {
				return recorded, err
			}
			if err := ev.Record(r, now); err != nil {
				return recorded, fmt.Errorf("the ending of the call of %s could not be recorded: %w", task, err)
			}
			recorded = true
		}
	}
	return recorded, nil
}

// answerOfCall is the ending a call's task takes from the run it called, once that run has ended.
func (co *Core) answerOfCall(ctx context.Context, called db.Called, namespace string, run agk.RunID, task agk.TaskID, step agk.Step, sh graph.ShardState, now time.Time) (graph.Result, error) {
	failed := graph.Result{
		Task: task, State: agk.TaskFailed, ExitCode: callFailure, Requeue: sh.Requeue, FinishedAt: now,
		Reason: fmt.Sprintf("the run %s it called, of %s/%s, ended %s", called.Run, called.Namespace, called.Workflow, called.State),
	}
	if called.Reason != "" {
		failed.Reason += ": " + called.Reason
	}
	if called.State != agk.Succeeded {
		return failed, nil
	}
	g, err := co.versions.Graph(ctx, called.Namespace, called.Workflow, called.Commit)
	if err != nil {
		return graph.Result{}, fmt.Errorf("the graph of the run %s called could not be resolved: %w", called.Run, err)
	}
	outputs := map[agk.Port]agk.Envelope{}
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for name := range g.Workflow().Outputs {
			if !yield(name) {
				return
			}
		}
	}) {
		var out db.Output
		err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
			return w.Within(ctx, called.Namespace, func(ctx context.Context, ns *db.NS) error {
				var err error
				out, err = ns.Output(ctx, called.Run, name)
				return err
			})
		})
		if errors.Is(err, db.ErrNoOutput) {
			failed.Reason = fmt.Sprintf("the run %s it called, of %s/%s, succeeded and records no output %s", called.Run, called.Namespace, called.Workflow, name)
			return failed, nil
		}
		if err != nil {
			return graph.Result{}, err
		}
		storage, err := co.storage(ctx, called.Namespace)
		if err != nil {
			return graph.Result{}, err
		}
		envelope, err := artifact.GetEnvelope(ctx, co.objects, storage, out.Envelope.Digest, co.limits)
		if err != nil {
			return graph.Result{}, fmt.Errorf("the output %s of run %s could not be read: %w", name, called.Run, err)
		}
		// A file is kept in its namespace's store, and a task is handed a file by the key of its
		// own: a file a run of another namespace published is one no step of the caller could
		// fetch. The call fails saying so, rather than a step below failing on a fetch.
		if called.Namespace != namespace {
			for _, item := range envelope.Items {
				if len(item.Files) > 0 {
					failed.Reason = fmt.Sprintf("the run %s it called, of %s/%s, succeeded and its output %s carries files, which stay in %s: a call across namespaces hands back items, and files do not cross a namespace", called.Run, called.Namespace, called.Workflow, name, called.Namespace)
					return failed, nil
				}
			}
		}
		// The step's own, as a cache hit's are: the items are the called run's, and one batch
		// names one run, step and port.
		envelope.Meta.RunID, envelope.Meta.Step, envelope.Meta.Port, envelope.Meta.Attempt, envelope.Meta.ProducedAt = run, step, agk.Port(name), sh.Attempt, now.UTC()
		outputs[agk.Port(name)] = envelope
	}
	return graph.Result{
		Task: task, State: agk.TaskSucceeded, Requeue: sh.Requeue, FinishedAt: now, Outputs: outputs,
	}, nil
}
