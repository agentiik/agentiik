package controller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// A run waiting on its tasks, with no root timeout and no backoff, is what most runs are most of
// the time, and the sweep comes round every ten seconds. A pass over such a run decides nothing,
// so it writes nothing and leaves the run out of the next sweep, where it once took the sequence
// one further and wrote the same document on every one.

// fanningWorkflow splits invoice over the orders and feeds what it publishes to archive, so that
// the first result of invoice makes nothing runnable and the last one makes archive runnable.
const fanningWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: archive, port: ok } }
steps:
  invoice:
    image: ` + theImage + `
    inputs:
      orders: ${{ workflow.inputs.orders }}
    strategy: { fan_out: item }
    outputs: [ok]
  archive:
    image: ` + theImage + `
    needs:
      - { step: invoice, port: ok, as: orders }
    outputs: [ok]
`

// createFannedRun creates a run of a workflow whose first step fans out over two orders.
func createFannedRun(t *testing.T, pool *db.Pool, steps ...agk.Step) {
	t.Helper()
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.CreateRun(ctx, db.NewRun{
			ID: decidedRun, Workflow: "monthly-invoicing", Commit: "a3f9c1e",
			Trigger: agk.TriggerManual, TriggeredBy: "alice",
			Inputs: json.RawMessage(`{"orders": [{"customer_id": "C-1042"}, {"customer_id": "C-1043"}]}`),
			Steps:  steps,
		})
	}); err != nil {
		t.Fatal(err)
	}
}

// written is the run's sequence and the transaction that last wrote its row, which moves on any
// write of it, a decision or a clock alike.
func written(t *testing.T, super string) (seq int, xmin string) {
	t.Helper()
	if err := dbtest.Superuser(t, super).QueryRow(t.Context(),
		`select seq, xmin::text from runs where id = $1`, string(decidedRun)).Scan(&seq, &xmin); err != nil {
		t.Fatal(err)
	}
	return seq, xmin
}

// quiet sweeps four times, ten seconds apart, while the runner holding keys reports them all, and
// holds that no sweep found the run, decided it or wrote it. Each sweep is followed by a pass over
// the run all the same, as a notification about it would take, which finds nothing to decide
// either.
func quiet(t *testing.T, co *Core, super, when string, keys ...agk.TaskID) {
	t.Helper()
	seq, xmin := written(t, super)
	for i := 1; i <= 4; i++ {
		clock.advance(10 * time.Second)
		cancelled(t, co, keys...)
		var due []agk.RunID
		if err := co.controller.Fenced(t.Context(), co.term, func(ctx context.Context, w *db.Wide) error {
			var err error
			due, err = w.Actionable(ctx, co.now(), 0)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if holds(due, decidedRun) {
			t.Errorf("%s, sweep %d finds the run actionable", when, i)
		}
		if err := co.Wake(t.Context(), Wake{Swept: true}); err != nil {
			t.Fatal(err)
		}
		if err := co.Wake(t.Context(), Wake{Run: decidedRun}); err != nil {
			t.Fatal(err)
		}
		if now, was := written(t, super); now != seq || was != xmin {
			t.Fatalf("%s, sweep %d and a pass after it took the run from sequence %d to %d and wrote it (xmin %s, then %s)", when, i, seq, now, xmin, was)
		}
	}
}

func TestARunWaitingOnItsTasksIsLeftAloneByEverySweep(t *testing.T) {
	core, q, pool, super := decidingOn(t, fanningWorkflow)
	joinedAsTheRunner(t, super)
	createFannedRun(t, pool, "invoice", "archive")
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	shards := q.dispatched()
	if len(shards) != 2 {
		t.Fatalf("the first pass dispatched %d tasks, want both shards of invoice", len(shards))
	}
	for _, d := range shards {
		if err := core.redeem(t, d, theRunner); err != nil {
			t.Fatal(err)
		}
	}
	quiet(t, core, super, "with both shards running", shards[0].Task.ID, shards[1].Task.ID)

	// A result that makes nothing runnable is written, and then the run is as quiet as before:
	// the result left it due for the pass that follows it, and that pass, finding nothing to
	// decide, put the clock back.
	seq, _ := written(t, super)
	core.answer(t, succeeded(t, shards[0].Task, core.now()))
	if now, _ := written(t, super); now == seq {
		t.Fatal("the result of the first shard was not written")
	}
	if got := q.taken(); len(got) != 0 {
		t.Fatalf("the result of one shard of two sent out %+v", got)
	}
	quiet(t, core, super, "with one shard left", shards[1].Task.ID)

	// And a result that makes archive runnable has it sent out at once, not on a sweep.
	core.answer(t, succeeded(t, shards[1].Task, core.now()))
	archive := q.dispatched()
	if len(archive) != 1 || archive[0].Task.Step != "archive" {
		t.Fatalf("the last result of invoice sent out %+v, want archive", archive)
	}
	if err := core.redeem(t, archive[0], theRunner); err != nil {
		t.Fatal(err)
	}
	quiet(t, core, super, "with archive running", archive[0].Task.ID)
}

// hooked is the installation's versions, with something done before a given call, which is the
// one moment between reading a run and writing its decision that a test can reach.
type hooked struct {
	Versions
	calls  int
	before func(call int) error
}

func (h *hooked) Graph(ctx context.Context, namespace, workflow, commit string) (*graph.Graph, error) {
	h.calls++
	if err := h.before(h.calls); err != nil {
		return nil, err
	}
	return h.Versions.Graph(ctx, namespace, workflow, commit)
}

// A result is written by one transaction and acted on by the pass after it, and a controller can
// stop in between. The run is then due, so the sweep comes for it and sends out what the result
// made runnable.
func TestAResultTheControllerStoppedBeforeActingOnIsSentOutByTheSweep(t *testing.T) {
	core, q, pool, super := deciding(t)
	joinedAsTheRunner(t, super)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	normalize := q.dispatched()
	if len(normalize) != 1 {
		t.Fatalf("the first pass dispatched %d tasks", len(normalize))
	}
	if err := core.redeem(t, normalize[0], theRunner); err != nil {
		t.Fatal(err)
	}

	stopped := errors.New("the controller stopped here")
	core.versions = &hooked{Versions: core.versions, before: func(call int) error {
		// The first call is the answer's own, and the second the pass after it.
		if call == 2 {
			return stopped
		}
		return nil
	}}
	if err := core.Answer(t.Context(), core.answerOf(t, succeeded(t, normalize[0].Task, core.now()))); !errors.Is(err, stopped) {
		t.Fatalf("the answer whose next pass never happened answered %v", err)
	}
	if got := q.taken(); len(got) != 0 {
		t.Fatalf("a pass that never happened sent out %+v", got)
	}

	clock.advance(10 * time.Second)
	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	if got := q.taken(); len(got) != 1 || got[0].Step != "archive" {
		t.Errorf("the sweep after the result sent out %+v, want archive", got)
	}
}

// A task stopped as sibling_failed while its run goes on, whose runner went silent: the heartbeat
// declares it lost after the pass that stops it has read the run and before it writes, so the
// stop is written over a loss and the loss stands. The evaluator ended the task as the stop went
// out and has nothing to hear of its loss, so the run is not woken for it. Woken, each pass
// decided nothing, wrote the stop over the loss again, and woke the run again, on every sweep.
func TestALossOfAStoppedTaskIsNotHeardOnEverySweep(t *testing.T) {
	core, q, pool, super := decidingOn(t, failingFastWorkflow)
	joinedAsTheRunner(t, super)
	createFannedRun(t, pool, "invoice", "archive")
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	var first, second, archive Dispatch
	for _, d := range q.dispatched() {
		switch {
		case d.Task.Step == "archive":
			archive = d
		case d.Task.Shard.Index == 1:
			first = d
		default:
			second = d
		}
	}
	if first.Row == "" || second.Row == "" || archive.Row == "" {
		t.Fatal("the first pass did not dispatch both shards of invoice and archive")
	}
	for _, d := range []Dispatch{first, second, archive} {
		if err := core.redeem(t, d, theRunner); err != nil {
			t.Fatal(err)
		}
	}

	conn := dbtest.Superuser(t, super)
	core.versions = &hooked{Versions: core.versions, before: func(call int) error {
		// The pass after the answer is the one that stops the second shard. Its runner has
		// gone silent, and the heartbeat declares the dispatch lost as it would, with the
		// run due at that moment.
		if call != 2 {
			return nil
		}
		_, err := conn.Exec(t.Context(), `
			with gone as (
			  update tasks set state = 'lost', finished_at = $2 where id = $1 returning run_id
			)
			update runs set wake_at = $2 where id = (select run_id from gone)`,
			second.Row, core.now())
		return err
	}}
	core.answer(t, failed(first.Task, 7, core.now()))
	if stops := q.stops(); !slices.Contains(stops, graph.Stop{Task: second.Task.ID, Reason: graph.StopSiblingFailed}) {
		t.Fatalf("the failure of the first shard stopped %+v", stops)
	}
	if state, _, _ := rowOf(t, super, second.Row); state != "lost" {
		t.Fatalf("the stopped shard lost before its stop was written reads %s", state)
	}
	if got := stateOf(t, core); got != agk.Running {
		t.Fatalf("the run is %s, and archive is still in flight", got)
	}

	quiet(t, core, super, "with the stopped shard lost", archive.Task.ID)
	if state, _, _ := rowOf(t, super, second.Row); state != "lost" {
		t.Errorf("the sweeps moved the lost shard to %s", state)
	}

	// And what the run does wait for is decided at once.
	core.answer(t, succeeded(t, archive.Task, core.now()))
	if got := stateOf(t, core); got != agk.Failed {
		t.Errorf("the run is %s once archive succeeded beside a failed invoice", got)
	}
}
