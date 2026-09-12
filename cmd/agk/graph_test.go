package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheDrawingIsTheOnlyThingOnStandardOutput is what makes agk graph | dot work.
func TestTheDrawingIsTheOnlyThingOnStandardOutput(t *testing.T) {
	for _, c := range []struct {
		format string
		starts string
	}{
		{formatDOT, "digraph \"finance/reading-commands\" {"},
		{formatMermaid, "flowchart LR"},
	} {
		t.Run(c.format, func(t *testing.T) {
			e, out, errs := reading(t)
			code := run(t.Context(), e, []string{"graph", "-f", "testdata/scripted/agentiik.yaml", "--format", c.format})
			if code != exitSucceeded {
				t.Fatalf("the exit code is %d: %s", code, errs)
			}
			if !strings.HasPrefix(out.String(), c.starts) {
				t.Errorf("the drawing starts %q and it starts %q", first(out.String()), c.starts)
			}
			if errs.String() != "" {
				t.Errorf("something was said on standard error beside a drawing that went to a pipe: %s", errs)
			}
			// The graph is drawn from the file after Load and Check, so what the
			// include carried is in it.
			for _, want := range []string{"normalize", "count", "ok", "orders", "counted"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("the drawing does not name %s", want)
				}
			}
		})
	}
}

func TestAFormatThatIsNeitherIsTheCommandLineBeingWrong(t *testing.T) {
	e, out, errs := reading(t)
	code := run(t.Context(), e, []string{"graph", "--format", "svg"})
	if code != exitUsage {
		t.Fatalf("the exit code is %d and a command line that is wrong leaves with %d", code, exitUsage)
	}
	if out.String() != "" {
		t.Errorf("a refused format wrote to standard output: %s", out)
	}
	for _, want := range []string{"svg", formatDOT, formatMermaid} {
		if !strings.Contains(errs.String(), want) {
			t.Errorf("the refusal does not name %q: %s", want, errs)
		}
	}
}

// TestWithAnOutputFileTheAnswerIsTheFile, so what is said about it is said on standard error.
func TestWithAnOutputFileTheAnswerIsTheFile(t *testing.T) {
	e, out, errs := reading(t)
	path := filepath.Join(t.TempDir(), "graph.dot")
	code := run(t.Context(), e, []string{"graph", "-f", "testdata/scripted/agentiik.yaml", "-o", path})
	if code != exitSucceeded {
		t.Fatalf("the exit code is %d: %s", code, errs)
	}
	if out.String() != "" {
		t.Errorf("the drawing went to the file and to standard output: %s", out)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(written), "digraph") {
		t.Errorf("%s does not hold a digraph: %q", path, first(string(written)))
	}
	for _, want := range []string{path, "2 steps", "1 edge"} {
		if !strings.Contains(errs.String(), want) {
			t.Errorf("the line about the file does not say %q: %s", want, errs)
		}
	}
}

// TestARefusedWorkflowIsNotDrawn: a drawing of a graph that is not a graph would be a picture of
// something the engine refuses to run.
func TestARefusedWorkflowIsNotDrawn(t *testing.T) {
	e, out, errs := reading(t)
	code := run(t.Context(), e, []string{"graph", "-f", "testdata/cyclic/agentiik.yaml"})
	if code != exitRefused {
		t.Fatalf("the exit code is %d: %s", code, errs)
	}
	if out.String() != "" {
		t.Errorf("a refused workflow was drawn: %s", out)
	}
	if !strings.Contains(errs.String(), "cycle-in-graph") {
		t.Errorf("the refusal does not name the rule: %s", errs)
	}
}

// TestTwoDrawingsOfOneFileAreTheSameBytes through the command line, which is where a person
// commits one beside the file it describes.
func TestTwoDrawingsOfOneFileAreTheSameBytes(t *testing.T) {
	first, _, _ := reading(t)
	second, _, _ := reading(t)
	a, b := first.Out, second.Out
	if code := run(t.Context(), first, []string{"graph", "-f", "testdata/scripted/agentiik.yaml"}); code != exitSucceeded {
		t.Fatal(code)
	}
	if code := run(t.Context(), second, []string{"graph", "-f", "testdata/scripted/agentiik.yaml"}); code != exitSucceeded {
		t.Fatal(code)
	}
	if a.(interface{ String() string }).String() != b.(interface{ String() string }).String() {
		t.Error("two drawings of one file differ, and a diff of two drawings has to be a diff of the graph")
	}
}

// first is the first line of something written, for a message about what it starts with.
func first(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
