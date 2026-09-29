package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
)

// A workflow repository's image pins and brick manifests: what a git push is judged against beyond
// its own tree, which agk push records before it pushes and the tree push records with each version.

const imagesPath = "/api/v1/finance/workflows/monthly-invoicing/images"

// movedImage is the image of workflowDocument at another digest, which its tag is moved to.
const movedImage = "ghcr.io/acme/agk-invoice@sha256:9999999999999999999999999999999999999999999999999999999999999999"

// created is the workflow of workflowDocument, pushed once as a tree so that its repository exists.
func created(t *testing.T, h http.Handler) {
	t.Helper()
	if w, _ := call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/"+aCommit, "alice", aPush(t)); w.Code != http.StatusOK {
		t.Fatalf("the push answered %d: %s", w.Code, w.Body)
	}
}

// imagesOf is what GET answers for the repository.
func imagesOf(t *testing.T, h http.Handler) api.Images {
	t.Helper()
	w, _ := call(t, h, "GET", imagesPath, "alice", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s answered %d: %s", imagesPath, w.Code, w.Body)
	}
	var got api.Images
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET %s answered %s: %v", imagesPath, w.Body, err)
	}
	return got
}

// pinsAudited are the image.pin entries of the log, as action target reference image was.
func pinsAudited(t *testing.T, entries []audit.Entry) []string {
	t.Helper()
	var out []string
	for _, e := range entries {
		if e.Action != audit.ImagePin {
			continue
		}
		d := detailOf(t, e)
		out = append(out, fmt.Sprintf("%s %s %s %s %v %v", e.Actor, e.Namespace, e.Target, d["reference"], d["image"], d["was"]))
	}
	return out
}

func TestARepositorysPinsAndManifestsAreRecordedAndReadBack(t *testing.T) {
	h, pool, _ := serving(t)
	created(t, h)

	w, _ := call(t, h, "POST", imagesPath, "alice", api.RecordImages{
		Pins:      map[string]string{taggedImage: image},
		Manifests: map[string]string{image: brickManifest},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("the recording answered %d: %s", w.Code, w.Body)
	}
	var recorded api.Images
	if err := json.Unmarshal(w.Body.Bytes(), &recorded); err != nil {
		t.Fatal(err)
	}
	if len(recorded.Pins) != 1 || recorded.Pins[0].Reference != taggedImage || recorded.Pins[0].Image != image || recorded.Pins[0].PinnedBy != "alice" {
		t.Errorf("the recording is answered with the pins %+v", recorded.Pins)
	}
	if len(recorded.Manifests) != 1 || recorded.Manifests[0].Image != image || recorded.Manifests[0].RecordedBy != "alice" {
		t.Errorf("the recording is answered with the manifests %+v", recorded.Manifests)
	}

	got := imagesOf(t, h)
	if len(got.Pins) != 1 || got.Pins[0] != recorded.Pins[0] {
		t.Errorf("GET answers the pins %+v, where the recording answered %+v", got.Pins, recorded.Pins)
	}
	if len(got.Manifests) != 1 || got.Manifests[0] != recorded.Manifests[0] {
		t.Errorf("GET answers the manifests %+v, where the recording answered %+v", got.Manifests, recorded.Manifests)
	}
	// Kept as it was sent, the file the image holds, and not as the document a version keeps of
	// it, which lifts a parameter's boolean required out.
	if held := manifestHeld(t, pool, image); held != brickManifest {
		t.Errorf("the manifest is kept as %q, where it was sent as %q", held, brickManifest)
	}

	// Named again at the digest it holds, the pin changed nothing: it keeps who pinned it and
	// when, and nothing is recorded of it. Moved, it is recorded with the digest it named.
	pinnedAt := got.Pins[0].PinnedAt
	if w, _ := call(t, h, "POST", imagesPath, "alice", api.RecordImages{Pins: map[string]string{taggedImage: image}}); w.Code != http.StatusOK {
		t.Fatalf("recording the pin again answered %d: %s", w.Code, w.Body)
	}
	if again := imagesOf(t, h); !again.Pins[0].PinnedAt.Equal(pinnedAt) {
		t.Errorf("a pin named again at its digest moved from %s to %s", pinnedAt, again.Pins[0].PinnedAt)
	}
	if w, _ := call(t, h, "POST", imagesPath, "alice", api.RecordImages{Pins: map[string]string{taggedImage: movedImage}}); w.Code != http.StatusOK {
		t.Fatalf("moving the pin answered %d: %s", w.Code, w.Body)
	}
	if moved := imagesOf(t, h); moved.Pins[0].Image != movedImage {
		t.Errorf("the pin moved to %s is read back as %+v", movedImage, moved.Pins)
	}

	want := []string{
		"alice finance monthly-invoicing " + taggedImage + " " + image + " <nil>",
		"alice finance monthly-invoicing " + taggedImage + " " + movedImage + " " + image,
	}
	if got := pinsAudited(t, audited(t, pool)); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the log records the pins\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// And the manifest recorded, once, where it was new: the same bytes recorded again change nothing.
	var manifests []string
	for _, e := range audited(t, pool) {
		if e.Action == audit.ImageManifest {
			d := detailOf(t, e)
			manifests = append(manifests, fmt.Sprintf("%s %s %v %v", e.Actor, d["image"], d["sha256"], d["was"]))
		}
	}
	sum := sha256.Sum256([]byte(brickManifest))
	if len(manifests) != 1 || manifests[0] != "alice "+image+" "+hex.EncodeToString(sum[:])+" <nil>" {
		t.Errorf("the log records the manifests %v", manifests)
	}
}

// A recording is refused whole, and what it refuses leaves nothing behind it.
func TestARecordingThatCannotBeHeldRecordsNothing(t *testing.T) {
	h, pool, _ := serving(t)
	created(t, h)
	before := imagesOf(t, h)
	other := "ghcr.io/acme/agk-other@sha256:" + strings.Repeat("a", 64)
	many := map[string]string{}
	for i := range 257 {
		many[fmt.Sprintf("ghcr.io/acme/agk-invoice:1.%d", i)] = image
	}

	for _, c := range []struct {
		name   string
		body   any
		status int
	}{
		{"nothing", map[string]any{}, http.StatusBadRequest},
		{"a field it does not read", map[string]any{"pins": map[string]string{taggedImage: image}, "tags": []string{}}, http.StatusBadRequest},
		{"a digest pinned", api.RecordImages{Pins: map[string]string{image: image}}, http.StatusBadRequest},
		{"a tag pinned to a tag", api.RecordImages{Pins: map[string]string{taggedImage: "ghcr.io/acme/agk-invoice:1.5.0"}}, http.StatusBadRequest},
		{"a tag pinned to another repository", api.RecordImages{Pins: map[string]string{taggedImage: other}}, http.StatusBadRequest},
		{"a reference holding a space", api.RecordImages{Pins: map[string]string{"ghcr.io/acme/agk invoice:1.4.0": image}}, http.StatusBadRequest},
		{"a reference that is not ASCII", api.RecordImages{Pins: map[string]string{"ghcr.io/acme/agk-facturé:1.4.0": image}}, http.StatusBadRequest},
		{"one image given two manifests, under two spellings", api.RecordImages{Manifests: map[string]string{image: brickManifest, strings.Replace(image, "@", ":1.4.0@", 1): brickManifest}}, http.StatusBadRequest},
		{"a reference past its bound", api.RecordImages{Pins: map[string]string{"ghcr.io/" + strings.Repeat("a", 380) + ":1": image}}, http.StatusBadRequest},
		{"a manifest of a tag", api.RecordImages{Manifests: map[string]string{taggedImage: brickManifest}}, http.StatusBadRequest},
		{"an empty manifest", api.RecordImages{Manifests: map[string]string{image: ""}}, http.StatusBadRequest},
		{"a manifest past its bound", api.RecordImages{Manifests: map[string]string{image: brickManifest + "#" + strings.Repeat("x", 256<<10)}}, http.StatusRequestEntityTooLarge},
		{"a manifest that is no brick's", api.RecordImages{Manifests: map[string]string{image: "kind: Workflow\n"}}, http.StatusUnprocessableEntity},
		{"a manifest declaring a port past 250 characters", api.RecordImages{Manifests: map[string]string{image: strings.Replace(brickManifest, "ok: {}", strings.Repeat("o", 251)+": {}", 1)}}, http.StatusUnprocessableEntity},
		{"more pins than a recording carries", api.RecordImages{Pins: many}, http.StatusRequestEntityTooLarge},
		// And a pin that is good, beside a manifest that is not, is not recorded either.
		{"a good pin beside a bad manifest", api.RecordImages{Pins: map[string]string{taggedImage: image}, Manifests: map[string]string{image: "kind: Workflow\n"}}, http.StatusUnprocessableEntity},
	} {
		if w, _ := call(t, h, "POST", imagesPath, "alice", c.body); w.Code != c.status {
			t.Errorf("%s: answered %d, want %d: %s", c.name, w.Code, c.status, w.Body)
		}
	}
	if got := imagesOf(t, h); len(got.Pins) != 0 || len(got.Manifests) != len(before.Manifests) {
		t.Errorf("recordings refused left %+v behind them, where the repository held %+v", got, before)
	}
	if got := pinsAudited(t, audited(t, pool)); len(got) != 0 {
		t.Errorf("recordings refused are recorded as %v", got)
	}
}

// A manifest recorded again with other bytes replaces the one held: the runner reads the manifest out
// of the image when a step runs, so one recorded wrong is put right by recording it again rather than
// left to refuse every push after it.
func TestABrickManifestRecordedAgainReplacesTheOneHeld(t *testing.T) {
	h, pool, _ := serving(t)
	created(t, h)
	if w, _ := call(t, h, "POST", imagesPath, "alice", api.RecordImages{Manifests: map[string]string{image: brickManifest}}); w.Code != http.StatusOK {
		t.Fatalf("the first recording answered %d: %s", w.Code, w.Body)
	}
	another := strings.Replace(brickManifest, "version: 1.0.0", "version: 1.0.1", 1)
	if w, _ := call(t, h, "POST", imagesPath, "alice", api.RecordImages{Manifests: map[string]string{image: another}}); w.Code != http.StatusOK {
		t.Fatalf("another manifest for the same image answered %d: %s", w.Code, w.Body)
	}
	if held := manifestHeld(t, pool, image); held != another {
		t.Errorf("the manifest recorded again is kept as %q", held)
	}
}

// manifestHeld is the manifest the repository keeps for an image, as the hook reads it.
func manifestHeld(t *testing.T, pool *db.Pool, image string) string {
	t.Helper()
	var held []byte
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		var err error
		held, err = ns.Manifest(ctx, "monthly-invoicing", image)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return string(held)
}

// A repository nobody created has no pins to read or write: 404, as one the caller cannot see.
func TestTheImagesOfAWorkflowNobodyCreatedAreNotFound(t *testing.T) {
	h, _, _ := serving(t)
	for _, method := range []string{"GET", "POST"} {
		var body any
		if method == "POST" {
			body = api.RecordImages{Pins: map[string]string{taggedImage: image}}
		}
		if w, _ := call(t, h, method, imagesPath, "alice", body); w.Code != http.StatusNotFound {
			t.Errorf("%s of a workflow nobody created answered %d: %s", method, w.Code, w.Body)
		}
	}
}

// The tree push records the pins a new version was judged with, so that a git push of the next commit
// finds them; a commit already stored, pushed again, records nothing, since its pins may be long
// stale. The manifests it carries are the documents a version keeps, not the files the images hold,
// and are not recorded.
func TestATreePushRecordsItsPins(t *testing.T) {
	h, pool, _ := serving(t)
	push := taggedPush(t)
	push.Images = map[string]string{taggedImage: image}
	path := "/api/v1/finance/workflows/monthly-invoicing/versions/"
	if w, _ := call(t, h, "PUT", path+aCommit, "alice", push); w.Code != http.StatusOK {
		t.Fatalf("the push answered %d: %s", w.Code, w.Body)
	}
	got := imagesOf(t, h)
	if len(got.Pins) != 1 || got.Pins[0].Reference != taggedImage || got.Pins[0].Image != image || got.Pins[0].PinnedBy != "alice" {
		t.Errorf("the push recorded the pins %+v", got.Pins)
	}
	if len(got.Manifests) != 0 {
		t.Errorf("the push recorded the manifests %+v", got.Manifests)
	}

	push.Images = map[string]string{taggedImage: movedImage}
	if w, _ := call(t, h, "PUT", path+aCommit, "alice", push); w.Code != http.StatusOK {
		t.Fatalf("the same commit pushed again answered %d: %s", w.Code, w.Body)
	}
	if again := imagesOf(t, h); again.Pins[0].Image != image {
		t.Errorf("the same commit pushed again, its tag moved, moved the pin to %s", again.Pins[0].Image)
	}
	if w, _ := call(t, h, "PUT", path+anotherCommit, "alice", push); w.Code != http.StatusOK {
		t.Fatalf("the next commit answered %d: %s", w.Code, w.Body)
	}
	if moved := imagesOf(t, h); moved.Pins[0].Image != movedImage {
		t.Errorf("the next commit, its tag moved, left the pin at %s", moved.Pins[0].Image)
	}

	want := []string{
		"alice finance monthly-invoicing " + taggedImage + " " + image + " <nil>",
		"alice finance monthly-invoicing " + taggedImage + " " + movedImage + " " + image,
	}
	if got := pinsAudited(t, audited(t, pool)); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the log records the pins\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// An image by digest is kept less a tag written beside its digest, since the digest is what is
// pulled: a pin to one, and a manifest recorded under one, are found under the digest alone, which
// is how the hook asks for a step writing either spelling.
func TestAnImageIsKeptLessATagWrittenBesideItsDigest(t *testing.T) {
	h, pool, _ := serving(t)
	created(t, h)
	beside := strings.Replace(image, "@", ":1.4.0@", 1)
	w, _ := call(t, h, "POST", imagesPath, "alice", api.RecordImages{
		Pins:      map[string]string{taggedImage: beside},
		Manifests: map[string]string{beside: brickManifest},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("the recording answered %d: %s", w.Code, w.Body)
	}
	got := imagesOf(t, h)
	if len(got.Pins) != 1 || got.Pins[0].Image != image {
		t.Errorf("a pin to %s is kept as %+v", beside, got.Pins)
	}
	sum := sha256.Sum256([]byte(brickManifest))
	if len(got.Manifests) != 1 || got.Manifests[0].Image != image || got.Manifests[0].SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("a manifest recorded under %s is kept as %+v", beside, got.Manifests)
	}
	if held := manifestHeld(t, pool, beside); held != brickManifest {
		t.Errorf("the manifest is not found under %s, as a step writing it asks", beside)
	}
}
