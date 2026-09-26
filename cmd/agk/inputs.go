package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"strings"

	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/schema"
)

// The workflow inputs, through package schema and nothing else.
//
// What this file does is read the command line. What a value has to look like, whether a run
// can start without one, what stands in when it is absent and what happens to a name the
// workflow never declared are all package schema's, applied before a run exists, which is
// what graph.Options.Inputs says it expects. The declaration is compiled by
// graph.Workflow.DeclaredInputs, which the API compiles it with too.

// bindInputs binds what a local run was given against the workflow's declaration, compiled against
// the tree: the same two calls the API binds a server run's inputs with, so that one set of inputs
// is one set of values wherever it runs.
func bindInputs(wf *graph.Workflow, tree fs.FS, supplied map[string]any) (map[string]any, error) {
	declared, err := wf.DeclaredInputs(tree)
	if err != nil {
		return nil, err
	}
	return schema.Bind(declared, supplied)
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
		if err := decodeJSON(raw, &document); err != nil {
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
	if err := decodeJSON([]byte(trimmed), &v); err == nil {
		return v
	}
	return trimmed
}

// decodeJSON reads one JSON document as json.Unmarshal does, except that a number is read as it
// was written, a json.Number: "a number written without a fraction or an exponent is an int in
// an expression, and any other a double", and the API reads a server run's inputs the same way,
// so --input n=3 is the int 3 in ${{ workflow.inputs.n + 1 }} wherever the run goes.
func decodeJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return err
	}
	// Unmarshal refuses what follows the document, and so does this: --input n='1 2' is the
	// text it is and not the number 1.
	if _, err := d.Token(); err != io.EOF {
		return errors.New("a second document follows the first")
	}
	return nil
}
