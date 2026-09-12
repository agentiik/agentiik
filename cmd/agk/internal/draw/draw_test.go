package draw

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/graph"
)

// update rewrites the golden files. A drawing is bytes, and the only honest way to review a
// change to it is to read the diff of the file it produces.
var update = flag.Bool("update", false, "rewrite the golden drawings in testdata")

// fixture is the workflow under testdata, loaded and checked exactly as agk graph loads it:
// Load resolves the include and the hidden block the invoice step extends, and Check holds
// the graph together. Build is deliberately not called, because it needs the brick manifests
// and reading one means pulling an image, which the person reading a merge request has no
// daemon for.
func fixture(t *testing.T) *graph.Workflow {
	t.Helper()
	wf, err := graph.Load(os.DirFS("testdata"), "agentiik.yaml", nil)
	if err != nil {
		t.Fatalf("loading the fixture: %v", err)
	}
	if err := graph.Check(wf); err != nil {
		t.Fatalf("the fixture does not hold together: %v", err)
	}
	return wf
}

func TestTheDrawingsAreTheBytesTheGoldenFilesHold(t *testing.T) {
	wf := fixture(t)
	for _, c := range []struct {
		name  string
		write func(*bytes.Buffer, *graph.Workflow) error
		file  string
	}{
		{"DOT", func(b *bytes.Buffer, wf *graph.Workflow) error { return DOT(b, wf) }, "monthly-invoicing.dot"},
		{"Mermaid", func(b *bytes.Buffer, wf *graph.Workflow) error { return Mermaid(b, wf) }, "monthly-invoicing.mmd"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got bytes.Buffer
			if err := c.write(&got, wf); err != nil {
				t.Fatalf("writing: %v", err)
			}
			path := filepath.Join("testdata", c.file)
			if *update {
				if err := os.WriteFile(path, got.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("rewrote %s", path)
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v. Run go test -update to write it", err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Errorf("the drawing is not what %s holds:\n%s", path, got.String())
			}
		})
	}
}

// TestTwoDrawingsOfOneFileAreTheSameBytes is why the order is sorted: a diff of two drawings
// has to be a diff of the graph, and a map iterated twice would make it a diff of nothing.
func TestTwoDrawingsOfOneFileAreTheSameBytes(t *testing.T) {
	for _, write := range []func(*bytes.Buffer, *graph.Workflow) error{
		func(b *bytes.Buffer, wf *graph.Workflow) error { return DOT(b, wf) },
		func(b *bytes.Buffer, wf *graph.Workflow) error { return Mermaid(b, wf) },
	} {
		var first, second bytes.Buffer
		// Loaded twice as well, so that nothing rests on one map's iteration order
		// happening to repeat inside one process.
		if err := write(&first, fixture(t)); err != nil {
			t.Fatal(err)
		}
		if err := write(&second, fixture(t)); err != nil {
			t.Fatal(err)
		}
		if first.String() != second.String() {
			t.Errorf("two drawings of one file differ:\n%s\n%s", first.String(), second.String())
		}
	}
}

// TestBothWritersNameEveryStepEveryPortEveryEdgeAndBothBoundaries is the claim a golden file
// cannot make on its own: that the drawing is of this graph and not of a subset of it.
func TestBothWritersNameEveryStepEveryPortEveryEdgeAndBothBoundaries(t *testing.T) {
	wf := fixture(t)
	for _, c := range []struct {
		name  string
		write func(*bytes.Buffer, *graph.Workflow) error
		edges []string
	}{
		{
			name:  "DOT",
			write: func(b *bytes.Buffer, wf *graph.Workflow) error { return DOT(b, wf) },
			edges: []string{
				`"normalize":out_ok:e -> "invoice":in_in:w`,
				`"invoice":out_out:e -> "archive":in_invoices:w`,
				`"normalize":out_rejected:e -> "archive":in_invoices:w`,
				`"archive":out_out:e -> "audit-log":in_in:w`,
				`"input:orders" -> "normalize":in_orders:w [style=dashed]`,
				`"archive":out_out:e -> "output:invoices" [style=dashed]`,
				`"invoice":out_error:e -> "output:errors" [style=dashed]`,
			},
		},
		{
			name:  "Mermaid",
			write: func(b *bytes.Buffer, wf *graph.Workflow) error { return Mermaid(b, wf) },
			edges: []string{
				`step_normalize -->|"ok to in"| step_invoice`,
				`step_invoice -->|"out to invoices"| step_archive`,
				`step_normalize -->|"rejected to invoices"| step_archive`,
				`step_archive -->|"out to in"| step_audit_log`,
				`input_orders -. "orders" .-> step_normalize`,
				`step_archive -. "out" .-> output_invoices`,
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			var b bytes.Buffer
			if err := c.write(&b, wf); err != nil {
				t.Fatal(err)
			}
			got := b.String()

			for _, step := range []string{"normalize", "invoice", "archive", "audit-log"} {
				if !strings.Contains(got, step) {
					t.Errorf("the step %s is not drawn", step)
				}
			}
			for _, port := range []string{"ok", "rejected", "error", "invoices", "customers"} {
				if !strings.Contains(got, port) {
					t.Errorf("the port %s is not drawn", port)
				}
			}
			for _, edge := range c.edges {
				if !strings.Contains(got, edge) {
					t.Errorf("the edge %s is not drawn:\n%s", edge, got)
				}
			}
			// The keywords that belong inside the box rather than beside it.
			if !strings.Contains(got, "fan_out: item") {
				t.Errorf("the fan-out is not drawn inside the step it belongs to")
			}
			if !strings.Contains(got, "merge: zip") {
				t.Errorf("the merge is not drawn on the step whose port two edges feed")
			}
			if !strings.Contains(got, "script on alpine:3.21") {
				t.Errorf("a script step does not say that its image is a base image")
			}
			// The condition travels with its comparison intact, which is what the
			// escaping is for in both languages.
			if strings.Contains(got, "inputs.in.count > 0") {
				t.Errorf("the condition carries a raw > , which both renderers read as markup")
			}
			if !strings.Contains(got, "if: ") {
				t.Errorf("the condition is not drawn")
			}
		})
	}
}

// TestAWorkflowWithNoStepsDrawsTheBoundaryAndNothingElse, because a drawing of an empty
// graph is a drawing and not an error, and because both writers have to close what they
// open.
func TestAWorkflowWithNoStepsDrawsTheBoundaryAndNothingElse(t *testing.T) {
	wf, err := graph.Parse([]byte("apiVersion: agentiik.dev/v1\nkind: Workflow\nmetadata:\n  name: empty\n  namespace: finance\nsteps: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	var dot, mmd bytes.Buffer
	if err := DOT(&dot, wf); err != nil {
		t.Fatal(err)
	}
	if err := Mermaid(&mmd, wf); err != nil {
		t.Fatal(err)
	}
	if got := dot.String(); !strings.HasPrefix(got, `digraph "finance/empty" {`) || !strings.HasSuffix(got, "}\n") {
		t.Errorf("the empty digraph reads %q", got)
	}
	if got := mmd.String(); got != "flowchart LR\n" {
		t.Errorf("the empty flowchart reads %q", got)
	}
}

// TestTwoNamesThatSanitiseAlikeAreTwoNodes, because a name is letters, digits, hyphens and
// underscores, and a Mermaid identifier is letters, digits and underscores: start-date and
// start_date are two declared inputs of one workflow that sanitise to one word.
//
// A drawing that answered the second with the first's node would hang both edges off one
// box, which is a drawing of a workflow with one input where the file declares two. It is
// the same rule the steps already follow, asked of the boundary.
func TestTwoNamesThatSanitiseAlikeAreTwoNodes(t *testing.T) {
	wf, err := graph.Parse([]byte(`
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: boundary
  namespace: finance
inputs:
  start-date: {}
  start_date: {}
steps:
  one:
    image: example/one:1
    inputs:
      from: ${{ workflow.inputs.start-date }}
      to: ${{ workflow.inputs.start_date }}
    outputs: [ok]
  fan-out:
    image: example/two:1
    needs:
      - { step: one, port: ok, as: in }
    outputs: [ok]
  fan_out:
    image: example/two:1
    needs:
      - { step: one, port: ok, as: in }
    outputs: [ok]
outputs:
  a-report:
    from: { step: fan-out, port: ok }
  a_report:
    from: { step: fan_out, port: ok }
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Check(wf); err != nil {
		t.Fatalf("the fixture does not hold together: %v", err)
	}

	var b bytes.Buffer
	if err := Mermaid(&b, wf); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	t.Logf("\n%s", got)

	// Every declared name is drawn, and no two of them share a box: six nodes for two
	// inputs, three steps and two outputs, which is one identifier each.
	ids := map[string]bool{}
	for _, line := range strings.Split(got, "\n") {
		line = strings.TrimSpace(line)
		for _, open := range []string{"([", "["} {
			if id, _, found := strings.Cut(line, open); found && id != "" && !strings.Contains(id, " ") {
				ids[id] = true
				break
			}
		}
	}
	if len(ids) != 7 {
		t.Errorf("the drawing names %d nodes, %v, and the file declares two inputs, three steps and two outputs", len(ids), ids)
	}
	for _, name := range []string{`"start-date"`, `"start_date"`, `"a-report"`, `"a_report"`} {
		if strings.Count(got, name) != 1 {
			t.Errorf("%s is drawn %d times, and it is one declared name of the file", name, strings.Count(got, name))
		}
	}
}

func TestThereIsNoWorkflowToDraw(t *testing.T) {
	var b bytes.Buffer
	if err := DOT(&b, nil); err == nil {
		t.Error("a drawing of nothing was written")
	}
	if err := Mermaid(&b, nil); err == nil {
		t.Error("a drawing of nothing was written")
	}
}
