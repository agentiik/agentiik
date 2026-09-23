package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
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

	// Row is the task_id of the dispatch the answer is about, carried back unchanged from the
	// task message: on the wire it is the result's task_id, and nothing else is read for it. A
	// requeue after loss keeps the idempotency key and takes a new task_id, so the key in Result
	// says which unit of work this is and only the row says which dispatch of it, which is what
	// a grant was issued for and so what a runner was bound to.
	Row string

	// Runner is the runner that published it. "A user never learns which host executed a task
	// beyond its runner name and labels." It is also who the answer is taken from: the runner
	// the dispatch was bound to, by its redemption, by an ending that never reached a container,
	// or by the ending its host recorded of an earlier dispatch of the key, and no other.
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

// ErrNotTheHolder is a result about a dispatch the runner that published it does not hold: one
// bound to another runner, by its redemption or by an ending that never reached a container; one
// saying a container ran for a dispatch nobody redeemed, from a runner that redeemed no earlier
// dispatch of its key; and a loss of a dispatch the runner never held, nobody's included, since a
// runner cannot lose what it never had.
//
// It always comes wrapped with ErrNotAResult, since no delivery would change it: a binding is never
// released. It has a name of its own because it points somewhere else. A result that is not an
// ending is a runner that misread the wire; this is a machine of the pool speaking for work it was
// never given, which is what a compromised host does, and the security model's "Its reach is the
// tasks in its hands" holds only because something refuses it.
var ErrNotTheHolder = errors.New("controller: a result from a runner that does not hold the task")

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
//
// And taken from one runner. Every machine of a pool can publish a result about any task of the
// pool, and the first ending recorded for an attempt stands, so an answer is matched on its key and
// its dispatch, and then held to the runner that dispatch was bound to at redemption. The dispatch
// and not the key, because a requeue after loss keeps the key and is bound on its own: holding
// the dispatch that was lost gives a runner nothing of a requeue somebody else redeemed, and
// holding the requeue gives nothing of the dispatch it replaced. That Runner is the machine that
// sent it is the bus's to vouch for, and package bus does, by giving each runner a subject only it
// may publish on. Another runner's answer is refused with ErrNotTheHolder before the run is
// decided, and so is a loss of a dispatch nobody redeemed, since a runner cannot lose what it
// never held, and so is one saying a container ran for such a dispatch, but in the one case below.
//
// One saying no container ran is another matter. Of "a refused pull or a grant that would not
// redeem", the second ends a dispatch nobody is bound to: a runner redeems before it pulls, so a
// refused pull is reported by the runner the redemption bound, but a refused redemption binds
// nobody, and the runner reports it all the same, and acknowledges the message only then. The first
// runner to report such an ending is bound to the dispatch as a redemption would have bound it, in
// the transaction that writes the ending, and the answer is taken from it and from no other.
//
// The one case is a requeue that came back to the host which had already ended its key. The
// heartbeat declares a task lost when its host stops reporting, and a host only cut off may have
// run it to its end and reported that ending into the same silence. The requeue is likeliest to
// come back to that host, and certain to where it is its pool's only runner, and the host refuses
// to run the key again, reports the ending it recorded under the requeue's task_id, and
// acknowledges the message once that is published. Nobody redeems the requeue, so nobody else will
// ever answer it, and the run would wait on it until its own timeout, or for ever where it has
// none. So an ending of a dispatch nobody holds is also taken from a runner that redeemed an
// earlier dispatch of the same key, and binds it the same way. Its reach is still the tasks in its
// hands: it was given that key, and a machine that never was is refused as before. An ending that
// is not news writes nothing and binds nobody.
func (co *Core) Answer(ctx context.Context, a Answer) error {
	run, step, _, shard, err := agk.ParseTaskID(string(a.Result.Task))
	if err != nil {
		return fmt.Errorf("%w: it names no task: %w", ErrNotAResult, err)
	}
	if !a.Result.State.Terminal() {
		return fmt.Errorf("%w: %s is %s, which is not one of the five endings a result reports", ErrNotAResult, a.Result.Task, a.Result.State)
	}
	switch {
	case a.Row == "":
		return fmt.Errorf("%w: %s names no dispatch, and a requeue keeps the key, so the key alone cannot say which dispatch ended, nor which runner it was bound to", ErrNotAResult, a.Result.Task)
	case a.Runner == "":
		return fmt.Errorf("%w: %s names no runner, and a result is taken from the runner its task is bound to and from no other", ErrNotAResult, a.Result.Task)
	case len(a.Result.Outputs) > 0:
		return fmt.Errorf("%w: %s carries its envelopes, and a result names them by digest", ErrNotAResult, a.Result.Task)
	}

	// The dispatch is read with the run, by its task_id and its key together, since the two
	// travel as separate fields and one naming a dispatch of another key is an answer assembled
	// out of two. Everything after is about that dispatch. Its binding is who the answer is
	// taken from. And which dispatch of its key it is decides whether the answer is news,
	// because the evaluator counts dispatches and the answer names a row: an ending of a
	// dispatch the key was requeued past after it was lost is the late report of a runner the
	// attempt stopped waiting on, and the requeue it was replaced by is still owed its own.
	var e db.Evaluation
	var holder string
	var redeemedBefore bool
	if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		if e, err = w.Run(ctx, run); err != nil {
			return err
		}
		if holder, err = w.HeldBy(ctx, e.Namespace, a.Result.Task, a.Row); err != nil {
			return err
		}
		if holder == "" && a.Result.State != agk.TaskLost && !unreached(a) {
			if redeemedBefore, err = w.RedeemedBefore(ctx, e.Namespace, a.Result.Task, a.Row, a.Runner); err != nil {
				return err
			}
		}
		a.Result.Requeue, err = w.RequeueOf(ctx, e.Namespace, a.Result.Task, a.Row)
		return err
	}); err != nil {
		// A run nobody holds and a dispatch nobody wrote are the same on every delivery:
		// rows are written before a message leaves, so a result naming none is not early.
		if errors.Is(err, db.ErrNoRun) || errors.Is(err, db.ErrNoDispatch) {
			return fmt.Errorf("%w: %w", ErrNotAResult, err)
		}
		return err
	}
	// An ending that never reached a container, of a dispatch nobody holds, is taken from the
	// runner reporting it, and so is one a host answered from its record of an earlier dispatch
	// of the key it redeemed. Either binds that runner in the transaction that writes the ending
	// and not before. Bound on its own, a pass that failed before the ending was written would
	// leave the dispatch in flight with a runner and no redemption, and the heartbeat, which
	// counts a bound dispatch as held, would declare lost a task that nobody is running.
	bind := holder == "" && (unreached(a) || redeemedBefore)
	if bind {
		holder = a.Runner
	}
	switch {
	case holder == "":
		return fmt.Errorf("%w: %w: %s reported %s for dispatch %s of %s, which no runner has redeemed, and only a task that never reached a container, or whose key that runner redeemed and ended on an earlier dispatch, ends with nobody holding it", ErrNotAResult, ErrNotTheHolder, a.Runner, a.Result.State, a.Row, a.Result.Task)
	case holder != a.Runner:
		return fmt.Errorf("%w: %w: %s reported %s for dispatch %s of %s, which is bound to %s", ErrNotAResult, ErrNotTheHolder, a.Runner, a.Result.State, a.Row, a.Result.Task, holder)
	}
	if e.State.Terminal() {
		// A run that has ended has nothing to learn. The answer is late rather than
		// wrong, which a cancelled run and a run that timed out both produce.
		return nil
	}
	if a.Result.State == agk.TaskLost {
		return co.lose(ctx, run, a)
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
		if err := w.SaveDecision(ctx, db.Decision{
			Namespace: e.Namespace, Run: run,
			Was: e.Seq, Seq: state.Seq,
			Document:   encoded,
			State:      state.Run.State,
			StartedAt:  state.Run.StartedAt,
			FinishedAt: state.Run.FinishedAt,
			Steps:      steps, Tasks: tasks,
			Envelopes: referencesOf(doc),
			Artifacts: artifactsOf(g, state),
		}); err != nil || !bind {
			return err
		}
		// After the decision rather than before it, so that the run's row is locked before
		// the task's, in the order every pass takes them. A runner that redeemed the
		// dispatch since it was read, or ended it the same way, holds it, and the whole of
		// this is undone.
		bound, err := w.BindUnredeemed(ctx, e.Namespace, a.Result.Task, a.Row, a.Runner)
		if err != nil {
			return err
		}
		if bound != a.Runner {
			return fmt.Errorf("%w: %w: %s reported %s for dispatch %s of %s, which was bound to %s before it could be written", ErrNotAResult, ErrNotTheHolder, a.Runner, a.Result.State, a.Row, a.Result.Task, bound)
		}
		return nil
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
// An envelope that could not be read is an error and not a refusal: the runner uploads before it
// reports, and a store that has not got it yet, or cannot be reached, may have it on the next
// delivery. Everything else is a refusal, since it will be the same on every delivery: the digest
// names the bytes, and the bytes do not change. That is an object that is not the envelope its
// digest names, whether too long for one, holding other bytes or not decoding as one, and an
// envelope that disagrees with what the answer says of it.
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
		switch {
		case errors.Is(err, artifact.ErrNotAnEnvelope):
			return nil, fmt.Errorf("%w: %s names on port %s what is not an envelope: %w", ErrNotAResult, a.Result.Task, o.Port, err)
		case err != nil:
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

// unreached says whether an answer is about a task that never reached a container: it is not a
// success, and nothing started, exited or published. It is what a refused pull or a grant that
// would not redeem produces, and the one ending a runner can report for a dispatch it never
// redeemed.
//
// Not a loss, though a loss reports no container either. A loss "travels on a result only where a
// runner recovers one it had already lost", and a runner cannot lose what it never held, so a loss
// of a dispatch nobody redeemed binds nobody and is refused. Bound by it, the runner that reported
// it would then move the dispatch to lost as its holder, and a requeue nobody had taken yet would
// be requeued again on the word of a machine that never had it.
func unreached(a Answer) bool {
	return a.Result.State != agk.TaskSucceeded && a.Result.State != agk.TaskLost &&
		a.Result.StartedAt.IsZero() && a.Result.FinishedAt.IsZero() &&
		a.Result.ExitCode == 0 && len(a.Outputs) == 0
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

// lose takes a loss a runner reported, which "travels on a result only where a runner recovers
// one it had already lost".
//
// It is written where the heartbeat writes its own and heard the way those are, on the pass that
// follows, rather than recorded here. A loss is about one dispatch, and a requeue keeps the key,
// so the key alone cannot say which dispatch a runner means, and neither can the key and the
// runner together, since the runner that lost a dispatch may be the one holding its requeue. The
// row can. The same loss delivered twice, or delivered late, then finds its dispatch already
// lost, moves nothing, and decides nothing, where recording it would requeue the key a second
// time.
//
// Answer has already held it to the runner that dispatch was bound to, as it holds every ending,
// and the runner is the one that published it, which the bus vouches for. Lose holds it to the
// binding once more, in the transaction that moves the row, and a loss it refuses is refused the
// way Answer refuses one: it is speaking for somebody else's task, the same on every delivery.
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
		return fmt.Errorf("%w: %w: %w", ErrNotAResult, ErrNotTheHolder, err)
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
// before, and stamp then writes on it where its log went and what it cost, as it does for any
// other row. Who held it is left as the redemption wrote it.
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
// knows: where its log went, and what it cost.
//
// Only that row, and only the dispatch of it the answer named. A result says nothing about the
// other shards of its step, and a projection that spread one runner's log across them would be
// inventing; nor about the other dispatches of its key, which another runner may hold. Who held
// it is not written here: the redemption wrote it, and the answer was taken because it named the
// same runner.
func stamp(tasks []db.TaskRow, a Answer) {
	for i := range tasks {
		if tasks[i].ID != a.Result.Task || tasks[i].Requeue != a.Result.Requeue {
			continue
		}
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
