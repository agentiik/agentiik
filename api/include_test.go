package api_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/version"
)

// A workflow include, against the git routes and a real PostgreSQL: a library pushed with git is a
// version marked as one, which nothing runs, and a workflow including it at a tag or a commit is
// judged against what its hook accepted, under the pusher's workflow:read on it, and keeps what it
// read so that its runs name the commit it resolved to.

// including is monthly-invoicing including finance/common at ref, its one step extending the block
// the library carries.
func including(workflow, ref string) string {
	return fmt.Sprintf(`apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
include:
  - workflow: %s
    ref: %s
steps:
  normalize:
    extends: .api-brick
    image: alpine@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    script: ["./normalize.sh"]
    outputs: [ok]
`, workflow, ref)
}

func TestAWorkflowIncludeReadsALibraryAtATagOrACommitItsHookJudged(t *testing.T) {
	finance, ops := api.Target{Namespace: "finance"}, api.Target{Namespace: "team-ops"}
	g := servingGit(t, granted{
		"alice": {{api.WorkflowRead, finance}, {api.WorkflowWrite, finance}, {api.WorkflowRun, finance}, {api.RunRead, finance}},
		"ops":   {{api.WorkflowRead, ops}, {api.WorkflowWrite, ops}},
	})
	if err := g.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.SaveWorkflow(ctx, "common", "main")
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.pool.In(t.Context(), "team-ops", func(ctx context.Context, ns *db.NS) error {
		return ns.SaveWorkflow(ctx, "shared", "main")
	}); err != nil {
		t.Fatal(err)
	}

	// The library, pushed with git and tagged: its root is a fragment, its hook judges it as one.
	lib := g.newClone("alice")
	lib.must("remote", "set-url", "origin", g.remoteOf("alice", "common"))
	lib.write("agentiik.yaml", "include:\n  - path: ./blocks/api.yaml\n")
	lib.write("blocks/api.yaml", ".api-brick:\n  timeout: 2m\n")
	tagged := lib.commit("the blocks")
	lib.must("tag", "v2.1.0")
	lib.must("push", "-q", "origin", "main", "v2.1.0")
	lib.write("blocks/api.yaml", ".api-brick:\n  timeout: 3m\n")
	head := lib.commit("a longer timeout")
	lib.must("push", "-q", "origin", "main")
	var libVersion db.Version
	if err := g.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		var err error
		libVersion, err = ns.Version(ctx, "common", tagged)
		return err
	}); err != nil {
		t.Fatalf("the library's tagged commit is no version: %v", err)
	}
	if !libVersion.Library || len(libVersion.Includes) != 1 {
		t.Errorf("the library's version is %+v", libVersion)
	}
	broken := g.newClone("alice")
	broken.must("remote", "set-url", "origin", g.remoteOf("alice", "common"))
	broken.must("fetch", "-q", "origin")
	broken.must("reset", "-q", "--hard", "origin/main")
	broken.write("agentiik.yaml", "include:\n  - path: ./missing.yaml\n")
	broken.commit("an include of nothing")
	if out, err := broken.run("push", "origin", "main"); err == nil || !strings.Contains(out, "include-missing") {
		t.Errorf("a library including a file it lacks was answered:\n%s", out)
	}

	// Nothing runs a library, and its repository says what it is.
	if w, _ := call(t, g.h, "POST", "/api/v1/finance/workflows/common/runs", "alice", map[string]any{}); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "is a library") {
		t.Errorf("a run of a library is answered %d: %s", w.Code, w.Body)
	}
	if w, detail := call(t, g.h, "GET", "/api/v1/finance/workflows/common", "alice", nil); w.Code != http.StatusOK || detail["version"].(map[string]any)["library"] != true || detail["graph"] != nil {
		t.Errorf("the library's repository reads %d: %s", w.Code, w.Body)
	}

	// A workflow including it at the tag reads the tagged commit, whatever main does since, and
	// keeps what it read.
	work := g.newClone("alice")
	work.write("agentiik.yaml", including("finance/common", "v2.1.0"))
	first := work.commit("include the blocks")
	work.must("push", "-q", "origin", "main")
	v, err := g.version(first)
	if err != nil {
		t.Fatalf("the including commit is no version: %v", err)
	}
	kept := v.Libraries["finance/common@v2.1.0"]
	if kept.Commit != tagged || string(kept.Files["blocks/api.yaml"]) != ".api-brick:\n  timeout: 2m\n" {
		t.Errorf("the version kept %s and %q", kept.Commit, kept.Files["blocks/api.yaml"])
	}
	built, err := version.Build(v)
	if err != nil {
		t.Fatalf("the version does not rebuild from what it kept: %v", err)
	}
	if st, _ := built.Step("normalize"); st.Timeout.String() != "2m" {
		t.Errorf("the included block gave the step a timeout of %s", st.Timeout)
	}

	// Its runs name the commit the include resolved to.
	w, started := call(t, g.h, "POST", "/api/v1/finance/workflows/monthly-invoicing/runs", "alice", map[string]any{})
	if w.Code != http.StatusAccepted {
		t.Fatalf("the run was answered %d: %s", w.Code, w.Body)
	}
	if w, detail := call(t, g.h, "GET", "/api/v1/runs/"+started["run"].(string), "alice", nil); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"includes":[{"workflow":"finance/common","ref":"v2.1.0","commit":"`+tagged+`"}]`) {
		t.Errorf("the run reads %d: %v", w.Code, detail)
	}

	// At a commit, written whole.
	work.write("agentiik.yaml", including("finance/common", head))
	work.commit("include the longer timeout")
	work.must("push", "-q", "origin", "main")
	if v, err := g.version(strings.TrimSpace(work.must("rev-parse", "HEAD"))); err != nil || v.Libraries["finance/common@"+head].Commit != head {
		t.Errorf("an include at a commit kept %+v: %v", v.Libraries, err)
	}

	// And refused: a branch, a commit abbreviated, a workflow the pusher may not read and one
	// that is not there alike, a ref naming nothing, and a repository that is no library.
	for ref, says := range map[[2]string]string{
		{"finance/common", "main"}:           "names a branch of finance/common",
		{"finance/common", head[:9]}:         "a workflow include names a commit whole",
		{"team-ops/shared", "v1"}:            "team-ops/shared is no workflow you may read",
		{"finance/nothing", "v1"}:            "finance/nothing is no workflow you may read",
		{"finance/common", "v9"}:             "v9 names no tag of finance/common",
		{"finance/monthly-invoicing", first}: "is a workflow, and a workflow include reads a library",
	} {
		work.write("agentiik.yaml", including(ref[0], ref[1]))
		work.commit("include " + ref[0] + "@" + ref[1])
		out, err := work.run("push", "origin", "main")
		if err == nil || !strings.Contains(out, says) {
			t.Errorf("an include of %s at %s was answered:\n%s", ref[0], ref[1], out)
		}
		work.must("reset", "-q", "--hard", "HEAD~1")
	}
}
