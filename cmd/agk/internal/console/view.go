package console

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/agentiik/agentiik/db"
)

// The emphases the screen is drawn with until the design system's palette is (#624): bold, faint
// and reverse, which every terminal has and NO_COLOR keeps, and a state is always its word.
var (
	strong   = lipgloss.NewStyle().Bold(true)
	faint    = lipgloss.NewStyle().Faint(true)
	selected = lipgloss.NewStyle().Reverse(true)
)

func (m Model) View() tea.View {
	v := tea.NewView(m.screen())
	v.AltScreen = true
	v.WindowTitle = "agk console"
	return v
}

// screen is the whole window as text: the top line, the view, and the line of its keys.
func (m Model) screen() string {
	if m.width == 0 {
		return ""
	}
	if m.width < leastWidth || m.height < leastHeight {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
			fmt.Sprintf("The window is %d by %d: agk console needs %d by %d.\nMake it larger, or press q to quit.", m.width, m.height, leastWidth, leastHeight))
	}
	body := m.height - 2
	var lines []string
	switch {
	case m.listing:
		lines = m.keysListed()
	case m.view == runView:
		lines = m.runLines()
	default:
		lines = m.runsLines(body)
	}
	for len(lines) < body {
		lines = append(lines, "")
	}
	return strings.Join(append(append([]string{m.topLine()}, lines[:body]...), m.keyLine()), "\n")
}

// topLine names the installation, the namespace shown, the principal, and whether the
// installation answers: one that stops answering is said so here and asked again, since somebody
// is watching.
func (m Model) topLine() string {
	where := m.o.Namespace
	if where == "" {
		where = "every namespace"
	}
	left := strong.Render("agentiik") + "  " + m.o.Installation + "  " + where
	if m.principal != "" {
		left += "  " + m.principal
	}
	right := "live"
	if m.unanswered != "" {
		right = "not answering, asked again: " + m.unanswered
	}
	return fitted(left, right, m.width)
}

// keyLine names the keys of the view by their effect, as the documentation's bottom line does.
func (m Model) keyLine() string {
	var keys [][2]string
	switch {
	case m.listing:
		keys = [][2]string{{"esc", "Close"}, {"q", "Quit"}}
	case m.view == runView:
		keys = [][2]string{{"esc", "Runs"}, {"q", "Quit"}, {"?", "Every key"}}
	default:
		keys = [][2]string{{"↑↓", "Move"}, {"enter", "Open"}, {"q", "Quit"}, {"?", "Every key"}}
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = strong.Render(k[0]) + " " + k[1]
	}
	return truncated(strings.Join(parts, "   "), m.width)
}

// keysListed is every key of the view, which ? opens over it.
func (m Model) keysListed() []string {
	rows := [][2]string{{"q, ctrl+c", "Quit, handing the screen back as it was"}, {"?", "List every key, and close the list"}}
	if m.view == runView {
		rows = append(rows, [2]string{"esc", "Back to the runs"})
	} else {
		rows = append(rows, [2]string{"↑ ↓, k j", "Move the selection over the runs"}, [2]string{"enter", "Open the run selected"})
	}
	lines := []string{strong.Render("Every key of this view"), ""}
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("  %-12s %s", r[0], r[1]))
	}
	return lines
}

// The runs view's columns, as wide as their widest value, the workflow taking what is left:
// timed_out, terraform, and the longest of the durations Took writes.
const (
	stateWidth   = 9
	triggerWidth = 9
	tookWidth    = 7
)

// startedWidth is as wide as the start of the runs listed: a time of day where every one started
// today, which leaves the workflow the room a date would take, and a date with its time otherwise.
func (m Model) startedWidth() int {
	now := m.o.Now()
	width := len("STARTED")
	for _, r := range m.runs {
		at := r.StartedAt
		if at.IsZero() {
			at = r.CreatedAt
		}
		width = max(width, lipgloss.Width(clock(at, now)))
	}
	return width
}

// runsLines is the runs view: the runs that failed lifted into a band above the list, since they
// are why the view is opened, then every run, newest first, the one selected in reverse.
func (m Model) runsLines(height int) []string {
	if !m.read {
		if m.unanswered != "" {
			return []string{"The runs could not be read: " + m.unanswered}
		}
		return []string{faint.Render("Reading the runs.")}
	}
	if len(m.runs) == 0 {
		return []string{faint.Render("No run yet.")}
	}
	now := m.o.Now()
	var lines []string
	var failed []db.ListedRun
	for _, r := range m.runs {
		if failing(r.State) {
			failed = append(failed, r)
		}
	}
	if len(failed) > 0 {
		lines = append(lines, strong.Render(fmt.Sprintf("Failed, %d of the last %d", len(failed), len(m.runs))))
		for _, r := range failed[:min(len(failed), 3)] {
			lines = append(lines, m.row(r, now))
		}
		lines = append(lines, "")
	}
	lines = append(lines, faint.Render(m.header()))
	room := height - len(lines)
	at := 0
	for i, r := range m.runs {
		if string(r.Run) == m.selected {
			at = i
		}
	}
	first := min(max(0, at-room+1), max(0, len(m.runs)-room))
	for i := first; i < len(m.runs) && i < first+room; i++ {
		row := m.row(m.runs[i], now)
		if string(m.runs[i].Run) == m.selected {
			row = selected.Render(padded(row, m.width))
		}
		lines = append(lines, row)
	}
	return lines
}

// wide says whether the window has room for every column: a run's identifier in full, its
// trigger, and who started it at length. Under 120 columns, what an 80-column window holds, the
// list keeps the columns a run is told apart and judged by, and the workflow keeps room to be read.
func (m Model) wide() bool { return m.width >= 120 }

// cell is one column of a row: its text, its width, and whether it is set against its right edge,
// as a duration is so that a column of them lines up.
type cell struct {
	text  string
	width int
	right bool
}

func (m Model) header() string {
	return m.columns("STATE", "RUN", "WORKFLOW", "TRIGGER", "STARTED", "TOOK", "BY")
}

func (m Model) row(r db.ListedRun, now time.Time) string {
	started := r.StartedAt
	if started.IsZero() {
		started = r.CreatedAt
	}
	return m.columns(r.State.String(), string(r.Run), r.Namespace+"/"+r.Workflow, r.Trigger.String(), clock(started, now), lasted(r.RunSummary, now), r.TriggeredBy)
}

// columns lays one row out at the view's widths, cutting what is too long rather than wrapping it,
// since a list read line by line is no longer one where a line runs onto the next. A run's
// identifier is shown in full where there is room, and by its first twelve characters otherwise,
// which still tell two runs of one day apart.
func (m Model) columns(state, run, workflow, trigger, started, took, by string) string {
	cells := []cell{{state, stateWidth, false}, {run, 12, false}, {workflow, 0, false}}
	if m.wide() {
		cells[1].width = 26
		cells = append(cells, cell{trigger, triggerWidth, false})
	}
	byWidth := 10
	if m.wide() {
		byWidth = 14
	}
	cells = append(cells, cell{started, m.startedWidth(), false}, cell{took, tookWidth, true}, cell{by, byWidth, false})
	taken := len(cells) - 1
	for _, c := range cells {
		taken += c.width
	}
	cells[2].width = max(12, m.width-taken)

	parts := make([]string, len(cells))
	for i, c := range cells {
		text := cut(c.text, c.width)
		if c.right {
			parts[i] = strings.Repeat(" ", c.width-lipgloss.Width(text)) + text
		} else {
			parts[i] = text + strings.Repeat(" ", c.width-lipgloss.Width(text))
		}
	}
	return strings.TrimRight(strings.Join(parts, " "), " ")
}

// runLines is the run view: how the run stands, in agk status's words, until the inspector draws
// it (#609).
func (m Model) runLines() []string {
	head := strong.Render("Run " + m.selected)
	switch {
	case m.runFailed != "":
		return []string{head, "", "The run could not be read: " + m.runFailed}
	case !m.runRead:
		return []string{head, "", faint.Render("Reading the run.")}
	}
	var b strings.Builder
	m.o.Describe(&b, *m.run, m.o.Now())
	lines := []string{head, ""}
	for _, l := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		lines = append(lines, truncated(l, m.width))
	}
	return lines
}

// cut shortens a cell to its width, its last character an ellipsis where something was left out.
func cut(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > width {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// truncated is a line no wider than the window.
func truncated(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(s)
}

// padded is a line as wide as the window, so that a row selected is reversed across it.
func padded(s string, width int) string {
	if w := lipgloss.Width(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

// fitted puts left and right on one line as wide as the window, the right cut first where both
// do not fit.
func fitted(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 2 {
		right = cut(right, max(0, width-lipgloss.Width(left)-2))
		gap = max(2, width-lipgloss.Width(left)-lipgloss.Width(right))
	}
	return truncated(left+strings.Repeat(" ", gap)+right, width)
}
