package graph

import (
	"fmt"
	"regexp"
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
	if err := identifier(namespace, "the namespace", where); err != nil {
		return r, err
	}
	if err := identifier(name, "the workflow", where); err != nil {
		return r, err
	}
	r.Namespace, r.Name = namespace, name
	return r, nil
}
