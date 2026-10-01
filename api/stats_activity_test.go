package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// GET /api/v1/stats/activity answers administrators alone, every bucket in the fields openapi.json
// names and what runs now beside it; and in CSV a row a bucket and a column a state. What the
// figures count is held in the db package, over runs, tasks and runners.
func TestTheInstallationsActivityAnswersAdministratorsAlone(t *testing.T) {
	h, _ := withRunnersAuthorizedBy(t, everything{who: "admin"})
	query := "?from=2026-09-30T06:00:00Z&to=2026-09-30T06:02:00Z&bucket=1m"
	w, _ := call(t, h, "GET", "/api/v1/stats/activity"+query, "admin", nil)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("an administrator asking for the activity was answered %d: %s", w.Code, w.Body)
	}
	d := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	d.DisallowUnknownFields()
	var got struct {
		From    string `json:"from"`
		To      string `json:"to"`
		Bucket  string `json:"bucket"`
		Buckets []struct {
			Since            string         `json:"since"`
			Until            string         `json:"until"`
			Runs             map[string]int `json:"runs"`
			TasksInFlightMax int            `json:"tasks_in_flight_max"`
		} `json:"buckets"`
		Now struct {
			At            string         `json:"at"`
			Runs          map[string]int `json:"runs"`
			TasksInFlight int            `json:"tasks_in_flight"`
			Slots         int64          `json:"slots"`
			RunnersReady  int            `json:"runners_ready"`
			Runners       int            `json:"runners"`
		} `json:"now"`
	}
	if err := d.Decode(&got); err != nil {
		t.Fatalf("the activity answered %s: %v", w.Body, err)
	}
	if got.From != "2026-09-30T06:00:00Z" || got.To != "2026-09-30T06:02:00Z" || got.Bucket != "1m" || len(got.Buckets) != 2 {
		t.Fatalf("the activity answered %+v", got)
	}
	if b := got.Buckets[1]; b.Since != "2026-09-30T06:01:00Z" || b.Until != "2026-09-30T06:01:59.999999999Z" || len(b.Runs) != 7 {
		t.Errorf("the second bucket answered %+v", b)
	}
	if len(got.Now.Runs) != 3 || got.Now.At == "" {
		t.Errorf("now answered %+v", got.Now)
	}

	r, _ := http.NewRequestWithContext(t.Context(), "GET", "/api/v1/stats/activity"+query, nil)
	r.Header.Set("Authorization", "Bearer admin")
	r.Header.Set("Accept", "text/csv")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	want := "since,until,queued,running,waiting,succeeded,failed,cancelled,timed_out,tasks_in_flight_max\r\n" +
		"2026-09-30T06:00:00Z,2026-09-30T06:00:59.999999999Z,0,0,0,0,0,0,0,0\r\n" +
		"2026-09-30T06:01:00Z,2026-09-30T06:01:59.999999999Z,0,0,0,0,0,0,0,0\r\n"
	if csv := rec.Body.String(); csv != want {
		t.Errorf("the activity in CSV was answered\n%q\nwant\n%q", csv, want)
	}

	if w, _ := call(t, h, "GET", "/api/v1/stats/activity"+query, "alice", nil); w.Code != http.StatusForbidden {
		t.Errorf("somebody who is no administrator was answered %d: %s", w.Code, w.Body)
	}
	if w, _ := call(t, h, "GET", "/api/v1/stats/activity?bucket=5m", "admin", nil); w.Code != http.StatusBadRequest {
		t.Errorf("a bucket that is none was answered %d", w.Code)
	}
}
