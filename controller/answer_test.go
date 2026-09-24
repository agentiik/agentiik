package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
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
		// An ending, and of this task, but of no dispatch of it: a requeue keeps the key.
		{graph.Result{Task: task, State: agk.TaskSucceeded}, "an ending naming no dispatch"},
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
// have landed yet: the message is left for the next delivery and nothing is written. One naming
// what is not an envelope, or an envelope that contradicts what the result says of it, is refused,
// since no delivery will change it.
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

	// Bytes that are not an envelope, uploaded under their own digest, which a runner can do
	// with anything it likes.
	stray := []byte(`{"meta":{"port":"ok","count":3},"items":[]}`)
	sum := sha256.Sum256(stray)
	strayDigest := hex.EncodeToString(sum[:])
	if err := core.objects.Put(t.Context(), artifact.Key("finance", strayDigest), bytes.NewReader(stray)); err != nil {
		t.Fatal(err)
	}

	conn := dbtest.Superuser(t, super)
	before := seqOf(t, conn)
	for _, c := range []struct {
		why     string
		with    func(*Answer)
		refused bool
	}{
		{"a digest the store does not hold", func(a *Answer) { a.Outputs[0].Digest = strings.Repeat("0", 64) }, false},
		{"bytes that are not an envelope, under their own digest", func(a *Answer) { a.Outputs[0].Digest = strayDigest }, true},
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

// A failure's envelopes are not read back. The evaluator keeps the ports of a shard that succeeded
// and of no other, so a failure naming a digest the store does not hold is recorded all the same:
// holding its ending back until the envelope turned up would be waiting on what nothing reads.
func TestAFailuresOutputsAreNotReadBack(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	taken := q.taken()
	if len(taken) != 1 {
		t.Fatalf("the first pass published %d tasks", len(taken))
	}
	answer := core.answerOf(t, failed(taken[0], 1, core.now()))
	answer.Outputs = []Output{{Port: "rejected", Digest: strings.Repeat("0", 64), Items: 3}}
	if err := core.Answer(t.Context(), answer); err != nil {
		t.Fatalf("a failure naming an envelope the store does not hold answered %s", err)
	}
	var state string
	if err := dbtest.Superuser(t, super).QueryRow(t.Context(),
		`select state from tasks where idempotency_key = $1`, string(taken[0].ID)).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Errorf("the task reads %s after its failure was answered", state)
	}
}

// "Its reach is the tasks in its hands." A task is in a runner's hands once it redeemed the task's
// grant, and a result is taken from that runner and from no other: not from another machine of the
// pool, which holds the same bus credential and could otherwise settle a task it was never given,
// and not from anybody saying a container ran before a redemption, when nobody holds it. Each is
// refused, with an error the bus takes off the queue and reports, and the run is left as it was.
func TestAResultFromARunnerThatDoesNotHoldTheTaskIsRefused(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	sent := q.dispatched()
	if len(sent) != 1 {
		t.Fatalf("the first pass dispatched %d tasks", len(sent))
	}
	d := sent[0]
	result := succeeded(t, d.Task, core.now())

	conn := dbtest.Superuser(t, super)
	before := seqOf(t, conn)
	refused := func(why string, a Answer, holder bool) {
		t.Helper()
		err := core.Answer(t.Context(), a)
		if !errors.Is(err, ErrNotAResult) {
			t.Errorf("%s answered %v, and it is the same on every delivery", why, err)
		}
		if errors.Is(err, ErrNotTheHolder) != holder {
			t.Errorf("%s answered %v", why, err)
		}
	}

	// Nobody has redeemed it yet, so the answer is written by hand rather than by answerOf,
	// which would redeem it.
	early := Answer{Result: result, Row: d.Row, Runner: theRunner}
	early.Result.Outputs = nil
	refused("a result for a task nobody redeemed", early, true)

	// runner-dmz-02 redeems it, and the task is in its hands.
	answer := core.answerOf(t, result)
	if answer.Runner != theRunner {
		t.Fatalf("the task is held by %s", answer.Runner)
	}
	other := answer
	other.Runner = "runner-lan-01"
	refused("a result from another runner of the pool", other, true)
	elsewhere := answer
	elsewhere.Row = "01M2ZZZZZZZZZZZZZZZZZZZZZZ"
	refused("a result naming a dispatch that is not this task's", elsewhere, false)

	if after := seqOf(t, conn); after != before {
		t.Errorf("refused results took the run from seq %d to %d", before, after)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("refused results published %+v", got)
	}
	var state, runner string
	if err := conn.QueryRow(t.Context(),
		`select state, runner from tasks where idempotency_key = $1`, string(d.Task.ID)).
		Scan(&state, &runner); err != nil {
		t.Fatal(err)
	}
	if state != "dispatched" || runner != theRunner {
		t.Errorf("the task reads %s, held by %s, after results from runners that do not hold it", state, runner)
	}

	// And the runner that holds it is heard.
	if err := core.Answer(t.Context(), answer); err != nil {
		t.Fatalf("the result of the runner holding the task was refused: %s", err)
	}
	if got := q.taken(); len(got) != 1 || got[0].Step != "archive" {
		t.Errorf("the holder's result published %+v, want archive", got)
	}
}

// "A task that never reached a container writes what stopped it", "a refused pull or a grant that
// would not redeem being the usual reasons", and a grant that would not redeem binds nobody, so
// such an ending is about a dispatch nobody holds. It is taken from the first runner to report it,
// which is bound to the dispatch as a redemption would have bound it: another runner's word on it
// is refused, and its grant redeems for nobody.
func TestATaskThatNeverReachedAContainerIsEndedByTheRunnerThatReportsIt(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	sent := q.dispatched()
	if len(sent) != 1 {
		t.Fatalf("the first pass dispatched %d tasks", len(sent))
	}
	d := sent[0]
	log, err := agk.NewLogURI(d.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	pulled := Answer{
		Result: graph.Result{Task: d.Task.ID, State: agk.TaskFailed},
		Row:    d.Row, Runner: theRunner,
		Log: log, LogLines: 3,
	}
	if err := core.Answer(t.Context(), pulled); err != nil {
		t.Fatalf("the failure of a task whose image was refused, reported before any redemption, answered %s", err)
	}

	conn := dbtest.Superuser(t, super)
	var state string
	var runner *string
	var exit *int
	if err := conn.QueryRow(t.Context(),
		`select state, runner, exit_code from tasks where idempotency_key = $1`, string(d.Task.ID)).
		Scan(&state, &runner, &exit); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || runner == nil || *runner != theRunner {
		t.Errorf("the task reads %s, held by %v, after %s reported it never reached a container", state, runner, theRunner)
	}
	if exit != nil {
		t.Errorf("a task that never reached a container is recorded as exiting %d", *exit)
	}

	other := pulled
	other.Runner = "runner-lan-01"
	if err := core.Answer(t.Context(), other); !errors.Is(err, ErrNotTheHolder) {
		t.Errorf("another runner reporting the same ending answered %v", err)
	}
	grant, ok := issued.Load(d.Row)
	if !ok {
		t.Fatal("no grant went out for the task")
	}
	if err := core.controller.Fenced(t.Context(), core.term, func(ctx context.Context, w *db.Wide) error {
		_, err := w.Redeem(ctx, grant.(string), d.Task.ID, "runner-lan-01", core.now())
		return err
	}); !errors.Is(err, db.ErrTaskHeld) {
		t.Errorf("the grant of a task another runner ended redeemed, answering %v", err)
	}
}

// failsFirst resolves no graph the first time it is asked, which stands for a controller that read
// an answer and could not go on with it: the store, git or the database did not answer, or it
// died. Every later call answers as the Versions it wraps.
type failsFirst struct {
	Versions

	mu    sync.Mutex
	asked bool
}

func (f *failsFirst) Graph(ctx context.Context, namespace, workflow, commit string) (*graph.Graph, error) {
	f.mu.Lock()
	first := !f.asked
	f.asked = true
	f.mu.Unlock()
	if first {
		return nil, errors.New("the graph could not be resolved this time")
	}
	return f.Versions.Graph(ctx, namespace, workflow, commit)
}

// An ending that never reached a container binds its runner when the ending is written, and not
// before. A controller that read it and could not go on leaves the dispatch as it found it, bound
// to nobody, so the heartbeat, which takes a bound dispatch for one a runner redeemed, finds no
// task lost where no container ran, and the step is not requeued for a loss the infrastructure
// never had. The redelivery then ends the dispatch as the runner reported it, where it would
// otherwise find it lost and requeued and its ending no longer news.
func TestAnUnreachedEndingTheControllerCouldNotWriteBindsNobody(t *testing.T) {
	core, q, pool, super := decidingOn(t, requeueingWorkflow)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	sent := q.dispatched()
	if len(sent) != 1 {
		t.Fatalf("the first pass dispatched %d tasks", len(sent))
	}
	d := sent[0]

	troubled, err := NewCore(core.controller, core.term, Options{
		Queue: q, Versions: &failsFirst{Versions: core.versions}, Objects: core.objects, Now: core.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	pulled := Answer{Result: graph.Result{Task: d.Task.ID, State: agk.TaskFailed}, Row: d.Row, Runner: "runner-1"}
	if err := troubled.Answer(t.Context(), pulled); err == nil || errors.Is(err, ErrNotAResult) {
		t.Fatalf("an answer the controller could not go on with answered %v, and it is one to deliver again", err)
	}
	conn := dbtest.Superuser(t, super)
	if got, want := dispatchesOf(t, conn, d.Task.ID), []string{"0 dispatched -"}; !slices.Equal(got, want) {
		t.Errorf("after an ending that was never written the key holds %q, want %q", got, want)
	}

	core.silence(t)
	if got, want := dispatchesOf(t, conn, d.Task.ID), []string{"0 dispatched -"}; !slices.Equal(got, want) {
		t.Errorf("after the sweep the key holds %q, want %q", got, want)
	}
	if again := q.dispatched(); len(again) != 0 {
		t.Errorf("a task that never reached a container was sent out again as %+v", again)
	}

	if err := core.Answer(t.Context(), pulled); err != nil {
		t.Fatalf("the ending delivered again was refused: %s", err)
	}
	if got, want := dispatchesOf(t, conn, d.Task.ID), []string{"0 failed runner-1"}; !slices.Equal(got, want) {
		t.Errorf("after the ending was delivered again the key holds %q, want %q", got, want)
	}
}

// redeemsMeanwhile has a runner redeem the dispatch the first time the graph is asked for, which is
// after Answer has read who holds it and before it writes what it decided.
type redeemsMeanwhile struct {
	Versions

	redeem func() error
	once   sync.Once
}

func (r *redeemsMeanwhile) Graph(ctx context.Context, namespace, workflow, commit string) (*graph.Graph, error) {
	var err error
	r.once.Do(func() { err = r.redeem() })
	if err != nil {
		return nil, err
	}
	return r.Versions.Graph(ctx, namespace, workflow, commit)
}

// A runner may redeem a dispatch after an ending that never reached a container was read and
// before it was written. The redemption bound first, so the ending is somebody else's word on the
// dispatch: it is refused as such, and nothing of it is written.
func TestAnUnreachedEndingIsRefusedOnceARedemptionBoundItMeanwhile(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	sent := q.dispatched()
	if len(sent) != 1 {
		t.Fatalf("the first pass dispatched %d tasks", len(sent))
	}
	d := sent[0]

	racing, err := NewCore(core.controller, core.term, Options{
		Queue: q, Objects: core.objects, Now: core.now,
		Versions: &redeemsMeanwhile{Versions: core.versions, redeem: func() error { return core.redeem(t, d, "runner-2") }},
	})
	if err != nil {
		t.Fatal(err)
	}
	conn := dbtest.Superuser(t, super)
	before := seqOf(t, conn)
	pulled := Answer{Result: graph.Result{Task: d.Task.ID, State: agk.TaskFailed}, Row: d.Row, Runner: "runner-1"}
	if err := racing.Answer(t.Context(), pulled); !errors.Is(err, ErrNotAResult) || !errors.Is(err, ErrNotTheHolder) {
		t.Errorf("an unreached ending of a dispatch runner-2 redeemed meanwhile answered %v", err)
	}
	if after := seqOf(t, conn); after != before {
		t.Errorf("the refused ending took the run from seq %d to %d", before, after)
	}
	if got, want := dispatchesOf(t, conn, d.Task.ID), []string{"0 dispatched runner-2"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
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

// timingOutWorkflow retries its one step once where it is stopped at its deadline.
var timingOutWorkflow = strings.Replace(retryingWorkflow, "on: [failed]", "on: [timeout]", 1)

// "A timed_out or cancelled task carries an exit code wherever a container ran", and the tasks
// table keeps it for a person to read through the API: on the row of an attempt a retry moved past,
// and on the row of one that ended the step.
func TestAStoppedTasksExitCodeIsRecordedAndRead(t *testing.T) {
	stopped := func(task graph.Task, code int, at time.Time) graph.Result {
		return graph.Result{Task: task.ID, State: agk.TaskTimedOut, ExitCode: code, StartedAt: at, FinishedAt: at}
	}
	codes := func(t *testing.T, pool *db.Pool) map[string]string {
		t.Helper()
		var detail db.RunDetail
		if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
			var err error
			detail, err = ns.RunDetail(ctx, decidedRun)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, task := range detail.Tasks {
			code := "none"
			if task.ExitCode != nil {
				code = strconv.Itoa(*task.ExitCode)
			}
			out[string(task.Task)] = task.State.String() + " " + code
		}
		return out
	}

	// Moved past by a retry: the row of the attempt that timed out is written as it ended.
	core, q, pool, _ := decidingOn(t, timingOutWorkflow)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.dispatched()
	if len(first) != 1 {
		t.Fatalf("the first pass dispatched %d tasks", len(first))
	}
	core.answer(t, stopped(first[0].Task, 137, core.now()))
	if got := codes(t, pool)[string(first[0].Task.ID)]; got != "timed_out 137" {
		t.Errorf("an attempt killed after the grace at its deadline reads %q", got)
	}
	// And an attempt reported with no code, moved past by the retry after it, reads none.
	clock.advance(2 * time.Minute)
	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	second := q.dispatched()
	if len(second) != 1 || second[0].Task.Attempt != 2 {
		t.Fatalf("the retry dispatched %+v", second)
	}
	quiet := stopped(second[0].Task, 0, core.now())
	quiet.NoExitCode = true
	core.answer(t, quiet)
	if got := codes(t, pool)[string(second[0].Task.ID)]; got != "timed_out none" {
		t.Errorf("an attempt stopped at its deadline and reported with no code reads %q", got)
	}

	// Ending the step: nothing retries it, and the row keeps the code all the same.
	core, q, pool, _ = deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first = q.dispatched()
	if len(first) != 1 {
		t.Fatalf("the first pass dispatched %d tasks", len(first))
	}
	core.answer(t, stopped(first[0].Task, 143, core.now()))
	if got := codes(t, pool)[string(first[0].Task.ID)]; got != "timed_out 143" {
		t.Errorf("a task that obeyed the stop at its deadline reads %q", got)
	}

	// Reported with no code, as a host answering from a record kept before stops kept theirs
	// reports it: the row says none rather than the 0 of success.
	core, q, pool, _ = deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first = q.dispatched()
	if len(first) != 1 {
		t.Fatalf("the first pass dispatched %d tasks", len(first))
	}
	silent := stopped(first[0].Task, 0, core.now())
	silent.NoExitCode = true
	core.answer(t, silent)
	if got := codes(t, pool)[string(first[0].Task.ID)]; got != "timed_out none" {
		t.Errorf("a stopped task reported with no code reads %q", got)
	}
}

// A run's ending stops what it holds before any container has exited, so the rows it ends carry no
// code, and the runner's report comes to a run with nothing left to decide. The code it reports
// for its own dispatch still lands on that row, once, and a runner that reports none, as a host
// answering from a record kept before stops kept their code does, leaves none rather than the 0 of
// success.
func TestTheCodeOfAContainerARunsEndingStoppedIsRecorded(t *testing.T) {
	core, q, pool, _ := decidingOn(t, bothAtOnceWorkflow)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	sent := q.dispatched()
	if len(sent) != 2 {
		t.Fatalf("the first pass published %d tasks", len(sent))
	}
	for _, d := range sent {
		if err := core.redeem(t, d, theRunner); err != nil {
			t.Fatal(err)
		}
	}
	askedToCancel(t, pool, core, decidedRun)
	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, core); got != agk.Cancelled {
		t.Fatalf("a run somebody asked to cancel is %s after a sweep", got)
	}

	at := core.now()
	obeyed, silent := sent[0], sent[1]
	stop := func(d Dispatch, code int, none bool) Answer {
		return Answer{
			Result: graph.Result{Task: d.Task.ID, State: agk.TaskCancelled, ExitCode: code, NoExitCode: none, StartedAt: at, FinishedAt: at},
			Row:    d.Row, Runner: theRunner,
		}
	}
	for _, a := range []Answer{stop(obeyed, 143, false), stop(obeyed, 137, false), stop(silent, 0, true)} {
		if err := core.Answer(t.Context(), a); err != nil {
			t.Fatalf("the report of a container the cancellation stopped answered %s", err)
		}
	}
	other := stop(silent, 143, false)
	other.Runner = "runner-lan-01"
	if err := core.Answer(t.Context(), other); !errors.Is(err, ErrNotTheHolder) {
		t.Errorf("another runner's code for a dispatch it does not hold answered %v", err)
	}

	var detail db.RunDetail
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		var err error
		detail, err = ns.RunDetail(ctx, decidedRun)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got := map[agk.TaskID]string{}
	for _, task := range detail.Tasks {
		code := "none"
		if task.ExitCode != nil {
			code = strconv.Itoa(*task.ExitCode)
		}
		got[task.Task] = task.State.String() + " " + code
	}
	if got[obeyed.Task.ID] != "cancelled 143" || got[silent.Task.ID] != "cancelled none" {
		t.Errorf("the stopped tasks read %v: the one that obeyed exited 143, reported once and then again as 137, and the other reported no code", got)
	}
}

// A container that exited on its own as its run was cancelled is reported failed, to a run that
// has ended: the row keeps the ending the cancellation wrote and takes the code it exited with. A
// dispatch that was lost before the run ended keeps its loss and takes no code, whatever its runner
// says of it afterwards.
func TestARunsEndingKeepsItsStateAndTakesTheCodeAContainerExitedWith(t *testing.T) {
	core, q, pool, super := decidingOn(t, bothAtOnceWorkflow)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	sent := q.dispatched()
	if len(sent) != 2 {
		t.Fatalf("the first pass published %d tasks", len(sent))
	}
	for _, d := range sent {
		if err := core.redeem(t, d, theRunner); err != nil {
			t.Fatal(err)
		}
	}
	exited, lost := sent[0], sent[1]
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `update tasks set state = 'lost' where id = $1`, lost.Row); err != nil {
		t.Fatal(err)
	}
	askedToCancel(t, pool, core, decidedRun)
	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, core); got != agk.Cancelled {
		t.Fatalf("a run somebody asked to cancel is %s after a sweep", got)
	}

	at := core.now()
	for _, a := range []Answer{
		{Result: graph.Result{Task: exited.Task.ID, State: agk.TaskFailed, ExitCode: 1, StartedAt: at, FinishedAt: at}, Row: exited.Row, Runner: theRunner},
		{Result: graph.Result{Task: lost.Task.ID, State: agk.TaskCancelled, ExitCode: 143, StartedAt: at, FinishedAt: at}, Row: lost.Row, Runner: theRunner},
	} {
		if err := core.Answer(t.Context(), a); err != nil {
			t.Errorf("the report of %s after its run ended answered %s", a.Result.Task, err)
		}
	}
	for _, c := range []struct {
		row, want string
	}{{exited.Row, "cancelled 1"}, {lost.Row, "lost none"}} {
		var state string
		var code *int
		if err := conn.QueryRow(t.Context(), `select state, exit_code from tasks where id = $1`, c.row).Scan(&state, &code); err != nil {
			t.Fatal(err)
		}
		got := state + " none"
		if code != nil {
			got = state + " " + strconv.Itoa(*code)
		}
		if got != c.want {
			t.Errorf("dispatch %s reads %q, want %q", c.row, got, c.want)
		}
	}
}
