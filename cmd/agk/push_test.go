package main

import (
	"context"
	"encoding/json"
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

// "A version is a commit." Pushing the bytes in the working copy under the name of a commit whose
// tree differs is a version that says it is one thing and is another, for ever, and nothing
// downstream can notice: the digests match what was pushed.
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
	if !strings.Contains(errs, "not what that commit names") {
		t.Errorf("the refusal reads %q", errs)
	}
	if !strings.Contains(errs, "agentiik.yaml") {
		t.Errorf("the refusal does not name what differs: %q", errs)
	}

	// And somebody who knows what they are doing says so.
	code, _, _, got = pushing(t, dir, http.StatusOK, "--allow-dirty")
	if code != exitSucceeded {
		t.Errorf("--allow-dirty answered %d", code)
	}
	if got == nil || !strings.Contains(string(got.Document), "sneaky") {
		t.Error("what arrived is not the modified tree")
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
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "a script"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}

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

func keysOf(m map[string]api.PushFile) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
