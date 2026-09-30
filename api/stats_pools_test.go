package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GET /api/v1/stats/pools answers administrators alone, every pool by name with its runners and
// their silences, in the fields openapi.json names; and in CSV a row a pool and a bucket, its
// runner empty. What the figures count is held in the db package, over heartbeats and tasks.
func TestTheStatisticsOfThePoolsAnswerAdministratorsAlone(t *testing.T) {
	h, _ := withRunnersAuthorizedBy(t, everything{who: "admin"})
	query := "?from=2026-09-30T06:00:00Z&to=2026-09-30T08:00:00Z&bucket=1h"
	w, _ := call(t, h, "GET", "/api/v1/stats/pools"+query, "admin", nil)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("an administrator asking for the pools was answered %d: %s", w.Code, w.Body)
	}
	d := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	d.DisallowUnknownFields()
	var got struct {
		From   string `json:"from"`
		To     string `json:"to"`
		Bucket string `json:"bucket"`
		Pools  []struct {
			Pool    string `json:"pool"`
			Buckets []struct {
				Since    string `json:"since"`
				Until    string `json:"until"`
				InUseMax int    `json:"slots_in_use_max"`
				Capacity int64  `json:"capacity"`
			} `json:"buckets"`
			Runners []json.RawMessage `json:"runners"`
		} `json:"pools"`
	}
	if err := d.Decode(&got); err != nil {
		t.Fatalf("the pools answered %s: %v", w.Body, err)
	}
	var names []string
	for _, p := range got.Pools {
		names = append(names, p.Pool)
		if len(p.Buckets) != 2 || p.Buckets[0].Since != "2026-09-30T06:00:00Z" || p.Buckets[1].Until != "2026-09-30T07:59:59.999999999Z" || p.Runners == nil {
			t.Errorf("pool %s answered %+v and runners %v", p.Pool, p.Buckets, p.Runners)
		}
	}
	if strings.Join(names, " ") != "default dmz" {
		t.Errorf("the pools answered are %v, want default and dmz by name", names)
	}

	r, _ := http.NewRequestWithContext(t.Context(), "GET", "/api/v1/stats/pools"+query, nil)
	r.Header.Set("Authorization", "Bearer admin")
	r.Header.Set("Accept", "text/csv")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	csv := rec.Body.String()
	want := "pool,runner,since,until,slots_in_use_max,capacity\r\n" +
		"default,,2026-09-30T06:00:00Z,2026-09-30T06:59:59.999999999Z,0,0\r\n" +
		"default,,2026-09-30T07:00:00Z,2026-09-30T07:59:59.999999999Z,0,0\r\n" +
		"dmz,,2026-09-30T06:00:00Z,2026-09-30T06:59:59.999999999Z,0,0\r\n" +
		"dmz,,2026-09-30T07:00:00Z,2026-09-30T07:59:59.999999999Z,0,0\r\n"
	if csv != want {
		t.Errorf("the pools in CSV were answered\n%q\nwant\n%q", csv, want)
	}

	h, _ = withRunnersAuthorizedBy(t, everything{who: "admin"})
	if w, _ := call(t, h, "GET", "/api/v1/stats/pools"+query, "alice", nil); w.Code != http.StatusForbidden {
		t.Errorf("somebody who is no administrator was answered %d: %s", w.Code, w.Body)
	}
	if w, _ := call(t, h, "GET", "/api/v1/stats/pools?bucket=5m", "admin", nil); w.Code != http.StatusBadRequest {
		t.Errorf("a bucket that is none was answered %d", w.Code)
	}
}
