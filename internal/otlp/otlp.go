// Package otlp sends spans to an OpenTelemetry collector, over OTLP/HTTP with the JSON encoding,
// written with the standard library.
//
// # Why the standard library and no dependency
//
// The documentation names OpenTelemetry, which is the format and the protocol, and not its SDK.
// What is sent is a few spans per run, built after the fact from rows the database already holds:
// no context threaded through calls, no sampler, no instrumentation of a library, which is what the
// SDK is for. What is left is one POST of one JSON document, whose shape the OTLP specification
// fixes, and a queue in front of it. The SDK would put its trace, metric and resource modules and
// gRPC in go.sum for everyone who builds any part of this repository, agk run --local on a laptop
// included, to carry that; internal/docker gives the same reason for writing the Engine API by
// hand, and refuses the same modules by name.
//
// # What it costs a run
//
// Nothing a run waits on. Export queues and returns; one goroutine sends what is queued, a batch at
// a time, and a collector that is slow, down or refusing costs the spans it would have been sent
// and never a decision. A span that cannot be sent is dropped and said once per batch through
// Trouble, since telemetry that holds a run back to deliver itself has its priorities backwards.
// So is a span queued past the queue's bound, which is what a collector gone for long enough
// leaves, rather than a controller whose memory grows with the outage.
//
// A program given no endpoint builds no Exporter, and nothing here runs.
package otlp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// TracesPath is where an OTLP/HTTP collector takes spans, below the endpoint it is reached at, as
// OTEL_EXPORTER_OTLP_ENDPOINT is read by every SDK.
const TracesPath = "/v1/traces"

// The defaults, each of which is an Options field.
const (
	// DefaultInterval is how long a span waits for the batch it goes in. A few seconds, so that a
	// run's trace is readable soon after it ends, and not less, since a batch of one span is a
	// request per span.
	DefaultInterval = 5 * time.Second

	// DefaultBatch is the most spans one request carries. A run of a few hundred tasks goes in a
	// request or two, well under the four mebibytes a collector takes by default.
	DefaultBatch = 512

	// DefaultQueue is the most spans held waiting to be sent. Past it a new span is dropped: a
	// collector gone for an hour costs that hour's spans and not the controller's memory.
	DefaultQueue = 8192

	// DefaultTimeout bounds one request, so that a collector that accepts the connection and
	// never answers holds up nothing but its own batch.
	DefaultTimeout = 10 * time.Second
)

// Kind is OTLP's SpanKind. Internal is the only one written here: a run and a task are work the
// installation does, not a call between two services it can see both ends of.
type Kind int

const (
	KindInternal Kind = 1
)

// Attribute is one key and its value, a string or a whole number, which is all a span here carries.
type Attribute struct {
	Key   string
	Value any
}

// String is an attribute holding text.
func String(key, value string) Attribute { return Attribute{Key: key, Value: value} }

// Int is an attribute holding a whole number.
func Int(key string, value int) Attribute { return Attribute{Key: key, Value: int64(value)} }

// Span is one span, as it ended.
type Span struct {
	Trace  agk.TraceID
	ID     agk.SpanID
	Parent agk.SpanID

	Name       string
	Start, End time.Time
	Attributes []Attribute

	// Error is the status: false is left unset, as OTLP asks of whatever did not fail, and true
	// is an error, whose Message says what it was.
	Error   bool
	Message string
}

// Options are what an Exporter is given.
type Options struct {
	// Endpoint is the collector's base URL, http://localhost:4318 for one beside the program, and
	// the spans are posted to it followed by TracesPath.
	Endpoint string

	// Resource describes the program sending: service.name at least, which is what a backend
	// lists the spans under.
	Resource []Attribute

	// Client sends each request. Nil is a client bounded by DefaultTimeout.
	Client *http.Client

	// Interval, Batch and Queue are the defaults above where zero.
	Interval time.Duration
	Batch    int
	Queue    int

	// Trouble hears every batch that could not be sent and every span dropped for want of room,
	// once for each. Nil hears nothing.
	Trouble func(error)
}

// Exporter queues spans and sends them.
type Exporter struct {
	url      string
	resource []Attribute
	client   *http.Client
	interval time.Duration
	batch    int
	bound    int
	trouble  func(error)

	mu      sync.Mutex
	queued  []Span
	dropped int
	closed  bool

	// sending bounds what the loop sends, and abandon ends it: Close waits for the batch on
	// its way for as long as its own context allows, and no longer.
	sending context.Context
	abandon context.CancelFunc

	full chan struct{}
	stop chan struct{}
	done chan struct{}
}

// New starts an exporter to o.Endpoint. The endpoint is taken as written: whoever read it from a
// setting has already held it to that setting's rules.
func New(o Options) (*Exporter, error) {
	if o.Endpoint == "" {
		return nil, errors.New("otlp: an exporter with no endpoint, and a program given none builds none")
	}
	if o.Client == nil {
		// A redirect is not followed, since the endpoint was held to https, or http on this
		// machine, and a collector answering 307 towards http elsewhere would carry the spans
		// across a network in plaintext. A 3xx is then an answer that is not 2xx, and refused.
		o.Client = &http.Client{
			Timeout:       DefaultTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.Batch <= 0 {
		o.Batch = DefaultBatch
	}
	if o.Queue <= 0 {
		o.Queue = DefaultQueue
	}
	if o.Trouble == nil {
		o.Trouble = func(error) {}
	}
	e := &Exporter{
		url: o.Endpoint + TracesPath, resource: o.Resource, client: o.Client,
		interval: o.Interval, batch: o.Batch, bound: o.Queue, trouble: o.Trouble,
		full: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	e.sending, e.abandon = context.WithCancel(context.Background())
	go e.loop()
	return e, nil
}

// Export queues spans and returns at once. Past the queue's bound a span is dropped, and the drop
// is heard by Trouble with the next batch; once the exporter is closed, every span is dropped and
// heard at once, since there is no next batch.
func (e *Exporter) Export(spans ...Span) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		if len(spans) > 0 {
			e.trouble(fmt.Errorf("otlp: %d spans were dropped, exported after the exporter to %s was closed", len(spans), e.url))
		}
		return
	}
	for _, s := range spans {
		if len(e.queued) >= e.bound {
			e.dropped++
			continue
		}
		e.queued = append(e.queued, s)
	}
	ready := len(e.queued) >= e.batch
	e.mu.Unlock()
	if ready {
		select {
		case e.full <- struct{}{}:
		default:
		}
	}
}

// Close sends what is queued, within ctx, and stops. What ctx leaves no time for is dropped, a
// batch already on its way included.
func (e *Exporter) Close(ctx context.Context) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	e.mu.Unlock()
	close(e.stop)
	defer e.abandon()
	select {
	case <-e.done:
	case <-ctx.Done():
		e.abandon()
		<-e.done
		e.discard()
		return ctx.Err()
	}
	for {
		batch := e.take()
		if len(batch) == 0 {
			return nil
		}
		if err := e.send(ctx, batch); err != nil {
			e.trouble(err)
			if ctx.Err() != nil {
				e.discard()
				return ctx.Err()
			}
		}
	}
}

// discard drops what is still queued once Close has no time left to send it, and says how much.
func (e *Exporter) discard() {
	e.mu.Lock()
	n := len(e.queued) + e.dropped
	e.queued, e.dropped = nil, 0
	e.mu.Unlock()
	if n > 0 {
		e.trouble(fmt.Errorf("otlp: %d spans were dropped, the exporter to %s closing before they could be sent", n, e.url))
	}
}

// loop sends a batch whenever one is full or the interval has passed, until Close.
func (e *Exporter) loop() {
	defer close(e.done)
	tick := time.NewTicker(e.interval)
	defer tick.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-tick.C:
		case <-e.full:
		}
		for {
			batch := e.take()
			if len(batch) == 0 {
				break
			}
			if err := e.send(e.sending, batch); err != nil {
				e.trouble(err)
			}
			if len(batch) < e.batch {
				break
			}
		}
	}
}

// take removes up to one batch from the queue, and says any drop since the last one.
func (e *Exporter) take() []Span {
	e.mu.Lock()
	n := min(len(e.queued), e.batch)
	batch := append([]Span(nil), e.queued[:n]...)
	e.queued = e.queued[n:]
	dropped := e.dropped
	e.dropped = 0
	e.mu.Unlock()
	if dropped > 0 {
		e.trouble(fmt.Errorf("otlp: %d spans were dropped, the queue holding %d already: the collector at %s has not been taking them", dropped, e.bound, e.url))
	}
	return batch
}

// send posts one batch.
func (e *Exporter) send(ctx context.Context, spans []Span) error {
	body, err := json.Marshal(Encode(e.resource, spans))
	if err != nil {
		return fmt.Errorf("otlp: %d spans could not be written: %w", len(spans), err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("otlp: %d spans could not be sent: %w", len(spans), err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("otlp: %d spans were dropped, the collector at %s could not be reached: %w", len(spans), e.url, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("otlp: %d spans were dropped, the collector at %s answered %s", len(spans), e.url, resp.Status)
	}
	return nil
}

// Encode is the ExportTraceServiceRequest a batch travels as, in OTLP's JSON encoding, which departs
// from protobuf's own JSON mapping where a collector would refuse or misread it: identifiers in
// lowercase hexadecimal rather than base64, enumerations as integers only, and keys in
// lowerCamelCase only. Every 64-bit number is a decimal string, as that mapping writes one.
func Encode(resource []Attribute, spans []Span) any {
	out := make([]map[string]any, 0, len(spans))
	for _, s := range spans {
		span := map[string]any{
			"traceId":           s.Trace.String(),
			"spanId":            s.ID.String(),
			"name":              s.Name,
			"kind":              int(KindInternal),
			"startTimeUnixNano": nanos(s.Start),
			"endTimeUnixNano":   nanos(s.End),
			"attributes":        attributes(s.Attributes),
		}
		if s.Parent.Valid() {
			span["parentSpanId"] = s.Parent.String()
		}
		if s.Error {
			span["status"] = map[string]any{"code": 2, "message": s.Message}
		}
		out = append(out, span)
	}
	return map[string]any{
		"resourceSpans": []any{map[string]any{
			"resource": map[string]any{"attributes": attributes(resource)},
			"scopeSpans": []any{map[string]any{
				"scope": map[string]any{"name": "github.com/agentiik/agentiik"},
				"spans": out,
			}},
		}},
	}
}

// nanos is an instant as OTLP writes one: nanoseconds since the Unix epoch, as a string.
func nanos(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.UnixNano(), 10)
}

// attributes writes each key and its value as an AnyValue.
func attributes(as []Attribute) []any {
	out := make([]any, 0, len(as))
	for _, a := range as {
		var value map[string]any
		switch v := a.Value.(type) {
		case int64:
			value = map[string]any{"intValue": strconv.FormatInt(v, 10)}
		default:
			value = map[string]any{"stringValue": fmt.Sprint(v)}
		}
		out = append(out, map[string]any{"key": a.Key, "value": value})
	}
	return out
}
