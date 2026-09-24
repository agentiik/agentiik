package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"slices"
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
const grantKey agk.TaskID = grantRun + "/render/1"

// asking is the redemption a runner holding the grant sends: the grant, the row it names, and the
// key of the attempt it was dispatched for.
func asking(clear string) api.Redemption {
	return api.Redemption{Grant: clear, TaskID: grantTaskRow, IdempotencyKey: grantKey}
}

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

	// clock is the instant the store checks a signature at, which a test moves to see what a
	// redemption answered stop working when the grant does. It is the wall clock while zero.
	clock *time.Time
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

	clock := new(time.Time)
	signed, err := artifact.NewSigned(objects, artifact.SignedOptions{
		Key: []byte("0123456789abcdef0123456789abcdef"), Base: "https://agentiik.example.com/objects",
		Now: func() time.Time {
			if clock.IsZero() {
				return time.Now().UTC()
			}
			return *clock
		},
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
	g.handler, g.signed, g.clock = rt, signed, clock
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
		now := time.Now().UTC()
		token, err = w.IssueJoinToken(ctx, "dmz", nil, "admin", now, now.Add(time.Hour))
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
// artifact itself, and the grant that says the task may have both, with each secret at the path
// its name puts it.
func (g grants) dispatched(t *testing.T, secrets []string) (clear, envelope, file string) {
	t.Helper()
	mounts := make([]db.GrantSecret, 0, len(secrets))
	for _, name := range secrets {
		mounts = append(mounts, db.GrantSecret{Name: name, Mount: "/agk/secrets/" + name})
	}
	return g.dispatchedWith(t, mounts)
}

// dispatchedWith is dispatched with each secret's mount said rather than derived, which is what a
// brick manifest asking for a secret somewhere else under /agk/secrets/ comes to.
func (g grants) dispatchedWith(t *testing.T, secrets []db.GrantSecret) (clear, envelope, file string) {
	t.Helper()
	envelope, file = g.enveloped(t, "collect", "out", "the whole of an invoice")
	clear = g.granted(t, db.GrantScope{
		Run: grantRun, Step: "render",
		Workflow: "monthly-invoicing", Commit: "a3f9c1e",
		Inputs:  []db.GrantInput{{Port: "in", Digest: envelope, Items: 1}},
		Secrets: secrets,
	})
	return clear, envelope, file
}

// enveloped stores a file and the envelope a step published on one port naming it, as the
// controller finds them when it dispatches, and answers the digest of each.
func (g grants) enveloped(t *testing.T, step agk.Step, port agk.Port, content string) (envelope, file string) {
	t.Helper()
	return g.envelopedAs(t, step, port, content, "invoice.pdf")
}

// envelopedAs is enveloped with one item for each name, every item attaching the same bytes
// under the name it is given.
func (g grants) envelopedAs(t *testing.T, step agk.Step, port agk.Port, content string, names ...string) (envelope, file string) {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	file = hex.EncodeToString(sum[:])
	if err := g.objects.Put(t.Context(), artifact.Key("finance", file), readerOf(content)); err != nil {
		t.Fatal(err)
	}

	e := agk.Envelope{
		Meta: agk.Meta{RunID: grantRun, Step: step, Port: port, Attempt: 1, Count: len(names), ProducedAt: time.Now().UTC()},
	}
	for _, name := range names {
		item := agk.NewItem(map[string]any{"total": 42})
		item.Files = []agk.File{{
			Name:      name,
			URI:       agk.URI{Run: grantRun, Step: step, Port: port, Name: name},
			MediaType: "application/pdf", Size: int64(len(content)), SHA256: file,
		}}
		e.Items = append(e.Items, item)
	}
	envelope, _, err := artifact.PutEnvelope(t.Context(), g.objects, "finance", e)
	if err != nil {
		t.Fatal(err)
	}
	return envelope, file
}

// granted issues the task's grant with the scope a controller wrote, and answers its clear value.
func (g grants) granted(t *testing.T, scope db.GrantScope) string {
	t.Helper()
	var granted db.Granted
	if err := g.pool.Installation(t.Context(), db.ControllerSweep, func(ctx context.Context, w *db.Wide) error {
		var err error
		granted, err = w.IssueGrant(ctx, "finance", grantKey, grantTaskRow, scope, time.Now().UTC().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return granted.Clear
}

// grantedFor issues the task's grant again, naming one version and nothing else, which is what a
// controller does when it writes the scope. What the version is decides what the runner is given.
func (g grants) grantedFor(t *testing.T, workflow, commit string) string {
	t.Helper()
	return g.granted(t, db.GrantScope{Run: grantRun, Step: "render", Workflow: workflow, Commit: commit})
}

// redeemed is a redemption that has to succeed, read into the shape the API answers.
func (g grants) redeemed(t *testing.T, credential string, ask api.Redemption) api.Grant {
	t.Helper()
	w, _ := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, ask)
	if w.Code != http.StatusOK {
		t.Fatalf("redeeming answered %d: %s", w.Code, w.Body)
	}
	var answer api.Grant
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	return answer
}

// bound is the runner a task is held by, or nothing when no redemption has taken it.
func (g grants) bound(t *testing.T) *string {
	t.Helper()
	var runner *string
	if err := dbtest.Superuser(t, g.super).QueryRow(t.Context(),
		`select runner from tasks where id = $1`, grantTaskRow).Scan(&runner); err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestAGrantTurnsIntoTheInputsTheArtifactsAndTheSecrets(t *testing.T) {
	g := withGrants(t, held{"finance/stripe": "sk_live_notreal"})
	credential := g.joined(t)
	clear, envelope, file := g.dispatched(t, []string{"stripe"})

	w, _ := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, asking(clear))
	if w.Code != http.StatusOK {
		t.Fatalf("redeeming answered %d: %s", w.Code, w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("an answer carrying every value the task was given says %q", w.Header().Get("Cache-Control"))
	}
	var answer api.Grant
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	// The row the grant was issued for, echoed back, so that a runner holding several
	// tasks knows which container this document belongs to.
	if answer.TaskID != grantTaskRow {
		t.Errorf("the grant answered for task %q", answer.TaskID)
	}

	if len(answer.Inputs) != 1 {
		t.Fatalf("the grant answered %d inputs", len(answer.Inputs))
	}
	in := answer.Inputs[0]
	if in.Port != "in" || in.Envelope.Digest != "sha256:"+envelope {
		t.Errorf("the input reads %+v", in)
	}

	// The artifacts the envelope names are resolved here, because a runner that could name
	// a digest of its own would reach every object in the namespace.
	if len(in.Artifacts) != 1 {
		t.Fatalf("the input carries %d artifacts: %+v", len(in.Artifacts), in)
	}
	if in.Artifacts[0].SHA256 != file {
		t.Errorf("the artifact reads %+v", in.Artifacts[0])
	}

	// And the URLs are the whole of the authorisation: they work, on their own.
	for _, raw := range []string{in.Envelope.URL, in.Artifacts[0].URL} {
		if res := follow(t, g.handler, "GET", raw, ""); res.Code != http.StatusOK {
			t.Errorf("following %s answered %d", raw, res.Code)
		}
	}

	want := []api.Secret{{Name: "stripe", Mount: "/agk/secrets/stripe", Encoding: "utf-8", Value: "sk_live_notreal"}}
	if !slices.Equal(answer.Secrets, want) {
		t.Errorf("the secrets read %+v", answer.Secrets)
	}
}

// The key of an object is the digest of bytes that do not exist when a task starts, so the runner
// is told where to write before it has made anything: one policy, at the first redemption, which
// stores whatever the task makes under its namespace's prefix and nothing anywhere else.
func TestARunnerIsToldWhereToWriteBeforeItHasMadeAnything(t *testing.T) {
	g := withGrants(t, api.NoSecrets{})
	credential := g.joined(t)
	clear, _, _ := g.dispatched(t, nil)

	answer := g.redeemed(t, credential, asking(clear))
	uploads := answer.Uploads
	if uploads.URL != "https://agentiik.example.com/objects/finance" || uploads.KeyPrefix != "finance/sha256/" {
		t.Errorf("the task is told to write to %s under %s", uploads.URL, uploads.KeyPrefix)
	}

	// One policy for everything the task makes, whatever that turns out to be.
	for _, produced := range []string{"a rendered invoice", "the envelope that names it"} {
		key := uploads.KeyPrefix + digestOf([]byte(produced))
		if w := posted(t, g.handler, uploads.URL, uploads.Fields, key, produced); w.Code != http.StatusCreated {
			t.Fatalf("storing %q answered %d", produced, w.Code)
		}
		if held, err := g.objects.Has(t.Context(), key); err != nil || !held {
			t.Errorf("%s is not held once it was stored: %v", key, err)
		}
	}
	// And the object its key names and no other.
	other := uploads.KeyPrefix + digestOf([]byte("an invoice nobody rendered"))
	if w := posted(t, g.handler, uploads.URL, uploads.Fields, other, "something else"); w.Code != http.StatusBadRequest {
		t.Errorf("storing other bytes than the key names answered %d", w.Code)
	}

	// Never in another namespace, whether the key names one or the form is posted to one.
	elsewhere := artifact.Key("ops", digestOf([]byte("a rendered invoice")))
	for _, to := range []string{uploads.URL, "https://agentiik.example.com/objects/ops"} {
		if w := posted(t, g.handler, to, uploads.Fields, elsewhere, "a rendered invoice"); w.Code != http.StatusForbidden {
			t.Errorf("storing into another namespace through %s answered %d", to, w.Code)
		}
	}

	// Signed for the run the grant was issued in. Nothing over HTTP shows it, since the store keeps
	// an object for its namespace and not for a run, so it is read back out of the policy.
	fields := url.Values{}
	for name, value := range uploads.Fields {
		fields.Set(name, value)
	}
	if run, err := g.signed.CheckPolicy("finance", uploads.KeyPrefix+digestOf([]byte("a rendered invoice")), fields); err != nil || run != grantRun {
		t.Errorf("the policy is signed for run %q: %v", run, err)
	}

	// And good for as long as the grant and no longer. Outputs are written once the container
	// has exited, which may be in the grant's last minute, and nothing written after it is the
	// task's to write.
	ends, err := time.Parse(time.RFC3339Nano, answer.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		at   time.Time
		want int
	}{
		{"a second before the grant ends", ends.Add(-time.Second), http.StatusCreated},
		{"once the grant has ended", ends, http.StatusForbidden},
	} {
		*g.clock = c.at
		produced := "an invoice rendered " + c.name
		if w := posted(t, g.handler, uploads.URL, uploads.Fields, uploads.KeyPrefix+digestOf([]byte(produced)), produced); w.Code != c.want {
			t.Errorf("storing %s answered %d", c.name, w.Code)
		}
	}
}

func TestWhatAGrantWillNotDo(t *testing.T) {
	g := withGrants(t, api.NoSecrets{})
	credential := g.joined(t)
	clear, _, _ := g.dispatched(t, nil)

	// A body naming its task by anything less than both the row and the key is refused before
	// anything is read, and so is the shape this route took before it took the wire's: the
	// grant is checked against both, and a comparison with nothing is no comparison.
	for _, c := range []struct {
		name string
		body any
	}{
		{"a body naming its task as task", map[string]any{"grant": clear, "task": grantKey}},
		{"a body naming what it wants to upload", map[string]any{
			"grant": clear, "task_id": grantTaskRow, "idempotency_key": grantKey,
			"upload": []string{strings.Repeat("a", 64)},
		}},
		{"a body with no task_id", api.Redemption{Grant: clear, IdempotencyKey: grantKey}},
		{"a body with no idempotency_key", api.Redemption{Grant: clear, TaskID: grantTaskRow}},
	} {
		if w, _ := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, c.body); w.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d", c.name, w.Code)
		}
	}

	// A value that is not a grant, and a grant presented for another task, by its row or by
	// the key of another attempt, are one refusal, and none of them takes the task.
	for _, c := range []struct {
		name string
		ask  api.Redemption
	}{
		{"a value that opens nothing", api.Redemption{Grant: "agkgrant_notarealgrantatallbutlongenoughtopass", TaskID: grantTaskRow, IdempotencyKey: grantKey}},
		{"a grant redeemed for another task's row", api.Redemption{Grant: clear, TaskID: "01M2ZZZZZZZZZZZZZZZZZZZZZZ", IdempotencyKey: grantKey}},
		{"a grant redeemed for another attempt", api.Redemption{Grant: clear, TaskID: grantTaskRow, IdempotencyKey: grantRun + "/render/2"}},
		{"a grant redeemed for another task", api.Redemption{Grant: clear, TaskID: grantTaskRow, IdempotencyKey: "01M2ZZZZZZZZZZZZZZZZZZZZZZ/other/1"}},
	} {
		w, _ := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, c.ask)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s answered %d", c.name, w.Code)
		}
		if runner := g.bound(t); runner != nil {
			t.Fatalf("%s bound the task to %s", c.name, *runner)
		}
	}

	// It is a runner route, so a principal's token reaches nothing.
	w, _ := call(t, g.handler, "POST", "/api/v1/tasks/redeem", "admin", asking(clear))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("an administrator redeeming a grant answered %d", w.Code)
	}

	// So the task is still there for the first machine to redeem it properly, and the one
	// refused above is then told the work is somebody else's rather than starting a
	// container for it.
	second := g.joined(t)
	g.redeemed(t, second, asking(clear))
	w, _ = call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, asking(clear))
	if w.Code != http.StatusConflict {
		t.Errorf("a machine redeeming a grant another has redeemed answered %d", w.Code)
	}

	// But only when the body agrees with the grant. One that names another row or another
	// attempt is the refusal above whatever the task is doing, and neither half of it is told
	// whose the work is.
	for _, c := range []struct {
		name string
		ask  api.Redemption
	}{
		{"another task's row", api.Redemption{Grant: clear, TaskID: "01M2ZZZZZZZZZZZZZZZZZZZZZZ", IdempotencyKey: grantKey}},
		{"another attempt", api.Redemption{Grant: clear, TaskID: grantTaskRow, IdempotencyKey: grantRun + "/render/2"}},
	} {
		if w, _ := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, c.ask); w.Code != http.StatusUnauthorized {
			t.Errorf("a grant another machine holds, redeemed for %s, answered %d", c.name, w.Code)
		}
	}
}

// An installation with no secret provider holds nothing, and a task naming a secret fails in
// front of somebody rather than mounting an empty file. That is also what an installation that
// attached no store at all gets, since the runner half's options default to holding nothing.
func TestATaskNamingASecretNobodyHoldsFails(t *testing.T) {
	for what, secrets := range map[string]api.Secrets{"NoSecrets": api.NoSecrets{}, "nothing attached": nil} {
		t.Run(what, func(t *testing.T) {
			g := withGrants(t, secrets)
			credential := g.joined(t)
			clear, _, _ := g.dispatched(t, []string{"stripe"})

			w, answer := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, asking(clear))
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("a secret nobody holds answered %d: %s", w.Code, w.Body)
			}
			if said, _ := answer["error"].(string); said == "" || !strings.Contains(said, "stripe") {
				t.Errorf("the refusal does not name the secret: %v", answer)
			}
		})
	}
}

// unreadable is a store that holds every secret it is asked for and cannot read one, the way a
// value sealed under a master key the ring no longer holds is.
type unreadable struct{}

func (unreadable) Value(_ context.Context, namespace, name string) ([]byte, error) {
	return nil, fmt.Errorf("secret: %s/%s was sealed under a master key this installation's keyring does not hold", namespace, name)
}

// A secret the store holds and could not read is told apart from one it does not hold. The runner
// is told which secret and which of the two, and the store's reason goes to whoever runs the
// installation, naming the task and the secret, since they are the one who can act on it.
func TestWhyASecretWasNotGivenGoesToTheInstallation(t *testing.T) {
	for what, c := range map[string]struct {
		secrets    api.Secrets
		says, not  string
		reasonSays string
	}{
		"a secret nobody holds":             {api.NoSecrets{}, "is not held", "could not be read", "no secret of that name"},
		"a secret held that cannot be read": {unreadable{}, "could not be read", "is not held", "keyring does not hold"},
	} {
		t.Run(what, func(t *testing.T) {
			g := withGrants(t, api.NoSecrets{})
			var told []error
			rt := router(t, everything{who: "admin"})
			if _, err := api.NewRunners(rt, api.RunnerOptions{
				Pool: g.pool, Objects: g.objects, URLs: g.signed, Secrets: c.secrets,
				Trouble: func(err error) { told = append(told, err) },
			}); err != nil {
				t.Fatal(err)
			}
			g.handler = rt
			credential := g.joined(t)
			clear, _, _ := g.dispatched(t, []string{"stripe"})

			w, answer := call(t, rt, "POST", "/api/v1/tasks/redeem", credential, asking(clear))
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("%s answered %d: %s", what, w.Code, w.Body)
			}
			said, _ := answer["error"].(string)
			if !strings.Contains(said, "stripe") || !strings.Contains(said, c.says) || strings.Contains(said, c.not) || strings.Contains(said, c.reasonSays) {
				t.Errorf("the runner was told %q", said)
			}
			if len(told) != 1 {
				t.Fatalf("the installation was told %v", told)
			}
			if reason := told[0].Error(); !strings.Contains(reason, "finance/stripe") || !strings.Contains(reason, grantTaskRow) || !strings.Contains(reason, c.reasonSays) {
				t.Errorf("the installation was told %q", reason)
			}
		})
	}
}

// A secret is a file's worth of bytes and not always text, and a JSON string cannot carry bytes
// that are not UTF-8: a keystore sent as one would arrive as a different file. So a value that is
// not text travels as base64 and says so, and one that is travels as itself.
func TestASecretThatIsNotTextTravelsAsBase64(t *testing.T) {
	// Four bytes, which do not fill whole groups of three, so that the value is padded: a
	// runner decoding with the standard alphabet refuses one that is not, and the task would
	// stop with its secret unmounted.
	keystore := []byte{0xff, 0xfe, 0x00, 0x01}
	const pem = "-----BEGIN PRIVATE KEY-----\nbm90IGEga2V5\n-----END PRIVATE KEY-----\n"
	g := withGrants(t, held{"finance/keystore": string(keystore), "finance/tls-key": pem})
	credential := g.joined(t)
	clear, _, _ := g.dispatched(t, []string{"keystore", "tls-key"})

	answer := g.redeemed(t, credential, asking(clear))
	secrets := map[string]api.Secret{}
	for _, s := range answer.Secrets {
		secrets[s.Name] = s
	}

	binary := secrets["keystore"]
	if binary.Encoding != "base64" || binary.Value != "//4AAQ==" {
		t.Errorf("a value that is not text travels as %q %q", binary.Encoding, binary.Value)
	}
	if decoded, err := base64.StdEncoding.DecodeString(binary.Value); err != nil || !bytes.Equal(decoded, keystore) {
		t.Errorf("a value that is not text decodes to %x (%v), and the store holds %x", decoded, err, keystore)
	}

	text := secrets["tls-key"]
	if text.Encoding != "utf-8" || text.Value != pem {
		t.Errorf("a value that is text travels as %q %q", text.Encoding, text.Value)
	}
}

// A brick manifest may ask for a secret somewhere other than where its name would put it, and only
// the controller read the manifest. So a value arrives with the mount the grant was written with,
// and never with one the API worked out from the name.
func TestASecretArrivesWithTheMountItWasDispatchedWith(t *testing.T) {
	g := withGrants(t, held{"finance/billing": "bk_live_notreal"})
	credential := g.joined(t)
	clear, _, _ := g.dispatchedWith(t, []db.GrantSecret{{Name: "billing", Mount: "/agk/secrets/api-key"}})

	answer := g.redeemed(t, credential, asking(clear))
	want := []api.Secret{{Name: "billing", Mount: "/agk/secrets/api-key", Encoding: "utf-8", Value: "bk_live_notreal"}}
	if !slices.Equal(answer.Secrets, want) {
		t.Errorf("the secrets read %+v", answer.Secrets)
	}
}

// The runner pairs an artifact with the file entry it read by the URI the envelope writes, so each
// port carries the artifacts of its own envelope. The same bytes named from two ports are listed
// under both, each by the name that port's envelope gives them.
func TestEachPortCarriesTheArtifactsItsEnvelopeNames(t *testing.T) {
	g := withGrants(t, api.NoSecrets{})
	credential := g.joined(t)
	const content = "the whole of an invoice"
	collected, file := g.enveloped(t, "collect", "out", content)
	split, _ := g.enveloped(t, "split", "ok", content)
	clear := g.granted(t, db.GrantScope{
		Run: grantRun, Step: "render", Workflow: "monthly-invoicing", Commit: "a3f9c1e",
		Inputs: []db.GrantInput{
			{Port: "in", Digest: collected, Items: 1},
			{Port: "orders", Digest: split, Items: 1},
		},
	})

	answer := g.redeemed(t, credential, asking(clear))
	if len(answer.Inputs) != 2 {
		t.Fatalf("the grant answered %d inputs: %+v", len(answer.Inputs), answer.Inputs)
	}
	for i, want := range []struct {
		port     agk.Port
		envelope string
		uri      string
	}{
		{"in", collected, "agk://run/" + grantRun + "/collect/out/invoice.pdf"},
		{"orders", split, "agk://run/" + grantRun + "/split/ok/invoice.pdf"},
	} {
		in := answer.Inputs[i]
		if in.Port != want.port || in.Envelope.Digest != "sha256:"+want.envelope {
			t.Errorf("input %d reads %+v", i, in)
			continue
		}
		// The envelope is the one the digest names, which is what the runner holds the
		// transfer against.
		res := follow(t, g.handler, "GET", in.Envelope.URL, "")
		if sum := sha256.Sum256(res.Body.Bytes()); res.Code != http.StatusOK || hex.EncodeToString(sum[:]) != want.envelope {
			t.Errorf("the envelope on %s answered %d and hashes to %x", want.port, res.Code, sum)
		}

		if len(in.Artifacts) != 1 {
			t.Errorf("%s carries %d artifacts: %+v", want.port, len(in.Artifacts), in.Artifacts)
			continue
		}
		a := in.Artifacts[0]
		if a.URI.String() != want.uri || a.SHA256 != file {
			t.Errorf("%s carries %s %s", want.port, a.URI, a.SHA256)
		}
		if res := follow(t, g.handler, "GET", a.URL, ""); res.Code != http.StatusOK || res.Body.String() != content {
			t.Errorf("following the artifact on %s answered %d: %q", want.port, res.Code, res.Body)
		}
	}
}

// The same holds inside one envelope: an envelope may attach one content under two names, and the
// runner looks each name up by its URI. So each name is an entry of its own, both fetching the
// same bytes, and a name that two items attach is still one entry.
func TestTheSameBytesUnderTwoNamesAreListedUnderBoth(t *testing.T) {
	g := withGrants(t, api.NoSecrets{})
	credential := g.joined(t)
	const content = "the whole of an invoice"
	envelope, file := g.envelopedAs(t, "collect", "out", content, "a.pdf", "b.pdf", "a.pdf")
	clear := g.granted(t, db.GrantScope{
		Run: grantRun, Step: "render", Workflow: "monthly-invoicing", Commit: "a3f9c1e",
		Inputs: []db.GrantInput{{Port: "in", Digest: envelope, Items: 3}},
	})

	answer := g.redeemed(t, credential, asking(clear))
	if len(answer.Inputs) != 1 {
		t.Fatalf("the grant answered %d inputs: %+v", len(answer.Inputs), answer.Inputs)
	}
	var listed []string
	for _, a := range answer.Inputs[0].Artifacts {
		listed = append(listed, a.URI.String())
		if a.SHA256 != file {
			t.Errorf("%s is named by %s", a.URI, a.SHA256)
		}
		if res := follow(t, g.handler, "GET", a.URL, ""); res.Code != http.StatusOK || res.Body.String() != content {
			t.Errorf("following %s answered %d: %q", a.URI, res.Code, res.Body)
		}
	}
	under := "agk://run/" + grantRun + "/collect/out/"
	if got, want := strings.Join(listed, ", "), under+"a.pdf, "+under+"b.pdf"; got != want {
		t.Errorf("the port lists %s, want %s", got, want)
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

// conforms says whether a value, as it would be written on the wire, is what one definition of the
// vendored wire schema describes.
func conforms(t *testing.T, pointer string, value any) error {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	return wire(t, pointer).Validate(v)
}

// A redemption is held to the wire whole, because the wire is what a runner is written against:
// what a runner sends and all of what it is answered, uploads included.
func TestARedemptionIsWhatTheWireDescribes(t *testing.T) {
	// A runner written from the wire is understood: the request of every valid fixture reads
	// as a redemption, with no field in it the API does not know.
	cases, err := fixtures.GrantRedemptions()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if !c.Valid {
			continue
		}
		body, err := fs.ReadFile(fixtures.FS, c.File)
		if err != nil {
			t.Fatal(err)
		}
		var pair struct {
			Request json.RawMessage `json:"request"`
		}
		if err := json.Unmarshal(body, &pair); err != nil {
			t.Fatal(err)
		}
		d := json.NewDecoder(bytes.NewReader(pair.Request))
		d.DisallowUnknownFields()
		var ask api.Redemption
		if err := d.Decode(&ask); err != nil {
			t.Errorf("the request of %s is not a redemption the API reads: %s", c.File, err)
			continue
		}
		if ask.Grant == "" || ask.TaskID == "" || ask.IdempotencyKey == "" {
			t.Errorf("the request of %s reads as %+v", c.File, ask)
		}
	}

	g := withGrants(t, held{"finance/billing": "bk_live_notreal"})
	credential := g.joined(t)
	clear, _, _ := g.dispatched(t, []string{"billing"})

	ask := asking(clear)
	w, answer := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, ask)
	if w.Code != http.StatusOK {
		t.Fatalf("redeeming answered %d: %s", w.Code, w.Body)
	}
	// The exchange as the wire writes it, request and response together against the one
	// definition. The response is closed and requires every part it names, so this is also
	// the answer carrying nothing the wire leaves out and leaving out nothing it names, which a
	// runner reading it strictly would refuse the document for.
	if err := conforms(t, "/$defs/grantRedemption", map[string]any{"request": ask, "response": answer}); err != nil {
		t.Errorf("the redemption is not what the wire describes: %s", err)
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

	// Another version of the same workflow, whose files the task must never be handed.
	other := map[string]api.PushFile{
		"agentiik.yaml":    {Content: []byte("apiVersion: agentiik.dev/v1\nkind: Workflow\n# later\n"), Mode: "0644"},
		"scripts/other.sh": {Content: []byte("#!/bin/sh\necho somewhere else\n"), Mode: "0755"},
	}
	g.recorded(t, "b4a0d2f", other)

	// And the runner has no way to ask for it: the version is what the controller wrote, and
	// a body naming one of its own is refused rather than half understood.
	w, _ := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential,
		map[string]any{"grant": clear, "task_id": grantTaskRow, "idempotency_key": grantKey, "commit": "b4a0d2f"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("a redemption naming its own commit answered %d", w.Code)
	}

	w, answer := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, asking(clear))
	if w.Code != http.StatusOK {
		t.Fatalf("redeeming answered %d: %s", w.Code, w.Body)
	}

	// The shape is the wire's, held to the schema the schemas repository publishes.
	if err := conforms(t, "/$defs/grantRedemption/properties/response/properties/tree", answer["tree"]); err != nil {
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
	w, answer = call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, asking(again))
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
		w, answer := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, asking(clear))
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

		if runner := g.bound(t); runner != nil {
			t.Errorf("%s bound the task to %s, and a runner told there is no tree has not taken it", c.name, *runner)
		}
	}
}

func readerOf(s string) *strings.Reader { return strings.NewReader(s) }
