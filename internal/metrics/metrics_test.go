package metrics

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// written is what r answers a scrape with.
func written(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	if err := r.WriteTo(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The text format, line for line: each family's HELP and TYPE, its series in the order of their
// label values, a histogram's buckets cumulated with +Inf last, then its sum and its count.
func TestTheFamiliesAreWrittenInTheTextFormat(t *testing.T) {
	r := NewRegistry()
	lost := r.Counter("agentiik_tasks_lost_total", "Dispatches declared lost.", "pool")
	took := r.Histogram("agentiik_task_duration_seconds", "How long a container ran.", []float64{1, 10}, "brick", "state")
	r.Gauges(func(_ context.Context, g *Gauges) error {
		g.Set("agentiik_queue_depth", 3, "gpu")
		g.Set("agentiik_queue_depth", 0, "default")
		return nil
	}, Desc{Name: "agentiik_queue_depth", Help: "Messages on a pool's queue.", Labels: []string{"pool"}})

	lost.Inc("gpu")
	lost.Inc("default")
	lost.Add(2, "gpu")
	took.Observe(0.5, "invoice", "succeeded")
	took.Observe(10, "invoice", "succeeded")
	took.Observe(42, "invoice", "succeeded")
	took.Observe(3, `say "hi"`, "failed")

	want := `# HELP agentiik_metrics_folded_total Observations counted under the label set _other because their family already held as many label sets as it keeps.
# TYPE agentiik_metrics_folded_total counter
# HELP agentiik_tasks_lost_total Dispatches declared lost.
# TYPE agentiik_tasks_lost_total counter
agentiik_tasks_lost_total{pool="default"} 1
agentiik_tasks_lost_total{pool="gpu"} 3
# HELP agentiik_task_duration_seconds How long a container ran.
# TYPE agentiik_task_duration_seconds histogram
agentiik_task_duration_seconds_bucket{brick="invoice",state="succeeded",le="1"} 1
agentiik_task_duration_seconds_bucket{brick="invoice",state="succeeded",le="10"} 2
agentiik_task_duration_seconds_bucket{brick="invoice",state="succeeded",le="+Inf"} 3
agentiik_task_duration_seconds_sum{brick="invoice",state="succeeded"} 52.5
agentiik_task_duration_seconds_count{brick="invoice",state="succeeded"} 3
agentiik_task_duration_seconds_bucket{brick="say \"hi\"",state="failed",le="1"} 0
agentiik_task_duration_seconds_bucket{brick="say \"hi\"",state="failed",le="10"} 1
agentiik_task_duration_seconds_bucket{brick="say \"hi\"",state="failed",le="+Inf"} 1
agentiik_task_duration_seconds_sum{brick="say \"hi\"",state="failed"} 3
agentiik_task_duration_seconds_count{brick="say \"hi\"",state="failed"} 1
# HELP agentiik_queue_depth Messages on a pool's queue.
# TYPE agentiik_queue_depth gauge
agentiik_queue_depth{pool="default"} 0
agentiik_queue_depth{pool="gpu"} 3
`
	if got := written(t, r); got != want {
		t.Errorf("the registry wrote\n%s\nwant\n%s", got, want)
	}
}

// Past the limit, a new label set is counted under _other, and the family that folded is named. A
// label set already held keeps being counted as itself.
func TestALabelSetPastTheLimitIsFoldedIntoOther(t *testing.T) {
	r := NewRegistry()
	r.Limit = 2
	runs := r.Counter("agentiik_runs_total", "Runs.", "namespace", "workflow")
	runs.Inc("finance", "monthly-invoicing")
	runs.Inc("finance", "payroll")
	runs.Inc("finance", "reporting")
	runs.Inc("hr", "onboarding")
	runs.Inc("finance", "payroll")

	got := written(t, r)
	for _, line := range []string{
		`agentiik_runs_total{namespace="finance",workflow="monthly-invoicing"} 1`,
		`agentiik_runs_total{namespace="finance",workflow="payroll"} 2`,
		`agentiik_runs_total{namespace="_other",workflow="_other"} 2`,
		`agentiik_metrics_folded_total{metric="agentiik_runs_total"} 2`,
	} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("the registry wrote no line %s:\n%s", line, got)
		}
	}
	if strings.Contains(got, "reporting") || strings.Contains(got, "onboarding") {
		t.Errorf("a label set past the limit was kept:\n%s", got)
	}
}

// A gauge that cannot be read is left out, and said, and the rest is answered: the counters are
// worth having without it.
func TestAGaugeThatCannotBeReadIsLeftOutAndSaid(t *testing.T) {
	r := NewRegistry()
	var heard []string
	r.Trouble = func(families []string, err error) { heard = append(heard, families...) }
	r.Counter("agentiik_tasks_dispatched_total", "Dispatches.", "pool").Inc("default")
	r.Gauges(func(context.Context, *Gauges) error { return errors.New("the bus is away") },
		Desc{Name: "agentiik_queue_depth", Help: "Depth.", Labels: []string{"pool"}})

	got := written(t, r)
	if strings.Contains(got, "agentiik_queue_depth") {
		t.Errorf("a gauge that could not be read was written:\n%s", got)
	}
	if !strings.Contains(got, `agentiik_tasks_dispatched_total{pool="default"} 1`) {
		t.Errorf("the counters were not written beside a gauge that could not be read:\n%s", got)
	}
	if !slices.Equal(heard, []string{"agentiik_queue_depth"}) {
		t.Errorf("Trouble heard of %q", heard)
	}
}

// Only the token whose hash the program holds reads the metrics, only at /metrics and only with
// GET or HEAD.
func TestOnlyTheTokenReadsTheMetrics(t *testing.T) {
	r := NewRegistry()
	r.Counter("agentiik_tasks_lost_total", "Lost.", "pool").Inc("default")
	h := Handler(r, sha256.Sum256([]byte("s3cret")))

	for _, c := range []struct {
		method, path, auth string
		want               int
	}{
		{"GET", "/metrics", "", http.StatusUnauthorized},
		{"GET", "/metrics", "Bearer wrong", http.StatusUnauthorized},
		{"GET", "/metrics", "Bearer ", http.StatusUnauthorized},
		{"GET", "/metrics", "Basic czNjcmV0", http.StatusUnauthorized},
		{"GET", "/metrics", "s3cret", http.StatusUnauthorized},
		{"POST", "/metrics", "Bearer s3cret", http.StatusMethodNotAllowed},
		{"GET", "/", "Bearer s3cret", http.StatusNotFound},
		{"HEAD", "/metrics", "Bearer s3cret", http.StatusOK},
		{"GET", "/metrics", "Bearer s3cret", http.StatusOK},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s %s with %q was answered %d, want %d", c.method, c.path, c.auth, rec.Code, c.want)
			continue
		}
		body := rec.Body.String()
		switch {
		case c.want != http.StatusOK && strings.Contains(body, "agentiik_tasks_lost_total"):
			t.Errorf("%s %s with %q was answered the metrics", c.method, c.path, c.auth)
		case c.want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "":
			t.Errorf("a refused scrape was not told how to authenticate")
		case c.method == "GET" && c.want == http.StatusOK:
			if !strings.Contains(body, `agentiik_tasks_lost_total{pool="default"} 1`) {
				t.Errorf("the scrape was answered\n%s", body)
			}
			if got := rec.Header().Get("Content-Type"); got != ContentType {
				t.Errorf("the scrape was answered as %q", got)
			}
		}
	}
}

// The count of folds is never folded itself, however small the limit: folding it would be one more
// fold to count, for ever.
func TestTheCountOfFoldsIsNeverFolded(t *testing.T) {
	r := NewRegistry()
	r.Limit = 1
	for _, name := range []string{"agentiik_a_total", "agentiik_b_total", "agentiik_c_total"} {
		c := r.Counter(name, "A family.", "pool")
		c.Inc("one")
		c.Inc("two")
	}
	got := written(t, r)
	for _, name := range []string{"agentiik_a_total", "agentiik_b_total", "agentiik_c_total"} {
		if line := `agentiik_metrics_folded_total{metric="` + name + `"} 1`; !strings.Contains(got, line+"\n") {
			t.Errorf("the registry wrote no line %s:\n%s", line, got)
		}
	}
}

// The gauges of a scrape are read under one deadline, inside the time the scraper says it waits,
// however many of them hang: the counters are answered on time and the hung gauges left out.
func TestAScrapeAnswersWithinTheTimeItsScraperWaits(t *testing.T) {
	r := NewRegistry()
	r.Counter("agentiik_tasks_lost_total", "Lost.", "pool").Inc("default")
	hang := func(ctx context.Context, _ *Gauges) error {
		<-ctx.Done()
		return ctx.Err()
	}
	r.Gauges(hang, Desc{Name: "agentiik_queue_depth", Help: "Depth.", Labels: []string{"pool"}})
	r.Gauges(hang, Desc{Name: "agentiik_runner_slots", Help: "Slots.", Labels: []string{"pool"}})
	h := Handler(r, sha256.Sum256([]byte("s3cret")))

	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	req.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", "2")
	rec := httptest.NewRecorder()
	began := time.Now()
	h.ServeHTTP(rec, req)
	if took := time.Since(began); took > 1500*time.Millisecond {
		t.Errorf("a scrape its scraper waits 2s for took %s", took)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `agentiik_tasks_lost_total{pool="default"} 1`) {
		t.Errorf("the scrape was answered %d:\n%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "agentiik_queue_depth") {
		t.Errorf("a gauge that hung was answered:\n%s", rec.Body)
	}
}

// The gauges are given a second less than the scraper waits, and never more than DefaultReadBound,
// whatever it says it waits: the server writes within its own timeout.
func TestTheGaugesAreReadWithinTheScrapersTimeAndTheBound(t *testing.T) {
	for header, want := range map[string]time.Duration{
		"":     DefaultReadBound,
		"none": DefaultReadBound,
		"2":    time.Second,
		"0.5":  100 * time.Millisecond,
		"60":   DefaultReadBound,
	} {
		if got := readBound(header); got != want {
			t.Errorf("a scraper waiting %q gives the gauges %s, want %s", header, got, want)
		}
	}
}
