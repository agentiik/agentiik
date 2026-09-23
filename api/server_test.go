package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/version"
)

// The routes, against a real PostgreSQL. What is tested is the whole of what the API does at this
// milestone: it writes a row and tells the controller, and it decides nothing.

const image = "ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc"

const workflowDocument = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: archive, port: ok } }
steps:
  normalize:
    image: ` + image + `
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
  archive:
    image: ` + image + `
    needs: [{ step: normalize, port: ok, as: orders }]
    outputs: [ok]
`

const brickManifest = `
apiVersion: agentiik.dev/v1
kind: Brick
metadata: { name: invoice, version: 1.0.0 }
spec:
  inputs:
    orders: {}
  outputs:
    ok: {}
    rejected: {}
  runtime: { user: "65532:65532" }
`

// everything allows one principal everything, which is what a v0.3.0 authorizer will do for an
// owner and what lets these tests be about the routes rather than about the hook.
type everything struct{ who api.Principal }

func (e everything) Allow(_ context.Context, who api.Principal, _ api.Permission, _ api.Target) (bool, error) {
	return who == e.who, nil
}

func serving(t *testing.T) (http.Handler, *db.Pool, string) {
	h, pool, super, _ := servingWithObjects(t)
	return h, pool, super
}

func servingWithObjects(t *testing.T) (http.Handler, *db.Pool, string, artifact.Objects) {
	t.Helper()
	objects := artifact.Dir(t.TempDir())
	h, pool, super := servingOn(t, objects)
	return h, pool, super, objects
}

// servingOn is the same server over objects of the caller's, which may be nil: an installation
// with no object store is one of the things a test needs to stand up.
func servingOn(t *testing.T, objects artifact.Objects) (http.Handler, *db.Pool, string) {
	t.Helper()
	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `insert into namespaces (name) values ('finance'), ('team-ops')`); err != nil {
		t.Fatal(err)
	}

	store, err := version.New(pool, version.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(everything{who: "alice"}, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewServer(rt, api.ServerOptions{Pool: pool, Versions: store, Objects: objects}); err != nil {
		t.Fatal(err)
	}
	return rt, pool, super
}

func call(t *testing.T, h http.Handler, method, path, as string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(encoded))
		r.Header.Set("Content-Type", "application/json")
	}
	if as != "" {
		r.Header.Set("Authorization", "Bearer "+as)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	var answer map[string]any
	if w.Body.Len() > 0 {
		json.Unmarshal(w.Body.Bytes(), &answer)
	}
	return w, answer
}

func aPush(t *testing.T) api.Push {
	t.Helper()
	m, err := brick.ParseManifest([]byte(brickManifest))
	if err != nil {
		t.Fatal(err)
	}
	tree := fstest.MapFS{"agentiik.yaml": &fstest.MapFile{Data: []byte(workflowDocument)}}
	v, err := version.Capture(tree, "agentiik.yaml", map[string]brick.Manifest{image: m})
	if err != nil {
		t.Fatal(err)
	}
	return api.Push{
		Entry: v.Entry, Document: v.Document,
		Includes: v.Includes, Manifests: v.Manifests, Branch: "main",
		Tree: map[string]api.PushFile{"agentiik.yaml": {Content: []byte(workflowDocument), Mode: "0644"}},
	}
}

// The whole path a person takes: push a version, start a run of it, read it back.
func TestAVersionIsPushedAndARunIsStarted(t *testing.T) {
	h, pool, _ := serving(t)

	w, _ := call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/a3f9c1e", "alice", aPush(t))
	if w.Code != http.StatusOK {
		t.Fatalf("the push answered %d: %s", w.Code, w.Body)
	}

	w, answer := call(t, h, "POST", "/api/v1/finance/workflows/monthly-invoicing/runs", "alice",
		api.Start{Commit: "a3f9c1e", Inputs: map[string]any{"orders": []any{}}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("starting a run answered %d: %s", w.Code, w.Body)
	}
	run, _ := answer["run"].(string)
	if run == "" {
		t.Fatalf("the answer reads %v", answer)
	}
	if answer["state"] != "queued" {
		t.Errorf("a new run is %v, and a run waits on a concurrency lock or on quota before it runs", answer["state"])
	}
	if got := w.Header().Get("Location"); got != "/api/v1/finance/runs/"+run {
		t.Errorf("the run is at %q", got)
	}

	// The run is there, queued, with one step row per step of the graph, which is what the
	// controller needs to find on its first pass.
	w, detail := call(t, h, "GET", "/api/v1/finance/runs/"+run, "alice", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("reading the run answered %d: %s", w.Code, w.Body)
	}
	if detail["state"] != "queued" || detail["workflow"] != "monthly-invoicing" {
		t.Errorf("the run reads %v", detail)
	}
	if detail["triggered_by"] != "alice" {
		t.Errorf("the run says it was started by %v", detail["triggered_by"])
	}
	if steps, _ := detail["steps"].([]any); len(steps) != 2 {
		t.Errorf("the run holds %d steps", len(steps))
	}

	// And the listing holds it.
	w, listing := call(t, h, "GET", "/api/v1/finance/runs", "alice", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("the listing answered %d", w.Code)
	}
	runs, _ := listing["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("the listing holds %d runs", len(runs))
	}

	// The API decided nothing: the run is queued and no task exists. What happens next is the
	// controller's, and it has been told.
	var tasks int
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		d, err := ns.RunDetail(ctx, agk.RunID(run))
		if err != nil {
			return err
		}
		tasks = len(d.Tasks)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if tasks != 0 {
		t.Errorf("the API created %d tasks, and it decides nothing", tasks)
	}
}

// A version that cannot be rebuilt is refused at the push rather than found by the first run of
// it, which is the difference between failing in front of somebody and failing at three in the
// morning.
func TestAVersionThatCannotBeRebuiltIsRefused(t *testing.T) {
	h, _, _ := serving(t)

	broken := aPush(t)
	broken.Manifests = nil
	w, _ := call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/a3f9c1e", "alice", broken)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("a version whose manifests are missing answered %d: %s", w.Code, w.Body)
	}

	// A tree holding an empty entry point, so that what is refused is the version and not the
	// tree around it.
	empty := api.Push{Entry: "agentiik.yaml", Tree: map[string]api.PushFile{"agentiik.yaml": {Mode: "0644"}}}
	w, _ = call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/a3f9c1e", "alice", empty)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("a version with no document answered %d", w.Code)
	}
}

// A run of a commit nobody pushed answers the same thing as one of a workflow the caller cannot
// see, which is the rule the whole surface is held to.
func TestARunOfACommitNobodyPushedIsNotFound(t *testing.T) {
	h, _, _ := serving(t)
	w, _ := call(t, h, "POST", "/api/v1/finance/workflows/monthly-invoicing/runs", "alice",
		api.Start{Commit: "deadbee"})
	if w.Code != http.StatusNotFound {
		t.Errorf("a run of a commit nobody pushed answered %d", w.Code)
	}

	// And one with no commit at all is a bad request rather than a 404: the caller has not
	// asked about something that might exist.
	w, _ = call(t, h, "POST", "/api/v1/finance/workflows/monthly-invoicing/runs", "alice", api.Start{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("a run pinned to nothing answered %d", w.Code)
	}
}

// A body carrying a field nobody knows is refused rather than half understood.
func TestABodyWithAFieldNobodyKnowsIsRefused(t *testing.T) {
	h, _, _ := serving(t)
	r := httptest.NewRequest("POST", "/api/v1/finance/workflows/monthly-invoicing/runs",
		bytes.NewReader([]byte(`{"commit":"a3f9c1e","priority":"urgent"}`)))
	r.Header.Set("Authorization", "Bearer alice")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("a body carrying a field nobody knows answered %d: %s", w.Code, w.Body)
	}
}

// One namespace cannot read another's runs, and the refusal looks like an absence.
func TestOneNamespaceCannotReadAnother(t *testing.T) {
	h, _, _ := serving(t)
	call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/a3f9c1e", "alice", aPush(t))
	w, answer := call(t, h, "POST", "/api/v1/finance/workflows/monthly-invoicing/runs", "alice",
		api.Start{Commit: "a3f9c1e"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("starting answered %d", w.Code)
	}
	run, _ := answer["run"].(string)

	// The same run, asked for under another namespace, by somebody who holds everything.
	w, _ = call(t, h, "GET", "/api/v1/team-ops/runs/"+run, "alice", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("a run asked for under another namespace answered %d", w.Code)
	}
}

// The clock a version records is the server's, and a push carries no timestamp of its own: a
// caller that could name its own creation time could make a version look older than the one it
// replaced.
func TestAPushDoesNotChooseItsOwnMoment(t *testing.T) {
	h, pool, _ := serving(t)
	before := time.Now().UTC().Add(-time.Minute)
	call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/a3f9c1e", "alice", aPush(t))

	var v db.Version
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		var err error
		v, err = ns.Version(ctx, "monthly-invoicing", "a3f9c1e")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !v.CreatedAt.After(before) {
		t.Errorf("the version says it was created at %s", v.CreatedAt)
	}
	if v.Author != "alice" {
		t.Errorf("the version says its author is %q", v.Author)
	}
}
