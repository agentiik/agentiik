package console

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

// The run inspector: the run, its steps with their verdicts, how long each took and its shards, and
// the step chosen as its tasks left it: each shard's last attempt with its exit code and what the
// code means, the image by digest, the runner by name with its pool and labels, never its host;
// and each port the step published, with the items in its envelope, and the envelope itself under
// run:read_data alone, hidden rather than refused to anybody else.

// principal is the caller as GET /api/v1/me gives it: who, whether an administrator, and what they
// hold where, by scope as a grant writes it.
type principal struct {
	Principal   string              `json:"principal"`
	Admin       bool                `json:"admin"`
	Permissions map[string][]string `json:"permissions"`
}

// holds says whether the caller holds a permission on a workflow, its own scope where /me carries
// one and its namespace's otherwise, as the web console reads it.
func (p principal) holds(permission, namespace, workflow string) bool {
	held, ok := p.Permissions[namespace+"/"+workflow]
	if !ok {
		held = p.Permissions[namespace]
	}
	return slices.Contains(held, permission)
}

type (
	// runnersRead is the inventory, read once a run is opened by an administrator, who alone may.
	runnersRead struct{ runners map[string]db.Runner }
	// payloadRead is one port's envelope, read when it is chosen.
	payloadRead struct {
		key  string
		text string
		err  error
	}
)

func (m Model) readRunners() tea.Cmd {
	return func() tea.Msg {
		var listed struct {
			Runners []db.Runner `json:"runners"`
		}
		if err := m.o.Read(m.ctx, "/api/v1/runners", &listed); err != nil {
			return runnersRead{}
		}
		byName := make(map[string]db.Runner, len(listed.Runners))
		for _, r := range listed.Runners {
			byName[r.ID] = r
		}
		return runnersRead{runners: byName}
	}
}

// shownLines is how many lines of an envelope the inspector draws: enough to read an item or two,
// and the rest is the web console's, or agk's, to page through.
const shownLines = 40

func (m Model) readPayload(run, step, port string) tea.Cmd {
	key := step + "/" + port
	return func() tea.Msg {
		var envelope json.RawMessage
		path := "/api/v1/runs/" + url.PathEscape(run) + "/steps/" + url.PathEscape(step) + "/outputs/" + url.PathEscape(port)
		if err := m.o.Read(m.ctx, path, &envelope); err != nil {
			return payloadRead{key: key, err: err}
		}
		var b bytes.Buffer
		if err := json.Indent(&b, envelope, "", "  "); err != nil {
			return payloadRead{key: key, text: string(envelope)}
		}
		return payloadRead{key: key, text: b.String()}
	}
}

// mayReadData says whether the run's payloads are the caller's to read.
func (m Model) mayReadData() bool {
	return m.run != nil && m.me.holds("run:read_data", m.run.Namespace, m.run.Workflow)
}

// stepOf is the step chosen, the first that failed where none is, or the first.
func stepOf(run *db.RunDetail, chosen string) string {
	for _, s := range run.Steps {
		if string(s.Step) == chosen {
			return chosen
		}
	}
	for _, s := range run.Steps {
		if s.Verdict == agk.VerdictFailed {
			return string(s.Step)
		}
	}
	if len(run.Steps) > 0 {
		return string(run.Steps[0].Step)
	}
	return ""
}

func summaryOf(run *db.RunDetail, step string) *db.StepSummary {
	for i := range run.Steps {
		if string(run.Steps[i].Step) == step {
			return &run.Steps[i]
		}
	}
	return nil
}

// portsOf is a step's ports that hold an envelope, in the order the step declares its outputs and
// by name after them.
func portsOf(s *db.StepSummary) []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, p := range s.OutputPorts {
		if _, ok := s.Ports[p]; ok {
			out = append(out, string(p))
		}
	}
	var rest []string
	for p := range s.Ports {
		if !slices.Contains(out, string(p)) {
			rest = append(rest, string(p))
		}
	}
	slices.Sort(rest)
	return append(out, rest...)
}

// lastAttempts is each shard's last attempt of a step, its shards in order: the attempt a step's
// verdict was decided on, the earlier ones being what a retry moved past.
func lastAttempts(run *db.RunDetail, step string) []db.TaskSummary {
	last := map[int]db.TaskSummary{}
	for _, t := range run.Tasks {
		if string(t.Step) != step {
			continue
		}
		shard := 0
		if t.Shard != nil {
			shard = t.Shard.Index
		}
		if was, ok := last[shard]; !ok || t.Attempt >= was.Attempt {
			last[shard] = t
		}
	}
	shards := make([]int, 0, len(last))
	for s := range last {
		shards = append(shards, s)
	}
	slices.Sort(shards)
	out := make([]db.TaskSummary, 0, len(shards))
	for _, s := range shards {
		out = append(out, last[s])
	}
	return out
}

func verdictRole(v agk.Verdict) role {
	switch v {
	case agk.VerdictRunning:
		return runningText
	case agk.VerdictSucceeded:
		return succeededText
	case agk.VerdictFailed:
		return failedText
	}
	return quiet
}

func taskRole(s agk.TaskState) role {
	switch s {
	case agk.TaskRunning, agk.TaskDispatched, agk.TaskPublishing:
		return runningText
	case agk.TaskSucceeded:
		return succeededText
	case agk.TaskFailed, agk.TaskLost, agk.TaskTimedOut:
		return failedText
	}
	return quiet
}

func took(start, end, now time.Time) string {
	if start.IsZero() {
		return ""
	}
	if end.IsZero() {
		end = now
	}
	return Took(end.Sub(start))
}

// exited is an exit code and what the table makes of it: exit 108, a transient failure.
func exited(code int) string {
	return fmt.Sprintf("exit %d, %s", code, agk.Band(code))
}

// runHeader is the run in two lines: what it is and how it stands, and where it failed.
func (m Model) runHeader(now time.Time) [][]part {
	r := m.run
	lines := [][]part{
		{{strong, "Run "}, {muted, string(r.Run)}, {plain, "  " + r.Namespace + "/" + r.Workflow + "@" + short(r.Commit) + "  "}, {stateRole(r.State), "●"}, {plain, " " + r.State.String()}},
	}
	about := []string{r.Trigger.String()}
	if r.TriggeredBy != "" {
		about = append(about, "by "+r.TriggeredBy)
	}
	if !r.StartedAt.IsZero() {
		about = append(about, "started "+clock(r.StartedAt, now), "took "+lasted(r.RunSummary, now))
	}
	if r.ReplayOf != "" {
		replays := "replays " + string(r.ReplayOf)
		if r.ReplayFrom != "" {
			replays += " from " + string(r.ReplayFrom)
		}
		about = append(about, replays)
	}
	lines = append(lines, []part{{muted, strings.Join(about, " · ")}})
	for _, s := range r.Steps {
		if s.Verdict != agk.VerdictFailed {
			continue
		}
		for _, t := range lastAttempts(r, string(s.Step)) {
			if t.State == agk.TaskSucceeded || t.ExitCode == nil {
				continue
			}
			where := string(s.Step)
			if t.Shard != nil && !t.Shard.IsZero() {
				where += ", shard " + t.Shard.String()
			}
			lines = append(lines, []part{{plain, "failed at " + where + ": "}, {failedText, exited(*t.ExitCode)}, {plain, fmt.Sprintf(", attempt %d", t.Attempt)}})
			return lines
		}
	}
	if r.Reason != "" {
		lines = append(lines, []part{{failedText, r.Reason}})
	}
	return lines
}

func short(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}

// stepRow is one step in the steps pane: its verdict, name, how long and how many shards.
func (m Model) stepRow(s db.StepSummary, now time.Time, width int) []part {
	tasks := lastAttempts(m.run, string(s.Step))
	shards := len(tasks)
	failed := 0
	for _, t := range tasks {
		if t.State == agk.TaskFailed || t.State == agk.TaskLost || t.State == agk.TaskTimedOut {
			failed++
		}
	}
	count := ""
	switch {
	case shards > 1 && failed > 0:
		count = fmt.Sprintf("%d shards · %d failed", shards, failed)
	case shards > 1:
		count = fmt.Sprintf("%d shards", shards)
	case s.Attempts > 1:
		count = fmt.Sprintf("attempt %d", s.Attempts)
	}
	// The name is as wide as the run's longest, so that the durations line up, and is cut only
	// where the verdict and the duration leave it less; the shards counted take what is left.
	longest := 0
	for _, other := range m.run.Steps {
		longest = max(longest, len([]rune(string(other.Step))))
	}
	name := cellOf(string(s.Step), max(8, min(longest, width-12-8-1)))
	return []part{
		{verdictRole(s.Verdict), "●"}, {plain, " " + cellOf(s.Verdict.String(), 10) + " "}, {plain, name + " "},
		{muted, padLeft(took(s.StartedAt, s.FinishedAt, now), 7) + " "}, {quiet, count},
	}
}

// stepLines is the step chosen: its image, each shard's last attempt, and its ports.
func (m Model) stepLines(step string, now time.Time, width int) [][]part {
	s := summaryOf(m.run, step)
	if s == nil {
		return [][]part{{{quiet, "No step to show yet."}}}
	}
	lines := [][]part{{{strong, step}, {plain, "  "}, {verdictRole(s.Verdict), "●"}, {plain, " " + s.Verdict.String()}, {muted, "  " + took(s.StartedAt, s.FinishedAt, now)}}}
	if s.Image != "" {
		lines = append(lines, []part{{muted, "image   "}, {plain, s.Image}})
	}
	tasks := lastAttempts(m.run, step)
	if len(tasks) > 0 {
		lines = append(lines, nil, []part{{quiet, "SHARD  STATE      ATTEMPT  EXIT                          RUNNER"}})
	}
	for _, t := range tasks {
		shard := "-"
		if t.Shard != nil && !t.Shard.IsZero() {
			shard = t.Shard.String()
		}
		exit := ""
		if t.ExitCode != nil {
			exit = exited(*t.ExitCode)
		}
		where := t.Runner
		switch {
		case t.MemoisedFrom != "":
			where = "none, a cache hit of " + string(t.MemoisedFrom)
		case t.Called != "":
			where = "none, it called " + string(t.Called)
		case t.Runner == "":
			where = "not held yet"
		case m.runners != nil:
			if r, ok := m.runners[t.Runner]; ok {
				where += " · pool " + r.Pool
				if len(r.Labels) > 0 {
					where += " · " + strings.Join(r.Labels, ",")
				}
			}
		}
		exitRole := plain
		if t.ExitCode != nil && *t.ExitCode != 0 {
			exitRole = failedText
		}
		lines = append(lines, []part{
			{plain, cellOf(shard, 6) + " "}, {taskRole(t.State), "●"}, {plain, " " + cellOf(t.State.String(), 9) + " "},
			{plain, cellOf(fmt.Sprint(t.Attempt), 8) + " "}, {exitRole, cellOf(exit, 29) + " "}, {muted, where},
		})
	}
	ports := portsOf(s)
	if len(ports) > 0 {
		lines = append(lines, nil, []part{{quiet, "  PORT         ITEMS   SIZE      DIGEST"}})
	}
	for i, p := range ports {
		e := s.Ports[agk.Port(p)]
		mark, r := "  ", plain
		if i == m.port {
			mark, r = "▸ ", strong
		}
		digest := e.Digest
		if !e.PurgedAt.IsZero() {
			digest += ", purged"
		}
		lines = append(lines, []part{{r, mark + cellOf(p, 12) + " "}, {plain, padLeft(fmt.Sprint(e.Items), 5) + "   " + cellOf(sizeOf(e.Size), 9) + " "}, {muted, digest}})
	}
	if len(ports) > 0 && m.mayReadData() {
		lines = append(lines, nil)
		key := step + "/" + ports[min(m.port, len(ports)-1)]
		switch p, ok := m.payloads[key]; {
		case !ok:
			lines = append(lines, []part{{quiet, "Reading the envelope."}})
		case p.err != nil:
			lines = append(lines, []part{{failedText, "The envelope could not be read: " + said(p.err)}})
		default:
			text := strings.Split(strings.TrimRight(p.text, "\n"), "\n")
			for _, l := range text[:min(len(text), shownLines)] {
				lines = append(lines, []part{{plain, l}})
			}
			if len(text) > shownLines {
				lines = append(lines, []part{{quiet, fmt.Sprintf("%d lines more", len(text)-shownLines)}})
			}
		}
	}
	return lines
}

func sizeOf(bytes int64) string {
	switch {
	case bytes < 1024:
		return fmt.Sprintf("%d B", bytes)
	case bytes < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(bytes)/(1<<20))
}

// cellOf is a text as wide as a cell, cut with an ellipsis or padded.
func cellOf(s string, width int) string {
	r := []rune(s)
	if len(r) > width {
		return string(r[:max(0, width-1)]) + "…"
	}
	return s + strings.Repeat(" ", width-len(r))
}

func padLeft(s string, width int) string {
	if n := len([]rune(s)); n < width {
		return strings.Repeat(" ", width-n) + s
	}
	return s
}

// runLines is the run view: the run's header, then its steps beside the step chosen where the
// window has room, 120 columns and more, and above it where it has not.
func (m Model) runLines(t theme, height int) []string {
	switch {
	case m.runFailed != "":
		return []string{t.line(false, m.width, part{strong, "Run " + m.selected}), t.line(false, m.width), t.line(false, m.width, part{failedText, "The run could not be read: " + m.runFailed})}
	case !m.runRead || m.run == nil:
		return []string{t.line(false, m.width, part{strong, "Run " + m.selected}), t.line(false, m.width), t.line(false, m.width, part{quiet, "Reading the run."})}
	}
	now := m.o.Now()
	var lines []string
	for _, h := range m.runHeader(now) {
		lines = append(lines, t.line(false, m.width, within(h, m.width)...))
	}
	if m.acted != "" {
		lines = append(lines, t.line(false, m.width, within([]part{{muted, m.acted}}, m.width)...))
	}
	if m.problem != "" {
		lines = append(lines, t.line(false, m.width, within([]part{{failedText, m.problem}}, m.width)...))
	}
	lines = append(lines, t.line(false, m.width))
	step := stepOf(m.run, m.step)
	left := m.width
	if m.wide() {
		left = max(40, m.width*2/5)
	}
	var steps []string
	steps = append(steps, t.line(false, left, part{quiet, fmt.Sprintf("STEPS %d", len(m.run.Steps))}))
	for _, s := range m.run.Steps {
		steps = append(steps, t.line(string(s.Step) == step, left, within(m.stepRow(s, now, left), left)...))
	}
	right := m.width
	if m.wide() {
		right = m.width - left - 2
	}
	var detail []string
	for _, l := range m.stepLines(step, now, right) {
		detail = append(detail, t.line(false, right, within(l, right)...))
	}
	if !m.wide() {
		return m.withLog(t, append(append(append(lines, steps...), t.line(false, m.width)), detail...), step, height)
	}
	gap := t.line(false, 2)
	for i := 0; i < max(len(steps), len(detail)); i++ {
		l, r := t.line(false, left), t.line(false, right)
		if i < len(steps) {
			l = steps[i]
		}
		if i < len(detail) {
			r = detail[i]
		}
		lines = append(lines, l+gap+r)
	}
	return m.withLog(t, lines, step, height)
}

// withLog puts the log of the step chosen beneath the run, taking what the run leaves and never
// under a third of the window, the run cut where it would take more: the log is what is read while
// a step runs, and the run is read again on its own when the window is larger.
func (m Model) withLog(t theme, lines []string, step string, height int) []string {
	if m.o.Follow == nil {
		return lines
	}
	room := max(6, height/3)
	if len(lines)+1+room <= height {
		room = height - len(lines) - 1
	} else {
		lines = lines[:max(0, height-room-1)]
	}
	return append(append(lines, t.line(false, m.width)), m.logLines(t, step, room)...)
}
