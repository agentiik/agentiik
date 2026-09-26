package agk

import (
	"crypto/sha256"
	"encoding/hex"
)

// The trace a run is, and the span a task is.
//
// "OpenTelemetry: one trace per run, one span per task, context propagated into the container
// environment for bricks that use it." Every identifier here is derived and never minted, from
// what every program already holds: the run's identifier names its trace and its own span, and a
// task's identifier names the task's span. So the controller that exports a run's spans, the
// runner that tells a container which span it runs under and agk run --local, which tells a
// container the same thing with no control plane at all, name the same trace without a word
// passing between them, and nothing is added to the task message to carry it.
//
// SHA-256, and not the identifiers' own bits, because a run identifier is not always a ULID: the
// documentation prints shorter ones, and RunID.Validate imposes no length. A hash reads every
// identifier the same way, and its bits are as random as W3C Trace Context asks a trace-id to be.
// A trace-id is the first sixteen bytes of the hash of the run's identifier and the run's span-id
// the next eight, so one hash names both. A task's span-id is the first eight bytes of the hash of
// its identifier: the task_id of its dispatch on a server, which a requeue after loss renews, so
// that each dispatch is a span of its own, and the idempotency key on a laptop, where nothing is
// dispatched and nothing is requeued.

// TraceID is a W3C trace-id: sixteen bytes, written as thirty-two lowercase hexadecimal characters.
type TraceID [16]byte

// SpanID is a W3C parent-id, which OpenTelemetry calls a span identifier: eight bytes, written as
// sixteen lowercase hexadecimal characters.
type SpanID [8]byte

// String writes the identifier as Trace Context and OTLP both write it.
func (t TraceID) String() string { return hex.EncodeToString(t[:]) }

// String writes the identifier as Trace Context and OTLP both write it.
func (s SpanID) String() string { return hex.EncodeToString(s[:]) }

// Valid says whether the identifier is one Trace Context allows: all zeroes is the one it refuses.
func (t TraceID) Valid() bool { return t != TraceID{} }

// Valid says whether the identifier is one Trace Context allows: all zeroes is the one it refuses.
func (s SpanID) Valid() bool { return s != SpanID{} }

// Trace is the trace of a run, and the span the run itself is, which every task's span is a child
// of.
func (r RunID) Trace() (TraceID, SpanID) {
	sum := sha256.Sum256([]byte(r))
	var trace TraceID
	var span SpanID
	copy(trace[:], sum[:16])
	copy(span[:], sum[16:24])
	return trace, span
}

// TaskSpan is the span of one task, named by the identifier the program running it holds: the
// task_id of the dispatch on a server, the idempotency key on a laptop.
func TaskSpan(task string) SpanID {
	sum := sha256.Sum256([]byte(task))
	var span SpanID
	copy(span[:], sum[:8])
	return span
}

// TraceParent is the traceparent a container is given, which names the task's span as its parent:
// version 00, the run's trace, the task's span and the sampled flag. Empty where either identifier
// is empty, or hashes to the one value Trace Context refuses, since a traceparent a reader must
// discard is worse than none.
//
// Always sampled. The runner does not know whether the control plane exports spans, and a brick
// whose sampler follows its parent would drop its own spans under an unsampled one, which is to
// say it would record nothing because the installation records nothing, where its spans are its
// own business.
func TraceParent(run RunID, task string) string {
	if run == "" || task == "" {
		return ""
	}
	trace, _ := run.Trace()
	span := TaskSpan(task)
	if !trace.Valid() || !span.Valid() {
		return ""
	}
	return "00-" + trace.String() + "-" + span.String() + "-01"
}
