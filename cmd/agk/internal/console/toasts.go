package console

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/agentiik/agentiik/db"
)

// A toast is a notification GET /api/v1/me holds that arrived while the console is open, read again
// every 30 seconds, or the end of a run the principal started, seen in the runs the console already
// reads. Half a minute late costs nothing for a notice kept 90 days, and costs the API one small
// read. Notifications already waiting at start are counted in the top line and not toasted, so that
// opening the console never buries the screen. A toast stays eight seconds in the corner over the
// panes and never takes the focus: a key pressed while one shows goes where it was going, since a
// toast that took keys would swallow the y of a prompt.

// notice is a notification as GET /api/v1/me lists it.
type notice struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Act       string `json:"act"`
	By        string `json:"by"`
	Namespace string `json:"namespace"`
	Login     string `json:"login"`
}

// acts are what an administrator did that widened their own access, as the web console says them.
var acts = map[string]string{
	"granted":       "wrote a grant by the installation's power",
	"deny_lifted":   "lifted a deny from their own access",
	"joined_group":  "put a user in a group holding a role",
	"left_group":    "left a group whose deny applied",
	"group_removed": "removed a group whose deny applied",
}

// sentence is what a notification says, in the web console's words, the identifiers it names kept
// as they are written everywhere else.
func (n notice) sentence() string {
	or := func(s, instead string) string {
		if s == "" {
			return instead
		}
		return s
	}
	switch n.Kind {
	case "admin_access_widened":
		act := "widened access"
		if n.Act != "" {
			act = or(acts[n.Act], n.Act)
		}
		return or(n.By, "An administrator") + " " + act + " in " + or(n.Namespace, "a namespace") + "."
	case "passkey_counter_refused":
		return "A sign-in with one of your passkeys was refused: its signature counter did not move forward, as a copied authenticator's does."
	case "break_glass_recovery":
		return "A recovery code was issued to " + or(n.Login, "an administrator") + " from the installation's host."
	}
	return n.Kind
}

// toast is one notice shown in the corner, until it goes.
type toast struct {
	id   string
	text string
}

type (
	// meAgain is half a minute passed since GET /api/v1/me was last read.
	meAgain struct{}
	// toastGone is a toast's eight seconds passed.
	toastGone struct{ id string }
)

const (
	// meEvery is how often GET /api/v1/me is read for the notifications that arrived.
	meEvery = 30 * time.Second
	// toastFor is how long a toast stays.
	toastFor = 8 * time.Second
)

// noticed takes GET /api/v1/me as read again: the first time, every notification it holds is
// counted and none toasted; after it, each one not seen before is toasted.
func (m Model) noticed(me principal) (Model, []tea.Cmd) {
	first := m.seen == nil
	if first {
		m.seen = map[string]bool{}
	}
	var cmds []tea.Cmd
	for _, n := range me.Notifications {
		if m.seen[n.ID] {
			continue
		}
		m.seen[n.ID] = true
		if !first {
			var cmd tea.Cmd
			m, cmd = m.toasted(n.ID, n.sentence())
			cmds = append(cmds, cmd)
		}
	}
	return m, append(cmds, m.tick(meEvery, func(time.Time) tea.Msg { return meAgain{} }))
}

// toasted shows a toast and asks for it to go eight seconds later.
func (m Model) toasted(id, text string) (Model, tea.Cmd) {
	m.toasts = append(m.toasts, toast{id: id, text: text})
	return m, m.tick(toastFor, func(time.Time) tea.Msg { return toastGone{id: id} })
}

// runsEnded toasts the end of each run the principal started that the console saw going and now sees
// over, in the runs it already reads.
func (m Model) runsEnded(was, now []db.ListedRun) (Model, []tea.Cmd) {
	going := map[string]bool{}
	for _, r := range was {
		if !r.State.Terminal() {
			going[string(r.Run)] = true
		}
	}
	var cmds []tea.Cmd
	for _, r := range now {
		if !going[string(r.Run)] || !r.State.Terminal() || r.TriggeredBy == "" || r.TriggeredBy != m.me.Principal {
			continue
		}
		var cmd tea.Cmd
		m, cmd = m.toasted("run:"+string(r.Run), "Your run "+string(r.Run)+" of "+r.Namespace+"/"+r.Workflow+" ended "+r.State.String()+".")
		cmds = append(cmds, cmd)
	}
	return m, cmds
}

// gone takes a toast away.
func (m Model) gone(id string) Model {
	kept := m.toasts[:0:0]
	for _, t := range m.toasts {
		if t.id != id {
			kept = append(kept, t)
		}
	}
	m.toasts = kept
	return m
}

// withToasts lays the toasts over the screen, in its lower right corner above the key line, the
// newest lowest, each in a rounded frame no wider than half the window.
func (m Model) withToasts(t theme, screen string) string {
	if len(m.toasts) == 0 {
		return screen
	}
	width := max(30, m.width/2)
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).Width(width)
	if t.painted() {
		style = style.Background(t.colour("surface")).Foreground(t.colour("text")).
			BorderBackground(t.colour("surface")).BorderForeground(t.colour("accent"))
	}
	layers := []*lipgloss.Layer{lipgloss.NewLayer(screen)}
	bottom := m.height - 1
	for i := len(m.toasts) - 1; i >= 0 && bottom > 1; i-- {
		box := style.Render(strings.TrimSpace(m.toasts[i].text))
		h := lipgloss.Height(box)
		if bottom-h < 1 {
			break
		}
		t.toastPicks(m.toasts[i].id, m.width-lipgloss.Width(box)-1, bottom-h, lipgloss.Width(box), h)
		layers = append(layers, lipgloss.NewLayer(box).X(m.width-lipgloss.Width(box)-1).Y(bottom-h).Z(1))
		bottom -= h
	}
	c := lipgloss.NewCanvas(m.width, m.height)
	c.Compose(lipgloss.NewCompositor(layers...))
	// The canvas leaves out what ends a line in blank cells, which the ground is painted on.
	lines := strings.Split(c.Render(), "\n")
	for i, l := range lines {
		if w := lipgloss.Width(l); w < m.width {
			lines[i] = l + t.line(false, m.width-w)
		}
	}
	return strings.Join(lines, "\n")
}
