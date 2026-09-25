package graph

import (
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/schema"
)

// DeclaredInputs compiles the schema of every input the workflow declares against the tree of
// the commit it came from, into what schema.Bind binds a run's inputs against.
//
// It is here, beside the declaration, so that agk run --local, agk push and the API that starts a
// run on an installation all compile a declaration one way: a workflow whose inputs bind one way
// on a laptop and another on a server is two workflows. The tree is the caller's, as it is for
// every other use of it: the working tree on a laptop, the commit's own files at a push, and the
// version's tree out of the object store on an installation.
//
// The compiler is given the tree rather than a directory, so a { $ref: "./schemas/order.json" }
// resolves inside the commit it travelled with and a reference that leaves it is refused.
//
// An input with no schema is accepted as it comes, which is what a workflow says when the shape
// of a value is not its business.
func (wf *Workflow) DeclaredInputs(tree fs.FS) (map[string]schema.Input, error) {
	if wf == nil {
		return nil, fmt.Errorf("there is no workflow to read the inputs of")
	}
	compiler := schema.NewCompiler(tree)
	out := make(map[string]schema.Input, len(wf.Inputs))
	for _, name := range slices.Sorted(maps.Keys(wf.Inputs)) {
		in := wf.Inputs[name]
		declared := schema.Input{Required: in.Required, Default: in.Default}
		// The reader writes JSON null for a key that was present and empty, and compiling
		// that would refuse a workflow the language accepts.
		if len(in.Schema) > 0 && strings.TrimSpace(string(in.Schema)) != "null" {
			compiled, err := compiler.Compile(in.Schema)
			if err != nil {
				return nil, fmt.Errorf("the workflow input %s: %w", name, err)
			}
			declared.Schema = compiled
		}
		out[name] = declared
	}
	return out, nil
}
