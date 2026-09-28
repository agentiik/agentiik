package api_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// PUT /api/v1/{ns}/images: what agk push and agk validate read of the images a workflow names,
// recorded where a git push is judged against it.

const (
	invoiceTag    = "ghcr.io/acme/agk-invoice:1.4.0"
	invoicePinned = "ghcr.io/acme/agk-invoice@sha256:8214cabcb148ac56e69ee08e1554684f7609d08b58c4527938fdcf400be68595"
	invoiceMoved  = "ghcr.io/acme/agk-invoice@sha256:9999999999999999999999999999999999999999999999999999999999999999"
)

// recording is the image routes over a database holding finance and team-ops, asked through auth.
func recordingImages(t *testing.T, auth api.Authorizer) (http.Handler, *db.Pool) {
	t.Helper()
	pool, super := dbtest.Open(t)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `insert into namespaces (name) values ('finance'), ('team-ops')`); err != nil {
		t.Fatal(err)
	}
	rt := router(t, auth)
	if _, err := api.NewImages(rt, api.ImageOptions{Pool: pool, Now: func() time.Time { return time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC) }}); err != nil {
		t.Fatal(err)
	}
	return rt, pool
}

func pinned(t *testing.T, pool *db.Pool, namespace, reference string) (db.ImagePin, error) {
	t.Helper()
	var p db.ImagePin
	err := pool.In(t.Context(), namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		p, err = ns.Pinned(ctx, reference)
		return err
	})
	return p, err
}

func manifestHeld(t *testing.T, pool *db.Pool, namespace, image string) (db.ImageManifest, error) {
	t.Helper()
	var m db.ImageManifest
	err := pool.In(t.Context(), namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		m, err = ns.Manifest(ctx, image)
		return err
	})
	return m, err
}

// A record keeps its tags at their digests and its manifests by their images, as who sent it and
// when, answered 204; a tag moved later names the new digest. The namespace is the path's.
func TestARecordOfImagesIsKeptInItsNamespace(t *testing.T) {
	h, pool := recordingImages(t, everything{who: "alice"})
	w, _ := call(t, h, "PUT", "/api/v1/finance/images", "alice", api.Images{
		Pins:      map[string]string{invoiceTag: invoicePinned},
		Manifests: map[string][]byte{invoicePinned: []byte(brickManifest)},
	})
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("a record was answered %d: %s", w.Code, w.Body)
	}
	p, err := pinned(t, pool, "finance", invoiceTag)
	if err != nil || p.Pinned != invoicePinned || p.PinnedBy != "alice" || !p.PinnedAt.Equal(time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("the tag reads %+v, %v", p, err)
	}
	if m, err := manifestHeld(t, pool, "finance", invoicePinned); err != nil || string(m.Document) != brickManifest || m.RecordedBy != "alice" {
		t.Errorf("the manifest reads %+v, %v", m, err)
	}
	if _, err := pinned(t, pool, "team-ops", invoiceTag); !errors.Is(err, db.ErrNotRecorded) {
		t.Errorf("team-ops reads finance's pin: %v", err)
	}

	if w, _ := call(t, h, "PUT", "/api/v1/finance/images", "alice", api.Images{Pins: map[string]string{invoiceTag: invoiceMoved}}); w.Code != http.StatusNoContent {
		t.Fatalf("moving the tag was answered %d: %s", w.Code, w.Body)
	}
	if p, _ := pinned(t, pool, "finance", invoiceTag); p.Pinned != invoiceMoved {
		t.Errorf("the tag moved reads %s", p.Pinned)
	}
	// A record of nothing records nothing, and is no mistake.
	if w, _ := call(t, h, "PUT", "/api/v1/finance/images", "alice", api.Images{}); w.Code != http.StatusNoContent {
		t.Errorf("a record of nothing was answered %d: %s", w.Code, w.Body)
	}
}

// What a record changes is what every workflow of the namespace is judged against, so it takes
// workflow:write on the namespace: held on one workflow, or workflow:read on the namespace, it is
// answered as a namespace that is not there, and nothing is kept.
func TestARecordOfImagesTakesWorkflowWriteOnTheNamespace(t *testing.T) {
	body := api.Images{Pins: map[string]string{invoiceTag: invoicePinned}}
	for _, c := range []struct {
		held api.Authorizer
		want int
	}{
		{holder{who: "alice", what: api.WorkflowWrite, over: api.Target{Namespace: "finance"}}, http.StatusNoContent},
		{holder{who: "alice", what: api.WorkflowWrite, over: api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}}, http.StatusNotFound},
		{holder{who: "alice", what: api.WorkflowRead, over: api.Target{Namespace: "finance"}}, http.StatusNotFound},
		{holder{who: "alice", what: api.WorkflowWrite, over: api.Target{Namespace: "team-ops"}}, http.StatusNotFound},
	} {
		h, pool := recordingImages(t, c.held)
		w, _ := call(t, h, "PUT", "/api/v1/finance/images", "alice", body)
		if w.Code != c.want {
			t.Errorf("alice holding %+v was answered %d, want %d: %s", c.held, w.Code, c.want, w.Body)
		}
		_, err := pinned(t, pool, "finance", invoiceTag)
		if kept := err == nil; kept != (c.want == http.StatusNoContent) {
			t.Errorf("alice holding %+v was answered %d, and the pin is kept: %t", c.held, w.Code, kept)
		}
	}
}

// A record holding anything no registry serves, a pin that is not one or a manifest that is not
// one, is refused before anything of it is kept, the good entries beside it included.
func TestARecordOfWhatNoRegistryServesIsRefused(t *testing.T) {
	h, pool := recordingImages(t, everything{who: "alice"})
	good := map[string]string{"alpine:3.21": "alpine@sha256:" + strings.Repeat("4", 64)}
	long := "ghcr.io/" + strings.Repeat("a", 400) + ":" + strings.Repeat("t", 110)
	for _, c := range []struct {
		why    string
		body   api.Images
		status int
		says   string
	}{
		{"a digest pinned as though it were a tag", api.Images{Pins: map[string]string{invoicePinned: invoicePinned}}, http.StatusBadRequest, "a pin is a tag's"},
		{"a tag pinned to a tag", api.Images{Pins: map[string]string{invoiceTag: invoiceTag}}, http.StatusBadRequest, "pinned to a digest"},
		{"a tag pinned to a digest that is not one", api.Images{Pins: map[string]string{invoiceTag: "ghcr.io/acme/agk-invoice@sha256:abc"}}, http.StatusBadRequest, "sixty-four"},
		{"a tag pinned to another repository", api.Images{Pins: map[string]string{invoiceTag: "ghcr.io/acme/agk-other@sha256:" + strings.Repeat("1", 64)}}, http.StatusUnprocessableEntity, "its own repository"},
		{"a tag pinned with a tag beside the digest", api.Images{Pins: map[string]string{invoiceTag: "ghcr.io/acme/agk-invoice:1.4.0@sha256:" + strings.Repeat("1", 64)}}, http.StatusUnprocessableEntity, "its own repository"},
		{"a tag holding a space", api.Images{Pins: map[string]string{"alpine :3.21": "alpine@sha256:" + strings.Repeat("4", 64)}}, http.StatusBadRequest, "space"},
		{"a tag holding a vertical tab", api.Images{Pins: map[string]string{"alpine\v:3.21": "alpine@sha256:" + strings.Repeat("4", 64)}}, http.StatusBadRequest, "control"},
		{"an empty tag", api.Images{Pins: map[string]string{"": "alpine@sha256:" + strings.Repeat("4", 64)}}, http.StatusBadRequest, "empty"},
		{"a tag past 512 bytes", api.Images{Pins: map[string]string{long: "ghcr.io/" + strings.Repeat("a", 400) + "@sha256:" + strings.Repeat("1", 64)}}, http.StatusBadRequest, "512"},
		{"a manifest kept by a tag", api.Images{Manifests: map[string][]byte{invoiceTag: []byte(brickManifest)}}, http.StatusBadRequest, "named by digest"},
		{"a manifest that is none", api.Images{Manifests: map[string][]byte{invoicePinned: []byte("kind: Workflow\n")}}, http.StatusUnprocessableEntity, "the manifest of"},
		{"a manifest declaring a port past 250", api.Images{Manifests: map[string][]byte{invoicePinned: []byte(strings.Replace(brickManifest, "ok: {}", strings.Repeat("o", 251)+": {}", 1))}}, http.StatusUnprocessableEntity, "250"},
		{"two manifests of one image", api.Images{Manifests: map[string][]byte{
			invoicePinned: []byte(brickManifest),
			"ghcr.io/acme/agk-invoice:1.4.0@sha256:" + invoicePinned[len(invoicePinned)-64:]: []byte(brickManifest + "\n# another\n"),
		}}, http.StatusUnprocessableEntity, "two manifests"},
	} {
		body := c.body
		if body.Pins == nil {
			body.Pins = good
		} else {
			for ref, digest := range good {
				body.Pins[ref] = digest
			}
		}
		w, answer := call(t, h, "PUT", "/api/v1/finance/images", "alice", body)
		if w.Code != c.status || !strings.Contains(answer["error"].(string), c.says) {
			t.Errorf("%s was answered %d, want %d saying %q: %s", c.why, w.Code, c.status, c.says, w.Body)
		}
	}
	if _, err := pinned(t, pool, "finance", "alpine:3.21"); !errors.Is(err, db.ErrNotRecorded) {
		t.Errorf("a record refused kept the pin beside what it was refused for: %v", err)
	}
}
