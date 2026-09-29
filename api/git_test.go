package api_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/version"
)

// Git's smart HTTP, held to the git binary: what a person does with git, clone, commit, push and
// fetch, against the routes over a real PostgreSQL. A test that cannot find git fails rather than
// skips, as package repo's do, since a skip would be a green run that proved nothing.

// gitServer is the API over finance and team-ops, with the workflow monthly-invoicing created empty
// in finance, and the manifest of image recorded in its repository, served over HTTP for git.
type gitServer struct {
	t     *testing.T
	url   string
	pool  *db.Pool
	super string
}

func servingGit(t *testing.T, auth api.Authorizer) *gitServer {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("these tests drive the git binary, and there is none on this machine")
	}
	pool, super := dbtest.Open(t)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `insert into namespaces (name) values ('finance'), ('team-ops')`); err != nil {
		t.Fatal(err)
	}
	store, err := version.New(pool, version.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(auth, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewServer(rt, api.ServerOptions{Pool: pool, Versions: store, Objects: artifact.Dir(t.TempDir())}); err != nil {
		t.Fatal(err)
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		if err := ns.SaveWorkflow(ctx, "monthly-invoicing", "main"); err != nil {
			return err
		}
		_, err := ns.RecordImages(ctx, "monthly-invoicing", "alice", time.Time{}, db.Images{Manifests: map[string][]byte{image: []byte(brickManifest)}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(rt)
	t.Cleanup(server.Close)
	return &gitServer{t: t, url: server.URL, pool: pool, super: super}
}

// remote is the repository's URL, with the principal as the password git sends, which is how a
// credential helper would hand it a token.
func (g *gitServer) remote(who string) string {
	return strings.Replace(g.url, "http://", "http://agk:"+who+"@", 1) + "/finance/monthly-invoicing.git"
}

// clone is a working copy of the test's own.
type clone struct {
	t   *testing.T
	dir string
}

// gitEnv is an environment with no configuration but git's own, no prompt, and a fixed author and
// date, so that nobody's settings change what git does and the same commands make the same commits.
func gitEnv(home string) []string {
	return []string{
		"HOME=" + home, "PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Alice", "GIT_AUTHOR_EMAIL=alice@example.com", "GIT_AUTHOR_DATE=2026-09-29T10:00:00Z",
		"GIT_COMMITTER_NAME=Alice", "GIT_COMMITTER_EMAIL=alice@example.com", "GIT_COMMITTER_DATE=2026-09-29T10:00:00Z",
	}
}

// run runs git in the clone and answers its output and standard error, and its failure.
func (c *clone) run(args ...string) (string, error) {
	c.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = c.dir
	cmd.Env = gitEnv(filepath.Dir(c.dir))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// must runs git and fails the test where it fails.
func (c *clone) must(args ...string) string {
	c.t.Helper()
	out, err := c.run(args...)
	if err != nil {
		c.t.Fatalf("git %s: %s\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// write writes a file of the working copy.
func (c *clone) write(name, content string) {
	c.t.Helper()
	path := filepath.Join(c.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		c.t.Fatal(err)
	}
}

// newClone is an empty working copy whose origin is the repository, as who.
func (g *gitServer) newClone(who string) *clone {
	g.t.Helper()
	dir := filepath.Join(g.t.TempDir(), "work")
	if err := os.Mkdir(dir, 0o755); err != nil {
		g.t.Fatal(err)
	}
	c := &clone{t: g.t, dir: dir}
	c.must("init", "-q", "-b", "main")
	c.must("remote", "add", "origin", g.remote(who))
	return c
}

// cloned is a clone of the repository, as who.
func (g *gitServer) cloned(who string, args ...string) *clone {
	g.t.Helper()
	parent := g.t.TempDir()
	c := &clone{t: g.t, dir: filepath.Join(parent, "work")}
	cmd := exec.Command("git", append(append([]string{"clone", "-q"}, args...), g.remote(who), c.dir)...)
	cmd.Dir = parent
	cmd.Env = gitEnv(parent)
	if out, err := cmd.CombinedOutput(); err != nil {
		g.t.Fatalf("git clone as %s: %s\n%s", who, err, out)
	}
	return c
}

// commit commits the working copy as it is, and answers the commit.
func (c *clone) commit(message string) string {
	c.t.Helper()
	c.must("add", "-A")
	c.must("commit", "-q", "-m", message)
	return strings.TrimSpace(c.must("rev-parse", "HEAD"))
}

// version is the version recorded for a commit, or the error reading it.
func (g *gitServer) version(commit string) (db.Version, error) {
	var v db.Version
	err := g.pool.In(g.t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		var err error
		v, err = ns.Version(ctx, "monthly-invoicing", commit)
		return err
	})
	return v, err
}

// refs are the repository's refs, by name, each the object it names.
func (g *gitServer) refs() map[string]string {
	g.t.Helper()
	out := map[string]string{}
	if err := g.pool.In(g.t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		r, err := ns.Repository(ctx, "monthly-invoicing")
		for _, ref := range r.Refs {
			out[ref.Name] = ref.Target()
		}
		return err
	}); err != nil {
		g.t.Fatal(err)
	}
	return out
}

// everyone holds everything alice needs to push, bob to read, and owner to do what only an owner
// does, on finance.
func everyone() api.Authorizer {
	finance := api.Target{Namespace: "finance"}
	return granted{
		"alice": {{api.WorkflowRead, finance}, {api.WorkflowWrite, finance}},
		"bob":   {{api.WorkflowRead, finance}},
		"owner": {{api.WorkflowRead, finance}, {api.WorkflowWrite, finance}, {api.GrantManage, finance}, {api.SecretUse, finance}},
	}
}

// The whole path a person takes: a first commit pushed to an empty repository becomes the default
// branch and a version, a clone reads it back as git wrote it, and a second commit pushed on top of
// it, as the thin pack git pushes by default, is fetched into the clone.
func TestAWorkflowIsPushedClonedAndFetchedWithGit(t *testing.T) {
	g := servingGit(t, everyone())
	empty := g.cloned("alice")
	if out := empty.must("log", "--all", "--oneline"); strings.TrimSpace(out) != "" {
		t.Errorf("a clone of an empty repository holds %q", out)
	}

	work := g.newClone("alice")
	work.write("agentiik.yaml", workflowDocument)
	work.write("scripts/normalize.py", "print('normalize')\n")
	first := work.commit("first")
	work.must("push", "-q", "origin", "main")

	if got := g.refs()["refs/heads/main"]; got != first {
		t.Fatalf("main names %s after the push of %s", got, first)
	}
	v, err := g.version(first)
	if err != nil {
		t.Fatalf("the pushed commit is no version: %s", err)
	}
	if v.Source != db.SourceGit || v.Author != "alice" || v.Parent != "" || len(v.Tree) != 2 {
		t.Errorf("the version is %+v", v)
	}

	read := g.cloned("bob")
	if head := strings.TrimSpace(read.must("rev-parse", "HEAD")); head != first {
		t.Errorf("a clone reads HEAD as %s, where main is %s", head, first)
	}
	if got, err := os.ReadFile(filepath.Join(read.dir, "scripts/normalize.py")); err != nil || string(got) != "print('normalize')\n" {
		t.Errorf("a clone reads the script as %q: %v", got, err)
	}
	read.must("fsck", "--strict", "--full")

	work.write("scripts/normalize.py", "print('normalize, twice')\n")
	second := work.commit("second")
	work.must("push", "-q", "origin", "main")
	if v, err := g.version(second); err != nil || v.Parent != first {
		t.Errorf("the second commit is recorded as %+v: %v", v, err)
	}
	read.must("fetch", "-q", "origin")
	if got := strings.TrimSpace(read.must("rev-parse", "origin/main")); got != second {
		t.Errorf("a fetch reads origin/main as %s, where main is %s", got, second)
	}
	read.must("fsck", "--strict", "--full")

	// And with the protocol version 0 asked for, as a git older than 2.26 speaks by default.
	old := g.cloned("bob", "-c", "protocol.version=0")
	if head := strings.TrimSpace(old.must("rev-parse", "HEAD")); head != second {
		t.Errorf("a clone asking for version 0 reads HEAD as %s", head)
	}
}

// "A pre-receive hook validates agentiik.yaml, so an invalid workflow never reaches the branch", and
// the pusher reads why on git's own error stream: the file, the location and the rule.
func TestAnInvalidWorkflowIsRefusedAtThePushNamingTheFileTheLocationAndTheRule(t *testing.T) {
	g := servingGit(t, everyone())
	work := g.newClone("alice")
	work.write("agentiik.yaml", strings.ReplaceAll(workflowDocument, image, taggedImage))
	refused := work.commit("an image nobody pinned")
	out, err := work.run("push", "origin", "main")
	if err == nil {
		t.Fatalf("a workflow naming an image nobody pinned was pushed:\n%s", out)
	}
	for _, said := range []string{"remote:", "image-not-pinned", "agentiik.yaml:"} {
		if !strings.Contains(out, said) {
			t.Errorf("git says %q, and does not say %s", out, said)
		}
	}
	if got := g.refs()["refs/heads/main"]; got != "" {
		t.Errorf("main names %s after a refused push", got)
	}
	if _, err := g.version(refused); err == nil {
		t.Error("a refused commit is a version")
	}
	var found bool
	for _, e := range audited(t, g.pool) {
		if e.Action == audit.PushRefuse {
			d := detailOf(t, e)
			found = e.Actor == "alice" && e.Target == "monthly-invoicing" && d["rule"] == "image-not-pinned" && d["commit"] == refused
		}
	}
	if !found {
		t.Error("the refused push is not recorded, with its rule and its commit")
	}

	// Pinned, the same commit goes through, and is recorded as a version of the digest.
	if err := g.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		_, err := ns.RecordImages(ctx, "monthly-invoicing", "alice", time.Time{}, db.Images{Pins: map[string]string{taggedImage: image}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	work.must("push", "-q", "origin", "main")
	if v, err := g.version(refused); err != nil || v.Images[taggedImage] != image {
		t.Errorf("the commit pushed once its tag was pinned is %+v: %v", v, err)
	}
}

// "Authenticate a git client with the command line's API token, and refuse any other credential":
// no credential is a 401 that makes git ask its credential helper, and a console session's cookie is
// no credential here.
func TestGitIsAnsweredOnlyWithTheCommandLinesToken(t *testing.T) {
	g := servingGit(t, everyone())
	info := g.url + "/finance/monthly-invoicing.git/info/refs?service=git-upload-pack"
	for name, set := range map[string]func(*http.Request){
		"nothing":          func(*http.Request) {},
		"a session cookie": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "__Host-agentiik_session", Value: "alice"}) },
	} {
		r, _ := http.NewRequestWithContext(t.Context(), "GET", info, nil)
		set(r)
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(res.Header.Get("WWW-Authenticate"), "Basic") {
			t.Errorf("%s is answered %d, %q", name, res.StatusCode, res.Header.Get("WWW-Authenticate"))
		}
	}
	for name, set := range map[string]func(*http.Request){
		"the token as Basic's password": func(r *http.Request) { r.SetBasicAuth("anybody", "bob") },
		"the token as Bearer":           func(r *http.Request) { r.Header.Set("Authorization", "Bearer bob") },
	} {
		r, _ := http.NewRequestWithContext(t.Context(), "GET", info, nil)
		set(r)
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/x-git-upload-pack-advertisement" {
			t.Errorf("%s is answered %d, %s", name, res.StatusCode, res.Header.Get("Content-Type"))
		}
	}
}

// "Clone and fetch need workflow:read, push needs workflow:write": a reader clones and is told they
// may not push, and a caller holding nothing is answered as a repository that is not there is.
func TestAReaderClonesAndIsRefusedAPushAndAStrangerFindsNothing(t *testing.T) {
	g := servingGit(t, everyone())
	work := g.newClone("alice")
	work.write("agentiik.yaml", workflowDocument)
	work.commit("first")
	work.must("push", "-q", "origin", "main")

	read := g.cloned("bob")
	read.write("README.md", "read me\n")
	read.commit("a reader's commit")
	out, err := read.run("push", "origin", "main")
	if err == nil || !strings.Contains(out, "403") {
		t.Errorf("a reader's push is answered:\n%s", out)
	}

	stranger := g.newClone("carol")
	out, err = stranger.run("fetch", "origin")
	if err == nil || !strings.Contains(out, "not found") {
		t.Errorf("a stranger's fetch is answered:\n%s", out)
	}
}

// "Push to the protected default branch, force-push, delete a ref: grant:manage", and "the default
// branch is never deleted".
func TestAForcedPushADeletionAndTheProtectedBranchTakeGrantManage(t *testing.T) {
	g := servingGit(t, everyone())
	work := g.newClone("alice")
	work.write("agentiik.yaml", workflowDocument)
	first := work.commit("first")
	work.must("push", "-q", "origin", "main")
	work.write("scripts/a.sh", "true\n")
	work.commit("second")
	work.must("push", "-q", "origin", "main", "main:feature")

	// A rewrite of main, pushed with --force: refused to an editor, taken from an owner.
	work.must("reset", "-q", "--hard", first)
	work.write("scripts/b.sh", "true\n")
	rewritten := work.commit("rewritten")
	if out, err := work.run("push", "--force", "origin", "main"); err == nil || !strings.Contains(out, "grant:manage") {
		t.Errorf("an editor's forced push is answered:\n%s", out)
	}
	owner := g.newClone("owner")
	owner.must("fetch", "-q", "origin")
	owner.must("fetch", "-q", work.dir, "main")
	if out, err := owner.run("push", "--force", "origin", "FETCH_HEAD:refs/heads/main"); err != nil {
		t.Errorf("an owner's forced push is answered:\n%s", out)
	}
	if got := g.refs()["refs/heads/main"]; got != rewritten {
		t.Errorf("main names %s after an owner forced it to %s", got, rewritten)
	}

	// A branch deleted: refused to an editor, taken from an owner; the default branch never.
	if out, err := work.run("push", "origin", ":feature"); err == nil || !strings.Contains(out, "grant:manage") {
		t.Errorf("an editor's deletion is answered:\n%s", out)
	}
	if out, err := owner.run("push", "origin", ":feature"); err != nil {
		t.Errorf("an owner's deletion is answered:\n%s", out)
	}
	if out, err := owner.run("push", "origin", ":main"); err == nil || !strings.Contains(out, "default branch") {
		t.Errorf("deleting the default branch is answered:\n%s", out)
	}

	// Protected, the default branch takes grant:manage for any push; another branch does not.
	if _, err := dbtest.Superuser(t, g.super).Exec(t.Context(),
		`update workflow_refs set protected = true where namespace = 'finance' and workflow = 'monthly-invoicing' and ref = 'refs/heads/main'`); err != nil {
		t.Fatal(err)
	}
	editor := g.cloned("alice")
	editor.write("scripts/c.sh", "true\n")
	editor.commit("third")
	if out, err := editor.run("push", "origin", "main"); err == nil || !strings.Contains(out, "protected") {
		t.Errorf("an editor's push to the protected branch is answered:\n%s", out)
	}
	if out, err := editor.run("push", "origin", "main:another"); err != nil {
		t.Errorf("an editor's push to another branch is answered:\n%s", out)
	}
}

// A tag is pushed and fetched, an annotated one peeled in the advertisement; moving one takes
// grant:manage, since others fetched the commit it named.
func TestTagsArePushedFetchedAndNotMovedByAnEditor(t *testing.T) {
	g := servingGit(t, everyone())
	work := g.newClone("alice")
	work.write("agentiik.yaml", workflowDocument)
	first := work.commit("first")
	work.must("tag", "-a", "-m", "the first release", "v1.0.0")
	work.must("tag", "lightweight")
	work.must("push", "-q", "origin", "main", "--tags")

	read := g.cloned("bob")
	if got := strings.TrimSpace(read.must("rev-parse", "v1.0.0^{commit}")); got != first {
		t.Errorf("the annotated tag peels to %s in a clone", got)
	}
	if got := strings.TrimSpace(read.must("cat-file", "-t", "v1.0.0")); got != "tag" {
		t.Errorf("the annotated tag is a %s in a clone", got)
	}

	work.write("scripts/a.sh", "true\n")
	work.commit("second")
	work.must("push", "-q", "origin", "main")
	work.must("tag", "-f", "lightweight")
	if out, err := work.run("push", "--force", "origin", "lightweight"); err == nil || !strings.Contains(out, "grant:manage") {
		t.Errorf("an editor moving a tag is answered:\n%s", out)
	}
}

// "secret:use is checked when a version is pushed, against whoever pushes it": the hook asks the
// pusher's.
func TestACommitNamingASecretIsPushedOnlyBySomeoneHoldingSecretUse(t *testing.T) {
	g := servingGit(t, everyone())
	if _, err := dbtest.Superuser(t, g.super).Exec(t.Context(),
		`insert into secret_declarations (namespace, name, provider, declared_by) values ('finance', 'billing', 'builtin', 'owner')`); err != nil {
		t.Fatal(err)
	}
	work := g.newClone("alice")
	work.write("agentiik.yaml", namingASecret)
	work.commit("naming billing")
	if out, err := work.run("push", "origin", "main"); err == nil || !strings.Contains(out, "secret:use") {
		t.Errorf("an editor without secret:use pushing a secret's name is answered:\n%s", out)
	}
	owner := g.newClone("owner")
	owner.must("fetch", "-q", work.dir, "main")
	if out, err := owner.run("push", "origin", "FETCH_HEAD:refs/heads/main"); err != nil {
		t.Errorf("an owner pushing a secret's name is answered:\n%s", out)
	}
}
