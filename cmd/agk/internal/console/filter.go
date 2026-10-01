package console

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/db"
)

// The runs are filtered as they are typed, as the documentation's filter does: / opens a line above
// the list, and each key narrows it at once. A word matches its letters in order against what is
// loaded, the closest matches first; a name=value term is one GET /api/v1/runs takes, sent once
// typing pauses for a quarter of a second, since the list holds only the runs loaded and a filter
// over those alone would miss older ones. The words then match within what the installation
// answered. enter keeps the filter and goes back to the list; esc clears it.

// termNames are the terms GET /api/v1/runs takes, in the order a query writes them.
var termNames = []string{"namespace", "workflow", "state", "since", "until"}

// pause is how long typing has to pause before the terms are sent: long enough not to ask for
// every letter of a workflow's name, short enough to feel at once.
const pause = 250 * time.Millisecond

// typedPaused is a quarter of a second passed since a key typed into the filter; typed counts the
// keys, so that one passed since an earlier key is told from the last.
type typedPaused struct{ typed int }

// parsed is a filter read into its words and its terms, and the names of terms it does not know.
type parsed struct {
	words   []string
	terms   map[string]string
	unknown []string
}

func parse(filter string) parsed {
	p := parsed{terms: map[string]string{}}
	for _, f := range strings.Fields(filter) {
		name, value, isTerm := strings.Cut(f, "=")
		switch {
		case !isTerm:
			p.words = append(p.words, f)
		case slices.Contains(termNames, name) && value != "":
			p.terms[name] = value
		case !slices.Contains(termNames, name):
			p.unknown = append(p.unknown, name)
		}
	}
	return p
}

// query is the terms as GET /api/v1/runs reads them, since written back as an instant where it is
// a duration back from now: 30m, 24h or 7d.
func (p parsed) query(now time.Time) string {
	var q []string
	for _, name := range termNames {
		v, ok := p.terms[name]
		if !ok {
			continue
		}
		if name == "since" {
			if back, ok := durationBack(v); ok {
				v = now.Add(-back).UTC().Format(time.RFC3339)
			}
		}
		q = append(q, name+"="+url.QueryEscape(v))
	}
	return strings.Join(q, "&")
}

// durationBack reads a length of time back from now, as Go writes one or as a number of days.
func durationBack(s string) (time.Duration, bool) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n >= 0 {
			return time.Duration(n) * 24 * time.Hour, true
		}
	}
	d, err := time.ParseDuration(s)
	return d, err == nil && d >= 0
}

// matched scores how closely a word matches a text, its letters in order: most where it is there
// whole and at the start of a word of the text, then whole anywhere, then its letters apart, more
// for each two that follow each other; and -1 where its letters are not all there.
func matched(word, text string) int {
	w, t := []rune(strings.ToLower(word)), []rune(strings.ToLower(text))
	if i := strings.Index(string(t), string(w)); i >= 0 {
		at := len([]rune(string(t)[:i]))
		score := 100 + len(w)
		if at == 0 || !unicode.IsLetter(t[at-1]) && !unicode.IsDigit(t[at-1]) {
			score += 50
		}
		return score
	}
	score, j, last := 0, 0, -2
	for i := 0; i < len(t) && j < len(w); i++ {
		if t[i] != w[j] {
			continue
		}
		if i == last+1 {
			score += 5
		} else {
			score++
		}
		last = i
		j++
	}
	if j < len(w) {
		return -1
	}
	return score
}

// shownRuns are the runs the filter's words leave, the closest matches first and newest first
// among equals; every run where it has none.
func (m Model) shownRuns() []db.ListedRun {
	words := parse(m.filter).words
	if len(words) == 0 {
		return m.runs
	}
	type scored struct {
		run   db.ListedRun
		score int
	}
	var kept []scored
	for _, r := range m.runs {
		fields := []string{string(r.Run), r.Namespace + "/" + r.Workflow, r.State.String(), r.Trigger.String(), r.TriggeredBy}
		total := 0
		for _, w := range words {
			best := -1
			for _, f := range fields {
				best = max(best, matched(w, f))
			}
			if best < 0 {
				total = -1
				break
			}
			total += best
		}
		if total >= 0 {
			kept = append(kept, scored{r, total})
		}
	}
	slices.SortStableFunc(kept, func(a, b scored) int { return b.score - a.score })
	runs := make([]db.ListedRun, len(kept))
	for i, k := range kept {
		runs[i] = k.run
	}
	return runs
}

// typing takes a key while the filter line is open: a character typed narrows the list, enter
// keeps the filter and goes back to the list, esc clears it, and the arrows still move the
// selection, so that the run wanted is chosen without leaving the line.
func (m Model) typing(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key := msg.String(); key {
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		m.filtering = false
		return m, nil
	case "esc":
		m.filtering = false
		return m.filtered("")
	case "up", "down":
		return m.press(key)
	case "backspace":
		r := []rune(m.filter)
		if len(r) == 0 {
			return m, nil
		}
		return m.filtered(string(r[:len(r)-1]))
	}
	if msg.Text == "" {
		return m, nil
	}
	return m.filtered(m.filter + msg.Text)
}

// filtered is the filter changed to f: the words narrow the list at once, the selection kept where
// it is still shown and moved to the first run where it is not, and the terms sent once typing
// pauses.
func (m Model) filtered(f string) (tea.Model, tea.Cmd) {
	m.filter = f
	shown := m.shownRuns()
	if !slices.ContainsFunc(shown, func(r db.ListedRun) bool { return string(r.Run) == m.selected }) {
		m.selected = ""
		if len(shown) > 0 {
			m.selected = string(shown[0].Run)
		}
	}
	m.keys++
	typed := m.keys
	return m, m.tick(pause, func(time.Time) tea.Msg { return typedPaused{typed: typed} })
}

// paused sends the terms where typing has paused and they changed since they were last sent.
func (m Model) paused(msg typedPaused) (tea.Model, tea.Cmd) {
	if msg.typed != m.keys || m.view != runsView {
		return m, nil
	}
	now := m.o.Now()
	if parse(m.filter).query(now) == parse(m.terms).query(now) {
		m.terms = m.filter
		return m, nil
	}
	m.terms, m.filterRefused = m.filter, ""
	m.shown++
	return m, m.readShown()
}

// filterLine is the filter as it is typed, above the list: what was typed, the terms the
// installation was asked for, and how many runs it leaves.
func (m Model) filterLine(t theme) string {
	left := []part{{strong, "/ "}}
	switch {
	case m.filter == "" && m.filtering:
		left = append(left, part{quiet, "filter as you type, or state=failed since=24h"})
	default:
		left = append(left, part{plain, m.filter})
	}
	if m.filtering {
		left = append(left, part{strong, "▏"})
	}
	p := parse(m.filter)
	var right []part
	switch {
	case m.filterRefused != "":
		right = []part{{failedText, m.filterRefused}}
	case len(p.unknown) > 0:
		right = []part{{failedText, strings.Join(p.unknown, ", ") + ": a term is namespace, workflow, state, since or until"}}
	case m.filter != "":
		right = []part{{muted, strconv.Itoa(len(m.shownRuns())) + " of " + counting(len(m.runs), "run")}}
	}
	return t.line(false, m.width, fitted(left, right, m.width)...)
}
