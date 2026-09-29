package api_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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
	h     http.Handler
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
	return &gitServer{t: t, h: rt, url: server.URL, pool: pool, super: super}
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

	// A script large enough that git sends its next version as a delta against this one, which
	// the repository holds and the push does not carry: git leaves out a delta for an object of
	// a few dozen bytes.
	script := strings.Repeat("# a line of the script that the next commit leaves alone\n", 100)
	work := g.newClone("alice")
	work.write("agentiik.yaml", workflowDocument)
	work.write("scripts/normalize.py", script+"print('normalize')\n")
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
	if got, err := os.ReadFile(filepath.Join(read.dir, "scripts/normalize.py")); err != nil || string(got) != script+"print('normalize')\n" {
		t.Errorf("a clone reads the script as %q: %v", got, err)
	}
	read.must("fsck", "--strict", "--full")

	work.write("scripts/normalize.py", script+"print('normalize, twice')\n")
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
	for _, said := range []string{"remote:", "image-not-pinned"} {
		if !strings.Contains(out, said) {
			t.Errorf("git says %q, and does not say %s", out, said)
		}
	}
	if !regexp.MustCompile(`image-not-pinned at agentiik\.yaml:\d+:\d+`).MatchString(out) {
		t.Errorf("git says %q, and does not name the file, the line and the column", out)
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
	moves := map[string]map[string]any{}
	for _, e := range audited(t, g.pool) {
		if e.Action == audit.RefUpdate && e.Target == "monthly-invoicing" {
			d := detailOf(t, e)
			moves[e.Actor+" "+d["ref"].(string)+" "+d["new"].(string)] = d
		}
	}
	if d := moves["owner refs/heads/main "+rewritten]; d == nil || d["forced"] != true {
		t.Errorf("the owner's forced push is recorded as %v", d)
	}
	if d := moves["alice refs/heads/main "+first]; d == nil || d["forced"] != false || d["old"] != "" {
		t.Errorf("the first push of main is recorded as %v", d)
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

	// A version already is not judged again: what secret:use answers for is writing a secret's
	// name, which the owner did, so an editor may leave another branch at it.
	if out, err := work.run("push", "origin", "main:elsewhere"); err != nil {
		t.Errorf("an editor leaving a branch at a version naming a secret is answered:\n%s", out)
	}
}

// "One row per commit a push left a branch or a tag pointing at, once its hook accepted it, and per
// tree agk push sent before the repository's first git push": once git hosts the repository, a tree
// pushed as a new version is refused, since no ref would reach it and no clone would hold it, and a
// commit that is a version already is answered as it always was.
func TestATreeIsNoNewVersionOnceGitHostsTheRepository(t *testing.T) {
	g := servingGit(t, everyone())
	const versions = "/api/v1/finance/workflows/monthly-invoicing/versions/"
	if w, _ := call(t, g.h, "PUT", versions+aCommit, "alice", aPush(t)); w.Code != http.StatusOK {
		t.Fatalf("a tree pushed before any git push answered %d: %s", w.Code, w.Body)
	}

	work := g.newClone("alice")
	work.write("agentiik.yaml", workflowDocument)
	work.commit("first")
	work.must("push", "-q", "origin", "main")

	other := strings.Repeat("b", 40)
	w, _ := call(t, g.h, "PUT", versions+other, "alice", aPush(t))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "git hosts finance/monthly-invoicing") {
		t.Errorf("a tree pushed as a new version once git hosts the repository answered %d: %s", w.Code, w.Body)
	}
	if _, err := g.version(other); err == nil {
		t.Error("the refused tree is a version")
	}
	if w, _ := call(t, g.h, "PUT", versions+aCommit, "alice", aPush(t)); w.Code != http.StatusOK {
		t.Errorf("the tree of a version pushed again answered %d: %s", w.Code, w.Body)
	}
}

// A version is held to the files a pushed tree may hold, whichever push makes it, for as long as
// every task of it is sent the URL of every file: TreeMaxFiles go through, and one more is refused,
// naming the bound, with no ref moved.
func TestAVersionOfMoreFilesThanATreeHoldsIsRefusedAtThePush(t *testing.T) {
	g := servingGit(t, everyone())
	work := g.newClone("alice")
	work.write("agentiik.yaml", workflowDocument)
	for i := range api.TreeMaxFiles - 1 {
		work.write(fmt.Sprintf("data/%04d", i), "the same bytes, one object\n")
	}
	full := work.commit("as many files as a tree holds")
	work.must("push", "-q", "origin", "main")
	if v, err := g.version(full); err != nil || len(v.Tree) != api.TreeMaxFiles {
		t.Fatalf("a commit of %d files is recorded with %d: %v", api.TreeMaxFiles, len(v.Tree), err)
	}

	work.write("data/one-more", "the same bytes, one object\n")
	over := work.commit("one file more")
	out, err := work.run("push", "origin", "main")
	if err == nil || !strings.Contains(out, fmt.Sprintf("more than %d files", api.TreeMaxFiles)) {
		t.Errorf("a commit of one file more than a tree holds was answered: %v\n%s", err, out)
	}
	if got := g.refs()["refs/heads/main"]; got != full {
		t.Errorf("main names %s after a refused push", got)
	}
	if _, err := g.version(over); err == nil {
		t.Error("the refused commit is a version")
	}
}

// pkt is a line as a packet of git's protocol.
func pkt(line string) string { return fmt.Sprintf("%04x%s", len(line)+4, line) }

// post sends a request to one of the repository's git routes as who, as git would send it, and
// answers the status and the body.
func (g *gitServer) post(who, service, body string) (int, string) {
	g.t.Helper()
	r, err := http.NewRequestWithContext(g.t.Context(), "POST", g.url+"/finance/monthly-invoicing.git/"+service, strings.NewReader(body))
	if err != nil {
		g.t.Fatal(err)
	}
	r.SetBasicAuth("agk", who)
	r.Header.Set("Content-Type", "application/x-"+service+"-request")
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		g.t.Fatal(err)
	}
	defer res.Body.Close()
	answer, err := io.ReadAll(res.Body)
	if err != nil {
		g.t.Fatal(err)
	}
	return res.StatusCode, string(answer)
}

// A fetch of a history longer than one round of haves: git sends sixteen haves a request, and
// negotiates on for as long as the server acknowledges each common one, which only
// multi_ack_detailed does over HTTP. Without it, the pull after a clone of twenty-one commits was
// answered with the whole history where git read the pack, and failed.
func TestAFetchNegotiatesPastOneRoundOfHaves(t *testing.T) {
	g := servingGit(t, everyone())
	work := g.newClone("alice")
	work.write("agentiik.yaml", workflowDocument)
	for i := range 21 {
		work.write("scripts/count.sh", fmt.Sprintf("echo %d\n", i))
		work.commit(fmt.Sprintf("commit %d", i))
	}
	work.must("push", "-q", "origin", "main")

	read := g.cloned("bob")
	work.write("scripts/count.sh", "echo once more\n")
	last := work.commit("one more")
	work.must("push", "-q", "origin", "main")
	read.must("pull", "-q", "--ff-only", "origin", "main")
	if got := strings.TrimSpace(read.must("rev-parse", "HEAD")); got != last {
		t.Errorf("a pull reads HEAD as %s, where main is %s", got, last)
	}
	read.must("fsck", "--strict", "--full")
}

// "Clone and fetch need workflow:read" and nothing a ref does not reach is sent: a want no longer a
// tip, because a push moved its ref between the advertisement and the fetch, is served as git's own
// upload-pack serves it over HTTP, since the ref still reaches it, and one no ref reaches is not.
func TestAWantBehindARefIsServedAndOneNoRefReachesIsNot(t *testing.T) {
	g := servingGit(t, everyone())
	work := g.newClone("alice")
	work.write("agentiik.yaml", workflowDocument)
	first := work.commit("first")
	work.must("push", "-q", "origin", "main")
	work.write("scripts/a.sh", "true\n")
	work.commit("second")
	work.must("push", "-q", "origin", "main")

	status, answer := g.post("bob", "git-upload-pack", pkt("want "+first+" side-band-64k multi_ack_detailed\n")+"0000"+pkt("done\n"))
	if status != http.StatusOK || strings.Contains(answer, "ERR") || !strings.Contains(answer, "PACK") {
		t.Errorf("a want the ref moved past is answered %d: %.200q", status, answer)
	}
	elsewhere := strings.Repeat("1", 40)
	_, answer = g.post("bob", "git-upload-pack", pkt("want "+elsewhere+" side-band-64k\n")+"0000"+pkt("done\n"))
	if !strings.Contains(answer, "ERR upload-pack: not our ref "+elsewhere) {
		t.Errorf("a want no ref reaches is answered %.200q", answer)
	}
}

// A POST is of the type git sends, as git's own http-backend holds it to: neither is a type a page
// of another site may send a browser holding a token without asking first.
func TestAGitRequestOfAnotherTypeIsRefused(t *testing.T) {
	g := servingGit(t, everyone())
	for _, service := range []string{"git-upload-pack", "git-receive-pack"} {
		r, _ := http.NewRequestWithContext(t.Context(), "POST", g.url+"/finance/monthly-invoicing.git/"+service, strings.NewReader("0000"))
		r.SetBasicAuth("agk", "alice")
		r.Header.Set("Content-Type", "text/plain")
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnsupportedMediaType {
			t.Errorf("a POST to %s as text/plain is answered %d", service, res.StatusCode)
		}
	}
}

// Every object a push stores names only objects the repository holds, reached by a new tip or not:
// a pack slipping in a commit whose parent nobody sent is refused whole, where it would have let a
// later push leave a ref at it, its history missing and every clone failing.
func TestAPackHoldingAnObjectWhoseHistoryIsMissingIsRefused(t *testing.T) {
	g := servingGit(t, everyone())
	work := g.newClone("alice")
	work.write("agentiik.yaml", workflowDocument)
	tip := work.commit("the tip")
	tree := strings.TrimSpace(work.must("rev-parse", tip+"^{tree}"))
	parent := strings.TrimSpace(work.must("commit-tree", tree, "-m", "a parent nobody sends"))
	orphan := strings.TrimSpace(work.must("commit-tree", tree, "-p", parent, "-m", "a commit nothing reaches"))

	objects := work.must("rev-list", "--objects", tip)
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(objects), "\n") {
		id, _, _ := strings.Cut(line, " ")
		ids = append(ids, id)
	}
	ids = append(ids, orphan)
	cmd := exec.Command("git", "pack-objects", "--stdout")
	cmd.Dir = work.dir
	cmd.Env = gitEnv(filepath.Dir(work.dir))
	cmd.Stdin = strings.NewReader(strings.Join(ids, "\n") + "\n")
	pack, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	zero := strings.Repeat("0", 40)
	_, answer := g.post("alice", "git-receive-pack", pkt(zero+" "+tip+" refs/heads/main\x00report-status\n")+"0000"+string(pack))
	if !strings.Contains(answer, "missing necessary objects: "+parent) || !strings.Contains(answer, "ng refs/heads/main") {
		t.Errorf("a pack holding a commit whose parent nobody sent is answered %.300q", answer)
	}
	if got := g.refs()["refs/heads/main"]; got != "" {
		t.Errorf("main names %s after the refused push", got)
	}
}

// A version is a commit, and a commit names exactly one tree: a commit a tree push recorded with
// other files than it holds is refused at a git push, which would otherwise leave a branch at files
// nobody judged, and one recorded with the files it holds is taken as the version it is.
func TestACommitRecordedByATreePushWithOtherFilesIsRefused(t *testing.T) {
	g := servingGit(t, everyone())
	const versions = "/api/v1/finance/workflows/monthly-invoicing/versions/"
	work := g.newClone("alice")
	work.write("agentiik.yaml", workflowDocument)
	work.write("scripts/benign.sh", "true\n")
	other := work.commit("files the tree push did not carry")
	if w, _ := call(t, g.h, "PUT", versions+other, "alice", aPush(t)); w.Code != http.StatusOK {
		t.Fatalf("the tree push answered %d: %s", w.Code, w.Body)
	}
	out, err := work.run("push", "origin", "main")
	if err == nil || !strings.Contains(out, "recorded with other files") {
		t.Errorf("a commit recorded with other files is pushed:\n%s", out)
	}
	if got := g.refs()["refs/heads/main"]; got != "" {
		t.Errorf("main names %s after the refused push", got)
	}

	same := g.newClone("alice")
	same.write("agentiik.yaml", workflowDocument)
	commit := same.commit("the files the tree push carried")
	if w, _ := call(t, g.h, "PUT", versions+commit, "alice", aPush(t)); w.Code != http.StatusOK {
		t.Fatalf("the tree push answered %d: %s", w.Code, w.Body)
	}
	same.must("push", "-q", "origin", "main")
	if v, err := g.version(commit); err != nil || v.Source != db.SourceTree {
		t.Errorf("the version is %+v after the git push of its commit: %v", v, err)
	}
}

// Two pushes racing to create one branch: one lands whole, and the other is refused whole, its
// commit no version, whichever of the two checks catches it, git's own or the installation's.
func TestTwoPushesRacingForOneBranchLandOne(t *testing.T) {
	g := servingGit(t, everyone())
	var works [2]*clone
	var commits [2]string
	for i := range works {
		works[i] = g.newClone("alice")
		works[i].write("agentiik.yaml", workflowDocument)
		works[i].write("scripts/racer.sh", fmt.Sprintf("echo %d\n", i))
		commits[i] = works[i].commit(fmt.Sprintf("racer %d", i))
	}
	var errs [2]error
	var outs [2]string
	var wg sync.WaitGroup
	for i := range works {
		wg.Go(func() { outs[i], errs[i] = works[i].run("push", "origin", "main") })
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("two pushes racing for main answered %v and %v:\n%s\n%s", errs[0], errs[1], outs[0], outs[1])
	}
	won, lost := 0, 1
	if errs[0] != nil {
		won, lost = 1, 0
	}
	if got := g.refs()["refs/heads/main"]; got != commits[won] {
		t.Errorf("main names %s, where the push that landed moved it to %s", got, commits[won])
	}
	if _, err := g.version(commits[lost]); err == nil {
		t.Errorf("the commit of the push that lost is a version:\n%s", outs[lost])
	}
}
