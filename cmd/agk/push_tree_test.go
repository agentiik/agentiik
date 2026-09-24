package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/dockertest"
	"github.com/agentiik/agentiik/internal/fixtures"
	versions "github.com/agentiik/agentiik/version"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The tree a push carries, followed all the way to the runner: agk push reads a commit out of
// git, the API stores it, the controller dispatches a task of a run of that commit, and the
// runner's redemption answers the files it lays out under /agk/repo.
//
// The two halves were built apart and each is tested against a stand-in for the other: push_test
// against a server that only decodes the body, api/grant_test against a version written straight
// into the database. This is the one place the real client talks to the real server, and what the
// runner is handed is compared with git's own view of the commit rather than with anything either
// half computed, so that the two cannot agree with each other and both be wrong.

const treeWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
include:
  - path: fragments/common.yaml
outputs:
  invoices: { from: { step: normalize, port: ok } }
steps:
  normalize:
    extends: .alpine
    script: ["/agk/repo/scripts/render.sh"]
    outputs: [ok]
`

// treeFragment includes a file of its own, which resolves against the fragment's directory.
const treeFragment = `
include:
  - path: nested/deeper.yaml
.alpine:
  image: docker.io/library/alpine@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
`

const treeNested = `
defaults:
  timeout: 10m
`

const renderScript = "#!/bin/sh\necho hello > /agk/out/ports/ok\n"

// alice may do everything, and says who she is in the header, which is what the api package's
// own tests do.
type alice struct{}

func (alice) Allow(_ context.Context, who api.Principal, _ api.Permission, _ api.Target) (bool, error) {
	return who == "alice", nil
}

func bearerOf(r *http.Request) (api.Principal, error) {
	return api.Principal(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), nil
}

// dispatched keeps what the controller published instead of putting it on a bus.
type dispatched struct {
	mu   sync.Mutex
	sent []controller.Dispatch
}

func (q *dispatched) Publish(_ context.Context, d controller.Dispatch) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sent = append(q.sent, d)
	return nil
}

func (q *dispatched) Stop(context.Context, graph.Stop) error { return nil }

// installation is the API over PostgreSQL and a directory of objects, served over HTTP so that
// the presigned URLs a redemption mints are URLs a client can follow. Every route is on one
// router, as an installation serves them.
type installation struct {
	url     string
	pool    *db.Pool
	objects artifact.Objects
	store   *versions.Store
}

func anInstallation(t *testing.T) installation {
	t.Helper()
	pool, super := dbtest.Open(t)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
		t.Fatal(err)
	}
	if err := pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		return w.CreateRunnerPool(ctx, db.RunnerPool{Name: "dmz", Labels: []string{"zone=dmz"}, CreatedBy: "alice"})
	}); err != nil {
		t.Fatal(err)
	}

	// The URL is needed before the handler exists, because the presigner is built with it.
	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)

	objects := artifact.Dir(t.TempDir())
	store, err := versions.New(pool, versions.Options{})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := artifact.NewSigned(objects, artifact.SignedOptions{
		Key: []byte("0123456789abcdef0123456789abcdef"), Base: srv.URL + "/objects",
	})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(alice{}, bearerOf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewServer(rt, api.ServerOptions{Pool: pool, Versions: store, Objects: objects}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewRunners(rt, api.RunnerOptions{Pool: pool, Objects: objects, URLs: signed, Secrets: api.NoSecrets{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewObjects(rt, signed); err != nil {
		t.Fatal(err)
	}
	handler = rt
	return installation{url: srv.URL, pool: pool, objects: objects, store: store}
}

// ask sends one JSON request, decodes a successful answer into out, and answers the status.
func (in installation) ask(t *testing.T, method, path, credential string, body, out any) int {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequestWithContext(t.Context(), method, in.url+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	if credential != "" {
		r.Header.Set("Authorization", "Bearer "+credential)
	}
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode >= 300 {
		t.Logf("%s %s answered %d: %s", method, path, res.StatusCode, raw)
		return res.StatusCode
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s answered %s", method, path, raw)
		}
	}
	return res.StatusCode
}

// pushFrom runs agk push in dir against the installation, as alice.
func (in installation) pushFrom(t *testing.T, dir string, args ...string) (int, string) {
	t.Helper()
	out, errs := &strings.Builder{}, &strings.Builder{}
	e := Env{
		Out: out, Err: errs, Dir: dir,
		Getenv: func(k string) string {
			switch k {
			case tokenVariable:
				return "alice"
			case serverVariable:
				return in.url
			}
			return ""
		},
	}
	code := push(t.Context(), e, append([]string{"--namespace", "finance"}, args...))
	return code, out.String() + errs.String()
}

// firstDispatch starts a run of the commit, as alice, and answers the one task the controller
// dispatches for it, with the grant the controller wrote.
func (in installation) firstDispatch(t *testing.T, sha string) controller.Dispatch {
	t.Helper()
	var started struct {
		Run string `json:"run"`
	}
	if code := in.ask(t, "POST", "/api/v1/finance/workflows/monthly-invoicing/runs", "alice", api.Start{Commit: sha}, &started); code != http.StatusAccepted {
		t.Fatalf("starting a run answered %d", code)
	}
	c, err := controller.New(in.pool, "end-to-end")
	if err != nil {
		t.Fatal(err)
	}
	term, err := in.pool.BeginTerm(t.Context(), "end-to-end")
	if err != nil {
		t.Fatal(err)
	}
	q := &dispatched{}
	core, err := controller.NewCore(c, term, controller.Options{Queue: q, Versions: in.store, Objects: in.objects})
	if err != nil {
		t.Fatal(err)
	}
	if err := core.Decide(t.Context(), agk.RunID(started.Run)); err != nil {
		t.Fatal(err)
	}
	if len(q.sent) != 1 {
		t.Fatalf("the controller dispatched %d tasks", len(q.sent))
	}
	return q.sent[0]
}

// committedFile is one file of a commit as git itself gives it.
type committedFile struct {
	content []byte
	mode    string
}

// gitsView is the tree at treeish, read by git archive for the bytes and git ls-tree for the mode.
// Not cat-file --batch, which is how push reads it: what is compared against has to come from
// somewhere push does not.
func gitsView(t *testing.T, dir, treeish string) map[string]committedFile {
	t.Helper()
	cmd := exec.Command("git", "archive", "--format=tar", treeish)
	cmd.Dir = dir
	archive, err := cmd.Output()
	if err != nil {
		t.Fatalf("git archive %s: %v", treeish, err)
	}
	out := map[string]committedFile{}
	r := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		content, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = committedFile{content: content}
	}

	// The mode from the listing rather than from the archive, which applies tar.umask.
	cmd = exec.Command("git", "ls-tree", "-r", "-z", treeish)
	cmd.Dir = dir
	listed, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range strings.Split(string(listed), "\x00") {
		if record == "" {
			continue
		}
		meta, path, _ := strings.Cut(record, "\t")
		f := out[path]
		switch strings.Fields(meta)[0] {
		case "100755":
			f.mode = "0755"
		case "100644":
			f.mode = "0644"
		default:
			t.Fatalf("%s is committed as %s", path, meta)
		}
		out[path] = f
	}
	return out
}

func sha256Of(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// "The whole tree is mounted read-only at /agk/repo/ in every step", and a runner "fetches
// content-addressed objects with the task's grant". So what a runner is handed for a task of a
// pushed commit is that commit's tree: every path, every byte and every mode, and nothing else.
func TestWhatARunnerIsHandedIsTheCommitThatWasPushed(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	in := anInstallation(t)

	// A workflow below the top of its repository, so that the root is the entry point's
	// directory rather than the repository's, with an include that itself includes, an
	// executable script, a name with a space, a second file holding the script's bytes at
	// the other mode, and an empty file. The repository around it holds files of its own,
	// which the runner must not be handed.
	dir := repository(t)
	write(t, dir, "README.md", "# the repository, outside the workflow's directory\n")
	write(t, dir, "billing/agentiik.yaml", treeWorkflow)
	write(t, dir, "billing/fragments/common.yaml", treeFragment)
	write(t, dir, "billing/fragments/nested/deeper.yaml", treeNested)
	write(t, dir, "billing/scripts/render.sh", renderScript)
	if err := os.Chmod(filepath.Join(dir, "billing/scripts/render.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "billing/data/monthly report.txt", "a name with a space\n")
	write(t, dir, "billing/data/render copy.sh", renderScript)
	write(t, dir, "billing/data/empty", "")
	commitAll(t, dir, "a workflow below the top")
	sha := gitIn(t, dir, "rev-parse", "HEAD")
	want := gitsView(t, dir, sha+":billing")

	code, said := in.pushFrom(t, filepath.Join(dir, "billing"))
	if code != exitSucceeded {
		t.Fatalf("push answered %d: %s", code, said)
	}
	// And the same commit pushed again is the same version, which the server answers as one.
	if code, said := in.pushFrom(t, filepath.Join(dir, "billing")); code != exitSucceeded {
		t.Fatalf("pushing the same commit again answered %d: %s", code, said)
	}

	// What the graph is rebuilt from is the committed bytes, under the paths the tree names.
	var v db.Version
	if err := in.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		var err error
		v, err = ns.Version(ctx, "monthly-invoicing", sha)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if v.Entry != "agentiik.yaml" || !bytes.Equal(v.Document, want["agentiik.yaml"].content) {
		t.Errorf("the version's entry point is %q holding %q", v.Entry, v.Document)
	}
	if got := slices.Sorted(maps.Keys(v.Includes)); !slices.Equal(got, []string{"fragments/common.yaml", "fragments/nested/deeper.yaml"}) {
		t.Errorf("the version includes %q", got)
	}
	for p, body := range v.Includes {
		if !bytes.Equal(body, want[p].content) {
			t.Errorf("the include %s is %q and the commit holds %q", p, body, want[p].content)
		}
	}

	// A run of that commit, and the task the controller dispatches for it, with the grant
	// the controller wrote.
	d := in.firstDispatch(t, sha)

	// A machine joins, and redeems the task's grant.
	var join db.JoinToken
	if err := in.pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		var err error
		now := time.Now().UTC()
		join, err = w.IssueJoinToken(ctx, "dmz", nil, "alice", now, now.Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var joined struct {
		Credential string `json:"credential"`
	}
	if code := in.ask(t, "POST", "/api/v1/runners", "", api.Join{
		Token: join.Clear, CPU: 8, MemoryBytes: 1 << 34, DiskBytes: 1 << 38, Architecture: "amd64", AgentVersion: "0.2.0",
	}, &joined); code != http.StatusCreated {
		t.Fatalf("joining answered %d", code)
	}
	var grant api.Grant
	if code := in.ask(t, "POST", "/api/v1/tasks/redeem", joined.Credential, api.Redemption{Grant: d.Grant, TaskID: d.Row, IdempotencyKey: d.Task.ID}, &grant); code != http.StatusOK {
		t.Fatalf("redeeming answered %d", code)
	}

	// Every file of the commit at the entry point's directory, and nothing beside it, each
	// fetched through the URL it came with and compared byte for byte and mode for mode.
	var paths []string
	urls := map[string]string{}
	for _, e := range grant.Tree {
		paths = append(paths, e.Path)
		urls[e.Path] = e.URL
		committed, held := want[e.Path]
		if !held {
			t.Errorf("the runner is handed %q, which the commit does not hold under billing/", e.Path)
			continue
		}
		if e.Mode != committed.mode {
			t.Errorf("%s is handed at %s and committed at %s", e.Path, e.Mode, committed.mode)
		}
		if e.SHA256 != sha256Of(committed.content) {
			t.Errorf("%s is named by %s", e.Path, e.SHA256)
		}
		res, err := http.Get(e.URL)
		if err != nil {
			t.Fatal(err)
		}
		fetched, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusOK {
			t.Errorf("fetching %s answered %d: %s", e.Path, res.StatusCode, fetched)
			continue
		}
		if !bytes.Equal(fetched, committed.content) {
			t.Errorf("fetching %s answered %q and the commit holds %q", e.Path, fetched, committed.content)
		}
	}
	if wantPaths := slices.Sorted(maps.Keys(want)); !slices.Equal(paths, wantPaths) {
		t.Errorf("the runner is handed %q, and the commit holds %q under billing/", paths, wantPaths)
	}
	// Two files of the same bytes are one object whatever their modes, and one URL.
	if urls["scripts/render.sh"] == "" || urls["scripts/render.sh"] != urls["data/render copy.sh"] {
		t.Error("two files of identical bytes were not handed one URL")
	}
}

// A tag a workflow names is dispatched as the digest the push resolved it to, whichever day the
// run starts: what the controller hands the bus is what the wire's imageRef takes, compiled out of
// the vendored document, and never the tag the file writes, which that same schema refuses.
func TestATaskOfAPushedVersionNamesItsImageByDigest(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	in := anInstallation(t)
	dir := repository(t)
	write(t, dir, "agentiik.yaml", strings.Replace(scriptWorkflow,
		"docker.io/library/alpine@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc", "alpine:3.21", 1))
	commitAll(t, dir, "a base image by tag")
	sha := gitIn(t, dir, "rev-parse", "HEAD")
	aDaemon(t, map[string]dockertest.Image{"alpine:3.21": {Digest: alpineDigest}})

	if code, said := in.pushFrom(t, dir); code != exitSucceeded {
		t.Fatalf("push answered %d: %s", code, said)
	}
	d := in.firstDispatch(t, sha)
	if want := "alpine@" + alpineDigest; d.Task.Image != want {
		t.Errorf("the task names %q, and its tag was pushed as %s", d.Task.Image, want)
	}

	imageRef := wireSchema(t, "imageRef")
	if err := imageRef.Validate(d.Task.Image); err != nil {
		t.Errorf("the wire refuses the image the task names: %v", err)
	}
	if imageRef.Validate("alpine:3.21") == nil {
		t.Error("the wire's imageRef accepts a tag, so holding the task to it holds it to nothing")
	}
}

// A commit is the version its first push recorded, the digest each of its tags resolved to
// included, so that every run of it runs the same bytes. Pushed again once a tag has moved, it is
// still that version: the push succeeds, a task of it names the first digest, and agk push says so
// rather than leaving the digest it resolved this time to read as the one the version took.
func TestACommitPushedAgainAfterItsTagMovedKeepsItsFirstDigest(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	const movedDigest = "sha256:9999999999999999999999999999999999999999999999999999999999999999"
	in := anInstallation(t)
	dir := repository(t)
	write(t, dir, "agentiik.yaml", strings.Replace(scriptWorkflow,
		"docker.io/library/alpine@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc", "alpine:3.21", 1))
	commitAll(t, dir, "a base image by tag")
	sha := gitIn(t, dir, "rev-parse", "HEAD")

	aDaemon(t, map[string]dockertest.Image{"alpine:3.21": {Digest: alpineDigest}})
	if code, said := in.pushFrom(t, dir); code != exitSucceeded || strings.Contains(said, "already pushed") {
		t.Fatalf("the first push answered %d: %s", code, said)
	}

	aDaemon(t, map[string]dockertest.Image{"alpine:3.21": {Digest: movedDigest}})
	code, said := in.pushFrom(t, dir)
	if code != exitSucceeded {
		t.Fatalf("pushing the same commit again answered %d: %s", code, said)
	}
	var kept string
	for _, line := range strings.Split(said, "\n") {
		if strings.Contains(line, "already pushed") {
			kept = line
		}
	}
	for _, want := range []string{"alpine:3.21", "alpine@" + alpineDigest, "alpine@" + movedDigest, "a new commit"} {
		if !strings.Contains(kept, want) {
			t.Errorf("the second push does not say that the version keeps its first digest, naming %q: %s", want, said)
		}
	}

	if d := in.firstDispatch(t, sha); d.Task.Image != "alpine@"+alpineDigest {
		t.Errorf("the task names %q, and the commit was first pushed with alpine@%s", d.Task.Image, alpineDigest)
	}
}

// wireSchema compiles one definition of the vendored wire document.
func wireSchema(t *testing.T, definition string) *jsonschema.Schema {
	t.Helper()
	doc, err := fixtures.Wire()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("wire.schema.json", raw); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("wire.schema.json#/$defs/" + definition)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
