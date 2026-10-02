package console

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

func (m Model) View() tea.View {
	v := tea.NewView(m.screen())
	v.AltScreen = true
	v.WindowTitle = "agk console"
	return v
}

// screen is the whole window as text: the top line, the view, and the line of its keys, every
// line as wide as the window so that the ground is painted under all of it.
func (m Model) screen() string {
	if m.width == 0 {
		return ""
	}
	t := m.theme()
	if m.width < leastWidth || m.height < leastHeight {
		asked := lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
			fmt.Sprintf("The window is %d by %d: agk console needs %d by %d.\nMake it larger, or press q to quit.", m.width, m.height, leastWidth, leastHeight))
		lines := strings.Split(asked, "\n")
		for i, l := range lines {
			lines[i] = t.line(false, m.width, part{plain, l})
		}
		return strings.Join(lines, "\n")
	}
	body := m.height - 2
	var lines []string
	switch {
	case m.listing:
		lines = m.keysListed(t)
	case m.view == runView:
		lines = m.runLines(t, body)
	case m.view == runnersView:
		lines = m.runnersLines(t, body)
	case m.view == graphView:
		lines = m.graphLines(t, body)
	default:
		lines = m.runsLines(t, body)
	}
	for len(lines) < body {
		lines = append(lines, t.line(false, m.width))
	}
	if m.asking != notAsking {
		// The prompt sits above the keys that answer it, over the last line of the view.
		lines[body-1] = t.line(false, m.width, within([]part{{strong, m.question()}}, m.width)...)
	}
	return m.withPalette(t, m.withToasts(t, strings.Join(append(append([]string{m.topLine(t)}, lines[:body]...), m.keyLine(t)), "\n")))
}

// theme is how the screen is drawn now: the terminal's depth, on the ground its background asked
// for once it has answered.
func (m Model) theme() theme { return theme{depth: m.depth, light: m.light} }

// topLine names the installation, the namespace shown, the principal, the views a digit turns
// to, and whether the installation answers: a green dot and the word while it does, and in words,
// asked again, once it stops, since somebody is watching. Where the window is too narrow for all
// of it, the installation is left out first, since it is the one thing on the line that does not
// change while the console is open.
func (m Model) topLine(t theme) string {
	where := m.o.Namespace
	if where == "" {
		where = "every namespace"
	}
	right := []part{{succeededText, "●"}, {plain, " live"}}
	if n := len(m.me.Notifications); n > 0 {
		right = append([]part{{muted, counting(n, "notification") + "  "}}, right...)
	}
	if m.unanswered != "" {
		right = []part{{failedText, "not answering, asked again: " + m.unanswered}}
	}
	var left []part
	for _, installation := range []string{m.o.Installation, ""} {
		rest := "  " + where
		if installation != "" {
			rest = "  " + installation + rest
		}
		if m.me.Principal != "" {
			rest += "  " + m.me.Principal
		}
		left = append([]part{{strong, "agentiik"}, {muted, rest}}, m.tabs()...)
		if widthOf(left)+2+widthOf(right) <= m.width {
			break
		}
	}
	return t.line(false, m.width, fitted(left, right, m.width)...)
}

// tab is a view a digit turns to.
type tab struct {
	key, name string
	view      view
}

// tabs are the views a digit turns to, the one shown in bold: the runs, and the runners to an
// administrator. Workflows and sharing take 2 and 3 once the console draws them.
func (m Model) tabs() []part {
	tabs := []tab{{"1", "Runs", runsView}}
	if m.me.Admin {
		tabs = append(tabs, tab{"4", "Runners", runnersView})
	}
	var parts []part
	for _, v := range tabs {
		r := muted
		if m.view == v.view || v.view == runsView && m.view == runView {
			r = strong
		}
		parts = append(parts, part{plain, "   "}, part{r, v.key + " " + v.name})
	}
	return parts
}

// keyLine names the keys of the view by their effect, as the documentation's bottom line does.
func (m Model) keyLine(t theme) string {
	var keys [][2]string
	switch {
	case m.palette != nil:
		keys = paletteKeys()
	case m.asking == askingCancel:
		keys = [][2]string{{"y", "Cancel run"}, {"any other key", "Keep it"}}
	case m.asking == askingReplay:
		keys = [][2]string{{"y", "Replay from " + stepOf(m.run, m.step)}, {"any other key", "Keep it"}}
	case m.listing:
		keys = [][2]string{{"esc", "Close"}, {"q", "Quit"}}
	case m.filtering && m.view == runsView:
		keys = [][2]string{{"type", "Filter"}, {"↑↓", "Move"}, {"enter", "Keep"}, {"esc", "Clear"}}
	case m.view == runView:
		keys = [][2]string{{"↑↓", "Step"}, {"[]", "Port"}, {"g", "Graph"}}
		if m.mayCancel() {
			keys = append(keys, [2]string{"c", "Cancel run"})
		}
		if m.mayReplay() {
			keys = append(keys, [2]string{"p", "Replay from step"})
		}
		keys = append(keys, [2]string{"esc", "Runs"}, [2]string{"q", "Quit"}, [2]string{"?", "Every key"})
	case m.view == runnersView:
		keys = [][2]string{{"↑↓", "Move"}, {"esc", "Runs"}, {"q", "Quit"}, {"?", "Every key"}}
	case m.view == graphView:
		written := "List"
		if m.asList {
			written = "Drawing"
		}
		back := "Run"
		if m.graphFrom == runsView {
			back = "Runs"
		}
		keys = [][2]string{{"↑↓", "Step"}, {"enter", "Inspect"}, {"g", written}, {"esc", back}, {"q", "Quit"}, {"?", "Every key"}}
	default:
		keys = [][2]string{{"↑↓", "Move"}, {"enter", "Open"}, {"g", "Graph"}, {"/", "Filter"}}
		if m.filter != "" {
			keys = append(keys, [2]string{"esc", "Clear"})
		}
		keys = append(keys, [2]string{"q", "Quit"}, [2]string{"?", "Every key"})
	}
	var parts []part
	for i, k := range keys {
		if i > 0 {
			parts = append(parts, part{plain, "   "})
		}
		parts = append(parts, part{strong, k[0]}, part{muted, " " + k[1]})
	}
	return t.line(false, m.width, within(parts, m.width)...)
}

// keysListed is every key of the view, which ? opens over it.
func (m Model) keysListed(t theme) []string {
	rows := [][2]string{{"q, ctrl+c", "Quit, handing the screen back as it was"}, {"?", "List every key, and close the list"}, {":", "Open the command palette: commands, views, runs, workflows and namespaces"}, {"1", "The runs"}}
	if m.me.Admin {
		rows = append(rows, [2]string{"4", "The runners and their pools"})
	}
	switch m.view {
	case runView:
		rows = append(rows, [2]string{"↑ ↓, k j", "Move between the steps"}, [2]string{"[ ]", "The previous or next port of the step"}, [2]string{"g", "The graph of the run's workflow, its state laid over it"}, [2]string{"esc", "Back to the runs"})
		if m.mayCancel() {
			rows = append(rows, [2]string{"c", "Cancel the run, once a prompt naming it is answered y"})
		}
		if m.mayReplay() {
			rows = append(rows, [2]string{"p", "Replay the run from the step chosen, once a prompt naming both is answered y"})
		}
	case runnersView:
		rows = append(rows, [2]string{"↑ ↓, k j", "Move the selection over the runners"}, [2]string{"esc", "Back to the runs"})
	case graphView:
		rows = append(rows, [2]string{"↑ ↓, k j", "Move between the steps, in the order the graph runs them"}, [2]string{"enter", "Open the step chosen in the inspector"},
			[2]string{"g", "Write the graph as a list, and draw it again"}, [2]string{"esc", "Back to the view the graph was opened from"})
	default:
		rows = append(rows, [2]string{"↑ ↓, k j", "Move the selection over the runs"}, [2]string{"enter", "Open the run selected"}, [2]string{"g", "The graph of the run selected"},
			[2]string{"/", "Filter the runs as you type: words against what is loaded, and namespace=, workflow=, state=, since= and until= asked of the installation"},
			[2]string{"esc", "Clear the filter"})
	}
	lines := []string{t.line(false, m.width, part{strong, "Every key of this view"}), t.line(false, m.width)}
	for _, r := range rows {
		lines = append(lines, t.line(false, m.width, part{plain, "  "}, part{strong, fmt.Sprintf("%-12s", r[0])}, part{plain, " " + r[1]}))
	}
	return lines
}

// The runs view's columns, as wide as their widest value, the workflow taking what is left: a dot
// and timed_out, terraform, and the longest of the durations Took writes.
const (
	stateWidth   = 11
	triggerWidth = 9
	tookWidth    = 7
)

// startedWidth is as wide as the start of the runs listed: a time of day where every one started
// today, which leaves the workflow the room a date would take, and a date with its time otherwise.
func (m Model) startedWidth() int {
	now := m.o.Now()
	width := len("STARTED")
	for _, r := range m.runs {
		width = max(width, lipgloss.Width(clock(startOf(r), now)))
	}
	return width
}

// startOf is when a run started, or was created where it has not started.
func startOf(r db.ListedRun) time.Time {
	if r.StartedAt.IsZero() {
		return r.CreatedAt
	}
	return r.StartedAt
}

// runsLines is the runs view: the runs that failed lifted into a band above the list, since they
// are why the view is opened, then every run, newest first, the one selected drawn as the
// selection.
func (m Model) runsLines(t theme, height int) []string {
	var lines []string
	if m.filtering || m.filter != "" {
		lines = append(lines, m.filterLine(t))
	}
	if !m.read {
		if m.unanswered != "" {
			return append(lines, t.line(false, m.width, part{failedText, "The runs could not be read: " + m.unanswered}))
		}
		return append(lines, t.line(false, m.width, part{quiet, "Reading the runs."}))
	}
	runs := m.shownRuns()
	if len(runs) == 0 {
		said := "No run yet."
		if m.filter != "" {
			said = "No run matches the filter."
		}
		return append(lines, t.line(false, m.width, part{quiet, said}))
	}
	now := m.o.Now()
	var failed []db.ListedRun
	for _, r := range runs {
		if failing(r.State) {
			failed = append(failed, r)
		}
	}
	// A filter is what was asked for, and the band would take the room of what it found.
	if len(failed) > 0 && m.filter == "" {
		lines = append(lines, t.line(false, m.width, part{strong, fmt.Sprintf("Failed, %d of the last %d", len(failed), len(runs))}))
		for _, r := range failed[:min(len(failed), 3)] {
			lines = append(lines, t.line(false, m.width, m.row(r, now)...))
		}
		lines = append(lines, t.line(false, m.width))
	}
	lines = append(lines, t.line(false, m.width, m.header()...))
	room := height - len(lines)
	at := 0
	for i, r := range runs {
		if string(r.Run) == m.selected {
			at = i
		}
	}
	first := min(max(0, at-room+1), max(0, len(runs)-room))
	for i := first; i < len(runs) && i < first+room; i++ {
		lines = append(lines, t.line(string(runs[i].Run) == m.selected, m.width, m.row(runs[i], now)...))
	}
	return lines
}

// wide says whether the window has room for every column: a run's identifier in full, its
// trigger, and who started it at length. Under 120 columns, what an 80-column window holds, the
// list keeps the columns a run is told apart and judged by, and the workflow keeps room to be read.
func (m Model) wide() bool { return m.width >= 120 }

// cell is one column of a row: what it holds, its width, and whether it is set against its right
// edge, as a duration is so that a column of them lines up.
type cell struct {
	parts []part
	width int
	right bool
}

func (m Model) header() []part {
	h := func(s string) []part { return []part{{quiet, s}} }
	return m.columns(h("STATE"), h("RUN"), h("WORKFLOW"), h("TRIGGER"), h("STARTED"), h("TOOK"), h("BY"), h("STEPS"), h("LAST 20"))
}

// row is one run as the list draws it: its state a word beside a dot in the state's colour, so
// that the state is never its colour alone.
func (m Model) row(r db.ListedRun, now time.Time) []part {
	p := func(s string) []part { return []part{{plain, s}} }
	return m.columns(
		[]part{{stateRole(r.State), m.mark(r.State == agk.Running)}, {plain, " " + r.State.String()}},
		[]part{{muted, string(r.Run)}},
		p(r.Namespace+"/"+r.Workflow), p(r.Trigger.String()), p(clock(startOf(r), now)), p(lasted(r.RunSummary, now)), p(r.TriggeredBy),
		stepStrip(r), m.sparkOf(r, now))
}

// sparkOf is the bars of a listed run's workflow's last runs, once they are read.
func (m Model) sparkOf(r db.ListedRun, now time.Time) []part {
	drawn, _ := bars(m.sparks[r.Namespace+"/"+r.Workflow].runs, now)
	return drawn
}

// columns lays one row out at the view's widths, cutting what is too long rather than wrapping it,
// since a list read line by line is no longer one where a line runs onto the next. A run's
// identifier is shown in full where there is room, and by its first twelve characters otherwise,
// which still tell two runs of one day apart.
func (m Model) columns(state, run, workflow, trigger, started, took, by, steps, last []part) []part {
	cells := []cell{{state, stateWidth, false}, {run, 12, false}, {workflow, 0, false}}
	byWidth := 10
	if m.wide() {
		cells[1].width = 26
		cells = append(cells, cell{trigger, triggerWidth, false})
		byWidth = 14
	}
	cells = append(cells, cell{started, m.startedWidth(), false}, cell{took, tookWidth, true}, cell{by, byWidth, false})
	if m.full() {
		// At full width, a strip of the run's steps and its workflow's last twenty runs.
		cells = append(cells, cell{steps, 12, false}, cell{last, 20, false})
	}
	return laid(cells, 2, m.width)
}

// laid lays cells out on one line as wide as width, a space between two, the cell at grow taking
// what the others leave and never under twelve columns.
func laid(cells []cell, grow, width int) []part {
	taken := len(cells) - 1
	for i, c := range cells {
		if i != grow {
			taken += c.width
		}
	}
	cells[grow].width = max(12, width-taken)

	var parts []part
	for i, c := range cells {
		if i > 0 {
			parts = append(parts, part{plain, " "})
		}
		fit := within(c.parts, c.width)
		gap := part{plain, strings.Repeat(" ", c.width-widthOf(fit))}
		if c.right {
			parts = append(append(parts, gap), fit...)
		} else {
			parts = append(append(parts, fit...), gap)
		}
	}
	return parts
}

// widthOf is how many columns parts take.
func widthOf(parts []part) int {
	w := 0
	for _, p := range parts {
		w += lipgloss.Width(p.text)
	}
	return w
}

// within is parts cut to width, the last character an ellipsis where something was left out.
func within(parts []part, width int) []part {
	if widthOf(parts) <= width {
		return parts
	}
	if width <= 0 {
		return nil
	}
	var out []part
	used := 0
	for _, p := range parts {
		w := lipgloss.Width(p.text)
		if used+w <= width-1 {
			out = append(out, p)
			used += w
			continue
		}
		r := []rune(p.text)
		for len(r) > 0 && used+lipgloss.Width(string(r)) > width-1 {
			r = r[:len(r)-1]
		}
		return append(out, part{p.role, string(r) + "…"})
	}
	return out
}

// fitted puts left and right on one line as wide as the window, the right cut first where both
// do not fit, then the left.
func fitted(left, right []part, width int) []part {
	if room := width - widthOf(left) - 2; widthOf(right) > room {
		right = within(right, max(0, room))
	}
	gap := max(2, width-widthOf(left)-widthOf(right))
	return within(append(append(left, part{plain, strings.Repeat(" ", gap)}), right...), width)
}
