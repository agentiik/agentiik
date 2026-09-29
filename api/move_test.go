package api_test

import (
	"context"
	"net/http"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/purge"
)

// owningGrants is granted, with the namespaces each principal owns: "move between namespaces the
// principal owns on both sides".
type owningGrants struct {
	granted
	owned map[api.Principal][]string
}

func (o owningGrants) Owned(_ context.Context, who api.Principal) ([]string, error) {
	return o.owned[who], nil
}

// "Moves it to another namespace, namespace under workflow:write and ownership of both namespaces",
// and "a move answers 202 with Location, whatever else the body names, and carries everything too
// ... the workflow frozen meanwhile, its pushes, runs and replays answered 409; it is refused with
// 409 while a run of it is queued or running ... and where it names a secret the target does not
// declare". Asked, refused, frozen, carried out by the controller's pass, and then read, run and
// cloned where it went.
func TestAWorkflowMovesToAnotherNamespaceItsMoverOwns(t *testing.T) {
	finance, ops := api.Target{Namespace: "finance"}, api.Target{Namespace: "team-ops"}
	everything := func(over api.Target) []grant {
		return []grant{{api.WorkflowRead, over}, {api.WorkflowWrite, over}, {api.WorkflowRun, over}, {api.RunRead, over}, {api.SecretUse, over}}
	}
	g := servingGit(t, owningGrants{
		granted: granted{
			"alice": append(everything(finance), everything(ops)...),
			"bob":   everything(finance),
		},
		owned: map[api.Principal][]string{"alice": {"finance", "team-ops"}, "bob": {"finance"}},
	})
	conn := dbtest.Superuser(t, g.super)
	sql := func(stmt string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(t.Context(), stmt, args...); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	sql(`insert into secret_declarations (namespace, name, provider, declared_by) values ('finance', 'billing', 'builtin', 'alice')`)
	work := g.newClone("alice")
	work.write("agentiik.yaml", strings.Replace(workflowDocument, "steps:\n", "secrets: [billing]\nsteps:\n", 1))
	work.write("scripts/normalize.py", "print('normalize')\n")
	pushed := work.commit("first")
	work.must("push", "-q", "origin", "main")

	at := "/api/v1/finance/workflows/monthly-invoicing"
	move := map[string]any{"namespace": "team-ops"}

	// Owning one side is not enough, and a target nobody can own is answered as one that is not
	// there.
	if w, _ := call(t, g.h, "PATCH", at, "bob", move); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "no namespace team-ops you own") {
		t.Errorf("a move to a namespace its mover does not own answered %d: %s", w.Code, w.Body)
	}
	if w, _ := call(t, g.h, "PATCH", at, "alice", map[string]any{"namespace": "nowhere"}); w.Code != http.StatusNotFound {
		t.Errorf("a move to a namespace that does not exist answered %d: %s", w.Code, w.Body)
	}
	// The secret its version names, which the target does not declare.
	if w, _ := call(t, g.h, "PATCH", at, "alice", move); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "names the secret billing, which team-ops does not declare") {
		t.Errorf("a move naming a secret the target lacks answered %d: %s", w.Code, w.Body)
	}
	sql(`insert into secret_declarations (namespace, name, provider, declared_by) values ('team-ops', 'billing', 'builtin', 'alice')`)
	// A run of it going, and a request that would rename it as well changes nothing.
	w, started := call(t, g.h, "POST", at+"/runs", "alice", map[string]any{"inputs": map[string]any{"orders": []any{}, "cycle": "2026-09", "edges": nil, "n": 1}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("the run was answered %d: %s", w.Code, w.Body)
	}
	run := started["run"].(string)
	if w, _ := call(t, g.h, "PATCH", at, "alice", map[string]any{"namespace": "team-ops", "name": "invoicing"}); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "has not finished") {
		t.Errorf("a move with a run queued answered %d: %s", w.Code, w.Body)
	}
	if w, _ := call(t, g.h, "GET", at, "alice", nil); w.Code != http.StatusOK {
		t.Errorf("a move refused renamed the workflow all the same: %d", w.Code)
	}
	sql(`update runs set state = 'succeeded', started_at = now(), finished_at = now() where id = $1`, run)

	// Asked: 202, and where it will be.
	w, _ = call(t, g.h, "PATCH", at, "alice", move)
	if w.Code != http.StatusAccepted || w.Header().Get("Location") != "/api/v1/team-ops/workflows/monthly-invoicing" || w.Body.Len() != 0 {
		t.Fatalf("the move answered %d at %q: %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	// Frozen: a run, a push, a rename, and the name held where it goes.
	if w, _ := call(t, g.h, "POST", at+"/runs", "alice", map[string]any{"inputs": map[string]any{"orders": []any{}, "cycle": "2026-09", "edges": nil, "n": 1}}); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "being moved") {
		t.Errorf("a run of a moving workflow answered %d: %s", w.Code, w.Body)
	}
	if w, _ := call(t, g.h, "PATCH", at, "alice", map[string]any{"name": "invoicing"}); w.Code != http.StatusConflict {
		t.Errorf("a rename of a moving workflow answered %d: %s", w.Code, w.Body)
	}
	if w, _ := call(t, g.h, "POST", "/api/v1/team-ops/workflows", "alice", map[string]any{"name": "monthly-invoicing"}); w.Code != http.StatusConflict {
		t.Errorf("a workflow created under the name a move holds answered %d: %s", w.Code, w.Body)
	}
	work.write("scripts/normalize.py", "print('normalize, twice')\n")
	work.commit("second")
	if out, err := work.run("push", "origin", "main"); err == nil || !strings.Contains(out, "being moved") {
		t.Errorf("a push to a moving workflow is answered:\n%s", out)
	}
	work.must("reset", "-q", "--hard", "HEAD~1")

	// Carried out by the controller's pass.
	purger := &purge.Purger{Pool: g.pool, Objects: artifact.Dir(g.objects)}
	purged, err := purger.Pass(t.Context())
	if err != nil || purged.Moved != 1 {
		t.Fatalf("the pass moved %d workflows: %v", purged.Moved, err)
	}

	// Read, run and cloned where it went, and nowhere where it was.
	moved := "/api/v1/team-ops/workflows/monthly-invoicing"
	if w, _ := call(t, g.h, "GET", at, "alice", nil); w.Code != http.StatusNotFound {
		t.Errorf("the workflow still answers where it was: %d", w.Code)
	}
	w, detail := call(t, g.h, "GET", moved, "alice", nil)
	if w.Code != http.StatusOK || detail["version"].(map[string]any)["commit"] != pushed {
		t.Fatalf("the moved workflow reads %d: %s", w.Code, w.Body)
	}
	if w, _ := call(t, g.h, "GET", moved+"/tree/main?path=scripts/normalize.py", "alice", nil); w.Code != http.StatusOK || w.Body.String() != "print('normalize')\n" {
		t.Errorf("a file of its tree reads %d: %q", w.Code, w.Body)
	}
	if w, detail := call(t, g.h, "GET", "/api/v1/runs/"+run, "alice", nil); w.Code != http.StatusOK || detail["namespace"] != "team-ops" {
		t.Errorf("its run reads %d: %v", w.Code, detail)
	}
	if w, _ := call(t, g.h, "POST", moved+"/runs", "alice", map[string]any{"inputs": map[string]any{"orders": []any{}, "cycle": "2026-09", "edges": nil, "n": 1}}); w.Code != http.StatusAccepted {
		t.Errorf("a run where it went answered %d: %s", w.Code, w.Body)
	}
	parent := t.TempDir()
	cloning := exec.Command("git", "clone", "-q", strings.Replace(g.url, "http://", "http://agk:alice@", 1)+"/team-ops/monthly-invoicing.git", filepath.Join(parent, "moved"))
	cloning.Dir, cloning.Env = parent, gitEnv(parent)
	if out, err := cloning.CombinedOutput(); err != nil {
		t.Fatalf("the moved repository could not be cloned: %s\n%s", err, out)
	}
	there := &clone{t: t, dir: filepath.Join(parent, "moved")}
	if head := strings.TrimSpace(there.must("rev-parse", "HEAD")); head != pushed {
		t.Errorf("a clone of the moved repository reads HEAD as %s", head)
	}

	var recorded []string
	for _, e := range audited(t, g.pool) {
		if e.Action == audit.WorkflowUpdate && e.Actor == "alice" && detailOf(t, e)["namespace"] == "team-ops" {
			recorded = append(recorded, e.Namespace)
		}
	}
	slices.Sort(recorded)
	if !slices.Equal(recorded, []string{"finance", "team-ops"}) {
		t.Errorf("the move is recorded in %v", recorded)
	}
}
