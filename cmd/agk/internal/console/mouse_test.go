package console

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/agk"
)

// where is the column and line where text is first drawn on the screen.
func where(t *testing.T, m Model, text string) (int, int) {
	t.Helper()
	for y, l := range strings.Split(screen(m), "\n") {
		if i := strings.Index(l, text); i >= 0 {
			return len([]rune(l[:i])), y
		}
	}
	t.Fatalf("%q is nowhere on the screen:\n%s", text, screen(m))
	return 0, 0
}

// click is a left click on where text is drawn.
func click(t *testing.T, m Model, text string) Model {
	t.Helper()
	x, y := where(t, m, text)
	return send(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}).(Model)
}

// inPalette is a left click on a line of the palette, inside its frame.
func inPalette(t *testing.T, m Model, label string) Model {
	t.Helper()
	x, y := where(t, m, "│ "+label)
	return send(t, m, tea.MouseClickMsg{X: x + 2, Y: y, Button: tea.MouseLeft}).(Model)
}

func wheel(t *testing.T, m Model, button tea.MouseButton) Model {
	t.Helper()
	return send(t, m, tea.MouseWheelMsg{X: 10, Y: 10, Button: button}).(Model)
}

// The console asks the terminal for cell motion: a button held, and nothing while none is, which
// leaves shift and drag to the terminal.
func TestTheConsoleAsksForTheMouse(t *testing.T) {
	m := opened(t, &installation{runs: someRuns()}, Options{}, 120, 24)
	if v := m.View(); v.MouseMode != tea.MouseModeCellMotion {
		t.Errorf("the console asks for mouse mode %v", v.MouseMode)
	}
}

// A click on a run selects it, a second click on it opens it, and a click on a tab turns the view.
func TestAClickSelectsADoubleClickOpens(t *testing.T) {
	in := &installation{runs: someRuns()}
	m := opened(t, in, Options{}, 120, 24)
	m = click(t, m, "01RUNCCCCCCCCCCCCCCCCCCCCC")
	if m.selected != "01RUNCCCCCCCCCCCCCCCCCCCCC" || m.view != runsView {
		t.Fatalf("a click leaves %q selected in view %d", m.selected, m.view)
	}
	m = click(t, m, "01RUNCCCCCCCCCCCCCCCCCCCCC")
	if m.view != runView || m.selected != "01RUNCCCCCCCCCCCCCCCCCCCCC" {
		t.Fatalf("a second click leaves view %d on %q", m.view, m.selected)
	}
	m = click(t, m, "2 Workflows")
	if m.view != workflowsView {
		t.Errorf("a click on the tab leaves view %d", m.view)
	}
	m = click(t, m, "1 Runs")
	if m.view != runsView {
		t.Errorf("a click on the runs' tab leaves view %d", m.view)
	}
}

// The wheel moves the selection as the arrows do.
func TestTheWheelMovesTheSelection(t *testing.T) {
	m := opened(t, &installation{runs: someRuns()}, Options{}, 120, 24)
	m = wheel(t, m, tea.MouseWheelDown)
	m = wheel(t, m, tea.MouseWheelDown)
	if m.selected != "01RUNCCCCCCCCCCCCCCCCCCCCC" {
		t.Errorf("two turns of the wheel leave %q selected", m.selected)
	}
	m = wheel(t, m, tea.MouseWheelUp)
	if m.selected != "01RUNBBBBBBBBBBBBBBBBBBBBB" {
		t.Errorf("a turn back leaves %q selected", m.selected)
	}
}

// In a run, a click on a step chooses it and one on a port chooses the port; in the graph, a click
// on a box chooses its step and a second opens it in the inspector.
func TestAClickChoosesAStepAPortAndABox(t *testing.T) {
	in := &installation{graph: invoicing}
	in.run = aFailedRun()
	m := opened(t, in, Options{Run: failedRun}, 160, 40)
	m = click(t, m, "normalize")
	if m.step != "normalize" {
		t.Fatalf("a click on normalize leaves %q chosen", m.step)
	}
	m = click(t, m, "rejected ")
	if m.port != 1 {
		t.Errorf("a click on the port rejected leaves port %d chosen", m.port)
	}
	m = press(t, m, keyG)
	m = click(t, m, "● archive")
	if m.view != graphView || m.step != "archive" {
		t.Fatalf("a click on archive's box leaves %q chosen in view %d", m.step, m.view)
	}
	m = click(t, m, "● archive")
	if m.view != runView || m.step != "archive" {
		t.Errorf("a second click on archive's box leaves view %d on %q", m.view, m.step)
	}
}

// A click on a line of the palette chooses it and a second does it; a click beside it closes it.
func TestAClickChoosesInThePalette(t *testing.T) {
	colon := tea.KeyPressMsg{Code: ':', Text: ":"}
	m := press(t, opened(t, &installation{runs: someRuns()}, Options{}, 120, 30), colon)
	m = inPalette(t, m, "Filter the runs")
	if m.palette == nil {
		t.Fatal("a click on Filter the runs closes the palette")
	}
	if l := m.listed(); l[m.palette.chosen].label != "Filter the runs" {
		t.Fatalf("a click on Filter the runs leaves %q chosen", l[m.palette.chosen].label)
	}
	m = inPalette(t, m, "Filter the runs")
	if m.palette != nil || !m.filtering {
		t.Fatalf("a second click on Filter the runs leaves the palette %v, the filter's line open %t", m.palette, m.filtering)
	}
	m = press(t, m, esc, colon)
	m = send(t, m, tea.MouseClickMsg{X: 1, Y: 28, Button: tea.MouseLeft}).(Model)
	if m.palette != nil || m.view != runsView {
		t.Errorf("a click beside the palette leaves it %v in view %d", m.palette, m.view)
	}
}

// A click on the toast of a run ended opens the run.
func TestAClickOnAToastOpensWhatItNames(t *testing.T) {
	in := &installation{runs: someRuns()}
	m := opened(t, in, Options{}, 120, 24)
	ended := someRuns()
	ended[0].State, ended[0].FinishedAt = agk.Succeeded, now
	in.runs = ended
	m = readAgain(t, m, again{view: runsView, shown: m.shown})
	m = click(t, m, "ended succeeded.")
	if m.view != runView || m.selected != "01RUNAAAAAAAAAAAAAAAAAAAAA" || len(m.toasts) != 0 {
		t.Errorf("a click on the toast leaves view %d on %q, with %d toasts", m.view, m.selected, len(m.toasts))
	}
}

// Neither a click nor the wheel answers a prompt: it is answered with a key.
func TestTheMouseNeverAnswersAPrompt(t *testing.T) {
	in := &installation{me: mayRunIn()}
	d := aFailedRun()
	d.State = agk.Running
	m := press(t, openedOn(t, in, d, 160), keyC)
	m = wheel(t, m, tea.MouseWheelDown)
	m = send(t, m, tea.MouseClickMsg{X: 3, Y: 5, Button: tea.MouseLeft}).(Model)
	if m.asking != askingCancel || len(in.sent) != 0 {
		t.Errorf("the mouse leaves the prompt %d, having sent %v", m.asking, in.sent)
	}
}
