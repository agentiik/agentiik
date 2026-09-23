package graph

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/agk"
)

// Check applies every rule of the language that is about the file as a whole: the rules
// a reader of one block cannot see, and that no JSON Schema can express, because they
// need the graph, the workflow's own boundary or its published surface.
//
// It is the second of the three layers. Parse has already refused everything about the
// shape of the document; Build refuses what only a brick manifest can answer for. A
// workflow that parses is not yet a workflow that holds together, and a workflow that
// Check accepts is one whose graph is a graph: every edge names a step that exists and a
// port that step declares, and no path through it comes back to where it started.
//
// Every refusal here is a *Refusal naming the step, the port and the rule in the
// documentation's own words, so that a person holding the file can find the line. The
// first refusal is returned: a workflow is refused, not annotated.
func Check(wf *Workflow) error {
	if wf == nil {
		return fmt.Errorf("there is no workflow to check")
	}
	for _, check := range []func(*Workflow) error{
		checkSteps,
		checkEdges,
		checkCycles,
		checkWorkflowOutputs,
		checkMCP,
		// The expression rules belong to this list too. They are checked where the
		// scopes are known, because which roots a keyword may read depends on where
		// the keyword sits and on the step's own fan_out.
		checkExpressions,
	} {
		if err := check(wf); err != nil {
			return err
		}
	}
	return nil
}

// checkSteps applies the rules about one step that need nothing but the file: that it
// runs something, that a script step declares its ports, and that the secrets it mounts
// are secrets this workflow names.
func checkSteps(wf *Workflow) error {
	for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
		st := wf.Steps[name]

		if st.Image == "" && st.Call == nil {
			if len(st.Script) > 0 {
				return fmt.Errorf("graph: step %s: the step carries script and no image: the commands run inside image, under exactly the contract every brick honours, and what changes is only that the image is a base image rather than a published brick", name)
			}
			return fmt.Errorf("graph: step %s: the step runs nothing: a step runs an image, or calls a sub-workflow with workflow: <namespace>/<name>", name)
		}
		if len(st.Script) > 0 && len(st.Outputs) == 0 {
			return refuse(RuleScriptWithoutOutputs, name, "", "the image is a base image, no manifest is read and nothing about its ports is inferred, so outputs must be declared")
		}
		// A matrix fan-out is "the cartesian product of variable lists", and "every
		// combination is a shard". A step that names the strategy and declares no
		// list is a product of nothing, which is the same thing the reader already
		// refuses one variable for; left alone it runs as a single shard carrying an
		// empty combination, so ${{ matrix.region }} beside it resolves to nothing
		// inside a container rather than being refused before the push.
		if st.Strategy.FanOut == FanOutMatrix && len(st.Strategy.Matrix) == 0 {
			return fmt.Errorf("graph: step %s: strategy.fan_out is matrix and the step declares no matrix: a matrix fan-out is the cartesian product of variable lists, every combination is a shard, and a product of no list is a shard of nothing", name)
		}
		for _, secret := range st.Secrets {
			if !slices.Contains(wf.Secrets, secret) {
				return refuse(RuleSecretNotDeclared, name, "", fmt.Sprintf("the step mounts %s, which the secrets block does not name: secrets are mounted by the name the secrets block lists them under, and only secrets the owning namespace declares can be named there", secret))
			}
		}
	}
	return nil
}

// checkEdges holds every edge to the two things it names. "An edge connects one output
// port of a step to one input port of another. That is the only form of dependency."
func checkEdges(wf *Workflow) error {
	for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
		for _, e := range wf.Steps[name].Needs {
			from, ok := wf.Steps[e.Step]
			if !ok {
				return refuse(RuleNeedsUnknownStep, name, e.Port, fmt.Sprintf("the edge comes from %s, which the workflow does not declare: an edge is the only form of dependency there is, so it has to name something that exists", e.Step))
			}
			if !publishes(from, e.Port) {
				return refuse(RuleEdgePortNotDeclared, name, e.Port, fmt.Sprintf("the edge takes %s from %s, which publishes %s: an edge cannot invent a port on a step", e.Port, e.Step, whatItPublishes(from)))
			}
		}
	}
	return nil
}

// publishes says whether a step publishes the port an edge takes.
//
// A sub-workflow call is the one step this is not asked of. Its ports are the declared
// outputs of the workflow it calls, which this commit does not carry and this package
// never fetches, so the edge is checked against the callee when the call is resolved and
// not here. Reading the absence of outputs on a call as an undeclared port would refuse
// every workflow that calls another, which the language writes out as a step keyword.
func publishes(st Step, port agk.Port) bool {
	return st.Call != nil || slices.Contains(st.Outputs, port)
}

// whatItPublishes writes what a step does declare, so that a refusal about a port shows
// the ports beside it rather than sending a reader back to the file to find them.
func whatItPublishes(st Step) string {
	switch len(st.Outputs) {
	case 0:
		return "no output port"
	case 1:
		return "the output port " + portList(st.Outputs)
	default:
		return "the output ports " + portList(st.Outputs)
	}
}

// checkCycles refuses a graph that comes back to where it started. "Cycles are rejected
// at validation, before registration. Looping is done by calling a sub-workflow, with a
// configurable maximum depth."
//
// The refusal names the cycle, in the order a reader would follow it, because a workflow
// of forty steps with one cycle in it is a file nobody can read the cycle out of.
func checkCycles(wf *Workflow) error {
	const (
		open   = 1
		closed = 2
	)
	state := make(map[agk.Step]int, len(wf.Steps))
	var path []agk.Step

	var walk func(agk.Step) error
	walk = func(name agk.Step) error {
		state[name] = open
		path = append(path, name)
		for _, e := range edgesOf(wf, name) {
			if _, ok := wf.Steps[e.Step]; !ok {
				continue
			}
			switch state[e.Step] {
			case open:
				at := slices.Index(path, e.Step)
				cycle := append(slices.Clone(path[at:]), e.Step)
				return refuse(RuleCycleInGraph, name, "", fmt.Sprintf("the graph comes back to where it started, %s: cycles are rejected at validation, before registration, and looping is done by calling a sub-workflow", stepList(cycle)))
			case closed:
				continue
			}
			if err := walk(e.Step); err != nil {
				return err
			}
		}
		path = path[:len(path)-1]
		state[name] = closed
		return nil
	}

	for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
		if state[name] != 0 {
			continue
		}
		if err := walk(name); err != nil {
			return err
		}
	}
	return nil
}

// checkWorkflowOutputs holds a workflow output to the step port it is a view of.
func checkWorkflowOutputs(wf *Workflow) error {
	for _, name := range slices.Sorted(maps.Keys(wf.Outputs)) {
		out := wf.Outputs[name]
		st, ok := wf.Steps[out.From.Step]
		if !ok {
			return refuse(RuleOutputFromUnknownStep, out.From.Step, out.From.Port, fmt.Sprintf("the output %s is taken from %s, which the workflow does not declare", name, out.From.Step))
		}
		if !publishes(st, out.From.Port) {
			return refuse(RuleEdgePortNotDeclared, out.From.Step, out.From.Port, fmt.Sprintf("the output %s is taken from the port %s of %s, which publishes %s: an output is a view of one step port, and a port nobody publishes is a view of nothing", name, out.From.Port, out.From.Step, whatItPublishes(st)))
		}
	}
	return nil
}

// edgesOf is the inbound edges of a step, which is where the order of evaluation comes
// from: an edge points at what has to have ended before this step starts.
func edgesOf(wf *Workflow, name agk.Step) []Edge {
	st, ok := wf.Steps[name]
	if !ok {
		return nil
	}
	return st.Needs
}

// stepList and portList write a list of names the way a refusal reads it aloud.
func stepList(steps []agk.Step) string {
	names := make([]string, 0, len(steps))
	for _, s := range steps {
		names = append(names, string(s))
	}
	return strings.Join(names, " -> ")
}

func portList(ports []agk.Port) string {
	names := make([]string, 0, len(ports))
	for _, p := range ports {
		names = append(names, string(p))
	}
	return strings.Join(names, ", ")
}
