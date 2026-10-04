package graph

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/agk"
)

// The two grammars every name in the file is written on.
//
// "Names in the file are written on one grammar: ^[A-Za-z0-9][A-Za-z0-9_-]*$, letters,
// digits, hyphens and underscores, beginning with a letter or a digit. It governs the
// workflow name and its namespace, each half of a <namespace>/<name> reference, a step,
// an input, an output, a port, a variable, a secret and a published tool name, so that
// one name survives a URL, a directory and a tool list unchanged."
//
// "Parameter names are the exception and are narrower, ^[A-Za-z_][A-Za-z0-9_]*$, with no
// hyphen, in a step's params and in the manifest that declares them alike: the name is
// exported as AGK_PARAM_<NAME> to a script, so api-key is refused."
//
// The same two are written in package agk for a step and a port and in package brick for
// a manifest's own names. They are written again here rather than reached for because
// what is held to them here is an input, an output, a variable, a secret and a tool, and
// neither of those packages knows those exist.
var (
	identifierName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	parameterName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	hiddenName     = regexp.MustCompile(`^\.[A-Za-z0-9][A-Za-z0-9_-]*$`)
)

// identifier refuses a name that is not written on the one grammar, or is longer than a
// directory holds a name to, saying what kind of name it was and where it was written.
// The bound is agk's, reached for rather than written again, since it is a number and
// not a grammar: a name survives a directory unchanged only if it fits in one.
func identifier(name, what, where string) error {
	if len(name) > agk.IdentifierMaxBytes {
		return fmt.Errorf("%s names %s %.64s..., which is %d characters long: a name is at most %d, so that one name survives a URL, a directory and a tool list unchanged, and no filesystem holds a longer one", where, what, name, len(name), agk.IdentifierMaxBytes)
	}
	if identifierName.MatchString(name) {
		return nil
	}
	return fmt.Errorf("%s names %s %q, which is not an identifier: a name is letters, digits, hyphens and underscores, beginning with a letter or a digit, so that one name survives a URL, a directory and a tool list unchanged", where, what, name)
}

// newRules are the rules added since a version could be stored, applied to one entry point
// where a version is made and never where a stored one is read back (LoadStored): a stored
// version keeps rebuilding, and its runs and replays keep going, after an upgrade.
//
// Four of them. The mcp block is one tool, "one block, one tool, whose arguments are the workflow's
// inputs", where a version stored before v0.7.0 may list several and is read back as publishing
// none. A port or a workflow output is written at most agk.PortMaxBytes, "since each
// becomes the file <name>.json and no filesystem holds that name past 255 characters with its
// suffix", which is checked in every place a port is named: the workflow outputs and the port
// each is taken from, the tool's output, and in each step and each hidden block the ports it
// declares, the inputs it feeds and both ends of every edge. And a file relocated by the long
// form of files is relocated to an absolute path, since "a relative path has nothing inside a
// container to be relative to", which is why the runner refuses one and the task message holds
// it to the same grammar, and a selector names a path inside the tree, as a glob that reads.
func (w *Workflow) newRules() error {
	at := origin{src: w.src}
	for _, name := range slices.Sorted(maps.Keys(w.Outputs)) {
		if len(name) > agk.PortMaxBytes {
			r := refuse(RuleWorkflowOutputPastBound, "", "", portPastBound("outputs", "the output", name))
			r.At = at.at("outputs", name).key()
			return r
		}
		if port := w.Outputs[name].From.Port; len(port) > agk.PortMaxBytes {
			r := refuse(RulePortPastBound, w.Outputs[name].From.Step, "", portPastBound("outputs."+name+".from", "the port", string(port)))
			r.At = at.at("outputs", name, "from", "port").value()
			return r
		}
	}
	if w.mcpRefused != nil {
		var r *Refusal
		if errors.As(w.mcpRefused, &r) && r.At == (Position{}) {
			r.At = at.at("mcp").key()
		}
		return w.mcpRefused
	}
	if w.MCP != nil && len(w.MCP.Output) > agk.PortMaxBytes {
		r := refuse(RulePortPastBound, "", "", portPastBound("mcp.output", "the workflow output", w.MCP.Output))
		r.At = at.at("mcp", "output").value()
		return r
	}
	if err := w.newTriggerRules(); err != nil {
		return err
	}
	return newStepRules(w.values, w.blocks, w.Defaults)
}

// newStepRules is newRules over the steps, the hidden blocks and the defaults one document
// writes, which is all of it an included file may carry.
func newStepRules(values map[agk.Step]stepValues, blocks map[string]stepValues, defaults Defaults) error {
	for _, name := range slices.Sorted(maps.Keys(values)) {
		if err := stepPortLengths(name, values[name], "steps."+string(name)); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(blocks)) {
		if err := stepPortLengths("", blocks[name], name); err != nil {
			return err
		}
	}
	if err := relocatedTo(defaults, "defaults"); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(values)) {
		if err := relocatedTo(values[name].Defaults, "steps."+string(name)); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(blocks)) {
		if err := relocatedTo(blocks[name].Defaults, name); err != nil {
			return err
		}
	}
	return nil
}

// stepPortLengths is the port bound over one step or one hidden block.
func stepPortLengths(step agk.Step, s stepValues, where string) error {
	past := func(port agk.Port, what, at string, node Position) error {
		r := refuse(RulePortPastBound, step, "", portPastBound(at, what, string(port)))
		r.At = node
		return r
	}
	for i, port := range s.Outputs {
		if len(port) > agk.PortMaxBytes {
			return past(port, "the port", where+".outputs", s.written["outputs"].at("outputs", i).value())
		}
	}
	for _, port := range slices.Sorted(maps.Keys(s.Inputs)) {
		if len(port) > agk.PortMaxBytes {
			return past(port, "the port", where+".inputs", s.written["inputs"].at("inputs", string(port)).key())
		}
	}
	for i, e := range s.Needs {
		at := fmt.Sprintf("%s.needs[%d]", where, i)
		edge := s.written["needs"].at("needs", i)
		if len(e.Port) > agk.PortMaxBytes {
			return past(e.Port, "the port", at, edge.at("port").value())
		}
		if len(e.As) > agk.PortMaxBytes {
			return past(e.As, "the port", at, edge.at("as").value())
		}
	}
	return nil
}

// portPastBound says what named a port past agk.PortMaxBytes and where, printing the start of it
// rather than all of it, as identifier does.
func portPastBound(where, what, name string) string {
	return fmt.Sprintf("%s names %s %.64s..., which is %d characters long: a port or a workflow output is at most %d, since it becomes the file <name>.json and no filesystem holds that name past %d characters", where, what, name, len(name), agk.PortMaxBytes, agk.IdentifierMaxBytes)
}

// relocatedTo refuses a file relocated to a relative path, and a selector no tree can be matched
// against: one leaving the tree, or a glob whose set is not closed. Each would otherwise be found
// by the first task of the step, after the version was accepted.
func relocatedTo(d Defaults, where string) error {
	for i, f := range d.Files {
		if f.To != "" && !strings.HasPrefix(f.To, "/") {
			return fmt.Errorf("%s.files[%d] relocates %s to %q, and a file is relocated to an absolute path: a relative path has nothing inside a container to be relative to, which is why the runner refuses one", where, i, f.From, f.To)
		}
		if _, err := compileSelector(f); err != nil {
			return fmt.Errorf("%s.files[%d]: %w", where, i, err)
		}
	}
	return nil
}

// namespaceName holds a namespace to the identifier grammar and refuses the words the API
// routes on, which no namespace can be named after.
//
// A word reserved after namespaces could be created under it is read: an installation may hold
// a namespace created under it before, and a version of one of its workflows is read again
// every time a run of it is evaluated, so refusing it here would fail runs an upgrade has to
// leave going (agk.LateReservations). Creating a namespace of that name is refused where a
// namespace is created.
func namespaceName(name, where string) error {
	if err := identifier(name, "the namespace", where); err != nil {
		return err
	}
	if agk.NamesNoNamespace(name) {
		words := slices.DeleteFunc(slices.Clone(agk.ReservedNamespaces), func(w string) bool { return !agk.NamesNoNamespace(w) })
		return fmt.Errorf("%s names the namespace %q, a word the API routes on: the first path segment after /api/v1/ decides the route, so %s cannot name a namespace", where, name, strings.Join(words, ", "))
	}
	return nil
}

// parameter refuses a parameter name that could not become AGK_PARAM_<NAME>.
func parameter(name, where string) error {
	if parameterName.MatchString(name) {
		return nil
	}
	return fmt.Errorf("%s names the parameter %q, which is not an identifier: a parameter name is a key of /agk/params.json and is exported as AGK_PARAM_<NAME> when the step runs a script, so it cannot carry a hyphen", where, name)
}

// hidden says whether a name is a hidden block: "a hidden block is exactly a name that
// starts with a dot and is never executed". A hidden block may sit at the root of the
// entry point, at the root of an included file, or among the steps.
func hidden(name string) bool { return strings.HasPrefix(name, ".") }

// WorkflowRef names one workflow, and a ref of it where one is pinned. It is written
// <namespace>/<name> everywhere it appears, with @<ref> appended in the short form of a
// sub-workflow call.
//
// It is comparable, and it is what a workflow include asks Remote for: the evaluator never
// reaches another repository, so a caller resolves the ref and hands over that repository's tree
// at the commit it resolved to.
type WorkflowRef struct {
	Namespace string
	Name      string
	Ref       string
}

// String is the reference as <namespace>/<name>@<ref>, the ref left out where there is none.
func (r WorkflowRef) String() string { return r.text() }

// text writes the reference back the way the file writes it.
func (r WorkflowRef) text() string {
	s := r.Namespace + "/" + r.Name
	if r.Ref != "" {
		s += "@" + r.Ref
	}
	return s
}

// parseWorkflowRef reads <namespace>/<name>, with an optional @<ref>, and holds each
// half to the one grammar.
func parseWorkflowRef(s, where string) (WorkflowRef, error) {
	var r WorkflowRef
	path, ref, pinned := strings.Cut(s, "@")
	if pinned {
		if ref == "" {
			return r, fmt.Errorf("%s pins the workflow %q to nothing: a ref is a tag or a commit", where, s)
		}
		r.Ref = ref
	}
	namespace, name, ok := strings.Cut(path, "/")
	if !ok {
		return r, fmt.Errorf("%s names the workflow %q: a workflow is named <namespace>/<name>, because a workflow belongs to exactly one namespace", where, s)
	}
	if err := namespaceName(namespace, where); err != nil {
		return r, err
	}
	if err := identifier(name, "the workflow", where); err != nil {
		return r, err
	}
	r.Namespace, r.Name = namespace, name
	return r, nil
}
