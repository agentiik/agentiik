package console

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// The runs, a run and its log are panes laid out from the window's size, again at every resize, so
// one screen serves a wide monitor and an SSH session opened at its default size. From 160 columns
// the three sit side by side; from 120 the runs sit beside the run, the log beneath the run in the
// lower third of its column; under 120 they are stacked, the runs folded to one line that still
// counts the failed and the running, the pane in focus taking two thirds of the rows. With no run
// open, the runs take the whole window. tab moves the focus to the next pane and a click does too;
// the pane in focus has the accent's border. A border between two panes side by side is dragged
// with the mouse to move it, for the rest of the session.
//
// Each pane is drawn by the view it holds, at the pane's size rather than the window's: the view is
// handed a copy of the console as wide and as high as the inside of the pane, so that a run drawn
// in a column lays itself out as it would in a window that size, and nothing is cut.

// pane is one of the panes of the runs and a run.
type pane int

const (
	runsPane pane = iota
	runPane
	logPane
	// portsPane is the focus on the ports of the step chosen, inside the run's pane: tab moves "in
	// a run, steps, ports, then log", and the run's frame is the one in focus for both.
	portsPane
)

var paneNames = map[pane]string{runsPane: "runs", runPane: "run", logPane: "log"}

// box is where a pane is drawn, and whether it is folded to one line.
type box struct {
	pane       pane
	x, y, w, h int
	folded     bool
}

// split is where a border dragged was left: the runs' width and the log's, nothing where it was not
// dragged.
type split struct{ runs, log int }

// Panes narrower than these are not drawn by a drag: a run cannot be read in fewer columns than its
// steps' names and verdicts take.
const (
	leastPane    = 30
	leastRunPane = 44
)

// panesShown are the panes of the view, in the order tab moves through them.
func (m Model) panesShown() []pane {
	if m.view != runView {
		return []pane{runsPane}
	}
	if m.o.Follow == nil {
		return []pane{runsPane, runPane}
	}
	return []pane{runsPane, runPane, logPane}
}

// widths are the runs' and the log's widths side by side, as dragged or by default: some more than a
// quarter of a window of 160 columns or more each, the run taking the rest, and three tenths for
// the runs under it, never so wide that the run is left under its least.
func (m Model) widths() (runs, log int) {
	shown := m.panesShown()
	if m.width >= 160 && slices.Contains(shown, logPane) {
		runs, log = max(36, m.width*27/100), max(36, m.width*27/100)
	} else {
		runs = max(36, m.width*3/10)
	}
	if m.split.runs > 0 {
		runs = m.split.runs
	}
	if m.split.log > 0 && log > 0 {
		log = m.split.log
	}
	runs = min(max(leastPane, runs), m.width-log-leastRunPane)
	if log > 0 {
		log = min(max(leastPane, log), m.width-runs-leastRunPane)
	}
	return runs, log
}

// laidOut are the panes' boxes in a body of the height given.
func (m Model) laidOut(height int) []box {
	shown := m.panesShown()
	if len(shown) == 1 {
		return []box{{pane: runsPane, w: m.width, h: height}}
	}
	hasLog := slices.Contains(shown, logPane)
	switch {
	case m.width >= 160 && hasLog:
		runs, log := m.widths()
		return []box{{pane: runsPane, w: runs, h: height}, {pane: runPane, x: runs, w: m.width - runs - log, h: height}, {pane: logPane, x: m.width - log, w: log, h: height}}
	case m.width >= 120:
		runs, _ := m.widths()
		if !hasLog {
			return []box{{pane: runsPane, w: runs, h: height}, {pane: runPane, x: runs, w: m.width - runs, h: height}}
		}
		logH := max(6, height/3)
		return []box{{pane: runsPane, w: runs, h: height}, {pane: runPane, x: runs, w: m.width - runs, h: height - logH}, {pane: logPane, x: runs, y: height - logH, w: m.width - runs, h: logH}}
	}
	// Stacked: the pane in focus takes two thirds of the rows; the runs, out of focus, one line.
	if m.focus == runsPane {
		runsH := height * 2 / 3
		return []box{{pane: runsPane, w: m.width, h: runsH}, {pane: runPane, y: runsH, w: m.width, h: height - runsH}}
	}
	rest := height - 1
	boxes := []box{{pane: runsPane, w: m.width, h: 1, folded: true}}
	if !hasLog {
		return append(boxes, box{pane: runPane, y: 1, w: m.width, h: rest})
	}
	focused := rest * 2 / 3
	if m.focus == logPane {
		return append(boxes, box{pane: runPane, y: 1, w: m.width, h: rest - focused}, box{pane: logPane, y: 1 + rest - focused, w: m.width, h: focused})
	}
	return append(boxes, box{pane: runPane, y: 1, w: m.width, h: focused}, box{pane: logPane, y: 1 + focused, w: m.width, h: rest - focused})
}

// panesLines are the runs and the run as panes, each framed and drawn at its own size.
func (m Model) panesLines(t theme, height int) []string {
	boxes := m.laidOut(height)
	drawn := make([][]string, len(boxes))
	for i, b := range boxes {
		drawn[i] = m.paneLines(t, b)
	}
	// The boxes tile the body: each line is the boxes crossing it, left to right.
	lines := make([]string, height)
	for y := range height {
		var b strings.Builder
		for i, bx := range boxes {
			if y >= bx.y && y < bx.y+bx.h {
				b.WriteString(drawn[i][y-bx.y])
			}
		}
		lines[y] = b.String()
	}
	// A border between two panes side by side is where a drag starts, after what each pane draws.
	for i, bx := range boxes {
		if i == 0 || bx.y != 0 || bx.h != height || bx.x == 0 {
			continue
		}
		edge := map[pane]string{runPane: "runs", logPane: "log"}[bx.pane]
		for y := range height {
			t.pickAt(y, bx.x-1, bx.x+1, "border", edge)
		}
	}
	return lines
}

// paneLines is one pane framed: rounded corners, its name in its top border, the accent's border
// when in focus, its inside on the palette's surface.
func (m Model) paneLines(t theme, b box) []string {
	focused := m.focus == b.pane || b.pane == runPane && m.focus == portsPane
	for y := range b.h {
		t.pickAt(b.y+y, b.x, b.x+b.w, "pane", paneNames[b.pane])
	}
	if b.folded {
		return []string{t.line(false, b.w, within(m.foldedRuns(), b.w)...)}
	}
	edge := quiet
	if focused {
		edge = runningText
	}
	inner, rows := b.w-4, b.h-2
	pm := m
	pm.width, pm.height, pm.framed = inner, rows, b.w-inner
	in := t.at(b.x+2, b.y+1)
	in.surface = true
	var content []string
	switch b.pane {
	case runsPane:
		content = pm.runsLines(in, rows)
	case runPane:
		content = pm.runLines(in, rows)
	case logPane:
		if m.run != nil {
			content = pm.logLines(in, stepOf(m.run, m.step), rows)
		}
	}
	title := m.paneTitle(b.pane, focused)
	ts := t
	ts.surface = true
	top := append([]part{{edge, "╭─ "}}, title...)
	top = append(within(top, b.w-2), part{edge, " "})
	if fill := b.w - 1 - widthOf(top); fill > 0 {
		top = append(top, part{edge, strings.Repeat("─", fill)})
	}
	lines := []string{ts.line(false, b.w, append(top, part{edge, "╮"})...)}
	left, right := ts.line(false, 2, part{edge, "│"}, part{plain, " "}), ts.line(false, 2, part{plain, " "}, part{edge, "│"})
	for i := range rows {
		line := in.line(false, inner)
		if i < len(content) {
			line = content[i]
		}
		lines = append(lines, left+line+right)
	}
	return append(lines, ts.line(false, b.w, part{edge, "╰" + strings.Repeat("─", max(0, b.w-2)) + "╯"}))
}

// paneTitle names a pane in its top border, with what it holds.
func (m Model) paneTitle(p pane, focused bool) []part {
	name := strong
	if !focused {
		name = muted
	}
	switch p {
	case runsPane:
		where := m.o.Namespace
		if where == "" {
			where = "every namespace"
		}
		return []part{{name, "Runs"}, {quiet, " " + where + " · " + counting(len(m.runs), "run")}}
	case runPane:
		return []part{{name, "Run"}}
	}
	return []part{{name, "Log"}}
}

// foldedRuns is the runs folded to their line under 120 columns, counting what is why they are
// opened: the failed and the running.
func (m Model) foldedRuns() []part {
	failed, running := 0, 0
	for _, r := range m.runs {
		switch {
		case failing(r.State):
			failed++
		case !r.State.Terminal():
			running++
		}
	}
	parts := []part{{quiet, "▸ "}, {strong, "Runs"}, {muted, "  " + counting(len(m.runs), "run")}}
	if failed > 0 {
		parts = append(parts, part{muted, " · "}, part{failedText, fmt.Sprintf("%d failed", failed)})
	}
	if running > 0 {
		parts = append(parts, part{muted, " · "}, part{runningText, fmt.Sprintf("%d running", running)})
	}
	return append(parts, part{quiet, "   folded · tab"})
}

// focused moves the focus by one, forward or back: the runs, then in a run its steps, its ports
// and its log.
func (m Model) focused(by int) Model {
	var order []pane
	for _, p := range m.panesShown() {
		order = append(order, p)
		if p == runPane {
			order = append(order, portsPane)
		}
	}
	i := slices.Index(order, m.focus)
	m.focus = order[(max(0, i)+by+len(order))%len(order)]
	return m
}

// dragged moves the border a drag holds to the column x.
func (m Model) dragged(x int) Model {
	runs, log := m.widths()
	switch m.dragging {
	case "runs":
		m.split.runs = min(max(leastPane, x+1), m.width-log-leastRunPane)
	case "log":
		m.split.log = min(max(leastPane, m.width-x), m.width-runs-leastRunPane)
	}
	return m
}

// panePress does what a key names in the pane in focus of a run, and reports whether it did.
func (m Model) panePress(key string) (tea.Model, tea.Cmd, bool) {
	switch key {
	case "tab":
		return m.focused(1), nil, true
	case "shift+tab":
		return m.focused(-1), nil, true
	}
	switch m.focus {
	case runsPane:
		switch key {
		case "up", "k":
			m.selected = moved(m.shownRuns(), m.selected, -1)
			return m, nil, true
		case "down", "j":
			m.selected = moved(m.shownRuns(), m.selected, 1)
			return m, nil, true
		case "enter":
			if m.run != nil && m.selected != string(m.run.Run) {
				next, cmd := m.opened(m.selected)
				return next, cmd, true
			}
			m.focus = runPane
			return m, nil, true
		}
	case portsPane:
		switch key {
		case "up", "k":
			next, cmd := m.press("[")
			return next, cmd, true
		case "down", "j":
			next, cmd := m.press("]")
			return next, cmd, true
		}
	case logPane:
		switch key {
		case "up", "k":
			m.logBack, m.logBackOf = min(max(0, len(m.logs[m.logKey()].lines)-1), m.scrolledBack()+1), m.logKey()
			return m, nil, true
		case "down", "j":
			m.logBack, m.logBackOf = max(0, m.scrolledBack()-1), m.logKey()
			return m, nil, true
		}
	}
	return m, nil, false
}

// logKey names the log shown: the run's and its step chosen.
func (m Model) logKey() string {
	if m.run == nil {
		return ""
	}
	return string(m.run.Run) + "/" + stepOf(m.run, m.step)
}

// scrolledBack is how many lines the log shown is scrolled back from its end: none for another
// log than the one scrolled.
func (m Model) scrolledBack() int {
	if m.logBackOf != m.logKey() {
		return 0
	}
	return m.logBack
}

// opened is another run opened in the run's pane, the run before forgotten.
func (m Model) opened(run string) (tea.Model, tea.Cmd) {
	m = m.unfollow().forgetRun()
	m.selected, m.focus = run, runPane
	m.shown++
	return m, m.readShown()
}
