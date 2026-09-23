package main

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
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

// pushing runs the command against a server that records what arrived.
func pushing(t *testing.T, dir string, answer int, args ...string) (int, string, string, *api.Push) {
	t.Helper()
	code, out, errs, got, _ := pushingTo(t, dir, answer, args...)
	return code, out, errs, got
}

// pushingTo is pushing, and the path the version was sent to, which is where its commit is named.
func pushingTo(t *testing.T, dir string, answer int, args ...string) (int, string, string, *api.Push, string) {
	t.Helper()
	var got *api.Push
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.Header.Get("Authorization") != "Bearer the-token" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"no"}`))
			return
		}
		var p api.Push
		json.NewDecoder(r.Body).Decode(&p)
		got = &p
		w.WriteHeader(answer)
		if answer != http.StatusOK {
			w.Write([]byte(`{"error":"the installation said no"}`))
			return
		}
		w.Write([]byte(`{"commit":"x"}`))
	}))
	t.Cleanup(server.Close)

	out, errs := &strings.Builder{}, &strings.Builder{}
	e := Env{
		Out: out, Err: errs, Dir: dir,
		Getenv: func(k string) string {
			switch k {
			case tokenVariable:
				return "the-token"
			case serverVariable:
				return server.URL
			}
			return ""
		},
	}
	code := push(context.Background(), e, append([]string{"--namespace", "finance"}, args...))
	return code, out.String(), errs.String(), got, path
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
func TestAllowDirtyPushesTheCommitAndNotTheEdits(t *testing.T) {
	dir := repository(t)
	write(t, dir, "agentiik.yaml", scriptWorkflow+`
  sneaky:
    image: docker.io/library/alpine@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    script: ["true"]
    outputs: [ok]
`)
	write(t, dir, "notes/draft.txt", "never committed")

	code, out, errs, got := pushing(t, dir, http.StatusOK, "--allow-dirty")
	if code != exitSucceeded {
		t.Fatalf("--allow-dirty answered %d: %s%s", code, out, errs)
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
		{http.StatusUnprocessableEntity, "refused the version"},
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

// A commit is resolved before anything is read: one commit typed three ways is one version rather
// than three, and a name the repository does not hold is refused rather than pushed under.
func TestACommitIsPushedUnderItsWholeHash(t *testing.T) {
	dir := repository(t)
	head := gitIn(t, dir, "rev-parse", "HEAD")
	branch := gitIn(t, dir, "rev-parse", "--abbrev-ref", "HEAD")

	for _, args := range [][]string{nil, {"--commit", head[:7]}, {"--commit", branch}} {
		code, out, errs, _, path := pushingTo(t, dir, http.StatusOK, args...)
		if code != exitSucceeded {
			t.Fatalf("a push of %v answered %d: %s%s", args, code, out, errs)
		}
		if !strings.HasSuffix(path, "/versions/"+head) {
			t.Errorf("a push of %v was sent to %s", args, path)
		}
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

	code, out, errs, got, path := pushingTo(t, dir, http.StatusOK, "--commit", earlier)
	if code != exitSucceeded {
		t.Fatalf("push answered %d: %s%s", code, out, errs)
	}
	if !strings.HasSuffix(path, "/versions/"+earlier) {
		t.Errorf("the version was sent to %s", path)
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

// Every size comes out of git's listing, so a tree above the limit is refused before a byte of it
// is read: git is asked for the listing and never for a blob.
func TestAnOversizedTreeIsRefusedBeforeItsContentIsRead(t *testing.T) {
	dir := repository(t)
	write(t, dir, "fixtures/big.bin", strings.Repeat("x", api.TreeMaxBytes))
	commitAll(t, dir, "a fixture that belongs somewhere else")

	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	code, out, errs, got := pushing(t, dir, http.StatusOK)
	if code != exitRefused {
		t.Fatalf("an oversized tree answered %d: %s%s", code, out, errs)
	}
	if got != nil {
		t.Error("it reached the server anyway")
	}
	if !strings.Contains(errs, "belongs in an image or in an artifact") {
		t.Errorf("the refusal does not say where something this size goes: %q", errs)
	}

	said, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(said), "ls-tree") {
		t.Fatalf("git was not traced, so this proves nothing:\n%s", said)
	}
	if strings.Contains(string(said), "cat-file") {
		t.Error("git was asked for content before the size refused the tree")
	}
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
