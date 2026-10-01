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

	"github.com/agentiik/agentiik/internal/dbtest"
)

// The series of GET /api/v1/{ns}/stats/quotas, over finance's load in the half hour starting two
// days back, with max_runs_per_hour 500, max_concurrent_tasks 20 and max_artifact_bytes 1,000,000.

type quotasAnswer struct {
	From     string         `json:"from"`
	To       string         `json:"to"`
	Bucket   string         `json:"bucket"`
	Quotas   map[string]any `json:"quotas"`
	Buckets  []quotaBucket  `json:"buckets"`
	Previous *struct {
		From    string        `json:"from"`
		To      string        `json:"to"`
		Buckets []quotaBucket `json:"buckets"`
	} `json:"previous,omitempty"`
}

type quotaBucket struct {
	Since              string `json:"since"`
	Until              string `json:"until"`
	RunsCreated        int    `json:"runs_created"`
	RunsRefused        int    `json:"runs_refused"`
	TasksInFlightMax   int    `json:"tasks_in_flight_max"`
	ArtifactBytes      int64  `json:"artifact_bytes"`
	ArtifactBytesAdded int64  `json:"artifact_bytes_added"`
}

// loaded is someNamespaces with finance's load laid out from two days back:
//
//   - runs created at 1, 2 and 20 minutes, and runs refused 4 times in the third minute and once in
//     the sixteenth;
//   - tasks in flight from 1 to 10 minutes, 2 to 5, 4 to 18, 10 to 12, the one ending as the other
//     is handed out, 13 to 18, and one handed out at 19 still running: the most at once is 3 in the
//     first quarter, 2 carried into the second, where no more than 1 is ever handed out, and 1
//     carried into the third, where nothing changes;
//   - artifacts of 100 bytes written twice under one digest at 1 and 2 minutes, of 50 at 3 minutes
//     retired at 20, and of 7 at 16;
//   - and a task of hr's in flight throughout, which no series of finance counts.
func loaded(t *testing.T) (namespaces, time.Time) {
	t.Helper()
	in := someNamespaces(t)
	from := time.Now().UTC().Truncate(time.Hour).Add(-48 * time.Hour)
	at := func(minutes int) time.Time { return from.Add(time.Duration(minutes) * time.Minute) }
	conn := dbtest.Superuser(t, in.super)
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(t.Context(), statement, args...); err != nil {
			t.Fatalf("%s: %s", statement, err)
		}
	}
	exec(`update namespaces set max_runs_per_hour = 500, max_artifact_bytes = 1000000 where name = 'finance'`)
	exec(`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at)
		values ('finance', 'monthly-invoicing', 'a3f9c1e', '{}', 'carol', now()), ('hr', 'onboarding', 'a3f9c1e', '{}', 'carol', now())`)
	run := func(namespace, workflow string, i, minutes int) string {
		id := fmt.Sprintf("01M2Q%021d", i)
		exec(`insert into runs (namespace, id, workflow, commit, state, trigger, triggered_by, created_at)
			values ($1, $2, $3, 'a3f9c1e', 'running', 'manual', 'carol', $4)`, namespace, id, workflow, at(minutes))
		exec(`insert into steps (namespace, run_id, step, state) values ($1, $2, 'normalize', 'running')`, namespace, id)
		return id
	}
	first := run("finance", "monthly-invoicing", 1, 1)
	run("finance", "monthly-invoicing", 2, 2)
	run("finance", "monthly-invoicing", 3, 20)
	theirs := run("hr", "onboarding", 4, 1)
	exec(`insert into run_refusals (namespace, minute, refused) values ('finance', $1, 4), ('finance', $2, 1)`, at(3), at(16))

	for i, task := range []struct {
		namespace, run string
		published      int
		finished       int
	}{
		{"finance", first, 1, 10}, {"finance", first, 2, 5}, {"finance", first, 4, 18}, {"finance", first, 10, 12},
		{"finance", first, 13, 18}, {"finance", first, 19, -1}, {"hr", theirs, 0, -1},
	} {
		state, finished := "succeeded", any(nil)
		if task.finished >= 0 {
			finished = at(task.finished)
		} else {
			state = "running"
		}
		exec(`insert into tasks (namespace, id, run_id, step, attempt, state, published_at, dispatched_at, finished_at)
			values ($1, $2, $3, 'normalize', $4, $5, $6, $6, $7)`,
			task.namespace, fmt.Sprintf("01M2R%021d", i), task.run, i+1, state, at(task.published), finished)
	}
	for i, a := range []struct {
		digest   string
		size     int
		created  int
		retired  int
		stillHas bool
	}{
		{"d1", 100, 1, -1, true}, {"d1", 100, 2, -1, true}, {"d2", 50, 3, 20, false}, {"d3", 7, 16, -1, true},
	} {
		status, retired := "live", any(nil)
		if !a.stillHas {
			status, retired = "collected", at(a.retired)
		}
		exec(`insert into artifacts (namespace, run_id, step, port, name, digest, size_bytes, media_type, status, expires_at, created_at, retired_at)
			values ('finance', $1, 'normalize', 'ok', $2, $3, $4, 'application/pdf', $5, now() + interval '30 days', $6, $7)`,
			first, fmt.Sprintf("invoice-%d.pdf", i), "sha256:"+strings.Repeat(a.digest[1:], 64), a.size, status, at(a.created), retired)
	}
	return in, from
}

func quotas(t *testing.T, in namespaces, token, path string) quotasAnswer {
	t.Helper()
	w := in.ask(t, "GET", path, token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("%s was answered %d: %s", path, w.Code, w.Body)
	}
	d := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	d.DisallowUnknownFields()
	var out quotasAnswer
	if err := d.Decode(&out); err != nil {
		t.Fatalf("%s answered %s: %v", path, w.Body, err)
	}
	return out
}

// What the namespace asked of each quota, bucket by bucket: the runs it created and those it was
// refused, the most tasks in flight at once, one handed out as another ends counted once and one
// still running counted to now, and the bytes max_artifact_bytes counted at each bucket's end and
// written in it, each digest once; beside the quotas as they stand now, as its quotas route
// answers them. Nothing of another namespace is counted.
func TestASeriesOfQuotasCountsTheNamespacesLoad(t *testing.T) {
	in, from := loaded(t)
	stamp := func(minutes int) string {
		return from.Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339Nano)
	}
	until := func(minutes int) string {
		return from.Add(time.Duration(minutes)*time.Minute - time.Nanosecond).Format(time.RFC3339Nano)
	}
	path := fmt.Sprintf("/api/v1/finance/stats/quotas?from=%s&to=%s&bucket=15m", stamp(0), stamp(45))
	got := quotas(t, in, in.alice, path)
	want := []quotaBucket{
		{Since: stamp(0), Until: until(15), RunsCreated: 2, RunsRefused: 4, TasksInFlightMax: 3, ArtifactBytes: 150, ArtifactBytesAdded: 150},
		{Since: stamp(15), Until: until(30), RunsCreated: 1, RunsRefused: 1, TasksInFlightMax: 2, ArtifactBytes: 107, ArtifactBytesAdded: 7},
		{Since: stamp(30), Until: until(45), TasksInFlightMax: 1, ArtifactBytes: 107},
	}
	if !reflect.DeepEqual(got.Buckets, want) {
		t.Errorf("%s counted\n%+v\nwant\n%+v", path, got.Buckets, want)
	}
	if got.Bucket != "15m" || got.From != stamp(0) || got.To != stamp(45) {
		t.Errorf("%s answered from %s to %s in %s", path, got.From, got.To, got.Bucket)
	}
	w := in.ask(t, "GET", "/api/v1/namespaces/finance/quotas", in.alice, "")
	var asQuotas map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &asQuotas); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Quotas, asQuotas) || got.Quotas["max_runs_per_hour"] != 500.0 {
		t.Errorf("the series names the quotas %v, and the quotas route %v", got.Quotas, asQuotas)
	}

	path = fmt.Sprintf("/api/v1/finance/stats/quotas?from=%s&to=%s&bucket=15m&compare=previous", stamp(15), stamp(30))
	got = quotas(t, in, in.carol, path)
	if p := got.Previous; p == nil || p.From != stamp(0) || p.To != stamp(15) || !reflect.DeepEqual(p.Buckets, want[:1]) || !reflect.DeepEqual(got.Buckets, want[1:2]) {
		t.Errorf("%s answered %+v, then %+v before", path, got.Buckets, got.Previous)
	}

	csvWant := "period,since,until,runs_created,runs_refused,max_runs_per_hour,tasks_in_flight_max,max_concurrent_tasks,artifact_bytes,artifact_bytes_added,max_artifact_bytes\r\n" +
		"current," + stamp(0) + "," + until(15) + ",2,4,500,3,20,150,150,1000000\r\n" +
		"current," + stamp(15) + "," + until(30) + ",1,1,500,2,20,107,7,1000000\r\n"
	if csv := askCSV(t, in, fmt.Sprintf("/api/v1/finance/stats/quotas?from=%s&to=%s&bucket=15m", stamp(0), stamp(30))); csv != csvWant {
		t.Errorf("the quotas in CSV were answered\n%q\nwant\n%q", csv, csvWant)
	}

	// A series reaches back as far as the namespace keeps its runs, and no further.
	if _, err := dbtest.Superuser(t, in.super).Exec(t.Context(), `update namespaces set max_retention_days = 1 where name = 'finance'`); err != nil {
		t.Fatal(err)
	}
	got = quotas(t, in, in.alice, fmt.Sprintf("/api/v1/finance/stats/quotas?from=%s&to=%s&bucket=15m", stamp(0), stamp(30)))
	for _, b := range got.Buckets {
		if b.RunsCreated != 0 || b.RunsRefused != 0 || b.TasksInFlightMax != 0 || b.ArtifactBytes != 0 || b.ArtifactBytesAdded != 0 {
			t.Errorf("a bucket past a retention of one day counted %+v", b)
		}
	}
}

func askCSV(t *testing.T, in namespaces, path string) string {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), "GET", path, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+in.alice)
	r.Header.Set("Accept", "text/csv")
	w := httptest.NewRecorder()
	in.h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("%s in CSV was answered %d in %q: %s", path, w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	return w.Body.String()
}

// Whoever reads a namespace's quotas reads its load: an administrator and whoever holds a grant in
// it. Anybody else is answered as about a namespace that does not exist.
func TestASeriesOfQuotasIsReadAsTheQuotasAre(t *testing.T) {
	in, from := loaded(t)
	hour := fmt.Sprintf("?from=%s&to=%s", from.Format(time.RFC3339), from.Add(time.Hour).Format(time.RFC3339))
	for _, c := range []struct {
		token, namespace string
		status           int
	}{
		{in.alice, "finance", http.StatusOK},
		{in.carol, "hr", http.StatusOK},
		{in.erin, "hr", http.StatusNotFound},
		{in.erin, "finance", http.StatusNotFound},
		{in.carol, "nowhere", http.StatusNotFound},
		{"", "finance", http.StatusUnauthorized},
	} {
		w := in.ask(t, "GET", "/api/v1/"+c.namespace+"/stats/quotas"+hour, c.token, "")
		read := in.ask(t, "GET", "/api/v1/namespaces/"+c.namespace+"/quotas", c.token, "")
		if w.Code != c.status || w.Code != read.Code {
			t.Errorf("%s's load was answered %d, want %d, and its quotas %d: %s", c.namespace, w.Code, c.status, read.Code, w.Body)
		}
	}
	if w := in.ask(t, "GET", "/api/v1/finance/stats/quotas?bucket=5m", in.alice, ""); w.Code != http.StatusBadRequest {
		t.Errorf("a bucket that is none was answered %d", w.Code)
	}
}
