package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
