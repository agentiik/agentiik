package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/internal/dockertest"
	versions "github.com/agentiik/agentiik/version"
)

// agk push, which is where "a version is a commit" stops being a sentence and starts refusing
// things.

const scriptWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
outputs:
  invoices: { from: { step: normalize, port: ok } }
steps:
  normalize:
    image: docker.io/library/alpine@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    script: ["echo hello > /agk/out/ports/ok"]
    outputs: [ok]
`

// repository makes a git repository with a workflow in it, because the one rule this command
// enforces is about what git says.
func repository(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	dir := t.TempDir()
	// A script step, so that nothing here reaches a Docker daemon: "A script step is not
	// among them", says graph.Images of the manifests a workflow needs read.
	write(t, dir, "agentiik.yaml", scriptWorkflow)
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"add", "-A"},
		{"commit", "-qm", "the workflow"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	return dir
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gitIn runs git in the repository a test made, and answers what it said.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
	return strings.TrimSpace(string(out))
}

// commitAll commits whatever the working tree holds.
func commitAll(t *testing.T, dir, message string) {
	t.Helper()
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", message)
}

// pushing runs the command against a stand-in for an installation, and answers what the push left
// there, nil where nothing arrived.
func pushing(t *testing.T, dir string, answer int, args ...string) (int, string, string, *api.Push) {
	t.Helper()
	code, out, errs, got, _ := pushingTo(t, dir, answer, args...)
	return code, out, errs, got
}

// pushingTo is pushing, and the commit the ref the push moved was left at.
func pushingTo(t *testing.T, dir string, answer int, args ...string) (int, string, string, *api.Push, string) {
	t.Helper()
	in := aPushStandIn(t, answer)
	code, out, errs := pushAgainst(dir, in.server.URL, args...)
	got, commit := in.landed()
	return code, out, errs, got, commit
}

// pushStandIn is an installation as far as agk push reaches one: the routes that create a repository
// and record its images, and git's smart HTTP, served by git http-backend over bare repositories,
// so that what a push leaves is read back with git itself rather than with anything this command
// computed. answer 404 answers every route as an installation holding nothing of the caller's does,
// and 422 refuses every push with a hook, as the installation's refuses an invalid workflow.
type pushStandIn struct {
	t      *testing.T
	root   string
	answer int
	server *httptest.Server

	mu      sync.Mutex
	created []api.WorkflowCreate
	images  []api.RecordImages
}

func aPushStandIn(t *testing.T, answer int) *pushStandIn {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on this machine")
	}
	in := &pushStandIn{t: t, root: t.TempDir(), answer: answer}
	backend := &cgi.Handler{
		Path: git, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + in.root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=alice", "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1"},
	}
	in.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer the-token" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"no"}`))
			return
		}
		if in.answer == http.StatusNotFound {
			http.Error(w, "no such repository, or not yours", http.StatusNotFound)
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/finance/workflows":
			var c api.WorkflowCreate
			json.NewDecoder(r.Body).Decode(&c)
			in.mu.Lock()
			in.created = append(in.created, c)
			in.mu.Unlock()
			bare := in.bare(c.Name)
			if _, err := os.Stat(bare); err == nil {
				w.WriteHeader(http.StatusConflict)
				w.Write([]byte(`{"error":"held"}`))
				return
			}
			in.git("", "init", "-q", "--bare", "-b", c.DefaultBranch, bare)
			if in.answer == http.StatusUnprocessableEntity {
				hook := filepath.Join(bare, "hooks", "pre-receive")
				os.WriteFile(hook, []byte("#!/bin/sh\necho 'refused: image-not-pinned at agentiik.yaml:9:12'\nexit 1\n"), 0o755)
			}
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{}`))
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/v1/finance/workflows/") && strings.HasSuffix(r.URL.Path, "/images"):
			var recording api.RecordImages
			json.NewDecoder(r.Body).Decode(&recording)
			in.mu.Lock()
			in.images = append(in.images, recording)
			in.mu.Unlock()
			w.Write([]byte(`{}`))
		case strings.HasPrefix(r.URL.Path, "/finance/") && strings.Contains(r.URL.Path, ".git/"):
			name, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/finance/"), ".git/")
			if _, err := os.Stat(in.bare(name)); err != nil {
				http.Error(w, "no such repository, or not yours", http.StatusNotFound)
				return
			}
			// Go's CGI takes no chunked body, which a push streaming its pack sends, and the
			// installation does: read whole here, as the stand-in's shortcoming.
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.Body, r.ContentLength, r.TransferEncoding = io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
			backend.ServeHTTP(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"no such thing, or not yours"}`))
		}
	}))
	t.Cleanup(in.server.Close)
	return in
}

// standInEnv is the environment the stand-in's own git runs in: untraced, so that a test tracing
// what agk push asks of git reads that alone.
func standInEnv() []string {
	return append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TRACE=0")
}

// bare is where the stand-in keeps a workflow's repository.
func (in *pushStandIn) bare(name string) string {
	return filepath.Join(in.root, "finance", name+".git")
}

// git runs git in dir, failing the test where it fails.
func (in *pushStandIn) git(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = standInEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		in.t.Fatalf("git %v: %s", args, out)
	}
	return strings.TrimSpace(string(out))
}

// landed is what a push left in the one repository the stand-in holds, as git reads it, and the
// commit its ref was left at: nil and nothing where no ref moved.
func (in *pushStandIn) landed() (*api.Push, string) {
	entries, _ := os.ReadDir(filepath.Join(in.root, "finance"))
	if len(entries) != 1 {
		return nil, ""
	}
	bare := filepath.Join(in.root, "finance", entries[0].Name())
	refs := in.git(bare, "for-each-ref", "--format=%(refname)")
	if refs == "" {
		return nil, ""
	}
	ref, _, _ := strings.Cut(refs, "\n")
	commit := in.git(bare, "rev-parse", ref+"^{commit}")
	got := &api.Push{Entry: entryPoint, Tree: map[string]api.PushFile{}, Manifests: map[string][]byte{}}
	for _, line := range strings.Split(in.git(bare, "ls-tree", "-r", "-z", commit), "\x00") {
		meta, path, found := strings.Cut(line, "\t")
		if !found {
			continue
		}
		fields := strings.Fields(meta)
		mode := "0644"
		if fields[0] == "100755" {
			mode = "0755"
		}
		// Read raw rather than through git, which trims what it answers.
		read := exec.Command("git", "-C", bare, "cat-file", "blob", fields[2])
		read.Env = standInEnv()
		content, err := read.Output()
		if err != nil {
			in.t.Fatalf("%s of %s could not be read: %v", path, commit, err)
		}
		got.Tree[path] = api.PushFile{Content: content, Mode: mode}
	}
	got.Document = got.Tree[entryPoint].Content
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, recording := range in.images {
		if got.Images == nil {
			got.Images = map[string]string{}
		}
		maps.Copy(got.Images, recording.Pins)
		for image, text := range recording.Manifests {
			got.Manifests[image] = []byte(text)
		}
	}
	return got, commit
}

// pushAgainst runs the command against the installation at url, and answers what it said.
func pushAgainst(dir, url string, args ...string) (int, string, string) {
	out, errs := &strings.Builder{}, &strings.Builder{}
	e := Env{
		Out: out, Err: errs, Dir: dir,
		Getenv: func(k string) string {
			switch k {
			case tokenVariable:
				return "the-token"
			case serverVariable:
				return url
			}
			return ""
		},
	}
	code := push(context.Background(), e, append([]string{"--namespace", "finance"}, args...))
	return code, out.String(), errs.String()
}

// The ordinary path: a clean tree, and what arrives is what the version is.
func TestPushSendsWhatTheVersionIs(t *testing.T) {
	dir := repository(t)
	code, out, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitSucceeded {
		t.Fatalf("push answered %d: %s%s", code, out, errs)
	}
	if got == nil {
		t.Fatal("nothing arrived at the server")
	}
	if got.Entry != "agentiik.yaml" || len(got.Document) == 0 {
		t.Errorf("what arrived reads entry %q, %d bytes", got.Entry, len(got.Document))
	}
	if !strings.Contains(string(got.Document), "monthly-invoicing") {
		t.Error("the document that arrived is not the workflow")
	}
	if !strings.Contains(out, "pushed to") {
		t.Errorf("it said %q", out)
	}
}

// "A version is a commit", so an uncommitted edit is never pushed, and a working copy holding one
// is refused all the same: somebody pushing it most likely believes the edit goes with the push.
func TestAModifiedTreeIsRefused(t *testing.T) {
	dir := repository(t)
	write(t, dir, "agentiik.yaml", scriptWorkflow+`
  sneaky:
    image: docker.io/library/alpine@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    script: ["true"]
    outputs: [ok]
`)

	code, _, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitRefused {
		t.Fatalf("a push from a modified tree answered %d", code)
	}
	if got != nil {
		t.Error("it reached the server anyway")
	}
	if !strings.Contains(errs, "uncommitted changes") {
		t.Errorf("the refusal reads %q", errs)
	}
	if !strings.Contains(errs, "agentiik.yaml") {
		t.Errorf("the refusal does not name what differs: %q", errs)
	}
}

// --allow-dirty says the edits are meant to stay behind, and they do: what arrives is the commit as
// it was committed, in the graph and in the tree alike, and a file nobody committed is in neither.
// Nor is the workflow the command reads, whose name the version is sent under and whose steps it
// counts: the server takes the name in the path as it comes, so a workflow read off the disk would
// register the committed document under a name nobody committed.
func TestAllowDirtyPushesTheCommitAndNotTheEdits(t *testing.T) {
	dir := repository(t)
	head := gitIn(t, dir, "rev-parse", "HEAD")
	write(t, dir, "agentiik.yaml", strings.Replace(scriptWorkflow, "name: monthly-invoicing", "name: renamed-invoicing", 1)+`
  sneaky:
    image: docker.io/library/alpine@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    script: ["true"]
    outputs: [ok]
`)
	write(t, dir, "notes/draft.txt", "never committed")

	code, out, errs, got, pushed := pushingTo(t, dir, http.StatusOK, "--allow-dirty")
	if code != exitSucceeded {
		t.Fatalf("--allow-dirty answered %d: %s%s", code, out, errs)
	}
	if pushed != head || !strings.Contains(out, "finance/monthly-invoicing@") {
		t.Errorf("%s was pushed, where the commit is %s: %s", pushed, head, out)
	}
	if !strings.Contains(out, "1 step,") {
		t.Errorf("it counted the steps of a workflow nobody committed: %q", out)
	}
	if strings.Contains(string(got.Document), "sneaky") {
		t.Error("the uncommitted edit arrived as the entry point the graph is rebuilt from")
	}
	if f := got.Tree["agentiik.yaml"]; string(f.Content) != scriptWorkflow {
		t.Errorf("the entry point arrived in the tree as\n%s", f.Content)
	}
	if _, held := got.Tree["notes/draft.txt"]; held {
		t.Errorf("a file nobody committed travelled: %v", keysOf(got.Tree))
	}
}

// The credential is never an argument, because an argument is in the shell history, in the
// process list and in whatever recorded the terminal.
func TestTheCredentialIsNotAFlag(t *testing.T) {
	fs := flags(Env{Out: &strings.Builder{}, Err: &strings.Builder{}}, "agk push", "")
	_ = fs
	dir := repository(t)

	out, errs := &strings.Builder{}, &strings.Builder{}
	e := Env{Out: out, Err: errs, Dir: dir, Getenv: func(string) string { return "" }}
	if code := push(context.Background(), e, []string{"--namespace", "finance", "--server", "https://example.com"}); code != exitUsage {
		t.Errorf("a push with no credential answered %d", code)
	}
	if !strings.Contains(errs.String(), tokenVariable) {
		t.Errorf("it did not say where the credential comes from: %q", errs)
	}
	if strings.Contains(errs.String(), "--token") {
		t.Error("it offered a flag for the credential")
	}
}

// What the installation says is what the person reads, and the two answers that mean something
// specific are said specifically.
func TestWhatTheInstallationSaysIsPassedOn(t *testing.T) {
	dir := repository(t)
	for _, c := range []struct {
		answer int
		reads  string
	}{
		{http.StatusUnprocessableEntity, "remote: refused: image-not-pinned at agentiik.yaml:9:12"},
		{http.StatusNotFound, "no such namespace or workflow"},
	} {
		code, _, errs, _ := pushing(t, dir, c.answer)
		if code != exitRefused {
			t.Errorf("%d answered %d", c.answer, code)
		}
		if !strings.Contains(errs, c.reads) {
			t.Errorf("%d reads %q", c.answer, errs)
		}
	}
}

// A namespace is required, because "a workflow belongs to exactly one namespace" and guessing one
// is how a workflow ends up somewhere nobody meant.
func TestANamespaceIsRequired(t *testing.T) {
	out, errs := &strings.Builder{}, &strings.Builder{}
	e := Env{Out: out, Err: errs, Dir: t.TempDir(), Getenv: func(string) string { return "x" }}
	if code := push(context.Background(), e, nil); code != exitUsage {
		t.Errorf("a push with no namespace answered %d", code)
	}
	if !strings.Contains(errs.String(), "exactly one namespace") {
		t.Errorf("it said %q", errs)
	}
}

// "The rest of the tree is yours to arrange, and every step of every run sees it, mounted
// read-only at /agk/repo." So the whole of it travels, and what travels is what git tracks.
func TestThePushCarriesTheTreeGitTracks(t *testing.T) {
	dir := repository(t)
	write(t, dir, "scripts/render.sh", "#!/bin/sh\necho hello\n")
	if err := os.Chmod(filepath.Join(dir, "scripts/render.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, ".gitignore", "build/\n")
	write(t, dir, "build/leftover.o", "not part of the repository")
	commitAll(t, dir, "a script")

	code, out, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitSucceeded {
		t.Fatalf("push answered %d: %s%s", code, out, errs)
	}
	if _, held := got.Tree["agentiik.yaml"]; !held {
		t.Errorf("the tree holds %v", keysOf(got.Tree))
	}
	script, held := got.Tree["scripts/render.sh"]
	if !held {
		t.Fatalf("the tree holds %v", keysOf(got.Tree))
	}
	if script.Mode != "0755" {
		t.Errorf("the script travels with mode %q, and a container has to be able to run it", script.Mode)
	}
	// An ignored file is ignored because somebody said it is not part of the repository,
	// and putting it in every run of every version would be this command deciding otherwise.
	if _, held := got.Tree["build/leftover.o"]; held {
		t.Errorf("an ignored file travelled: %v", keysOf(got.Tree))
	}
	if !strings.Contains(out, "files") {
		t.Errorf("it said %q", out)
	}
}

// A version is a commit, and outside a git repository there is none. So the push is refused
// rather than made of whatever the directory holds, under a hash nobody could check it against,
// and naming a commit by hand does not change that.
func TestAPushWithNoRepositoryBehindItIsRefused(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	dir := t.TempDir()
	// And git looks no higher than the directory, whatever this machine keeps its
	// temporary directories inside.
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(real))
	write(t, dir, "agentiik.yaml", scriptWorkflow)
	write(t, dir, "scripts/render.sh", "#!/bin/sh\n")

	for _, args := range [][]string{nil, {"--commit", "a3f9c1e", "--allow-dirty"}} {
		code, _, errs, got := pushing(t, dir, http.StatusOK, args...)
		if code != exitRefused {
			t.Errorf("a push of %v with no repository answered %d", args, code)
		}
		if got != nil {
			t.Errorf("a push of %v with no repository reached the server", args)
		}
		if !strings.Contains(errs, "not in a git repository") {
			t.Errorf("the refusal of %v reads %q", args, errs)
		}
	}
}

// A repository git will not read is not the absence of one. Git refuses a checkout another user
// owns, which is every CI container that mounts one, and says what to do about it: that sentence
// reaches the person pushing, rather than one telling them to go and find the repository they are
// standing in.
func TestARepositoryGitWillNotReadIsNotCalledNoRepository(t *testing.T) {
	dir := repository(t)
	// Git's own test switch for "owned by somebody else", with no configuration of this
	// machine's that could mark every directory safe.
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	code, _, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitRefused {
		t.Fatalf("a repository git will not read answered %d", code)
	}
	if got != nil {
		t.Error("it reached the server anyway")
	}
	if strings.Contains(errs, "not in a git repository") {
		t.Errorf("the refusal says there is no repository: %q", errs)
	}
	if !strings.Contains(errs, "dubious ownership") || !strings.Contains(errs, "safe.directory") {
		t.Errorf("the refusal does not pass on what git said and what it said to do: %q", errs)
	}
}

// A repository with no commit yet has nothing a version could be, and says so rather than that it
// holds no commit called HEAD.
func TestAnEmptyRepositoryIsRefused(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	write(t, dir, "agentiik.yaml", scriptWorkflow)

	code, _, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitRefused {
		t.Fatalf("a push from a repository with no commit answered %d", code)
	}
	if got != nil {
		t.Error("it reached the server anyway")
	}
	if !strings.Contains(errs, "has no commit yet") {
		t.Errorf("the refusal reads %q", errs)
	}
}

// A replace ref lives in one clone and in no other, so honouring it would push, under the commit's
// name, bytes no other clone of that commit holds. What travels is what the commit holds.
func TestAReplaceRefIsNotPushed(t *testing.T) {
	dir := repository(t)
	committed := gitIn(t, dir, "rev-parse", "HEAD:agentiik.yaml")
	other := filepath.Join(t.TempDir(), "other.yaml")
	if err := os.WriteFile(other, []byte(strings.Replace(scriptWorkflow, "monthly-invoicing", "replaced-invoicing", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	replacement := gitIn(t, dir, "hash-object", "-w", other)
	gitIn(t, dir, "replace", committed, replacement)
	if shown := gitIn(t, dir, "show", "HEAD:agentiik.yaml"); !strings.Contains(shown, "replaced-invoicing") {
		t.Fatalf("git does not honour the replace ref, so this proves nothing:\n%s", shown)
	}

	code, out, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitSucceeded {
		t.Fatalf("push answered %d: %s%s", code, out, errs)
	}
	if string(got.Tree["agentiik.yaml"].Content) != scriptWorkflow || string(got.Document) != scriptWorkflow {
		t.Errorf("what travelled is the replacement:\n%s", got.Document)
	}
	if !strings.Contains(out, "finance/monthly-invoicing@") {
		t.Errorf("the version was pushed as %s", out)
	}
}

// A commit is resolved before anything is read: one commit typed three ways is one version rather
// than three, and a name the repository does not hold is refused rather than pushed under. A hash
// names no branch to leave at it, which --branch names.
func TestACommitIsPushedUnderItsWholeHash(t *testing.T) {
	dir := repository(t)
	head := gitIn(t, dir, "rev-parse", "HEAD")
	branch := gitIn(t, dir, "rev-parse", "--abbrev-ref", "HEAD")

	for _, args := range [][]string{nil, {"--commit", head[:7], "--branch", branch}, {"--commit", branch}} {
		code, out, errs, _, pushed := pushingTo(t, dir, http.StatusOK, args...)
		if code != exitSucceeded {
			t.Fatalf("a push of %v answered %d: %s%s", args, code, out, errs)
		}
		if pushed != head {
			t.Errorf("a push of %v left the branch at %s", args, pushed)
		}
	}
	if code, _, errs, got := pushing(t, dir, http.StatusOK, "--commit", head[:7]); code != exitRefused || got != nil || !strings.Contains(errs, "--branch") {
		t.Errorf("a hash with no branch answered %d: %s", code, errs)
	}

	for _, c := range []struct{ named, reads string }{
		{"deadbeef", "holds no commit deadbeef"},
		// And a name git would take for an option of its own never reaches it.
		{"--output=elsewhere", "names no commit"},
	} {
		code, _, errs, got := pushing(t, dir, http.StatusOK, "--commit", c.named)
		if code != exitRefused || got != nil {
			t.Errorf("--commit %s answered %d", c.named, code)
		}
		if !strings.Contains(errs, c.reads) {
			t.Errorf("the refusal of --commit %s reads %q", c.named, errs)
		}
	}
}

// --commit names a commit the repository holds, and what is pushed is that commit: its entry
// point, its tree and its name, whatever HEAD has become since.
func TestACommitOtherThanHeadPushesItsOwnTree(t *testing.T) {
	dir := repository(t)
	write(t, dir, "scripts/old.sh", "#!/bin/sh\necho old\n")
	commitAll(t, dir, "the old script")
	earlier := gitIn(t, dir, "rev-parse", "HEAD")

	write(t, dir, "agentiik.yaml", scriptWorkflow+`
  later:
    image: docker.io/library/alpine@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    script: ["true"]
    outputs: [ok]
`)
	if err := os.Remove(filepath.Join(dir, "scripts/old.sh")); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "scripts/new.sh", "#!/bin/sh\necho new\n")
	commitAll(t, dir, "a later step and a new script")

	code, out, errs, got, pushed := pushingTo(t, dir, http.StatusOK, "--commit", earlier, "--branch", "earlier")
	if code != exitSucceeded {
		t.Fatalf("push answered %d: %s%s", code, out, errs)
	}
	if pushed != earlier {
		t.Errorf("the branch was left at %s", pushed)
	}
	if strings.Contains(string(got.Document), "later") {
		t.Error("the entry point the graph is rebuilt from is the one of HEAD")
	}
	if f := got.Tree["agentiik.yaml"]; string(f.Content) != scriptWorkflow {
		t.Errorf("the entry point travelled in the tree as\n%s", f.Content)
	}
	if _, held := got.Tree["scripts/old.sh"]; !held {
		t.Errorf("a file the commit holds was left out: %v", keysOf(got.Tree))
	}
	if _, held := got.Tree["scripts/new.sh"]; held {
		t.Errorf("a file of HEAD travelled: %v", keysOf(got.Tree))
	}
}

// A -f that names nothing, on the disk or in the commit, is a mistyped path and is said in the words
// agk validate says it in, before a byte of the tree is read. Telling somebody to commit a file
// that does not exist sends them looking for it; that sentence is for a file that is on the disk
// and was never committed.
func TestAMistypedEntryPointIsSaidToBeNowhere(t *testing.T) {
	dir := repository(t)
	write(t, dir, "docs/readme.md", "not a workflow")
	commitAll(t, dir, "some documentation")
	write(t, dir, "draft.yaml", scriptWorkflow)

	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	for _, c := range []struct{ entry, reads string }{
		{"workflow.yml", "there is no workflow at " + filepath.Join(dir, "workflow.yml")},
		{"docs/agentiik.yaml", "there is no workflow at " + filepath.Join(dir, "docs", "agentiik.yaml")},
		{"nowhere/agentiik.yaml", "there is no workflow at " + filepath.Join(dir, "nowhere", "agentiik.yaml")},
		{"draft.yaml", "holds no draft.yaml: what is pushed is the commit, so the entry point has to be committed"},
	} {
		code, _, errs, got := pushing(t, dir, http.StatusOK, "-f", c.entry)
		if code != exitRefused || got != nil {
			t.Errorf("-f %s answered %d", c.entry, code)
		}
		if !strings.Contains(errs, c.reads) {
			t.Errorf("the refusal of -f %s reads %q", c.entry, errs)
		}
	}

	said, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(said), "rev-parse") {
		t.Fatalf("git was not traced, so this proves nothing:\n%s", said)
	}
	if strings.Contains(string(said), "ls-tree") || strings.Contains(string(said), "cat-file") {
		t.Error("the tree was read before the entry point was found missing")
	}
}

// "The entry point is always at the root": a workflow kept in a directory of a larger repository is
// pushed as a repository of its own, and a push run from billing/, or given -f billing/agentiik.yaml,
// is refused naming the command that makes one, before a byte of the tree is read. Until v0.4.0 it
// pushed billing/ alone, and a version pushed that way stays runnable; a new one is not made so.
func TestAWorkflowBelowTheTopIsRefusedNamingTheSplitThatMakesItsRepository(t *testing.T) {
	dir := repository(t)
	write(t, dir, "billing/agentiik.yaml", scriptWorkflow)
	write(t, dir, "billing/scripts/render.sh", "#!/bin/sh\necho hello\n")
	commitAll(t, dir, "a second workflow")

	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	for _, c := range []struct {
		from string
		args []string
	}{
		{filepath.Join(dir, "billing"), nil},
		{dir, []string{"-f", "billing/agentiik.yaml"}},
	} {
		code, _, errs, got := pushing(t, c.from, http.StatusOK, c.args...)
		if code != exitRefused || got != nil {
			t.Fatalf("a push of billing/agentiik.yaml answered %d: %s", code, errs)
		}
		for _, want := range []string{"billing/agentiik.yaml", "entry-point-below-root", "git subtree split --prefix billing"} {
			if !strings.Contains(errs, want) {
				t.Errorf("the refusal does not say %q: %s", want, errs)
			}
		}
	}
	said, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(said), "ls-tree") || strings.Contains(string(said), "cat-file") {
		t.Error("the tree was read before the entry point was found below the root")
	}
}

// agk push judges a version as the installation and a hook will: the namespace a push is made to is
// the one the file writes, where it writes one, and a push to another is refused before anything is
// sent, at the line that writes it.
func TestAPushToAnotherNamespaceThanTheFileWritesIsRefused(t *testing.T) {
	dir := repository(t)
	code, _, errs, got := pushing(t, dir, http.StatusOK, "--namespace", "team-ops")
	if code != exitRefused || got != nil {
		t.Fatalf("a push to team-ops of a workflow of finance answered %d: %s", code, errs)
	}
	if !strings.Contains(errs, "agentiik.yaml:4:49: metadata-namespace-not-repository") {
		t.Errorf("the refusal reads %q", errs)
	}
}

// What is pushed is the commit, so a commit is pushed although a later commit has removed its entry
// point from the working copy.
func TestACommitFromBeforeItsEntryPointWasRemovedIsPushed(t *testing.T) {
	dir := repository(t)
	write(t, dir, "scripts/old.sh", "#!/bin/sh\necho old\n")
	commitAll(t, dir, "the workflow and its script")
	earlier := gitIn(t, dir, "rev-parse", "HEAD")
	gitIn(t, dir, "rm", "-q", "agentiik.yaml")
	gitIn(t, dir, "commit", "-qm", "the workflow retired")

	code, out, errs, got, pushed := pushingTo(t, dir, http.StatusOK, "--commit", earlier, "--branch", "earlier")
	if code != exitSucceeded {
		t.Fatalf("push answered %d: %s%s", code, out, errs)
	}
	if pushed != earlier {
		t.Errorf("the branch was left at %s", pushed)
	}
	if got.Entry != "agentiik.yaml" || string(got.Tree["agentiik.yaml"].Content) != scriptWorkflow {
		t.Errorf("the entry point travelled as %q in a tree holding %v", got.Entry, keysOf(got.Tree))
	}
	if _, held := got.Tree["scripts/old.sh"]; !held || len(got.Tree) != 2 {
		t.Errorf("the tree holds %v", keysOf(got.Tree))
	}
}

// A symbolic link is resolved on whatever host lays the tree out, where nothing stops it pointing
// outside the repository. So one is refused naming it, and what it points at is never read: the
// tree comes out of git's objects, where a link is the name of its target and nothing more.
func TestASymbolicLinkOutOfTheRepositoryIsRefused(t *testing.T) {
	dir := repository(t)
	outside := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(outside, []byte("a-secret-nobody-committed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "config", "credentials")); err != nil {
		t.Fatal(err)
	}
	commitAll(t, dir, "a link")

	code, out, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitRefused {
		t.Fatalf("a tree holding a symbolic link answered %d: %s%s", code, out, errs)
	}
	if got != nil {
		t.Error("it reached the server anyway")
	}
	if !strings.Contains(errs, "config/credentials") || !strings.Contains(errs, "symbolic link") {
		t.Errorf("the refusal does not name the link and say what it is: %q", errs)
	}
	if strings.Contains(out+errs, "a-secret-nobody-committed") {
		t.Error("what the link points at was read")
	}
}

// A submodule is another repository, which a runner would need a credential to fetch and never
// holds, so one is refused naming it.
func TestASubmoduleIsRefused(t *testing.T) {
	dir := repository(t)
	// Inside a commit a submodule is a gitlink and nothing more, so one is written straight
	// into the index rather than cloned from somewhere: the entry is what is refused. The
	// empty directory is what an uninitialised submodule looks like, and it leaves the
	// working tree clean.
	head := gitIn(t, dir, "rev-parse", "HEAD")
	gitIn(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+head+",vendor/lib")
	if err := os.MkdirAll(filepath.Join(dir, "vendor", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "commit", "-qm", "a submodule")

	code, out, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitRefused {
		t.Fatalf("a tree holding a submodule answered %d: %s%s", code, out, errs)
	}
	if got != nil {
		t.Error("it reached the server anyway")
	}
	if !strings.Contains(errs, "vendor/lib") || !strings.Contains(errs, "submodule") {
		t.Errorf("the refusal does not name the submodule and say what it is: %q", errs)
	}
}

// A name travels as it was committed. -z hands it over unquoted, and a space at either end of it
// is part of the name rather than something to trim.
func TestANameWithASpaceAtEitherEndSurvives(t *testing.T) {
	dir := repository(t)
	names := map[string]string{" leading.txt": "leading", "data/trailing.txt ": "trailing"}
	for name, body := range names {
		write(t, dir, name, body)
	}
	commitAll(t, dir, "names with spaces")

	code, out, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitSucceeded {
		t.Fatalf("push answered %d: %s%s", code, out, errs)
	}
	for name, body := range names {
		if f, held := got.Tree[name]; !held || string(f.Content) != body {
			t.Errorf("%q did not travel as committed: the tree holds %q", name, keysOf(got.Tree))
		}
	}
}

// A name that is not UTF-8 would arrive as some other name, since JSON text cannot carry it, so it
// is refused rather than renamed on the way.
func TestANameThatIsNotUTF8IsRefused(t *testing.T) {
	dir := repository(t)
	blob := gitIn(t, dir, "hash-object", "-w", "agentiik.yaml")
	gitIn(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+blob+",caf\xe9.txt")
	gitIn(t, dir, "commit", "-qm", "a Latin-1 name")

	// The name is in the commit and not on the disk, which is an uncommitted deletion.
	code, out, errs, got := pushing(t, dir, http.StatusOK, "--allow-dirty")
	if code != exitRefused {
		t.Fatalf("a name that is not UTF-8 answered %d: %s%s", code, out, errs)
	}
	if got != nil {
		t.Error("it reached the server anyway")
	}
	if !strings.Contains(errs, "not a UTF-8 name") {
		t.Errorf("the refusal reads %q", errs)
	}
}

// A name that is UTF-8 and holds U+FFFD is refused as well, and in words that are true of it: the
// installation refuses it because it cannot tell it from a name JSON mangled, and saying the name
// was not UTF-8 would be saying something false. Refused before any content is read, rather than
// by the installation after every file had been read and sent.
func TestANameHoldingTheReplacementCharacterIsRefusedBeforeTheTreeIsRead(t *testing.T) {
	dir := repository(t)
	blob := gitIn(t, dir, "hash-object", "-w", "agentiik.yaml")
	gitIn(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+blob+",notes/r\uFFFDsum\u00e9.txt")
	gitIn(t, dir, "commit", "-qm", "a name holding U+FFFD")

	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	code, out, errs, got := pushing(t, dir, http.StatusOK, "--allow-dirty")
	if code != exitRefused {
		t.Fatalf("a name holding U+FFFD answered %d: %s%s", code, out, errs)
	}
	if got != nil {
		t.Error("it reached the server anyway")
	}
	if !strings.Contains(errs, "U+FFFD") || !strings.Contains(errs, "rename") || strings.Contains(errs, "not a UTF-8 name") {
		t.Errorf("the refusal reads %q", errs)
	}
	readNoContent(t, trace)
}

// Every other name the installation refuses is refused here by the installation's own rule, before
// any content is read: a name a runner could not lay out, or would lay out somewhere else.
func TestANameTheInstallationRefusesIsRefusedBeforeTheTreeIsRead(t *testing.T) {
	for _, c := range []struct{ name, path, says string }{
		{"a backslash", `scripts\render.sh`, "backslash"},
		{"a name longer than a filesystem holds", "data/" + strings.Repeat("n", versions.TreeNameMaxBytes+1), fmt.Sprint(versions.TreeNameMaxBytes)},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := repository(t)
			blob := gitIn(t, dir, "hash-object", "-w", "agentiik.yaml")
			gitIn(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+c.path)
			gitIn(t, dir, "commit", "-qm", c.name)

			trace := filepath.Join(t.TempDir(), "trace")
			t.Setenv("GIT_TRACE", trace)
			code, out, errs, got := pushing(t, dir, http.StatusOK, "--allow-dirty")
			if code != exitRefused {
				t.Fatalf("%s answered %d: %s%s", c.name, code, out, errs)
			}
			if got != nil {
				t.Error("it reached the server anyway")
			}
			if !strings.Contains(errs, c.says) {
				t.Errorf("the refusal reads %q", errs)
			}
			readNoContent(t, trace)
		})
	}
}

// readNoContent says whether git, traced into trace, listed the tree and read none of it.
func readNoContent(t *testing.T, trace string) {
	t.Helper()
	said, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(said), "ls-tree") {
		t.Fatalf("git was not traced listing the tree, so this proves nothing:\n%s", said)
	}
	if strings.Contains(string(said), "cat-file") {
		t.Error("git was asked for content before the name refused the tree")
	}
}

// A repository git made with --object-format=sha256 names a commit by sixty-four characters, and an
// installation records a version under the forty of SHA-1. It is refused before any of the tree is
// read, rather than by the installation after all of it was read and sent, with the commit called
// not a commit.
func TestARepositoryOfSHA256IsRefusedBeforeTheTreeIsRead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	dir := t.TempDir()
	write(t, dir, "agentiik.yaml", scriptWorkflow)
	cmd := exec.Command("git", "init", "-q", "--object-format=sha256")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this git makes no SHA-256 repository: %s", out)
	}
	gitIn(t, dir, "config", "user.email", "test@example.com")
	gitIn(t, dir, "config", "user.name", "Test")
	commitAll(t, dir, "the workflow")

	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	code, out, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitRefused {
		t.Fatalf("a SHA-256 repository answered %d: %s%s", code, out, errs)
	}
	if got != nil {
		t.Error("it reached the server anyway")
	}
	if !strings.Contains(errs, "SHA-256") || !strings.Contains(errs, "SHA-1") {
		t.Errorf("the refusal reads %q", errs)
	}
	said, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(said), "rev-parse") {
		t.Fatalf("git was not traced, so this proves nothing:\n%s", said)
	}
	if strings.Contains(string(said), "ls-tree") || strings.Contains(string(said), "cat-file") {
		t.Error("git was asked for the tree before the hash refused the repository")
	}
}

// The mode is git's and not the disk's. A file committed executable travels as 0755 whatever its
// bits are on this machine, and an ordinary one travels as 0644, said rather than left out.
func TestTheModeOfAFileIsWhatGitSays(t *testing.T) {
	dir := repository(t)
	write(t, dir, "scripts/render.sh", "#!/bin/sh\necho hello\n")
	write(t, dir, "notes.txt", "an ordinary file")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "update-index", "--chmod=+x", "scripts/render.sh")
	gitIn(t, dir, "commit", "-qm", "modes")
	// And the disk says the opposite of both, which is only an uncommitted change.
	if err := os.Chmod(filepath.Join(dir, "scripts/render.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "notes.txt"), 0o755); err != nil {
		t.Fatal(err)
	}

	code, out, errs, got := pushing(t, dir, http.StatusOK, "--allow-dirty")
	if code != exitSucceeded {
		t.Fatalf("push answered %d: %s%s", code, out, errs)
	}
	for name, mode := range map[string]string{
		"scripts/render.sh": "0755", "notes.txt": "0644", "agentiik.yaml": "0644",
	} {
		if got.Tree[name].Mode != mode {
			t.Errorf("%s travelled with mode %q, and git says %s", name, got.Tree[name].Mode, mode)
		}
	}
}

// A tree is judged reading what the validation reads and nothing else: a large file beside the
// workflow is pushed in the pack, since a git push is bounded by its pack rather than by a request,
// and never read into the command's memory to judge the entry point.
func TestAFileTheValidationDoesNotReadIsNotReadToJudgeTheTree(t *testing.T) {
	dir := repository(t)
	write(t, dir, "fixtures/big.bin", strings.Repeat("x", 5<<20))
	commitAll(t, dir, "a fixture the steps read")
	big := gitIn(t, dir, "rev-parse", "HEAD:fixtures/big.bin")

	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	code, out, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitSucceeded {
		t.Fatalf("a tree holding a large file answered %d: %s%s", code, out, errs)
	}
	if len(got.Tree["fixtures/big.bin"].Content) != 5<<20 {
		t.Errorf("the large file arrived as %d bytes", len(got.Tree["fixtures/big.bin"].Content))
	}
	said, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(said), "cat-file") {
		t.Fatalf("git was not traced reading the entry point, so this proves nothing:\n%s", said)
	}
	if strings.Contains(string(said), "cat-file blob "+big) {
		t.Error("the large file was read to judge the tree")
	}
}

// The server counts files as well as bytes, so the client refuses the same tree before it reads a
// byte of it rather than sending it all to be refused.
func TestATreeWithTooManyFilesIsRefusedBeforeItsContentIsRead(t *testing.T) {
	dir := repository(t)
	for i := 0; i <= api.TreeMaxFiles; i++ {
		write(t, dir, fmt.Sprintf("vendor/%05d.txt", i), "")
	}
	commitAll(t, dir, "dependencies that belong in an image")

	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	code, out, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitRefused {
		t.Fatalf("a tree of too many files answered %d: %s%s", code, out, errs)
	}
	if got != nil {
		t.Error("it reached the server anyway")
	}
	if !strings.Contains(errs, "belong in an image") {
		t.Errorf("the refusal does not say where they go: %q", errs)
	}
	said, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(said), "cat-file") {
		t.Error("git was asked for content before the count refused the tree")
	}
}

// A partial clone fetches a blob it lacks the moment it is asked about, one request per file and
// all of them before a size could be added up. So a commit whose files the clone lacks is refused
// by name, with a way to fetch them at once, and nothing is fetched behind the person's back.
func TestAPartialCloneIsRefusedRatherThanFetchedFileByFile(t *testing.T) {
	origin := repository(t)
	write(t, origin, "scripts/render.sh", "#!/bin/sh\necho earlier\n")
	commitAll(t, origin, "a script")
	earlier := gitIn(t, origin, "rev-parse", "HEAD")
	lacking := gitIn(t, origin, "rev-parse", "HEAD:scripts/render.sh")
	write(t, origin, "scripts/render.sh", "#!/bin/sh\necho later\n")
	commitAll(t, origin, "a later script")
	// What a server has to allow for a clone to filter blobs out and fetch them later.
	gitIn(t, origin, "config", "uploadpack.allowFilter", "true")
	gitIn(t, origin, "config", "uploadpack.allowAnySHA1InWant", "true")

	// Checking HEAD out fetches HEAD's files, which leaves the earlier script as the one blob
	// the clone lacks.
	clone := filepath.Join(t.TempDir(), "clone")
	gitIn(t, origin, "clone", "-q", "--filter=blob:none", "file://"+origin, clone)
	if !lacks(clone, lacking) {
		t.Skip("this clone holds every blob, which is a git before 2.45 fetching whatever it is asked about, or one that did not filter")
	}

	code, out, errs, got := pushing(t, clone, http.StatusOK, "--commit", earlier, "--branch", "earlier")
	if code != exitRefused {
		t.Fatalf("a commit whose files the clone lacks answered %d: %s%s", code, out, errs)
	}
	if got != nil {
		t.Error("it reached the server anyway")
	}
	if !strings.Contains(errs, "holds scripts/render.sh, which this clone lacks") || !strings.Contains(errs, "git backfill") {
		t.Errorf("the refusal does not name the file and say how to fetch it: %q", errs)
	}
	if !lacks(clone, lacking) {
		t.Error("the blob was fetched on the way to the refusal")
	}
}

// lacks is whether the clone at dir lacks an object, asked without letting git fetch it.
func lacks(dir, object string) bool {
	cmd := exec.Command("git", "cat-file", "-e", object)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
	return cmd.Run() != nil
}

// Git reading an object it cannot inflate dies partway through its answer, and what it said on
// the way out is the refusal, rather than the end of a stream that stopped early.
func TestWhatGitSaysOfAnObjectItCannotReadIsPassedOn(t *testing.T) {
	dir := repository(t)
	// Bytes zlib cannot shrink, so that the object on the disk is long enough to be cut in
	// half past its header: the listing still reads the size, and only the content breaks.
	noise := make([]byte, 200<<10)
	rand.NewChaCha8([32]byte{}).Read(noise)
	if err := os.WriteFile(filepath.Join(dir, "fixtures.bin"), noise, 0o644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, dir, "a fixture")
	blob := gitIn(t, dir, "rev-parse", "HEAD:fixtures.bin")
	object := filepath.Join(dir, ".git", "objects", blob[:2], blob[2:])
	stored, err := os.ReadFile(object)
	if err != nil {
		t.Fatalf("the fixture is not a loose object, so this proves nothing: %v", err)
	}
	if err := os.Chmod(object, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(object, stored[:len(stored)/2], 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitRefused {
		t.Fatalf("a corrupt object answered %d: %s%s", code, out, errs)
	}
	if got != nil {
		t.Error("it reached the server anyway")
	}
	if strings.Contains(errs, "EOF") || !strings.Contains(errs, blob) {
		t.Errorf("the refusal is not what git said of %s: %q", blob, errs)
	}
}

func keysOf(m map[string]api.PushFile) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// taggedWorkflow names its images by tag, as a workflow written against a laptop's daemon does: a
// brick step, and a script step in a base image.
const taggedWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
outputs:
  invoices: { from: { step: normalize, port: ok } }
steps:
  normalize:
    image: ghcr.io/acme/agk-invoice:1.4.0
    outputs: [ok]
  report:
    image: alpine:3.21
    needs: [{ step: normalize, port: ok, as: in }]
    script: ["cat /agk/in/in/envelope.json"]
    outputs: [out]
`

const invoiceManifest = `apiVersion: agentiik.dev/v1
kind: Brick
metadata: { name: invoice, version: 1.4.0 }
spec:
  outputs:
    ok: {}
  runtime: { user: "65532:65532" }
`

const (
	invoiceDigest = "sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc"
	alpineDigest  = "sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d"
)

// taggedRepository is a repository holding taggedWorkflow, committed.
func taggedRepository(t *testing.T) string {
	t.Helper()
	dir := repository(t)
	write(t, dir, "agentiik.yaml", taggedWorkflow)
	commitAll(t, dir, "images by tag")
	return dir
}

// aDaemon is a fake daemon holding images, which the push reaches as it reaches any daemon: through
// DOCKER_HOST.
func aDaemon(t *testing.T, images map[string]dockertest.Image, bs ...dockertest.Behaviour) {
	t.Helper()
	daemon, err := dockertest.NewDaemon(append(bs, dockertest.With(dockertest.Options{Images: images}))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { daemon.Close() })
	t.Setenv("DOCKER_HOST", "unix://"+daemon.Socket())
}

// "A tag is a mutable pointer, and a commit must determine what ran." So every tag a workflow
// names, a script step's base image included, travels with the digest the registry serves it
// under, which the daemon holds it by; the file itself travels as it was committed, and the
// manifest under the tag it writes.
func TestEveryTagIsPushedWithTheDigestItsRegistryServes(t *testing.T) {
	dir := taggedRepository(t)
	aDaemon(t, map[string]dockertest.Image{
		"ghcr.io/acme/agk-invoice:1.4.0": {Digest: invoiceDigest, Manifest: []byte(invoiceManifest)},
		"alpine:3.21":                    {Digest: alpineDigest, Remote: true},
	})

	code, out, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitSucceeded {
		t.Fatalf("push answered %d: %s%s", code, out, errs)
	}
	want := map[string]string{
		"ghcr.io/acme/agk-invoice:1.4.0": "ghcr.io/acme/agk-invoice@" + invoiceDigest,
		"alpine:3.21":                    "alpine@" + alpineDigest,
	}
	if !maps.Equal(got.Images, want) {
		t.Errorf("the push carries the images %v, want %v", got.Images, want)
	}
	// Recorded by the digest the tag was resolved to, which is the image a version runs.
	if _, held := got.Manifests["ghcr.io/acme/agk-invoice@"+invoiceDigest]; !held || len(got.Manifests) != 1 {
		t.Errorf("the push records manifests for %v", slices.Sorted(maps.Keys(got.Manifests)))
	}
	if string(got.Document) != taggedWorkflow {
		t.Error("the entry point travelled as something other than what was committed")
	}
	for _, line := range []string{
		"ghcr.io/acme/agk-invoice:1.4.0 resolved to ghcr.io/acme/agk-invoice@" + invoiceDigest,
		"alpine:3.21 resolved to alpine@" + alpineDigest,
		"2 tags resolved to their digests",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("the push does not say %q: %s", line, out)
		}
	}
	if strings.Contains(errs, "already pushed") {
		t.Errorf("a version recording what was pushed is said to keep something else: %s", errs)
	}
}

// Each manifest is read out of the digest its tag was resolved to, and never out of the tag, so a
// tag moved on this machine between the two, by a build or a pull of it finishing, cannot pair the
// digest of one image with the manifest of another: what the version holds a step to is the image
// the version names.
func TestAManifestIsReadOutOfTheDigestItsTagWasResolvedTo(t *testing.T) {
	const tag = "ghcr.io/acme/agk-invoice:1.4.0"
	const movedDigest = "sha256:9999999999999999999999999999999999999999999999999999999999999999"
	dir := taggedRepository(t)
	aDaemon(t, map[string]dockertest.Image{
		tag:           {Digest: invoiceDigest, Manifest: []byte(invoiceManifest)},
		"alpine:3.21": {Digest: alpineDigest},
	}, dockertest.TagMoves(tag, dockertest.Image{
		Digest: movedDigest, Manifest: []byte(strings.Replace(invoiceManifest, "version: 1.4.0", "version: 1.4.1", 1)),
	}))

	code, out, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitSucceeded {
		t.Fatalf("push answered %d: %s%s", code, out, errs)
	}
	if want := "ghcr.io/acme/agk-invoice@" + invoiceDigest; got.Images[tag] != want {
		t.Fatalf("the tag was pushed as %s, and it named %s when it was resolved", got.Images[tag], want)
	}
	m, err := brick.ParseManifest(got.Manifests["ghcr.io/acme/agk-invoice@"+invoiceDigest])
	if err != nil {
		t.Fatalf("the manifest pushed for %s: %v", tag, err)
	}
	if m.Metadata.Version != "1.4.0" {
		t.Errorf("the version names %s with the manifest of %s %s, the image the tag moved to", got.Images[tag], m.Metadata.Name, m.Metadata.Version)
	}
}

// An installation that answers the repository's routes and not git, one from before v0.4.0 behind
// a proxy that answers anything, is said to be one, rather than read as a refusal of the push.
func TestAnInstallationThatDoesNotSpeakGitIsSaidSo(t *testing.T) {
	dir := repository(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/v1/finance/workflows" {
			w.WriteHeader(http.StatusCreated)
		}
		w.Write([]byte("<html>recorded</html>"))
	}))
	t.Cleanup(server.Close)

	code, out, errs := pushAgainst(dir, server.URL)
	if code != exitRefused || !strings.Contains(errs, "rather than git's") || strings.Contains(out, "pushed to") {
		t.Errorf("a push to an installation that does not speak git answered %d: %s%s", code, out, errs)
	}
}

// An image built on the machine and never pushed is refused naming it, with exit 1, and nothing
// is sent: a version naming it would be a version no runner could pull an image for. The
// containerd store, which holds such an image under a digest as it holds any other, is caught by
// asking the registry, which answers 403 or 401 for a repository it holds nothing of, the common
// case, and 404 for one it holds other images of. The classic store holds it under no digest.
func TestAnImageNeverPushedIsRefusedAndNothingIsSent(t *testing.T) {
	for _, c := range []struct {
		name   string
		images map[string]dockertest.Image
		bs     []dockertest.Behaviour
	}{
		{"a repository the registry holds nothing of", nil, nil},
		{"a registry that answers 401", nil, []dockertest.Behaviour{dockertest.RegistryAnswers401}},
		{"a repository the registry holds other images of", map[string]dockertest.Image{
			"ghcr.io/acme/agk-invoice:1.3.0": {Digest: alpineDigest},
		}, nil},
		{"the classic store", nil, []dockertest.Behaviour{dockertest.ClassicImageStore}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := taggedRepository(t)
			images := map[string]dockertest.Image{
				"ghcr.io/acme/agk-invoice:1.4.0": {Digest: invoiceDigest, Manifest: []byte(invoiceManifest), Unpushed: true},
				"alpine:3.21":                    {Digest: alpineDigest},
			}
			maps.Copy(images, c.images)
			aDaemon(t, images, c.bs...)

			code, out, errs, got := pushing(t, dir, http.StatusOK)
			if code != exitRefused {
				t.Fatalf("a push naming an image never pushed answered %d: %s%s", code, out, errs)
			}
			if got != nil {
				t.Error("the version reached the server anyway")
			}
			for _, want := range []string{"normalize", "ghcr.io/acme/agk-invoice:1.4.0", "never pushed"} {
				if !strings.Contains(errs, want) {
					t.Errorf("the refusal does not name %q: %s", want, errs)
				}
			}
		})
	}
}

// A registry that could not be asked has not said anything about the image, so the push stops
// with exit 4, as a daemon that is not there does, rather than telling somebody off their network
// that their image was never pushed.
func TestARegistryThatCannotBeAskedIsNoOutcome(t *testing.T) {
	dir := taggedRepository(t)
	aDaemon(t, map[string]dockertest.Image{
		"ghcr.io/acme/agk-invoice:1.4.0": {Digest: invoiceDigest, Manifest: []byte(invoiceManifest)},
		"alpine:3.21":                    {Digest: alpineDigest},
	}, dockertest.RegistryUnreachable)

	code, _, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitNoOutcome || got != nil {
		t.Errorf("a push whose registry could not be asked answered %d, and sent %v: %s", code, got != nil, errs)
	}
	if strings.Contains(errs, "never pushed") {
		t.Errorf("a registry nobody reached was taken for an image nobody pushed: %s", errs)
	}
}

// A reference that writes a digest the wire does not carry is refused before any daemon is asked,
// naming the step, and so is nothing a runner could be handed.
func TestADigestThatIsNotOneIsRefusedBeforeADaemonIsAsked(t *testing.T) {
	dir := repository(t)
	write(t, dir, "agentiik.yaml", strings.Replace(scriptWorkflow, "@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc", "@sha256:1ab74e66", 1))
	commitAll(t, dir, "a digest cut short")
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "nobody.sock"))

	code, _, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitRefused || got != nil {
		t.Fatalf("a digest cut short answered %d: %s", code, errs)
	}
	for _, want := range []string{"normalize", "sixty-four"} {
		if !strings.Contains(errs, want) {
			t.Errorf("the refusal does not name %q: %s", want, errs)
		}
	}
}

// A port past agk.PortMaxBytes is refused by agk push as by the installation, and before anything
// is sent: the commit is read as a version about to be made.
func TestAPortPastItsBoundIsRefusedBeforeThePushIsSent(t *testing.T) {
	dir := repository(t)
	write(t, dir, "agentiik.yaml", strings.Replace(scriptWorkflow, "outputs: [ok]", "outputs: [ok, rejected-"+strings.Repeat("x", 242)+"]", 1))
	commitAll(t, dir, "a port of 251 characters")

	code, out, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitRefused {
		t.Fatalf("the push answered %d: %s%s", code, out, errs)
	}
	if got != nil {
		t.Error("it reached the installation anyway")
	}
	if !strings.Contains(errs, "at most 250") {
		t.Errorf("the refusal reads %q", errs)
	}
}

// A repository a push creates takes the branch the push moves as its default, so that its first
// push is to the branch a clone checks out and a run naming no ref runs, whichever branch is
// checked out here.
func TestARepositoryAPushCreatesTakesThePushedBranchAsItsDefault(t *testing.T) {
	dir := repository(t)
	in := aPushStandIn(t, http.StatusOK)
	code, out, errs := pushAgainst(dir, in.server.URL, "--branch", "release")
	if code != exitSucceeded {
		t.Fatalf("push answered %d: %s%s", code, out, errs)
	}
	if len(in.created) != 1 || in.created[0].DefaultBranch != "release" {
		t.Errorf("the repository was created as %+v", in.created)
	}
	if head := in.git(in.bare("monthly-invoicing"), "symbolic-ref", "HEAD"); head != "refs/heads/release" {
		t.Errorf("the repository's HEAD names %s", head)
	}
}
