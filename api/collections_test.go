package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/version"
)

// toolDocument is monthly-invoicing published as one tool: its inputs each with a schema, one of
// them a file of the tree, and the output it returns with a schema of its own.
const toolDocument = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders: { schema: { $ref: "./schemas/order.json" }, required: true }
  note: { schema: { type: string }, default: "" }
outputs:
  invoices: { from: { step: archive, port: ok }, schema: { type: object, properties: { total: { type: number } } } }
mcp:
  name: create_invoices
  title: Create invoices
  description: Issue one invoice per order and return them.
  output: invoices
  timeout: 2s
  annotations: { idempotentHint: false }
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

const toolOrderSchema = `{"type": "array", "items": {"type": "object", "required": ["order"], "properties": {"order": {"type": "string"}}}}`

// collected is an installation serving collections: finance, where alice edits and runs and bob
// reads, holding monthly-invoicing published as a tool and payroll publishing none, and team-ops,
// where nobody may run anything.
type collected struct {
	t       *testing.T
	rt      *api.Router
	pool    *db.Pool
	super   string
	objects artifact.Objects
}

func servingCollections(t *testing.T) *collected {
	t.Helper()
	pool, super := dbtest.Open(t)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(),
		`insert into namespaces (name) values ('finance'), ('team-ops');
		 insert into principals (id, kind) values ('alice', 'user'), ('bob', 'user')`); err != nil {
		t.Fatal(err)
	}
	store, err := version.New(pool, version.Options{})
	if err != nil {
		t.Fatal(err)
	}
	finance := api.Target{Namespace: "finance"}
	auth := owningGrants{granted: granted{
		"alice": {{api.WorkflowRead, finance}, {api.WorkflowWrite, finance}, {api.WorkflowRun, finance}, {api.RunRead, finance}},
		"bob":   {{api.WorkflowRead, finance}},
	}}
	rt, err := api.NewRouter(auth, bearer)
	if err != nil {
		t.Fatal(err)
	}
	objects := artifact.Dir(t.TempDir())
	if _, err := api.NewServer(rt, api.ServerOptions{Pool: pool, Versions: store, Objects: objects, PublicURL: "https://agentiik.example.com"}); err != nil {
		t.Fatal(err)
	}
	collections, err := api.NewCollections(rt, api.CollectionOptions{Pool: pool, Versions: store, Objects: objects, PublicURL: "https://agentiik.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewMCP(rt, api.MCPOptions{PublicURL: "https://agentiik.example.com", Collections: collections}); err != nil {
		t.Fatal(err)
	}
	c := &collected{t: t, rt: rt, pool: pool, super: super, objects: objects}
	for _, wf := range []struct{ name, document string }{
		{"monthly-invoicing", toolDocument},
		{"payroll", strings.ReplaceAll(workflowDocument, "monthly-invoicing", "payroll")},
	} {
		if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
			if err := ns.SaveWorkflow(ctx, wf.name, "main"); err != nil {
				return err
			}
			_, err := ns.RecordImages(ctx, wf.name, "alice", time.Time{}, db.Images{Manifests: map[string][]byte{image: []byte(brickManifest)}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		status, answer := c.ask("POST", "/api/v1/finance/workflows/"+wf.name+"/commits", "alice",
			map[string]any{"message": "first", "files": map[string]any{"agentiik.yaml": wf.document, "schemas/order.json": toolOrderSchema}})
		if status != http.StatusCreated {
			t.Fatalf("%s could not be committed: %d %v", wf.name, status, answer)
		}
	}
	return c
}

// ask makes one request as who, and answers its status and its JSON body.
func (c *collected) ask(method, path, who string, body any) (int, map[string]any) {
	c.t.Helper()
	var payload string
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		payload = string(b)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(payload))
	r.Header.Set("Authorization", "Bearer "+who)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	c.rt.ServeHTTP(w, r)
	var answer map[string]any
	json.Unmarshal(w.Body.Bytes(), &answer)
	return w.Code, answer
}

// made makes a collection of who's, and answers its identifier.
func (c *collected) made(who, name string) string {
	c.t.Helper()
	status, answer := c.ask("POST", "/api/v1/me/collections", who, map[string]any{"name": name, "description": "For the finance assistant."})
	if status != http.StatusCreated {
		c.t.Fatalf("the collection could not be made: %d %v", status, answer)
	}
	return answer["id"].(string)
}

// A collection is made with a URL a client is given, holds what its owner adds, says what each
// member offers or why it offers nothing, and keeps its identifier through a rename.
func TestACollectionIsMadeAndSaysWhatEachMemberOffers(t *testing.T) {
	c := servingCollections(t)
	status, made := c.ask("POST", "/api/v1/me/collections", "alice", map[string]any{"name": "back-office", "description": "For the finance assistant."})
	if status != http.StatusCreated {
		t.Fatalf("the collection was answered %d %v", status, made)
	}
	id := made["id"].(string)
	if made["url"] != "https://agentiik.example.com/mcp/collections/"+id || made["description"] != "For the finance assistant." {
		t.Fatalf("the collection was made as %v", made)
	}

	if status, answer := c.ask("PUT", "/api/v1/me/collections/"+id+"/members/finance/monthly-invoicing", "alice", map[string]any{}); status != http.StatusOK {
		t.Fatalf("a member was answered %d %v", status, answer)
	}
	status, answer := c.ask("PUT", "/api/v1/me/collections/"+id+"/members/finance/payroll", "alice", map[string]any{"ref": "main"})
	if status != http.StatusOK {
		t.Fatalf("a member was answered %d %v", status, answer)
	}
	members := answer["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("the collection holds %v", members)
	}
	first, second := members[0].(map[string]any), members[1].(map[string]any)
	if first["workflow"] != "finance/monthly-invoicing" || first["tool"] != "create_invoices" || first["reason"] != nil {
		t.Errorf("the first member reads %v", first)
	}
	if second["workflow"] != "finance/payroll" || second["ref"] != "main" || second["tool"] != nil || second["reason"] != "no_mcp_block" {
		t.Errorf("a member declaring no mcp block reads %v", second)
	}

	if status, answer := c.ask("PATCH", "/api/v1/me/collections/"+id, "alice", map[string]any{"name": "finance-desk"}); status != http.StatusOK || answer["id"] != id || answer["name"] != "finance-desk" {
		t.Errorf("a rename was answered %d %v", status, answer)
	}
	if status, answer := c.ask("GET", "/api/v1/me/collections", "alice", nil); status != http.StatusOK || len(answer["collections"].([]any)) != 1 {
		t.Errorf("the list was answered %d %v", status, answer)
	}
}

// A collection is its owner's alone: anybody else, reading, changing, adding to or calling through
// it, is answered as if it did not exist.
func TestACollectionIsItsOwnersAlone(t *testing.T) {
	c := servingCollections(t)
	id := c.made("alice", "back-office")
	for _, ask := range []struct{ method, path string }{
		{"GET", "/api/v1/me/collections/" + id},
		{"DELETE", "/api/v1/me/collections/" + id},
	} {
		if status, answer := c.ask(ask.method, ask.path, "bob", nil); status != http.StatusNotFound {
			t.Errorf("%s %s by bob was answered %d %v", ask.method, ask.path, status, answer)
		}
	}
	if status, _ := c.ask("PATCH", "/api/v1/me/collections/"+id, "bob", map[string]any{"name": "mine"}); status != http.StatusNotFound {
		t.Errorf("a rename by bob was answered %d", status)
	}
	if status, _ := c.ask("PUT", "/api/v1/me/collections/"+id+"/members/finance/monthly-invoicing", "bob", map[string]any{}); status != http.StatusNotFound {
		t.Errorf("an addition by bob was answered %d", status)
	}
	if status, _, _ := calledAt(t, c.rt, "/mcp/collections/"+id, "bob", "tools/list", nil); status != http.StatusNotFound {
		t.Errorf("bob listing alice's collection was answered %d", status)
	}
	if status, answer := c.ask("GET", "/api/v1/me/collections", "bob", nil); status != http.StatusOK || len(answer["collections"].([]any)) != 0 {
		t.Errorf("bob's list was answered %d %v", status, answer)
	}
}

// A collection holds what its owner may already run: a workflow they cannot run is answered as one
// that does not exist, a ref the repository does not hold is refused, and so is a commit; a tool
// name another member gives is refused naming it, and as resolves it.
func TestAMemberIsJudgedAsItIsAdded(t *testing.T) {
	c := servingCollections(t)
	id := c.made("alice", "back-office")
	member := func(ns, name string, body any) (int, map[string]any) {
		return c.ask("PUT", "/api/v1/me/collections/"+id+"/members/"+ns+"/"+name, "alice", body)
	}
	if status, answer := member("team-ops", "nightly", map[string]any{}); status != http.StatusNotFound {
		t.Errorf("a workflow alice cannot run was answered %d %v", status, answer)
	}
	if status, answer := member("finance", "nothing", map[string]any{}); status != http.StatusNotFound {
		t.Errorf("a workflow that does not exist was answered %d %v", status, answer)
	}
	if status, answer := member("finance", "monthly-invoicing", map[string]any{"ref": "v9"}); status != http.StatusUnprocessableEntity {
		t.Errorf("a ref the repository does not hold was answered %d %v", status, answer)
	}
	if status, answer := member("finance", "monthly-invoicing", map[string]any{"ref": strings.Repeat("a", 40)}); status != http.StatusBadRequest {
		t.Errorf("a commit was answered %d %v", status, answer)
	}
	if status, answer := member("finance", "monthly-invoicing", map[string]any{"as": "not a name"}); status != http.StatusBadRequest {
		t.Errorf("an as outside the grammar was answered %d %v", status, answer)
	}
	if status, answer := member("finance", "monthly-invoicing", map[string]any{"ref": nil}); status != http.StatusBadRequest {
		t.Errorf("a null ref was answered %d %v", status, answer)
	}
	if status, answer := member("finance", "monthly-invoicing", map[string]any{}); status != http.StatusOK {
		t.Fatalf("the member was answered %d %v", status, answer)
	}

	// payroll published under the same name clashes, and as resolves it.
	status, head := c.ask("GET", "/api/v1/finance/workflows/payroll", "alice", nil)
	if status != http.StatusOK {
		t.Fatalf("payroll was read %d %v", status, head)
	}
	parent := head["repository"].(map[string]any)["head"].(string)
	clashing := strings.ReplaceAll(toolDocument, "monthly-invoicing", "payroll")
	if status, answer := c.ask("POST", "/api/v1/finance/workflows/payroll/commits", "alice",
		map[string]any{"parent": parent, "message": "publish", "files": map[string]any{"agentiik.yaml": clashing, "schemas/order.json": toolOrderSchema}}); status != http.StatusCreated {
		t.Fatalf("payroll could not be published: %d %v", status, answer)
	}
	status, answer := member("finance", "payroll", map[string]any{})
	if status != http.StatusConflict || !strings.Contains(answer["error"].(string), "finance/monthly-invoicing") {
		t.Errorf("a tool name another member gives was answered %d %v", status, answer)
	}
	status, answer = member("finance", "payroll", map[string]any{"as": "pay_people"})
	if status != http.StatusOK {
		t.Fatalf("a member given its own name was answered %d %v", status, answer)
	}
	names := []any{}
	for _, m := range answer["members"].([]any) {
		names = append(names, m.(map[string]any)["tool"])
	}
	if !slices.Equal(names, []any{"create_invoices", "pay_people"}) {
		t.Errorf("the collection offers %v", names)
	}
}

// A collection's endpoint offers one tool per member that offers one, as the documentation writes
// it: its inputSchema an object of the workflow's inputs, each with its schema as it stands and the
// file it references bundled in, and its outputSchema the envelope of the output it returns.
func TestACollectionOffersEachMembersTool(t *testing.T) {
	c := servingCollections(t)
	id := c.made("alice", "back-office")
	c.ask("PUT", "/api/v1/me/collections/"+id+"/members/finance/monthly-invoicing", "alice", map[string]any{})
	c.ask("PUT", "/api/v1/me/collections/"+id+"/members/finance/payroll", "alice", map[string]any{})

	status, result, refusal := calledAt(t, c.rt, "/mcp/collections/"+id, "alice", "tools/list", nil)
	if status != http.StatusOK || refusal != nil {
		t.Fatalf("the list was answered %d %v", status, refusal)
	}
	tools := result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("the collection offers %v, and payroll declares no mcp block", tools)
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "create_invoices" || tool["title"] != "Create invoices" || tool["description"] != "Issue one invoice per order and return them." {
		t.Errorf("the tool reads %v", tool)
	}
	input := tool["inputSchema"].(map[string]any)
	if input["type"] != "object" || !slices.Equal(input["required"].([]any), []any{"orders"}) {
		t.Errorf("the inputSchema reads %v", input)
	}
	properties := input["properties"].(map[string]any)
	if properties["orders"].(map[string]any)["$ref"] != "./schemas/order.json" || properties["note"].(map[string]any)["type"] != "string" {
		t.Errorf("the inputs read %v", properties)
	}
	bundled := input["$defs"].(map[string]any)["schemas/order.json"].(map[string]any)
	if input["$id"] != "agk://repo/" || bundled["$id"] != "agk://repo/schemas/order.json" || bundled["type"] != "array" {
		t.Errorf("the file the inputs reference is bundled as %v under %v", bundled, input["$id"])
	}
	output := tool["outputSchema"].(map[string]any)
	data := output["properties"].(map[string]any)["items"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["data"].(map[string]any)
	if data["type"] != "object" || data["properties"] == nil {
		t.Errorf("the outputSchema holds the items' data as %v", data)
	}
	if hints := tool["annotations"].(map[string]any); hints["idempotentHint"] != false || len(hints) != 1 {
		t.Errorf("the annotations read %v", hints)
	}

	// A collection offers tools alone.
	if _, _, refusal := calledAt(t, c.rt, "/mcp/collections/"+id, "alice", "resources/list", nil); refusal == nil {
		t.Error("a collection answered resources/list")
	}
}

// A call is a run of the caller's, of trigger kind mcp, recording the collection and the tool, at
// the commit the member's ref points at; a malformed argument is refused before any run exists; and
// a sync call waits for the run, answering what became of it.
func TestACallIsARunThroughTheCollection(t *testing.T) {
	c := servingCollections(t)
	id := c.made("alice", "back-office")
	c.ask("PUT", "/api/v1/me/collections/"+id+"/members/finance/monthly-invoicing", "alice", map[string]any{})
	path := "/mcp/collections/" + id

	if _, _, refusal := calledAt(t, c.rt, path, "alice", "tools/call", map[string]any{"name": "create_invoices", "arguments": map[string]any{"orders": "not a list"}}); refusal == nil || refusal.Code != -32602 {
		t.Errorf("a malformed argument was answered %v", refusal)
	}
	if runs := c.runs(); len(runs) != 0 {
		t.Fatalf("a refused call created %v", runs)
	}

	// The run ends while the call waits: here, by hand, as the controller would.
	super := dbtest.Superuser(t, c.super)
	ended := make(chan error, 1)
	go func() {
		for range 40 {
			time.Sleep(50 * time.Millisecond)
			tag, err := super.Exec(context.Background(),
				`with ended as (update runs set state = 'failed', started_at = now(), finished_at = now()
				                where trigger = 'mcp' and state = 'queued' returning namespace, id)
				 update steps set state = 'failed' from ended where steps.namespace = ended.namespace and steps.run_id = ended.id and steps.step = 'archive'`)
			if err != nil || tag.RowsAffected() > 0 {
				ended <- err
				return
			}
		}
		ended <- nil
	}()
	status, result, refusal := calledAt(t, c.rt, path, "alice", "tools/call", map[string]any{
		"name": "create_invoices", "arguments": map[string]any{"orders": []any{map[string]any{"order": "ORD-0001"}}},
	})
	if status != http.StatusOK || refusal != nil {
		t.Fatalf("the call was answered %d %v", status, refusal)
	}
	if err := <-ended; err != nil {
		t.Fatal(err)
	}
	if result["isError"] != true || !strings.Contains(textOf(result), "Step archive failed") {
		t.Errorf("a failed run was answered %v", result)
	}
	runs := c.runs()
	if len(runs) != 1 {
		t.Fatalf("the call created %v", runs)
	}
	run := runs[0]
	if run.Trigger.String() != "mcp" || run.TriggeredBy != "alice" || run.Collection == nil || run.Collection.ID != id || run.Collection.Tool != "create_invoices" {
		t.Errorf("the run reads %+v %+v", run, run.Collection)
	}
}

// "Sanitize tool outputs: secret masking, as in logs: a secret never appears in a result. Above a
// size ceiling, the result is a reference to an artifact rather than the artifact." A sync call
// answers the output's envelope as it was stored, which the driver masked before storing it, and
// read under envelope_max_bytes: an item's files are the references the envelope holds, never their
// bytes, whatever their size, so no second ceiling is invented.
func TestASyncCallAnswersTheEnvelopeAsStored(t *testing.T) {
	c := servingCollections(t)
	id := c.made("alice", "back-office")
	c.ask("PUT", "/api/v1/me/collections/"+id+"/members/finance/monthly-invoicing", "alice", map[string]any{})

	super := dbtest.Superuser(t, c.super)
	stored := make(chan agk.Envelope, 1)
	go func() {
		defer close(stored)
		for range 40 {
			time.Sleep(50 * time.Millisecond)
			var run string
			if err := super.QueryRow(context.Background(), `select id from runs where trigger = 'mcp' and state = 'queued'`).Scan(&run); err != nil {
				continue
			}
			uri, _ := agk.ParseURI("agk://run/" + run + "/archive/ok/invoices.pdf")
			envelope := agk.Envelope{
				Meta: agk.Meta{RunID: agk.RunID(run), Step: "archive", Port: "ok", Attempt: 1, Count: 1, ProducedAt: time.Now().UTC().Truncate(time.Millisecond)},
				Items: []agk.Item{{
					ID: "01JMZ8W4K7A1B2C3D4E5F6G7H8", Data: map[string]any{"total": 1290.5, "note": "the key was [masked]"},
					Files: []agk.File{{Name: "invoices.pdf", URI: uri, MediaType: "application/pdf", Size: 50 << 20, SHA256: strings.Repeat("ab", 32)}},
				}},
			}
			digest, size, err := artifact.PutEnvelope(context.Background(), c.objects, "finance", envelope)
			if err != nil {
				return
			}
			ports, _ := json.Marshal(map[string]any{"ok": map[string]any{"digest": "sha256:" + digest, "size": size, "items": 1}})
			outputs, _ := json.Marshal(map[string]any{"invoices": map[string]any{"step": "archive", "port": "ok", "count": 1}})
			super.Exec(context.Background(), `update steps set ports = $2, state = 'succeeded' where run_id = $1 and step = 'archive'`, run, ports)
			super.Exec(context.Background(), `update runs set state = 'succeeded', started_at = now(), finished_at = now(), outputs = $2 where id = $1`, run, outputs)
			stored <- envelope
			return
		}
	}()
	status, result, refusal := calledAt(t, c.rt, "/mcp/collections/"+id, "alice", "tools/call", map[string]any{
		"name": "create_invoices", "arguments": map[string]any{"orders": []any{map[string]any{"order": "ORD-0001"}}},
	})
	envelope, ok := <-stored
	if !ok {
		t.Fatal("no run was started to finish")
	}
	if status != http.StatusOK || refusal != nil || result["isError"] == true {
		t.Fatalf("the call was answered %d %v %v", status, refusal, result)
	}
	var want bytes.Buffer
	if _, err := envelope.Encode(&want); err != nil {
		t.Fatal(err)
	}
	var wanted any
	json.Unmarshal(want.Bytes(), &wanted)
	got, _ := json.Marshal(result["structuredContent"])
	if expected, _ := json.Marshal(wanted); string(got) != string(expected) {
		t.Errorf("the result is %s, and the envelope stored %s", got, expected)
	}
	if text := textOf(result); text != strings.TrimSpace(want.String()) || !strings.Contains(text, "agk://run/") || !strings.Contains(text, "[masked]") {
		t.Errorf("the result reads %q", text)
	}
}

// runs are the runs of monthly-invoicing, as a listing reads them.
func (c *collected) runs() []db.RunDetail {
	c.t.Helper()
	var out []db.RunDetail
	if err := c.pool.In(context.Background(), "finance", func(ctx context.Context, ns *db.NS) error {
		rows, err := dbtest.Superuser(c.t, c.super).Query(ctx, `select id from runs where namespace = 'finance' order by created_at`)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			d, err := ns.RunDetail(ctx, agk.RunID(id))
			if err != nil {
				return err
			}
			out = append(out, d)
		}
		return nil
	}); err != nil {
		c.t.Fatal(err)
	}
	return out
}

// textOf is the text of a tool's result.
func textOf(result map[string]any) string {
	var out []string
	for _, c := range result["content"].([]any) {
		out = append(out, c.(map[string]any)["text"].(string))
	}
	return strings.Join(out, "\n")
}
