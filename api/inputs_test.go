package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/version"
)

// A run's inputs, bound by the API against the declaration of the version it runs: every client
// starts a run through this route, so the defaults and the refusals are the API's to apply.

// declaringWorkflow declares one input of each kind a binding has to tell apart: a required one
// whose schema is a file of the tree, defaults of three shapes, one with no schema at all, and one
// bounded where a 64-bit float stops holding every whole number.
const declaringWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders:
    schema: { $ref: "./schemas/order.json" }
    required: true
  cycle:
    schema: { type: string, pattern: "^[0-9]{4}-[0-9]{2}$" }
    default: "2026-01"
  batch:
    schema: { type: integer }
    default: 3
  customers:
    default: []
  note: {}
  edge: { schema: { maximum: 9007199254740992 } }
outputs:
  invoices: { from: { step: normalize, port: ok } }
steps:
  normalize:
    image: ` + image + `
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
`

const orderSchema = `{"type": "array", "items": {"type": "object", "required": ["id"]}}`

// declaringPush is a push of a workflow document and the files of its tree beside it.
func declaringPush(t *testing.T, document string, files map[string]string) api.Push {
	t.Helper()
	m, err := brick.ParseManifest([]byte(brickManifest))
	if err != nil {
		t.Fatal(err)
	}
	tree := fstest.MapFS{"agentiik.yaml": &fstest.MapFile{Data: []byte(document)}}
	pushed := map[string]api.PushFile{"agentiik.yaml": {Content: []byte(document), Mode: "0644"}}
	for path, content := range files {
		tree[path] = &fstest.MapFile{Data: []byte(content)}
		pushed[path] = api.PushFile{Content: []byte(content), Mode: "0644"}
	}
	v, err := version.Capture(tree, "agentiik.yaml", map[string]brick.Manifest{image: m})
	if err != nil {
		t.Fatal(err)
	}
	return api.Push{
		Entry: v.Entry, Document: v.Document,
		Includes: v.Includes, Manifests: v.Manifests, Branch: "main", Tree: pushed,
	}
}

const startAt = "/api/v1/finance/workflows/monthly-invoicing/runs"

// runsHeld counts the runs the installation holds, which is how a refusal is seen to have created
// none.
func runsHeld(t *testing.T, super string) int {
	t.Helper()
	var n int
	if err := dbtest.Superuser(t, super).QueryRow(t.Context(), `select count(*) from runs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The run records what was bound, defaults included, and a request the declaration refuses is
// answered 422 naming the input and the rule, before any run exists.
func TestARunsInputsAreBoundAgainstTheDeclarationOfItsVersion(t *testing.T) {
	h, _, super := serving(t)
	if w, _ := call(t, h, "PUT", pushTo, "alice", declaringPush(t, declaringWorkflow, map[string]string{"schemas/order.json": orderSchema})); w.Code != http.StatusOK {
		t.Fatalf("the push answered %d: %s", w.Code, w.Body)
	}

	w := sent(t, h, "POST", startAt, "alice", `{"commit":"`+aCommit+`","inputs":{"orders":[{"id":"A-1","amount":12.50}]}}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("starting a run answered %d: %s", w.Code, w.Body)
	}
	var started map[string]any
	json.Unmarshal(w.Body.Bytes(), &started)
	_, detail := call(t, h, "GET", "/api/v1/finance/runs/"+started["run"].(string), "alice", nil)
	got, _ := json.Marshal(detail["inputs"])
	if want := `{"batch":3,"customers":[],"cycle":"2026-01","orders":[{"amount":12.5,"id":"A-1"}]}`; string(got) != want {
		t.Errorf("the run holds the inputs %s, where binding gives %s", got, want)
	}

	// A number is held to its schema as agk run --local holds it, as a 64-bit float, so that one no
	// float holds exactly is accepted or refused alike: read exactly, 2^53 + 1 is past a maximum of
	// 2^53, and read as a float it is 2^53.
	if w := sent(t, h, "POST", startAt, "alice", `{"commit":"`+aCommit+`","inputs":{"orders":[],"edge":9007199254740993}}`); w.Code != http.StatusAccepted {
		t.Errorf("a number a local run accepts answered %d: %s", w.Code, w.Body)
	}

	for name, c := range map[string]struct {
		inputs      string
		input, rule string
	}{
		"a required input left out":          {`{}`, "orders", "required"},
		"no inputs at all":                   {`null`, "orders", "required"},
		"a value its referenced schema does": {`{"orders":[{"amount":1}]}`, "orders", "schema"},
		"a value its own schema refuses":     {`{"orders":[],"cycle":"January"}`, "cycle", "schema"},
		"a whole number that is not one":     {`{"orders":[],"batch":2.5}`, "batch", "schema"},
		"a name nothing declares":            {`{"orders":[],"cycel":"2026-02"}`, "cycel", "undeclared"},
	} {
		w := sent(t, h, "POST", startAt, "alice", `{"commit":"`+aCommit+`","inputs":`+c.inputs+`}`)
		var refused map[string]string
		json.Unmarshal(w.Body.Bytes(), &refused)
		if w.Code != http.StatusUnprocessableEntity || refused["input"] != c.input || refused["rule"] != c.rule {
			t.Errorf("%s answered %d: %s", name, w.Code, w.Body)
		}
		if want := "input " + c.input + ": " + c.rule; !strings.HasPrefix(refused["error"], want) {
			t.Errorf("%s is refused saying %q, and a local run says %q first", name, refused["error"], want)
		}
	}
	if n := runsHeld(t, super); n != 2 {
		t.Errorf("%d runs exist, and two starts were accepted", n)
	}
}

// A declaration no run could be bound against is refused at the push, as a graph that cannot be
// built is, and nothing of it is stored.
func TestAVersionWhoseInputsCannotBeBoundIsRefusedAtThePush(t *testing.T) {
	h, _, super, objects := servingWithObjects(t)
	for name, c := range map[string]struct {
		document string
		files    map[string]string
		says     string
	}{
		"a reference to a file the commit does not carry": {declaringWorkflow, nil, "schemas/order.json"},
		"a schema that does not compile": {
			strings.Replace(declaringWorkflow, `{ type: integer }`, `{ type: 12 }`, 1),
			map[string]string{"schemas/order.json": orderSchema}, "batch",
		},
	} {
		w, _ := call(t, h, "PUT", pushTo, "alice", declaringPush(t, c.document, c.files))
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), c.says) {
			t.Errorf("a push with %s answered %d: %s", name, w.Code, w.Body)
		}
	}
	var versions int
	if err := dbtest.Superuser(t, super).QueryRow(t.Context(), `select count(*) from workflow_versions`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 0 {
		t.Errorf("%d versions were recorded", versions)
	}
	if held, err := objects.Has(t.Context(), keyOf("finance", []byte(declaringWorkflow))); err != nil || held {
		t.Errorf("a refused push stored its tree: %v, %v", held, err)
	}
}

// unanswering is an object store that stops answering Open once told to.
type unanswering struct {
	artifact.Objects
	open bool
}

func (f *unanswering) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if f.open {
		return nil, errors.New("the store is not answering")
	}
	return f.Objects.Open(ctx, key)
}

// A reference is read out of the object store, held to its digest, and a store that cannot answer
// is the installation's trouble, never a refusal of the request or of the workflow.
func TestAReferenceIntoTheTreeIsReadFromTheStoreAndHeldToItsDigest(t *testing.T) {
	under := artifact.Dir(t.TempDir())
	store := &unanswering{Objects: under}
	h, pool, super := servingOn(t, store)
	if w, _ := call(t, h, "PUT", pushTo, "alice", declaringPush(t, declaringWorkflow, map[string]string{"schemas/order.json": orderSchema})); w.Code != http.StatusOK {
		t.Fatalf("the push answered %d: %s", w.Code, w.Body)
	}
	start := `{"commit":"` + aCommit + `","inputs":{"orders":[{"id":"A-1"}]}}`

	store.open = true
	if w := sent(t, h, "POST", startAt, "alice", start); w.Code != http.StatusInternalServerError {
		t.Errorf("a store that cannot answer answered %d: %s", w.Code, w.Body)
	}
	store.open = false

	// Other bytes under the schema's key, where the version names its digest.
	if err := under.Put(t.Context(), keyOf("finance", []byte(orderSchema)), strings.NewReader(`true`)); err != nil {
		t.Fatal(err)
	}
	if w := sent(t, h, "POST", startAt, "alice", start); w.Code != http.StatusInternalServerError {
		t.Errorf("a schema whose object is other bytes answered %d: %s", w.Code, w.Body)
	}

	// The same version on an API with no object store: nowhere to read the schema from.
	versions, err := version.New(pool, version.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(everything{who: "alice"}, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewServer(rt, api.ServerOptions{Pool: pool, Versions: versions}); err != nil {
		t.Fatal(err)
	}
	if w := sent(t, rt, "POST", startAt, "alice", start); w.Code != http.StatusServiceUnavailable {
		t.Errorf("an API with no object store answered %d: %s", w.Code, w.Body)
	}
	if n := runsHeld(t, super); n != 0 {
		t.Errorf("%d runs exist, and every start was refused", n)
	}

	// And a declaration that names no file needs no store to be bound against.
	if w, _ := call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/"+anotherCommit, "alice", aPush(t)); w.Code != http.StatusOK {
		t.Fatalf("the push answered %d: %s", w.Code, w.Body)
	}
	if w := sent(t, rt, "POST", startAt, "alice", `{"commit":"`+anotherCommit+`","inputs":{"orders":[]}}`); w.Code != http.StatusAccepted {
		t.Errorf("a declaration naming no file answered %d on an API with no object store: %s", w.Code, w.Body)
	}
}
