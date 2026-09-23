package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/fixtures"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// What a grant turns into, and what it refuses to turn into.

const grantRun = "01JMZ8V1P9C4XQ7K2N4D6F8H0A"
const grantTaskRow = "01M2T1AAAAAAAAAAAAAAAAAAAA"

// held is a secret store holding exactly what a test put in it.
type held map[string]string

func (h held) Value(_ context.Context, namespace, name string) ([]byte, error) {
	v, ok := h[namespace+"/"+name]
	if !ok {
		return nil, api.ErrNoSecret
	}
	return []byte(v), nil
}

type grants struct {
	handler http.Handler
	pool    *db.Pool
	super   string
	objects artifact.Objects
	signed  *artifact.Signed
}

// theTree is the repository the run's version is: an entry point, a script that has to run, and a
// second script holding the first one's bytes, which is one object and therefore one URL.
func theTree() map[string]api.PushFile {
	return map[string]api.PushFile{
		"agentiik.yaml":     {Content: []byte("apiVersion: agentiik.dev/v1\nkind: Workflow\n"), Mode: "0644"},
		"scripts/render.sh": {Content: []byte("#!/bin/sh\necho render\n"), Mode: "0755"},
		"scripts/again.sh":  {Content: []byte("#!/bin/sh\necho render\n"), Mode: "0755"},
	}
}

func withGrants(t *testing.T, secrets api.Secrets) grants {
	t.Helper()
	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	for _, stmt := range []string{
		`insert into namespaces (name) values ('finance')`,
		`insert into workflows (namespace, name) values ('finance','monthly-invoicing')`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("seeding: %s", err)
		}
	}
	// The version the run pins, recorded the way a push records it, before the run that
	// names it.
	objects := artifact.Dir(t.TempDir())
	g := grants{pool: pool, objects: objects, super: super}
	g.recorded(t, "a3f9c1e", theTree())
	for _, stmt := range []string{
		`insert into runs (namespace, id, workflow, commit, trigger)
		   values ('finance','` + grantRun + `','monthly-invoicing','a3f9c1e','manual')`,
		`insert into steps (namespace, run_id, step) values ('finance','` + grantRun + `','render')`,
		`insert into tasks (namespace, id, run_id, step, attempt, state)
		   values ('finance','` + grantTaskRow + `','` + grantRun + `','render',1,'dispatched')`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("seeding: %s", err)
		}
	}
	if err := pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		return w.CreateRunnerPool(ctx, db.RunnerPool{Name: "dmz", Labels: []string{"zone=dmz"}, CreatedBy: "admin"})
	}); err != nil {
		t.Fatal(err)
	}

	signed, err := artifact.NewSigned(objects, artifact.SignedOptions{
		Key: []byte("0123456789abcdef0123456789abcdef"), Base: "https://agentiik.example.com/objects",
	})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(everything{who: "admin"}, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewRunners(rt, api.RunnerOptions{
		Pool: pool, Objects: objects, URLs: signed, Secrets: secrets,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewObjects(rt, signed); err != nil {
		t.Fatal(err)
	}
	g.handler, g.signed = rt, signed
	return g
}

// recorded writes one version of the workflow the way a push does: every file in the store under
// the digest of its bytes, and the version naming them.
func (g grants) recorded(t *testing.T, commit string, files map[string]api.PushFile) {
	t.Helper()
	var tree []db.TreeFile
	for path, f := range files {
		if err := g.objects.Put(t.Context(), keyOf("finance", f.Content), bytes.NewReader(f.Content)); err != nil {
			t.Fatal(err)
		}
		tree = append(tree, db.TreeFile{Path: path, SHA256: digestOf(f.Content), Size: int64(len(f.Content)), Mode: f.Mode})
	}
	if err := g.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		_, err := ns.SaveVersion(ctx, db.Version{
			Workflow: "monthly-invoicing", Commit: commit,
			Entry: "agentiik.yaml", Document: files["agentiik.yaml"].Content,
			Tree: tree, Author: "alice",
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// joined puts a machine in the pool and answers its credential.
func (g grants) joined(t *testing.T) string {
	t.Helper()
	var token db.JoinToken
	if err := g.pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		var err error
		token, err = w.IssueJoinToken(ctx, "dmz", nil, "admin", time.Now().UTC().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w, answer := call(t, g.handler, "POST", "/api/v1/runners", "", aMachine(token.Clear))
	if w.Code != http.StatusCreated {
		t.Fatalf("joining answered %d: %s", w.Code, w.Body)
	}
	credential, _ := answer["credential"].(string)
	return credential
}

// dispatched writes what the controller writes: an input envelope naming an artifact, the
// artifact itself, and the grant that says the task may have both.
func (g grants) dispatched(t *testing.T, secrets []string) (clear, envelope, file string) {
	t.Helper()
	const content = "the whole of an invoice"
	sum := sha256.Sum256([]byte(content))
	file = hex.EncodeToString(sum[:])
	if err := g.objects.Put(t.Context(), artifact.Key("finance", file), readerOf(content)); err != nil {
		t.Fatal(err)
	}

	e := agk.Envelope{
		Meta: agk.Meta{RunID: grantRun, Step: "collect", Port: "out", Attempt: 1, Count: 1, ProducedAt: time.Now().UTC()},
		Items: []agk.Item{{
			ID:   "01M2ITEMAAAAAAAAAAAAAAAAAA",
			Data: map[string]any{"total": 42},
			Files: []agk.File{{
				Name:      "invoice.pdf",
				URI:       agk.URI{Run: grantRun, Step: "collect", Port: "out", Name: "invoice.pdf"},
				MediaType: "application/pdf", Size: int64(len(content)), SHA256: file,
			}},
		}},
	}
	envelope, _, err := artifact.PutEnvelope(t.Context(), g.objects, "finance", e)
	if err != nil {
		t.Fatal(err)
	}

	var granted db.Granted
	if err := g.pool.Installation(t.Context(), db.ControllerSweep, func(ctx context.Context, w *db.Wide) error {
		var err error
		granted, err = w.IssueGrant(ctx, "finance", agk.TaskID(grantRun+"/render/1"), grantTaskRow,
			db.GrantScope{
				Run: grantRun, Step: "render",
				Workflow: "monthly-invoicing", Commit: "a3f9c1e",
				Inputs:  []db.GrantInput{{Port: "in", Digest: envelope, Items: 1}},
				Secrets: secrets,
			}, time.Now().UTC().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return granted.Clear, envelope, file
}

// grantedFor issues the task's grant again, naming one version and nothing else, which is what a
// controller does when it writes the scope. What the version is decides what the runner is given.
func (g grants) grantedFor(t *testing.T, workflow, commit string) string {
	t.Helper()
	var granted db.Granted
	if err := g.pool.Installation(t.Context(), db.ControllerSweep, func(ctx context.Context, w *db.Wide) error {
		var err error
		granted, err = w.IssueGrant(ctx, "finance", agk.TaskID(grantRun+"/render/1"), grantTaskRow,
			db.GrantScope{Run: grantRun, Step: "render", Workflow: workflow, Commit: commit},
			time.Now().UTC().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return granted.Clear
}

func TestAGrantTurnsIntoTheInputsTheArtifactsAndTheSecrets(t *testing.T) {
	g := withGrants(t, held{"finance/stripe": "sk_live_notreal"})
	credential := g.joined(t)
	clear, envelope, file := g.dispatched(t, []string{"stripe"})

	w, answer := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential,
		api.Redemption{Grant: clear, Task: agk.TaskID(grantRun + "/render/1")})
	if w.Code != http.StatusOK {
		t.Fatalf("redeeming answered %d: %s", w.Code, w.Body)
	}
	if answer["namespace"] != "finance" || answer["run"] != grantRun || answer["step"] != "render" {
		t.Fatalf("the grant answered %v", answer)
	}

	inputs, _ := answer["inputs"].([]any)
	if len(inputs) != 1 {
		t.Fatalf("the grant answered %d inputs", len(inputs))
	}
	first, _ := inputs[0].(map[string]any)
	if first["port"] != "in" || first["digest"] != envelope {
		t.Errorf("the input reads %v", first)
	}

	// The artifacts the envelope names are resolved here, because a runner that could name
	// a digest of its own would reach every object in the namespace.
	artifacts, _ := answer["artifacts"].([]any)
	if len(artifacts) != 1 {
		t.Fatalf("the grant answered %d artifacts: %v", len(artifacts), answer)
	}
	named, _ := artifacts[0].(map[string]any)
	if named["digest"] != file {
		t.Errorf("the artifact reads %v", named)
	}

	// And the URLs are the whole of the authorisation: they work, on their own.
	for _, u := range []any{first["url"], named["url"]} {
		raw, _ := u.(string)
		if res := follow(t, g.handler, "GET", raw, ""); res.Code != http.StatusOK {
			t.Errorf("following %s answered %d", raw, res.Code)
		}
	}

	secrets, _ := answer["secrets"].([]any)
	if len(secrets) != 1 {
		t.Fatalf("the grant answered %d secrets", len(secrets))
	}
	one, _ := secrets[0].(map[string]any)
	if one["name"] != "stripe" || one["value"] != "sk_live_notreal" {
		t.Errorf("the secret reads %v", one)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("an answer carrying every value the task was given says %q", w.Header().Get("Cache-Control"))
	}
}

// A PUT URL cannot be minted in advance, because the key of an object is the digest of bytes that
// do not exist yet. So the runner asks for one when it knows what it produced.
func TestARunnerAsksForSomewhereToPutWhatItMade(t *testing.T) {
	g := withGrants(t, api.NoSecrets{})
	credential := g.joined(t)
	clear, _, _ := g.dispatched(t, nil)

	const produced = "a rendered invoice"
	sum := sha256.Sum256([]byte(produced))
	digest := hex.EncodeToString(sum[:])

	w, answer := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential,
		api.Redemption{Grant: clear, Task: agk.TaskID(grantRun + "/render/1"), Upload: []string{digest}})
	if w.Code != http.StatusOK {
		t.Fatalf("redeeming answered %d: %s", w.Code, w.Body)
	}
	uploads, _ := answer["uploads"].([]any)
	if len(uploads) != 1 {
		t.Fatalf("the grant answered %d upload URLs", len(uploads))
	}
	one, _ := uploads[0].(map[string]any)
	url, _ := one["url"].(string)
	if res := follow(t, g.handler, "PUT", url, produced); res.Code != http.StatusCreated {
		t.Fatalf("storing what the task made answered %d", res.Code)
	}
	// And it stores that object and no other.
	if res := follow(t, g.handler, "PUT", url, "something else"); res.Code != http.StatusBadRequest {
		t.Errorf("storing other bytes under the same URL answered %d", res.Code)
	}
}

func TestWhatAGrantWillNotDo(t *testing.T) {
	g := withGrants(t, api.NoSecrets{})
	credential := g.joined(t)
	clear, _, _ := g.dispatched(t, nil)
	key := agk.TaskID(grantRun + "/render/1")

	// A value that is not a grant, and a grant for another task.
	for _, c := range []struct {
		name string
		ask  api.Redemption
	}{
		{"a value that opens nothing", api.Redemption{Grant: "agkgrant_notarealgrantatallbutlongenoughtopass", Task: key}},
		{"a grant redeemed for another task", api.Redemption{Grant: clear, Task: "01M2ZZZZZZZZZZZZZZZZZZZZZZ/other/1"}},
	} {
		w, _ := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, c.ask)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s answered %d", c.name, w.Code)
		}
	}

	// It is a runner route, so a principal's token reaches nothing.
	w, _ := call(t, g.handler, "POST", "/api/v1/tasks/redeem", "admin", api.Redemption{Grant: clear, Task: key})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("an administrator redeeming a grant answered %d", w.Code)
	}

	// The first machine to redeem holds the task, and a second is told so rather than
	// starting a container for it.
	if w, _ := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, api.Redemption{Grant: clear, Task: key}); w.Code != http.StatusOK {
		t.Fatalf("the first redemption answered %d", w.Code)
	}
	second := g.joined(t)
	w, _ = call(t, g.handler, "POST", "/api/v1/tasks/redeem", second, api.Redemption{Grant: clear, Task: key})
	if w.Code != http.StatusConflict {
		t.Errorf("a second machine redeeming the same grant answered %d", w.Code)
	}
}

// An installation with no secret provider holds nothing, and a task naming a secret fails in
// front of somebody rather than mounting an empty file.
func TestATaskNamingASecretNobodyHoldsFails(t *testing.T) {
	g := withGrants(t, api.NoSecrets{})
	credential := g.joined(t)
	clear, _, _ := g.dispatched(t, []string{"stripe"})

	w, answer := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential,
		api.Redemption{Grant: clear, Task: agk.TaskID(grantRun + "/render/1")})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a secret nobody holds answered %d: %s", w.Code, w.Body)
	}
	if said, _ := answer["error"].(string); said == "" || !strings.Contains(said, "stripe") {
		t.Errorf("the refusal does not name the secret: %v", answer)
	}
}

// wire compiles one definition out of the vendored wire schema, by JSON pointer, so that what is
// checked is the shape the schemas repository published rather than a copy of it written here.
func wire(t *testing.T, pointer string) *jsonschema.Schema {
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
	s, err := c.Compile("wire.schema.json#" + pointer)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The corpus first, so that a failure below is this package's and not the compiler's.
func TestTheVendoredRedemptionCorpusIsWhatItSaysItIs(t *testing.T) {
	s := wire(t, "/$defs/grantRedemption")
	cases, err := fixtures.GrantRedemptions()
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("the vendored redemption corpus holds nothing")
	}
	for _, c := range cases {
		body, err := fs.ReadFile(fixtures.FS, c.File)
		if err != nil {
			t.Fatal(err)
		}
		v, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		err = s.Validate(v)
		switch {
		case c.Valid && err != nil:
			t.Errorf("%s should be accepted: %s", c.File, err)
		case !c.Valid && err == nil:
			t.Errorf("%s should be refused: %s", c.File, c.Rule)
		}
	}
}

// "A runner still never speaks git and never holds a credential, because the controller resolves a
// commit to a tree and the runner fetches content-addressed objects with the task's grant, exactly
// as it fetches an artifact." So the tree a redemption answers is the one version the scope names:
// every file of it, at its mode, with a URL that fetches the bytes that were committed.
func TestAGrantAnswersTheTreeOfItsOwnVersion(t *testing.T) {
	g := withGrants(t, api.NoSecrets{})
	credential := g.joined(t)
	clear, _, _ := g.dispatched(t, nil)
	key := agk.TaskID(grantRun + "/render/1")

	// Another version of the same workflow, whose files the task must never be handed.
	other := map[string]api.PushFile{
		"agentiik.yaml":    {Content: []byte("apiVersion: agentiik.dev/v1\nkind: Workflow\n# later\n"), Mode: "0644"},
		"scripts/other.sh": {Content: []byte("#!/bin/sh\necho somewhere else\n"), Mode: "0755"},
	}
	g.recorded(t, "b4a0d2f", other)

	// And the runner has no way to ask for it: the version is what the controller wrote, and
	// a body naming one of its own is refused rather than half understood.
	w, _ := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential,
		map[string]any{"grant": clear, "task": key, "commit": "b4a0d2f"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("a redemption naming its own commit answered %d", w.Code)
	}

	w, answer := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, api.Redemption{Grant: clear, Task: key})
	if w.Code != http.StatusOK {
		t.Fatalf("redeeming answered %d: %s", w.Code, w.Body)
	}

	// The shape is the wire's, held to the schema the schemas repository publishes.
	encoded, err := json.Marshal(answer["tree"])
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if err := wire(t, "/$defs/grantRedemption/properties/response/properties/tree").Validate(v); err != nil {
		t.Errorf("the tree is not what the wire describes: %s", err)
	}

	committed := theTree()
	entries, _ := answer["tree"].([]any)
	var read []string
	urls := map[string]string{}
	for _, e := range entries {
		f, _ := e.(map[string]any)
		path, _ := f["path"].(string)
		mode, _ := f["mode"].(string)
		sum, _ := f["sha256"].(string)
		url, _ := f["url"].(string)
		read = append(read, path+" "+mode)
		urls[path] = url

		file, held := committed[path]
		if !held {
			t.Errorf("the tree answers %s, which the version does not hold", path)
			continue
		}
		if sum != digestOf(file.Content) {
			t.Errorf("%s is named by %s", path, sum)
		}
		// The URL is the whole of the authorisation, and what it fetches is the bytes
		// that were committed, not something near them.
		res := follow(t, g.handler, "GET", url, "")
		if res.Code != http.StatusOK {
			t.Errorf("fetching %s answered %d", path, res.Code)
			continue
		}
		if !bytes.Equal(res.Body.Bytes(), file.Content) {
			t.Errorf("fetching %s answered %q", path, res.Body.Bytes())
		}
	}
	if got, want := strings.Join(read, ", "), "agentiik.yaml 0644, scripts/again.sh 0755, scripts/render.sh 0755"; got != want {
		t.Errorf("the tree reads %s, want %s", got, want)
	}
	// Two files with the same bytes are one object, and a runner is handed one URL for it.
	if urls["scripts/again.sh"] != urls["scripts/render.sh"] {
		t.Error("two files of identical bytes were handed two URLs")
	}
	// Nothing of the other version is in it, neither a path nor the bytes behind one.
	for path, f := range other {
		for _, url := range urls {
			if strings.Contains(url, digestOf(f.Content)) {
				t.Errorf("the tree carries a URL for %s of the other version", path)
			}
		}
	}

	// And a grant whose scope names the other version is handed that version's tree: what
	// decides is the scope, and only the scope.
	again := g.grantedFor(t, "monthly-invoicing", "b4a0d2f")
	w, answer = call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, api.Redemption{Grant: again, Task: key})
	if w.Code != http.StatusOK {
		t.Fatalf("redeeming the second grant answered %d: %s", w.Code, w.Body)
	}
	entries, _ = answer["tree"].([]any)
	read = nil
	for _, e := range entries {
		f, _ := e.(map[string]any)
		path, _ := f["path"].(string)
		read = append(read, path)
	}
	if got := strings.Join(read, ", "); got != "agentiik.yaml, scripts/other.sh" {
		t.Errorf("the other version's tree reads %s", got)
	}
}

// A redemption that cannot say what the task's repository is refuses, with a sentence, rather than
// answering an empty tree: an empty /agk/repo is a directory that looks like a repository and is
// not one. None of these is the runner's doing, and none of them binds the task to it.
func TestARedemptionWithNoRepositoryToGiveRefuses(t *testing.T) {
	g := withGrants(t, api.NoSecrets{})
	credential := g.joined(t)
	key := agk.TaskID(grantRun + "/render/1")

	conn := dbtest.Superuser(t, g.super)
	if _, err := conn.Exec(t.Context(),
		`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at)
		 values ('finance','monthly-invoicing','c5b1e3a','{}','alice', now())`); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name             string
		workflow, commit string
		says             string
	}{
		{"a scope written before a scope named its version", "", "", "commit"},
		{"a version recorded without its tree", "monthly-invoicing", "c5b1e3a", "/agk/repo"},
		{"a version nobody recorded", "monthly-invoicing", "deadbee", "/agk/repo"},
	} {
		clear := g.grantedFor(t, c.workflow, c.commit)
		w, answer := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, api.Redemption{Grant: clear, Task: key})
		if w.Code != http.StatusInternalServerError {
			t.Errorf("%s answered %d: %s", c.name, w.Code, w.Body)
			continue
		}
		if said, _ := answer["error"].(string); !strings.Contains(said, c.says) {
			t.Errorf("%s was refused with %q", c.name, said)
		}
		if _, held := answer["tree"]; held {
			t.Errorf("%s answered a tree anyway", c.name)
		}

		var runner *string
		if err := conn.QueryRow(t.Context(), `select runner from tasks where id = $1`, grantTaskRow).Scan(&runner); err != nil {
			t.Fatal(err)
		}
		if runner != nil {
			t.Errorf("%s bound the task to %s, and a runner told there is no tree has not taken it", c.name, *runner)
		}
	}
}

func readerOf(s string) *strings.Reader { return strings.NewReader(s) }
