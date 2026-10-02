package console

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// invoicing is the resolved graph of monthly-invoicing as the API answers it, shaped as the
// documentation's example: normalize reads the orders and sends its rejected items to archive,
// invoice fans out over what normalize accepted, and archive takes both.
const invoicing = `{
  "workflow": "monthly-invoicing",
  "commit": "a3f9c1e04b7d2e8f6a1c3b5d7e9f0a2b4c6d8e0f",
  "inputs": {"orders": {"required": true}, "customers": {}},
  "outputs": {
    "invoices": {"from": {"step": "archive", "port": "out"}},
    "errors": {"from": {"step": "invoice", "port": "error"}}
  },
  "order": ["normalize", "invoice", "archive"],
  "steps": {
    "normalize": {"kind": "brick", "inputs": {"orders": "${{ workflow.inputs.orders }}", "customers": "${{ inputs.customers }}"}, "outputs": ["ok", "rejected"]},
    "invoice": {"kind": "brick", "needs": [{"step": "normalize", "port": "ok", "as": "in"}], "outputs": ["out", "error"], "strategy": {"fan_out": "item"}},
    "archive": {"kind": "script", "needs": [{"step": "normalize", "port": "rejected", "as": "rejected"}, {"step": "invoice", "port": "out", "as": "invoices"}], "outputs": ["out"]}
  }
}`

var keyG = tea.KeyPressMsg{Code: 'g', Text: "g"}

func graphOf(t *testing.T, width, height int) (Model, *installation) {
	t.Helper()
	in := &installation{graph: invoicing}
	in.run = aFailedRun()
	m := opened(t, in, Options{Run: failedRun}, width, height)
	return press(t, m, keyG), in
}

// g opens the graph of the run's workflow, drawn: each step a box with its state and a fan-out's
// shards, the step chosen in a heavier frame, each edge named at its port with the items it
// published, the workflow's inputs and outputs as pills at either edge.
func TestTheGraphIsDrawnAsBoxesAndEdges(t *testing.T) {
	m, in := graphOf(t, 160, 36)
	if m.view != graphView || !slices.Contains(in.asked, "/api/v1/finance/workflows/monthly-invoicing?limit=1") {
		t.Fatalf("g opens view %d, having read %v", m.view, in.asked)
	}
	s := screen(m)
	for _, want := range []string{
		"Graph finance/monthly-invoicing@a3f9c1e  run " + failedRun + "  ● failed",
		"╭────────────────╮", "│ ● normalize    │─ok 3──", "│ succeeded  1s  │┄rejected 0┄",
		"──▸┃ ● invoice      ┃─out───────▸│ ● archive", "┃ ■■■ 3/3        ┃",
		"( customers )──╯", "( orders )──", "▸( errors )", "▸( invoices )",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the graph drawn does not show %q:\n%s", want, s)
		}
	}
	lines := strings.Split(s, "\n")
	if len(lines) != 36 || lastLine(m) != "↑↓ Step   ←→ Along an edge   enter Inspect   g List   esc Run   q Quit   ? Every key" {
		t.Errorf("the graph view's keys are %q", lastLine(m))
	}
	for i, l := range lines {
		if w := len([]rune(l)); w != 160 {
			t.Errorf("line %d is %d columns wide in a window of 160: %q", i, w, l)
		}
	}
}

// A port's edge takes its colour: rejected dashed in the waiting amber, error in the failed red,
// every other one muted.
func TestAnEdgeTakesItsPortsColour(t *testing.T) {
	m, _ := graphOf(t, 160, 36)
	c := m.layout(m.graph).canvas
	roles := map[string]role{}
	for _, row := range c.cells {
		line := ""
		for _, cell := range row {
			line += string(max(cell.text, ' '))
		}
		for _, label := range []string{"rejected 0", "error", "ok 3"} {
			if i := strings.Index(line, label); i >= 0 {
				roles[label] = row[len([]rune(line[:i]))].role
			}
		}
	}
	if roles["rejected 0"] != waitingText || roles["error"] != failedText || roles["ok 3"] != muted {
		t.Errorf("the edges are drawn in %v", roles)
	}
	if s := screen(m); !strings.Contains(s, "┄rejected 0┄") || !strings.Contains(s, "┆") {
		t.Errorf("rejected is not dashed:\n%s", s)
	}
}

// g writes the same graph as a list, in the order it runs, each edge under the step it leaves and
// naming both ends, and g again draws it.
func TestGWritesTheGraphAsAListAndBack(t *testing.T) {
	m, _ := graphOf(t, 160, 36)
	m = press(t, m, keyG)
	s := screen(m)
	for _, want := range []string{
		"inputs     customers, orders  -> normalize",
		"  normalize  succeeded  1s",
		"    ok        -> invoice    in",
		"    rejected  -> archive    rejected",
		"▸ invoice    failed     1m 47s  3 of 3 shards done  fan_out: item",
		"    out      -> archive    invoices",
		"    error    -> output     errors",
		"  archive    skipped",
		"    out      -> output     invoices",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the list does not say %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "╭") || lastLine(m) != "↑↓ Step   ←→ Along an edge   enter Inspect   g Drawing   esc Run   q Quit   ? Every key" {
		t.Errorf("the list is still drawn, or names its keys %q", lastLine(m))
	}
	if s := screen(press(t, m, keyG)); !strings.Contains(s, "╭────") {
		t.Errorf("g again does not draw the graph:\n%s", s)
	}
}

// A drawing wider than its pane pans to keep the step chosen in view, as the keys move it, and is
// cut rather than shrunk.
func TestTheDrawingPansToTheStepChosen(t *testing.T) {
	m, _ := graphOf(t, 80, 24)
	if s := screen(m); !strings.Contains(s, "┃ ● invoice      ┃") {
		t.Errorf("at 80 columns the step chosen is not in view:\n%s", s)
	}
	m = press(t, m, up)
	if s := screen(m); m.step != "normalize" || !strings.Contains(s, "┃ ● normalize    ┃") || !strings.Contains(s, "( customers )") {
		t.Errorf("↑ does not pan to normalize:\n%s", s)
	}
}

// enter opens the step chosen in the inspector, and esc goes back to where the graph was opened
// from: the run, or the runs.
func TestTheGraphOpensAStepAndGoesBack(t *testing.T) {
	m, _ := graphOf(t, 160, 36)
	m = press(t, m, up, enter)
	if m.view != runView || m.step != "normalize" || !strings.Contains(screen(m), "normalize  ● succeeded") {
		t.Errorf("enter on normalize leaves view %d on %q:\n%s", m.view, m.step, screen(m))
	}
	m = press(t, m, keyG, esc)
	if m.view != runView {
		t.Errorf("esc from a graph opened on a run leaves view %d", m.view)
	}
	in := &installation{graph: invoicing, runs: someRuns()}
	in.run = aFailedRun()
	m = press(t, opened(t, in, Options{}, 160, 36), down, keyG)
	if m.view != graphView || !strings.Contains(screen(m), "Graph finance/monthly-invoicing") || lastLine(m) != "↑↓ Step   ←→ Along an edge   enter Inspect   g List   esc Runs   q Quit   ? Every key" {
		t.Fatalf("g on a run selected does not open its graph:\n%s", screen(m))
	}
	if m = press(t, m, esc); m.view != runsView || m.run != nil {
		t.Errorf("esc from a graph opened on the runs leaves view %d", m.view)
	}
}

// A run of a version older than the head is drawn on the head's graph, and says so; a graph that
// cannot be read is said.
func TestAGraphOfAnotherVersionIsSaidSo(t *testing.T) {
	in := &installation{graph: invoicing}
	in.run = aFailedRun()
	in.run.Commit = "5d0b7e2c9a4f1e3d8c6b0a2f4e6d8c0b2a4f6e8d"
	m := press(t, opened(t, in, Options{Run: failedRun}, 160, 36), keyG)
	if s := screen(m); !strings.Contains(s, "The run is of 5d0b7e2, drawn on the graph of the default branch's head, a3f9c1e.") {
		t.Errorf("a run of an older version is not said to be drawn on the head's graph:\n%s", s)
	}
	in = &installation{}
	in.run = aFailedRun()
	m = press(t, opened(t, in, Options{Run: failedRun}, 160, 36), keyG)
	if s := screen(m); !strings.Contains(s, "The graph could not be read: no route /api/v1/finance/workflows/monthly-invoicing?limit=1") {
		t.Errorf("a graph that cannot be read is not said:\n%s", s)
	}
}

// ← and → move along an edge: back to the first step the one chosen needs, forward to the first
// that needs it, and nowhere past either end.
func TestTheArrowsMoveAlongAnEdge(t *testing.T) {
	m, _ := graphOf(t, 160, 36)
	left := tea.KeyPressMsg{Code: tea.KeyLeft}
	right := tea.KeyPressMsg{Code: tea.KeyRight}
	h := tea.KeyPressMsg{Code: 'h', Text: "h"}
	if m.graphStep() != "invoice" {
		t.Fatalf("the graph opens on %q", m.graphStep())
	}
	if m = press(t, m, left); m.graphStep() != "normalize" {
		t.Errorf("← from invoice goes to %q, not the step it needs", m.graphStep())
	}
	if m = press(t, m, h); m.graphStep() != "normalize" {
		t.Errorf("h from the first step goes to %q", m.graphStep())
	}
	if m = press(t, m, right); m.graphStep() != "invoice" {
		t.Errorf("→ from normalize goes to %q, not the first step that needs it", m.graphStep())
	}
	if m = press(t, m, right, right); m.graphStep() != "archive" {
		t.Errorf("→ past the last step goes to %q", m.graphStep())
	}
}
