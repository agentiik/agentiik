package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// What the controller counts, against a scripted history on the real PostgreSQL: each thing told
// once, when it is written down, whatever the bus delivers twice.

// heard is an Observer that writes down what it is told, one line per thing.
type heard struct {
	mu    sync.Mutex
	lines []string
}

func (h *heard) say(format string, args ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, fmt.Sprintf(format, args...))
}

func (h *heard) Dispatched(pool string) { h.say("dispatched %s", pool) }
func (h *heard) Ended(brick, version string, state agk.TaskState, ran time.Duration) {
	h.say("ended %s %s %s %s", brick, version, state, ran)
}
func (h *heard) Retried(brick, version string) { h.say("retried %s %s", brick, version) }
func (h *heard) Lost(pool string)              { h.say("lost %s", pool) }
func (h *heard) RunEnded(namespace, workflow string, state agk.RunState, took time.Duration) {
	h.say("run %s/%s %s %s", namespace, workflow, state, took)
}

// since is what was told since the last call.
func (h *heard) since() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.lines
	h.lines = nil
	return out
}

// countingWorkflow retries normalize once, after a loss or a failure, with a backoff of a second,
// and archive waits on it.
const countingWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: archive, port: ok } }
steps:
  normalize:
    image: ` + theImage + `
    idempotent: true
    retry: { max: 1, on: [lost, failed], backoff: { type: exponential, base: 1s, max: 1s } }
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
  archive:
    image: ` + theImage + `
    needs:
      - { step: normalize, port: ok, as: orders }
    outputs: [ok]
`

// counting is a core on countingWorkflow that tells h, with a run created at the clock's instant
// and theRunner a runner of the pool default.
func counting(t *testing.T) (*Core, *fakeQueue, *heard) {
	t.Helper()
	core, q, pool, super := decidingOn(t, countingWorkflow)
	h := &heard{}
	core.observer = h
	createRun(t, pool)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `update runs set created_at = $1`, clock.now()); err != nil {
		t.Fatal(err)
	}
	joinedAsTheRunner(t, super)
	return core, q, h
}

// ran is an ending a runner reports of a container that ran for d and ended now.
func ran(task graph.Task, state agk.TaskState, code int, d time.Duration, now time.Time) graph.Result {
	return graph.Result{Task: task.ID, State: state, ExitCode: code, StartedAt: now.Add(-d), FinishedAt: now}
}

// The history: normalize is dispatched, lost on the runner that took it and requeued, fails after
// 30 seconds and is retried, and succeeds after 90; archive then runs for 5 and the run ends 17
// minutes and 31 seconds after it was created, the silence included. Each is told once, in that
// order, and a result delivered again, a sweep that finds nothing and a loss already heard tell
// nothing more.
func TestWhatTheControllerDecidesIsToldOnceItIsWritten(t *testing.T) {
	core, q, h := counting(t)
	ctx := t.Context()

	if err := core.Decide(ctx, decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.dispatched()
	if len(first) != 1 {
		t.Fatalf("the first pass dispatched %d tasks", len(first))
	}
	if err := core.redeem(t, first[0], theRunner); err != nil {
		t.Fatal(err)
	}
	if got, want := h.since(), []string{"dispatched default"}; !slices.Equal(got, want) {
		t.Fatalf("the first pass told %q, want %q", got, want)
	}

	// theRunner goes quiet: the loss is heard and the key requeued.
	core.silence(t)
	requeued := q.dispatched()
	if len(requeued) != 1 {
		t.Fatalf("the loss was followed by %d dispatches", len(requeued))
	}
	if got, want := h.since(), []string{"lost default", "dispatched default"}; !slices.Equal(got, want) {
		t.Fatalf("the sweep that heard the loss told %q, want %q", got, want)
	}

	// The requeue fails after 30 seconds and is granted its retry, which goes out once the
	// backoff has passed. The failure delivered again tells nothing.
	clock.advance(time.Minute)
	failure := core.answerOf(t, ran(requeued[0].Task, agk.TaskFailed, 1, 30*time.Second, core.now()))
	if err := core.Answer(ctx, failure); err != nil {
		t.Fatal(err)
	}
	if err := core.Answer(ctx, failure); err != nil {
		t.Fatal(err)
	}
	if got, want := h.since(), []string{"ended invoice 1.0.0 failed 30s", "retried invoice 1.0.0"}; !slices.Equal(got, want) {
		t.Fatalf("the failure, delivered twice, told %q, want %q", got, want)
	}
	clock.advance(time.Minute)
	if err := core.Wake(ctx, Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	second := q.taken()
	if len(second) != 1 || second[0].Attempt != 2 {
		t.Fatalf("after the backoff the sweep published %+v", second)
	}
	if got, want := h.since(), []string{"dispatched default"}; !slices.Equal(got, want) {
		t.Fatalf("the retry went out telling %q, want %q", got, want)
	}

	// Attempt 2 succeeds after 90 seconds, and archive goes out.
	clock.advance(5 * time.Minute)
	ok := succeeded(t, second[0], core.now())
	ok.StartedAt = core.now().Add(-90 * time.Second)
	core.answer(t, ok)
	archive := q.taken()
	if len(archive) != 1 || archive[0].Step != "archive" {
		t.Fatalf("after normalize the controller published %+v", archive)
	}
	if got, want := h.since(), []string{"ended invoice 1.0.0 succeeded 1m30s", "dispatched default"}; !slices.Equal(got, want) {
		t.Fatalf("the success told %q, want %q", got, want)
	}

	// archive runs for 5 seconds, and the run ends 17m31s after it was created: 31 seconds of
	// silence and 17 minutes of waits.
	clock.advance(10 * time.Minute)
	done := succeeded(t, archive[0], core.now())
	done.StartedAt = core.now().Add(-5 * time.Second)
	last := core.answerOf(t, done)
	if err := core.Answer(ctx, last); err != nil {
		t.Fatal(err)
	}
	if err := core.Answer(ctx, last); err != nil {
		t.Fatal(err)
	}
	if err := core.Wake(ctx, Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	if got, want := h.since(), []string{"ended invoice 1.0.0 succeeded 5s", "run finance/monthly-invoicing succeeded 17m31s"}; !slices.Equal(got, want) {
		t.Errorf("the last result, delivered twice, and a sweep told %q, want %q", got, want)
	}
}

// A cancelled run is a run that ended, and its latency is told like any other's.
func TestACancelledRunIsToldAsItsVerdict(t *testing.T) {
	core, q, h := counting(t)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	q.dispatched()
	h.since()

	clock.advance(3 * time.Minute)
	askedToCancel(t, core.controller.pool, core, decidedRun)
	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	if got, want := h.since(), []string{"run finance/monthly-invoicing cancelled 3m0s"}; !slices.Equal(got, want) {
		t.Errorf("the cancellation told %q, want %q", got, want)
	}
}

// usurped stands for another controller taking the term while this one decides: the pass has read
// the run under its own term, and its write is the one the fence refuses.
type usurped struct {
	Versions
	pool  *db.Pool
	armed bool
}

func (u *usurped) Graph(ctx context.Context, namespace, workflow, commit string) (*graph.Graph, error) {
	if u.armed {
		u.armed = false
		if _, err := u.pool.BeginTerm(ctx, "usurper"); err != nil {
			return nil, err
		}
	}
	return u.Versions.Graph(ctx, namespace, workflow, commit)
}

// A pass the fence refuses has written nothing, and tells nothing, though it holds news: a failure
// that ran for 30 seconds and the retry it was granted, taken from a result read under a term that
// passed before the decision could be written.
func TestAPassTheFenceRefusesTellsNothing(t *testing.T) {
	core, q, h := counting(t)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.dispatched()
	if len(first) != 1 {
		t.Fatalf("the first pass dispatched %d tasks", len(first))
	}
	h.since()

	failure := core.answerOf(t, ran(first[0].Task, agk.TaskFailed, 1, 30*time.Second, core.now()))
	u := &usurped{Versions: core.versions, pool: core.controller.pool, armed: true}
	core.versions = u
	if err := core.Answer(t.Context(), failure); !errors.Is(err, db.ErrFenced) {
		t.Fatalf("an answer whose term passed while it decided answered %v", err)
	}
	if u.armed {
		t.Fatal("the answer never resolved the graph, so the term never passed under it")
	}
	if got := h.since(); len(got) != 0 {
		t.Errorf("a pass the fence refused told %q", got)
	}
}

// A shard fail_fast stopped ended as its stop went out, and the report of how its container exited
// is not an ending of its own: the container the stop cut short ran for no length worth a brick's
// duration, and the failure that stopped it is the ending counted.
func TestTheReportOfAShardAStopEndedIsNotAnEnding(t *testing.T) {
	core, q, pool, super := decidingOn(t, failingFastWorkflow)
	h := &heard{}
	core.observer = h
	joinedAsTheRunner(t, super)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.CreateRun(ctx, db.NewRun{
			ID: decidedRun, Workflow: "monthly-invoicing", Commit: "a3f9c1e",
			Trigger: agk.TriggerManual, TriggeredBy: "alice",
			Inputs: json.RawMessage(`{"orders": [{"customer_id": "C-1042"}, {"customer_id": "C-1043"}]}`),
			Steps:  []agk.Step{"invoice", "archive"},
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	var first, second Dispatch
	for _, d := range q.dispatched() {
		switch {
		case d.Task.Step == "archive":
		case d.Task.Shard.Index == 1:
			first = d
		default:
			second = d
		}
		if err := core.redeem(t, d, theRunner); err != nil {
			t.Fatal(err)
		}
	}
	h.since()

	core.answer(t, ran(first.Task, agk.TaskFailed, 7, 20*time.Second, core.now()))
	if got, want := h.since(), []string{"ended invoice 1.0.0 failed 20s"}; !slices.Equal(got, want) {
		t.Fatalf("the failure that stopped its sibling told %q, want %q", got, want)
	}
	late := ran(second.Task, agk.TaskCancelled, 143, 25*time.Second, core.now())
	if err := core.Answer(t.Context(), Answer{Result: late, Row: second.Row, Runner: theRunner}); err != nil {
		t.Fatal(err)
	}
	if got := h.since(); len(got) != 0 {
		t.Errorf("the report of the stopped shard told %q", got)
	}
}
