package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// A result delivered twice is written once. The bus delivers at least once, so the second
// delivery is the ordinary case rather than a fault, and the test of it is the sequence: a
// sequence that moved is a decision written down and a run decided again.

// An attempt a retry replaced is over, though its shard is pending again. The failure that was
// granted the retry, delivered a second time, is a duplicate rather than a second failure: it
// neither spends another attempt nor sends one out early.
func TestAResultForAnAttemptAlreadyRetriedChangesNothing(t *testing.T) {
	core, q, pool, super := decidingOn(t, retryingWorkflow)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.taken()
	if len(first) != 1 || first[0].Attempt != 1 {
		t.Fatalf("the first pass published %+v", first)
	}
	core.answer(t, failed(first[0], 1, core.now()))
	if got := q.taken(); len(got) != 0 {
		t.Fatalf("the retry was published before its backoff had passed: %+v", got)
	}

	conn := dbtest.Superuser(t, super)
	before := seqOf(t, conn)
	core.answer(t, failed(first[0], 1, core.now()))
	if after := seqOf(t, conn); after != before {
		t.Errorf("the failure of attempt 1, delivered again, took the run from seq %d to %d", before, after)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("the failure of attempt 1, delivered again, published %+v", got)
	}

	// And the retry is still the one the first delivery was granted: attempt 2, once the
	// backoff has passed, and not attempt 3.
	clock.advance(time.Minute)
	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	if second := q.taken(); len(second) != 1 || second[0].Attempt != 2 {
		t.Errorf("after the backoff the sweep published %+v, want attempt 2 alone", second)
	}
}

// dying stands for a controller that writes a result down and dies before deciding the run
// again. Answer resolves the graph once to record the result, and Decide resolves it again;
// the second resolution is where this one stops.
type dying struct {
	Versions

	mu    sync.Mutex
	calls int
}

func (d *dying) Graph(ctx context.Context, namespace, workflow, commit string) (*graph.Graph, error) {
	d.mu.Lock()
	d.calls++
	calls := d.calls
	d.mu.Unlock()
	if calls > 1 {
		return nil, errors.New("the controller died before deciding the run again")
	}
	return d.Versions.Graph(ctx, namespace, workflow, commit)
}

// A result whose decision committed and whose run was never decided again comes back, because
// the bus heard an error rather than an acknowledgement. The second delivery is a duplicate and
// writes nothing, so what the first one made runnable is the sweep's to publish: the decision
// Answer writes leaves no wake time, and a run with none is one the sweep reaches.
func TestARedeliveryAfterTheDecisionCommittedIsANoOp(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	taken := q.taken()
	if len(taken) != 1 || taken[0].Step != "normalize" {
		t.Fatalf("the first pass published %+v", taken)
	}

	conn := dbtest.Superuser(t, super)
	before := seqOf(t, conn)
	dies, err := NewCore(core.controller, core.term, Options{
		Queue: q, Versions: &dying{Versions: core.versions}, Objects: core.objects, Now: core.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	answer := core.answerOf(t, succeeded(t, taken[0], core.now()))
	if err := dies.Answer(t.Context(), answer); err == nil {
		t.Fatal("a controller that died before deciding the run again answered as if it had")
	}
	written := seqOf(t, conn)
	if written == before {
		t.Fatal("the result was not written before the controller died, so this is not the case under test")
	}
	if got := q.taken(); len(got) != 0 {
		t.Fatalf("a controller that died before deciding the run again published %+v", got)
	}

	// The redelivery, to a controller that is alive.
	if err := core.Answer(t.Context(), answer); err != nil {
		t.Fatalf("a result redelivered after its decision committed was refused: %s", err)
	}
	if after := seqOf(t, conn); after != written {
		t.Errorf("the redelivery took the run from seq %d to %d, and it is a duplicate rather than news", written, after)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("the redelivery published %+v, and a duplicate decides nothing", got)
	}

	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	if got := q.taken(); len(got) != 1 || got[0].Step != "archive" {
		t.Errorf("the sweep published %+v, want archive, which the result made runnable", got)
	}
}

// "A heartbeat is what says a task is still running, and a result saying so would be a result
// for work that has not finished." One that says so anyway is refused, and so is one whose key
// names no task, both before anything is read and with an error the bus can tell apart from a
// result that could not be recorded yet.
func TestAResultThatIsNotAnEndingIsRefused(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	taken := q.taken()
	if len(taken) != 1 {
		t.Fatalf("the first pass published %d tasks", len(taken))
	}
	task := taken[0].ID

	conn := dbtest.Superuser(t, super)
	before := seqOf(t, conn)
	for _, c := range []struct {
		result graph.Result
		why    string
	}{
		{graph.Result{Task: task, State: agk.TaskRunning}, "running, which a heartbeat says"},
		{graph.Result{Task: task, State: agk.TaskPending}, "pending, which no runner has seen"},
		{graph.Result{Task: task, State: agk.TaskState(99)}, "a state that is not one"},
		{graph.Result{Task: "normalize", State: agk.TaskSucceeded}, "a key that is a step and not a task"},
		{graph.Result{Task: "", State: agk.TaskSucceeded}, "no key at all"},
		// A run nobody holds, which would be db.ErrNoRun had anything been read first.
		{graph.Result{Task: agk.NewTaskID(agk.NewRunID(), "normalize", 1, agk.Shard{}), State: agk.TaskRunning}, "running, for a run nobody holds"},
	} {
		err := core.Answer(t.Context(), Answer{Result: c.result, Runner: "runner-dmz-02"})
		if !errors.Is(err, ErrNotAResult) {
			t.Errorf("%s: the answer came back as %v, want ErrNotAResult", c.why, err)
		}
	}

	if after := seqOf(t, conn); after != before {
		t.Errorf("answers that were not results took the run from seq %d to %d", before, after)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("answers that were not results published %+v", got)
	}
	var state string
	var runner *string
	if err := conn.QueryRow(t.Context(),
		`select state, runner from tasks where idempotency_key = $1`, string(task)).
		Scan(&state, &runner); err != nil {
		t.Fatal(err)
	}
	if state != "dispatched" {
		t.Errorf("the task reads %s, and nothing it was told was a result", state)
	}
	if runner != nil {
		t.Errorf("the task is stamped as held by %s from an answer that was not a result", *runner)
	}
}

// "It carries no item and no artifact content: what travels is a digest per port." The runner
// uploads what it produced and names it, and the controller reads it back before the evaluator is
// shown the result, so what the next step is handed is what the store holds under those digests.
// A digest the store does not hold is an error rather than a refusal, because an upload may not
// have landed yet: the message is left for the next delivery and nothing is written. One whose
// envelope contradicts what the result says of it is refused, since no delivery will change it.
func TestAResultNamesItsOutputsByDigest(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	taken := q.taken()
	if len(taken) != 1 || taken[0].Step != "normalize" {
		t.Fatalf("the first pass published %+v", taken)
	}
	result := succeeded(t, taken[0], core.now())
	answer := core.answerOf(t, result)
	if len(answer.Outputs) != 2 {
		t.Fatalf("the answer names %+v, and normalize declares two ports", answer.Outputs)
	}
	named := map[agk.Port]Output{}
	for _, o := range answer.Outputs {
		named[o.Port] = o
	}

	conn := dbtest.Superuser(t, super)
	before := seqOf(t, conn)
	for _, c := range []struct {
		why     string
		with    func(*Answer)
		refused bool
	}{
		{"a digest the store does not hold", func(a *Answer) { a.Outputs[0].Digest = strings.Repeat("0", 64) }, false},
		{"a digest that is not one", func(a *Answer) { a.Outputs[0].Digest = "../../secrets" }, true},
		{"a count its envelope does not carry", func(a *Answer) { a.Outputs[0].Items = 7 }, true},
		{"one port's envelope on another port", func(a *Answer) {
			a.Outputs[0].Digest, a.Outputs[1].Digest = a.Outputs[1].Digest, a.Outputs[0].Digest
			a.Outputs[0].Items, a.Outputs[1].Items = a.Outputs[1].Items, a.Outputs[0].Items
		}, true},
		{"one port twice", func(a *Answer) { a.Outputs[1] = a.Outputs[0] }, true},
	} {
		wrong := answer
		wrong.Outputs = append([]Output(nil), answer.Outputs...)
		c.with(&wrong)
		err := core.Answer(t.Context(), wrong)
		switch {
		case err == nil:
			t.Errorf("%s was recorded", c.why)
		case c.refused && !errors.Is(err, ErrNotAResult):
			t.Errorf("%s answered %v, and it is the same on every delivery", c.why, err)
		case !c.refused && errors.Is(err, ErrNotAResult):
			t.Errorf("%s was refused for good, and the upload may land before the next delivery: %s", c.why, err)
		}
	}
	if after := seqOf(t, conn); after != before {
		t.Fatalf("answers whose envelopes could not be read took the run from seq %d to %d", before, after)
	}
	if got := q.taken(); len(got) != 0 {
		t.Fatalf("answers whose envelopes could not be read published %+v", got)
	}

	if err := core.Answer(t.Context(), answer); err != nil {
		t.Fatalf("the answer naming what the store holds was refused: %s", err)
	}
	var ports db.Ports
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		var err error
		ports, err = ns.PublishedPorts(ctx, decidedRun, "normalize")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for port, o := range named {
		if got := ports[port]; got.Digest != o.Digest || got.Items != o.Items {
			t.Errorf("port %s is recorded as %+v, and the result named %s holding %d", port, got, o.Digest, o.Items)
		}
	}

	// And the next step is handed the envelope the store holds under the digest ok was named by.
	next := q.dispatched()
	if len(next) != 1 || next[0].Task.Step != "archive" {
		t.Fatalf("the result made runnable %+v", next)
	}
	if in := next[0].Inputs["orders"]; in.Digest != named["ok"].Digest || in.Items != 1 {
		t.Errorf("archive is handed %+v, and normalize published %+v on ok", in, named["ok"])
	}
	if got := next[0].Task.Inputs["orders"].Items; len(got) != 1 || got[0].ID != result.Outputs["ok"].Items[0].ID {
		t.Errorf("archive is handed the items %+v", got)
	}
}
