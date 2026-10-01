package console

import (
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The mouse is taken beside the keys, in the terminal's own escape sequences, so that it works over
// SSH as it does locally: a click selects a row, a step, a port, a tab or a line of the palette, a
// second click on the same within half a second opens it as enter does, and the wheel moves the
// selection of the list under the pointer as the arrows do, which scrolls it, or the step chosen in
// the graph, which pans the drawing to it. A click on a toast opens what it names. The console asks
// for cell motion, which a terminal reports while a button is held and not otherwise, and a
// terminal keeps shift and drag to itself, so that a run identifier or a log line is copied as it
// is anywhere else. Every mouse action has a key, and no key needs the mouse.

// pick is what a click lands on: the line of the screen, the columns it spans, and what is there.
type pick struct {
	y, x0, x1 int
	kind, id  string
}

// doubleWithin is how soon a second click on the same thing opens it.
const doubleWithin = 500 * time.Millisecond

// at is the theme drawing lines that sit dx columns right of and dy lines below where its own sit.
func (t theme) at(dx, dy int) theme {
	t.dx, t.dy = t.dx+dx, t.dy+dy
	return t
}

// pick writes in the book that the line y of the lines being drawn, as wide as width, is kind id.
func (t theme) pick(y, width int, kind, id string) { t.pickAt(y, 0, width, kind, id) }

// pickAt writes in the book that columns x0 to x1 of the line y of the lines being drawn are kind
// id. Nothing is written where no click is being placed.
func (t theme) pickAt(y, x0, x1 int, kind, id string) {
	if t.picks != nil {
		*t.picks = append(*t.picks, pick{y: t.dy + y, x0: t.dx + x0, x1: t.dx + x1, kind: kind, id: id})
	}
}

// pickedAt is what a click at x, y lands on: what was drawn there last, an overlay over what is
// beneath it.
func (m Model) pickedAt(x, y int) (pick, bool) {
	var picks []pick
	m.drawn(&picks)
	for i := len(picks) - 1; i >= 0; i-- {
		if p := picks[i]; p.y == y && x >= p.x0 && x < p.x1 {
			return p, true
		}
	}
	return pick{}, false
}

// paneAt is the pane drawn at x, y, where the runs and a run are panes.
func (m Model) paneAt(x, y int) (pane, bool) {
	var picks []pick
	m.drawn(&picks)
	for _, p := range picks {
		if p.kind == "pane" && p.y == y && x >= p.x0 && x < p.x1 {
			for k, name := range paneNames {
				if name == p.id {
					return k, true
				}
			}
		}
	}
	return 0, false
}

// mouse does what a mouse message names, where the screen is drawn and nothing waits on an answer:
// a prompt is answered with a key, and the wheel or a click never answers it for somebody.
func (m Model) mouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.asking != notAsking || m.listing || m.width < leastWidth || m.height < leastHeight {
		return m, nil
	}
	e := msg.Mouse()
	switch msg.(type) {
	case tea.MouseWheelMsg:
		key := ""
		switch e.Button {
		case tea.MouseWheelUp:
			key = "up"
		case tea.MouseWheelDown:
			key = "down"
		default:
			return m, nil
		}
		if m.palette != nil {
			return m.typingCommand(tea.KeyPressMsg{Code: map[string]rune{"up": tea.KeyUp, "down": tea.KeyDown}[key]})
		}
		if under, ok := m.paneAt(e.X, e.Y); ok && m.view == runView {
			// The pane under the pointer scrolls, and the focus stays where it was.
			focus := m.focus
			m.focus = under
			next, cmd := m.press(key)
			n := next.(Model)
			n.focus = focus
			return n, cmd
		}
		return m.press(key)
	case tea.MouseMotionMsg:
		if m.dragging != "" {
			return m.dragged(e.X), nil
		}
		return m, nil
	case tea.MouseReleaseMsg:
		m.dragging = ""
		return m, nil
	case tea.MouseClickMsg:
		if e.Button != tea.MouseLeft {
			return m, nil
		}
		p, ok := m.pickedAt(e.X, e.Y)
		if ok && p.kind == "border" && m.palette == nil {
			// The border follows the pointer until the button is let go.
			m.dragging = p.id
			return m, nil
		}
		now := m.o.Now()
		double := ok && m.clicked.kind == p.kind && m.clicked.id == p.id && now.Sub(m.clickedAt) <= doubleWithin
		m.clicked, m.clickedAt = p, now
		if double {
			// A third click starts again rather than opening twice.
			m.clicked = pick{}
		}
		if !ok {
			if m.palette != nil {
				// A click beside the palette closes it, as esc does.
				m.palette = nil
			}
			return m, nil
		}
		return m.clickedOn(p, double)
	}
	return m, nil
}

// clickedOn selects what a click lands on, and opens it on a second click.
func (m Model) clickedOn(p pick, double bool) (tea.Model, tea.Cmd) {
	switch p.kind {
	case "palette":
		return m, nil
	case "command":
		i, _ := strconv.Atoi(p.id)
		m.palette.chosen = i
		if double {
			return m.typingCommand(tea.KeyPressMsg{Code: tea.KeyEnter})
		}
		return m, nil
	case "toast":
		m = m.gone(p.id)
		if run, ok := strings.CutPrefix(p.id, "run:"); ok {
			m.selected = run
			return m.showing(runView)
		}
		m.palette = &palette{among: Model.noticeCommands, prompt: "notifications: ", none: "No notification."}
		return m, nil
	}
	if m.palette != nil {
		// Beneath the palette, nothing is clicked but the palette closed.
		m.palette = nil
		return m, nil
	}
	if p.kind == "tab" {
		return m.press(p.id)
	}
	if under, ok := m.paneAt(p.x0, p.y); ok && m.view == runView {
		m.focus = under
	}
	var cmd tea.Cmd
	switch p.kind {
	case "pane":
		return m, nil
	case "run":
		m.selected = p.id
	case "step":
		if m.view == runView && m.step != p.id {
			m.step, m.port = p.id, 0
			var follow tea.Cmd
			m, follow = m.followChosen(true)
			cmd = tea.Batch(m.readChosen(), follow)
		}
		m.step = p.id
	case "port":
		m.port, _ = strconv.Atoi(p.id)
		cmd = m.readChosen()
	case "runner":
		m.runner = p.id
	case "flow":
		if m.flow != p.id {
			m.flow = p.id
			cmd = tea.Batch(m.readChosenFlow()...)
		}
	case "grant":
		m.grant = p.id
	}
	if double && p.kind != "port" && !(p.kind == "step" && m.view == runView) {
		next, open := m.press("enter")
		return next, tea.Batch(cmd, open)
	}
	return m, cmd
}

// palettePicks writes where the palette's box is drawn, which a click lands on rather than on the
// view beneath, and each of its lines listed, below the border and the line typed into.
func (t theme) palettePicks(x, y, width, height, first, listed int) {
	for line := y; line < y+height; line++ {
		t.pickAt(line, x, x+width, "palette", "")
	}
	for i := 0; i < listed; i++ {
		t.pickAt(y+2+i, x+2, x+width-2, "command", strconv.Itoa(first+i))
	}
}

// toastPicks writes where a toast's frame is drawn, every line of it.
func (t theme) toastPicks(id string, x, y, width, height int) {
	for line := y; line < y+height; line++ {
		t.pickAt(line, x, x+width, "toast", id)
	}
}
