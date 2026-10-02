package console

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

// The graph view draws the resolved graph of a run's workflow, as GET /api/v1/{ns}/workflows/{name}
// answers it for the version its default branch's head resolves to, each step with the run's state
// laid over it. g opens it from a run or the run selected, and g within it writes the same graph
// as a list and back: the list fits any window, and is what a screen reader reads.

// flowGraph is what the graph view reads of a resolved graph: its steps in the order it runs them,
// each one's edges and ports, and the workflow's inputs and outputs. The API writes more, which the
// view has no use for; graph.Resolved holds the whole and keeps no Go type of it to export, so this
// names the fields drawn and nothing else.
type flowGraph struct {
	Workflow string                     `json:"workflow"`
	Commit   string                     `json:"commit"`
	Inputs   map[string]json.RawMessage `json:"inputs"`
	Outputs  map[string]struct {
		From struct {
			Step string `json:"step"`
			Port string `json:"port"`
		} `json:"from"`
	} `json:"outputs"`
	Order []string            `json:"order"`
	Steps map[string]flowStep `json:"steps"`
}

type flowStep struct {
	Needs []struct {
		Step string `json:"step"`
		Port string `json:"port"`
		As   string `json:"as"`
	} `json:"needs"`
	Inputs   map[string]any `json:"inputs"`
	Outputs  []string       `json:"outputs"`
	When     []string       `json:"when"`
	Merge    any            `json:"merge"`
	Strategy *struct {
		FanOut string `json:"fan_out"`
	} `json:"strategy"`
}

// workflowRead is a workflow's graph, read for the run whose workflow it is.
type workflowRead struct {
	key   string
	graph *flowGraph
	err   error
}

// workflowKey names a run's workflow, which the graph read is kept for.
func workflowKey(r *db.RunDetail) string { return r.Namespace + "/" + r.Workflow }

func (m Model) readWorkflow() tea.Cmd {
	if m.run == nil {
		return nil
	}
	return m.readWorkflowNamed(workflowKey(m.run))
}

// readWorkflowNamed reads the graph of a workflow named namespace/name.
func (m Model) readWorkflowNamed(key string) tea.Cmd {
	ns, name, _ := strings.Cut(key, "/")
	path := "/api/v1/" + url.PathEscape(ns) + "/workflows/" + url.PathEscape(name) + "?limit=1"
	return func() tea.Msg {
		var detail struct {
			Graph *flowGraph `json:"graph"`
		}
		if err := m.o.Read(m.ctx, path, &detail); err != nil {
			return workflowRead{key: key, err: err}
		}
		return workflowRead{key: key, graph: detail.Graph}
	}
}

// fedBy is the workflow input an input port's expression reads where it reads one and nothing else,
// which the graph draws as an edge from the workflow's inputs, as the web console does.
var fedBy = regexp.MustCompile(`^\s*\$\{\{\s*(?:workflow\.)?inputs\.([A-Za-z0-9_-]+)\s*\}\}\s*$`)

func inputOf(expression any) string {
	s, ok := expression.(string)
	if !ok {
		return ""
	}
	if m := fedBy.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// edge is one edge of the graph: a port of a step to a port of another, or to a workflow output,
// where to is empty and as names the output.
type edge struct {
	from, port, to, as string
}

// edgesFrom are the edges leaving a step, its ports in the order it declares them and each port's
// edges in the order the graph runs the steps they reach, then the workflow's outputs by name.
func (g *flowGraph) edgesFrom(step string) []edge {
	var out []edge
	s := g.Steps[step]
	ports := slices.Clone(s.Outputs)
	for _, other := range g.Order {
		for _, n := range g.Steps[other].Needs {
			if n.Step == step && !slices.Contains(ports, n.Port) {
				ports = append(ports, n.Port)
			}
		}
	}
	for _, port := range ports {
		for _, other := range g.Order {
			for _, n := range g.Steps[other].Needs {
				if n.Step == step && n.Port == port {
					out = append(out, edge{step, port, other, n.As})
				}
			}
		}
		var names []string
		for name, o := range g.Outputs {
			if o.From.Step == step && o.From.Port == port {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		for _, name := range names {
			out = append(out, edge{step, port, "", name})
		}
	}
	return out
}

// fedFromInputs are the workflow inputs each step reads, by step in running order.
func (g *flowGraph) fedFromInputs() [][2]string {
	var fed [][2]string
	for _, step := range g.Order {
		var names []string
		for port, expression := range g.Steps[step].Inputs {
			_ = port
			if name := inputOf(expression); name != "" && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			slices.Sort(names)
			fed = append(fed, [2]string{strings.Join(names, ", "), step})
		}
	}
	return fed
}

// stateOf is a step as the run laid over the graph leaves it: its verdict, how long it took, and a
// fan-out's shards done; nothing where no run is laid over it.
func (m Model) stateOf(step string) (agk.Verdict, string, bool) {
	if m.run == nil || string(m.run.Run) == "" {
		return 0, "", false
	}
	s := summaryOf(m.run, step)
	if s == nil {
		return agk.VerdictPending, "", true
	}
	about := took(s.StartedAt, s.FinishedAt, m.o.Now())
	tasks := lastAttempts(m.run, step)
	if of := shardsOf(tasks); of > 1 {
		about = strings.TrimSpace(about + fmt.Sprintf("  %d of %d shards done", doneOf(tasks), of))
	}
	return s.Verdict, about, true
}

// listLines is the graph written as a list, in the order it runs: the workflow inputs and the steps
// they feed, then each step with its state, and under it each edge it leaves, naming the port, the
// step or workflow output it reaches and that one's port, so that nothing is read off the
// direction of a line.
func (m Model) listLines(g *flowGraph) [][]part {
	width := len("inputs")
	for _, step := range g.Order {
		width = max(width, len([]rune(step)))
	}
	var lines [][]part
	for _, fed := range g.fedFromInputs() {
		lines = append(lines, []part{{muted, cellOf("inputs", width) + "  "}, {plain, fed[0]}, {quiet, "  -> "}, {plain, fed[1]}})
	}
	chosen := m.graphStep()
	for _, step := range g.Order {
		s := g.Steps[step]
		row := []part{{strong, cellOf(step, width) + "  "}}
		if verdict, about, ok := m.stateOf(step); ok {
			row = append(row, part{verdictRole(verdict), cellOf(verdict.String(), 10)})
			if about != "" {
				row = append(row, part{muted, " " + about})
			}
		}
		if s.Strategy != nil && s.Strategy.FanOut != "" {
			row = append(row, part{muted, "  fan_out: " + s.Strategy.FanOut})
		}
		if s.Merge != nil {
			if text, ok := s.Merge.(string); ok {
				row = append(row, part{muted, "  merge: " + text})
			}
		}
		if len(s.When) > 0 {
			row = append(row, part{muted, "  when: " + strings.Join(s.When, ", ")})
		}
		if step == chosen {
			row = append([]part{{strong, "▸ "}}, row...)
		} else {
			row = append([]part{{plain, "  "}}, row...)
		}
		lines = append(lines, row)
		ports := 0
		for _, e := range g.edgesFrom(step) {
			ports = max(ports, len([]rune(e.port)))
		}
		for _, e := range g.edgesFrom(step) {
			to, as := e.to, e.as
			if to == "" {
				to = "output"
			}
			lines = append(lines, []part{{plain, "    "}, {portRole(e.port), cellOf(e.port, max(ports, width-2))}, {quiet, "  -> "}, {plain, cellOf(to, width)}, {muted, "  " + as}})
		}
	}
	return lines
}

// portRole is the colour an edge is drawn in: rejected in the waiting amber, items a step refused
// and passed on rather than a failure; error in the failed red; every other port muted.
func portRole(port string) role {
	switch port {
	case "rejected":
		return waitingText
	case "error":
		return failedText
	}
	return muted
}

// graphStep is the step chosen in the graph, the inspector's own.
func (m Model) graphStep() string {
	if m.graph == nil || len(m.graph.Order) == 0 {
		return ""
	}
	if m.step != "" && slices.Contains(m.graph.Order, m.step) {
		return m.step
	}
	if m.run != nil {
		if s := stepOf(m.run, ""); slices.Contains(m.graph.Order, s) {
			return s
		}
	}
	return m.graph.Order[0]
}

// graphLines is the graph view: a header naming the graph, then the graph drawn, or written as a
// list.
func (m Model) graphLines(t theme, height int) []string {
	line := func(parts ...part) string { return t.line(false, m.width, within(parts, m.width)...) }
	if m.run == nil && m.graphOf == "" {
		return []string{line(part{quiet, "Reading the run."})}
	}
	name := m.graphOf
	if m.run != nil {
		name = m.run.Namespace + "/" + m.run.Workflow
	}
	header := []part{{strong, "Graph "}, {plain, name}}
	switch {
	case m.graphFailed != "":
		return []string{line(header...), line(), line(part{failedText, "The graph could not be read: " + m.graphFailed})}
	case m.graph == nil:
		return []string{line(header...), line(), line(part{quiet, "Reading the graph."})}
	}
	header = append(header, part{plain, "@" + short(m.graph.Commit) + "  "})
	if m.run != nil {
		header = append(header, part{muted, "run " + string(m.run.Run) + "  "}, part{stateRole(m.run.State), m.mark(m.run.State == agk.Running)}, part{plain, " " + m.run.State.String()})
	} else {
		header = append(header, part{muted, "the version the default branch's head resolves to, with no run laid over it"})
	}
	lines := []string{line(header...)}
	if m.run != nil && m.run.Commit != "" && m.graph.Commit != "" && m.run.Commit != m.graph.Commit {
		// The API resolves the head's graph alone, as the web console says of a run of an older
		// version laid over it.
		lines = append(lines, line(part{waitingText, "The run is of " + short(m.run.Commit) + ", drawn on the graph of the default branch's head, " + short(m.graph.Commit) + "."}))
	}
	lines = append(lines, line())
	if !m.asList {
		return append(lines, m.drawnLines(t, m.layout(m.graph), m.width, height-len(lines))...)
	}
	for _, l := range m.listLines(m.graph) {
		lines = append(lines, line(l...))
	}
	return lines
}
