package api_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// "Past it the API answers 429 with Retry-After, the seconds until the oldest run counted leaves
// the window": a run past the namespace's max_runs_per_hour is refused, no run is written, and
// the caller is told when one more fits, in whole seconds rounded up.
func TestARunPastTheRunsAnHourIsAnswered429WithRetryAfter(t *testing.T) {
	h, _, super := serving(t)
	if w, _ := call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/"+aCommit, "alice", aPush(t)); w.Code != http.StatusOK {
		t.Fatalf("the push answered %d: %s", w.Code, w.Body)
	}
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `update namespaces set max_runs_per_hour = 1 where name = 'finance'`); err != nil {
		t.Fatal(err)
	}
	start := api.Start{Commit: aCommit, Inputs: map[string]any{"orders": []any{}}}

	if w, _ := call(t, h, "POST", "/api/v1/finance/workflows/monthly-invoicing/runs", "alice", start); w.Code != http.StatusAccepted {
		t.Fatalf("the run within the quota answered %d: %s", w.Code, w.Body)
	}
	// Created 59 minutes and a half ago, so one more fits in 30 seconds.
	if _, err := conn.Exec(t.Context(),
		`update runs set created_at = now() - interval '59 minutes 30 seconds' where namespace = 'finance'`); err != nil {
		t.Fatal(err)
	}

	w, answer := call(t, h, "POST", "/api/v1/finance/workflows/monthly-invoicing/runs", "alice", start)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("a run past the quota answered %d: %s", w.Code, w.Body)
	}
	after, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || after < 26 || after > 30 {
		t.Errorf("Retry-After reads %q, and the oldest run counted leaves the window in 30 seconds", w.Header().Get("Retry-After"))
	}
	if said, _ := answer["error"].(string); !strings.Contains(said, "max_runs_per_hour") || !strings.Contains(said, "namespace finance") {
		t.Errorf("the refusal says %q", said)
	}
	if w.Header().Get("Location") != "" {
		t.Errorf("a refused run is at %q", w.Header().Get("Location"))
	}
	var runs int
	if err := conn.QueryRow(t.Context(), `select count(*) from runs where namespace = 'finance'`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Errorf("finance holds %d runs after one was refused", runs)
	}

	// Another namespace's quota is not team-ops' own.
	if w, _ := call(t, h, "PUT", "/api/v1/team-ops/workflows/monthly-invoicing/versions/"+aCommit, "alice", aPushOf(t, named(workflowDocument, "team-ops", "monthly-invoicing"))); w.Code != http.StatusOK {
		t.Fatalf("the push to team-ops answered %d: %s", w.Code, w.Body)
	}
	for range 3 {
		if w, _ := call(t, h, "POST", "/api/v1/team-ops/workflows/monthly-invoicing/runs", "alice", start); w.Code != http.StatusAccepted {
			t.Fatalf("a run in a namespace with no quota answered %d: %s", w.Code, w.Body)
		}
	}
}
