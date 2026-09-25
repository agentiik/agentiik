package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/otlp"
)

// The trace of a run, as the controller exports it once the run has ended: one trace per run, one
// span per task, the namespace on every span, and every identifier the one a container of the run
// was told in TRACEPARENT, which the runner derives from the same run and the same task_id.

// recorded is a Tracer that keeps what it was handed.
type recorded struct {
	mu    sync.Mutex
	spans []otlp.Span
	calls int
}

func (r *recorded) Export(spans ...otlp.Span) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, spans...)
	r.calls++
}

func (r *recorded) exported() ([]otlp.Span, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]otlp.Span(nil), r.spans...), r.calls
}

// attribute is the value of one attribute of a span, and whether it has it.
func attribute(s otlp.Span, key string) (any, bool) {
	for _, a := range s.Attributes {
		if a.Key == key {
			return a.Value, true
		}
	}
	return nil, false
}

// traceOfDecidedRun checks what every span of a trace has in common, and answers the run's span and
// the task spans by the task_id their span is named for.
func traceOfDecidedRun(t *testing.T, spans []otlp.Span) (otlp.Span, map[agk.SpanID]otlp.Span) {
	t.Helper()
	trace, root := decidedRun.Trace()
	var run otlp.Span
	tasks := map[agk.SpanID]otlp.Span{}
	for _, s := range spans {
		if s.Trace != trace {
			t.Errorf("span %s is in trace %s, and every span of run %s is in %s", s.Name, s.Trace, decidedRun, trace)
		}
		if ns, _ := attribute(s, "agentiik.namespace"); ns != "finance" {
			t.Errorf("span %s carries the namespace %v, want finance", s.Name, ns)
		}
		switch {
		case s.ID == root:
			if s.Parent.Valid() {
				t.Errorf("the run's span has the parent %s", s.Parent)
			}
			run = s
		case s.Parent != root:
			t.Errorf("span %s has the parent %s, and a task's span is a child of the run's, %s", s.Name, s.Parent, root)
		default:
			tasks[s.ID] = s
		}
		if s.End.Before(s.Start) {
			t.Errorf("span %s ends at %s, before it starts at %s", s.Name, s.End, s.Start)
		}
	}
	if run.Name == "" {
		t.Fatalf("no span of the %d exported is the run's", len(spans))
	}
	return run, tasks
}

func TestARunThatEndsExportsOneTraceWithASpanPerTask(t *testing.T) {
	core, q, pool, _ := deciding(t)
	traces := &recorded{}
	core.tracer = traces
	createRun(t, pool)

	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	rows := map[agk.SpanID]agk.TaskID{}
	for range 6 {
		taken := q.taken()
		if len(taken) == 0 {
			break
		}
		if spans, _ := traces.exported(); len(spans) != 0 {
			t.Fatalf("a run still going exported %d spans", len(spans))
		}
		for _, task := range taken {
			rows[agk.TaskSpan(core.row(t, task.ID))] = task.ID
			core.answer(t, succeeded(t, task, core.now()))
		}
	}

	spans, calls := traces.exported()
	if calls != 1 {
		t.Errorf("the trace was exported %d times, want once, as the run ended", calls)
	}
	run, tasks := traceOfDecidedRun(t, spans)
	if run.Name != "monthly-invoicing" || run.Error {
		t.Errorf("the run's span is %q, error %v, want monthly-invoicing and no error", run.Name, run.Error)
	}
	if state, _ := attribute(run, "agentiik.state"); state != "succeeded" {
		t.Errorf("the run's span says %v, want succeeded", state)
	}
	if len(tasks) != 2 || len(rows) != 2 {
		t.Fatalf("the trace holds %d task spans for %d dispatches", len(tasks), len(rows))
	}
	for id, key := range rows {
		s, ok := tasks[id]
		if !ok {
			t.Errorf("no span is named for the task_id of %s, which is the span its container was told", key)
			continue
		}
		if got, _ := attribute(s, "agentiik.idempotency_key"); got != string(key) {
			t.Errorf("span %s says it is %v, want %s", id, got, key)
		}
		if code, _ := attribute(s, "agentiik.exit_code"); code != int64(0) {
			t.Errorf("span %s carries the exit code %v, want 0", id, code)
		}
	}
}

// A lost dispatch and its requeue are two spans, each named for its own task_id, because a
// container of each was told its own: the lost one fails, and the requeue is what succeeded.
func TestALostDispatchAndItsRequeueAreTwoSpans(t *testing.T) {
	core, q, pool, _ := decidingOn(t, requeueingWorkflow)
	traces := &recorded{}
	core.tracer = traces
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	lost := q.dispatched()[0]
	if err := core.redeem(t, lost, theRunner); err != nil {
		t.Fatal(err)
	}
	core.silence(t)
	requeued := q.dispatched()[0]
	core.answer(t, succeeded(t, requeued.Task, core.now()))

	spans, _ := traces.exported()
	_, tasks := traceOfDecidedRun(t, spans)
	if len(tasks) != 2 {
		t.Fatalf("the trace holds %d task spans, want the lost dispatch's and its requeue's", len(tasks))
	}
	for row, want := range map[string]string{lost.Row: "lost", requeued.Row: "succeeded"} {
		s, ok := tasks[agk.TaskSpan(row)]
		if !ok {
			t.Errorf("no span is named for dispatch %s", row)
			continue
		}
		if state, _ := attribute(s, "agentiik.state"); state != want || s.Error != (want == "lost") {
			t.Errorf("dispatch %s's span says %v, error %v, want %s", row, state, s.Error, want)
		}
	}
}

// A run called off exports its trace too, since Cancel ends a run by a path of its own, and the
// task it stopped is in it.
func TestACancelledRunExportsItsTrace(t *testing.T) {
	core, q, pool, _ := deciding(t)
	traces := &recorded{}
	core.tracer = traces
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	held := q.dispatched()[0]
	if err := core.Cancel(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}

	spans, _ := traces.exported()
	run, tasks := traceOfDecidedRun(t, spans)
	if state, _ := attribute(run, "agentiik.state"); state != "cancelled" || run.Error {
		t.Errorf("the run's span says %v, error %v, want cancelled and no error: somebody meant it", state, run.Error)
	}
	s, ok := tasks[agk.TaskSpan(held.Row)]
	if !ok || len(tasks) != 1 {
		t.Fatalf("the trace holds %d task spans, want the one dispatch the cancellation stopped", len(tasks))
	}
	if state, _ := attribute(s, "agentiik.state"); state != "cancelled" {
		t.Errorf("the stopped task's span says %v, want cancelled", state)
	}
}

// The exporter is a Tracer, which is what the program hands the core.
var _ Tracer = (*otlp.Exporter)(nil)

// A pass that published a task and died before recording the dispatch leaves a row a runner may
// redeem and run, and its container was told the row's span. A cancellation that then ends the
// run without dispatching it again still exports that span, or the brick's spans would hang from a
// parent nobody sent.
func TestADispatchTakenBeforeItWasRecordedStillHasItsSpan(t *testing.T) {
	core, _, pool, _ := deciding(t)
	traces := &recorded{}
	core.tracer = traces
	createRun(t, pool)
	ctx, cancel := context.WithCancel(t.Context())
	died := &diesOnPublishing{cancel: cancel}
	dead, err := NewCore(core.controller, core.term, Options{
		Queue: died, Versions: core.versions, Objects: core.objects, Now: core.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dead.Decide(ctx, decidedRun); err == nil {
		t.Fatal("a pass that died after publishing answered as if it had recorded the dispatch")
	}
	sent := died.dispatched()
	if len(sent) != 1 {
		t.Fatalf("the pass that died published %d tasks", len(sent))
	}
	if err := core.redeem(t, sent[0], theRunner); err != nil {
		t.Fatal(err)
	}
	askedToCancel(t, pool, core, decidedRun)
	if err := core.Wake(t.Context(), Wake{Run: decidedRun}); err != nil {
		t.Fatal(err)
	}

	spans, _ := traces.exported()
	_, tasks := traceOfDecidedRun(t, spans)
	if _, ok := tasks[agk.TaskSpan(sent[0].Row)]; !ok || len(tasks) != 1 {
		t.Fatalf("the trace holds %d task spans and none for dispatch %s, which %s redeemed and whose container was told its span", len(tasks), sent[0].Row, theRunner)
	}
}

// The spans of a run as the rows describe it, one rule per case: what fails is an error and what
// was called off is not, a span begins at the earliest moment its row holds and never ends before
// it begins, and an exit code nobody reported is not written as one.
func TestTheSpansOfARunAreWhatItsRowsSay(t *testing.T) {
	at := func(minute int) time.Time { return time.Date(2026, 9, 14, 6, minute, 0, 0, time.UTC) }
	code := 1
	r := db.RunTrace{
		Namespace: "finance", Run: decidedRun, Workflow: "monthly-invoicing", Commit: "a3f9c1e",
		State: agk.TimedOut, Trigger: agk.TriggerManual,
		CreatedAt: at(0), StartedAt: at(1), FinishedAt: at(30),
		Dispatches: []db.TracedDispatch{
			// Published before its dispatch was recorded, and failed.
			{ID: "01M2Z8V1P9C4XQ7K2N4D6F8H1A", Key: "k/normalize/1", Step: "normalize", Attempt: 1, State: agk.TaskFailed,
				Runner: "runner-1", ExitCode: &code, PublishedAt: at(2), DispatchedAt: at(3), StartedAt: at(4), FinishedAt: at(5)},
			// Stopped at the deadline, with a finish written before its start by another clock.
			{ID: "01M2Z8V1P9C4XQ7K2N4D6F8H1B", Key: "k/archive/1", Step: "archive", Attempt: 1, State: agk.TaskTimedOut,
				Runner: "runner-1", DispatchedAt: at(10), FinishedAt: at(9)},
			// Cancelled before anybody took it: no span.
			{ID: "01M2Z8V1P9C4XQ7K2N4D6F8H1C", Key: "k/notify/1", Step: "notify", Attempt: 1, State: agk.TaskCancelled},
		},
	}
	spans := spansOf(r)
	run, tasks := traceOfDecidedRun(t, spans)
	if !run.Error || !run.Start.Equal(at(1)) || !run.End.Equal(at(30)) {
		t.Errorf("the run's span is error %v from %s to %s, want an error from its start to its end", run.Error, run.Start, run.End)
	}
	if len(tasks) != 2 {
		t.Fatalf("the trace holds %d task spans, want the two that were handed out", len(tasks))
	}
	failed := tasks[agk.TaskSpan("01M2Z8V1P9C4XQ7K2N4D6F8H1A")]
	if !failed.Error || !failed.Start.Equal(at(2)) || !failed.End.Equal(at(5)) {
		t.Errorf("the failed task's span is error %v from %s to %s, want an error from its publication to its end", failed.Error, failed.Start, failed.End)
	}
	if got, _ := attribute(failed, "agentiik.exit_code"); got != int64(1) {
		t.Errorf("the failed task's span carries the exit code %v, want 1", got)
	}
	stopped := tasks[agk.TaskSpan("01M2Z8V1P9C4XQ7K2N4D6F8H1B")]
	if !stopped.Error || !stopped.Start.Equal(at(10)) || !stopped.End.Equal(at(30)) {
		t.Errorf("the timed-out task's span is error %v from %s to %s, want an error ending with the run", stopped.Error, stopped.Start, stopped.End)
	}
	if got, ok := attribute(stopped, "agentiik.exit_code"); ok {
		t.Errorf("a task nobody reported an exit code for carries %v", got)
	}

	// A run cancelled while it queued starts at its creation, on the database's clock, and ends
	// on the controller's, which may read earlier; it is called off, not failed.
	queued := db.RunTrace{Namespace: "finance", Run: decidedRun, Workflow: "monthly-invoicing",
		State: agk.Cancelled, Trigger: agk.TriggerManual, CreatedAt: at(7), FinishedAt: at(6)}
	run, _ = traceOfDecidedRun(t, spansOf(queued))
	if run.Error || !run.Start.Equal(at(7)) || run.End.Before(run.Start) {
		t.Errorf("the span of a run cancelled while it queued is error %v from %s to %s", run.Error, run.Start, run.End)
	}
}
