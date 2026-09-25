package otlp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// collector is an OTLP/HTTP collector that keeps what it was sent.
type collector struct {
	*httptest.Server
	mu       sync.Mutex
	requests []map[string]any
	paths    []string
	types    []string
}

func newCollector(t *testing.T, status int) *collector {
	t.Helper()
	c := &collector{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Errorf("the collector was sent something that is not JSON: %s", body)
		}
		c.mu.Lock()
		c.requests = append(c.requests, doc)
		c.paths = append(c.paths, r.Method+" "+r.URL.Path)
		c.types = append(c.types, r.Header.Get("Content-Type"))
		c.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(c.Close)
	return c
}

// spans answers every span the collector was sent, in the order it was sent them.
func (c *collector) spans() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, doc := range c.requests {
		for _, rs := range doc["resourceSpans"].([]any) {
			for _, ss := range rs.(map[string]any)["scopeSpans"].([]any) {
				for _, s := range ss.(map[string]any)["spans"].([]any) {
					out = append(out, s.(map[string]any))
				}
			}
		}
	}
	return out
}

func (c *collector) sent() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

func aSpan(name string) Span {
	trace, run := agk.RunID("01M2Z8V1P9C4XQ7K2N4D6F8H0C").Trace()
	return Span{
		Trace: trace, ID: agk.TaskSpan(name), Parent: run, Name: name,
		Start: time.Unix(1757829600, 5).UTC(), End: time.Unix(1757829612, 0).UTC(),
		Attributes: []Attribute{String("agentiik.namespace", "finance"), Int("agentiik.attempt", 2)},
	}
}

// What travels is OTLP's JSON encoding, whose departures from protobuf's own JSON mapping are the
// ones a collector would refuse or misread: identifiers in hexadecimal, and enumerations as
// integers. Every 64-bit number is a string, as that mapping already writes one.
func TestASpanTravelsInOTLPsJSONEncoding(t *testing.T) {
	c := newCollector(t, http.StatusOK)
	e, err := New(Options{Endpoint: c.URL, Resource: []Attribute{String("service.name", "agentiik-controller")}})
	if err != nil {
		t.Fatal(err)
	}
	failed := aSpan("normalize")
	failed.Error, failed.Message = true, "failed"
	root := aSpan("monthly-invoicing")
	root.Parent = agk.SpanID{}
	e.Export(failed, root)
	if err := e.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	if c.paths[0] != "POST /v1/traces" || c.types[0] != "application/json" {
		t.Errorf("the spans went as %s, %s, want POST /v1/traces, application/json", c.paths[0], c.types[0])
	}
	resource, _ := json.Marshal(c.requests[0]["resourceSpans"].([]any)[0].(map[string]any)["resource"])
	if want := `{"attributes":[{"key":"service.name","value":{"stringValue":"agentiik-controller"}}]}`; string(resource) != want {
		t.Errorf("the resource is %s, want %s", resource, want)
	}
	spans := c.spans()
	if len(spans) != 2 {
		t.Fatalf("the collector was sent %d spans, want 2", len(spans))
	}
	got, _ := json.Marshal(spans[0])
	want := `{"attributes":[{"key":"agentiik.namespace","value":{"stringValue":"finance"}},{"key":"agentiik.attempt","value":{"intValue":"2"}}],` +
		`"endTimeUnixNano":"1757829612000000000","kind":1,"name":"normalize","parentSpanId":"1bcead9eed447f5e",` +
		`"spanId":"` + agk.TaskSpan("normalize").String() + `","startTimeUnixNano":"1757829600000000005",` +
		`"status":{"code":2,"message":"failed"},"traceId":"65adc84f140f8df02002e0395b782fe8"}`
	if string(got) != want {
		t.Errorf("the span travelled as\n%s\nwant\n%s", got, want)
	}
	if _, ok := spans[1]["parentSpanId"]; ok {
		t.Errorf("a root span names a parent: %v", spans[1])
	}
	if _, ok := spans[1]["status"]; ok {
		t.Errorf("a span that did not fail carries a status: %v", spans[1])
	}
}

// A full batch goes at once, rather than waiting out the interval.
func TestAFullBatchGoesWithoutWaitingForTheInterval(t *testing.T) {
	c := newCollector(t, http.StatusOK)
	e, err := New(Options{Endpoint: c.URL, Interval: time.Hour, Batch: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	e.Export(aSpan("a"), aSpan("b"))
	for deadline := time.Now().Add(5 * time.Second); c.sent() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a full batch waited for an interval of an hour")
		}
	}
}

// A collector that is not taking spans costs the spans and nothing else: Export never waits, what
// is queued past the bound is dropped and said, and a refused batch is said.
func TestACollectorThatRefusesCostsTheSpansAndSaysSo(t *testing.T) {
	c := newCollector(t, http.StatusServiceUnavailable)
	var mu sync.Mutex
	var heard []string
	e, err := New(Options{Endpoint: c.URL, Interval: time.Hour, Batch: 10, Queue: 3, Trouble: func(err error) {
		mu.Lock()
		heard = append(heard, err.Error())
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	e.Export(aSpan("a"), aSpan("b"), aSpan("c"), aSpan("d"), aSpan("e"))
	if err := e.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(c.spans()); got != 3 {
		t.Errorf("the collector was sent %d spans, want the 3 the queue holds", got)
	}
	mu.Lock()
	said := strings.Join(heard, "\n")
	mu.Unlock()
	if !strings.Contains(said, "2 spans were dropped") || !strings.Contains(said, "503") {
		t.Errorf("trouble heard\n%s\nwant the 2 spans dropped for want of room and the refusal", said)
	}
}

// A collector that never answers holds up nobody: Export returns at once, and Close gives up when
// its context does.
func TestACollectorThatNeverAnswersHoldsUpNothing(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(hang)
	e, err := New(Options{Endpoint: srv.URL, Interval: time.Millisecond, Batch: 1})
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	for range 100 {
		e.Export(aSpan("a"))
	}
	if waited := time.Since(began); waited > time.Second {
		t.Fatalf("exporting to a collector that never answers took %s", waited)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := e.Close(ctx); err == nil {
		t.Error("closing on a collector that never answers says everything was sent")
	}
	if waited := time.Since(began); waited > 3*time.Second {
		t.Errorf("closing took %s, and its context allowed 200ms", waited)
	}
}

func TestAnExporterWithNoEndpointIsRefused(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("an exporter with no endpoint was built")
	}
}

// Every span dropped is said, however it was dropped: queued behind a batch Close had no time to
// finish, or exported once the exporter was closed. The count heard is the count lost.
func TestEverySpanDroppedOnTheWayOutIsSaid(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(hang)
	var mu sync.Mutex
	var heard []string
	e, err := New(Options{Endpoint: srv.URL, Interval: time.Millisecond, Batch: 100, Trouble: func(err error) {
		mu.Lock()
		heard = append(heard, err.Error())
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	e.Export(aSpan("in flight"))
	time.Sleep(100 * time.Millisecond)
	for range 50 {
		e.Export(aSpan("queued"))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	e.Close(ctx)
	e.Export(aSpan("late"), aSpan("later"))

	mu.Lock()
	defer mu.Unlock()
	dropped := 0
	for _, line := range heard {
		var n int
		if _, err := fmt.Sscanf(line, "otlp: %d spans were dropped", &n); err == nil {
			dropped += n
		}
	}
	if dropped != 53 {
		t.Errorf("trouble heard of %d spans dropped, and 53 were: one in flight, 50 queued behind it and 2 exported once closed\n%s", dropped, strings.Join(heard, "\n"))
	}
}

// A collector that redirects is refused rather than followed: the endpoint was held to https, or
// http on this machine, and a redirect could send the spans anywhere in plaintext.
func TestARedirectIsNotFollowed(t *testing.T) {
	elsewhere := newCollector(t, http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+TracesPath, http.StatusPermanentRedirect)
	}))
	defer srv.Close()
	var heard []string
	e, err := New(Options{Endpoint: srv.URL, Trouble: func(err error) { heard = append(heard, err.Error()) }})
	if err != nil {
		t.Fatal(err)
	}
	e.Export(aSpan("a"))
	e.Close(t.Context())
	if elsewhere.sent() != 0 {
		t.Error("the spans followed a redirect to another server")
	}
	if len(heard) != 1 || !strings.Contains(heard[0], "308") {
		t.Errorf("trouble heard %q, want the redirect refused", heard)
	}
}
