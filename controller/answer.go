package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
)

// Taking a result back, which is the other half of the loop.
//
// "It publishes a result on the bus. The controller consumes it, writes the new state,
// evaluates the graph again and publishes whatever has just become runnable."

// Answer is what a runner sent back about one task.
//
// graph.Result is the part the evaluator reads: the task, what became of it, its exit code, the
// envelopes it published and when it ran. The rest is what a person reads, and it is here rather
// than in the evaluator for the reason the evaluator's own comment gives about the shape of the
// handover: what crosses is written once, beside the values that cross it, and a runner's
// measurements are not something a graph decides anything by.
type Answer struct {
	Result graph.Result

	// Row is the task_id of the dispatch the answer is about, carried back unchanged from the
	// task message. A requeue after loss keeps the idempotency key and takes a new task_id, so
	// the key in Result says which unit of work this is and only the row says which dispatch
	// of it.
	Row string

	// Runner is the name of the host that held it. "A user never learns which host executed
	// a task beyond its runner name and labels."
	Runner string

	// Log is where the lines went, how many there were and whether they were cut. Never the
	// lines: "Envelopes and logs are not stored in the database: it keeps only their
	// digests and URIs."
	Log      agk.LogURI
	LogLines int
	LogCut   bool

	// Usage is what the attempt cost, as the runner measured it.
	Usage map[string]any
}

// ErrNotAResult is an answer no controller could ever record: one whose key names no task, or
// whose state is not how a task ends.
//
// It is an error of its own because the consumer has to tell it apart from a result that could
// not be recorded yet. That one is worth delivering again; this one is the same on every
// delivery, so a consumer that delivered it again would deliver it for ever.
var ErrNotAResult = errors.New("controller: not a result any controller could record")

// Answer records one result and decides the run again.
//
// It is called before the message is acknowledged and never after, so that a controller dying
// in the middle gets the result again rather than losing it. Recording twice is free: "A result
// for an attempt that is over changes nothing: the task identifier is the idempotency key, a
// bus is allowed to deliver twice, and a late answer about an attempt already judged is a
// duplicate rather than news."
//
// Free for an ending and for nothing else. The evaluator records any state, because the
// controller itself records a dispatch through it; a runner's answer that said running would
// be taken as news on every delivery, and each would be a decision. So an answer is refused
// with ErrNotAResult before anything is read: "a heartbeat is what says a task is still
// running, and a result saying so would be a result for work that has not finished."
func (co *Core) Answer(ctx context.Context, a Answer) error {
	run, step, _, shard, err := agk.ParseTaskID(string(a.Result.Task))
	if err != nil {
		return fmt.Errorf("%w: it names no task: %w", ErrNotAResult, err)
	}
	if !a.Result.State.Terminal() {
		return fmt.Errorf("%w: %s is %s, which is not one of the five endings a result reports", ErrNotAResult, a.Result.Task, a.Result.State)
	}
	if a.Result.State == agk.TaskLost {
		return co.lose(ctx, run, a)
	}

	var e db.Evaluation
	if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		e, err = w.Run(ctx, run)
		return err
	}); err != nil {
		return err
	}
	if e.State.Terminal() {
		// A run that has ended has nothing to learn. The answer is late rather than
		// wrong, which a cancelled run and a run that timed out both produce.
		return nil
	}
	if len(e.Document) == 0 {
		return fmt.Errorf("controller: a result arrived for run %s, which nothing has decided: a task nobody planned cannot have run", run)
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
	before, _ := shardOf(ev.State(), step, shard)
	if err := ev.Record(a.Result, now); err != nil {
		return fmt.Errorf("controller: the result of %s could not be recorded: %w", a.Result.Task, err)
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
	if after, _ := shardOf(state, step, shard); after.Attempt != before.Attempt {
		tasks = append(tasks, passed(run, step, before, a.Result))
	}
	stamp(tasks, a)

	// "A result for an attempt that is over changes nothing", and the evaluator says so by
	// not counting a decision. There is then nothing to write, and writing it anyway would
	// be refused for taking the run from a sequence to the same sequence.
	if state.Seq == e.Seq {
		return nil
	}

	if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		return w.SaveDecision(ctx, db.Decision{
			Namespace: e.Namespace, Run: run,
			Was: e.Seq, Seq: state.Seq,
			Document:   encoded,
			State:      state.Run.State,
			StartedAt:  state.Run.StartedAt,
			FinishedAt: state.Run.FinishedAt,
			Steps:      steps, Tasks: tasks,
			Envelopes: referencesOf(doc),
			Artifacts: artifactsOf(g, state),
		})
	}); err != nil {
		return err
	}

	// And round again, because a result is the only thing that makes a step downstream of it
	// runnable: "The controller consumes it, writes the new state, evaluates the graph again
	// and publishes whatever has just become runnable."
	return co.Decide(ctx, run)
}

// lose takes a loss a runner reported, which "travels on a result only where a runner recovers
// one it had already lost".
//
// It is written where the heartbeat writes its own and heard the way those are, on the pass that
// follows, rather than recorded here. A loss is about one dispatch, and a requeue keeps the key,
// so the key alone cannot say which dispatch a runner means, and neither can the key and the
// runner together, since the runner that lost a dispatch may be the one holding its requeue. The
// row can. The same loss delivered twice, or delivered late, then finds its dispatch already
// lost, moves nothing, and decides nothing, where recording it would requeue the key a second
// time. A loss naming a dispatch that was never bound to its runner is speaking for somebody
// else's task, and that answer is the same on every delivery.
func (co *Core) lose(ctx context.Context, run agk.RunID, a Answer) error {
	moved := false
	err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		e, err := w.Run(ctx, run)
		if err != nil || e.State.Terminal() {
			return err
		}
		moved, err = w.Lose(ctx, e.Namespace, a.Result.Task, a.Row, a.Runner, co.now().UTC())
		return err
	})
	if errors.Is(err, db.ErrNotHeld) {
		return fmt.Errorf("%w: %w", ErrNotAResult, err)
	}
	if err != nil || !moved {
		return err
	}
	return co.Decide(ctx, run)
}

// shardOf is one shard of one step as a state holds it.
func shardOf(s *graph.State, step agk.Step, shard agk.Shard) (graph.ShardState, bool) {
	for _, sh := range s.Steps[step].Shards {
		if sh.Shard == shard {
			return sh, true
		}
	}
	return graph.ShardState{}, false
}

// passed is the row of the attempt an answer ended, where the evaluator has already moved its
// shard on to the next one.
//
// The projection is the state, and the state holds the attempt a shard is on and nothing of the
// ones before it. A further attempt is a new key, so the row the answer was about drops out of
// what project writes the moment the retry is granted, and left there it would read as
// dispatched for ever: a row counted against max_concurrent_tasks, a grant still honoured for a
// key that has completed, and a task the heartbeat would declare lost once its runner stopped
// listing it. So the ending the answer reported is written on it, from the shard as it stood
// before, and stamp then writes on it who held it as it does for any other row.
func passed(run agk.RunID, step agk.Step, before graph.ShardState, r graph.Result) db.TaskRow {
	ended := before
	ended.Task, ended.ExitCode = r.State, r.ExitCode
	if !r.StartedAt.IsZero() {
		ended.StartedAt = r.StartedAt.UTC()
	}
	if !r.FinishedAt.IsZero() {
		ended.FinishedAt = r.FinishedAt.UTC()
	}
	return taskOf(run, step, ended)
}

// stamp writes onto the projected row of the task the answer is about the things only the answer
// knows: who held it, where its log went, and what it cost.
//
// Only that row. A result says nothing about the other shards of its step, and a projection that
// spread one runner's name across them would be inventing.
func stamp(tasks []db.TaskRow, a Answer) {
	for i := range tasks {
		if tasks[i].ID != a.Result.Task {
			continue
		}
		tasks[i].Runner = a.Runner
		tasks[i].Log = a.Log
		tasks[i].LogLines = a.LogLines
		tasks[i].LogCut = a.LogCut
		tasks[i].Usage = a.Usage
		return
	}
}

// referencesOf is what the database counts, out of what the document names.
func referencesOf(d Document) []db.EnvelopeRef {
	out := make([]db.EnvelopeRef, 0, len(d.Envelopes))
	for _, e := range d.Envelopes {
		shard := e.Shard
		if shard == publishedByTheStep {
			shard = db.PublishedByTheStep
		}
		items := 0
		if d.State != nil {
			if st, ok := d.State.Steps[e.Step]; ok {
				if env, ok := st.Ports[e.Port]; ok && e.Shard == publishedByTheStep {
					items = env.Meta.Count
				}
			}
		}
		out = append(out, db.EnvelopeRef{
			Step: e.Step, Shard: shard, Port: e.Port,
			Digest: e.Digest, Size: e.Size, Items: items,
		})
	}
	return out
}
