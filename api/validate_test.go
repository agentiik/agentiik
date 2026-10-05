package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
)

// validated posts files to POST .../validate as who, and answers the status and the body.
func (g *gitServer) validated(who string, body any) (int, map[string]any) {
	g.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		g.t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/v1/finance/workflows/monthly-invoicing/validate", strings.NewReader(string(b)))
	r.Header.Set("Authorization", "Bearer "+who)
	w := httptest.NewRecorder()
	g.h.ServeHTTP(w, r)
	var answer map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		g.t.Fatalf("the route answered %d with %q, which is no JSON object", w.Code, w.Body)
	}
	return w.Code, answer
}

// A draft is judged by the hook's own check and committed nowhere: a reader may ask, what the hook
// would accept is answered with what the version would hold, and what it would refuse with the
// problem a refused commit is answered with.
func TestADraftIsJudgedAsThePushWouldJudgeItAndCommitsNothing(t *testing.T) {
	g := servingGit(t, everyone())
	status, answer := g.validated("bob", map[string]any{"files": map[string]any{"agentiik.yaml": workflowDocument}})
	if status != http.StatusOK || answer["valid"] != true || answer["steps"] != 2.0 || answer["inputs"] != 4.0 || answer["outputs"] != 1.0 {
		t.Fatalf("a valid draft is answered %d %v", status, answer)
	}
	if refs := g.refs(); refs["refs/heads/main"] != "" {
		t.Errorf("a validation moved main to %s", refs["refs/heads/main"])
	}

	status, answer = g.validated("bob", map[string]any{"files": map[string]any{"agentiik.yaml": strings.ReplaceAll(workflowDocument, image, taggedImage)}})
	if status != http.StatusUnprocessableEntity || answer["rule"] != "image-not-pinned" || !strings.HasSuffix(answer["pointer"].(string), "/image") || answer["topic"] != "steps" {
		t.Errorf("a draft naming an image nobody pinned is answered %d %v", status, answer)
	}
	status, answer = g.validated("bob", map[string]any{"files": map[string]any{"agentiik.yaml": workflowDocument + "stepz: {}\n"}})
	if status != http.StatusUnprocessableEntity || answer["rule"] != "schema" || answer["pointer"] != "/stepz" || answer["line"] == nil {
		t.Errorf("a draft with a key the language does not have is answered %d %v", status, answer)
	}

	// Laid over a ref: the files the draft leaves out are the ref's.
	_, first := g.committed("alice", map[string]any{"message": "first", "files": map[string]any{"agentiik.yaml": workflowDocument}})
	if status, answer := g.validated("bob", map[string]any{"ref": "main", "files": map[string]any{"scripts/a.sh": "true\n"}}); status != http.StatusOK || answer["steps"] != 2.0 {
		t.Errorf("a file laid over main is answered %d %v", status, answer)
	}
	if status, answer := g.validated("bob", map[string]any{"files": map[string]any{"agentiik.yaml": nil}}); status != http.StatusUnprocessableEntity || answer["rule"] != "entry-point-missing" {
		t.Errorf("a draft leaving the entry point out of the default branch's tree is answered %d %v", status, answer)
	}
	if status, answer := g.validated("bob", map[string]any{"ref": "nowhere", "files": map[string]any{"a.txt": "a"}}); status != http.StatusNotFound {
		t.Errorf("a ref naming nothing is answered %d %v", status, answer)
	}
	if g.refs()["refs/heads/main"] != first["commit"] {
		t.Error("a validation moved main")
	}
	if status, answer := g.validated("nobody", map[string]any{"files": map[string]any{"agentiik.yaml": workflowDocument}}); status == http.StatusOK {
		t.Errorf("a stranger's validation is answered %d %v", status, answer)
	}
}

// A draft is held to the namespace's secret declarations where its caller holds workflow:read on
// the namespace, which shows them, and to none where the grant is on the workflow alone, which
// "shows none": a refusal naming the secrets the namespace does not declare would tell such a
// caller which it does.
func TestADraftIsHeldToTheSecretDeclarationsOfAReaderOfTheNamespaceAlone(t *testing.T) {
	finance := api.Target{Namespace: "finance"}
	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	g := servingGit(t, owningGrants{granted: granted{
		"bob":   {{api.WorkflowRead, finance}},
		"carol": {{api.WorkflowRead, invoicing}},
	}})
	if err := g.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		_, _, err := ns.Declare(ctx, db.Declaration{Name: "billing", Provider: "builtin", DeclaredBy: "alice"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	naming := func(secrets string) map[string]any {
		return map[string]any{"files": map[string]any{"agentiik.yaml": strings.Replace(workflowDocument, "steps:\n", "secrets: ["+secrets+"]\nsteps:\n", 1)}}
	}

	status, answer := g.validated("bob", naming("billing, ledger"))
	detail, _ := answer["detail"].(string)
	if status != http.StatusUnprocessableEntity || answer["rule"] != "secret-not-declared-by-namespace" || !strings.Contains(detail, "ledger") || strings.Contains(detail, "billing") {
		t.Errorf("a reader of the namespace validating a draft naming a secret it does not declare is answered %d %v", status, answer)
	}
	if status, answer := g.validated("bob", naming("billing")); status != http.StatusOK {
		t.Errorf("a reader of the namespace validating a draft naming only what it declares is answered %d %v", status, answer)
	}

	declaredStatus, declared := g.validated("carol", naming("billing"))
	undeclaredStatus, undeclared := g.validated("carol", naming("ledger"))
	if declaredStatus != http.StatusOK || undeclaredStatus != http.StatusOK || !reflect.DeepEqual(declared, undeclared) {
		t.Errorf("a reader of the workflow alone is answered %d %v for a secret the namespace declares and %d %v for one it does not", declaredStatus, declared, undeclaredStatus, undeclared)
	}

	// workflow.validate is the route made as the caller, and answers each the same.
	rt := g.h.(*api.Router)
	if _, err := api.NewMCP(rt, api.MCPOptions{PublicURL: "https://agentiik.example.com"}); err != nil {
		t.Fatal(err)
	}
	tool := func(who, secrets string) map[string]any {
		args := naming(secrets)
		args["namespace"], args["workflow"] = "finance", "monthly-invoicing"
		_, res, rpcErr := called(t, rt, who, "tools/call", map[string]any{"name": "workflow.validate", "arguments": args})
		if rpcErr != nil {
			t.Fatalf("workflow.validate as %s is answered %v", who, rpcErr)
		}
		return res
	}
	if res := tool("bob", "ledger"); res["isError"] != true {
		t.Errorf("workflow.validate as a reader of the namespace, of a draft naming a secret it does not declare, is answered %v", res)
	}
	if res := tool("carol", "ledger"); res["isError"] == true {
		t.Errorf("workflow.validate as a reader of the workflow alone, of a draft naming a secret the namespace does not declare, is answered %v", res)
	}
}
