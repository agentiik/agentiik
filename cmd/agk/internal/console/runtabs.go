package console

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
)

// A run's pane has three tabs: its steps listed, its graph drawn where the pane's rows allow it, and
// the ports of the step chosen. The steps are what a run opens on; g draws the graph in the pane
// where it fits with the step chosen beneath it, top to bottom in a pane taller than wide, and
// full screen where it does not or once it is drawn there; esc goes back from the graph to the
// steps before it goes back to the runs. The ports are the tab while they have the focus, which
// tab moves to them after the steps.

// runTabShown is the tab the run's pane shows.
func (m Model) runTabShown() string {
	if m.focus == portsPane {
		return "ports"
	}
	if m.runTab == "graph" {
		return "graph"
	}
	return "steps"
}

// runTabs is the line naming the tabs, the one shown in bold, each where a click chooses it.
func (m Model) runTabs(t theme, y int) string {
	var parts []part
	x := 0
	for i, tab := range [][2]string{{"steps", "Steps"}, {"graph", "Graph"}, {"ports", "Ports"}} {
		if i > 0 {
			parts = append(parts, part{plain, "   "})
			x += 3
		}
		r := muted
		if tab[0] == m.runTabShown() {
			r = strong
		}
		parts = append(parts, part{r, tab[1]})
		t.pickAt(y, x, x+len(tab[1]), "runtab", tab[0])
		x += len(tab[1])
	}
	return t.line(false, m.width, within(parts, m.width)...)
}

// detailRows is how many lines of the step chosen the graph leaves beneath it, at the least.
const detailRows = 8

// graphTab is the run's graph drawn in its pane, the run's state laid over it and the step chosen
// in its heavier frame, then the step chosen.
func (m Model) graphTab(t theme, lines []string, step string, height int, now time.Time) []string {
	if m.graph == nil || m.graphFor != workflowKey(m.run) {
		said := "Reading the graph."
		if m.graphFailed != "" {
			said = "The graph could not be read: " + m.graphFailed
		}
		return append(lines, t.line(false, m.width, part{quiet, said}))
	}
	if m.run.Commit != "" && m.graph.Commit != "" && m.run.Commit != m.graph.Commit {
		lines = append(lines, t.line(false, m.width, within([]part{{waitingText, "The run is of " + short(m.run.Commit) + ", drawn on the graph of the default branch's head, " + short(m.graph.Commit) + "."}}, m.width)...))
	}
	described, ports, _ := m.stepLines(step, now, m.width)
	room := max(6, height-len(lines)-1-min(len(described), detailRows))
	d := m.drawingIn(m.graph, m.width, m.height, m.width, room)
	lines = append(lines, m.drawnLines(t.at(0, len(lines)), d, m.width, min(d.canvas.h, room))...)
	lines = append(lines, t.line(false, m.width))
	of := t.at(0, len(lines))
	for i, l := range described {
		if p, ok := ports[i]; ok {
			of.pick(i, m.width, "port", fmt.Sprint(p))
		}
		lines = append(lines, t.line(false, m.width, within(l, m.width)...))
	}
	return lines
}

// portsTab is the step chosen and its ports, each with its items, and the envelope of the one
// chosen where the principal may read it.
func (m Model) portsTab(t theme, lines []string, step string, now time.Time) []string {
	described, ports, from := m.stepLines(step, now, m.width)
	lines = append(lines, t.line(false, m.width, within(described[0], m.width)...), t.line(false, m.width))
	if from < 0 {
		return append(lines, t.line(false, m.width, part{quiet, "No port of " + step + " has published yet."}))
	}
	// From the ports' header on, each line at the place the step's own lines give it.
	of := t.at(0, len(lines)-from)
	for i, l := range described[from:] {
		if p, ok := ports[from+i]; ok {
			of.pick(from+i, m.width, "port", fmt.Sprint(p))
		}
		lines = append(lines, t.line(false, m.width, within(l, m.width)...))
	}
	return lines
}

// graphRows is the least room the graph is drawn in within a run's pane: two layers of boxes. A
// graph larger than its room pans to keep the step chosen in view, as it does full screen, and
// never shrinks.
const graphRows = 12

// graphFits says whether the run's pane has the rows for its graph and the step chosen beneath
// it: its header, its tabs, a dozen rows of graph and the step chosen's first lines.
func (m Model) graphFits() bool {
	for _, b := range m.laidOut(m.height - 2) {
		if b.pane == runPane && !b.folded {
			return b.h-2-6-detailRows >= graphRows
		}
	}
	return false
}

// drawGraph is g in a run: its graph drawn in the pane where it fits and is not yet, and full
// screen otherwise.
func (m Model) drawGraph() (tea.Model, tea.Cmd) {
	if m.run == nil {
		return m, nil
	}
	if m.runTab != "graph" && m.graphFits() {
		m.runTab = "graph"
		if m.focus == portsPane {
			m.focus = runPane
		}
		if m.graphFor != workflowKey(m.run) {
			m.graphFor, m.graph, m.graphFailed = workflowKey(m.run), nil, ""
			return m, m.readWorkflow()
		}
		return m, nil
	}
	m.graphFrom = runView
	return m.showing(graphView)
}
