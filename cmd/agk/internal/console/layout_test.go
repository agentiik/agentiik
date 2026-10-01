package console

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

var (
	tabKey   = tea.KeyPressMsg{Code: tea.KeyTab}
	shiftTab = tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
)

// paned is a console opened on the failed run with its log followed, and the runs read beside it.
func paned(t *testing.T, logs *stepLogs, width, height int) Model {
	t.Helper()
	return followingLogs(t, &installation{runs: someRuns()}, logs, width, height)
}

// everyLineIs fails where a line of the screen is not as wide as the window.
func everyLineIs(t *testing.T, s string, width int) {
	t.Helper()
	for i, l := range strings.Split(s, "\n") {
		if w := len([]rune(l)); w != width {
			t.Errorf("line %d is %d columns wide in a window of %d: %q", i, w, width, l)
		}
	}
}

// From 160 columns the runs, the run and its log sit side by side, each framed and named; from 120
// the log is beneath the run in the lower third of its column; under 120 they are stacked, the runs
// folded to the line that counts the failed and the running.
func TestThePanesAreLaidOutFromTheWindow(t *testing.T) {
	t.Parallel()
	wide := paned(t, invoiceLogs(), 160, 44)
	s := screen(wide)
	top := strings.Split(s, "\n")[1]
	if !strings.HasPrefix(top, "╭─ Runs every namespace · 3 runs") || !strings.Contains(top, "╮╭─ Run ") || !strings.Contains(top, "╮╭─ Log ") {
		t.Errorf("at 160 columns the three panes are not side by side: %q", top)
	}
	everyLineIs(t, s, 160)
	wide.unfollow()

	beside := paned(t, invoiceLogs(), 130, 36)
	s = screen(beside)
	lines := strings.Split(s, "\n")
	if !strings.Contains(lines[1], "╮╭─ Run ") || strings.Contains(lines[1], "Log") {
		t.Errorf("at 130 columns the runs are not beside the run alone: %q", lines[1])
	}
	logAt := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "│╭─ Log ") })
	if logAt < 36*2/3-2 {
		t.Errorf("at 130 columns the log is not beneath the run in the lower third, but on line %d:\n%s", logAt, s)
	}
	everyLineIs(t, s, 130)
	beside.unfollow()

	stacked := paned(t, invoiceLogs(), 100, 30)
	s = screen(stacked)
	lines = strings.Split(s, "\n")
	if !strings.HasPrefix(lines[1], "▸ Runs  3 runs · 1 failed · 1 running   folded · tab") || !strings.HasPrefix(lines[2], "╭─ Run ") {
		t.Errorf("under 120 columns the runs are not folded above the run:\n%s", s)
	}
	run, log := heightOf(stacked, runPane), heightOf(stacked, logPane)
	if run <= log {
		t.Errorf("the run in focus has %d rows and the log %d", run, log)
	}
	stacked = press(t, stacked, tabKey)
	if run, log := heightOf(stacked, runPane), heightOf(stacked, logPane); log <= run {
		t.Errorf("the log in focus has %d rows and the run %d", log, run)
	}
	everyLineIs(t, screen(stacked), 100)
	stacked.unfollow()
}

// heightOf is how many rows a pane takes in the window.
func heightOf(m Model, p pane) int {
	for _, b := range m.laidOut(m.height - 2) {
		if b.pane == p {
			return b.h
		}
	}
	return 0
}

// tab moves the focus through the runs, the run and its log, and shift+tab back; the keys act on
// the pane in focus, and the key line names them.
func TestTabMovesTheFocusAndTheKeysFollowIt(t *testing.T) {
	t.Parallel()
	in := &installation{runs: someRuns()}
	in.run = aFailedRun()
	m := opened(t, in, Options{Run: failedRun}, 160, 40)
	if m.focus != runPane || !strings.HasPrefix(lastLine(m), "tab Next pane   ↑↓ Step   [] Port") {
		t.Fatalf("a run opens with the focus on %v, the key line %q", m.focus, lastLine(m))
	}
	m = press(t, m, shiftTab)
	if m.focus != runsPane || !strings.HasPrefix(lastLine(m), "tab Next pane   ↑↓ Move   enter Open") {
		t.Fatalf("shift+tab leaves the focus on %v, the key line %q", m.focus, lastLine(m))
	}
	// The selection moves in the runs, and the run open stays until another is opened.
	m = press(t, m, down)
	if m.selected != "01RUNCCCCCCCCCCCCCCCCCCCCC" || string(m.run.Run) != failedRun {
		t.Fatalf("↓ in the runs leaves %q selected and %q open", m.selected, m.run.Run)
	}
	reads := len(in.asked)
	in.run.Run = "01RUNCCCCCCCCCCCCCCCCCCCCC"
	m = press(t, m, enter)
	if m.focus != runPane || m.run == nil || string(m.run.Run) != "01RUNCCCCCCCCCCCCCCCCCCCCC" || !slices.Contains(in.asked[reads:], "/api/v1/runs/01RUNCCCCCCCCCCCCCCCCCCCCC") {
		t.Errorf("enter in the runs leaves the focus on %v with %v open, having read %v", m.focus, m.run, in.asked[reads:])
	}
	m = press(t, m, tabKey)
	if m.focus != runsPane {
		t.Errorf("tab with no log goes to %v, not back to the runs", m.focus)
	}
}

// In focus, the log scrolls back with ↑ and forward to its end with ↓, saying how far back it is.
func TestTheLogScrollsBack(t *testing.T) {
	t.Parallel()
	logs := invoiceLogs()
	logs.lines["invoice"] = nil
	for i := 1; i <= 60; i++ {
		logs.lines["invoice"] = append(logs.lines["invoice"], fmt.Sprintf("line %03d", i))
	}
	m := paned(t, logs, 160, 30)
	if s := screen(m); !strings.Contains(s, "line 060") {
		t.Fatalf("the log does not end on its last line:\n%s", s)
	}
	m = press(t, m, tabKey, up, up, up)
	s := screen(m)
	if m.focus != logPane || !strings.Contains(s, "3 lines back") || !strings.Contains(s, "line 057") || strings.Contains(s, "line 058") {
		t.Errorf("↑ three times in the log shows:\n%s", s)
	}
	m = press(t, m, down, down, down, down)
	if s := screen(m); !strings.Contains(s, "line 060") || strings.Contains(s, "lines back") {
		t.Errorf("↓ back to the end shows:\n%s", s)
	}
	m.unfollow()
}

// A border between two panes side by side is dragged to move it, for the rest of the session; a
// click in a pane focuses it, and the wheel over the log scrolls it and leaves the focus where it
// was.
func TestABorderIsDraggedAndAClickFocuses(t *testing.T) {
	t.Parallel()
	logs := invoiceLogs()
	for i := 1; i <= 60; i++ {
		logs.lines["invoice"] = append(logs.lines["invoice"], fmt.Sprintf("line %03d", i))
	}
	m := paned(t, logs, 160, 40)
	runs, _ := m.widths()
	var tm tea.Model = m
	tm = send(t, tm, tea.MouseClickMsg{X: runs - 1, Y: 10, Button: tea.MouseLeft})
	tm = send(t, tm, tea.MouseMotionMsg{X: 59, Y: 10, Button: tea.MouseLeft})
	tm = send(t, tm, tea.MouseReleaseMsg{X: 59, Y: 10, Button: tea.MouseLeft})
	m = tm.(Model)
	if got, _ := m.widths(); got != 60 || m.dragging != "" {
		t.Fatalf("the runs' border dragged to column 59 leaves them %d wide, dragging %q", got, m.dragging)
	}
	if top := strings.Split(screen(m), "\n")[1]; !strings.HasPrefix(top[strings.Index(top, "╮╭─ Run"):], "╮╭─ Run") || len([]rune(top[:strings.Index(top, "╮╭─ Run")])) != 59 {
		t.Errorf("the runs are not drawn 60 columns wide: %q", top)
	}
	m = send(t, m, tea.MouseClickMsg{X: 158, Y: 20, Button: tea.MouseLeft}).(Model)
	if m.focus != logPane {
		t.Errorf("a click in the log leaves the focus on %v", m.focus)
	}
	m = send(t, m, tea.MouseClickMsg{X: 80, Y: 30, Button: tea.MouseLeft}).(Model)
	m = send(t, m, tea.MouseWheelMsg{X: 158, Y: 20, Button: tea.MouseWheelUp}).(Model)
	if m.focus != runPane || m.scrolledBack() != 1 {
		t.Errorf("the wheel over the log leaves the focus on %v and the log %d lines back", m.focus, m.scrolledBack())
	}
	m.unfollow()
}
