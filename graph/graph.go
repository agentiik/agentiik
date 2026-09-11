package graph

import (
	"fmt"
	"maps"
	"slices"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/brick"
)

// Graph is the resolved graph: the steps of a workflow, the edges between their ports,
// and an order that no cycle can be read out of.
//
// It is built once, when a workflow is registered or a run starts, and read many times
// while the run goes. Nothing in it changes as a run proceeds: what changes is the State,
// and the two are separate so that one graph can serve every run of a version.
type Graph struct {
	wf    *Workflow
	order []agk.Step

	// consumers is the graph read the other way round: which steps take a given port of
	// a given step. merge: first needs it to say whether a step it abandoned is still
	// needed, and walking every step's needs to answer that would be the same walk done
	// once per question.
	consumers map[agk.Step]map[agk.Port][]agk.Step

	// manifests are the ones Build was given, kept because the same schemas answer for
	// a parameter twice: once here, for what the file already settles, and once when a
	// task is built, for what an expression resolved to. They are not exported, because
	// what a caller has is what it handed in.
	manifests map[string]brick.Manifest
}

// Images names the image of every step whose manifest has to be read, so that a caller
// can fetch them between Check and Build.
//
// A script step is not among them, and that is the rule rather than an omission: "the
// image is treated as a base image and nothing about its ports is inferred", so no
// manifest is read for it and asking a registry for one would refuse a workflow the
// language accepts. A sub-workflow call has no image at all.
//
// The list is the distinct references, in order, so that a caller fetching them does it
// once per image and not once per step.
func Images(wf *Workflow) []string {
	if wf == nil {
		return nil
	}
	var images []string
	for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
		st := wf.Steps[name]
		if st.Image == "" || len(st.Script) > 0 {
			continue
		}
		if !slices.Contains(images, st.Image) {
			images = append(images, st.Image)
		}
	}
	return images
}

// Build resolves a checked workflow into the graph a run is evaluated against, holding
// every step to the manifest of the brick it runs.
//
// The manifests arrive as an argument, keyed by the image reference Images named, because
// "reading /agk/brick.yaml means pulling an image, and pulling an image is executing".
// The rules they answer for are the two the documentation states about a step and its
// brick, that its outputs are a subset of the manifest's ports and that its params
// satisfy the manifest's schemas, and neither can be read out of the workflow file alone.
//
// Check runs first, here rather than only in the caller: a graph built out of a workflow
// whose edges name steps that do not exist is not a graph, and letting Build be reached
// without it would make the order of two calls a rule somebody has to remember.
func Build(wf *Workflow, manifests map[string]brick.Manifest) (*Graph, error) {
	if err := Check(wf); err != nil {
		return nil, err
	}
	if !wf.resolved && len(wf.Include) > 0 {
		return nil, fmt.Errorf("graph: the workflow declares includes that were never resolved: it was parsed and not loaded, so the blocks its steps extend are not in hand")
	}
	for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
		if err := checkAgainstManifest(name, wf.Steps[name], manifests); err != nil {
			return nil, err
		}
	}

	order, err := topological(wf)
	if err != nil {
		return nil, err
	}
	g := &Graph{
		wf:        wf,
		order:     order,
		consumers: map[agk.Step]map[agk.Port][]agk.Step{},
		manifests: maps.Clone(manifests),
	}
	for _, name := range order {
		for _, e := range wf.Steps[name].Needs {
			ports, ok := g.consumers[e.Step]
			if !ok {
				ports = map[agk.Port][]agk.Step{}
				g.consumers[e.Step] = ports
			}
			if !slices.Contains(ports[e.Port], name) {
				ports[e.Port] = append(ports[e.Port], name)
			}
		}
	}
	return g, nil
}

// topological orders the steps so that a step comes after everything it needs.
//
// The order is deterministic: among the steps whose edges have all been satisfied, the
// first in name order is taken. A run is not scheduled by this order, since "a step
// becomes runnable once every declared input port is satisfied" and that is read from the
// state; what it is for is everything that reads a graph rather than a run, a console
// drawing it, a plan printed before a run, a test comparing two builds of one file.
func topological(wf *Workflow) ([]agk.Step, error) {
	waiting := make(map[agk.Step]int, len(wf.Steps))
	for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
		seen := map[agk.Step]bool{}
		for _, e := range wf.Steps[name].Needs {
			if e.Step == name || seen[e.Step] {
				continue
			}
			seen[e.Step] = true
			waiting[name]++
		}
	}

	ready := make([]agk.Step, 0, len(wf.Steps))
	for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
		if waiting[name] == 0 {
			ready = append(ready, name)
		}
	}

	order := make([]agk.Step, 0, len(wf.Steps))
	for len(ready) > 0 {
		slices.Sort(ready)
		name := ready[0]
		ready = ready[1:]
		order = append(order, name)

		for _, downstream := range slices.Sorted(maps.Keys(wf.Steps)) {
			for _, e := range distinctSources(wf.Steps[downstream]) {
				if e != name {
					continue
				}
				waiting[downstream]--
				if waiting[downstream] == 0 {
					ready = append(ready, downstream)
				}
			}
		}
	}
	if len(order) != len(wf.Steps) {
		// Check has already refused every cycle, so reaching this is a graph that
		// changed underneath the check rather than a workflow the language accepts.
		return nil, fmt.Errorf("graph: the steps cannot be ordered: %d of %d were reached, which means a cycle the check did not see", len(order), len(wf.Steps))
	}
	return order, nil
}

// distinctSources is the steps one step needs, each named once however many of its ports
// are taken.
func distinctSources(st Step) []agk.Step {
	var out []agk.Step
	for _, e := range st.Needs {
		if !slices.Contains(out, e.Step) {
			out = append(out, e.Step)
		}
	}
	return out
}

// Steps are the steps of the workflow, in name order.
func (g *Graph) Steps() []agk.Step { return slices.Sorted(maps.Keys(g.wf.Steps)) }

// Order is the steps in an order no edge points backwards through.
func (g *Graph) Order() []agk.Step { return slices.Clone(g.order) }

// Step is one step of the graph, resolved. It is returned by pointer because the
// evaluator reads it once per shard per evaluation and copying a step to answer a
// question about it is a copy nothing needed.
func (g *Graph) Step(name agk.Step) (*Step, bool) {
	st, ok := g.wf.Steps[name]
	if !ok {
		return nil, false
	}
	return &st, true
}

// Edges are the inbound edges of a step, in the order the file declares them. The order
// is load bearing: "wait_all waits for every upstream port, then concatenates items in
// edge declaration order".
func (g *Graph) Edges(name agk.Step) []Edge {
	st, ok := g.wf.Steps[name]
	if !ok {
		return nil
	}
	return slices.Clone(st.Needs)
}

// Consumers are the steps that take one port of one step, in the order they were found.
// A workflow output is a consumer too and is not among them: it is not a step, and it is
// read off the workflow.
func (g *Graph) Consumers(name agk.Step, port agk.Port) []agk.Step {
	return slices.Clone(g.consumers[name][port])
}

// published names every port of a step that something takes: the ports the step declares,
// the ports an edge names on it, and the ports a workflow output takes from it.
//
// For a step that runs a brick the first of those is already all of them, since an edge
// cannot invent a port on a step and a workflow output is a view of one the step
// publishes. A sub-workflow call declares none at all, because "its ports are the declared
// outputs of the workflow it calls, which this commit does not carry", so what is taken
// from it is the only thing there is to read, and a rule that read outputs alone would
// conclude that nothing is waiting on any call in the file.
//
// The order is the step's own outputs first and then name order, so that a rule reading
// this reads the same list twice.
func (g *Graph) published(name agk.Step) []agk.Port {
	st, ok := g.wf.Steps[name]
	if !ok {
		return nil
	}
	ports := slices.Clone(st.Outputs)
	add := func(port agk.Port) {
		if !slices.Contains(ports, port) {
			ports = append(ports, port)
		}
	}
	for _, port := range slices.Sorted(maps.Keys(g.consumers[name])) {
		add(port)
	}
	for _, out := range slices.Sorted(maps.Keys(g.wf.Outputs)) {
		if from := g.wf.Outputs[out].From; from.Step == name {
			add(from.Port)
		}
	}
	return ports
}

// Workflow is the file the graph was built from. It is the same value the caller parsed,
// not a copy: a graph is a reading of a workflow and never a second one.
func (g *Graph) Workflow() *Workflow { return g.wf }

// manifest is the brick manifest of a step, for the second reading of a parameter: the
// one that happens when a task is built and every expression has a value. A step with no
// manifest is a script step or a sub-workflow call, and there is nothing to read.
func (g *Graph) manifest(name agk.Step) (brick.Manifest, bool) {
	st, ok := g.wf.Steps[name]
	if !ok || st.Image == "" || len(st.Script) > 0 {
		return brick.Manifest{}, false
	}
	m, ok := g.manifests[st.Image]
	return m, ok
}
