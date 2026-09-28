package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"net/http"
	"slices"
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
}

// treeOf is the tree a push of these files records, as the push route writes it.
func treeOf(files map[string]api.PushFile) []db.TreeFile {
	var tree []db.TreeFile
	for _, path := range slices.Sorted(maps.Keys(files)) {
		sum := sha256.Sum256(files[path].Content)
		tree = append(tree, db.TreeFile{Path: path, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(files[path].Content)), Mode: files[path].Mode})
	}
	return tree
}

// A commit already stored, pushed again with the same files, is answered as that version,
// unchanged, and is not judged again by a rule added after it was stored: an agk of the release
// that stored it, pushing it again after an upgrade, meets no refusal it did not meet then. Here it
// names a port of 251 characters and a secret finance does not declare, as a version pushed before
// v0.4.0 may. The same commit with other files is refused, as it always was, since a commit names
// one tree.
func TestAStoredVersionPushedAgainIsAnsweredAsItWas(t *testing.T) {
	h, pool, _ := serving(t)
	document := strings.Replace(workflowDocument, "outputs: [ok, rejected]", "outputs: [ok, "+aPortPastItsBound+"]\n    secrets: [billing]", 1)
	document = strings.Replace(document, "steps:\n", "secrets: [billing]\nsteps:\n", 1)
	manifest := strings.Replace(brickManifest, "    rejected: {}\n", "    "+aPortPastItsBound+": {}\n", 1)
	push := pushedAsWritten(document, manifest)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		if err := ns.SaveWorkflow(ctx, "monthly-invoicing", "main"); err != nil {
			return err
		}
		_, err := ns.SaveVersion(ctx, db.Version{
			Workflow: "monthly-invoicing", Commit: aCommit, Author: "operator",
			Entry: push.Entry, Document: push.Document, Manifests: push.Manifests, Tree: treeOf(push.Tree),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	w, answer := call(t, h, "PUT", pushTo, "alice", push)
	if w.Code != http.StatusOK {
		t.Fatalf("the stored version pushed again with the same files answered %d: %s", w.Code, w.Body)
	}
	if answer["commit"] != aCommit {
		t.Errorf("the push was answered %v", answer)
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		held, err := ns.Version(ctx, "monthly-invoicing", aCommit)
		if err == nil && held.Author != "operator" {
			t.Errorf("the stored version reads author %q, and pushing it again changes nothing", held.Author)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// The same content under another commit is a version being made, and both rules apply.
	if w, _ := call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/"+anotherCommit, "alice", push); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("the same files as a new version answered %d: %s", w.Code, w.Body)
	}

	// And the same commit with its content changed names another tree.
	changed := pushedAsWritten(document+"# changed\n", manifest)
	w, answer = call(t, h, "PUT", pushTo, "alice", changed)
	if w.Code != http.StatusConflict {
		t.Fatalf("the stored commit pushed with other files answered %d: %s", w.Code, w.Body)
	}
	if said, _ := answer["error"].(string); !strings.Contains(said, "other files") {
		t.Errorf("the refusal reads %q", said)
	}
}

// A version is made under the name its file writes: metadata.name is the workflow's name, which
// the route names too, and metadata.namespace, where the file writes one, the namespace. A push
// under another is refused with 422 naming the rule and the line, before anything is stored. A
// commit already stored under another name, which a push before v0.4.0 could make, is not judged
// again by it.
func TestAVersionIsPushedUnderTheNameItsFileWrites(t *testing.T) {
	h, pool, _, objects := servingWithObjects(t)
	for _, c := range []struct {
		to, document, says string
	}{
		{"/api/v1/finance/workflows/payroll/versions/" + aCommit, workflowDocument, "agentiik.yaml:4:19: metadata-name-not-repository"},
		{pushTo, named(workflowDocument, "team-ops", "monthly-invoicing"), "agentiik.yaml:4:49: metadata-namespace-not-repository"},
	} {
		w, answer := call(t, h, "PUT", c.to, "alice", pushedAsWritten(c.document, brickManifest))
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("a push to %s answered %d: %s", c.to, w.Code, w.Body)
			continue
		}
		if said, _ := answer["error"].(string); !strings.Contains(said, c.says) {
			t.Errorf("a push to %s was refused with %q", c.to, said)
		}
		if held, err := objects.Has(t.Context(), keyOf("finance", []byte(c.document))); held || err != nil {
			t.Errorf("a push to %s was refused and its tree stored: %v %v", c.to, held, err)
		}
	}

	push := pushedAsWritten(workflowDocument, brickManifest)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		if err := ns.SaveWorkflow(ctx, "payroll", "main"); err != nil {
			return err
		}
		_, err := ns.SaveVersion(ctx, db.Version{
			Workflow: "payroll", Commit: aCommit, Author: "operator",
			Entry: push.Entry, Document: push.Document, Manifests: push.Manifests, Tree: treeOf(push.Tree),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if w, _ := call(t, h, "PUT", "/api/v1/finance/workflows/payroll/versions/"+aCommit, "alice", push); w.Code != http.StatusOK {
		t.Errorf("a version stored under another name, pushed again, answered %d: %s", w.Code, w.Body)
	}
}

// What a push records is what its version was judged over, the files resolution read and the
// manifest of every image a brick step runs, and nothing else the push carried, so that a rebuild
// reads what was accepted. A digest for a tag no step names is refused, since its reviewers would
// be reading about an image that never runs.
func TestAPushRecordsWhatItsVersionWasJudgedOver(t *testing.T) {
	h, pool, _ := serving(t)
	push := aPush(t)
	push.Tree["fragments/unused.yaml"] = api.PushFile{Content: []byte(".unused:\n  timeout: 1m\n"), Mode: "0644"}
	push.Includes = map[string][]byte{"fragments/unused.yaml": push.Tree["fragments/unused.yaml"].Content}
	push.Manifests["ghcr.io/acme/agk-unused@sha256:8214cabcb148ac56e69ee08e1554684f7609d08b58c4527938fdcf400be68595"] = []byte(brickManifest)

	tagged := push
	tagged.Images = map[string]string{"ghcr.io/acme/agk-unused:1.0.0": "ghcr.io/acme/agk-unused@sha256:8214cabcb148ac56e69ee08e1554684f7609d08b58c4527938fdcf400be68595"}
	w, answer := call(t, h, "PUT", pushTo, "alice", tagged)
	if said, _ := answer["error"].(string); w.Code != http.StatusUnprocessableEntity || !strings.Contains(said, "ghcr.io/acme/agk-unused:1.0.0") {
		t.Fatalf("a push recording a digest for a tag no step names answered %d: %s", w.Code, w.Body)
	}

	if w, _ := call(t, h, "PUT", pushTo, "alice", push); w.Code != http.StatusOK {
		t.Fatalf("the push answered %d: %s", w.Code, w.Body)
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		v, err := ns.Version(ctx, "monthly-invoicing", aCommit)
		if err != nil {
			return err
		}
		if len(v.Includes) != 0 {
			t.Errorf("the version holds the includes %v, and the workflow includes nothing", slices.Sorted(maps.Keys(v.Includes)))
		}
		if got := slices.Sorted(maps.Keys(v.Manifests)); !slices.Equal(got, []string{image}) {
			t.Errorf("the version holds the manifests of %v, and its bricks run %s", got, image)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
