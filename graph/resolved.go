package graph

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/agentiik/agentiik/agk"
)

// Resolved writes the graph down as wire.schema.json's resolvedGraph: what a version's hook
// accepted, as the engine resolves it for every run of the version, what the API answers and
// what the console draws.
//
// It is a record rather than a file. Every keyword a step carries is the one resolution left,
// includes, extends, defaults and the step's own values applied in the language's order, and
// it is written only where it is not the language's default, so that two workflows resolving
// to the same steps read the same, whichever layers they took to get there. idempotent is the
// exception, always written, and so is shell on a script step. The script keywords a defaults
// block writes reach a script step alone, since "a brick runs its own entry point and a call
// runs no container, whatever the defaults write", and a record claiming a brick runs a
// before_script would be describing something that never happens.
//
// It names steps, images, ports and edges, never a value a run was given: an expression is kept
// as written, for a run to evaluate. And it writes no namespace, since a namespace is the
// repository's and not part of what a commit resolves to: moving the repository moves nothing
// here.
//
// commit is the commit the graph was resolved from, which the graph cannot know: it was built
// from a tree.
func (g *Graph) Resolved(commit string) ([]byte, error) {
	wf := g.wf
	r := resolvedGraph{
		Workflow: wf.Metadata.Name,
		Commit:   commit,
		Includes: []resolvedInclude{},
		Order:    g.order,
		Steps:    make(map[agk.Step]resolvedStep, len(wf.Steps)),
	}
	for _, in := range wf.included {
		if in.Path != "" {
			r.Includes = append(r.Includes, resolvedInclude{Path: in.Path})
			continue
		}
		r.Includes = append(r.Includes, resolvedInclude{Workflow: in.Workflow.Namespace + "/" + in.Workflow.Name, Ref: in.Workflow.Ref, Commit: in.Commit})
	}
	// The entry point's own blocks, as it wrote them: nothing resolves them, and the
	// schemas they are held to are the workflow schema's.
	r.Inputs, r.On, r.MCP, r.Concurrency = wf.root["inputs"], wf.root["on"], wf.root["mcp"], wf.root["concurrency"]
	if len(wf.Outputs) > 0 {
		r.Outputs = make(map[string]resolvedOutput, len(wf.Outputs))
		for name, out := range wf.Outputs {
			o := resolvedOutput{Schema: out.Schema, Retain: retainOf(out.Retain)}
			o.From.Step, o.From.Port = out.From.Step, out.From.Port
			r.Outputs[name] = o
		}
	}
	if len(wf.Vars) > 0 {
		r.Vars = wf.Vars
	}
	r.Secrets = wf.Secrets
	if wf.Timeout != 0 {
		r.Timeout = wf.Timeout.String()
	}
	if wf.Defaults.Retain != nil && wf.Defaults.Retain.For != 0 {
		r.Retain = wf.Defaults.Retain.For.String()
	}
	for name, st := range wf.Steps {
		s, err := g.resolvedStep(name, st)
		if err != nil {
			return nil, err
		}
		r.Steps[name] = s
	}
	return json.Marshal(r)
}

// resolvedStep is one step as the record writes it.
func (g *Graph) resolvedStep(name agk.Step, st Step) (resolvedStep, error) {
	s := resolvedStep{
		Image:      st.Image,
		Inputs:     st.Inputs,
		Outputs:    st.Outputs,
		Params:     st.Params,
		If:         st.If,
		RunsOn:     st.RunsOn,
		Secrets:    st.Secrets,
		Cache:      st.Cache,
		Idempotent: st.Idempotent,
		Extends:    st.Extends,
		// continue_on_error is written where it is true and never where it is not, the
		// language's default.
		ContinueOnError: st.ContinueOnError,
	}
	switch {
	case st.Call != nil:
		s.Kind = "workflow"
		s.Workflow = &resolvedCall{Workflow: st.Call.Workflow, Ref: st.Call.Ref}
	case len(st.Script) > 0:
		s.Kind = "script"
		// shell is written on every script step, as resolution leaves it: the step's, or
		// the language's ["/bin/sh", "-e"], which materialise writes in.
		s.Script, s.BeforeScript, s.AfterScript, s.Shell = st.Script, st.BeforeScript, st.AfterScript, st.Shell
	default:
		s.Kind = "brick"
		brick, version, ok := g.Brick(name)
		if !ok {
			return resolvedStep{}, fmt.Errorf("graph: step %s runs a brick whose manifest the graph was not built with, and the record names the release it was held to", name)
		}
		s.Brick = &resolvedBrick{Name: brick, Version: version}
	}
	for _, e := range st.Needs {
		s.Needs = append(s.Needs, resolvedEdge{Step: e.Step, Port: e.Port, As: e.As})
	}
	if !slices.Equal(st.When, []When{WhenSucceeded}) {
		for _, w := range st.When {
			s.When = append(s.When, w.String())
		}
	}
	switch st.Merge {
	case MergeZip, MergeFirst:
		s.Merge = st.Merge.String()
	case MergeJoin:
		s.Merge = map[string]any{"join": map[string]any{"on": st.Join.On}}
	}
	if st.Strategy.FanOut != FanOutNone || st.Strategy.MaxParallel != 0 || len(st.Strategy.Matrix) > 0 || st.Strategy.FailFast {
		strategy := &resolvedStrategy{MaxParallel: st.Strategy.MaxParallel, Matrix: st.Strategy.Matrix, FailFast: st.Strategy.FailFast}
		switch st.Strategy.FanOut {
		case FanOutItem, FanOutMatrix:
			strategy.FanOut = st.Strategy.FanOut.String()
		case FanOutBatch:
			strategy.FanOut = fmt.Sprintf("batch(%d)", st.Strategy.Batch)
		}
		s.Strategy = strategy
	}
	if st.Retry.Max != 0 || len(st.Retry.On) > 0 || st.Retry.Backoff.Base != 0 || st.Retry.Backoff.Max != 0 {
		retry := &resolvedRetry{Max: st.Retry.Max}
		for _, f := range st.Retry.On {
			retry.On = append(retry.On, f.String())
		}
		if b := st.Retry.Backoff; b.Base != 0 || b.Max != 0 {
			retry.Backoff = &resolvedBackoff{Type: b.Type.String(), Base: durationText(b.Base), Max: durationText(b.Max)}
		}
		s.Retry = retry
	}
	s.Timeout = durationText(st.Timeout)
	s.Retain = durationText(st.Retain.For)
	if st.Resources != (Resources{}) {
		s.Resources = &resolvedResources{CPU: st.Resources.CPU, Memory: st.Resources.Memory, PIDs: st.Resources.PIDs}
	}
	if st.Network != NetworkNone {
		s.Network = st.Network.String()
	}
	if st.EgressAllow != nil {
		s.Egress = &resolvedEgress{Allow: st.EgressAllow}
	}
	for _, f := range st.Files {
		s.Files = append(s.Files, resolvedFile(f))
	}
	return s, nil
}

// durationText is a duration as the file writes it, and nothing for none.
func durationText(d Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}

// retainOf is a workflow output's retention in the long form, whichever form the file wrote, and
// nothing where it wrote none.
func retainOf(r Retain) *resolvedRetain {
	if r.For == 0 {
		return nil
	}
	return &resolvedRetain{For: r.For.String(), Fetches: r.Fetches}
}

// The record's shapes. Unexported, since what a caller holds is the document: the wire's schema
// is the definition, and a Go type beside it would be a second one to keep in step.
type (
	resolvedGraph struct {
		Workflow    string                    `json:"workflow"`
		Commit      string                    `json:"commit"`
		Includes    []resolvedInclude         `json:"includes"`
		Inputs      any                       `json:"inputs,omitempty"`
		Outputs     map[string]resolvedOutput `json:"outputs,omitempty"`
		On          any                       `json:"on,omitempty"`
		MCP         any                       `json:"mcp,omitempty"`
		Vars        Vars                      `json:"vars,omitempty"`
		Secrets     []string                  `json:"secrets,omitempty"`
		Concurrency any                       `json:"concurrency,omitempty"`
		Timeout     string                    `json:"timeout,omitempty"`
		Retain      string                    `json:"retain,omitempty"`
		Order       []agk.Step                `json:"order"`
		Steps       map[agk.Step]resolvedStep `json:"steps"`
	}
	resolvedInclude struct {
		Path     string `json:"path,omitempty"`
		Workflow string `json:"workflow,omitempty"`
		Ref      string `json:"ref,omitempty"`
		Commit   string `json:"commit,omitempty"`
	}
	resolvedOutput struct {
		From struct {
			Step agk.Step `json:"step"`
			Port agk.Port `json:"port"`
		} `json:"from"`
		Schema json.RawMessage `json:"schema,omitempty"`
		Retain *resolvedRetain `json:"retain,omitempty"`
	}
	resolvedRetain struct {
		For     string `json:"for"`
		Fetches int    `json:"fetches,omitempty"`
	}
	resolvedStep struct {
		Kind            string             `json:"kind"`
		Image           string             `json:"image,omitempty"`
		Brick           *resolvedBrick     `json:"brick,omitempty"`
		Workflow        *resolvedCall      `json:"workflow,omitempty"`
		Needs           []resolvedEdge     `json:"needs,omitempty"`
		Inputs          map[agk.Port]any   `json:"inputs,omitempty"`
		Outputs         []agk.Port         `json:"outputs,omitempty"`
		Params          map[string]any     `json:"params,omitempty"`
		If              string             `json:"if,omitempty"`
		When            []string           `json:"when,omitempty"`
		Merge           any                `json:"merge,omitempty"`
		Strategy        *resolvedStrategy  `json:"strategy,omitempty"`
		Retry           *resolvedRetry     `json:"retry,omitempty"`
		Timeout         string             `json:"timeout,omitempty"`
		Retain          string             `json:"retain,omitempty"`
		ContinueOnError bool               `json:"continue_on_error,omitempty"`
		Resources       *resolvedResources `json:"resources,omitempty"`
		Network         string             `json:"network,omitempty"`
		Egress          *resolvedEgress    `json:"egress,omitempty"`
		RunsOn          []string           `json:"runs_on,omitempty"`
		Secrets         []string           `json:"secrets,omitempty"`
		Cache           bool               `json:"cache,omitempty"`
		Idempotent      bool               `json:"idempotent"`
		Files           []resolvedFile     `json:"files,omitempty"`
		Script          []string           `json:"script,omitempty"`
		BeforeScript    []string           `json:"before_script,omitempty"`
		AfterScript     []string           `json:"after_script,omitempty"`
		Shell           []string           `json:"shell,omitempty"`
		Extends         []string           `json:"extends,omitempty"`
	}
	resolvedBrick struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	resolvedCall struct {
		Workflow string `json:"workflow"`
		Ref      string `json:"ref,omitempty"`
	}
	resolvedEdge struct {
		Step agk.Step `json:"step"`
		Port agk.Port `json:"port"`
		As   agk.Port `json:"as"`
	}
	resolvedStrategy struct {
		FanOut      string           `json:"fan_out,omitempty"`
		MaxParallel int              `json:"max_parallel,omitempty"`
		Matrix      map[string][]any `json:"matrix,omitempty"`
		FailFast    bool             `json:"fail_fast,omitempty"`
	}
	resolvedRetry struct {
		Max     int              `json:"max"`
		On      []string         `json:"on,omitempty"`
		Backoff *resolvedBackoff `json:"backoff,omitempty"`
	}
	resolvedBackoff struct {
		Type string `json:"type"`
		Base string `json:"base,omitempty"`
		Max  string `json:"max,omitempty"`
	}
	resolvedResources struct {
		CPU    string `json:"cpu,omitempty"`
		Memory string `json:"memory,omitempty"`
		PIDs   int    `json:"pids,omitempty"`
	}
	resolvedEgress struct {
		Allow []string `json:"allow"`
	}
	resolvedFile struct {
		From string `json:"from"`
		To   string `json:"to,omitempty"`
		Mode string `json:"mode,omitempty"`
	}
)
