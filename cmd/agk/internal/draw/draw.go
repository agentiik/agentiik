package draw

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/graph"

	"github.com/agentiik/agentiik/agk"
)

// What the two writers both read. A drawing is the same reading of the same workflow
// twice over, and the only difference between DOT and Mermaid is what the bytes look like,
// so the reading sits here and each writer is a writer.

// node is one step as it is drawn: its name, the few keywords that belong inside the box,
// and the ports on each side of it.
type node struct {
	Name agk.Step
	Rows []string
	In   []agk.Port
	Out  []agk.Port
}

// link is one edge of the graph, anchored from the port it leaves by to the port it feeds.
type link struct {
	From     agk.Step
	FromPort agk.Port
	To       agk.Step
	ToPort   agk.Port
}

// bound is one end of the workflow's own boundary: a declared input feeding a step port,
// or a step port a declared output is a view of.
type bound struct {
	Name string
	Step agk.Step
	Port agk.Port
	// Into says which way it points: an input feeds a port, and an output is fed by
	// one.
	Into bool
}

// drawing is the whole of what either writer needs.
type drawing struct {
	Title   string
	Nodes   []node
	Links   []link
	Inputs  []bound
	Outputs []bound
}

// read turns a resolved workflow into what is drawn.
//
// Steps and ports come out in sorted order, which is not tidiness: two runs of agk graph
// over one file have to produce identical bytes, so that a diff of two drawings is a diff
// of the graph and a drawing can be committed beside the file it describes. Edges keep the
// declaration order they were written in, inside the sorted step that consumes them,
// because that order is itself part of what the file says: wait_all concatenates in edge
// declaration order, and sorting them would draw an order the run does not take.
func read(wf *graph.Workflow) (*drawing, error) {
	if wf == nil {
		return nil, fmt.Errorf("there is no workflow to draw")
	}
	d := &drawing{Title: title(wf)}

	for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
		st := wf.Steps[name]
		d.Nodes = append(d.Nodes, node{
			Name: name,
			Rows: rows(st),
			In:   inPorts(st),
			Out:  slices.Sorted(slices.Values(st.Outputs)),
		})
		for _, e := range st.Needs {
			d.Links = append(d.Links, link{From: e.Step, FromPort: e.Port, To: name, ToPort: e.As})
		}
	}

	for _, in := range slices.Sorted(maps.Keys(wf.Inputs)) {
		d.Inputs = append(d.Inputs, feeds(wf, in)...)
	}
	for _, out := range slices.Sorted(maps.Keys(wf.Outputs)) {
		from := wf.Outputs[out].From
		d.Outputs = append(d.Outputs, bound{Name: out, Step: from.Step, Port: from.Port})
	}
	return d, nil
}

// title is what the drawing is called: the reference a run and a push both use.
func title(wf *graph.Workflow) string {
	switch {
	case wf.Metadata.Namespace != "" && wf.Metadata.Name != "":
		return wf.Metadata.Namespace + "/" + wf.Metadata.Name
	case wf.Metadata.Name != "":
		return wf.Metadata.Name
	default:
		return "workflow"
	}
}

// inPorts are the ports a step is fed on, from either of the two ways a port is fed: the
// inputs keyword, which feeds one from a workflow input or an expression with no
// dependency on another step, and the as of an edge, which is the port an upstream output
// arrives on.
func inPorts(st graph.Step) []agk.Port {
	ports := slices.Sorted(maps.Keys(st.Inputs))
	for _, e := range st.Needs {
		if !slices.Contains(ports, e.As) {
			ports = append(ports, e.As)
		}
	}
	slices.Sort(ports)
	return ports
}

// rows are the few keywords drawn inside the box rather than beside it.
//
// Each is a property of one step, and a badge beside a box is a thing a reader has to
// match up. What it runs comes first, because it is the one fact a reviewer of a merge
// request looks for; then if, then merge where it decides something, then fan_out.
func rows(st graph.Step) []string {
	var rows []string
	switch {
	case st.Call != nil:
		calls := "calls " + st.Call.Workflow
		if st.Call.Ref != "" {
			calls += "@" + st.Call.Ref
		}
		rows = append(rows, calls)
	case len(st.Script) > 0:
		// The image of a script step is a base image and no manifest is read of it,
		// so what the step is is the script and the image is where it runs.
		rows = append(rows, fmt.Sprintf("script on %s", st.Image))
	default:
		rows = append(rows, st.Image)
	}
	if st.If != "" {
		rows = append(rows, "if: "+st.If)
	}
	// merge is drawn where it decides something, which is a port fed by more than one
	// edge, or where the file wrote something other than the default it would have had
	// anyway.
	if st.Merge != graph.MergeWaitAll || fedTwice(st) {
		merge := "merge: " + st.Merge.String()
		if st.Merge == graph.MergeJoin && st.Join.On != "" {
			merge += " on " + st.Join.On
		}
		rows = append(rows, merge)
	}
	if st.Strategy.FanOut != graph.FanOutNone {
		rows = append(rows, "fan_out: "+fanOut(st.Strategy))
	}
	return rows
}

// fanOut spells the strategy the way the file writes it, batch(n) with the size the step
// asked for rather than with the letter n.
func fanOut(s graph.Strategy) string {
	if s.FanOut == graph.FanOutBatch {
		return fmt.Sprintf("batch(%d)", s.Batch)
	}
	return s.FanOut.String()
}

// fedTwice says whether any one input port of the step is fed by more than one edge, which
// is the case a merge strategy is about.
func fedTwice(st graph.Step) bool {
	seen := map[agk.Port]int{}
	for _, e := range st.Needs {
		seen[e.As]++
		if seen[e.As] > 1 {
			return true
		}
	}
	return false
}

// feeds says which step ports one workflow input reaches.
//
// It is read out of the text of the inputs keyword rather than out of an evaluation: this
// package compiles no expression and holds no context, and a drawing for a merge request
// is produced where there is no run to evaluate anything against. So an input is drawn
// reaching a port when the expression on that port names it, which is what ${{
// workflow.inputs.orders }} says in the only way a reader of the file can check.
func feeds(wf *graph.Workflow, input string) []bound {
	var found []bound
	needle := "workflow.inputs." + input
	for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
		st := wf.Steps[name]
		for _, port := range slices.Sorted(maps.Keys(st.Inputs)) {
			if !names(st.Inputs[port], needle) {
				continue
			}
			found = append(found, bound{Name: input, Step: name, Port: port, Into: true})
		}
	}
	return found
}

// names says whether a value written on a port mentions one workflow input anywhere inside
// it, a list and a block included, since a port can be fed a structure of expressions.
func names(v any, needle string) bool {
	switch value := v.(type) {
	case string:
		return strings.Contains(value, needle)
	case map[string]any:
		for _, sub := range value {
			if names(sub, needle) {
				return true
			}
		}
	case []any:
		for _, sub := range value {
			if names(sub, needle) {
				return true
			}
		}
	}
	return false
}
