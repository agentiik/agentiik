package api_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
)

// A workflow's repository as the API answers it: created empty, read with its history, its tree read
// at a ref, and its default branch named and protected. The git tests' installation, since the
// history and the tree are what git pushes.

// holdingRepositories holds what alice needs to push, bob to read, and owner to do what only an owner does, as
// everyone does, and workflow:run and run:read besides to alice.
func holdingRepositories() api.Authorizer {
	finance := api.Target{Namespace: "finance"}
	return granted{
		"alice": {{api.WorkflowRead, finance}, {api.WorkflowWrite, finance}, {api.WorkflowRun, finance}, {api.RunRead, finance}},
		"bob":   {{api.WorkflowRead, finance}},
		"owner": {{api.WorkflowRead, finance}, {api.WorkflowWrite, finance}, {api.GrantManage, finance}, {api.SecretUse, finance}},
	}
}

// remoteOf is another workflow's repository, as who.
func (g *gitServer) remoteOf(who, workflow string) string {
	return strings.Replace(g.url, "http://", "http://agk:"+who+"@", 1) + "/finance/" + workflow + ".git"
}

// "An empty workflow repository: {name, default_branch, protected}, main and unprotected where left
// out, answered 201 with the repository, and 409 where the namespace holds a workflow of that name."
func TestARepositoryIsCreatedEmptyAndItsProtectionHoldsFromItsFirstPush(t *testing.T) {
	g := servingGit(t, holdingRepositories())
	w, answer := call(t, g.h, "POST", "/api/v1/finance/workflows", "alice", map[string]any{"name": "vat-reconciliation", "protected": true})
	if w.Code != http.StatusCreated {
		t.Fatalf("creating a repository answered %d: %s", w.Code, w.Body)
	}
	if answer["default_branch"] != "main" || answer["protected"] != true || answer["head"] != nil ||
		answer["clone_url"] != "https://agentiik.example.com/finance/vat-reconciliation.git" {
		t.Errorf("the repository created is %v", answer)
	}
	for name, body := range map[string]any{
		"a name held":              map[string]any{"name": "vat-reconciliation"},
		"a name off grammar":       map[string]any{"name": "-vat"},
		"a branch git refuses":     map[string]any{"name": "other", "default_branch": "a..b"},
		"a field it does not read": map[string]any{"name": "other", "labels": map[string]any{}},
	} {
		want := http.StatusBadRequest
		if name == "a name held" {
			want = http.StatusConflict
		}
		if w, _ := call(t, g.h, "POST", "/api/v1/finance/workflows", "alice", body); w.Code != want {
			t.Errorf("%s answered %d, not %d: %s", name, w.Code, want, w.Body)
		}
	}
	var created bool
	for _, e := range audited(t, g.pool) {
		if e.Action == audit.WorkflowCreate && e.Target == "vat-reconciliation" && e.Actor == "alice" {
			created = true
		}
	}
	if !created {
		t.Error("the repository's creation is not recorded")
	}

	// Protected from its first push: an editor's is refused, an owner's taken.
	if err := g.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		_, err := ns.RecordImages(ctx, "vat-reconciliation", "alice", time.Time{}, db.Images{Manifests: map[string][]byte{image: []byte(brickManifest)}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	work := g.newClone("alice")
	work.must("remote", "set-url", "origin", g.remoteOf("alice", "vat-reconciliation"))
	work.write("agentiik.yaml", named(workflowDocument, "finance", "vat-reconciliation"))
	work.commit("first")
	if out, err := work.run("push", "origin", "main"); err == nil || !strings.Contains(out, "protected") {
		t.Errorf("an editor's first push to the protected default branch is answered:\n%s", out)
	}
	work.must("remote", "set-url", "origin", g.remoteOf("owner", "vat-reconciliation"))
	work.must("push", "-q", "origin", "main")
}

// "The repository; the version a run naming no ref runs, the default branch's head, with the graph
// it resolves to; and a page of the default branch's first-parent history, newest first, each commit
// that is a version marked."
func TestAWorkflowIsReadWithItsHeadItsGraphAndItsHistory(t *testing.T) {
	g := servingGit(t, holdingRepositories())
	work := g.newClone("alice")
	work.write("agentiik.yaml", workflowDocument)
	first := work.commit("first")
	work.write("scripts/a.sh", "true\n")
	second := work.commit("second, which no ref is left at")
	work.write("scripts/b.sh", "true\n")
	third := work.commit("third\n\nwith a body")
	work.must("push", "-q", "origin", "main")

	const at = "/api/v1/finance/workflows/monthly-invoicing"
	w, answer := call(t, g.h, "GET", at, "bob", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("reading the workflow answered %d: %s", w.Code, w.Body)
	}
	repository := answer["repository"].(map[string]any)
	if repository["head"] != third {
		t.Errorf("the head is %v, where main is %s", repository["head"], third)
	}
	if v, _ := answer["version"].(map[string]any); v["commit"] != third || v["source"] != "git" {
		t.Errorf("the version a run naming no ref runs is %v", answer["version"])
	}
	if g, _ := answer["graph"].(map[string]any); g["commit"] != third {
		t.Errorf("the graph is %v", answer["graph"])
	}
	history := answer["history"].([]any)
	if len(history) != 3 {
		t.Fatalf("the history is %v", history)
	}
	for i, want := range []string{third, second, first} {
		e := history[i].(map[string]any)
		if e["commit"] != want {
			t.Errorf("entry %d is %v, where %s was expected", i, e["commit"], want)
		}
		if a, _ := e["author"].(map[string]any); a["name"] != "Alice" || a["email"] != "alice@example.com" {
			t.Errorf("entry %d is written by %v", i, e["author"])
		}
		if (e["version"] != nil) != (want == third) {
			t.Errorf("entry %d is marked a version: %v", i, e["version"])
		}
	}
	if e := history[0].(map[string]any); e["subject"] != "third" || e["authored_at"] != "2026-09-29T10:00:00Z" {
		t.Errorf("the head's entry is %v", e)
	}

	w, answer = call(t, g.h, "GET", at+"?limit=2", "bob", nil)
	if w.Code != http.StatusOK || len(answer["history"].([]any)) != 2 || answer["next"] != first {
		t.Errorf("a page of two answered %d: %v, next %v", w.Code, answer["history"], answer["next"])
	}
	w, answer = call(t, g.h, "GET", at+"?limit=2&from="+first, "bob", nil)
	if w.Code != http.StatusOK || len(answer["history"].([]any)) != 1 || answer["next"] != nil {
		t.Errorf("the next page answered %d: %v, next %v", w.Code, answer["history"], answer["next"])
	}
	for query, want := range map[string]int{
		"?from=" + strings.Repeat("1", 40): http.StatusNotFound,
		"?from=abc":                        http.StatusBadRequest,
		"?limit=0":                         http.StatusBadRequest,
		"?limit=501":                       http.StatusBadRequest,
	} {
		if w, _ := call(t, g.h, "GET", at+query, "bob", nil); w.Code != want {
			t.Errorf("%s answered %d, not %d: %s", query, w.Code, want, w.Body)
		}
	}
}

// "A workflow no git push has filled lists the versions agk push sent as trees instead", and a run
// naming no commit runs the latest of them; once git fills it, the default branch's head.
func TestARunNamingNoCommitRunsTheDefaultBranchOrTheLatestTreeVersion(t *testing.T) {
	g := servingGit(t, holdingRepositories())
	const at = "/api/v1/finance/workflows/monthly-invoicing"
	if w, _ := call(t, g.h, "PUT", at+"/versions/"+aCommit, "alice", aPush(t)); w.Code != http.StatusOK {
		t.Fatalf("the tree push answered %d: %s", w.Code, w.Body)
	}
	_, answer := call(t, g.h, "GET", at, "bob", nil)
	if answer["repository"].(map[string]any)["head"] != nil {
		t.Errorf("a repository no git push filled has a head: %v", answer["repository"])
	}
	if v, _ := answer["version"].(map[string]any); v["commit"] != aCommit || v["source"] != "tree" {
		t.Errorf("the version of a repository no git push filled is %v", answer["version"])
	}
	if h := answer["history"].([]any); len(h) != 1 || h[0].(map[string]any)["commit"] != aCommit {
		t.Errorf("the history of a repository no git push filled is %v", h)
	}
	if got := runOf(t, g, at); got != aCommit {
		t.Fatalf("a run naming no commit runs %s, where the latest tree version is %s", got, aCommit)
	}

	work := g.newClone("alice")
	work.write("agentiik.yaml", workflowDocument)
	work.write("scripts/a.sh", "true\n")
	head := work.commit("the first git push")
	work.must("push", "-q", "origin", "main")
	if got := runOf(t, g, at); got != head {
		t.Errorf("a run naming no commit once git filled the repository runs %s, where main is %s", got, head)
	}
}

// runOf starts a run of the workflow at naming no commit, and answers the commit it was pinned to.
func runOf(t *testing.T, g *gitServer, at string) string {
	t.Helper()
	w, started := call(t, g.h, "POST", at+"/runs", "alice", map[string]any{"inputs": map[string]any{"orders": []any{}}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("a run naming no commit answered %d: %s", w.Code, w.Body)
	}
	w, detail := call(t, g.h, "GET", "/api/v1/runs/"+started["run"].(string), "alice", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("reading the run answered %d: %s", w.Code, w.Body)
	}
	commit, _ := detail["commit"].(string)
	return commit
}

// "The tree at a ref: every file with its path, mode, size and SHA-256, or with ?path= that one
// file's bytes. The ref is a branch or a tag, by its short name or in full, or a whole commit that is
// a version; a name a branch and a tag both hold is 400."
func TestTheTreeAtARefIsListedAndAFileOfItRead(t *testing.T) {
	g := servingGit(t, holdingRepositories())
	work := g.newClone("alice")
	work.write("agentiik.yaml", workflowDocument)
	work.write("scripts/normalize.py", "print('normalize')\n")
	commit := work.commit("first")
	work.must("tag", "release")
	work.must("branch", "both")
	work.must("tag", "both")
	work.must("push", "-q", "origin", "main", "release", "refs/heads/both", "refs/tags/both")

	const tree = "/api/v1/finance/workflows/monthly-invoicing/tree/"
	for _, ref := range []string{"main", "refs/heads/main", "release", "refs/tags/release", commit, "refs/heads/both"} {
		w, answer := call(t, g.h, "GET", tree+ref, "bob", nil)
		if w.Code != http.StatusOK || answer["commit"] != commit || len(answer["entries"].([]any)) != 2 {
			t.Errorf("the tree at %s answered %d: %s", ref, w.Code, w.Body)
			continue
		}
		e := answer["entries"].([]any)[1].(map[string]any)
		if e["path"] != "scripts/normalize.py" || e["mode"] != "0644" || e["size"] != float64(len("print('normalize')\n")) {
			t.Errorf("the tree at %s lists %v", ref, e)
		}
	}
	for ref, want := range map[string]int{"both": http.StatusBadRequest, "nothing": http.StatusNotFound, strings.Repeat("1", 40): http.StatusNotFound} {
		if w, _ := call(t, g.h, "GET", tree+ref, "bob", nil); w.Code != want {
			t.Errorf("the tree at %s answered %d, not %d: %s", ref, w.Code, want, w.Body)
		}
	}

	r, _ := http.NewRequestWithContext(t.Context(), "GET", g.url+tree+"main?path=scripts/normalize.py", nil)
	r.Header.Set("Authorization", "Bearer bob")
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || string(got) != "print('normalize')\n" {
		t.Errorf("a file of the tree answered %d: %q", res.StatusCode, got)
	}
	for _, path := range []string{"scripts", "nothing.py"} {
		if w, _ := call(t, g.h, "GET", tree+"main?path="+path, "bob", nil); w.Code != http.StatusNotFound {
			t.Errorf("?path=%s answered %d: %s", path, w.Code, w.Body)
		}
	}
}

// "Names its default branch and protects it, default_branch and protected under grant:manage, a
// branch the repository does not hold refused with 422 ... a change of protection as ref.protect."
func TestTheDefaultBranchIsNamedAndProtectedByWhoeverHoldsGrantManage(t *testing.T) {
	g := servingGit(t, holdingRepositories())
	work := g.newClone("alice")
	work.write("agentiik.yaml", workflowDocument)
	work.commit("first")
	work.must("push", "-q", "origin", "main", "main:feature")

	const at = "/api/v1/finance/workflows/monthly-invoicing"
	if w, _ := call(t, g.h, "PATCH", at, "alice", map[string]any{"protected": true}); w.Code != http.StatusNotFound {
		t.Errorf("an editor protecting the default branch answered %d: %s", w.Code, w.Body)
	}
	w, answer := call(t, g.h, "PATCH", at, "owner", map[string]any{"protected": true})
	if w.Code != http.StatusOK || answer["protected"] != true {
		t.Fatalf("an owner protecting the default branch answered %d: %s", w.Code, w.Body)
	}
	work.write("scripts/a.sh", "true\n")
	work.commit("second")
	if out, err := work.run("push", "origin", "main"); err == nil || !strings.Contains(out, "protected") {
		t.Errorf("an editor's push to the protected branch is answered:\n%s", out)
	}

	// The protection moves with the default branch.
	w, answer = call(t, g.h, "PATCH", at, "owner", map[string]any{"default_branch": "feature"})
	if w.Code != http.StatusOK || answer["default_branch"] != "feature" || answer["protected"] != true {
		t.Fatalf("naming another default branch answered %d: %s", w.Code, w.Body)
	}
	work.must("push", "-q", "origin", "main")
	if out, err := work.run("push", "origin", "main:feature"); err == nil || !strings.Contains(out, "protected") {
		t.Errorf("an editor's push to the new default branch is answered:\n%s", out)
	}

	for name, body := range map[string]map[string]any{
		"a branch it does not hold": {"default_branch": "nothing"},
		"a rename":                  {"name": "invoicing"},
		"nothing":                   {},
	} {
		want := http.StatusBadRequest
		if name == "a branch it does not hold" {
			want = http.StatusUnprocessableEntity
		}
		if w, _ := call(t, g.h, "PATCH", at, "owner", body); w.Code != want {
			t.Errorf("%s answered %d, not %d: %s", name, w.Code, want, w.Body)
		}
	}

	var protected, updated int
	for _, e := range audited(t, g.pool) {
		switch {
		case e.Action == audit.RefProtect && e.Actor == "owner":
			protected++
		case e.Action == audit.WorkflowUpdate && e.Actor == "owner":
			updated++
		}
	}
	// Protected once, then moved: the branch that took the protection over and the branch that
	// lost it, each once.
	if protected != 3 || updated != 1 {
		t.Errorf("the changes are recorded as %d ref.protect and %d workflow.update", protected, updated)
	}
}
