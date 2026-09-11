package graph

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/agentiik/agentiik/agk"
)

// The keys of the entry point, of a step and of defaults. They are written out because
// the file is closed: "every block the language owns refuses a key it does not
// recognise", so a misspelling is found where it was written rather than quietly doing
// nothing three hours into a run.
var (
	rootKeys = []string{
		"apiVersion", "kind", "metadata", "inputs", "outputs", "on", "mcp",
		"include", "vars", "secrets", "defaults", "concurrency", "timeout", "steps",
	}
	fragmentKeys = []string{"include", "vars", "secrets", "defaults", "steps"}
	defaultsKeys = []string{
		"timeout", "retain", "retry", "resources", "network", "egress", "runs_on",
		"secrets", "cache", "continue_on_error", "idempotent", "files", "shell",
		"before_script", "after_script", "when",
	}
	stepOnlyKeys = []string{
		"image", "script", "needs", "inputs", "outputs", "params", "if", "merge",
		"strategy", "extends", "workflow",
	}
	// A step carries every execution keyword but one. retain is written in defaults, or
	// on a workflow output, and never on a step: how long an artifact stays fetchable is
	// a property of the run's boundary and of what the workflow says about all of its
	// steps, and a step saying it for itself alone is a keyword the step table does not
	// carry.
	stepKeys = slices.Concat(without(defaultsKeys, "retain"), stepOnlyKeys)
)

func without(keys []string, drop string) []string {
	kept := make([]string, 0, len(keys))
	for _, key := range keys {
		if key != drop {
			kept = append(kept, key)
		}
	}
	return kept
}

// Parse reads one entry point, closed, and applies every rule that is about the shape of
// the document: the keys the language defines, the grammars its names are written on,
// the values its enumerations allow and the durations its keywords take.
//
// It is read by hand rather than against the released workflow.schema.json, on the
// precedent package agk set for the envelope and for the same reason: a refusal has to
// name the step, the port and the rule in the documentation's own words, and a JSON
// Schema error names a keyword and a JSON Pointer. That this reader agrees with the
// released schema is not claimed here; it is held by corpus_test.go, which runs the
// released fixture corpus.
//
// What Parse does not do is anything that needs a second document. The rules about the
// graph, the manifest and the published surface are Check and Build, because a workflow
// that parses is not yet a workflow that holds together.
//
// A file that declares includes is resolved as far as this document goes and no
// further: an extends naming a block the file does not carry is left standing, because
// an included fragment may carry it and Parse was not given the fragments. Load, which
// has them, refuses what is still unresolved.
func Parse(doc []byte) (*Workflow, error) {
	root, file, err := document(doc)
	if err != nil {
		return nil, err
	}
	if err := closedTo(root, "the workflow", rootKeys...); err != nil {
		return nil, err
	}

	wf := &Workflow{doc: file, values: map[agk.Step]stepValues{}, blocks: map[string]stepValues{}}
	if wf.APIVersion, err = constantAt(root, "apiVersion", "agentiik.dev/v1", "the workflow"); err != nil {
		return nil, fmt.Errorf("%w. apiVersion is read before anything else, because it decides how every other key in the file is interpreted", err)
	}
	if wf.Kind, err = constantAt(root, "kind", "Workflow", "the workflow"); err != nil {
		return nil, fmt.Errorf("%w. The same apiVersion also covers the brick manifest, so the kind is what tells a reader which of the two it is holding", err)
	}
	if wf.Metadata, err = metadataOf(root); err != nil {
		return nil, err
	}
	if wf.Inputs, err = inputsOf(root); err != nil {
		return nil, err
	}
	if wf.Outputs, err = outputsOf(root); err != nil {
		return nil, err
	}
	if wf.On, err = triggerOf(root); err != nil {
		return nil, err
	}
	if wf.MCP, err = mcpOf(root); err != nil {
		return nil, err
	}
	if wf.Include, err = includesOf(root); err != nil {
		return nil, err
	}
	if wf.Vars, err = varsOf(root); err != nil {
		return nil, err
	}
	if wf.Secrets, err = secretsOf(root); err != nil {
		return nil, err
	}
	if wf.Defaults, err = defaultsOf(root); err != nil {
		return nil, err
	}
	if wf.Concurrency, err = concurrencyOf(root); err != nil {
		return nil, err
	}
	if wf.Timeout, err = durationAt(root, "timeout", "the workflow"); err != nil {
		return nil, err
	}
	if err := stepsOf(root, wf.values, wf.blocks); err != nil {
		return nil, err
	}

	// A file that includes nothing has everything it will ever have, so resolution is
	// complete here and an extends naming nothing is a refusal. Otherwise the blocks
	// the includes carry are still to come and Load settles it.
	if err := wf.resolveSteps(len(wf.Include) == 0); err != nil {
		return nil, err
	}
	return wf, nil
}

// Fragment is an included file: "a fragment, not an entry point. It carries hidden blocks
// and shared settings; it has no apiVersion, no kind and no metadata."
//
// It is opaque, and deliberately: a Fragment comes from ParseFragment and goes into Load,
// so there is nothing for a caller to build one out of and no way to hand the evaluator a
// fragment that was never read as one. "Validating a fragment against the workflow schema
// is a category error", and so would constructing one be.
type Fragment struct {
	include  []Include
	vars     Vars
	secrets  map[string]SecretDecl
	defaults Defaults
	values   map[agk.Step]stepValues
	blocks   map[string]stepValues
}

// ParseFragment reads one included file.
//
// What a fragment may carry is what reuse is for: hidden blocks, steps, defaults, vars,
// secrets and includes of its own. The keys that make a document an entry point are
// refused here, which is the reading the mcp rule states and the rest of the boundary
// follows: "the published surface is declared by the workflow that publishes it, so that
// reading one file tells you everything that workflow exposes". The same is true of the
// inputs, the outputs and the triggers, which are the workflow's other boundary; an
// included file that declared one of them would move the boundary out of the file that
// names it.
func ParseFragment(doc []byte) (*Fragment, error) {
	root, _, err := document(doc)
	if err != nil {
		return nil, err
	}
	if _, ok := root["mcp"]; ok {
		return nil, refuse(RuleMCPInIncludedFile, "", "", "reuse applies to steps and defaults; the published surface is declared by the workflow that publishes it, so that reading one file tells you everything that workflow exposes")
	}
	for _, key := range []string{"apiVersion", "kind", "metadata"} {
		if _, ok := root[key]; ok {
			return nil, fmt.Errorf("the included file declares %s: an included file is a fragment, not an entry point, and it has no apiVersion, no kind and no metadata", key)
		}
	}
	for _, key := range []string{"inputs", "outputs", "on", "concurrency", "timeout"} {
		if _, ok := root[key]; ok {
			return nil, fmt.Errorf("the included file declares %s: the boundary of a workflow, what it is given, what it returns and what starts it, is declared by the workflow itself, so that reading one file tells you what that workflow is", key)
		}
	}
	if err := closedTo(root, "the included file", fragmentKeys...); err != nil {
		return nil, err
	}

	f := &Fragment{values: map[agk.Step]stepValues{}, blocks: map[string]stepValues{}}
	if f.include, err = includesOf(root); err != nil {
		return nil, err
	}
	if f.vars, err = varsOf(root); err != nil {
		return nil, err
	}
	if f.secrets, err = secretsOf(root); err != nil {
		return nil, err
	}
	if f.defaults, err = defaultsOf(root); err != nil {
		return nil, err
	}
	if err := stepsOf(root, f.values, f.blocks); err != nil {
		return nil, err
	}
	return f, nil
}

// document reads the file with a YAML 1.2 parser and puts what it read on JSON's own
// terms.
//
// The version of YAML is load bearing rather than a preference: "the trigger block is
// spelled on:, as the documentation spells it, and a YAML 1.1 parser reads that bare key
// as the boolean true. A loader that does so will fail every fixture carrying a trigger,
// and will do the same to the workflows people write."
func document(doc []byte) (map[string]any, *ast.File, error) {
	var v any
	if err := yaml.Unmarshal(doc, &v); err != nil {
		return nil, nil, fmt.Errorf("the file is not a YAML document: %w", err)
	}
	value, err := jsonLike(v, "")
	if err != nil {
		return nil, nil, err
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("the file is not a block of keys: a workflow is a mapping, and an empty file declares nothing at all")
	}
	// The document is parsed a second time, for positions alone. Nothing here reads it,
	// and what does is a refusal pointing at the line an expression was written on.
	file, err := parser.ParseBytes(doc, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("the file is not a YAML document: %w", err)
	}
	return root, file, nil
}

// position answers where a value sits in the document, as a line and a column, given the
// path of the value in YAML path form. It answers zero where the document does not carry
// it, which is what a refusal about a value the file never wrote would ask for.
func position(file *ast.File, path string) (int, int) {
	if file == nil {
		return 0, 0
	}
	p, err := yaml.PathString(path)
	if err != nil {
		return 0, 0
	}
	node, err := p.FilterFile(file)
	if err != nil || node == nil {
		return 0, 0
	}
	token := node.GetToken()
	if token == nil {
		return 0, 0
	}
	return token.Position.Line, token.Position.Column
}

func metadataOf(root map[string]any) (Metadata, error) {
	raw, ok := root["metadata"]
	if !ok {
		return Metadata{}, fmt.Errorf("the workflow declares no metadata: the name and the namespace form the identity a run, a grant and a git remote are addressed by")
	}
	m, err := mapping(raw, "metadata")
	if err != nil {
		return Metadata{}, err
	}
	if err := closedTo(m, "metadata", "name", "namespace", "labels"); err != nil {
		return Metadata{}, err
	}

	var md Metadata
	name, written, err := textAt(m, "name", "metadata")
	if err != nil {
		return Metadata{}, err
	}
	if !written {
		return Metadata{}, fmt.Errorf("the workflow declares no metadata.name: the name and the namespace form the identity a run, a grant and a git remote are addressed by")
	}
	if err := identifier(name, "the workflow", "metadata"); err != nil {
		return Metadata{}, err
	}
	md.Name = name
	if md.Namespace, _, err = textAt(m, "namespace", "metadata"); err != nil {
		return Metadata{}, err
	}
	if md.Namespace != "" {
		if err := identifier(md.Namespace, "the namespace", "metadata"); err != nil {
			return Metadata{}, err
		}
	}
	if labels, ok := m["labels"]; ok {
		b, err := mapping(labels, "metadata.labels")
		if err != nil {
			return Metadata{}, err
		}
		md.Labels = make(map[string]string, len(b))
		for _, key := range keysOf(b) {
			if err := identifier(key, "the label", "metadata.labels"); err != nil {
				return Metadata{}, err
			}
			text, ok := b[key].(string)
			if !ok {
				return Metadata{}, fmt.Errorf("metadata.labels.%s is written as %s, and a label is text: it is what a search matches on and what the console shows beside the workflow", key, kindOf(b[key]))
			}
			md.Labels[key] = text
		}
	}
	return md, nil
}

func inputsOf(root map[string]any) (map[string]Input, error) {
	raw, ok := root["inputs"]
	if !ok {
		return nil, nil
	}
	b, err := mapping(raw, "inputs")
	if err != nil {
		return nil, err
	}
	inputs := make(map[string]Input, len(b))
	for _, name := range keysOf(b) {
		where := "inputs." + name
		if err := identifier(name, "the input", "inputs"); err != nil {
			return nil, err
		}
		entry, err := mapping(b[name], where)
		if err != nil {
			return nil, err
		}
		if err := closedTo(entry, where, "schema", "required", "default"); err != nil {
			return nil, err
		}
		var in Input
		if in.Schema, err = documentAt(entry, "schema", where); err != nil {
			return nil, err
		}
		if in.Required, _, err = boolAt(entry, "required", where); err != nil {
			return nil, err
		}
		in.Default = entry["default"]
		inputs[name] = in
	}
	return inputs, nil
}

func outputsOf(root map[string]any) (map[string]Output, error) {
	raw, ok := root["outputs"]
	if !ok {
		return nil, nil
	}
	b, err := mapping(raw, "outputs")
	if err != nil {
		return nil, err
	}
	outputs := make(map[string]Output, len(b))
	for _, name := range keysOf(b) {
		where := "outputs." + name
		if err := identifier(name, "the output", "outputs"); err != nil {
			return nil, err
		}
		entry, err := mapping(b[name], where)
		if err != nil {
			return nil, err
		}
		if err := closedTo(entry, where, "from", "retain", "schema"); err != nil {
			return nil, err
		}

		var out Output
		from, ok := entry["from"]
		if !ok {
			return nil, fmt.Errorf("%s says nothing it is taken from: an output is a view of one step port, written from: { step, port }", where)
		}
		f, err := mapping(from, where+".from")
		if err != nil {
			return nil, err
		}
		if err := closedTo(f, where+".from", "step", "port"); err != nil {
			return nil, err
		}
		step, written, err := textAt(f, "step", where+".from")
		if err != nil {
			return nil, err
		}
		if !written {
			return nil, fmt.Errorf("%s.from names no step", where)
		}
		port, written, err := textAt(f, "port", where+".from")
		if err != nil {
			return nil, err
		}
		if !written {
			return nil, fmt.Errorf("%s.from names the step %s and no port: an output is a view of one step port, because an output that concatenated several would hide which step produced what", where, step)
		}
		if err := identifier(step, "the step", where+".from"); err != nil {
			return nil, err
		}
		if err := identifier(port, "the port", where+".from"); err != nil {
			return nil, err
		}
		out.From.Step, out.From.Port = agk.Step(step), agk.Port(port)

		if out.Retain, err = retainAt(entry, where, true); err != nil {
			return nil, err
		}
		if out.Schema, err = documentAt(entry, "schema", where); err != nil {
			return nil, err
		}
		outputs[name] = out
	}
	return outputs, nil
}

func triggerOf(root map[string]any) (Trigger, error) {
	raw, ok := root["on"]
	if !ok {
		return Trigger{}, nil
	}
	b, err := mapping(raw, "on")
	if err != nil {
		return Trigger{}, err
	}
	if err := closedTo(b, "on", "schedule", "webhook", "event"); err != nil {
		return Trigger{}, err
	}

	var t Trigger
	scheduled, err := listAt(b, "schedule", "on")
	if err != nil {
		return Trigger{}, err
	}
	for i, raw := range scheduled {
		where := fmt.Sprintf("on.schedule[%d]", i)
		entry, err := mapping(raw, where)
		if err != nil {
			return Trigger{}, err
		}
		if err := closedTo(entry, where, "cron", "timezone", "jitter", "catch_up"); err != nil {
			return Trigger{}, err
		}
		var s Schedule
		written := false
		if s.Cron, written, err = textAt(entry, "cron", where); err != nil {
			return Trigger{}, err
		}
		if !written || len(strings.Fields(s.Cron)) != 5 {
			return Trigger{}, fmt.Errorf("%s.cron is %q: a schedule is a five-field expression read in the timezone given beside it", where, s.Cron)
		}
		if s.Timezone, _, err = textAt(entry, "timezone", where); err != nil {
			return Trigger{}, err
		}
		if s.Jitter, err = durationAt(entry, "jitter", where); err != nil {
			return Trigger{}, err
		}
		if s.CatchUp, _, err = boolAt(entry, "catch_up", where); err != nil {
			return Trigger{}, err
		}
		t.Schedule = append(t.Schedule, s)
	}

	hooks, err := listAt(b, "webhook", "on")
	if err != nil {
		return Trigger{}, err
	}
	for i, raw := range hooks {
		where := fmt.Sprintf("on.webhook[%d]", i)
		entry, err := mapping(raw, where)
		if err != nil {
			return Trigger{}, err
		}
		if err := closedTo(entry, where, "path", "method", "auth", "response", "map"); err != nil {
			return Trigger{}, err
		}
		var w Webhook
		written := false
		if w.Path, written, err = textAt(entry, "path", where); err != nil {
			return Trigger{}, err
		}
		if !written || !webhookPath.MatchString(w.Path) {
			return Trigger{}, fmt.Errorf("%s.path is %q: a webhook path begins with a slash and is namespaced as /hooks/<namespace>/<path> when it is served", where, w.Path)
		}
		if w.Method, _, err = textAt(entry, "method", where); err != nil {
			return Trigger{}, err
		}
		if w.Method != "" && !httpMethod.MatchString(w.Method) {
			return Trigger{}, fmt.Errorf("%s.method is %q, and a method is written in capitals", where, w.Method)
		}
		if w.Auth, err = enumAt(entry, "auth", where, "hmac", "bearer", "mtls", "none"); err != nil {
			return Trigger{}, fmt.Errorf("%w. auth proves which caller is speaking, never which user", err)
		}
		if w.Response, err = enumAt(entry, "response", where, "async", "sync"); err != nil {
			return Trigger{}, err
		}
		if m, ok := entry["map"]; ok {
			mapped, err := mapping(m, where+".map")
			if err != nil {
				return Trigger{}, err
			}
			w.Map = make(map[string]any, len(mapped))
			for _, name := range keysOf(mapped) {
				if err := identifier(name, "the input", where+".map"); err != nil {
					return Trigger{}, err
				}
				w.Map[name] = mapped[name]
			}
		}
		t.Webhook = append(t.Webhook, w)
	}

	events, err := listAt(b, "event", "on")
	if err != nil {
		return Trigger{}, err
	}
	for i, raw := range events {
		where := fmt.Sprintf("on.event[%d]", i)
		entry, err := mapping(raw, where)
		if err != nil {
			return Trigger{}, err
		}
		if err := closedTo(entry, where, "type", "source", "filter"); err != nil {
			return Trigger{}, err
		}
		var e Event
		if e.Type, _, err = textAt(entry, "type", where); err != nil {
			return Trigger{}, err
		}
		if e.Source, _, err = textAt(entry, "source", where); err != nil {
			return Trigger{}, err
		}
		if e.Filter, _, err = textAt(entry, "filter", where); err != nil {
			return Trigger{}, err
		}
		t.Event = append(t.Event, e)
	}
	return t, nil
}

func mcpOf(root map[string]any) (*MCP, error) {
	raw, ok := root["mcp"]
	if !ok {
		return nil, nil
	}
	b, err := mapping(raw, "mcp")
	if err != nil {
		return nil, err
	}
	if err := closedTo(b, "mcp", "name", "description", "tools"); err != nil {
		return nil, err
	}

	m := &MCP{}
	if m.Name, _, err = textAt(b, "name", "mcp"); err != nil {
		return nil, err
	}
	if m.Name != "" {
		if err := identifier(m.Name, "the server", "mcp"); err != nil {
			return nil, err
		}
	}
	if m.Description, _, err = textAt(b, "description", "mcp"); err != nil {
		return nil, err
	}
	if raw, ok := b["tools"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("mcp.tools is not a list: it is the published tools, one entry each, and an empty list publishes a server with nothing on it")
		}
		// An empty list is not the same as no block at all, and it is kept as one: a
		// tools key written empty makes Tools non-nil.
		m.Tools = make([]Tool, 0, len(list))
		for i, raw := range list {
			t, err := toolOf(raw, fmt.Sprintf("mcp.tools[%d]", i))
			if err != nil {
				return nil, err
			}
			m.Tools = append(m.Tools, t)
		}
	}
	return m, nil
}

func toolOf(raw any, where string) (Tool, error) {
	b, err := mapping(raw, where)
	if err != nil {
		return Tool{}, err
	}
	if err := closedTo(b, where, "name", "title", "description", "input", "output", "mode", "timeout", "annotations"); err != nil {
		return Tool{}, err
	}

	var t Tool
	written := false
	if t.Name, written, err = textAt(b, "name", where); err != nil {
		return Tool{}, err
	}
	if !written {
		return Tool{}, fmt.Errorf("%s declares no name: a tool identifier is unique within the workflow and stable across commits, because clients hold it", where)
	}
	if err := identifier(t.Name, "the tool", where); err != nil {
		return Tool{}, err
	}
	if t.Title, _, err = textAt(b, "title", where); err != nil {
		return Tool{}, err
	}
	if t.Description, written, err = textAt(b, "description", where); err != nil {
		return Tool{}, err
	}
	if !written || strings.TrimSpace(t.Description) == "" {
		return Tool{}, fmt.Errorf("the tool %s declares no description: it is the field a model actually acts on, and a tool published without one is exactly the tool a model has no way to decide to call", t.Name)
	}

	input, ok := b["input"]
	if !ok {
		return Tool{}, fmt.Errorf("the tool %s names no input: a tool is a view of one workflow input, whose JSON Schema becomes its inputSchema", t.Name)
	}
	if t.Input, err = toolIOOf(input, where+".input", "input"); err != nil {
		return Tool{}, err
	}
	if output, ok := b["output"]; ok {
		io, err := toolIOOf(output, where+".output", "output")
		if err != nil {
			return Tool{}, err
		}
		t.Output = &io
	}

	mode, err := enumAt(b, "mode", where, "sync", "async")
	if err != nil {
		return Tool{}, err
	}
	if mode == "async" {
		t.Mode = ToolAsync
	}
	if t.Timeout, err = durationAt(b, "timeout", where); err != nil {
		return Tool{}, err
	}
	// The two rules about how long a call waits are read here, with the document, and
	// not with the graph: both are about the tool alone, the released schema carries
	// them in the grammar of the timeout itself, and the corpus marks them refused by
	// the schema. They are refusals of the language all the same, and they name their
	// rule.
	if err := toolTimeout(t); err != nil {
		return Tool{}, err
	}
	if raw, ok := b["annotations"]; ok {
		a, err := mapping(raw, where+".annotations")
		if err != nil {
			return Tool{}, err
		}
		if err := closedTo(a, where+".annotations", "readOnlyHint", "idempotentHint", "destructiveHint", "openWorldHint"); err != nil {
			return Tool{}, fmt.Errorf("%w. They are the protocol's own hints, passed through unchanged", err)
		}
		hints := []struct {
			key  string
			into **bool
		}{
			{"readOnlyHint", &t.Annotations.ReadOnlyHint},
			{"idempotentHint", &t.Annotations.IdempotentHint},
			{"destructiveHint", &t.Annotations.DestructiveHint},
			{"openWorldHint", &t.Annotations.OpenWorldHint},
		}
		for _, h := range hints {
			v, written, err := boolAt(a, h.key, where+".annotations")
			if err != nil {
				return Tool{}, err
			}
			if written {
				kept := v
				*h.into = &kept
			}
		}
	}
	return t, nil
}

func toolIOOf(raw any, where, side string) (ToolIO, error) {
	b, err := mapping(raw, where)
	if err != nil {
		return ToolIO{}, err
	}
	if err := closedTo(b, where, "from"); err != nil {
		return ToolIO{}, err
	}
	from, ok := b["from"]
	if !ok {
		return ToolIO{}, fmt.Errorf("%s says nothing it is taken from: it is written from: { %s: <name> }", where, side)
	}
	f, err := mapping(from, where+".from")
	if err != nil {
		return ToolIO{}, err
	}
	if err := closedTo(f, where+".from", side); err != nil {
		return ToolIO{}, fmt.Errorf("%w. The reference is to a workflow input or output, never to a step or a port: a tool is a view of the workflow's own boundary, which is what keeps the graph free to change beneath it", err)
	}
	name, written, err := textAt(f, side, where+".from")
	if err != nil {
		return ToolIO{}, err
	}
	if !written {
		return ToolIO{}, fmt.Errorf("%s.from names no %s", where, side)
	}
	if err := identifier(name, "the workflow "+side, where+".from"); err != nil {
		return ToolIO{}, err
	}
	if side == "input" {
		return ToolIO{Input: name}, nil
	}
	return ToolIO{Output: name}, nil
}

func includesOf(root map[string]any) ([]Include, error) {
	raw, ok := root["include"]
	if !ok {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("include is not a list: includes are resolved in declaration order, one entry each")
	}
	includes := make([]Include, 0, len(list))
	for i, raw := range list {
		where := fmt.Sprintf("include[%d]", i)
		b, err := mapping(raw, where)
		if err != nil {
			return nil, err
		}
		if err := closedTo(b, where, "path", "workflow", "ref"); err != nil {
			return nil, err
		}
		path, hasPath, err := textAt(b, "path", where)
		if err != nil {
			return nil, err
		}
		name, hasWorkflow, err := textAt(b, "workflow", where)
		if err != nil {
			return nil, err
		}
		ref, hasRef, err := textAt(b, "ref", where)
		if err != nil {
			return nil, err
		}
		switch {
		case hasPath && hasWorkflow:
			return nil, fmt.Errorf("%s is both a file of this repository and another repository: a path include resolves inside the same commit and a workflow include resolves elsewhere at a ref, and one entry cannot be both", where)
		case hasPath && hasRef:
			return nil, fmt.Errorf("%s pins a path include to a ref: a path include resolves inside the same commit, so it can never be stale and there is nothing for a ref to pin", where)
		case hasPath:
			includes = append(includes, Include{Path: path})
		case hasWorkflow && !hasRef:
			return nil, fmt.Errorf("%s includes the workflow %s with no ref: an unpinned cross-repository include would let another repository change what this commit does, which is the one property a versioned entry point exists to hold", where, name)
		case hasWorkflow:
			r, err := parseWorkflowRef(name, where)
			if err != nil {
				return nil, err
			}
			if strings.Contains(name, "@") {
				return nil, fmt.Errorf("%s pins the include inside the workflow name: a workflow include carries the ref beside it, as ref: %s", where, ref)
			}
			r.Ref = ref
			includes = append(includes, Include{Workflow: r})
		default:
			return nil, fmt.Errorf("%s includes nothing: an include is a path of this repository or a workflow of another, at a ref", where)
		}
	}
	return includes, nil
}

func varsOf(root map[string]any) (Vars, error) {
	raw, ok := root["vars"]
	if !ok {
		return nil, nil
	}
	b, err := mapping(raw, "vars")
	if err != nil {
		return nil, err
	}
	vars := make(Vars, len(b))
	for _, name := range keysOf(b) {
		if err := identifier(name, "the variable", "vars"); err != nil {
			return nil, err
		}
		vars[name] = b[name]
	}
	return vars, nil
}

func secretsOf(root map[string]any) (map[string]SecretDecl, error) {
	raw, ok := root["secrets"]
	if !ok {
		return nil, nil
	}
	b, err := mapping(raw, "secrets")
	if err != nil {
		return nil, err
	}
	secrets := make(map[string]SecretDecl, len(b))
	for _, name := range keysOf(b) {
		where := "secrets." + name
		if err := identifier(name, "the secret", "secrets"); err != nil {
			return nil, err
		}
		entry, err := mapping(b[name], where)
		if err != nil {
			return nil, err
		}
		if err := closedTo(entry, where, "provider", "path"); err != nil {
			return nil, err
		}
		var s SecretDecl
		written := false
		if s.Provider, written, err = textAt(entry, "provider", where); err != nil {
			return nil, err
		}
		if !written {
			return nil, fmt.Errorf("%s declares no provider: the value is never written in the file, so the provider is all there is to resolve it by", where)
		}
		if s.Path, _, err = textAt(entry, "path", where); err != nil {
			return nil, err
		}
		secrets[name] = s
	}
	return secrets, nil
}

func concurrencyOf(root map[string]any) (Concurrency, error) {
	raw, ok := root["concurrency"]
	if !ok {
		return Concurrency{}, nil
	}
	b, err := mapping(raw, "concurrency")
	if err != nil {
		return Concurrency{}, err
	}
	if err := closedTo(b, "concurrency", "group", "cancel_in_progress"); err != nil {
		return Concurrency{}, err
	}
	var c Concurrency
	if c.Group, _, err = textAt(b, "group", "concurrency"); err != nil {
		return Concurrency{}, err
	}
	if c.CancelInProgress, _, err = boolAt(b, "cancel_in_progress", "concurrency"); err != nil {
		return Concurrency{}, err
	}
	return c, nil
}

// stepsOf reads the steps block and the hidden blocks, wherever they are written. "A
// hidden block may be written at the root of the entry point as well as at the root of an
// included file, and a step under steps may be named with a leading dot."
func stepsOf(root map[string]any, into map[agk.Step]stepValues, blocks map[string]stepValues) error {
	for _, name := range keysOf(root) {
		if !hidden(name) {
			continue
		}
		if !hiddenName.MatchString(name) {
			return fmt.Errorf("the workflow declares the hidden block %q, whose name is not an identifier after the dot", name)
		}
		values, err := stepValuesOf(root[name], name)
		if err != nil {
			return err
		}
		blocks[name] = values
	}

	raw, ok := root["steps"]
	if !ok {
		return nil
	}
	b, err := mapping(raw, "steps")
	if err != nil {
		return err
	}
	for _, name := range keysOf(b) {
		where := "steps." + name
		values, err := stepValuesOf(b[name], where)
		if err != nil {
			return err
		}
		if hidden(name) {
			if !hiddenName.MatchString(name) {
				return fmt.Errorf("%s is a hidden block whose name is not an identifier after the dot", where)
			}
			blocks[name] = values
			continue
		}
		if err := agk.Step(name).Validate(); err != nil {
			return fmt.Errorf("steps carries %q: %w. The name is used in needs, in expressions and in the run detail", name, err)
		}
		into[agk.Step(name)] = values
	}
	return nil
}

// stepValuesOf reads one step, or one hidden block, which is the same shape: "a hidden
// block ... is never executed" and carries the same keywords.
func stepValuesOf(raw any, where string) (stepValues, error) {
	b, err := mapping(raw, where)
	if err != nil {
		return stepValues{}, err
	}
	if err := closedTo(b, where, stepKeys...); err != nil {
		return stepValues{}, fmt.Errorf("%w. The set of step keywords is closed, so a misspelling is refused rather than silently ignored", err)
	}

	var s stepValues
	if s.Defaults, err = executionOf(b, where); err != nil {
		return stepValues{}, err
	}

	image, hasImage, err := textAt(b, "image", where)
	if err != nil {
		return stepValues{}, err
	}
	if hasImage {
		s.Image = &image
	}
	if s.Call, err = callAt(b, where); err != nil {
		return stepValues{}, err
	}
	if hasImage && s.Call != nil {
		return stepValues{}, fmt.Errorf("%s runs an image and calls a sub-workflow: workflow is an alternative to image, never a companion to it", where)
	}
	if s.Script, err = commandsAt(b, "script", where); err != nil {
		return stepValues{}, err
	}
	if s.Call != nil {
		for _, key := range []string{"script", "before_script", "after_script", "shell"} {
			if _, ok := b[key]; ok {
				return stepValues{}, fmt.Errorf("%s calls a sub-workflow and carries %s: script commands run inside image and workflow is an alternative to image, so a sub-workflow call has no container for a script to run in", where, key)
			}
		}
	}
	if s.Needs, err = needsAt(b, where); err != nil {
		return stepValues{}, err
	}
	if raw, ok := b["inputs"]; ok {
		m, err := mapping(raw, where+".inputs")
		if err != nil {
			return stepValues{}, err
		}
		s.Inputs = make(map[agk.Port]any, len(m))
		for _, name := range keysOf(m) {
			if err := agk.Port(name).Validate(); err != nil {
				return stepValues{}, fmt.Errorf("%s.inputs feeds %q: %w", where, name, err)
			}
			s.Inputs[agk.Port(name)] = m[name]
		}
	}
	if raw, ok := b["outputs"]; ok {
		list, ok := raw.([]any)
		if !ok || len(list) == 0 {
			return stepValues{}, fmt.Errorf("%s.outputs is not a list of port names: a step declares the ports it publishes, one entry each", where)
		}
		s.Outputs = make([]agk.Port, 0, len(list))
		for _, raw := range list {
			name, ok := raw.(string)
			if !ok {
				return stepValues{}, fmt.Errorf("%s.outputs carries %v, and a port is named by text", where, raw)
			}
			if err := agk.Port(name).Validate(); err != nil {
				return stepValues{}, fmt.Errorf("%s.outputs declares %q: %w", where, name, err)
			}
			s.Outputs = append(s.Outputs, agk.Port(name))
		}
	}
	if raw, ok := b["params"]; ok {
		m, err := mapping(raw, where+".params")
		if err != nil {
			return stepValues{}, err
		}
		s.Params = make(map[string]any, len(m))
		for _, name := range keysOf(m) {
			if err := parameter(name, where+".params"); err != nil {
				return stepValues{}, err
			}
			s.Params[name] = m[name]
		}
	}
	if condition, ok, err := textAt(b, "if", where); err != nil {
		return stepValues{}, err
	} else if ok {
		s.If = &condition
	}
	if s.Merge, s.Join, err = mergeAt(b, where); err != nil {
		return stepValues{}, err
	}
	if s.Strategy, err = strategyAt(b, where); err != nil {
		return stepValues{}, err
	}
	if s.Extends, _, err = textAt(b, "extends", where); err != nil {
		return stepValues{}, err
	}
	if s.Extends != "" && !hiddenName.MatchString(s.Extends) {
		return stepValues{}, fmt.Errorf("%s extends %q, which is not a hidden block: only a hidden block can be extended, and a hidden block is exactly a name that starts with a dot and is never executed", where, s.Extends)
	}
	return s, nil
}

// defaultsOf reads the defaults block, which carries "execution settings, and only
// those".
func defaultsOf(root map[string]any) (Defaults, error) {
	raw, ok := root["defaults"]
	if !ok {
		return Defaults{}, nil
	}
	b, err := mapping(raw, "defaults")
	if err != nil {
		return Defaults{}, err
	}
	for _, key := range keysOf(b) {
		if slices.Contains(stepOnlyKeys, key) {
			return Defaults{}, fmt.Errorf("defaults carries %s, which is refused there: anything deciding what a step runs, or where it sits in the graph, belongs to the step, which is what keeps the graph readable from the steps block alone", key)
		}
	}
	if err := closedTo(b, "defaults", defaultsKeys...); err != nil {
		return Defaults{}, err
	}
	return executionOf(b, "defaults")
}

// executionOf reads the keywords a step and defaults share. The one difference between
// the two positions is retain: "only a workflow output can be one-shot", so a fetch count
// is refused everywhere else.
func executionOf(b map[string]any, where string) (Defaults, error) {
	var d Defaults
	var err error

	if _, ok := b["timeout"]; ok {
		timeout, err := durationAt(b, "timeout", where)
		if err != nil {
			return Defaults{}, err
		}
		d.Timeout = &timeout
	}
	if _, ok := b["retain"]; ok {
		retain, err := retainAt(b, where, false)
		if err != nil {
			return Defaults{}, err
		}
		d.Retain = &retain
	}
	if raw, ok := b["retry"]; ok {
		r, err := retryOf(raw, where+".retry")
		if err != nil {
			return Defaults{}, err
		}
		d.Retry = &r
	}
	if raw, ok := b["resources"]; ok {
		r, err := resourcesOf(raw, where+".resources")
		if err != nil {
			return Defaults{}, err
		}
		d.Resources = &r
	}
	if _, ok := b["network"]; ok {
		name, err := enumAt(b, "network", where, "none", "egress", "internal")
		if err != nil {
			return Defaults{}, fmt.Errorf("%w. Every task gets its own network, and there is no posture that puts a container on the host's", err)
		}
		n := NetworkNone
		switch name {
		case "egress":
			n = NetworkEgress
		case "internal":
			n = NetworkInternal
		}
		d.Network = &n
	}
	if raw, ok := b["egress"]; ok {
		e, err := mapping(raw, where+".egress")
		if err != nil {
			return Defaults{}, err
		}
		if err := closedTo(e, where+".egress", "allow"); err != nil {
			return Defaults{}, err
		}
		allow, err := textsAt(e, "allow", where+".egress")
		if err != nil {
			return Defaults{}, err
		}
		if len(allow) == 0 {
			return Defaults{}, fmt.Errorf("%s.egress allows nothing: an egress block is the allow list the runner proxy enforces", where)
		}
		for _, entry := range allow {
			if err := hostPort(entry); err != nil {
				return Defaults{}, fmt.Errorf("%s.egress.allow carries %w", where, err)
			}
		}
		d.EgressAllow = allow
	}
	if _, ok := b["runs_on"]; ok {
		if d.RunsOn, err = textsAt(b, "runs_on", where); err != nil {
			return Defaults{}, err
		}
		for _, selector := range d.RunsOn {
			if !runnerLabel.MatchString(selector) {
				return Defaults{}, fmt.Errorf("%s.runs_on carries %q, which is not a label: selectors are written as key=value, because that is what a runner's labels are", where, selector)
			}
		}
	}
	if _, ok := b["secrets"]; ok {
		if d.Secrets, err = textsAt(b, "secrets", where); err != nil {
			return Defaults{}, err
		}
		for _, name := range d.Secrets {
			if err := identifier(name, "the secret", where+".secrets"); err != nil {
				return Defaults{}, err
			}
		}
	}
	if cache, ok, err := boolAt(b, "cache", where); err != nil {
		return Defaults{}, err
	} else if ok {
		d.Cache = &cache
	}
	if tolerate, ok, err := boolAt(b, "continue_on_error", where); err != nil {
		return Defaults{}, err
	} else if ok {
		d.ContinueOnError = &tolerate
	}
	if idempotent, ok, err := boolAt(b, "idempotent", where); err != nil {
		return Defaults{}, err
	} else if ok {
		d.Idempotent = &idempotent
	}
	if raw, ok := b["files"]; ok {
		if d.Files, err = filesOf(raw, where+".files"); err != nil {
			return Defaults{}, err
		}
	}
	if _, ok := b["shell"]; ok {
		if d.Shell, err = textsAt(b, "shell", where); err != nil {
			return Defaults{}, err
		}
		if len(d.Shell) == 0 {
			return Defaults{}, fmt.Errorf("%s.shell names no interpreter: it is the interpreter and its flags, and it defaults to [\"/bin/sh\", \"-e\"]", where)
		}
	}
	if d.BeforeScript, err = commandsAt(b, "before_script", where); err != nil {
		return Defaults{}, err
	}
	if d.AfterScript, err = commandsAt(b, "after_script", where); err != nil {
		return Defaults{}, err
	}
	if _, ok := b["when"]; ok {
		states, err := textsAt(b, "when", where)
		if err != nil {
			return Defaults{}, err
		}
		if len(states) == 0 {
			return Defaults{}, fmt.Errorf("%s.when names no upstream state: it defaults to [succeeded] and an empty list would let a step start on nothing", where)
		}
		for _, state := range states {
			switch state {
			case "succeeded":
				d.When = append(d.When, WhenSucceeded)
			case "failed":
				d.When = append(d.When, WhenFailed)
			case "skipped":
				d.When = append(d.When, WhenSkipped)
			case "always":
				d.When = append(d.When, WhenAlways)
			default:
				return Defaults{}, fmt.Errorf("%s.when names %q: the upstream states that allow a step to start are succeeded, failed, skipped and always", where, state)
			}
		}
	}
	return d, nil
}

func retryOf(raw any, where string) (Retry, error) {
	b, err := mapping(raw, where)
	if err != nil {
		return Retry{}, err
	}
	if err := closedTo(b, where, "max", "on", "backoff"); err != nil {
		return Retry{}, err
	}

	var r Retry
	if n, ok, err := intAt(b, "max", where); err != nil {
		return Retry{}, err
	} else if ok {
		if n < 0 {
			return Retry{}, fmt.Errorf("%s.max is %d: it is how many further attempts are made, and there is no negative number of them", where, n)
		}
		r.Max = n
	}
	if _, ok := b["on"]; ok {
		kinds, err := textsAt(b, "on", where)
		if err != nil {
			return Retry{}, err
		}
		if len(kinds) == 0 {
			return Retry{}, fmt.Errorf("%s.on names no failure: it is the kinds of failure a retry is made on, transient, failed, lost and timeout", where)
		}
		for _, kind := range kinds {
			f, err := agk.ParseFailure(kind)
			if err != nil {
				return Retry{}, fmt.Errorf("%s.on names %q: %w", where, kind, err)
			}
			r.On = append(r.On, f)
		}
	}
	if raw, ok := b["backoff"]; ok {
		bo, err := mapping(raw, where+".backoff")
		if err != nil {
			return Retry{}, err
		}
		if err := closedTo(bo, where+".backoff", "type", "base", "max"); err != nil {
			return Retry{}, err
		}
		if _, err := enumAt(bo, "type", where+".backoff", "exponential"); err != nil {
			return Retry{}, fmt.Errorf("%w. exponential is the only type there is: the wait starts at base, lengthens after each failure and never goes past max", err)
		}
		if r.Backoff.Base, err = durationAt(bo, "base", where+".backoff"); err != nil {
			return Retry{}, err
		}
		if r.Backoff.Max, err = durationAt(bo, "max", where+".backoff"); err != nil {
			return Retry{}, err
		}
	}
	return r, nil
}

func resourcesOf(raw any, where string) (Resources, error) {
	b, err := mapping(raw, where)
	if err != nil {
		return Resources{}, err
	}
	if err := closedTo(b, where, "cpu", "memory", "pids"); err != nil {
		return Resources{}, err
	}

	var r Resources
	if cpu, ok := b["cpu"]; ok {
		s, ok := cpu.(string)
		if !ok {
			return Resources{}, fmt.Errorf("%s.cpu is written as %s: cpu and memory are written exactly as the brick manifest writes them, quotation marks included, so that half a core reads as 0.5 wherever it travels", where, kindOf(cpu))
		}
		if !cpuRequest.MatchString(s) {
			return Resources{}, fmt.Errorf("%s.cpu is %q: it is a decimal above zero, written as a string", where, s)
		}
		r.CPU = s
	}
	if memory, ok := b["memory"]; ok {
		s, ok := memory.(string)
		if !ok || !memoryRequest.MatchString(s) {
			return Resources{}, fmt.Errorf("%s.memory is %v: memory is a whole number above zero with a binary suffix, Ki, Mi, Gi or Ti, so that 512Mi cannot be read as 512 bytes", where, memory)
		}
		r.Memory = s
	}
	if n, ok, err := intAt(b, "pids", where); err != nil {
		return Resources{}, err
	} else if ok {
		if n < 1 {
			return Resources{}, fmt.Errorf("%s.pids is %d: it is a whole number of processes, one or more", where, n)
		}
		r.PIDs = n
	}
	return r, nil
}

func filesOf(raw any, where string) ([]FileSelector, error) {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("%s is not a list: files narrows what the step receives from the repository tree, one entry each", where)
	}
	files := make([]FileSelector, 0, len(list))
	for i, raw := range list {
		at := fmt.Sprintf("%s[%d]", where, i)
		if path, ok := raw.(string); ok {
			files = append(files, FileSelector{From: path})
			continue
		}
		b, err := mapping(raw, at)
		if err != nil {
			return nil, err
		}
		if err := closedTo(b, at, "from", "to", "mode"); err != nil {
			return nil, err
		}
		var f FileSelector
		written := false
		if f.From, written, err = textAt(b, "from", at); err != nil {
			return nil, err
		}
		if !written {
			return nil, fmt.Errorf("%s names no path: the long form of files relocates a path, and it says which one", at)
		}
		if f.To, _, err = textAt(b, "to", at); err != nil {
			return nil, err
		}
		if f.Mode, _, err = textAt(b, "mode", at); err != nil {
			return nil, err
		}
		if f.Mode != "" && !fileMode.MatchString(f.Mode) {
			return nil, fmt.Errorf("%s.mode is %q: a mode is three octal digits, written as a string", at, f.Mode)
		}
		files = append(files, f)
	}
	return files, nil
}

func needsAt(b map[string]any, where string) ([]Edge, error) {
	raw, ok := b["needs"]
	if !ok {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("%s.needs is not a list of edges: an edge connects one output port of a step to one input port of this one, one entry each", where)
	}
	edges := make([]Edge, 0, len(list))
	for i, raw := range list {
		at := fmt.Sprintf("%s.needs[%d]", where, i)
		// "Long form { step, port, as }, short form step-name equivalent to
		// { step: name, port: out, as: in }."
		if name, ok := raw.(string); ok {
			if err := agk.Step(name).Validate(); err != nil {
				return nil, fmt.Errorf("%s names %q: %w", at, name, err)
			}
			edges = append(edges, Edge{Step: agk.Step(name), Port: "out", As: "in"})
			continue
		}
		e, err := mapping(raw, at)
		if err != nil {
			return nil, err
		}
		if err := closedTo(e, at, "step", "port", "as"); err != nil {
			return nil, err
		}
		step, written, err := textAt(e, "step", at)
		if err != nil {
			return nil, err
		}
		if !written {
			return nil, fmt.Errorf("%s names no step: an edge is the only form of dependency there is, so it names the step it comes from", at)
		}
		if err := agk.Step(step).Validate(); err != nil {
			return nil, fmt.Errorf("%s names %q: %w", at, step, err)
		}
		edge := Edge{Step: agk.Step(step), Port: "out", As: "in"}
		if port, written, err := textAt(e, "port", at); err != nil {
			return nil, err
		} else if written {
			if err := agk.Port(port).Validate(); err != nil {
				return nil, fmt.Errorf("%s takes %q: %w", at, port, err)
			}
			edge.Port = agk.Port(port)
		}
		if as, written, err := textAt(e, "as", at); err != nil {
			return nil, err
		} else if written {
			if err := agk.Port(as).Validate(); err != nil {
				return nil, fmt.Errorf("%s feeds %q: %w", at, as, err)
			}
			edge.As = agk.Port(as)
		}
		edges = append(edges, edge)
	}
	return edges, nil
}

func mergeAt(b map[string]any, where string) (*Merge, *Join, error) {
	raw, ok := b["merge"]
	if !ok {
		return nil, nil, nil
	}
	if name, ok := raw.(string); ok {
		var m Merge
		switch name {
		case "wait_all":
			m = MergeWaitAll
		case "zip":
			m = MergeZip
		case "first":
			m = MergeFirst
		case "join":
			return nil, nil, fmt.Errorf("%s.merge is a key join that does not say what it matches items on: without it, unmatched items cannot be told from matched ones, so it is written merge: { join: { on: \"$.data.customer_id\" } }", where)
		default:
			return nil, nil, fmt.Errorf("%s.merge is %q: the merge strategies are wait_all, zip, join and first", where, name)
		}
		return &m, nil, nil
	}

	m, err := mapping(raw, where+".merge")
	if err != nil {
		return nil, nil, err
	}
	if err := closedTo(m, where+".merge", "join"); err != nil {
		return nil, nil, err
	}
	j, ok := m["join"]
	if !ok {
		return nil, nil, fmt.Errorf("%s.merge names no strategy: the merge strategies are wait_all, zip, join and first", where)
	}
	block, err := mapping(j, where+".merge.join")
	if err != nil {
		return nil, nil, err
	}
	if err := closedTo(block, where+".merge.join", "on"); err != nil {
		return nil, nil, err
	}
	on, written, err := textAt(block, "on", where+".merge.join")
	if err != nil {
		return nil, nil, err
	}
	if !written || on == "" {
		return nil, nil, fmt.Errorf("%s.merge is a key join that does not say what it matches items on: without it, unmatched items cannot be told from matched ones", where)
	}
	// The path is a grammar the file is written on, so it is read here, with the rest of
	// the grammars, rather than at the moment a join runs. A path nobody can match is a
	// workflow that would refuse its own merge three hours into a run.
	if _, err := parsePath(on); err != nil {
		return nil, nil, fmt.Errorf("%s.merge.join.on: %w", where, err)
	}
	strategy := MergeJoin
	return &strategy, &Join{On: on}, nil
}

func strategyAt(b map[string]any, where string) (*Strategy, error) {
	raw, ok := b["strategy"]
	if !ok {
		return nil, nil
	}
	m, err := mapping(raw, where+".strategy")
	if err != nil {
		return nil, err
	}
	if err := closedTo(m, where+".strategy", "fan_out", "max_parallel", "matrix", "fail_fast"); err != nil {
		return nil, err
	}

	var s Strategy
	if name, written, err := textAt(m, "fan_out", where+".strategy"); err != nil {
		return nil, err
	} else if written {
		switch {
		case name == "none":
			s.FanOut = FanOutNone
		case name == "item":
			s.FanOut = FanOutItem
		case name == "matrix":
			s.FanOut = FanOutMatrix
		case strings.HasPrefix(name, "batch"):
			size, ok := strings.CutPrefix(name, "batch(")
			size, closed := strings.CutSuffix(size, ")")
			n, err := strconv.Atoi(size)
			if !ok || !closed || err != nil || n < 1 {
				return nil, fmt.Errorf("%s.strategy.fan_out is %q: batch(n) runs one container per batch of n items, and the size is written in the parentheses, as batch(50). A batch of zero describes a shard that can never be filled", where, name)
			}
			s.FanOut, s.Batch = FanOutBatch, n
		default:
			return nil, fmt.Errorf("%s.strategy.fan_out is %q: a fan-out is none, item, batch(n) or matrix", where, name)
		}
	}
	if n, ok, err := intAt(m, "max_parallel", where+".strategy"); err != nil {
		return nil, err
	} else if ok {
		if n < 1 {
			return nil, fmt.Errorf("%s.strategy.max_parallel is %d: it is how many shards of this step run at once, and one is the fewest", where, n)
		}
		s.MaxParallel = n
	}
	if raw, ok := m["matrix"]; ok {
		matrix, err := mapping(raw, where+".strategy.matrix")
		if err != nil {
			return nil, err
		}
		s.Matrix = make(map[string][]any, len(matrix))
		for _, name := range keysOf(matrix) {
			if err := identifier(name, "the matrix variable", where+".strategy.matrix"); err != nil {
				return nil, err
			}
			values, ok := matrix[name].([]any)
			if !ok || len(values) == 0 {
				return nil, fmt.Errorf("%s.strategy.matrix.%s is not a list of values: every combination is a shard, and a variable with no value is a product of nothing", where, name)
			}
			s.Matrix[name] = values
		}
	}
	if v, ok, err := boolAt(m, "fail_fast", where+".strategy"); err != nil {
		return nil, err
	} else if ok {
		s.FailFast = v
	}
	return &s, nil
}

func callAt(b map[string]any, where string) (*Call, error) {
	raw, ok := b["workflow"]
	if !ok {
		return nil, nil
	}
	if name, ok := raw.(string); ok {
		r, err := parseWorkflowRef(name, where+".workflow")
		if err != nil {
			return nil, err
		}
		return &Call{Workflow: r.Namespace + "/" + r.Name, Ref: r.Ref}, nil
	}
	m, err := mapping(raw, where+".workflow")
	if err != nil {
		return nil, err
	}
	if err := closedTo(m, where+".workflow", "workflow", "ref"); err != nil {
		return nil, err
	}
	name, written, err := textAt(m, "workflow", where+".workflow")
	if err != nil {
		return nil, err
	}
	if !written {
		return nil, fmt.Errorf("%s.workflow names no workflow: a call is written workflow: <namespace>/<name>", where)
	}
	r, err := parseWorkflowRef(name, where+".workflow")
	if err != nil {
		return nil, err
	}
	if strings.Contains(name, "@") {
		return nil, fmt.Errorf("%s.workflow pins the ref inside the name: the long form carries it beside, as ref", where)
	}
	ref, _, err := textAt(m, "ref", where+".workflow")
	if err != nil {
		return nil, err
	}
	return &Call{Workflow: r.Namespace + "/" + r.Name, Ref: ref}, nil
}

// retainAt reads retain, in the one form a step may write and the two a workflow output
// may. "Only a workflow output can be one-shot: an artifact travelling between two steps
// is read once per shard and again by a replay."
func retainAt(b map[string]any, where string, isOutput bool) (Retain, error) {
	raw, ok := b["retain"]
	if !ok {
		return Retain{}, nil
	}
	if text, ok := raw.(string); ok {
		d, err := ParseDuration(text)
		if err != nil {
			return Retain{}, fmt.Errorf("%s.retain: %w", where, err)
		}
		return Retain{For: d}, nil
	}
	m, err := mapping(raw, where+".retain")
	if err != nil {
		return Retain{}, err
	}
	if err := closedTo(m, where+".retain", "for", "fetches"); err != nil {
		return Retain{}, err
	}
	var r Retain
	if _, ok := m["for"]; !ok {
		return Retain{}, fmt.Errorf("%s.retain says how many fetches it survives and not how long it lives: the two mechanisms compose, and a one-shot artifact still has a duration, which is what expires it when nobody ever comes for it", where)
	}
	if r.For, err = durationAt(m, "for", where+".retain"); err != nil {
		return Retain{}, err
	}
	if n, ok, err := intAt(m, "fetches", where+".retain"); err != nil {
		return Retain{}, err
	} else if ok {
		if !isOutput {
			return Retain{}, fmt.Errorf("%s.retain carries a fetch count, and only a workflow output can be one-shot: an artifact travelling between two steps is read once per shard and again by a replay", where)
		}
		if n < 1 {
			return Retain{}, fmt.Errorf("%s.retain survives %d fetches, which would declare an output that may never be collected: a one-shot output is fetches: 1, and two consumers are fetches: 2", where, n)
		}
		r.Fetches = n
	}
	return r, nil
}

// commandsAt reads script, before_script and after_script, which are "lists of command
// strings, one command per entry, and a single multi-line block scalar is refused. The
// list is what makes that exit attributable to one command rather than to a script."
func commandsAt(b map[string]any, key, where string) ([]string, error) {
	raw, ok := b[key]
	if !ok {
		return nil, nil
	}
	if _, ok := raw.(string); ok {
		return nil, fmt.Errorf("%s.%s is one block of text: it is a list of command strings, one command per entry, which is what makes a non-zero exit attributable to one command rather than to a script", where, key)
	}
	commands, err := textsAt(b, key, where)
	if err != nil {
		return nil, err
	}
	if len(commands) == 0 {
		return nil, fmt.Errorf("%s.%s is an empty list, and a list of commands carries at least one", where, key)
	}
	return commands, nil
}

// The grammars the values are written on, each one the released schema's own.
var (
	webhookPath   = regexp.MustCompile(`^/[A-Za-z0-9._~/-]*$`)
	httpMethod    = regexp.MustCompile(`^[A-Z]+$`)
	runnerLabel   = regexp.MustCompile(`^[^\s=]+=[^\s=]+$`)
	fileMode      = regexp.MustCompile(`^0?[0-7]{3}$`)
	cpuRequest    = regexp.MustCompile(`^(?:[0-9]*[1-9][0-9]*(?:\.[0-9]+)?|[0-9]+\.[0-9]*[1-9][0-9]*)$`)
	memoryRequest = regexp.MustCompile(`^[1-9][0-9]*(Ki|Mi|Gi|Ti)$`)
	egressEntry   = regexp.MustCompile(`^([^\s/:]+):([0-9]{1,5})$`)
)

// hostPort holds an egress entry to the pair the runner proxy opens a connection to.
func hostPort(entry string) error {
	m := egressEntry.FindStringSubmatch(entry)
	if m == nil {
		return fmt.Errorf("%q, which names no port: the runner proxy enforces the list as host and port pairs", entry)
	}
	port, err := strconv.Atoi(m[2])
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%q, whose port is outside 1 to 65535: the runner proxy opens a connection to the pair, and there is no port above that to open", entry)
	}
	return nil
}

// mapping reads one block, and says where it was expected when what is there is not one.
func mapping(v any, where string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is not a block: it is written as a mapping of keys to values, and what is there is %s", where, kindOf(v))
	}
	return m, nil
}

// closedTo refuses a key the block does not define. A hidden block is not a key of the
// root in that sense and is passed over here, because it is read where the steps are.
func closedTo(m map[string]any, where string, defined ...string) error {
	for _, key := range keysOf(m) {
		if hidden(key) && (where == "the workflow" || where == "the included file") {
			continue
		}
		if !slices.Contains(defined, key) {
			return fmt.Errorf("%s carries the key %q, which the language does not define: the file is closed, and timout: 10m is a rejected push rather than a line that quietly does nothing", where, key)
		}
	}
	return nil
}

func constantAt(m map[string]any, key, want, where string) (string, error) {
	got, written, err := textAt(m, key, where)
	if err != nil {
		return "", err
	}
	if !written {
		return "", fmt.Errorf("%s declares no %s", where, key)
	}
	if got != want {
		return "", fmt.Errorf("%s declares %s %q, and %s reads %q and nothing else", where, key, got, key, want)
	}
	return got, nil
}

func textAt(m map[string]any, key, where string) (string, bool, error) {
	v, ok := m[key]
	if !ok {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, fmt.Errorf("%s is written as %s, and it is text", keyPath(where, key), kindOf(v))
	}
	return s, true, nil
}

func boolAt(m map[string]any, key, where string) (bool, bool, error) {
	v, ok := m[key]
	if !ok {
		return false, false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, false, fmt.Errorf("%s is written as %s, and it is a boolean", keyPath(where, key), kindOf(v))
	}
	return b, true, nil
}

func intAt(m map[string]any, key, where string) (int, bool, error) {
	v, ok := m[key]
	if !ok {
		return 0, false, nil
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0, false, fmt.Errorf("%s is written as %s, and it is a whole number", keyPath(where, key), kindOf(v))
	}
	i, err := strconv.Atoi(n.String())
	if err != nil {
		return 0, false, fmt.Errorf("%s is %s, and it is a whole number", keyPath(where, key), n)
	}
	return i, true, nil
}

func durationAt(m map[string]any, key, where string) (Duration, error) {
	s, written, err := textAt(m, key, where)
	if err != nil {
		if _, ok := m[key]; ok {
			return 0, fmt.Errorf("%s is written as %s: %w", keyPath(where, key), kindOf(m[key]), durationRefusal(fmt.Sprint(m[key])))
		}
		return 0, err
	}
	if !written {
		return 0, nil
	}
	d, err := ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", keyPath(where, key), err)
	}
	return d, nil
}

func enumAt(m map[string]any, key, where string, allowed ...string) (string, error) {
	s, written, err := textAt(m, key, where)
	if err != nil {
		return "", err
	}
	if !written {
		return "", nil
	}
	if !slices.Contains(allowed, s) {
		return "", fmt.Errorf("%s is %q, and it is one of %s", keyPath(where, key), s, strings.Join(allowed, ", "))
	}
	return s, nil
}

func textsAt(m map[string]any, key, where string) ([]string, error) {
	raw, ok := m[key]
	if !ok {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is written as %s, and it is a list", keyPath(where, key), kindOf(raw))
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s carries %v, and every entry of it is text", keyPath(where, key), v)
		}
		out = append(out, s)
	}
	return out, nil
}

// listAt reads a list of blocks, and answers with nothing where the key is absent. A key
// written as anything but a list is refused here rather than passed over, because a
// trigger written as a block is a trigger that would never fire.
func listAt(m map[string]any, key, where string) ([]any, error) {
	raw, ok := m[key]
	if !ok {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is written as %s, and it is a list of entries", keyPath(where, key), kindOf(raw))
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("%s is an empty list, and a block that declares nothing is better left out", keyPath(where, key))
	}
	return list, nil
}

// documentAt returns one key as the JSON document it is, which is how a schema written in
// the file travels out of here: as bytes, for whoever holds a compiler and a tree.
func documentAt(m map[string]any, key, where string) (json.RawMessage, error) {
	v, ok := m[key]
	if !ok {
		return nil, nil
	}
	switch v.(type) {
	case map[string]any, bool:
	default:
		return nil, fmt.Errorf("%s is written as %s: a JSON Schema document is an object of keywords, or a boolean", keyPath(where, key), kindOf(v))
	}
	doc, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%s cannot be read as a JSON Schema document: %w", keyPath(where, key), err)
	}
	return doc, nil
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// jsonLike puts a decoded YAML value on JSON's own terms: every number a json.Number,
// every mapping keyed by text.
//
// The numbers matter twice over. A parameter is "validated against the manifest schema",
// which is a JSON Schema validator reading a JSON value, and a step's params reach the
// container as /agk/params.json, which is JSON. A YAML loader answering uint64 for 1
// would make both of those a conversion somebody has to remember.
func jsonLike(v any, where string) (any, error) {
	switch value := v.(type) {
	case map[string]any:
		converted := make(map[string]any, len(value))
		for k, sub := range value {
			c, err := jsonLike(sub, pathOf(where, k))
			if err != nil {
				return nil, err
			}
			converted[k] = c
		}
		return converted, nil
	case map[any]any:
		converted := make(map[string]any, len(value))
		for k, sub := range value {
			key, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("%s is keyed by %v, which is not text: every name in the file is written on one grammar, and a key that is not text cannot be on it", where, k)
			}
			c, err := jsonLike(sub, pathOf(where, key))
			if err != nil {
				return nil, err
			}
			converted[key] = c
		}
		return converted, nil
	case []any:
		converted := make([]any, len(value))
		for i, sub := range value {
			c, err := jsonLike(sub, fmt.Sprintf("%s[%d]", where, i))
			if err != nil {
				return nil, err
			}
			converted[i] = c
		}
		return converted, nil
	case uint64:
		return json.Number(strconv.FormatUint(value, 10)), nil
	case int64:
		return json.Number(strconv.FormatInt(value, 10)), nil
	case int:
		return json.Number(strconv.Itoa(value)), nil
	case float64:
		return json.Number(strconv.FormatFloat(value, 'g', -1, 64)), nil
	default:
		return v, nil
	}
}

func pathOf(where, key string) string {
	if where == "" {
		return key
	}
	return where + "." + key
}

// kindOf says what a value is in the file's own terms, so that a refusal about the wrong
// shape reads as the language and not as the reader: an author holding the file has
// written a number, a list or a block, and has never written a json.Number.
func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "nothing"
	case string:
		return "text"
	case bool:
		return "a boolean"
	case json.Number:
		return "a number"
	case []any:
		return "a list"
	case map[string]any:
		return "a block"
	}
	return "something the language does not write"
}

// keyPath names one key of one block the way the file writes it, with the one exception
// the root is: there is no name to put before a key written at the top of the file, so
// the root's own keys are named as the workflow's.
func keyPath(where, key string) string {
	if strings.HasPrefix(where, "the ") {
		return where + "'s " + key
	}
	return where + "." + key
}
