package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// committed posts files to POST .../commits as who, and answers the status and the body.
func (g *gitServer) committed(who string, body any) (int, map[string]any) {
	g.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		g.t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/v1/finance/workflows/monthly-invoicing/commits", strings.NewReader(string(b)))
	r.Header.Set("Authorization", "Bearer "+who)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.h.ServeHTTP(w, r)
	var answer map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		g.t.Fatalf("the route answered %d with %q, which is no JSON object", w.Code, w.Body)
	}
	return w.Code, answer
}

// The whole path a file takes without git: a first commit to an empty repository becomes the default
// branch and a version, a second follows it from its parent, a file is removed, and a clone reads
// back what the commits wrote, as git wrote it and by whom.
func TestFilesArePublishedAsCommitsAGitCloneReadsBack(t *testing.T) {
	g := servingGit(t, everyone())

	status, first := g.committed("alice", map[string]any{"message": "The first version", "files": map[string]any{"agentiik.yaml": workflowDocument}})
	if status != http.StatusCreated || first["branch"] != "main" || first["parent"] != "" {
		t.Fatalf("the first commit is answered %d %v", status, first)
	}
	head := first["commit"].(string)
	if got := g.refs()["refs/heads/main"]; got != head {
		t.Fatalf("main names %s after the first commit %s", got, head)
	}
	if _, err := g.version(head); err != nil {
		t.Errorf("the first commit is no version: %v", err)
	}

	status, second := g.committed("alice", map[string]any{"parent": head, "message": "Add the scripts", "files": map[string]any{"scripts/a.sh": "true\n", "scripts/b.sh": "false\n"}})
	if status != http.StatusCreated || second["parent"] != head {
		t.Fatalf("the second commit is answered %d %v", status, second)
	}
	status, third := g.committed("alice", map[string]any{"parent": second["commit"], "message": "Remove b", "files": map[string]any{"scripts/b.sh": nil}})
	if status != http.StatusCreated {
		t.Fatalf("a removal is answered %d %v", status, third)
	}

	clone := g.cloned("alice")
	if out := clone.must("ls-files"); out != "agentiik.yaml\nscripts/a.sh\n" {
		t.Errorf("the clone holds %q", out)
	}
	if out := strings.TrimSpace(clone.must("log", "--format=%an <%ae>|%s", "-3")); out != "alice <alice@agentiik.example.com>|Remove b\nalice <alice@agentiik.example.com>|Add the scripts\nalice <alice@agentiik.example.com>|The first version" {
		t.Errorf("git log reads %q", out)
	}
	if out := clone.must("fsck", "--strict"); strings.Contains(out, "error") {
		t.Errorf("git fsck says %s", out)
	}

	// Each ref moved is recorded as any push's is.
	moved := 0
	for _, e := range audited(t, g.pool) {
		if e.Action == audit.RefUpdate && e.Actor == "alice" && detailOf(t, e)["ref"] == "refs/heads/main" {
			moved++
		}
	}
	if moved != 3 {
		t.Errorf("%d moves of main are recorded, and three commits made them", moved)
	}
}

// A commit is made from the parent its files were read at: one made from a parent the branch has
// moved past is refused rather than undoing what landed since, and so is one naming none.
func TestACommitFromAParentTheBranchMovedPastIsRefused(t *testing.T) {
	g := servingGit(t, everyone())
	_, first := g.committed("alice", map[string]any{"message": "first", "files": map[string]any{"agentiik.yaml": workflowDocument}})
	_, second := g.committed("alice", map[string]any{"parent": first["commit"], "message": "second", "files": map[string]any{"a.txt": "a\n"}})

	if status, answer := g.committed("alice", map[string]any{"parent": first["commit"], "message": "late", "files": map[string]any{"b.txt": "b\n"}}); status != http.StatusConflict || !strings.Contains(answer["error"].(string), "moved") {
		t.Errorf("a commit from a parent main moved past is answered %d %v", status, answer)
	}
	if status, answer := g.committed("alice", map[string]any{"message": "no parent", "files": map[string]any{"b.txt": "b\n"}}); status != http.StatusUnprocessableEntity || !strings.Contains(answer["error"].(string), "parent") {
		t.Errorf("a commit naming no parent on a branch that exists is answered %d %v", status, answer)
	}
	if got := g.refs()["refs/heads/main"]; got != second["commit"] {
		t.Errorf("main names %s after two refusals, and the last commit was %s", got, second["commit"])
	}

	// A new branch starts at the parent named.
	status, branch := g.committed("alice", map[string]any{"branch": "feature", "parent": first["commit"], "message": "on a branch", "files": map[string]any{"b.txt": "b\n"}})
	if status != http.StatusCreated || g.refs()["refs/heads/feature"] != branch["commit"] {
		t.Errorf("a new branch is answered %d %v", status, branch)
	}
}

// What the hook refuses is answered with what it tells: the rule, where, the node as a pointer, what
// was expected there and the topic to read; and it is recorded as a refused push.
func TestARefusedCommitIsAnsweredWithTheHooksProblem(t *testing.T) {
	g := servingGit(t, everyone())
	status, answer := g.committed("alice", map[string]any{"message": "an image nobody pinned", "files": map[string]any{"agentiik.yaml": strings.ReplaceAll(workflowDocument, image, taggedImage)}})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("an invalid workflow is answered %d %v", status, answer)
	}
	if answer["rule"] != "image-not-pinned" || answer["file"] != "agentiik.yaml" || answer["line"] == nil || answer["expected"] == "" || answer["topic"] != "steps" {
		t.Errorf("the refusal is answered %v", answer)
	}
	if p, _ := answer["pointer"].(string); !strings.HasPrefix(p, "/steps/") || !strings.HasSuffix(p, "/image") {
		t.Errorf("the refusal points at %q", answer["pointer"])
	}
	if got := g.refs()["refs/heads/main"]; got != "" {
		t.Errorf("main names %s after a refused commit", got)
	}
	var found bool
	for _, e := range audited(t, g.pool) {
		found = found || (e.Action == audit.PushRefuse && e.Actor == "alice" && detailOf(t, e)["rule"] == "image-not-pinned")
	}
	if !found {
		t.Error("the refused commit is not recorded as a refused push")
	}
}

// The grants a commit takes are a push's: a reader commits nothing, and the protected default branch
// takes grant:manage, which an editor lacks and an owner holds.
func TestACommitTakesThePushersGrants(t *testing.T) {
	g := servingGit(t, everyone())
	if status, answer := g.committed("bob", map[string]any{"message": "a reader", "files": map[string]any{"agentiik.yaml": workflowDocument}}); status == http.StatusCreated {
		t.Errorf("a reader's commit is answered %d %v", status, answer)
	}
	_, first := g.committed("alice", map[string]any{"message": "first", "files": map[string]any{"agentiik.yaml": workflowDocument}})
	if _, err := dbtest.Superuser(t, g.super).Exec(t.Context(),
		`update workflow_refs set protected = true where namespace = 'finance' and workflow = 'monthly-invoicing' and ref = 'refs/heads/main'`); err != nil {
		t.Fatal(err)
	}
	if status, answer := g.committed("alice", map[string]any{"parent": first["commit"], "message": "protected", "files": map[string]any{"a.txt": "a\n"}}); status != http.StatusForbidden || !strings.Contains(answer["error"].(string), "grant:manage") {
		t.Errorf("an editor's commit to the protected branch is answered %d %v", status, answer)
	}
	if status, answer := g.committed("owner", map[string]any{"parent": first["commit"], "message": "protected", "files": map[string]any{"a.txt": "a\n"}}); status != http.StatusCreated {
		t.Errorf("an owner's commit to the protected branch is answered %d %v", status, answer)
	}
}

// A path off a tree's rules, a directory written as a file, and a file removed that the parent does
// not hold are refused before anything is pushed.
func TestACommitOfPathsATreeCannotHoldIsRefused(t *testing.T) {
	g := servingGit(t, everyone())
	_, first := g.committed("alice", map[string]any{"message": "first", "files": map[string]any{"agentiik.yaml": workflowDocument, "scripts/a.sh": "true\n"}})
	for name, files := range map[string]map[string]any{
		"a path leaving the tree": {"../outside.yaml": "x"},
		"a .git directory":        {".git/config": "x"},
		"a directory as a file":   {"scripts": "x"},
		"a file as a directory":   {"agentiik.yaml/x": "x"},
		"a file nobody holds":     {"missing.txt": nil},
	} {
		if status, answer := g.committed("alice", map[string]any{"parent": first["commit"], "message": name, "files": files}); status != http.StatusUnprocessableEntity {
			t.Errorf("%s is answered %d %v", name, status, answer)
		}
	}
	if status, answer := g.committed("alice", map[string]any{"parent": first["commit"], "message": "", "files": map[string]any{"a.txt": "a"}}); status != http.StatusUnprocessableEntity {
		t.Errorf("a commit with no message is answered %d %v", status, answer)
	}
	if status, answer := g.committed("alice", map[string]any{"parent": first["commit"], "message": "nothing"}); status != http.StatusUnprocessableEntity {
		t.Errorf("a commit writing no file is answered %d %v", status, answer)
	}
	if got := g.refs()["refs/heads/main"]; got != first["commit"] {
		t.Errorf("main names %s after refusals, and the commit was %s", got, first["commit"])
	}
}

// A user is the author by their name and their email address, where an administrator gave one.
func TestACommitIsAuthoredByTheUsersNameAndAddress(t *testing.T) {
	g := servingGit(t, everyone())
	if _, err := dbtest.Superuser(t, g.super).Exec(t.Context(),
		`insert into principals (id, kind) values ('alice', 'user'); insert into users (login, given_name, family_name, email) values ('alice', 'Alice', 'Martin', 'alice.martin@example.com')`); err != nil {
		t.Fatal(err)
	}
	g.committed("alice", map[string]any{"message": "first", "files": map[string]any{"agentiik.yaml": workflowDocument}})
	if out := strings.TrimSpace(g.cloned("alice").must("log", "--format=%an <%ae>", "-1")); out != "Alice Martin <alice.martin@example.com>" {
		t.Errorf("the commit is authored by %q", out)
	}
}

// workflow.validate and workflow.commit are their routes made as the caller: the draft refused is
// answered with where, what was expected and the topic, and the commit made is a real git commit,
// marked as made through MCP in its message and in the audit log, naming the tool.
func TestAClientValidatesAndCommitsThroughTheUsersServer(t *testing.T) {
	g := servingGit(t, owningGrants{granted: everyone().(granted)})
	rt := g.h.(*api.Router)
	if _, err := api.NewMCP(rt, api.MCPOptions{PublicURL: "https://agentiik.example.com"}); err != nil {
		t.Fatal(err)
	}
	where := map[string]any{"namespace": "finance", "workflow": "monthly-invoicing"}
	args := func(more map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range where {
			out[k] = v
		}
		for k, v := range more {
			out[k] = v
		}
		return out
	}

	_, res, rpcErr := called(t, rt, "alice", "tools/call", map[string]any{"name": "workflow.validate", "arguments": args(map[string]any{"files": map[string]any{"agentiik.yaml": workflowDocument + "stepz: {}\n"}})})
	if rpcErr != nil || res["isError"] != true {
		t.Fatalf("a draft the hook refuses is answered %v, %v", res, rpcErr)
	}
	said := res["content"].([]any)[0].(map[string]any)["text"].(string)
	for _, want := range []string{"/stepz", "expected", "workflow.language with topic repository"} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal reads %q, and does not say %s", said, want)
		}
	}
	_, res, rpcErr = called(t, rt, "alice", "tools/call", map[string]any{"name": "workflow.validate", "arguments": args(map[string]any{"files": map[string]any{"agentiik.yaml": workflowDocument}})})
	if rpcErr != nil || res["isError"] == true || res["structuredContent"].(map[string]any)["valid"] != true {
		t.Fatalf("a valid draft is answered %v, %v", res, rpcErr)
	}

	_, res, rpcErr = called(t, rt, "alice", "tools/call", map[string]any{"name": "workflow.commit", "arguments": args(map[string]any{"message": "Draft the invoicing", "files": map[string]any{"agentiik.yaml": workflowDocument}})})
	if rpcErr != nil || res["isError"] == true {
		t.Fatalf("the commit is answered %v, %v", res, rpcErr)
	}
	commit := res["structuredContent"].(map[string]any)["commit"].(string)
	if g.refs()["refs/heads/main"] != commit {
		t.Errorf("main does not name the commit workflow.commit made")
	}
	if body := strings.TrimSpace(g.cloned("alice").must("log", "--format=%B", "-1")); body != "Draft the invoicing\n\nVia: mcp" {
		t.Errorf("the commit's message reads %q", body)
	}
	var marked bool
	for _, e := range audited(t, g.pool) {
		d := detailOf(t, e)
		marked = marked || (e.Action == audit.RefUpdate && d["new"] == commit && d["through"] == "mcp" && d["tool"] == "workflow.commit")
	}
	if !marked {
		t.Error("the ref the commit moved is not recorded as moved through MCP by workflow.commit")
	}

	// The route's grants are the tool's: a reader commits nothing.
	_, res, _ = called(t, rt, "bob", "tools/call", map[string]any{"name": "workflow.commit", "arguments": args(map[string]any{"parent": commit, "message": "a reader", "files": map[string]any{"a.txt": "a"}})})
	if res["isError"] != true || g.refs()["refs/heads/main"] != commit {
		t.Errorf("a reader's commit through MCP is answered %v", res)
	}
}
