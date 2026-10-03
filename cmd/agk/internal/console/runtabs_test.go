package console

import (
	"strings"
	"testing"
)

// A pane wider than tall, as the eye measures it, draws the graph left to right, and one taller
// than wide top to bottom: each layer a row, each edge leaving a box by its foot and arriving on
// the top of the next with an arrowhead.
func TestTheGraphIsDrawnTopToBottomInATallPane(t *testing.T) {
	m, _ := graphOf(t, 160, 36)
	if d := m.drawingFor(m.graph, 120, 30); d.canvas.w <= d.canvas.h {
		t.Errorf("a pane 120 by 30 draws the graph %d by %d, not left to right", d.canvas.w, d.canvas.h)
	}
	d := m.drawingFor(m.graph, 50, 40)
	if d.canvas.h <= d.canvas.w/2 {
		t.Fatalf("a pane 50 by 40 draws the graph %d by %d, not top to bottom", d.canvas.w, d.canvas.h)
	}
	var rows []string
	for _, l := range m.drawnLines(m.theme(), d, d.canvas.w, d.canvas.h) {
		rows = append(rows, ansi.ReplaceAllString(l, ""))
	}
	at := func(s string) int {
		for i, r := range rows {
			if strings.Contains(r, s) {
				return i
			}
		}
		return -1
	}
	normalize, invoice, archive := at("● normalize"), at("● invoice"), at("● archive")
	if normalize < 0 || !(normalize < invoice && invoice < archive) || at("( orders )") > normalize || at("( invoices )") < archive {
		t.Errorf("the layers are not rows, top to bottom:\n%s", strings.Join(rows, "\n"))
	}
	for _, want := range []string{"│ok 3", "┆rejected 0", "▾", "┏━", "( errors )"} {
		if at(want) < 0 {
			t.Errorf("the graph top to bottom does not draw %q:\n%s", want, strings.Join(rows, "\n"))
		}
	}
}

// g draws the graph in the run's pane where its rows allow it, top to bottom in a pane taller than
// wide, the step chosen beneath it; g again opens it full screen, and esc goes back from it to the
// steps before the runs. Where the rows do not allow it, g opens it full screen at once.
func TestGDrawsTheGraphInTheRunWhereTheRowsAllow(t *testing.T) {
	t.Parallel()
	in := &installation{graph: invoicing, runs: someRuns()}
	in.run = aFailedRun()
	m := opened(t, in, Options{Run: failedRun, Follow: invoiceLogs().follow}, 160, 44)
	defer m.unfollow()
	if s := screen(m); !strings.Contains(s, "Steps   Graph   Ports") || !strings.Contains(s, "STEPS 3") {
		t.Fatalf("a run does not open on its steps:\n%s", s)
	}
	m = press(t, m, keyG)
	s := screen(m)
	if m.view != runView || m.runTabShown() != "graph" || !strings.Contains(s, "▾") || strings.Contains(s, "STEPS 3") || !strings.Contains(lastLine(m), "g Full screen") || !strings.Contains(lastLine(m), "esc Steps") {
		t.Fatalf("g in a run of 160 by 44 shows view %d, its key line %q:\n%s", m.view, lastLine(m), s)
	}
	if lines := strings.Split(s, "\n"); strings.Index(strings.Join(lines, "\n"), "● normalize") > strings.Index(strings.Join(lines, "\n"), "● invoice") {
		t.Errorf("the graph in the run's pane is not drawn top to bottom:\n%s", s)
	}
	if !strings.Contains(s, "invoice  ● failed") {
		t.Errorf("the step chosen is not beneath the graph:\n%s", s)
	}
	if m = press(t, m, esc); m.view != runView || m.runTabShown() != "steps" {
		t.Errorf("esc from the graph in the run leaves view %d on its %s", m.view, m.runTabShown())
	}
	if m = press(t, m, keyG, keyG); m.view != graphView {
		t.Errorf("g on the graph in the run leaves view %d, not the graph full screen", m.view)
	}

	small := &installation{graph: invoicing, runs: someRuns()}
	small.run = aFailedRun()
	if m := press(t, opened(t, small, Options{Run: failedRun}, 80, 24), keyG); m.view != graphView {
		t.Errorf("g in a run of 80 by 24 leaves view %d, not the graph full screen", m.view)
	}
}

// tab to the ports shows them on their own: the step chosen, each port with its items, and the
// envelope of the one chosen.
func TestThePortsAreATabOfTheRun(t *testing.T) {
	in := &installation{runs: someRuns()}
	in.run = aFailedRun()
	m := press(t, opened(t, in, Options{Run: failedRun}, 100, 30), up, tabKey)
	s := screen(m)
	if m.runTabShown() != "ports" || strings.Contains(s, "STEPS 3") || !strings.Contains(s, "PORT         ITEMS") || !strings.Contains(s, "▸ ok") {
		t.Errorf("tab to the ports of normalize shows:\n%s", s)
	}
}
