package main

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/agentiik/agentiik/cmd/agk/internal/draw"
	"github.com/agentiik/agentiik/graph"
)

// agk graph: "Writes the resolved graph as DOT or Mermaid, for review inside a merge
// request."
//
// It draws from the workflow after Load and Check and deliberately not from a graph.Graph.
// Build needs the brick manifests, reading a manifest means pulling an image, and the person
// reading a merge request has no daemon: a drawing that needed one would be a drawing nobody
// could produce in the place it is wanted. That rule is an import list rather than a
// convention, since internal/draw cannot reach the driver.
//
// The command is called drawing here because graph is the name of the package that resolves
// one, and a function shadowing it in this file would make the two impossible to read
// together.

// The two languages a drawing is written in.
const (
	formatDOT     = "dot"
	formatMermaid = "mermaid"
)

func drawing(_ context.Context, e Env, args []string) int {
	fs := flags(e, "agk graph", "agk graph [-f <path>] [--format dot|mermaid] [-o <path>]")
	entry := fs.String("f", "", "The entry point to draw. Defaults to "+entryPoint+" in the directory the command is run in.")
	format := fs.String("format", formatDOT, "dot writes a digraph with a port on each side of every step; mermaid writes a flowchart LR with the ports on the edges.")
	out := fs.String("o", "", "The file to write the drawing to. Defaults to standard output, so that agk graph | dot works.")
	if code, ok := parse(fs, args); !ok {
		return code
	}

	var write func(*bytes.Buffer, *graph.Workflow) error
	switch *format {
	case formatDOT:
		write = func(b *bytes.Buffer, wf *graph.Workflow) error { return draw.DOT(b, wf) }
	case formatMermaid:
		write = func(b *bytes.Buffer, wf *graph.Workflow) error { return draw.Mermaid(b, wf) }
	default:
		fmt.Fprintf(e.Err, "--format is %q: it is %s or %s\n", *format, formatDOT, formatMermaid)
		return exitUsage
	}

	wf, _, _, err := load(e, *entry)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}

	// The whole drawing is built before anything is written, so that a file named with
	// -o is either the drawing or what it was before, and never half of one.
	var b bytes.Buffer
	if err := write(&b, wf); err != nil {
		refusal(e.Err, err)
		return exitRefused
	}

	if *out == "" {
		if _, err := e.Out.Write(b.Bytes()); err != nil {
			fmt.Fprintf(e.Err, "the drawing could not be written: %v\n", err)
			return exitNoOutcome
		}
		return exitSucceeded
	}

	path := e.path(*out)
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		fmt.Fprintf(e.Err, "%s could not be written: %v\n", path, err)
		return exitNoOutcome
	}
	// The answer is the file, so what is said about it is said on standard error: a
	// drawing on standard output and a sentence about it on the same stream is a pipe
	// into dot that fails.
	fmt.Fprintf(e.Err, "%s written as %s: %s, %s\n", path, *format,
		counted(len(wf.Steps), "step", "steps"), counted(edges(wf), "edge", "edges"))
	return exitSucceeded
}

// edges counts the inbound edges of every step, which is the number of lines the drawing
// carries between two boxes.
func edges(wf *graph.Workflow) int {
	n := 0
	for _, st := range wf.Steps {
		n += len(st.Needs)
	}
	return n
}
