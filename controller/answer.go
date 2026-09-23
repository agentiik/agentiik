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
	run, _, _, _, err := agk.ParseTaskID(string(a.Result.Task))
	if err != nil {
		return fmt.Errorf("%w: it names no task: %w", ErrNotAResult, err)
	}
	if !a.Result.State.Terminal() {
		return fmt.Errorf("%w: %s is %s, which is not one of the five endings a result reports", ErrNotAResult, a.Result.Task, a.Result.State)
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
