package console

import (
	"net/url"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// : opens the command palette over whatever is on screen: every command of the view and every view,
// and every run, workflow and namespace the principal reads, by name or by the start of an
// identifier, matched as the filter matches. It opens on the five commands used last, and shows
// each command's key beside it, so that a command found once is a keystroke the next time. A
// command for what the principal does not hold is left out, as it is of the key line. Its
// Notifications lists every notification GET /api/v1/me holds, and dismisses one.

// command is one line of the palette: what it is, the key that does it where one does, and what
// choosing it does.
type command struct {
	label, key, kind string
	act              func(Model) (tea.Model, tea.Cmd)
}

// palette is the palette open, and whether it lists the notifications rather than the commands.
type palette struct {
	query   string
	chosen  int
	notices bool
}

// recentlyUsed is how many commands the palette opens on.
const recentlyUsed = 5

// pressing is a command that does what a key does in the view.
func pressing(label, key string) command {
	return command{label: label, key: key, kind: "command", act: func(m Model) (tea.Model, tea.Cmd) { return m.press(key) }}
}

// commands are every command of the view and every view, then everything the principal reads by
// name: workflows, namespaces and runs.
func (m Model) commands() []command {
	var out []command
	switch m.view {
	case runsView:
		if m.selected != "" {
			out = append(out, pressing("Open the run selected", "enter"), pressing("Graph of the run selected", "g"))
		}
		out = append(out, pressing("Filter the runs", "/"))
		if m.filter != "" {
			out = append(out, pressing("Clear the filter", "esc"))
		}
	case runView:
		out = append(out, pressing("Graph of the run", "g"), pressing("Next port", "]"), pressing("Previous port", "["))
		if m.mayCancel() {
			out = append(out, pressing("Cancel run", "c"))
		}
		if m.mayReplay() {
			out = append(out, pressing("Replay from "+stepOf(m.run, m.step), "p"))
		}
		out = append(out, pressing("Back to the runs", "esc"))
	case graphView:
		written := "Write the graph as a list"
		if m.asList {
			written = "Draw the graph"
		}
		out = append(out, pressing(written, "g"), pressing("Inspect the step chosen", "enter"), pressing("Back", "esc"))
	case runnersView:
		out = append(out, pressing("Back to the runs", "esc"))
	case workflowsView:
		out = append(out, pressing("Graph of the workflow chosen", "enter"), pressing("Back to the runs", "esc"))
	}
	out = append(out, command{label: "Runs", key: "1", kind: "view", act: func(m Model) (tea.Model, tea.Cmd) { return m.press("1") }},
		command{label: "Workflows", key: "2", kind: "view", act: func(m Model) (tea.Model, tea.Cmd) { return m.press("2") }})
	if m.me.Admin {
		out = append(out, command{label: "Runners", key: "4", kind: "view", act: func(m Model) (tea.Model, tea.Cmd) { return m.press("4") }})
	}
	out = append(out, command{label: "Notifications", kind: "view", act: func(m Model) (tea.Model, tea.Cmd) {
		m.palette = &palette{notices: true}
		return m, nil
	}})
	out = append(out, pressing("Every key", "?"), pressing("Quit", "q"))

	// The broadest first where two match as closely: a workflow before the runs of it.
	workflows := map[string]bool{}
	for _, r := range m.runs {
		workflows[r.Namespace+"/"+r.Workflow] = true
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for w := range workflows {
			if !yield(w) {
				return
			}
		}
	})
	for _, w := range names {
		ns, wf, _ := strings.Cut(w, "/")
		out = append(out, command{label: w, kind: "workflow", act: func(m Model) (tea.Model, tea.Cmd) {
			return m.narrowed("namespace=" + ns + " workflow=" + wf)
		}})
	}
	var namespaces []string
	for scope := range m.me.Permissions {
		if !strings.Contains(scope, "/") {
			namespaces = append(namespaces, scope)
		}
	}
	slices.Sort(namespaces)
	for _, ns := range namespaces {
		out = append(out, command{label: ns, kind: "namespace", act: func(m Model) (tea.Model, tea.Cmd) {
			return m.narrowed("namespace=" + ns)
		}})
	}
	for _, r := range m.runs {
		run := string(r.Run)
		out = append(out, command{label: run + "  " + r.Namespace + "/" + r.Workflow + "  " + r.State.String(), kind: "run", act: func(m Model) (tea.Model, tea.Cmd) {
			m.selected = run
			return m.showing(runView)
		}})
	}
	return out
}

// narrowed is the runs view filtered by terms the installation is asked, as typed after /.
func (m Model) narrowed(filter string) (tea.Model, tea.Cmd) {
	m.filter = filter
	if m.view == runsView {
		m.terms = filter
		m.shown++
		return m, m.readShown()
	}
	return m.showing(runsView)
}

// noticeCommands are the notifications, each dismissed when chosen.
func (m Model) noticeCommands() []command {
	var out []command
	for _, n := range m.me.Notifications {
		id := n.ID
		out = append(out, command{label: n.sentence(), kind: "notification", act: func(m Model) (tea.Model, tea.Cmd) {
			if m.o.Send == nil {
				return m, nil
			}
			send, ctx := m.o.Send, m.ctx
			return m, func() tea.Msg {
				if err := send(ctx, "DELETE", "/api/v1/me/notifications/"+url.PathEscape(id), nil, nil); err != nil {
					return meRead{err: err}
				}
				return meAgain{}
			}
		}})
	}
	return out
}

// listed are the palette's lines for what is typed: the closest first, as the filter orders runs;
// with nothing typed, the commands used last, then the rest of the view's.
func (m Model) listed() []command {
	all := m.commands()
	if m.palette.notices {
		all = m.noticeCommands()
	}
	words := strings.Fields(m.palette.query)
	if len(words) == 0 {
		if m.palette.notices {
			return all
		}
		var first []command
		for _, label := range m.used {
			if i := slices.IndexFunc(all, func(c command) bool { return c.label == label }); i >= 0 {
				first = append(first, all[i])
			}
		}
		for _, c := range all {
			if len(first) >= recentlyUsed {
				break
			}
			if c.kind == "command" && !slices.ContainsFunc(first, func(f command) bool { return f.label == c.label }) {
				first = append(first, c)
			}
		}
		return first
	}
	type scored struct {
		c     command
		score int
	}
	var kept []scored
	for _, c := range all {
		total := 0
		for _, w := range words {
			s := matched(w, c.label)
			if s < 0 {
				total = -1
				break
			}
			total += s
		}
		if total >= 0 {
			kept = append(kept, scored{c, total})
		}
	}
	slices.SortStableFunc(kept, func(a, b scored) int { return b.score - a.score })
	out := make([]command, len(kept))
	for i, k := range kept {
		out[i] = k.c
	}
	return out
}

// typingCommand takes a key while the palette is open: a character narrows it, ↑ ↓ move the
// choice, enter does what is chosen, esc closes it.
func (m Model) typingCommand(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	p := *m.palette
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.palette = nil
		return m, nil
	case "up":
		p.chosen = max(0, p.chosen-1)
	case "down":
		p.chosen = min(max(0, len(m.listed())-1), p.chosen+1)
	case "backspace":
		if r := []rune(p.query); len(r) > 0 {
			p.query, p.chosen = string(r[:len(r)-1]), 0
		}
	case "enter":
		listed := m.listed()
		if len(listed) == 0 {
			return m, nil
		}
		c := listed[min(p.chosen, len(listed)-1)]
		m.palette = nil
		if c.kind == "command" || c.kind == "view" {
			m.used = append([]string{c.label}, slices.DeleteFunc(m.used, func(l string) bool { return l == c.label })...)
			m.used = m.used[:min(len(m.used), recentlyUsed)]
		}
		return c.act(m)
	default:
		if msg.Text == "" {
			return m, nil
		}
		p.query, p.chosen = p.query+msg.Text, 0
	}
	m.palette = &p
	return m, nil
}

// withPalette lays the palette over the screen, below the top line: what is typed, then as many of
// the commands it leaves as the window holds, each with its key.
func (m Model) withPalette(t theme, screen string) string {
	if m.palette == nil {
		return screen
	}
	width := min(m.width-4, 80)
	inner := width - 4
	listed := m.listed()
	room := max(1, min(len(listed), m.height-8))
	first := min(max(0, m.palette.chosen-room+1), max(0, len(listed)-room))
	prompt := ": "
	if m.palette.notices {
		prompt = "notifications: "
	}
	var rows []string
	rows = append(rows, t.line(false, inner, part{strong, prompt}, part{plain, m.palette.query}, part{strong, "▏"}))
	if len(listed) == 0 {
		said := "Nothing matches."
		if m.palette.notices {
			said = "No notification."
		}
		rows = append(rows, t.line(false, inner, part{quiet, said}))
	}
	for i := first; i < first+room && i < len(listed); i++ {
		c := listed[i]
		left := []part{{plain, c.label}}
		right := []part{{quiet, c.kind}}
		if c.key != "" {
			right = []part{{strong, c.key}}
		}
		rows = append(rows, t.line(i == m.palette.chosen, inner, fitted(left, right, inner)...))
	}
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	if t.painted() {
		style = style.BorderBackground(t.colour("bg")).BorderForeground(t.colour("accent"))
	}
	box := style.Render(strings.Join(rows, "\n"))
	c := lipgloss.NewCanvas(m.width, m.height)
	c.Compose(lipgloss.NewCompositor(lipgloss.NewLayer(screen), lipgloss.NewLayer(box).X((m.width-lipgloss.Width(box))/2).Y(1).Z(2)))
	lines := strings.Split(c.Render(), "\n")
	for i, l := range lines {
		if w := lipgloss.Width(l); w < m.width {
			lines[i] = l + t.line(false, m.width-w)
		}
	}
	return strings.Join(lines, "\n")
}

// paletteKeys are the key line while the palette is open.
func paletteKeys() [][2]string {
	return [][2]string{{"type", "Find"}, {"↑↓", "Choose"}, {"enter", "Do"}, {"esc", "Close"}}
}
