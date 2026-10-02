package console

import (
	"cmp"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

// The workflows view lists the workflows of the namespace shown, or of every namespace the
// principal reads, each with its latest run and its last twenty, and for the one chosen its
// repository, its declared triggers, its versions, and the compact form of its statistics: a
// histogram of its durations over 30 days, its p50 and p95 marked. The API lists no namespace's
// workflows, so the view lists those its last 200 runs name, as the web console's workflows page
// does, and says so. enter opens the graph of the version the default branch's head resolves to.

// flowsRead is the runs the workflows are read from.
type flowsRead struct {
	runs  []db.ListedRun
	err   error
	shown int
}

// flowRow is one workflow as the list draws it: the runs of it among those read, newest first.
type flowRow struct {
	namespace, workflow string
	runs                []db.ListedRun
}

func (f flowRow) key() string { return f.namespace + "/" + f.workflow }

// flowDetail is what GET /api/v1/{ns}/workflows/{name} answers of a workflow, as the view draws it.
type flowDetail struct {
	Repository struct {
		DefaultBranch string `json:"default_branch"`
		Protected     bool   `json:"protected"`
		Head          string `json:"head"`
	} `json:"repository"`
	Graph *struct {
		Commit string `json:"commit"`
		On     struct {
			Schedule []struct {
				Cron     string `json:"cron"`
				Timezone string `json:"timezone"`
			} `json:"schedule"`
			Webhook []struct {
				Path string `json:"path"`
			} `json:"webhook"`
			Event []struct {
				Source string `json:"source"`
				Type   string `json:"type"`
			} `json:"event"`
		} `json:"on"`
	} `json:"graph"`
	History []struct {
		Commit  string `json:"commit"`
		Subject string `json:"subject"`
		Author  *struct {
			Name string `json:"name"`
		} `json:"author"`
		AuthoredAt time.Time `json:"authored_at"`
		Version    *struct {
			Commit string `json:"commit"`
		} `json:"version"`
	} `json:"history"`
}

// flowStats is what GET /api/v1/{ns}/stats/runs answers of a workflow over 30 days.
type flowStats struct {
	Overall struct {
		DurationMS struct {
			P50 *int64 `json:"p50"`
			P95 *int64 `json:"p95"`
		} `json:"duration_ms"`
	} `json:"overall"`
	Histogram []struct {
		FromMS  int64 `json:"from_ms"`
		UntilMS int64 `json:"until_ms"`
		Runs    int   `json:"runs"`
	} `json:"histogram"`
}

type (
	flowDetailRead struct {
		key    string
		detail *flowDetail
		err    error
	}
	flowStatsRead struct {
		key   string
		stats *flowStats
		err   error
	}
)

// flowsLimit is how many runs the workflows are read from, as many as the web console reads.
const flowsLimit = 200

// histogramBins and histogramDays are the compact statistics' bins and range: a dozen bins, which
// a line holds, over the 30 days the documentation names.
const (
	histogramBins = 12
	histogramDays = 30
)

func (m Model) readFlows() tea.Cmd {
	path := fmt.Sprintf("/api/v1/runs?limit=%d", flowsLimit)
	if m.o.Namespace != "" {
		path += "&namespace=" + url.QueryEscape(m.o.Namespace)
	}
	shown := m.shown
	return func() tea.Msg {
		var listed struct {
			Runs []db.ListedRun `json:"runs"`
		}
		err := m.o.Read(m.ctx, path, &listed)
		return flowsRead{runs: listed.Runs, err: err, shown: shown}
	}
}

// flowRows groups the runs read by workflow, by namespace and name.
func flowRows(runs []db.ListedRun) []flowRow {
	by := map[string]*flowRow{}
	var rows []*flowRow
	for _, r := range runs {
		key := r.Namespace + "/" + r.Workflow
		row, ok := by[key]
		if !ok {
			row = &flowRow{namespace: r.Namespace, workflow: r.Workflow}
			by[key] = row
			rows = append(rows, row)
		}
		row.runs = append(row.runs, r)
	}
	slices.SortFunc(rows, func(a, b *flowRow) int { return cmp.Compare(a.key(), b.key()) })
	out := make([]flowRow, len(rows))
	for i, r := range rows {
		out[i] = *r
	}
	return out
}

// chosenFlow is the workflow chosen, the first where none is.
func (m Model) chosenFlow() (flowRow, bool) {
	if len(m.flows) == 0 {
		return flowRow{}, false
	}
	for _, f := range m.flows {
		if f.key() == m.flow {
			return f, true
		}
	}
	return m.flows[0], true
}

// readChosenFlow reads what the workflow chosen shows beyond its row, once.
func (m Model) readChosenFlow() []tea.Cmd {
	f, ok := m.chosenFlow()
	if !ok {
		return nil
	}
	key, ns, wf := f.key(), f.namespace, f.workflow
	var cmds []tea.Cmd
	if _, read := m.flowDetails[key]; !read {
		cmds = append(cmds, func() tea.Msg {
			var d flowDetail
			if err := m.o.Read(m.ctx, "/api/v1/"+url.PathEscape(ns)+"/workflows/"+url.PathEscape(wf)+"?limit=5", &d); err != nil {
				return flowDetailRead{key: key, err: err}
			}
			return flowDetailRead{key: key, detail: &d}
		})
	}
	if _, read := m.flowStatsOf[key]; !read {
		to := m.o.Now().UTC().Truncate(time.Minute)
		from := to.AddDate(0, 0, -histogramDays)
		path := fmt.Sprintf("/api/v1/%s/stats/runs?workflow=%s&from=%s&to=%s&bucket=1d&histogram=%d",
			url.PathEscape(ns), url.QueryEscape(wf), url.QueryEscape(from.Format(time.RFC3339)), url.QueryEscape(to.Format(time.RFC3339)), histogramBins)
		cmds = append(cmds, func() tea.Msg {
			var s flowStats
			if err := m.o.Read(m.ctx, path, &s); err != nil {
				return flowStatsRead{key: key, err: err}
			}
			return flowStatsRead{key: key, stats: &s}
		})
	}
	return cmds
}

// histogram is a workflow's durations over 30 days in block characters, one per bin, the bins
// holding its p50 and p95 in the accent and the waiting amber, beside both written.
func histogram(s *flowStats) []part {
	if s == nil || len(s.Histogram) == 0 {
		return []part{{quiet, "no run ended in 30 days"}}
	}
	blocks := []rune("▁▂▃▄▅▆▇█")
	most := 0
	for _, b := range s.Histogram {
		most = max(most, b.Runs)
	}
	in := func(ms *int64, from, until int64, last bool) bool {
		return ms != nil && *ms >= from && (*ms < until || last && *ms <= until)
	}
	var parts []part
	for i, b := range s.Histogram {
		level := 0
		if most > 0 && b.Runs > 0 {
			level = max(1, b.Runs*(len(blocks)-1)/most)
		}
		r := muted
		last := i == len(s.Histogram)-1
		switch {
		case in(s.Overall.DurationMS.P95, b.FromMS, b.UntilMS, last):
			r = waitingText
		case in(s.Overall.DurationMS.P50, b.FromMS, b.UntilMS, last):
			r = runningText
		}
		parts = append(parts, part{r, string(blocks[level])})
	}
	if p := s.Overall.DurationMS.P50; p != nil {
		parts = append(parts, part{plain, "  "}, part{runningText, "p50 " + Took(time.Duration(*p)*time.Millisecond)})
	}
	if p := s.Overall.DurationMS.P95; p != nil {
		parts = append(parts, part{plain, "  "}, part{waitingText, "p95 " + Took(time.Duration(*p)*time.Millisecond)})
	}
	return parts
}

// triggersOf are a workflow's declared triggers, in a line, or that it runs by hand alone.
func triggersOf(d *flowDetail) string {
	if d == nil || d.Graph == nil {
		return ""
	}
	var said []string
	for _, s := range d.Graph.On.Schedule {
		t := "schedule " + s.Cron
		if s.Timezone != "" {
			t += " " + s.Timezone
		}
		said = append(said, t)
	}
	for _, w := range d.Graph.On.Webhook {
		said = append(said, "webhook "+w.Path)
	}
	for _, e := range d.Graph.On.Event {
		said = append(said, "event "+strings.TrimSpace(e.Source+" "+e.Type))
	}
	if len(said) == 0 {
		return "none, it is run by hand, by agk or the API"
	}
	return strings.Join(said, " · ")
}

// workflowsLines is the workflows view: the list, then the workflow chosen.
func (m Model) workflowsLines(t theme, height int) []string {
	line := func(parts ...part) string { return t.line(false, m.width, within(parts, m.width)...) }
	switch {
	case !m.flowsRead && m.flowsFailed != "":
		return []string{line(part{failedText, "The workflows could not be read: " + m.flowsFailed})}
	case !m.flowsRead:
		return []string{line(part{quiet, "Reading the workflows."})}
	case len(m.flows) == 0:
		return []string{line(part{quiet, "No workflow has run yet."})}
	}
	now := m.o.Now()
	lines := []string{line(fitted([]part{{strong, "Workflows"}}, []part{{muted, fmt.Sprintf("%s named by the last %d runs, since the API lists none", counting(len(m.flows), "workflow"), flowsLimit)}}, m.width)...)}
	h := func(s string) []part { return []part{{quiet, s}} }
	columns := func(name, latest, at, runs, last []part) []part {
		return laid([]cell{{name, 0, false}, {latest, stateWidth, false}, {at, 16, false}, {runs, 5, true}, {last, 20, false}}, 0, m.width)
	}
	lines = append(lines, line(columns(h("WORKFLOW"), h("LATEST"), h("STARTED"), h("RUNS"), h("LAST 20"))...))
	chosen, _ := m.chosenFlow()
	room := max(1, height/2-2)
	at := slices.IndexFunc(m.flows, func(f flowRow) bool { return f.key() == chosen.key() })
	first := min(max(0, at-room+1), max(0, len(m.flows)-room))
	for i := first; i < len(m.flows) && i < first+room; i++ {
		f := m.flows[i]
		latest := f.runs[0]
		drawn, _ := bars(f.runs[:min(len(f.runs), 20)], now)
		lines = append(lines, t.line(f.key() == chosen.key(), m.width, columns(
			[]part{{plain, f.key()}},
			[]part{{stateRole(latest.State), m.mark(latest.State == agk.Running)}, {plain, " " + latest.State.String()}},
			[]part{{muted, clock(startOf(latest), now)}},
			[]part{{plain, fmt.Sprint(len(f.runs))}},
			drawn)...))
	}
	lines = append(lines, line())

	detail, read := m.flowDetails[chosen.key()]
	header := []part{{strong, chosen.key()}}
	if read && detail.detail != nil {
		r := detail.detail.Repository
		about := "default branch " + r.DefaultBranch
		if r.Protected {
			about += ", protected"
		}
		if r.Head != "" {
			about += " · head " + short(r.Head)
		}
		header = append(header, part{muted, "  " + about})
	}
	lines = append(lines, line(header...))
	label := func(s string) part { return part{muted, cellOf(s, 10)} }
	switch {
	case !read:
		lines = append(lines, line(part{quiet, "Reading the workflow."}))
	case detail.err != nil:
		lines = append(lines, line(part{failedText, "The workflow could not be read: " + said(detail.err)}))
	default:
		lines = append(lines, line(label("triggers"), part{plain, triggersOf(detail.detail)}))
	}
	drawn, median := bars(chosen.runs[:min(len(chosen.runs), 20)], now)
	last := append([]part{label("last 20")}, drawn...)
	if median > 0 {
		last = append(last, part{muted, "  median " + Took(median)})
	}
	lines = append(lines, line(last...))
	switch s, read := m.flowStatsOf[chosen.key()]; {
	case !read:
		lines = append(lines, line(label("30 days"), part{quiet, "Reading the statistics."}))
	case s.err != nil:
		lines = append(lines, line(label("30 days"), part{failedText, "The statistics could not be read: " + said(s.err)}))
	default:
		lines = append(lines, line(append([]part{label("30 days")}, histogram(s.stats)...)...))
	}
	if read && detail.detail != nil && len(detail.detail.History) > 0 {
		lines = append(lines, line(), line(part{quiet, "VERSIONS"}))
		for _, c := range detail.detail.History {
			who := ""
			if c.Author != nil {
				who = c.Author.Name
			}
			mark := "  "
			if c.Version != nil {
				mark = "● "
			}
			lines = append(lines, line(part{runningText, mark}, part{plain, short(c.Commit) + "  "}, part{muted, cellOf(clock(c.AuthoredAt, now), 16) + "  " + cellOf(who, 16) + "  "}, part{plain, c.Subject}))
		}
		lines = append(lines, line(part{quiet, "● a version: a commit agentiik resolved, which a run can run"}))
	}
	return lines
}
