package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/schema"
)

// The workflow inputs, through package schema and nothing else.
//
// What this file does is read the command line. What a value has to look like, whether a run
// can start without one, what stands in when it is absent and what happens to a name the
// workflow never declared are all package schema's, applied before a run exists, which is
// what graph.Options.Inputs says it expects.

// declaredInputs compiles the schema of every declared input against the tree.
//
// The compiler is given the tree rather than a directory, so a { $ref: "./schemas/order.json" }
// resolves inside the commit it travelled with and a reference that leaves it is refused. On a
// laptop the commit is the working tree, which is the whole of what a local run means by
// pinned.
//
// An input with no schema is accepted as it comes, which is what a workflow says when the
// shape of a value is not its business.
func declaredInputs(wf *graph.Workflow, fsys fs.FS) (map[string]schema.Input, error) {
	if wf == nil {
		return nil, fmt.Errorf("there is no workflow to read the inputs of")
	}
	compiler := schema.NewCompiler(fsys)
	out := make(map[string]schema.Input, len(wf.Inputs))
	for _, name := range slices.Sorted(maps.Keys(wf.Inputs)) {
		in := wf.Inputs[name]
		declared := schema.Input{Required: in.Required, Default: in.Default}
		if len(in.Schema) > 0 && !isNull(in.Schema) {
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

// suppliedInputs reads the values the command line supplied.
//
// Three doors, and they are read in one order: the document of --inputs first, then each
// --input-file, then each --input, so that a name written twice is the last one winning, which
// is what a person retyping a flag means by it. The flags themselves keep the order they were
// written in.
//
// A value is read as JSON and falls back to the string it is. That direction is the only one
// that works: --input regions='["eu","us"]' is a list, and a command line that read every
// value as a string would make a list unwritable, while one that insisted on JSON would make
// --input cycle=2026-01 a parse error. The same rule reads a file, because a file holding a
// JSON document is the ordinary case and one holding a line of text is the other.
func suppliedInputs(values, files []string, doc string) (map[string]any, error) {
	out := map[string]any{}

	if doc != "" {
		raw, err := os.ReadFile(doc)
		if err != nil {
			return nil, fmt.Errorf("--inputs %s: %w", doc, err)
		}
		var document map[string]any
		if err := json.Unmarshal(raw, &document); err != nil {
			return nil, fmt.Errorf("--inputs %s is not a JSON object of input names and values: %w", doc, err)
		}
		maps.Copy(out, document)
	}

	for _, pair := range files {
		name, path, _ := strings.Cut(pair, "=")
		if name == "" {
			return nil, fmt.Errorf("--input-file %s names no input: it is written --input-file <name>=<path>", pair)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("--input-file %s: %w", name, err)
		}
		out[name] = valueOf(string(raw))
	}

	for _, pair := range values {
		name, value, _ := strings.Cut(pair, "=")
		if name == "" {
			return nil, fmt.Errorf("--input %s names no input: it is written --input <name>=<value>", pair)
		}
		out[name] = valueOf(value)
	}

	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// valueOf reads one value as JSON and falls back to the string it is.
//
// A trailing newline is taken off first, because a file written by an editor or by echo has
// one and a JSON document with one is the same document; a string that meant to end in a
// newline can be written through --inputs, where it is JSON and says so.
func valueOf(s string) any {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r")
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err == nil {
		return v
	}
	return trimmed
}

// isNull says whether a schema document is the JSON null, which the reader writes for a key
// that was present and empty. Compiling it would refuse a workflow the language accepts.
func isNull(doc []byte) bool { return strings.TrimSpace(string(doc)) == "null" }
