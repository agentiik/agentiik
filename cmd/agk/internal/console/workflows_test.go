package console

import (
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/db"
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

// finance's workflows as GET /api/v1/finance/workflows lists them: two with their newest run, and
// one never run, which no run names.
func financeWorkflows() map[string][]db.ListedWorkflow {
	runs := someRuns()
	latest := func(r db.ListedRun) *db.LatestRun {
		return &db.LatestRun{Run: r.Run, State: r.State, TriggerKind: r.Trigger, CreatedAt: r.CreatedAt}
	}
	return map[string][]db.ListedWorkflow{"finance": {
		{Name: "monthly-invoicing", CreatedAt: now.AddDate(0, -1, 0), Latest: latest(runs[0])},
		{Name: "nightly-export", CreatedAt: now.AddDate(0, -1, 0), Latest: latest(runs[2])},
		{Name: "quarterly-close", CreatedAt: now.AddDate(0, 0, -2)},
	}}
}

// 2 turns to the workflows GET /api/v1/{ns}/workflows lists, each with its latest run and its last
// twenty, and one never run among them; the one chosen shows its repository, its triggers, its
// durations over 30 days with p50 and p95 marked, and its versions. The view listed the workflows
// the last 200 runs named, and said the API listed none.
func TestTheWorkflowsViewListsTheWorkflowsTheAPIListsNeverRunIncluded(t *testing.T) {
	in := &installation{runs: someRuns(), workflows: financeWorkflows(), detail: monthlyInvoicing, stats: thirtyDays}
	m := press(t, opened(t, in, Options{}, 160, 40), two)
	if m.view != workflowsView || !slices.Contains(in.asked, "/api/v1/finance/workflows") || slices.Contains(in.asked, "/api/v1/runs?limit=200") {
		t.Fatalf("2 leaves view %d, having read %v", m.view, in.asked)
	}
	s := screen(m)
	for _, want := range []string{
		"1 Runs   2 Workflows",
		"Workflows", "3 workflows",
		"finance/monthly-invoicing", "finance/nightly-export",
		"finance/monthly-invoicing  default branch main, protected · head a3f9c1e",
		"triggers  schedule 0 6 1 * * Europe/Paris · webhook /invoicing/rerun",
		"last 20   ▁▂█  median 1m 07s",
		"30 days   ▂▄█▄▂▂  p50 3m 35s  p95 4m 58s",
		"● a3f9c1e  10:05:00          Alice Martin      Round VAT per line rather than per invoice",
		"  5d0b7e2  2026-09-24 09:40",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the workflows view does not show %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "the API lists none") || !slices.ContainsFunc(strings.Split(s, "\n"), func(l string) bool {
		return strings.HasPrefix(l, "finance/quarterly-close ") && strings.Contains(l, " never run ")
	}) {
		t.Errorf("the workflows view does not list the workflow never run, or says the API lists none:\n%s", s)
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
	// The workflow never run has no run to read the last of.
	asked = len(in.asked)
	if m = press(t, m, down); !strings.Contains(screen(m), "last 20   never run") || slices.ContainsFunc(in.asked[asked:], func(p string) bool {
		return strings.Contains(p, "quarterly-close") && strings.HasPrefix(p, "/api/v1/runs")
	}) {
		t.Errorf("the workflow never run reads %v, and shows:\n%s", in.asked[asked:], screen(m))
	}
}

// Every namespace where /me says the principal reads runs, on it or on one of its workflows, is
// listed, and no other; --namespace lists its own alone. A namespace that cannot be read fails the
// whole list, rather than leaving its workflows out of one that reads as complete.
func TestTheWorkflowsAreListedForEveryNamespaceTheRunsAreReadIn(t *testing.T) {
	me := &principal{Principal: "alice", Permissions: map[string][]string{
		"finance": {"run:read"}, "ops/nightly": {"run:read"}, "billing": {"secret:use"},
	}}
	in := &installation{runs: someRuns(), me: me}
	m := press(t, opened(t, in, Options{}, 120, 30), two)
	for path, want := range map[string]bool{"/api/v1/finance/workflows": true, "/api/v1/ops/workflows": true, "/api/v1/billing/workflows": false} {
		if slices.Contains(in.asked, path) != want {
			t.Errorf("reading %s is %v, having read %v", path, !want, in.asked)
		}
	}
	if s := screen(m); !strings.Contains(s, "2 workflows") || !strings.Contains(s, "finance/nightly-export") {
		t.Errorf("the workflows of every namespace are not listed:\n%s", s)
	}

	in = &installation{runs: someRuns(), me: me}
	press(t, opened(t, in, Options{Namespace: "ops"}, 120, 30), two)
	if slices.Contains(in.asked, "/api/v1/finance/workflows") || !slices.Contains(in.asked, "/api/v1/ops/workflows") {
		t.Errorf("a console opened on ops reads %v", in.asked)
	}

	in = &installation{runs: someRuns(), me: me, workflows: map[string][]db.ListedWorkflow{}}
	m = opened(t, in, Options{}, 120, 30)
	in.failing = errors.New("the installation could not be reached")
	if s := screen(press(t, m, two)); !strings.Contains(s, "The workflows could not be read") || strings.Contains(s, "No workflow") {
		t.Errorf("a namespace that could not be read leaves:\n%s", s)
	}
}

// The palette finds a workflow never run, which no run names, once the workflows are listed.
func TestThePaletteFindsAWorkflowNeverRun(t *testing.T) {
	in := &installation{runs: someRuns(), workflows: financeWorkflows()}
	m := typed(t, press(t, opened(t, in, Options{}, 120, 30), colon), "quarterly")
	if lines := paletteLines(m); len(lines) < 2 || !strings.HasPrefix(lines[1], "finance/quarterly-close") {
		t.Errorf("the palette does not find the workflow never run:\n%s", strings.Join(lines, "\n"))
	}
}

// enter opens the graph of the workflow chosen at its head's version, with no run laid over it,
// and esc goes back to the workflows.
func TestAWorkflowsGraphIsDrawnWithNoRun(t *testing.T) {
	in := &installation{runs: someRuns(), detail: monthlyInvoicing, stats: thirtyDays}
	m := press(t, opened(t, in, Options{}, 160, 40), two, enter)
	s := screen(m)
	if m.view != graphView || !strings.Contains(s, "Graph finance/monthly-invoicing@a3f9c1e  the default branch's head") || !strings.Contains(s, "○ normalize") {
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
