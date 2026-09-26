package local

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
)

// What a run leaves behind, which is a question with an answer and not a matter of taste.
//
// A task's working directory is "created fresh, owned by an unprivileged account, and removed
// with the container, so no residue of one namespace survives into the next task on that
// host". The driver keeps that of the directory it owns, which is one task's own: run/step/
// attempt and not the run and the step above it. Those were nobody's, so a laptop that ran one
// workflow a hundred times was left with a hundred skeletons of empty directories, in the one
// directory of the layout that sits outside the working directory and that therefore nobody
// thinks to look in. A directory nothing clears is also a directory nobody notices has stopped
// being empty.

// oneStepWorkflow is the smallest run there is: one script step that writes one port.
const oneStepWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: tidy, namespace: finance }
outputs:
  out: { from: { step: only, port: out } }
steps:
  only:
    image: alpine:3.21
    outputs: [out]
    script:
      - |
        printf '{"meta":{"run_id":"%s","step":"%s","port":"out","attempt":%s,"count":1,"produced_at":"2026-01-01T00:00:00Z"},"items":[{"id":"one","data":{"n":1},"files":[]}]}' \
          "$AGK_RUN_ID" "$AGK_STEP" "$AGK_ATTEMPT" > /agk/out/ports/out.json
`

// TestARealRunLeavesNothingUnderTheWorkRoot closes the gap between the task the driver removes
// and the run this package opened.
//
// The moment is the close of the session, which for agk run --local is the end of the process:
// every container has been accounted for by then, so nothing under the work root is still
// somebody's. What is asserted is the run's own directory and every directory under it, because
// the skeleton is the whole of what was being left.
func TestARealRunLeavesNothingUnderTheWorkRoot(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "agentiik.yaml"), []byte(oneStepWorkflow), 0o644); err != nil {
		t.Fatalf("writing the entry point: %s", err)
	}
	session, layout := realSession(t, tree)

	wf, err := graph.Load(os.DirFS(tree), "agentiik.yaml", nil)
	if err != nil {
		t.Fatalf("loading the workflow: %s", err)
	}
	g, err := session.Resolve(t.Context(), wf)
	if err != nil {
		t.Fatalf("resolving the graph: %s", err)
	}

	out, err := session.Run(t.Context(), Request{Graph: g, Tree: tree})
	if err != nil {
		t.Fatalf("running the graph: %s", err)
	}
	if out.Run.State != agk.Succeeded {
		t.Fatalf("the run is %s: %+v", out.Run.State, out.Failures)
	}

	// The session is closed here rather than left to the cleanup, because closing it is
	// what ends a local run and the assertion is about what an ended run leaves.
	if err := session.Close(); err != nil {
		t.Fatalf("closing the session: %s", err)
	}

	if left := entriesUnder(t, layout.Work(out.Run.ID)); len(left) > 0 {
		t.Errorf("%s still holds %v after the run: the run and step directories above a task's own are this package's to clear, because this package named the work root", layout.Work(out.Run.ID), left)
	}
	if _, err := os.Stat(layout.Work(out.Run.ID)); !os.IsNotExist(err) {
		t.Errorf("%s is still there after the run", layout.Work(out.Run.ID))
	}

	// What a run is meant to leave is still there, which is the line that catches a
	// cleanup reaching too far. The records, the outputs and the object store are the
	// whole answer a person keeps.
	for _, path := range []string{
		layout.RunFile(out.Run.ID),
		layout.State(out.Run.ID),
		filepath.Join(layout.Outputs(out.Run.ID), "out.json"),
		layout.Objects(),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s is not there: %s", path, err)
		}
	}
}

// TestAWorkDirectoryWithSomethingInItIsLeftToBeFound is the other side of that rule, and the
// reason it is os.Remove and not os.RemoveAll.
//
// A task's directory the driver could not take away is evidence, and a second run of the same
// working directory still holding its tasks is in use. Neither is the close of one session's
// business, and a cleanup that could not tell them apart would either delete the evidence or
// pull the directory out from under the other run.
func TestAWorkDirectoryWithSomethingInItIsLeftToBeFound(t *testing.T) {
	tree := t.TempDir()
	layout, err := NewLayout(filepath.Join(tree, DefaultDir))
	if err != nil {
		t.Fatalf("the layout: %s", err)
	}

	// A task directory with a file in it, as a task whose removal failed would leave.
	held := filepath.Join(layout.Work("01JMZ8W4K2R7Q0E3N5T9A1B2C3"), "only", "1")
	if err := os.MkdirAll(held, 0o700); err != nil {
		t.Fatal(err)
	}
	residue := filepath.Join(held, "params.json")
	if err := os.WriteFile(residue, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// An empty skeleton beside it, as an ended run leaves.
	empty := filepath.Join(layout.Work("01JMZ8W4K2R7Q0E3N5T9A1B2C4"), "only", "1")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}

	layout.pruneWork()

	if _, err := os.Stat(residue); err != nil {
		t.Errorf("%s was taken away: a task directory the driver could not remove is a thing worth finding, and a prune that deleted it would delete the evidence", residue)
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Errorf("%s is still there: an empty skeleton is what an ended run leaves and what this clears", empty)
	}
}

// entriesUnder names what a directory still holds, so that a failure says what was left rather
// than only that something was.
func entriesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil || rel == "." {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	return out
}
