package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/otlp"
)

// Tracing a run once it has ended.
//
// "OpenTelemetry: one trace per run, one span per task ... The namespace is a span attribute, so
// per-tenant latency is queryable." The controller is where both are known whole: it decided every
// dispatch, it heard every ending, a loss the heartbeat declared included, and it holds the
// namespace from the run's row rather than from whatever a runner says. So it exports them, and no
// runner exports anything.
//
// When the run ends, in one piece, read back from the rows. A task's span could go when the task
// ends, but a run's dispatches are ended by several passes, by whichever controller held the term
// for each, and by statements that end rows the evaluator's state does not describe, a retry's
// earlier attempt, a requeue's lost dispatch and the tasks a run's ending stops among them. The
// rows hold every one of them once the run has ended, and one read of them is the one place the
// whole trace can be built from. What that costs is a trace that appears when its run ends, which
// in v0.2.0, where nothing can make a run wait, is minutes after its last task did.
//
// And never at the price of a run. The spans are handed to a queue and sent by somebody else; a
// read that fails is reported and the run is not held back for it. A controller that dies between
// writing a run's ending and reading it back sends no trace for that run: nothing on the page asks
// that a trace survive a failover, and holding one in the database until it had been sent would be
// an outbox for telemetry.

// Tracer is where the spans of a run that has ended go: otlp.Exporter, or nothing.
type Tracer interface {
	Export(spans ...otlp.Span)
}

// The attributes a span carries, each spelled as the wire spells the field it copies, under the
// product's own prefix.
const (
	attrNamespace = "agentiik.namespace"
	attrRun       = "agentiik.run_id"
	attrWorkflow  = "agentiik.workflow"
	attrCommit    = "agentiik.commit"
	attrTrigger   = "agentiik.trigger"
	attrState     = "agentiik.state"
	attrTask      = "agentiik.task_id"
	attrKey       = "agentiik.idempotency_key"
	attrStep      = "agentiik.step"
	attrAttempt   = "agentiik.attempt"
	attrShard     = "agentiik.shard"
	attrRequeue   = "agentiik.requeue"
	attrRunner    = "agentiik.runner"
	attrExitCode  = "agentiik.exit_code"
)

// traced exports the trace of a run that has just ended, where the core was given a Tracer. It is
// called after the decision that ended the run is committed, and never returns an error: what it
// could not read is reported, and the run has ended either way.
func (co *Core) traced(ctx context.Context, namespace string, run agk.RunID) {
	if co.tracer == nil {
		return
	}
	var r db.RunTrace
	if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		r, err = w.Trace(ctx, namespace, run)
		return err
	}); err != nil {
		if !errors.Is(err, db.ErrFenced) && ctx.Err() == nil {
			co.controller.report(run, fmt.Errorf("controller: the trace of run %s could not be read, and none is sent: %w", run, err))
		}
		return
	}
	co.tracer.Export(spansOf(r)...)
}

// spansOf is a run's trace: the run's own span, and one span per dispatch, each a child of it.
//
// The run's span runs from its start to its end, or from its creation where it never started, a
// run cancelled while it queued. Those are the controller's own clock, and a dispatch's are too, a
// container's start aside, which is why a task's span begins at its dispatch and not at the
// container's start: that is when the work became somebody's, and the latency a tenant queries
// includes the queue.
//
// A task that was never handed out has no span. It never reached a runner, no container was told
// its trace, and a span of no duration at the moment the run ended would say that it ran. Handed
// out is more than a recorded dispatch, though: a pass that published a task and died before
// recording it leaves a row a runner may have redeemed and run, whose container was told its span,
// and which the next pass may end without dispatching, a cancellation among them. So a row a
// runner holds, or that was published or started, has a span, beginning at the earliest of the
// three moments the row holds, or at the run's start where it holds none.
func spansOf(r db.RunTrace) []otlp.Span {
	trace, root := r.Run.Trace()
	start := r.StartedAt
	if start.IsZero() {
		start = r.CreatedAt
	}
	end := r.FinishedAt
	if end.IsZero() {
		end = start
	}
	name, _, _ := strings.Cut(r.Workflow, "@")
	spans := []otlp.Span{{
		Trace: trace, ID: root, Name: name, Start: start, End: end,
		Attributes: []otlp.Attribute{
			otlp.String(attrNamespace, r.Namespace),
			otlp.String(attrRun, string(r.Run)),
			otlp.String(attrWorkflow, name),
			otlp.String(attrCommit, r.Commit),
			otlp.String(attrTrigger, r.Trigger.String()),
			otlp.String(attrState, r.State.String()),
		},
		Error:   r.State == agk.Failed || r.State == agk.TimedOut,
		Message: r.State.String(),
	}}
	for _, d := range r.Dispatches {
		began := earliest(d.DispatchedAt, d.PublishedAt, d.StartedAt)
		if began.IsZero() {
			if d.Runner == "" {
				continue
			}
			began = start
		}
		finished := d.FinishedAt
		if finished.IsZero() || finished.Before(began) {
			finished = maxTime(began, end)
		}
		attrs := []otlp.Attribute{
			otlp.String(attrNamespace, r.Namespace),
			otlp.String(attrRun, string(r.Run)),
			otlp.String(attrTask, d.ID),
			otlp.String(attrKey, string(d.Key)),
			otlp.String(attrStep, string(d.Step)),
			otlp.Int(attrAttempt, d.Attempt),
		}
		if !d.Shard.IsZero() {
			attrs = append(attrs, otlp.String(attrShard, d.Shard.String()))
		}
		attrs = append(attrs, otlp.Int(attrRequeue, d.Requeue), otlp.String(attrState, d.State.String()))
		if d.Runner != "" {
			attrs = append(attrs, otlp.String(attrRunner, d.Runner))
		}
		if d.ExitCode != nil {
			attrs = append(attrs, otlp.Int(attrExitCode, *d.ExitCode))
		}
		spans = append(spans, otlp.Span{
			Trace: trace, ID: agk.TaskSpan(d.ID), Parent: root, Name: string(d.Step),
			Start: began, End: finished, Attributes: attrs,
			Error:   d.State == agk.TaskFailed || d.State == agk.TaskTimedOut || d.State == agk.TaskLost,
			Message: d.State.String(),
		})
	}
	return spans
}

// earliest is the earliest of the moments that are set, and the zero time where none is.
func earliest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if !t.IsZero() && (out.IsZero() || t.Before(out)) {
			out = t
		}
	}
	return out
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
