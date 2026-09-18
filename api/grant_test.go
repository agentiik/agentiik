package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
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
	objects artifact.Objects
	signed  *artifact.Signed
}

func withGrants(t *testing.T, secrets api.Secrets) grants {
	t.Helper()
	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	for _, stmt := range []string{
		`insert into namespaces (name) values ('finance')`,
		`insert into workflows (namespace, name) values ('finance','monthly-invoicing')`,
		`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at)
		   values ('finance','monthly-invoicing','a3f9c1e','{}','alice', now())`,
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

	objects := artifact.Dir(t.TempDir())
	signed, err := artifact.NewSigned(objects, artifact.SignedOptions{
		Key: []byte("0123456789abcdef0123456789abcdef"), Base: "https://agentiik.example.com/api/v1/objects",
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
	return grants{handler: rt, pool: pool, objects: objects, signed: signed}
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
				Inputs:  []db.GrantInput{{Port: "in", Digest: envelope, Items: 1}},
				Secrets: secrets,
			}, time.Now().UTC().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return granted.Clear, envelope, file
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

func readerOf(s string) *strings.Reader { return strings.NewReader(s) }
