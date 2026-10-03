package console

import (
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

var two = tea.KeyPressMsg{Code: '2', Text: "2"}

// monthlyInvoicing is what GET /api/v1/finance/workflows/monthly-invoicing answers: its repository,
// its graph and two commits of its history, the first a version.
var monthlyInvoicing = `{
  "repository": {"namespace": "finance", "name": "monthly-invoicing", "default_branch": "main", "protected": true, "head": "a3f9c1e04b7d2e8f6a1c3b5d7e9f0a2b4c6d8e0f"},
  "graph": ` + strings.Replace(invoicing, `"order"`, `"on": {"schedule": [{"cron": "0 6 1 * *", "timezone": "Europe/Paris"}], "webhook": [{"path": "/invoicing/rerun"}]}, "order"`, 1) + `,
  "history": [
    {"commit": "a3f9c1e04b7d2e8f6a1c3b5d7e9f0a2b4c6d8e0f", "author": {"name": "Alice Martin"}, "authored_at": "2026-09-25T10:05:00Z", "subject": "Round VAT per line rather than per invoice", "version": {"commit": "a3f9c1e04b7d2e8f6a1c3b5d7e9f0a2b4c6d8e0f"}},
    {"commit": "5d0b7e2c9a4f1e3d8c6b0a2f4e6d8c0b2a4f6e8d", "author": {"name": "Alice Martin"}, "authored_at": "2026-09-24T09:40:00Z", "subject": "Split the rounding out of the charge step"}
  ]
}`

// thirtyDays is its statistics over 30 days, with a histogram of six bins.
const thirtyDays = `{
  "overall": {"duration_ms": {"p50": 215000, "p95": 298000}},
  "histogram": [
    {"from_ms": 100000, "until_ms": 140000, "runs": 2},
    {"from_ms": 140000, "until_ms": 180000, "runs": 4},
    {"from_ms": 180000, "until_ms": 220000, "runs": 9},
    {"from_ms": 220000, "until_ms": 260000, "runs": 5},
    {"from_ms": 260000, "until_ms": 300000, "runs": 1},
    {"from_ms": 300000, "until_ms": 340000, "runs": 1}
  ]
}`

// 2 turns to the workflows the last 200 runs name, each with its latest run and its last twenty;
// the one chosen shows its repository, its triggers, its durations over 30 days with p50 and p95
// marked, and its versions.
func TestTheWorkflowsViewListsTheWorkflowsTheRunsName(t *testing.T) {
	in := &installation{runs: someRuns(), detail: monthlyInvoicing, stats: thirtyDays}
	m := press(t, opened(t, in, Options{}, 160, 40), two)
	if m.view != workflowsView || !slices.Contains(in.asked, "/api/v1/runs?limit=200") {
		t.Fatalf("2 leaves view %d, having read %v", m.view, in.asked)
	}
	s := screen(m)
	for _, want := range []string{
		"1 Runs   2 Workflows",
		"Workflows", "2 workflows named by the last 200 runs, since the API lists none",
		"finance/monthly-invoicing", "finance/nightly-export",
		"finance/monthly-invoicing  default branch main, protected · head a3f9c1e",
		"triggers  schedule 0 6 1 * * Europe/Paris · webhook /invoicing/rerun",
		"last 20   ▂█  median 1m 52s",
		"30 days   ▂▄█▄▂▂  p50 3m 35s  p95 4m 58s",
		"● a3f9c1e  10:05:00          Alice Martin      Round VAT per line rather than per invoice",
		"  5d0b7e2  2026-09-24 09:40",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the workflows view does not show %q:\n%s", want, s)
		}
	}
	to := now.UTC().Truncate(time.Minute)
	stats := "/api/v1/finance/stats/runs?workflow=monthly-invoicing&from=" + url.QueryEscape(to.AddDate(0, 0, -30).Format(time.RFC3339)) + "&to=" + url.QueryEscape(to.Format(time.RFC3339)) + "&bucket=1d&histogram=12"
	for _, want := range []string{"/api/v1/finance/workflows/monthly-invoicing?limit=5", stats} {
		if !slices.Contains(in.asked, want) {
			t.Errorf("the workflow chosen did not read %s: %v", want, in.asked)
		}
	}
	if lastLine(m) != "↑↓ Move   enter Graph   esc Runs   q Quit   ? Every key" {
		t.Errorf("the workflows view's keys are %q", lastLine(m))
	}

	// Moving to another reads it once; coming back reads nothing again.
	m = press(t, m, down)
	asked := len(in.asked)
	m = press(t, m, up, down)
	if m.flow != "finance/nightly-export" || len(in.asked) != asked {
		t.Errorf("moving back and forth read %v again", in.asked[asked:])
	}
}

// enter opens the graph of the workflow chosen at its head's version, with no run laid over it,
// and esc goes back to the workflows.
func TestAWorkflowsGraphIsDrawnWithNoRun(t *testing.T) {
	in := &installation{runs: someRuns(), detail: monthlyInvoicing, stats: thirtyDays}
	m := press(t, opened(t, in, Options{}, 160, 40), two, enter)
	s := screen(m)
	if m.view != graphView || !strings.Contains(s, "Graph finance/monthly-invoicing@a3f9c1e  the version the default branch's head resolves to") || !strings.Contains(s, "○ normalize") {
		t.Fatalf("enter does not draw the workflow's graph with no run:\n%s", s)
	}
	if lastLine(m) != "↑↓ Step   ←→ Along an edge   g List   esc Workflows   q Quit   ? Every key" {
		t.Errorf("a graph with no run offers %q", lastLine(m))
	}
	if m = press(t, m, enter, esc); m.view != workflowsView || m.graphOf != "" {
		t.Errorf("esc from a workflow's graph leaves view %d", m.view)
	}
}

// The bins holding the p50 and the p95 are drawn in their colours, and a workflow with no run
// ended in 30 days says so.
func TestTheHistogramMarksItsP50AndP95(t *testing.T) {
	p50, p95 := int64(215000), int64(298000)
	s := &flowStats{}
	s.Overall.DurationMS.P50, s.Overall.DurationMS.P95 = &p50, &p95
	for _, b := range [][3]int64{{100000, 200000, 3}, {200000, 280000, 6}, {280000, 300000, 1}} {
		s.Histogram = append(s.Histogram, struct {
			FromMS  int64 `json:"from_ms"`
			UntilMS int64 `json:"until_ms"`
			Runs    int   `json:"runs"`
		}{b[0], b[1], int(b[2])})
	}
	parts := histogram(s)
	if parts[0].role != muted || parts[1].role != runningText || parts[2].role != waitingText {
		t.Errorf("the bins are drawn %v", parts[:3])
	}
	if got := histogram(&flowStats{}); got[0].text != "no run ended in 30 days" {
		t.Errorf("an empty histogram says %q", got[0].text)
	}
}
