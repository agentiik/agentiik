package graph

import (
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

// portLengths refuses a port or a workflow output written past agk.PortMaxBytes: "a port or a
// workflow output at most 250", since each becomes the file <name>.json and no filesystem holds
// that name past 255 characters with its suffix.
//
// It runs over what one document wrote, as it wrote it, which is every place a port is named: the
// workflow outputs and the port each is taken from, a tool's output, and in each step and each
// hidden block the ports it declares, the inputs it feeds and both ends of every edge. The grammar
// and the 255 characters are the reader's already, so this is the one bound a version already
// stored is read back without (LoadStored).
func portLengths(outputs map[string]Output, mcp *MCP, values map[agk.Step]stepValues, blocks map[string]stepValues) error {
	for _, name := range slices.Sorted(maps.Keys(outputs)) {
		if err := portLength(name, "the output", "outputs"); err != nil {
			return err
		}
		if err := portLength(string(outputs[name].From.Port), "the port", "outputs."+name+".from"); err != nil {
			return err
		}
	}
	if mcp != nil {
		for i, t := range mcp.Tools {
			if t.Output != nil {
				if err := portLength(t.Output.Output, "the workflow output", fmt.Sprintf("mcp.tools[%d].output.from", i)); err != nil {
					return err
				}
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(values)) {
		if err := stepPortLengths(values[name], "steps."+string(name)); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(blocks)) {
		if err := stepPortLengths(blocks[name], name); err != nil {
			return err
		}
	}
	return nil
}

// stepPortLengths is portLengths over one step or one hidden block.
func stepPortLengths(s stepValues, where string) error {
	for _, port := range s.Outputs {
		if err := portLength(string(port), "the port", where+".outputs"); err != nil {
			return err
		}
	}
	for _, port := range slices.Sorted(maps.Keys(s.Inputs)) {
		if err := portLength(string(port), "the port", where+".inputs"); err != nil {
			return err
		}
	}
	for i, e := range s.Needs {
		at := fmt.Sprintf("%s.needs[%d]", where, i)
		if err := portLength(string(e.Port), "the port", at); err != nil {
			return err
		}
		if err := portLength(string(e.As), "the port", at); err != nil {
			return err
		}
	}
	return nil
}

// portLength refuses one name past agk.PortMaxBytes, saying what it named and where, and
// printing the start of it rather than all of it, as identifier does.
func portLength(name, what, where string) error {
	if len(name) <= agk.PortMaxBytes {
		return nil
	}
	return fmt.Errorf("%s names %s %.64s..., which is %d characters long: a port or a workflow output is at most %d, since it becomes the file <name>.json and no filesystem holds that name past %d characters", where, what, name, len(name), agk.PortMaxBytes, agk.IdentifierMaxBytes)
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
// It is comparable, because it is the key an already fetched include arrives under: the
// evaluator never reaches another repository, so a caller resolves the ref and hands the
// fragment over.
type WorkflowRef struct {
	Namespace string
	Name      string
	Ref       string
}

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
