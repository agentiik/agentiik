package console

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
)

// The runners view is an administrator's alone, as GET /api/v1/runners and GET /api/v1/runner-pools
// are: the pools first, what each accepts, what it caps and what its runners offer now, then every
// runner by its identifier, pool and labels, its condition, concurrency and last heartbeat, and who
// drained or revoked it, when and why. Never a host: the API holds no name or address of one, and
// the console shows what the API answers.

// fleetRead is the pools and the runners, read together since each is read against the other.
type fleetRead struct {
	pools   []api.Pool
	runners []db.Runner
	err     error
}

func (m Model) readFleet() tea.Cmd {
	return func() tea.Msg {
		var pools struct {
			RunnerPools []api.RunnerPool `json:"runner_pools"`
		}
		if err := m.o.Read(m.ctx, "/api/v1/runner-pools", &pools); err != nil {
			return fleetRead{err: err}
		}
		var listed struct {
			Runners []db.Runner `json:"runners"`
		}
		if err := m.o.Read(m.ctx, "/api/v1/runners", &listed); err != nil {
			return fleetRead{err: err}
		}
		read := fleetRead{runners: listed.Runners}
		for _, p := range pools.RunnerPools {
			read.pools = append(read.pools, p.Pool)
		}
		return read
	}
}

// condition is one word for a runner, as the web console says it: the installation's order first,
// since that is what is obeyed, revoked or draining whatever the runner says; then whether it is
// heard from at all, one silent for db.LostAfter having had its tasks declared lost; and only then
// what it said of itself at its last heartbeat.
func condition(r db.Runner, now time.Time) string {
	if r.State == "revoked" || r.State == "draining" {
		return r.State
	}
	if r.LastSeenAt.IsZero() || now.Sub(r.LastSeenAt) > db.LostAfter {
		return "silent"
	}
	if r.ReportedState == "unhealthy" || r.ReportedState == "draining" {
		return r.ReportedState
	}
	return "ready"
}

// conditionRole is the colour a condition is drawn in, beside its word: green takes work, amber
// finishes what it holds, red takes nothing though nobody ordered it to stop, and faint is out of
// service for good.
func conditionRole(c string) role {
	switch c {
	case "ready":
		return succeededText
	case "draining":
		return waitingText
	case "unhealthy", "silent":
		return failedText
	}
	return quiet
}

// meaning is what a runner's condition does to work, said once for the runner chosen.
func meaning(r db.Runner, now time.Time) string {
	switch condition(r, now) {
	case "ready":
		return "takes work"
	case "draining":
		return "takes nothing new, and finishes what it holds"
	case "unhealthy":
		return "said at its last heartbeat that it takes nothing new, though nobody ordered it to stop"
	case "silent":
		if r.LastSeenAt.IsZero() {
			return "has sent no heartbeat yet"
		}
		return fmt.Sprintf("silent for %s: three heartbeats missed, and the tasks it held declared lost", Took(now.Sub(r.LastSeenAt)))
	}
	return "out of service for good"
}

// order is who drained or revoked a runner, when and why, or nothing where nobody did. A
// revocation replaces a drain's reason with its own, and says until when the results of the tasks
// it still holds are taken.
func order(r db.Runner, now time.Time) string {
	var s string
	switch {
	case r.RevokedBy != "" && !r.RevokedAt.IsZero():
		s = "revoked by " + r.RevokedBy + ", " + clock(r.RevokedAt, now)
	case r.DrainedBy != "" && !r.DrainedAt.IsZero():
		s = "drained by " + r.DrainedBy + ", " + clock(r.DrainedAt, now)
	default:
		return ""
	}
	if r.DrainReason != "" {
		s += ": " + r.DrainReason
	}
	return s
}

// heartbeat is how long ago a runner was last heard from.
func heartbeat(r db.Runner, now time.Time) string {
	if r.LastSeenAt.IsZero() {
		return "not yet"
	}
	return Took(max(0, now.Sub(r.LastSeenAt))) + " ago"
}

// labelled is a list of labels as one cell, or says there is none.
func labelled(labels []string) []part {
	if len(labels) == 0 {
		return []part{{quiet, "no label"}}
	}
	return []part{{plain, strings.Join(labels, ",")}}
}

// ceilings are a pool's caps on one task, as a step writes them, or none.
func ceilings(p api.Pool) string {
	var caps []string
	if c := p.Ceilings; c != nil {
		if c.CPU != "" {
			caps = append(caps, "cpu "+c.CPU)
		}
		if c.Memory != "" {
			caps = append(caps, "memory "+c.Memory)
		}
		if c.PIDs != 0 {
			caps = append(caps, fmt.Sprintf("pids %d", c.PIDs))
		}
	}
	if len(caps) == 0 {
		return "none"
	}
	return strings.Join(caps, " · ")
}

// offered is how many tasks a pool's runners take at once now: the concurrency of each one ready,
// which is what GET /api/v1/stats/pools calls its capacity. A draining, unhealthy or silent runner
// offers nothing new, whatever it would hold.
func offered(runners []db.Runner, now time.Time) int64 {
	var n int64
	for _, r := range runners {
		if condition(r, now) == "ready" {
			n += r.Concurrency
		}
	}
	return n
}

// counted says how many runners are in each condition, in the order a reader looks for trouble
// last.
func counted(runners []db.Runner, now time.Time) string {
	if len(runners) == 0 {
		return "none"
	}
	n := map[string]int{}
	for _, r := range runners {
		n[condition(r, now)]++
	}
	var said []string
	for _, c := range []string{"ready", "draining", "unhealthy", "silent", "revoked"} {
		if n[c] > 0 {
			said = append(said, fmt.Sprintf("%d %s", n[c], c))
		}
	}
	return strings.Join(said, " · ")
}

// capacity is what a runner's host has, as the agent measured it at the join.
func capacity(r db.Runner) []string {
	return []string{fmt.Sprintf("%d vCPU", r.CPU), hostBytes(r.MemoryBytes), hostBytes(r.DiskBytes) + " disk", r.Architecture}
}

// hostBytes is a host's memory or disk, a whole number of its unit written without a decimal,
// since a host's memory and disk usually are one.
func hostBytes(b int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	v, i := float64(b), 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0") + " " + units[i]
}

// movedRunner is the runner before or after the one chosen, the first where none is.
func movedRunner(runners []db.Runner, chosen string, by int) string {
	if len(runners) == 0 {
		return ""
	}
	for i, r := range runners {
		if r.ID == chosen {
			return runners[min(len(runners)-1, max(0, i+by))].ID
		}
	}
	return runners[0].ID
}

// The pool and runner columns that do not grow: a pool's name, a runner's pool and its condition
// as wide as their usual value, a heartbeat as wide as its header.
const (
	poolWidth      = 12
	conditionWidth = 11
	heartbeatWidth = 14
)

func (m Model) poolColumns(name, labels, accepts, caps, containment, runners, takes []part) []part {
	if !m.wide() {
		return laid([]cell{{name, poolWidth, false}, {labels, 0, false}, {runners, 22, false}, {takes, 13, true}}, 1, m.width)
	}
	// A pool's ceilings are as long as cpu 16 · memory 64Gi · pids 4096 where the window has the
	// room, and cut a little under 160 columns, where the runners counted keep theirs.
	capsWidth := 24
	if m.width >= 160 {
		capsWidth = 32
	}
	return laid([]cell{{name, poolWidth, false}, {labels, 20, false}, {accepts, 20, false}, {caps, capsWidth, false}, {containment, 11, false}, {runners, 0, false}, {takes, 13, true}}, 5, m.width)
}

func (m Model) runnerColumns(id, pool, labels, state, concurrency, seen, agent, order []part) []part {
	if !m.wide() {
		return laid([]cell{{id, 12, false}, {pool, poolWidth, false}, {state, conditionWidth, false}, {seen, heartbeatWidth, false}, {labels, 0, false}}, 4, m.width)
	}
	return laid([]cell{{id, 26, false}, {pool, poolWidth, false}, {labels, 20, false}, {state, conditionWidth, false}, {concurrency, 11, true}, {seen, heartbeatWidth, false}, {agent, 7, false}, {order, 0, false}}, 7, m.width)
}

// runnersLines is the runners view: the pools, then the runners, the one chosen drawn as the
// selection and described below the list.
func (m Model) runnersLines(t theme, height int) []string {
	line := func(parts ...part) string { return t.line(false, m.width, within(parts, m.width)...) }
	switch {
	case !m.fleetRead && m.fleetFailed != "":
		return []string{line(part{failedText, "The runners could not be read: " + m.fleetFailed})}
	case !m.fleetRead:
		return []string{line(part{quiet, "Reading the runners and their pools."})}
	}
	now := m.o.Now()
	h := func(s string) []part { return []part{{quiet, s}} }
	p := func(s string) []part { return []part{{plain, s}} }

	of := map[string][]db.Runner{}
	for _, r := range m.fleet {
		of[r.Pool] = append(of[r.Pool], r)
	}
	lines := []string{line(fitted([]part{{strong, "Pools"}}, []part{{muted, counting(len(m.pools), "pool")}}, m.width)...)}
	if len(m.pools) == 0 {
		lines = append(lines, line(part{quiet, "No pool yet."}))
	} else {
		lines = append(lines, line(m.poolColumns(h("POOL"), h("LABELS"), h("ACCEPTS"), h("CEILINGS"), h("CONTAINMENT"), h("RUNNERS"), h("TAKES AT ONCE"))...))
		// A pool list longer than a third of the window is cut, its count still said above it,
		// so that the runners keep the room they are opened for.
		for _, pool := range m.pools[:min(len(m.pools), max(1, height/3))] {
			accepts := []part{{quiet, "every namespace"}}
			if len(pool.Namespaces) > 0 {
				accepts = p(strings.Join(pool.Namespaces, ", "))
			}
			lines = append(lines, line(m.poolColumns(p(pool.Name), labelled(pool.Labels), accepts, p(ceilings(pool)), p(pool.Containment),
				p(counted(of[pool.Name], now)), p(fmt.Sprint(offered(of[pool.Name], now))))...))
		}
	}
	lines = append(lines, line(), line(fitted([]part{{strong, "Runners"}}, []part{{muted, counting(len(m.fleet), "runner")}}, m.width)...))
	if len(m.fleet) == 0 {
		return append(lines, line(part{quiet, "No runner has joined yet."}))
	}
	lines = append(lines, line(m.runnerColumns(h("RUNNER"), h("POOL"), h("LABELS"), h("STATE"), h("CONCURRENCY"), h("LAST HEARTBEAT"), h("AGENT"), h("DRAINED OR REVOKED"))...))

	chosen := m.fleet[0]
	at := 0
	for i, r := range m.fleet {
		if r.ID == m.runner {
			chosen, at = r, i
		}
	}
	detail := m.runnerDetail(chosen, now)
	note := line(part{quiet, "A runner is its identifier, pool and labels here, never the host it runs on: GET /api/v1/runners names none."})
	room := max(1, height-len(lines)-1-len(detail)-2)
	first := min(max(0, at-room+1), max(0, len(m.fleet)-room))
	for i := first; i < len(m.fleet) && i < first+room; i++ {
		r := m.fleet[i]
		c := condition(r, now)
		concurrency := ""
		if r.Concurrency > 0 {
			concurrency = fmt.Sprint(r.Concurrency)
		}
		lines = append(lines, t.line(r.ID == chosen.ID, m.width, m.runnerColumns(
			[]part{{muted, r.ID}}, p(r.Pool), labelled(r.Labels), []part{{conditionRole(c), "●"}, {plain, " " + c}},
			p(concurrency), p(heartbeat(r, now)), p(r.AgentVersion), []part{{muted, order(r, now)}})...))
	}
	lines = append(lines, line())
	for _, d := range detail {
		lines = append(lines, line(d...))
	}
	return append(lines, line(), note)
}

// runnerDetail is what is said of the runner chosen beyond its row: what its condition does to
// work, who stopped it and why, what its host has, and the dates it joined, was last heard from
// and has its credential to be rotated by.
func (m Model) runnerDetail(r db.Runner, now time.Time) [][]part {
	c := condition(r, now)
	detail := [][]part{{{strong, r.ID}, {plain, "  "}, {conditionRole(c), "●"}, {plain, " " + c + ": " + meaning(r, now)}}}
	if o := order(r, now); o != "" {
		if !r.ResultsAcceptedUntil.IsZero() {
			o += ", its results taken until " + clock(r.ResultsAcceptedUntil, now)
		}
		detail = append(detail, []part{{plain, o}})
	}
	about := []string{"pool " + r.Pool, labelled(r.Labels)[0].text}
	if r.Concurrency > 0 {
		about = append(about, fmt.Sprintf("concurrency %d", r.Concurrency))
	}
	about = append(append(about, capacity(r)...), "agent "+r.AgentVersion)
	dates := []string{"joined " + clock(r.JoinedAt, now), "last heartbeat " + heartbeat(r, now)}
	if !r.RotateBy.IsZero() {
		dates = append(dates, "its credential rotated by "+clock(r.RotateBy, now))
	}
	for _, l := range append(packed(about, m.width), packed(dates, m.width)...) {
		detail = append(detail, []part{{muted, l}})
	}
	return detail
}

// packed joins what is said with middle dots into as few lines as the width holds, a line broken
// between two things said rather than inside one.
func packed(said []string, width int) []string {
	var lines []string
	for _, s := range said {
		if n := len(lines); n > 0 && len([]rune(lines[n-1]))+3+len([]rune(s)) <= width {
			lines[n-1] += " · " + s
			continue
		}
		lines = append(lines, s)
	}
	return lines
}

// counting is a number of things with its noun, singular for one.
func counting(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
