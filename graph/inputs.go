package graph

import (
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/schema"
)

// InputSchemasMaxBytes is how much schema a workflow's declared inputs may compile, in all:
// each input's own schema, and each file it reaches, counted once for every input that reaches
// it.
//
// A declaration is compiled wherever a run's inputs are bound, which on an installation is at a
// push and at the first start of a version, on behalf of whoever holds workflow:write or
// workflow:run. The JSON Schema library's compile time grows faster than the square of a
// schema's subschemas: an object of bare properties took 0.1 s at 64 KiB, 0.33 s at 128 KiB,
// 2.5 s at 256 KiB and 12 s at 512 KiB, holding about 10 MiB at 128. So the bound is 128 KiB, and
// an input schema that describes what a person types or a trigger sends is kilobytes.
const InputSchemasMaxBytes = 128 << 10

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
	declared, _, err := wf.declare(tree)
	return declared, err
}

// InputFiles compiles the declaration as DeclaredInputs does, and answers each file of the tree
// the inputs' schemas reach by $ref, directly or through another, by its path, as parsed JSON:
// what a form needs beside the declaration to check a value before the run is asked for.
func (wf *Workflow) InputFiles(tree fs.FS) (map[string]any, error) {
	_, compiler, err := wf.declare(tree)
	if err != nil {
		return nil, err
	}
	return compiler.Reached(), nil
}

// declare compiles the declaration, and answers the compiler it was compiled with.
func (wf *Workflow) declare(tree fs.FS) (map[string]schema.Input, *schema.Compiler, error) {
	if wf == nil {
		return nil, nil, fmt.Errorf("there is no workflow to read the inputs of")
	}
	compiler := schema.NewCompilerWithin(tree, InputSchemasMaxBytes)
	out := make(map[string]schema.Input, len(wf.Inputs))
	for _, name := range slices.Sorted(maps.Keys(wf.Inputs)) {
		in := wf.Inputs[name]
		declared := schema.Input{Required: in.Required, Default: in.Default}
		// The reader writes JSON null for a key that was present and empty, and compiling
		// that would refuse a workflow the language accepts.
		if len(in.Schema) > 0 && strings.TrimSpace(string(in.Schema)) != "null" {
			compiled, err := compiler.Compile(in.Schema)
			if err != nil {
				return nil, nil, fmt.Errorf("the workflow input %s: %w", name, err)
			}
			declared.Schema = compiled
		}
		out[name] = declared
	}
	return out, compiler, nil
}
