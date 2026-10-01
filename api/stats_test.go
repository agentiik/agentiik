package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
)

// The series of GET /api/v1/{ns}/stats/runs, over runs laid out an hour at a time: what they come
// to, and who is shown it.

// seriesAnswer is what the route answers, as openapi.json names it field by field.
type seriesAnswer struct {
	From      string         `json:"from"`
	To        string         `json:"to"`
	Bucket    string         `json:"bucket"`
	Workflow  string         `json:"workflow,omitempty"`
	Buckets   []seriesBucket `json:"buckets"`
	Overall   seriesBucket   `json:"overall"`
	Histogram *[]seriesBin   `json:"histogram,omitempty"`
	Previous  *struct {
		From      string         `json:"from"`
		To        string         `json:"to"`
		Buckets   []seriesBucket `json:"buckets"`
		Overall   seriesBucket   `json:"overall"`
		Histogram *[]seriesBin   `json:"histogram,omitempty"`
	} `json:"previous,omitempty"`
}

type seriesBucket struct {
	Since     string          `json:"since"`
	Until     string          `json:"until"`
	Runs      map[string]int  `json:"runs"`
	Duration  *seriesPercents `json:"duration_ms,omitempty"`
	QueueWait *seriesPercents `json:"queue_wait_ms,omitempty"`
	Retries   []seriesRetry   `json:"retries"`
}

type seriesPercents struct {
	P50 int64 `json:"p50"`
	P95 int64 `json:"p95"`
	P99 int64 `json:"p99"`
}

type seriesRetry struct {
	ExitCode *int `json:"exit_code"`
	Attempts int  `json:"attempts"`
}

type seriesBin struct {
	From  int64 `json:"from_ms"`
	Until int64 `json:"until_ms"`
	Runs  int   `json:"runs"`
}

// laidOut is someRuns placed in the hour starting at from, two days back so that they sit well
// within the retention whenever the test runs:
//
//   - finance/monthly-invoicing's first run at from, which succeeded in 10 seconds after a lost
//     dispatch and a first attempt that exited 108, its tasks waiting 1, 2 and 3 seconds;
//   - its second at from plus 5 minutes, which failed in 30 seconds after three attempts, exiting
//     108 and 1, one task written before a wait was recorded, one handed out at once, and one never
//     dispatched;
//   - finance/payroll's run at from plus 20 minutes, still running after a lost dispatch, its task
//     waiting 4 seconds, and its second attempt read as dispatched 2 seconds before it was ready,
//     as a skew between two clocks reads it;
//   - and team-ops/monthly-invoicing's at from plus 2 minutes, which no series of finance counts.
func laidOut(t *testing.T) (someRuns, time.Time) {
	t.Helper()
	s := withSomeRuns(t)
	from := time.Now().UTC().Truncate(time.Hour).Add(-48 * time.Hour)
	at := func(d time.Duration) time.Time { return from.Add(d) }
	for _, r := range []struct {
		run              string
		state            string
		created, started time.Duration
		took             time.Duration
	}{
		{s.finance[0], "succeeded", 0, time.Minute, 10 * time.Second},
		{s.finance[1], "failed", 5 * time.Minute, 5 * time.Minute, 30 * time.Second},
		{s.payroll, "running", 20 * time.Minute, 20 * time.Minute, 0},
		{s.teamOps, "succeeded", 2 * time.Minute, 2 * time.Minute, time.Hour},
	} {
		var finished any
		if r.took > 0 {
			finished = at(r.started + r.took)
		}
		s.sql(t, `update runs set state = $2, created_at = $3, started_at = $4, finished_at = $5 where id = $1`,
			r.run, r.state, at(r.created), at(r.started), finished)
	}
	namespaceOf := map[string]string{s.finance[0]: "finance", s.finance[1]: "finance", s.payroll: "finance", s.teamOps: "team-ops"}
	for i, task := range []struct {
		run               string
		attempt, requeue  int
		state             string
		exit              any
		ready, dispatched any
	}{
		{s.finance[0], 1, 0, "lost", nil, at(60 * time.Second), at(61 * time.Second)},
		{s.finance[0], 1, 1, "failed", 108, at(70 * time.Second), at(72 * time.Second)},
		{s.finance[0], 2, 0, "succeeded", 0, at(80 * time.Second), at(83 * time.Second)},
		{s.finance[1], 1, 0, "failed", 108, nil, at(5*time.Minute + time.Second)},
		{s.finance[1], 2, 0, "failed", 1, at(5*time.Minute + 10*time.Second), at(5*time.Minute + 10*time.Second)},
		{s.finance[1], 3, 0, "pending", nil, at(5*time.Minute + 20*time.Second), nil},
		{s.payroll, 1, 0, "lost", nil, at(20 * time.Minute), at(20*time.Minute + 4*time.Second)},
		{s.payroll, 2, 0, "running", nil, at(20*time.Minute + 30*time.Second), at(20*time.Minute + 28*time.Second)},
		{s.teamOps, 1, 0, "failed", 99, at(2 * time.Minute), at(2*time.Minute + time.Hour)},
		{s.teamOps, 2, 0, "succeeded", 0, at(2 * time.Minute), at(2 * time.Minute)},
	} {
		s.sql(t, `insert into tasks (namespace, id, run_id, step, attempt, requeue, state, exit_code, ready_at, dispatched_at)
			values ($1, $2, $3, 'normalize', $4, $5, $6, $7, $8, $9)`,
			namespaceOf[task.run], fmt.Sprintf("01M2S%021d", i), task.run, task.attempt, task.requeue, task.state, task.exit, task.ready, task.dispatched)
	}
	return s, from
}

// statsOf asks for a series, in CSV where accept says so, and answers what was answered.
func statsOf(t *testing.T, h http.Handler, as, path, accept string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	if as != "" {
		r.Header.Set("Authorization", "Bearer "+as)
	}
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// series asks for a series in JSON and decodes it, refusing a field openapi.json does not name.
func series(t *testing.T, h http.Handler, as, path string) seriesAnswer {
	t.Helper()
	w := statsOf(t, h, as, path, "")
	if w.Code != http.StatusOK {
		t.Fatalf("%s asking for %s was answered %d: %s", as, path, w.Code, w.Body)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("%s was answered with Cache-Control %q", path, got)
	}
	d := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	d.DisallowUnknownFields()
	var out seriesAnswer
	if err := d.Decode(&out); err != nil {
		t.Fatalf("%s answered %s: %v", path, w.Body, err)
	}
	return out
}

// runsIn is a bucket's runs by state, those counted given and every other state zero.
func runsIn(counted map[string]int) map[string]int {
	out := map[string]int{"queued": 0, "running": 0, "waiting": 0, "succeeded": 0, "failed": 0, "cancelled": 0, "timed_out": 0}
	for state, n := range counted {
		out[state] = n
	}
	return out
}

func code(n int) *int { return &n }

// A series counts what the listing of runs lists, and only that: run:read held on the namespace
// counts every workflow's runs there, held on one workflow that workflow's, and a namespace or a
// workflow the caller cannot read counts nothing, answered exactly as one that does not exist, so
// that a series is no way of learning which exist. "An aggregate over runs discloses the runs, so it
// answers to the same grants."
func TestASeriesCountsOnlyTheRunsItsCallerCanRead(t *testing.T) {
	s, from := laidOut(t)
	finance := api.Target{Namespace: "finance"}
	payroll := api.Target{Namespace: "finance", Workflow: "payroll"}
	h := s.servedTo(t, denying{
		allowed: granted{
			"alice": {{api.RunRead, finance}},
			"bob":   {{api.RunRead, payroll}},
			"carol": {{api.RunRead, api.Target{Namespace: "team-ops"}}},
			"dave":  {{api.RunRead, finance}},
		},
		denied: granted{"dave": {{api.RunRead, payroll}}},
	})
	hour := fmt.Sprintf("?from=%s&to=%s&bucket=15m", from.Format(time.RFC3339), from.Add(time.Hour).Format(time.RFC3339))
	counts := func(as, path string) []map[string]int {
		t.Helper()
		out := []map[string]int{}
		for _, b := range series(t, h, as, path).Buckets {
			out = append(out, b.Runs)
		}
		return out
	}
	none := runsIn(nil)
	for _, c := range []struct {
		as, path string
		want     []map[string]int
	}{
		{"alice", "/api/v1/finance/stats/runs" + hour, []map[string]int{runsIn(map[string]int{"succeeded": 1, "failed": 1}), runsIn(map[string]int{"running": 1}), none, none}},
		{"alice", "/api/v1/finance/stats/runs" + hour + "&workflow=payroll", []map[string]int{none, runsIn(map[string]int{"running": 1}), none, none}},
		{"bob", "/api/v1/finance/stats/runs" + hour, []map[string]int{none, runsIn(map[string]int{"running": 1}), none, none}},
		{"bob", "/api/v1/finance/stats/runs" + hour + "&workflow=monthly-invoicing", []map[string]int{none, none, none, none}},
		{"carol", "/api/v1/finance/stats/runs" + hour, []map[string]int{none, none, none, none}},
		{"carol", "/api/v1/team-ops/stats/runs" + hour, []map[string]int{runsIn(map[string]int{"succeeded": 1}), none, none, none}},
		{"dave", "/api/v1/finance/stats/runs" + hour, []map[string]int{runsIn(map[string]int{"succeeded": 1, "failed": 1}), none, none, none}},
		{"dave", "/api/v1/finance/stats/runs" + hour + "&workflow=payroll", []map[string]int{none, none, none, none}},
	} {
		if got := counts(c.as, c.path); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s asking for %s counted %v, want %v", c.as, c.path, got, c.want)
		}
	}

	for _, c := range []struct {
		as, unreadable, nowhere string
	}{
		{"carol", "/api/v1/finance/stats/runs" + hour, "/api/v1/nowhere/stats/runs" + hour},
		{"dave", "/api/v1/finance/stats/runs" + hour + "&workflow=payroll", "/api/v1/finance/stats/runs" + hour + "&workflow=nothing"},
		{"bob", "/api/v1/finance/stats/runs" + hour + "&workflow=monthly-invoicing", "/api/v1/finance/stats/runs" + hour + "&workflow=nothing"},
	} {
		// Apart from the workflow it echoes, which the request named.
		unreadable, nowhere := series(t, h, c.as, c.unreadable), series(t, h, c.as, c.nowhere)
		unreadable.Workflow, nowhere.Workflow = "", ""
		if !reflect.DeepEqual(unreadable, nowhere) {
			t.Errorf("%s was answered %+v for %s, and %+v for %s, which does not exist", c.as, unreadable, c.unreadable, nowhere, c.nowhere)
		}
	}
	if unreadable, nowhere := statsOf(t, h, "carol", "/api/v1/finance/stats/runs"+hour, ""), statsOf(t, h, "carol", "/api/v1/nowhere/stats/runs"+hour, ""); unreadable.Body.String() != nowhere.Body.String() {
		t.Errorf("a namespace carol holds nothing in answers %s, and one that does not exist %s", unreadable.Body, nowhere.Body)
	}

	if w := statsOf(t, h, "", "/api/v1/finance/stats/runs"+hour, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("a request with no credential was answered %d", w.Code)
	}
	if w := statsOf(t, h, "alice", "/api/v1/finance/stats/runs?bucket=5m", ""); w.Code != http.StatusBadRequest {
		t.Errorf("a bucket that is none was answered %d", w.Code)
	}
}

// What the runs of a bucket came to, as openapi.json describes it: their states; the percentiles of
// how long those that ended took, from started_at to finished_at; those of how long their tasks
// waited to be handed out, over the tasks that recorded when they were ready and were dispatched, a
// wait a skew reads below zero counted as none; and the attempts retried, each under the exit code
// of its last dispatch, a lost one under none, the most frequent first. The histogram lays the
// durations of the runs that ended in bins from the shortest to the longest, and the span before
// is counted in the same buckets. The range taken as one bucket has its percentiles taken over every
// run and task of the range, which those of its buckets do not combine into.
func TestASeriesCountsWhatTheRunsCameTo(t *testing.T) {
	s, from := laidOut(t)
	h := s.servedTo(t, granted{"alice": {{api.RunRead, api.Target{Namespace: "finance"}}}})
	stamp := func(d time.Duration) string { return from.Add(d).Format(time.RFC3339Nano) }
	quarter := func(i int) (string, string) {
		return stamp(time.Duration(i) * 15 * time.Minute), stamp(time.Duration(i+1)*15*time.Minute - time.Nanosecond)
	}

	first := seriesBucket{
		Runs:      runsIn(map[string]int{"succeeded": 1, "failed": 1}),
		Duration:  &seriesPercents{P50: 20000, P95: 29000, P99: 29800},
		QueueWait: &seriesPercents{P50: 1500, P95: 2850, P99: 2970},
		Retries:   []seriesRetry{{ExitCode: code(108), Attempts: 2}, {ExitCode: code(1), Attempts: 1}},
	}
	first.Since, first.Until = quarter(0)
	second := seriesBucket{
		Runs:      runsIn(map[string]int{"running": 1}),
		QueueWait: &seriesPercents{P50: 2000, P95: 3800, P99: 3960},
		Retries:   []seriesRetry{{ExitCode: nil, Attempts: 1}},
	}
	second.Since, second.Until = quarter(1)
	want := seriesAnswer{From: stamp(0), To: stamp(time.Hour), Bucket: "15m", Buckets: []seriesBucket{first, second}}
	for i := 2; i < 4; i++ {
		b := seriesBucket{Runs: runsIn(nil), Retries: []seriesRetry{}}
		b.Since, b.Until = quarter(i)
		want.Buckets = append(want.Buckets, b)
	}
	bins := []seriesBin{{From: 10000, Until: 20001, Runs: 1}, {From: 20001, Until: 30000, Runs: 1}}
	want.Histogram = &bins
	// The waits of the hour are 1, 2 and 3 seconds, none, 4 seconds and one read below zero, so
	// none: a median of 1.5 seconds, where the buckets' are 1.5 and 2.
	want.Overall = seriesBucket{
		Since:     stamp(0),
		Until:     stamp(time.Hour - time.Nanosecond),
		Runs:      runsIn(map[string]int{"succeeded": 1, "failed": 1, "running": 1}),
		Duration:  &seriesPercents{P50: 20000, P95: 29000, P99: 29800},
		QueueWait: &seriesPercents{P50: 1500, P95: 3750, P99: 3950},
		Retries:   []seriesRetry{{ExitCode: code(108), Attempts: 2}, {ExitCode: code(1), Attempts: 1}, {ExitCode: nil, Attempts: 1}},
	}

	path := fmt.Sprintf("/api/v1/finance/stats/runs?from=%s&to=%s&bucket=15m&histogram=4", stamp(0), stamp(time.Hour))
	if got := series(t, h, "alice", path); !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("%s answered\n%s\nwant\n%s", path, gotJSON, wantJSON)
	}

	// The second quarter, with the first as the span before it and a histogram in each: one run
	// that ended lays out in one bin, and none in none.
	path = fmt.Sprintf("/api/v1/finance/stats/runs?from=%s&to=%s&bucket=15m&compare=previous&histogram=12", stamp(15*time.Minute), stamp(30*time.Minute))
	got := series(t, h, "alice", path)
	if got.From != stamp(15*time.Minute) || got.To != stamp(30*time.Minute) || !reflect.DeepEqual(got.Buckets, []seriesBucket{second}) || !reflect.DeepEqual(got.Overall, second) {
		t.Errorf("%s answered %+v, want the second quarter alone", path, got)
	}
	if got.Histogram == nil || len(*got.Histogram) != 0 {
		t.Errorf("%s answered the histogram %v, where no run of the quarter ended", path, got.Histogram)
	}
	if p := got.Previous; p == nil || p.From != stamp(0) || p.To != stamp(15*time.Minute) || !reflect.DeepEqual(p.Buckets, []seriesBucket{first}) ||
		!reflect.DeepEqual(p.Overall, first) || p.Histogram == nil || !reflect.DeepEqual(*p.Histogram, bins) {
		t.Errorf("%s answered the span before as %+v, want the first quarter", path, p)
	}

	// A narrowed series says which workflow it counts, and a histogram is only there when asked.
	path = fmt.Sprintf("/api/v1/finance/stats/runs?from=%s&to=%s&workflow=payroll", stamp(0), stamp(time.Hour))
	if got := series(t, h, "alice", path); got.Workflow != "payroll" || got.Bucket != "1m" || len(got.Buckets) != 60 || got.Histogram != nil || got.Previous != nil {
		t.Errorf("%s answered for %q in %d buckets of %s, with the histogram %v and the span before %v", path, got.Workflow, len(got.Buckets), got.Bucket, got.Histogram, got.Previous)
	}
}

// The same numbers to Accept: text/csv, a row a bucket and the span before after the range, then each
// span as one bucket, a figure the JSON leaves out an empty field and retried the attempts retried
// whatever their exit code.
func TestASeriesIsAnsweredInCSVToWhoeverAsks(t *testing.T) {
	s, from := laidOut(t)
	h := s.servedTo(t, granted{"alice": {{api.RunRead, api.Target{Namespace: "finance"}}}})
	stamp := func(d time.Duration) string { return from.Add(d).Format(time.RFC3339Nano) }
	path := fmt.Sprintf("/api/v1/finance/stats/runs?from=%s&to=%s&bucket=15m&compare=previous&histogram=3", stamp(15*time.Minute), stamp(30*time.Minute))

	w := statsOf(t, h, "alice", path, "text/csv")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("%s was answered %d in %q: %s", path, w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	want := "period,since,until,queued,running,waiting,succeeded,failed,cancelled,timed_out,duration_p50_ms,duration_p95_ms,duration_p99_ms,queue_wait_p50_ms,queue_wait_p95_ms,queue_wait_p99_ms,retried\r\n" +
		"current," + stamp(15*time.Minute) + "," + stamp(30*time.Minute-time.Nanosecond) + ",0,1,0,0,0,0,0,,,,2000,3800,3960,1\r\n" +
		"previous," + stamp(0) + "," + stamp(15*time.Minute-time.Nanosecond) + ",0,0,0,1,1,0,0,20000,29000,29800,1500,2850,2970,3\r\n" +
		"overall," + stamp(15*time.Minute) + "," + stamp(30*time.Minute-time.Nanosecond) + ",0,1,0,0,0,0,0,,,,2000,3800,3960,1\r\n" +
		"previous_overall," + stamp(0) + "," + stamp(15*time.Minute-time.Nanosecond) + ",0,0,0,1,1,0,0,20000,29000,29800,1500,2850,2970,3\r\n"
	if got := w.Body.String(); got != want {
		t.Errorf("%s answered\n%q\nwant\n%q", path, got, want)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("the CSV was answered with Cache-Control %q", got)
	}
	if json := statsOf(t, h, "alice", path, "text/csv;q=0.5, application/json"); !strings.HasPrefix(json.Header().Get("Content-Type"), "application/json") {
		t.Errorf("a request preferring JSON was answered in %q", json.Header().Get("Content-Type"))
	}
}

// A series reaches back as far as the namespace keeps its runs, and no further: a run's record
// outlives its retention, and a series stops where the retention does all the same.
func TestASeriesStopsWhereTheRetentionDoes(t *testing.T) {
	s, from := laidOut(t)
	h := s.servedTo(t, granted{"alice": {{api.RunRead, api.Target{Namespace: "finance"}}}})
	path := fmt.Sprintf("/api/v1/finance/stats/runs?from=%s&to=%s&bucket=1h&histogram=5", from.Format(time.RFC3339), from.Add(time.Hour).Format(time.RFC3339))
	if got := series(t, h, "alice", path); got.Buckets[0].Runs["succeeded"] != 1 {
		t.Fatalf("%s counted %v within the retention", path, got.Buckets[0].Runs)
	}
	s.sql(t, `update namespaces set max_retention_days = 1 where name = 'finance'`)
	got := series(t, h, "alice", path)
	if !reflect.DeepEqual(got.Buckets[0].Runs, runsIn(nil)) || got.Buckets[0].Duration != nil || got.Buckets[0].QueueWait != nil || len(got.Buckets[0].Retries) != 0 {
		t.Errorf("%s counted %+v past a retention of one day", path, got.Buckets[0])
	}
	if got.Histogram == nil || len(*got.Histogram) != 0 {
		t.Errorf("%s laid out %v past a retention of one day", path, got.Histogram)
	}
}
