package controller

import (
	"context"
	"errors"
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
	result := succeeded(t, taken[0], core.now())
	if err := dies.Answer(t.Context(), Answer{Result: result, Runner: "runner-dmz-02"}); err == nil {
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
	if err := core.Answer(t.Context(), Answer{Result: result, Runner: "runner-dmz-02"}); err != nil {
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

// An attempt a retry moved past is written as it ended. The state holds the attempt a shard is
// on and nothing of the one before, so a row nothing wrote again would read as dispatched for
// ever: counted against the namespace's ceiling, and a grant still honoured for a key that has
// completed.
func TestAnAttemptARetryMovedPastIsWrittenAsItEnded(t *testing.T) {
	core, q, pool, super := decidingOn(t, retryingWorkflow)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.dispatched()
	if len(first) != 1 {
		t.Fatalf("the first pass dispatched %d tasks", len(first))
	}
	if err := core.redeem(t, first[0], "runner-dmz-02"); err != nil {
		t.Fatal(err)
	}
	core.answer(t, failed(first[0].Task, 1, core.now()))

	conn := dbtest.Superuser(t, super)
	var state, runner string
	var code *int
	if err := conn.QueryRow(t.Context(),
		`select state, exit_code, runner from tasks where idempotency_key = $1`, string(first[0].Task.ID)).
		Scan(&state, &code, &runner); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || code == nil || *code != 1 || runner != "runner-dmz-02" {
		t.Errorf("attempt 1 reads %s, exit %v, runner %s, and it failed with 1 on runner-dmz-02", state, code, runner)
	}
	if err := core.redeem(t, first[0], "runner-dmz-02"); !errors.Is(err, db.ErrTaskHeld) {
		t.Errorf("the grant of an attempt that failed was redeemed again, answering %v", err)
	}
}
