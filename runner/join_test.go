package runner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	server "github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/dockertest"
	"github.com/agentiik/agentiik/internal/fixtures"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// installation is the real API's runner routes on a real database, behind a server a host joins
// through, with one pool, dmz, carrying zone=dmz and arch=amd64.
type installation struct {
	url     string
	pool    *db.Pool
	runners *server.RunnerAPI
}

func anInstallation(t *testing.T) installation {
	t.Helper()
	pool, _ := dbtest.Open(t)
	if err := pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		return w.CreateRunnerPool(ctx, db.RunnerPool{Name: "dmz", Labels: []string{"zone=dmz", "arch=amd64"}, CreatedBy: "admin"})
	}); err != nil {
		t.Fatal(err)
	}
	rt, err := server.NewRouter(server.DenyAll{}, func(*http.Request) (server.Principal, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	runners, err := server.NewRunners(rt, server.RunnerOptions{Pool: pool})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	return installation{url: srv.URL, pool: pool, runners: runners}
}

// issue mints a join token for dmz permitting the labels given, as an administrator would.
func (in installation) issue(t *testing.T, labels ...string) Secret {
	t.Helper()
	var token db.JoinToken
	if err := in.pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		var err error
		now := time.Now().UTC()
		token, err = w.IssueJoinToken(ctx, "dmz", labels, "admin", now, now.Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return Secret(token.Clear)
}

// inventory is every runner the installation knows.
func (in installation) inventory(t *testing.T) []db.Runner {
	t.Helper()
	var runners []db.Runner
	if err := in.pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		var err error
		runners, err = w.Runners(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return runners
}

// aJoiningHost is a host about to join the API at url: its two directories empty, its memory the
// fixture's, its daemon a fake one, and its files given to the account running the test, which
// is the one account a test can give a file to without being root.
func aJoiningHost(t *testing.T, url string, token Secret) Joining {
	t.Helper()
	d, err := dockertest.NewDaemon()
	if err != nil {
		t.Fatalf("starting a fake daemon: %s", err)
	}
	t.Cleanup(func() { d.Close() })
	root := t.TempDir()
	vars := map[string]string{WorkDir: filepath.Join(root, "var", "work")}
	return Joining{
		API: url, Token: token, Labels: "zone=dmz",
		Lookup: environment(vars),
		Owner:  &Owner{UID: os.Getuid(), GID: os.Getgid()},
		// Neither directory exists yet, as on a host being installed.
		EnvPath: filepath.Join(root, "etc", "runner.env"),
		KeyPath: filepath.Join(root, "var", "runner.key"),
		MemInfo: filepath.Join("testdata", "meminfo"),
		Socket:  d.Socket(),
	}
}

// ownedAlone holds a file join wrote to mode 0600 and to the account it was given to.
func ownedAlone(t *testing.T, path string, owner *Owner) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Errorf("%s is %s, and join writes a file of mode 0600", path, info.Mode())
	}
	if uid, ok := ownerOf(info); ok && owner != nil && uid != owner.UID {
		t.Errorf("%s is owned by account %d, and join gave it to %d", path, uid, owner.UID)
	}
}

func TestAJoinCreatesTheRunnerAndWritesWhatServeReads(t *testing.T) {
	in := anInstallation(t)
	h := aJoiningHost(t, in.url, in.issue(t, "zone=dmz", "arch=amd64"))
	h.Lookup = environment(map[string]string{WorkDir: filepath.Join(filepath.Dir(h.KeyPath), "work"), Namespaces: "finance"})
	if os.Geteuid() == 0 {
		// Root gives the files away, which is what join does on a host being installed.
		h.Owner = &Owner{UID: 65534, GID: 65534}
	}

	joined, err := Join(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if joined.Pool != "dmz" || joined.Runner == "" || joined.RotateBy.Before(time.Now()) {
		t.Fatalf("the join came to %+v", joined)
	}
	ownedAlone(t, h.EnvPath, h.Owner)
	ownedAlone(t, h.KeyPath, h.Owner)
	if info, err := os.Stat(filepath.Dir(h.KeyPath)); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the key's directory was created as %v (%v), and it is the agent's alone", info.Mode(), err)
	}

	// What join wrote is what serve reads, read the way serve reads it.
	if os.Geteuid() == 0 {
		defer func(was func() int) { geteuid = was }(geteuid)
		geteuid = func() int { return 65534 }
	}
	c, err := ReadConfig(environment(nil), h.EnvPath)
	if err != nil {
		t.Fatalf("serve refuses what join wrote: %s", err)
	}
	if c.API != in.url || c.Runner != joined.Runner || c.Pool != "dmz" ||
		!slices.Equal(c.Labels, []string{"zone=dmz"}) || !slices.Equal(c.Namespaces, []string{"finance"}) {
		t.Errorf("serve reads %+v", c)
	}
	// And the credential it read is the one the API minted for that runner.
	r, err := in.runners.Runner(t.Context(), string(c.Credential))
	if err != nil || r.ID != joined.Runner {
		t.Errorf("the credential in runner.env opens %+v (%v)", r, err)
	}

	// The API holds what the host said of itself, and the key on disk is the one it sent.
	runners := in.inventory(t)
	if len(runners) != 1 {
		t.Fatalf("the installation has %d runners", len(runners))
	}
	got := runners[0]
	switch {
	case got.ID != joined.Runner || !slices.Equal(got.Labels, []string{"zone=dmz"}):
		t.Errorf("the runner is %s claiming %v", got.ID, got.Labels)
	case got.MemoryBytes != 16318196<<10:
		t.Errorf("the runner has %d bytes of memory, where meminfo says 16318196 kB", got.MemoryBytes)
	case got.DiskBytes <= 0 || got.CPU < 1:
		t.Errorf("the runner has %d vCPU and %d bytes of disk", got.CPU, got.DiskBytes)
	case got.Architecture != Architecture() || got.AgentVersion != Version():
		t.Errorf("the runner is %s running %s", got.Architecture, got.AgentVersion)
	case !slices.Equal(got.Namespaces, []string{"finance"}):
		t.Errorf("the runner narrows itself to %v", got.Namespaces)
	case got.Containment == nil || got.Containment.Runtime != "runc" || got.Containment.UsernsRemap:
		t.Errorf("the runner's containment is %+v, and the daemon runs runc without remapping", got.Containment)
	}
	private := readKey(t, h.KeyPath)
	if !private.Public().(ed25519.PublicKey).Equal(got.PublicKey) {
		t.Error("the key on disk is not the one the API holds for this runner")
	}
}

// readKey reads the key join wrote, as a rotation will.
func readKey(t *testing.T, path string) ed25519.PrivateKey {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(b)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("%s is not one PEM private key", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	private, ok := key.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("%s holds a %T", path, key)
	}
	return private
}

// written is every file under root, directories left out, so that a temporary file left behind
// is counted as much as a file moved into place.
func written(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	return files
}

func TestARefusedTokenWritesNothingKeyIncluded(t *testing.T) {
	in := anInstallation(t)
	for name, token := range map[string]Secret{
		// Well formed, and never issued.
		"a token nobody issued": Secret("agkjoin_" + strings.Repeat("x", 43)),
		// Issued, and not permitting a label the host claims.
		"a token not permitting the labels": in.issue(t, "arch=amd64"),
	} {
		t.Run(name, func(t *testing.T) {
			h := aJoiningHost(t, in.url, token)
			_, err := Join(t.Context(), h)
			if err == nil || !strings.Contains(err.Error(), "refused the join token") {
				t.Fatalf("the join answered %v", err)
			}
			root := filepath.Dir(filepath.Dir(h.EnvPath))
			if files := written(t, root); len(files) > 0 {
				t.Errorf("a refused join left %v", files)
			}
			if strings.Contains(err.Error(), string(token)) {
				t.Error("the refusal repeats the token")
			}
		})
	}
	if runners := in.inventory(t); len(runners) != 0 {
		t.Errorf("a refused join created %d runners", len(runners))
	}
}

// fakeAPI answers a join as the API does, and keeps what it was sent.
func fakeAPI(t *testing.T, answer string) (url string, sent func() []byte) {
	t.Helper()
	// Written on the server's goroutine and read on the test's, which a socket between them
	// does not order for the race detector.
	var body atomic.Pointer[[]byte]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/runners" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("the join carried Authorization %q, and it is authenticated by its token alone", r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		body.Store(&b)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []byte {
		if b := body.Load(); b != nil {
			return *b
		}
		return nil
	}
}

const (
	aToken  = Secret("agkjoin_lYd41PgjnpmnYTZ1KB2FvzKYJA7GgyKSqukAh9n7h9s")
	aJoined = `{"runner": "runner-dmz-02", "pool": "dmz", "credential": "` + credential + `", "rotate_by": "2026-12-09T06:12:00Z"}`
)

func TestTheJoinIsTheWiresRunnerRegistrationRequest(t *testing.T) {
	url, sent := fakeAPI(t, aJoined)
	h := aJoiningHost(t, url, aToken)
	h.Lookup = environment(map[string]string{WorkDir: t.TempDir(), Namespaces: "finance,team-ops"})
	if _, err := Join(t.Context(), h); err != nil {
		t.Fatal(err)
	}

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
	s, err := c.Compile("wire.schema.json#/$defs/runnerRegistration/properties/request")
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(sent()))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(v); err != nil {
		t.Errorf("the join the wire refuses: %s\n%s", err, sent())
	}

	var said map[string]any
	if err := json.Unmarshal(sent(), &said); err != nil {
		t.Fatal(err)
	}
	if said["token"] != string(aToken) || said["architecture"] != Architecture() {
		t.Errorf("the join sent %v", said)
	}
	if capacity, _ := said["capacity"].(map[string]any); capacity["memory"] != "16318196Ki" {
		t.Errorf("the join sent the capacity %v, where meminfo says 16318196 kB", said["capacity"])
	}
}

func TestAHostThatHasJoinedIsReplacedOnlyWhenAsked(t *testing.T) {
	url, _ := fakeAPI(t, aJoined)
	h := aJoiningHost(t, url, aToken)
	if _, err := Join(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	first := readKey(t, h.KeyPath)
	env, err := os.ReadFile(h.EnvPath)
	if err != nil {
		t.Fatal(err)
	}

	for _, gone := range []string{"", "runner.env", "runner.key"} {
		// Either file alone is an identity: a key without runner.env is a host whose join
		// failed after its key, and a runner.env without a key is one whose key is gone.
		host := h
		switch gone {
		case "runner.env":
			host.EnvPath = filepath.Join(t.TempDir(), "runner.env")
		case "runner.key":
			host.KeyPath = filepath.Join(t.TempDir(), "runner.key")
		}
		_, err := Join(t.Context(), host)
		if err == nil || !strings.Contains(err.Error(), "already joined") {
			t.Errorf("with %q gone, joining again answered %v", gone, err)
		}
	}
	if !readKey(t, h.KeyPath).Equal(first) {
		t.Error("a refused join replaced the key")
	}
	if now, _ := os.ReadFile(h.EnvPath); !bytes.Equal(now, env) {
		t.Error("a refused join replaced runner.env")
	}

	h.Replace = true
	if _, err := Join(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if readKey(t, h.KeyPath).Equal(first) {
		t.Error("a replaced identity kept its key, and a new runner is a new key")
	}
	ownedAlone(t, h.KeyPath, h.Owner)
	ownedAlone(t, h.EnvPath, h.Owner)
	if files := written(t, filepath.Dir(h.EnvPath)); len(files) != 1 {
		t.Errorf("replacing left %v", files)
	}
}

func TestAnAnswerServeCouldNotReadIsNeverWritten(t *testing.T) {
	for name, answer := range map[string]string{
		"a runner named outside the grammar":  `{"runner": "Runner 2", "pool": "dmz", "credential": "` + credential + `", "rotate_by": "2026-12-09T06:12:00Z"}`,
		"a credential that is not a runner's": `{"runner": "runner-dmz-02", "pool": "dmz", "credential": "agkjoin_` + strings.Repeat("x", 43) + `", "rotate_by": "2026-12-09T06:12:00Z"}`,
		"a credential spanning two lines":     `{"runner": "runner-dmz-02", "pool": "dmz", "credential": "` + credential + `\nAGK_API=https://elsewhere.example.com", "rotate_by": "2026-12-09T06:12:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			url, _ := fakeAPI(t, answer)
			h := aJoiningHost(t, url, aToken)
			_, err := Join(t.Context(), h)
			if err == nil || !strings.Contains(err.Error(), "spent the token") {
				t.Fatalf("the join answered %v", err)
			}
			if files := written(t, filepath.Dir(filepath.Dir(h.EnvPath))); len(files) > 0 {
				t.Errorf("an answer serve could not read left %v", files)
			}
			if strings.Contains(err.Error(), credential) {
				t.Error("the refusal repeats the credential")
			}
		})
	}
}

func TestAJoinIsRefusedBeforeAnythingIsSentWhereItsSettingsAreWrong(t *testing.T) {
	for name, c := range map[string]struct {
		change func(*Joining)
		names  string
	}{
		"no address":             {func(j *Joining) { j.API = "" }, "--api"},
		"a plaintext address":    {func(j *Joining) { j.API = "http://agentiik.example.com" }, API},
		"an address with a $":    {func(j *Joining) { j.API = "https://agentiik.example.com/$x" }, API},
		"no labels":              {func(j *Joining) { j.Labels = "" }, "--labels"},
		"a label out of grammar": {func(j *Joining) { j.Labels = "zone dmz" }, Labels},
		"no token":               {func(j *Joining) { j.Token = "" }, "--token"},
		"a runner credential":    {func(j *Joining) { j.Token = credential }, "--token"},
		"no daemon":              {func(j *Joining) { j.Socket = filepath.Join(t.TempDir(), "docker.sock") }, "daemon"},
		"no meminfo":             {func(j *Joining) { j.MemInfo = filepath.Join(t.TempDir(), "meminfo") }, "memory"},
	} {
		t.Run(name, func(t *testing.T) {
			url, sent := fakeAPI(t, aJoined)
			h := aJoiningHost(t, url, aToken)
			c.change(&h)
			_, err := Join(t.Context(), h)
			if err == nil || !strings.Contains(err.Error(), c.names) {
				t.Fatalf("the join answered %v, and should have refused naming %s", err, c.names)
			}
			if sent() != nil {
				t.Error("the join reached the API")
			}
			if files := written(t, filepath.Dir(filepath.Dir(h.EnvPath))); len(files) > 0 {
				t.Errorf("a refused join left %v", files)
			}
		})
	}
}

// The labels come from the command line, and from AGK_RUNNER_LABELS where it gives none.
func TestTheLabelsAreTheCommandLinesOrTheEnvironments(t *testing.T) {
	url, sent := fakeAPI(t, aJoined)
	h := aJoiningHost(t, url, aToken)
	h.Labels = ""
	h.Lookup = environment(map[string]string{Labels: "zone=dmz,arch=amd64", WorkDir: t.TempDir()})
	if _, err := Join(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	var said struct{ Labels []string }
	json.Unmarshal(sent(), &said)
	if !slices.Equal(said.Labels, []string{"zone=dmz", "arch=amd64"}) {
		t.Errorf("the join claimed %v", said.Labels)
	}
	c, err := ReadConfig(environment(nil), h.EnvPath)
	if err != nil || !slices.Equal(c.Labels, said.Labels) {
		t.Errorf("runner.env carries the labels %v (%v)", c.Labels, err)
	}
}

// A join the API did not answer says nothing was written, and that the token may be spent.
func TestAJoinTheAPIDidNotAnswerSaysTheTokenMayBeSpent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error": "the runner could not be created"}`)
	}))
	t.Cleanup(srv.Close)
	h := aJoiningHost(t, srv.URL, aToken)
	_, err := Join(t.Context(), h)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "Nothing was written") {
		t.Fatalf("the join answered %v", err)
	}
	if files := written(t, filepath.Dir(filepath.Dir(h.EnvPath))); len(files) > 0 {
		t.Errorf("a join the API did not answer left %v", files)
	}
}
