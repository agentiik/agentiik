package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
)

// "A port or a workflow output at most 250", since each becomes the file <name>.json: refused where
// a version is made and never where one already stored is read back.

// aPortPastItsBound is a port of 251 characters, which a version stored before the bound may name.
var aPortPastItsBound = "rejected-" + strings.Repeat("x", 242)

// pushedAsWritten is a push of document and manifest as they are written, with nothing on this side
// reading them first, which is what a push from an agk older than the bound sends.
func pushedAsWritten(document, manifest string) api.Push {
	return api.Push{
		Entry: "agentiik.yaml", Document: []byte(document),
		Manifests: map[string][]byte{image: []byte(manifest)}, Branch: "main",
		Tree: map[string]api.PushFile{"agentiik.yaml": {Content: []byte(document), Mode: "0644"}},
	}
}

// Past the bound in the workflow file or in a manifest the push carries, the version is refused
// with 422, saying the bound, before any of its tree is stored.
func TestAPortPastItsBoundIsRefusedAtThePushBeforeAnythingIsStored(t *testing.T) {
	h, pool, _, objects := servingWithObjects(t)
	manifest := strings.Replace(brickManifest, "    rejected: {}\n", "    rejected: {}\n    "+aPortPastItsBound+": {}\n", 1)
	for what, document := range map[string]string{
		"a port the workflow file declares": strings.Replace(workflowDocument, "outputs: [ok, rejected]", "outputs: [ok, "+aPortPastItsBound+"]", 1),
		"a port the manifest declares":      workflowDocument,
	} {
		w, answer := call(t, h, "PUT", pushTo, "alice", pushedAsWritten(document, manifest))
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s of 251 characters was pushed and answered %d: %s", what, w.Code, w.Body)
			continue
		}
		if said, _ := answer["error"].(string); !strings.Contains(said, "at most 250") {
			t.Errorf("%s of 251 characters was refused with %q", what, said)
		}
		if held, err := objects.Has(t.Context(), keyOf("finance", []byte(document))); held || err != nil {
			t.Errorf("%s of 251 characters was refused and its tree stored: %v %v", what, held, err)
		}
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		if _, err := ns.Version(ctx, "monthly-invoicing", aCommit); err == nil {
			t.Error("a version was recorded")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A version stored before the bound, naming a port of 251 characters in its file and its manifest,
// starts after the upgrade exactly as it did before it: the API reads it back through the same store
// the controller decides its runs from, and nothing there applies the bound.
func TestAVersionStoredBeforeThePortBoundStillStarts(t *testing.T) {
	h, pool, _ := serving(t)
	document := strings.Replace(workflowDocument, "outputs: [ok, rejected]", "outputs: [ok, "+aPortPastItsBound+"]", 1)
	manifest := strings.Replace(brickManifest, "    rejected: {}\n", "    "+aPortPastItsBound+": {}\n", 1)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		if err := ns.SaveWorkflow(ctx, "monthly-invoicing", "main"); err != nil {
			return err
		}
		_, err := ns.SaveVersion(ctx, db.Version{
			Workflow: "monthly-invoicing", Commit: aCommit, Author: "operator",
			Entry: "agentiik.yaml", Document: []byte(document),
			Manifests: map[string][]byte{image: []byte(manifest)},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	w, answer := call(t, h, "POST", "/api/v1/finance/workflows/monthly-invoicing/runs", "alice",
		api.Start{Commit: aCommit, Inputs: map[string]any{"orders": []any{}}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("a run of a version stored before the port bound answered %d: %s", w.Code, w.Body)
	}
	run, _ := answer["run"].(string)
	if w, _ := call(t, h, "GET", "/api/v1/finance/runs/"+run, "alice", nil); w.Code != http.StatusOK {
		t.Errorf("reading the run back answered %d: %s", w.Code, w.Body)
	}

	// And the same version pushed now, from the same files, is refused: the push is where a
	// version is made, and the version already recorded is left as it is.
	if w, _ := call(t, h, "PUT", pushTo, "alice", pushedAsWritten(document, manifest)); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("the stored version pushed again answered %d: %s", w.Code, w.Body)
	}
}
