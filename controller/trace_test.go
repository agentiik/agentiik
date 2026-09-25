package controller

import (
	"sync"
	"testing"

	"github.com/agentiik/agentiik/agk"
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
