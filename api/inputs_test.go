package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/numbertest"
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

// declaringPush is a push of a workflow document and the files of its tree beside it. The
// document includes nothing, so the entry point and the manifest of its one image are the whole
// of what the version is rebuilt from, and the push carries them as agk push would, whatever the
// installation is about to say of them.
func declaringPush(t *testing.T, document string, files map[string]string) api.Push {
	t.Helper()
	pushed := map[string]api.PushFile{"agentiik.yaml": {Content: []byte(document), Mode: "0644"}}
	for path, content := range files {
		pushed[path] = api.PushFile{Content: []byte(content), Mode: "0644"}
	}
	return api.Push{
		Entry: "agentiik.yaml", Document: []byte(document),
		Manifests: map[string][]byte{image: []byte(brickManifest)}, Branch: "main", Tree: pushed,
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

	// A number is held as it was written, which is what makes it an int or a double in an
	// expression, save for an exponent, written out so that PostgreSQL writes back a double: 12.50
	// keeps its point, 2^53 - 1 every digit, and 1e1 is 10.0 rather than the 10 jsonb would make it.
	w := sent(t, h, "POST", startAt, "alice", `{"commit":"`+aCommit+`","inputs":{"orders":[{"id":"A-1","amount":12.50}],"note":1e1,"edge":9007199254740991}}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("starting a run answered %d: %s", w.Code, w.Body)
	}
	var started map[string]any
	json.Unmarshal(w.Body.Bytes(), &started)
	read := sent(t, h, "GET", "/api/v1/finance/runs/"+started["run"].(string), "alice", "")
	var detail struct{ Inputs json.RawMessage }
	json.Unmarshal(read.Body.Bytes(), &detail)
	if want := `{"batch":3,"customers":[],"cycle":"2026-01","edge":9007199254740991,"note":10.0,"orders":[{"amount":12.50,"id":"A-1"}]}`; string(detail.Inputs) != want {
		t.Errorf("the run holds the inputs %s, where binding gives %s", detail.Inputs, want)
	}

	// And held to its schema as it was written, as agk run --local holds it: 2^53 + 1 is past a
	// maximum of 2^53, where a 64-bit float would have read it as 2^53 and let it through.
	if w := sent(t, h, "POST", startAt, "alice", `{"commit":"`+aCommit+`","inputs":{"orders":[],"edge":9007199254740993}}`); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"input":"edge"`) {
		t.Errorf("a number past its maximum answered %d: %s", w.Code, w.Body)
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
	if n := runsHeld(t, super); n != 1 {
		t.Errorf("%d runs exist, and one start was accepted", n)
	}
}

// The workflow numbertest runs locally and through the controller is started here as any client
// starts it, and the run holds the inputs the controller test is given: the same numbers, the same
// kinds, whoever started the run.
func TestARunHoldsItsNumbersAsTheControllerIsGivenThem(t *testing.T) {
	h, _, super := serving(t)
	if w, _ := call(t, h, "PUT", pushTo, "alice", declaringPush(t, numbertest.Workflow, nil)); w.Code != http.StatusOK {
		t.Fatalf("the push answered %d: %s", w.Code, w.Body)
	}
	w := sent(t, h, "POST", startAt, "alice", `{"commit":"`+aCommit+`","inputs":`+numbertest.Body+`}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("starting a run answered %d: %s", w.Code, w.Body)
	}
	var held string
	if err := dbtest.Superuser(t, super).QueryRow(t.Context(), `select inputs::text from runs`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	// Compared as written, number by number, since jsonb orders the keys its own way.
	asWritten := func(doc string) any {
		d := json.NewDecoder(strings.NewReader(doc))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if !reflect.DeepEqual(asWritten(held), asWritten(numbertest.Stored)) {
		t.Errorf("the run holds the inputs %s, and the controller is tested on %s", held, numbertest.Stored)
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

// unanswering is an object store that stops answering Open once told to, and counts the objects
// it was asked to open.
type unanswering struct {
	artifact.Objects
	open bool

	mu     sync.Mutex
	opened int
}

func (f *unanswering) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	f.opened++
	f.mu.Unlock()
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

// A version's declaration is compiled once, and every start of it binds against what that one
// compiled, those arriving while it compiles included: it can take a third of a second, and a
// version never changes.
func TestADeclarationIsCompiledOnceForEveryStartOfItsVersion(t *testing.T) {
	store := &unanswering{Objects: artifact.Dir(t.TempDir())}
	h, _, super := servingOn(t, store)
	if w, _ := call(t, h, "PUT", pushTo, "alice", declaringPush(t, declaringWorkflow, map[string]string{"schemas/order.json": orderSchema})); w.Code != http.StatusOK {
		t.Fatalf("the push answered %d: %s", w.Code, w.Body)
	}
	start := `{"commit":"` + aCommit + `","inputs":{"orders":[{"id":"A-1"}]}}`
	var wg sync.WaitGroup
	for range 9 {
		wg.Go(func() {
			if w := sent(t, h, "POST", startAt, "alice", start); w.Code != http.StatusAccepted {
				t.Errorf("starting a run answered %d: %s", w.Code, w.Body)
			}
			if w := sent(t, h, "POST", startAt, "alice", `{"commit":"`+aCommit+`","inputs":{"orders":[{}]}}`); w.Code != http.StatusUnprocessableEntity {
				t.Errorf("an order with no id answered %d: %s", w.Code, w.Body)
			}
		})
	}
	wg.Wait()
	if store.opened != 1 {
		t.Errorf("the schema's object was opened %d times for 18 starts of one version", store.opened)
	}
	if n := runsHeld(t, super); n != 9 {
		t.Errorf("%d runs exist, and nine starts were accepted", n)
	}
}

// The defaults a workflow declares are held to the bounds the inputs a request sends are held to,
// since the controller reads them at every decision exactly as it reads those.
func TestTheInputsWithTheirDefaultsAreHeldToTheBoundsOfARunsInputs(t *testing.T) {
	h, _, super := serving(t)
	many := strings.TrimSuffix(strings.Repeat("0, ", agk.DefaultMaxItems/2), ", ")
	long := strings.Repeat("x", 2<<20)
	document := strings.Replace(declaringWorkflow, "  note: {}\n", "  note: {}\n  many: { default: ["+many+"] }\n  long: { default: \""+long+"\" }\n", 1)
	if w, _ := call(t, h, "PUT", pushTo, "alice", declaringPush(t, document, map[string]string{"schemas/order.json": orderSchema})); w.Code != http.StatusOK {
		t.Fatalf("the push answered %d: %s", w.Code, w.Body)
	}

	// Each alone is within the bounds, and so are the inputs sent.
	if w := sent(t, h, "POST", startAt, "alice", `{"commit":"`+aCommit+`","inputs":{"orders":[]}}`); w.Code != http.StatusAccepted {
		t.Fatalf("starting a run answered %d: %s", w.Code, w.Body)
	}
	values := strings.TrimSuffix(strings.Repeat(`{"id": 0}, `, agk.DefaultMaxItems/4), ", ")
	for name, inputs := range map[string]string{
		"more values than a run's inputs hold": `{"orders":[` + values + `]}`,
		"more bytes than a run's inputs weigh": `{"orders":[],"note":"` + strings.Repeat("x", 3<<20) + `"}`,
	} {
		w := sent(t, h, "POST", startAt, "alice", `{"commit":"`+aCommit+`","inputs":`+inputs+`}`)
		if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "defaults") {
			t.Errorf("inputs whose defaults take them past %s answered %d: %.200s", name, w.Code, w.Body)
		}
	}
	if n := runsHeld(t, super); n != 1 {
		t.Errorf("%d runs exist, and one start was accepted", n)
	}
}

// Whoever may ask for a run reads what it takes, and nothing else of the workflow: oscar, an
// operator, holds workflow:run and run:read without workflow:read, and reads the inputs and the
// file their schemas reach while the workflow itself answers him 404; vera, who reads the workflow
// and may not run it, is answered 404 here, as for a workflow that does not exist.
func TestWhoeverMayRunAWorkflowReadsTheInputsARunTakesAndNothingElse(t *testing.T) {
	finance := api.Target{Namespace: "finance"}
	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	h, _, _ := servingTo(t, granted{
		"alice": {{api.WorkflowWrite, finance}, {api.WorkflowRead, finance}},
		"oscar": {{api.WorkflowRun, invoicing}, {api.RunRead, invoicing}},
		"vera":  {{api.WorkflowRead, invoicing}, {api.RunRead, invoicing}},
	})
	if w, _ := call(t, h, "PUT", pushTo, "alice", declaringPush(t, declaringWorkflow, map[string]string{"schemas/order.json": orderSchema})); w.Code != http.StatusOK {
		t.Fatalf("the push answered %d: %s", w.Code, w.Body)
	}
	const inputsAt = "/api/v1/finance/workflows/monthly-invoicing/inputs"

	w := sent(t, h, "GET", inputsAt, "oscar", "")
	if w.Code != http.StatusOK {
		t.Fatalf("the operator reading the inputs was answered %d: %s", w.Code, w.Body)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("the inputs are answered with Cache-Control %q", got)
	}
	var read struct {
		Commit string
		Inputs map[string]map[string]json.RawMessage
		Files  map[string]json.RawMessage
	}
	if err := json.Unmarshal(w.Body.Bytes(), &read); err != nil {
		t.Fatal(err)
	}
	if read.Commit != aCommit {
		t.Errorf("the inputs are of %s, and the default branch's head is %s", read.Commit, aCommit)
	}
	for name, want := range map[string]map[string]string{
		"orders":    {"schema": `{"$ref":"./schemas/order.json"}`, "required": `true`},
		"cycle":     {"schema": `{"pattern":"^[0-9]{4}-[0-9]{2}$","type":"string"}`, "required": `false`, "default": `"2026-01"`},
		"batch":     {"schema": `{"type":"integer"}`, "required": `false`, "default": `3`},
		"customers": {"required": `false`, "default": `[]`},
		"note":      {"required": `false`},
		"edge":      {"schema": `{"maximum":9007199254740992}`, "required": `false`},
	} {
		got := map[string]string{}
		for k, v := range read.Inputs[name] {
			var compact bytes.Buffer
			if err := json.Compact(&compact, v); err != nil {
				t.Fatal(err)
			}
			got[k] = compact.String()
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("the input %s reads %v, and the file declares %v", name, got, want)
		}
	}
	if len(read.Inputs) != 6 {
		t.Errorf("%d inputs are answered, and the file declares 6", len(read.Inputs))
	}
	var file, written any
	json.Unmarshal(read.Files["schemas/order.json"], &file)
	json.Unmarshal([]byte(orderSchema), &written)
	if len(read.Files) != 1 || !reflect.DeepEqual(file, written) {
		t.Errorf("the files the schemas reach are answered as %v", read.Files)
	}
	// Nothing of the steps: their names, their images and what they are handed.
	for _, inside := range []string{"normalize", image, "steps", "outputs"} {
		if strings.Contains(w.Body.String(), inside) {
			t.Errorf("the inputs answer %q, which is the workflow's inside: %s", inside, w.Body)
		}
	}

	if w := sent(t, h, "GET", "/api/v1/finance/workflows/monthly-invoicing", "oscar", ""); w.Code != http.StatusNotFound {
		t.Errorf("the operator reading the workflow itself was answered %d", w.Code)
	}
	if w := sent(t, h, "GET", inputsAt, "vera", ""); w.Code != http.StatusNotFound {
		t.Errorf("a viewer reading the inputs a run takes was answered %d: %s", w.Code, w.Body)
	}
	if w := sent(t, h, "GET", inputsAt+"?ref=nightly", "oscar", ""); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "nightly") {
		t.Errorf("a ref naming nothing was answered %d: %s", w.Code, w.Body)
	}
	// A version named whole is a ref, as a run names one; a push of a tree moves no branch.
	if w := sent(t, h, "GET", inputsAt+"?ref="+aCommit, "oscar", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), aCommit) {
		t.Errorf("the version named whole was answered %d: %s", w.Code, w.Body)
	}
}
