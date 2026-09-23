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
// envelopes it published and when it ran. The envelopes are not in it when it arrives. A result
// "carries no item and no artifact content: what travels is a digest per port", so the runner
// uploads what it produced before it reports, the answer names each port's envelope by digest in
// Outputs, and Answer reads them back from the store before the evaluator is shown anything.
//
// The rest is what a person reads, and it is here rather than in the evaluator for the reason the
// evaluator's own comment gives about the shape of the handover: what crosses is written once,
// beside the values that cross it, and a runner's measurements are not something a graph decides
// anything by.
type Answer struct {
	Result graph.Result

	// Runner is the name of the host that held it. "A user never learns which host executed
	// a task beyond its runner name and labels."
	Runner string

	// Outputs are the envelopes the task published, one per port, named rather than carried.
	Outputs []Output

	// Log is where the lines went, how many there were and whether they were cut. Never the
	// lines: "Envelopes and logs are not stored in the database: it keeps only their
	// digests and URIs."
	Log      agk.LogURI
	LogLines int
	LogCut   bool

	// Usage is what the attempt cost, as the runner measured it.
	Usage map[string]any
}

// Output is one port's envelope, named by digest and count as InputRef names one on the way in.
type Output struct {
	Port agk.Port

	// Digest is sixty-four lowercase hexadecimal characters, as an envelope's digest is
	// written everywhere else, and the envelope is in the store under it.
	Digest string
	Items  int
}

// ErrNotAResult is an answer no controller could ever record: one whose key names no task, whose
// state is not how a task ends, or that says of itself what no delivery will change.
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
	if len(a.Result.Outputs) > 0 {
		return fmt.Errorf("%w: %s carries its envelopes, and a result names them by digest", ErrNotAResult, a.Result.Task)
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
	outputs, err := co.published(ctx, e.Namespace, a)
	if err != nil {
		return err
	}
	a.Result.Outputs = outputs

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

// published reads back the envelopes an answer names, which is what the evaluator is shown.
//
// Only a success's. The evaluator keeps the envelopes of a shard that succeeded and of no other,
// and a failed shard's ports are the empty envelopes its step publishes for it, so reading a
// failure's would be fetching what nothing reads, and holding its ending back whenever one of
// them could not be.
//
// An envelope that cannot be read is an error and not a refusal: the runner uploads before it
// reports, and a store that has not got it yet, or cannot be reached, may have it on the next
// delivery. One that is read and disagrees with what the answer says of it is a refusal, since it
// will disagree on every delivery: the digest names the bytes, and the bytes do not change.
func (co *Core) published(ctx context.Context, namespace string, a Answer) (map[agk.Port]agk.Envelope, error) {
	if a.Result.State != agk.TaskSucceeded {
		return nil, nil
	}
	out := make(map[agk.Port]agk.Envelope, len(a.Outputs))
	for _, o := range a.Outputs {
		if _, twice := out[o.Port]; twice {
			return nil, fmt.Errorf("%w: %s names port %s twice", ErrNotAResult, a.Result.Task, o.Port)
		}
		if !isDigest(o.Digest) {
			return nil, fmt.Errorf("%w: %s names %q on port %s, which is not a digest", ErrNotAResult, a.Result.Task, o.Digest, o.Port)
		}
		e, err := get(ctx, namespace, co.objects, EnvelopeRef{Digest: o.Digest}, co.limits)
		if err != nil {
			return nil, fmt.Errorf("controller: the envelope the result of %s names on port %s could not be read: %w", a.Result.Task, o.Port, err)
		}
		switch {
		case e.Meta.Port != o.Port:
			return nil, fmt.Errorf("%w: %s names on port %s an envelope that left by port %s", ErrNotAResult, a.Result.Task, o.Port, e.Meta.Port)
		case e.Meta.Count != o.Items:
			return nil, fmt.Errorf("%w: %s says port %s holds %d items and its envelope holds %d", ErrNotAResult, a.Result.Task, o.Port, o.Items, e.Meta.Count)
		}
		out[o.Port] = e
	}
	return out, nil
}

// isDigest says whether a string is sixty-four lowercase hexadecimal characters, which is what an
// object key is built from. Anything else is refused before the store is asked.
func isDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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
