package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/secret"
	"github.com/agentiik/agentiik/version"
	"github.com/jackc/pgx/v5"
	natsserver "github.com/nats-io/nats-server/v2/server"
)

// An installation as a person makes one, against a real PostgreSQL and a real NATS: migrate a
// database nobody prepared, create the bus identity with bus-init, start a NATS server on the
// configuration it wrote, and serve. Then the operator pushes, starts a run and makes a pool, a
// machine joins it, heartbeats and gets a bus credential the server takes, and nobody without the
// token gets anywhere.
//
// serve is handed its settings rather than reading them, because the test's database and bus
// speak plaintext and config.ReadAPI refuses both, as it should: what reading refuses is
// internal/config's to test, and main_test.go holds that the settings it reads reach serve.

// The workflow the operator pushes, which names its image by digest, as a push records it.
const (
	theImage  = "ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc"
	theCommit = "a3f9c1e5d2b8470f9e61c3a8b0d4f7e2a9c5b1d3"

	theWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: normalize, port: ok } }
steps:
  normalize:
    image: ` + theImage + `
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
`

	theManifest = `
apiVersion: agentiik.dev/v1
kind: Brick
metadata: { name: invoice, version: 1.0.0 }
spec:
  inputs:
    orders: {}
  outputs:
    ok: {}
    rejected: {}
  runtime: { user: "65532:65532" }
`
)

func TestAnInstallationIsMigratedThenServedAndTheOperatorAloneGetsIn(t *testing.T) {
	database := freshDatabase(t)

	// migrate, on a database with no schema and no role.
	var out bytes.Buffer
	if err := migrate(t.Context(), database, &out); err != nil {
		t.Fatalf("migrating failed: %s\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "applied 0001_") || !strings.Contains(out.String(), database.Application.Role+" is the role") {
		t.Errorf("migrating said:\n%s", out.String())
	}
	out.Reset()
	if err := migrate(t.Context(), database, &out); err != nil {
		t.Fatalf("migrating again failed: %s", err)
	}
	if strings.Contains(out.String(), "applied 0") || !strings.Contains(out.String(), "already applied") {
		t.Errorf("migrating again said:\n%s", out.String())
	}

	// A namespace, which v0.2.0 has no route to create.
	admin, err := pgx.Connect(t.Context(), database.Admin.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.WithoutCancel(t.Context()))
	if _, err := admin.Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
		t.Fatal(err)
	}

	// bus-init, and a server on what it wrote.
	dir := filepath.Join(t.TempDir(), "bus")
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"bus-init", dir}, empty, io.Discard, &stderr); code != exitStopped {
		t.Fatalf("bus-init exited %d: %s", code, stderr.String())
	}
	natsURL := natsFrom(t, dir)

	s := servingSettings(t, database.Application, dir, natsURL)
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	served := make(chan error, 1)
	go func() { served <- serve(ctx, s, ln, slog.New(slog.NewTextHandler(&logged, nil))) }()
	c := client{t: t, base: "http://" + ln.Addr().String(), served: served}

	// Nobody without the token gets anywhere: not with none, not with another, and not with the
	// token's hash presented as the token.
	for _, as := range []string{"", "agk_op_not-the-token", theHash} {
		for _, r := range []struct {
			method, path string
			body         any
		}{
			{"PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/" + theCommit, aPush(t)},
			{"POST", "/api/v1/finance/workflows/monthly-invoicing/runs", api.Start{Commit: theCommit}},
			{"GET", "/api/v1/finance/runs", nil},
			{"POST", "/api/v1/runner-pools", aPool()},
			{"GET", "/api/v1/runners", nil},
			{"PUT", "/api/v1/finance/secrets/billing", api.Declare{Provider: "builtin"}},
		} {
			if code, _ := c.do(r.method, r.path, as, r.body); code != http.StatusUnauthorized {
				t.Errorf("%s %s as %q answered %d, want 401", r.method, r.path, as, code)
			}
		}
	}

	// The operator pushes a version and starts a run of it, and can cancel it.
	if code, answer := c.do("PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/"+theCommit, theToken, aPush(t)); code != http.StatusOK {
		t.Fatalf("the operator's push answered %d: %v", code, answer)
	}
	code, answer := c.do("POST", "/api/v1/finance/workflows/monthly-invoicing/runs", theToken,
		api.Start{Commit: theCommit, Inputs: map[string]any{"orders": []any{}}})
	if code != http.StatusAccepted {
		t.Fatalf("the operator's run answered %d: %v", code, answer)
	}
	runID, _ := answer["run"].(string)
	if code, answer := c.do("GET", "/api/v1/finance/runs/"+runID, theToken, nil); code != http.StatusOK || answer["triggered_by"] != string(theOperator) {
		t.Errorf("reading the run answered %d: %v", code, answer)
	}
	if code, _ := c.do("POST", "/api/v1/runs/"+runID+"/cancel", "", nil); code != http.StatusUnauthorized {
		t.Errorf("cancelling without the token answered %d", code)
	}
	if code, answer := c.do("POST", "/api/v1/runs/"+runID+"/cancel", theToken, nil); code != http.StatusAccepted {
		t.Errorf("the operator's cancel answered %d: %v", code, answer)
	}

	// The built-in store is attached, with the master key: a value is sealed and taken.
	value := "hunter2-but-longer"
	if code, answer := c.do("PUT", "/api/v1/finance/secrets/billing", theToken, api.Declare{Provider: "builtin", Value: &value}); code != http.StatusCreated {
		t.Errorf("the operator's secret answered %d: %v", code, answer)
	}

	// A pool, a join token, and a machine joining with it.
	if code, answer := c.do("POST", "/api/v1/runner-pools", theToken, aPool()); code != http.StatusCreated {
		t.Fatalf("the operator's pool answered %d: %v", code, answer)
	}
	code, answer = c.do("POST", "/api/v1/runner-pools/dmz/join-tokens", theToken, api.Issue{Labels: []string{"zone=dmz"}})
	if code != http.StatusCreated {
		t.Fatalf("the operator's join token answered %d: %v", code, answer)
	}
	joinToken, _ := answer["join_token"].(map[string]any)["token"].(string)
	code, answer = c.do("POST", "/api/v1/runners", "", aMachine(joinToken))
	if code != http.StatusCreated {
		t.Fatalf("joining answered %d: %v", code, answer)
	}
	credential, _ := answer["credential"].(string)
	runner, _ := answer["runner"].(string)
	beat := api.Beat{Runner: runner, AgentVersion: "0.2.0", State: "ready", Concurrency: 4, Tasks: []agk.TaskID{}, SentAt: time.Now()}
	if code, answer := c.do("POST", "/api/v1/runners/heartbeat", credential, beat); code != http.StatusOK {
		t.Fatalf("the heartbeat answered %d: %v", code, answer)
	}
	// The runner credential is not the operator's token, and the token is not a runner's.
	if code, _ := c.do("GET", "/api/v1/runners", credential, nil); code != http.StatusUnauthorized {
		t.Errorf("a runner credential read the inventory, answering %d", code)
	}
	if code, _ := c.do("POST", "/api/v1/runners/heartbeat", theToken, beat); code != http.StatusUnauthorized {
		t.Errorf("the operator's token heartbeat, answering %d", code)
	}
	if code, answer := c.do("GET", "/api/v1/runners", theToken, nil); code != http.StatusOK || !strings.Contains(fmt.Sprint(answer), runner) {
		t.Errorf("the inventory answered %d: %v", code, answer)
	}

	// The bus credential it is given is one the installation's server takes, on the pool's
	// consumer the API created for it.
	code, answer = c.do("POST", "/api/v1/bus/token", credential, nil)
	if code != http.StatusOK {
		t.Fatalf("the bus credential answered %d: %v", code, answer)
	}
	jwt, _ := answer["jwt"].(string)
	seed, _ := answer["seed"].(string)
	if answer["url"] != natsURL || answer["consumer"] != "dmz" {
		t.Errorf("the bus credential reads %v", answer)
	}
	b, err := bus.OpenRunner(bus.Options{URL: natsURL, Credentials: &bus.Credentials{JWT: jwt, Seed: seed}})
	if err != nil {
		t.Fatalf("the server refused the runner's bus credential: %s", err)
	}
	defer b.Close()
	if _, err := b.Take(t.Context(), "dmz", 1, 200*time.Millisecond); err != nil {
		t.Errorf("the runner could not take from its pool: %s", err)
	}

	// The objects are served at the public URL's /objects, outside /api/v1, and answer a URL
	// nobody signed with a refusal rather than a 404.
	if code, _ := c.do("GET", "/objects/finance/sha256/"+strings.Repeat("0", 64), "", nil); code != http.StatusForbidden {
		t.Errorf("an object nobody signed a URL for answered %d, want 403", code)
	}

	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("stopped, serve answered %s", err)
		}
	case <-time.After(shutdownGrace + 10*time.Second):
		t.Fatal("serve did not return once stopped")
	}
	if !strings.Contains(logged.String(), "serving") {
		t.Errorf("serve did not say where it was serving:\n%s", logged.String())
	}
}

// Every route built so far is served, and each stands behind the guard its constructor gave it: an
// installation that left one constructor out would answer a runner, a pool or an object with the
// mux's 404, which a test of the routes alone would never see.
func TestServeRegistersEveryRouteBuiltSoFar(t *testing.T) {
	database := freshDatabase(t)
	if err := migrate(t.Context(), database, io.Discard); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "bus")
	if code := run(t.Context(), []string{"bus-init", dir}, empty, io.Discard, io.Discard); code != exitStopped {
		t.Fatal("bus-init failed")
	}
	in, err := open(t.Context(), servingSettings(t, database.Application, dir, natsFrom(t, dir)), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer in.close()

	var got []string
	for _, r := range in.router.Routes() {
		got = append(got, r.Method+" "+r.Pattern)
	}
	want := []string{
		"GET /api/v1/runner-pools",
		"POST /api/v1/runner-pools",
		"POST /api/v1/runner-pools/{pool}/join-tokens",
		"GET /api/v1/runners",
		"POST /api/v1/runners",
		"POST /api/v1/runners/heartbeat",
		"POST /api/v1/runners/rotate",
		"POST /api/v1/tasks/redeem",
		"POST /api/v1/bus/token",
		"GET /api/v1/runs",
		"GET /api/v1/runs/{run}",
		"GET /api/v1/runs/{run}/outputs/{name}",
		"POST /api/v1/runs/{run}/cancel",
		"GET /api/v1/artifacts/{uri}",
		"GET /api/v1/{namespace}/runs",
		"GET /api/v1/{namespace}/runs/{run}",
		"GET /api/v1/{namespace}/secrets",
		"DELETE /api/v1/{namespace}/secrets/{name}",
		"GET /api/v1/{namespace}/secrets/{name}",
		"PUT /api/v1/{namespace}/secrets/{name}",
		"POST /api/v1/{namespace}/workflows/{workflow}/runs",
		"PUT /api/v1/{namespace}/workflows/{workflow}/versions/{commit}",
		"GET /objects/{key...}",
		"PUT /objects/{key...}",
		"POST /objects/{namespace}",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("serve registers\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// freshDatabase is a database of the test's own on the PostgreSQL AGENTIIK_TEST_DATABASE_URL names,
// with no schema and no role for the application, and the settings migrate reads for it.
func freshDatabase(t *testing.T) config.Migration {
	t.Helper()
	super := os.Getenv("AGENTIIK_TEST_DATABASE_URL")
	if super == "" {
		t.Skip("no PostgreSQL on this machine: set AGENTIIK_TEST_DATABASE_URL")
	}
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Skipf("the database at AGENTIIK_TEST_DATABASE_URL could not be reached: %s", err)
	}
	defer conn.Close(ctx)

	name := "agk_api_" + strings.ToLower(strings.NewReplacer("/", "_", " ", "_", "-", "_").Replace(t.Name()))
	if len(name) > 60 {
		name = name[:60]
	}
	for _, stmt := range []string{
		`drop database if exists ` + name + ` with (force)`,
		`drop role if exists ` + name,
		`create database ` + name,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	// The role's cleanup runs last, since a role cannot be dropped while a database grants it
	// anything.
	t.Cleanup(func() { tidy(super, `drop role if exists `+name) })
	t.Cleanup(func() { tidy(super, `drop database if exists `+name+` with (force)`) })

	u, err := pgx.ParseConfig(super)
	if err != nil {
		t.Fatal(err)
	}
	address := fmt.Sprintf("%s:%d", u.Host, u.Port)
	return config.Migration{
		Admin: config.Database{URL: fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", u.User, u.Password, address, name), Role: u.User},
		Application: config.Database{
			URL:  fmt.Sprintf("postgres://%s@%s/%s?sslmode=disable", name, address, name),
			Role: name,
			// A password holding what a URL has to escape, as openssl rand -base64 writes one.
			Password: "Xy9/Qk+z=@#?%",
		},
	}
}

// tidy runs one statement as the superuser and says nothing if it cannot: a test that has finished
// is not failed by a cluster that is already gone.
func tidy(super, stmt string) {
	ctx := context.Background()
	c, err := pgx.Connect(ctx, super)
	if err != nil {
		return
	}
	defer c.Close(ctx)
	c.Exec(ctx, stmt)
}

// natsFrom starts a NATS server whose own configuration enables JetStream and includes the
// accounts.conf bus-init wrote in dir, as an installation's does, and answers its address. It is
// a server of the test's own rather than the shared one, which trusts no operator: a credential no
// installation signed would be let in there, and this is what would refuse it.
func natsFrom(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "nats-server.conf")
	conf := fmt.Sprintf("host: 127.0.0.1\nport: -1\njetstream {\n  store_dir: %q\n}\ninclude %q\n", t.TempDir(), bus.AccountsFile)
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := natsserver.ProcessConfigFile(path)
	if err != nil {
		t.Fatalf("the server could not read the configuration bus-init wrote: %s", err)
	}
	opts.NoLog, opts.NoSigs = true, true
	server, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("the server refused the configuration bus-init wrote: %s", err)
	}
	go server.Start()
	if !server.ReadyForConnections(10 * time.Second) {
		t.Fatal("the server did not come up")
	}
	t.Cleanup(server.Shutdown)
	return server.ClientURL()
}

// servingSettings are what serve reads for the installation the test made, as settings reads
// them from what the test hands a process of its own.
func servingSettings(t *testing.T, database config.Database, dir, natsURL string) settings {
	t.Helper()
	s, err := installationConfig(t, database, dir, natsURL).settings()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// installationConfig is the configuration of the installation the test made, with the bus's
// credential and seed read from the files bus-init wrote, as internal/config reads them.
func installationConfig(t *testing.T, database config.Database, dir, natsURL string) instanceConfig {
	t.Helper()
	controller, err := config.ReadController(func(name string) (string, bool) {
		switch name {
		case config.BusURL:
			return "tls://nats.example.com:4222", true
		case config.BusCredentialsFile:
			return filepath.Join(dir, bus.ControlPlaneFile), true
		}
		return "", false
	})
	if controller.Bus.JWT == "" {
		t.Fatalf("the control plane's credential bus-init wrote could not be read: %s", err)
	}
	seed, err := os.ReadFile(filepath.Join(dir, bus.AccountSeedFile))
	if err != nil {
		t.Fatal(err)
	}
	master, err := secret.NewMaster("2026-09")
	if err != nil {
		t.Fatal(err)
	}
	presign := make([]byte, 32)
	rand.Read(presign)
	return instanceConfig{
		Database: database.ConnString(), Bus: natsURL,
		JWT: controller.Bus.JWT, Seed: string(controller.Bus.Seed), Expires: controller.Bus.Expires,
		AccountSeed: strings.TrimSpace(string(seed)),
		Objects:     t.TempDir(),
		PresignKey:  presign,
		MasterKey:   string(master.Write()),
		Listen:      "127.0.0.1:0",
	}
}

// instanceConfig is what a test hands serve, in this process or in a process of its own, where it
// travels as JSON. So the database is the URL with the password in it, since a config.Secret is
// written as a mask.
type instanceConfig struct {
	Database, Bus, JWT, Seed, AccountSeed string
	Expires                               time.Time
	Objects, MasterKey, Listen            string
	PresignKey                            []byte
}

// settings are what reading would have made of c.
func (c instanceConfig) settings() (settings, error) {
	master, err := secret.ParseMaster([]byte(c.MasterKey))
	if err != nil {
		return settings{}, err
	}
	keys, err := secret.NewKeyring(master)
	if err != nil {
		return settings{}, err
	}
	api := config.API{
		Database:        config.Database{URL: c.Database},
		Bus:             config.Bus{URL: c.Bus, JWT: c.JWT, Seed: config.Secret(c.Seed), Expires: c.Expires},
		AccountSeed:     config.Secret(c.AccountSeed),
		Objects:         c.Objects,
		PublicURL:       "https://agentiik.example.com",
		PresignKey:      config.Secret(c.PresignKey),
		MasterKey:       config.Secret(c.MasterKey),
		Listen:          c.Listen,
		JoinRotation:    config.DefaultJoinRotation,
		RevocationGrace: config.DefaultTaskCeiling,
		OperatorToken:   theHash,
	}
	return settings{API: api, keys: keys}, nil
}

// client calls the API a test is serving.
type client struct {
	t      *testing.T
	base   string
	served chan error
}

// do sends one request, as the bearer of as where it is not empty, and answers the status and the
// body read as an object.
func (c client) do(method, path, as string, body any) (int, map[string]any) {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	ctx, cancel := context.WithTimeout(c.t.Context(), 30*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if as != "" {
		r.Header.Set("Authorization", "Bearer "+as)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		select {
		case served := <-c.served:
			c.t.Fatalf("serve ended: %v", served)
		default:
		}
		c.t.Fatalf("%s %s: %s", method, path, err)
	}
	defer resp.Body.Close()
	var answer map[string]any
	json.NewDecoder(resp.Body).Decode(&answer)
	return resp.StatusCode, answer
}

func aPush(t *testing.T) api.Push {
	t.Helper()
	m, err := brick.ParseManifest([]byte(theManifest))
	if err != nil {
		t.Fatal(err)
	}
	tree := fstest.MapFS{"agentiik.yaml": &fstest.MapFile{Data: []byte(theWorkflow)}}
	v, err := version.Capture(tree, "agentiik.yaml", map[string]brick.Manifest{theImage: m})
	if err != nil {
		t.Fatal(err)
	}
	return api.Push{
		Entry: v.Entry, Document: v.Document,
		Includes: v.Includes, Manifests: v.Manifests, Branch: "main",
		Tree: map[string]api.PushFile{"agentiik.yaml": {Content: []byte(theWorkflow), Mode: "0644"}},
	}
}

func aPool() api.RunnerPool {
	return api.RunnerPool{Pool: api.Pool{
		Name: "dmz", Labels: []string{"zone=dmz"}, Namespaces: []string{"finance"}, Ceilings: &api.Ceilings{},
	}}
}

// aMachine is what a machine presents at joining, with the public key the wire's own example
// writes.
func aMachine(token string) api.Join {
	return api.Join{
		Token:        token,
		PublicKey:    "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAXiy2zvWwTpj67NwwKIgCbjFcQdrNAboeffNXm+aJUcM=\n-----END PUBLIC KEY-----\n",
		Labels:       []string{"zone=dmz"},
		Capacity:     &api.Capacity{VCPU: 8, Memory: "16Gi", Disk: "256Gi"},
		Architecture: "amd64", AgentVersion: "0.2.0",
	}
}

// instanceVariable holds the configuration a process of this test binary serves on, and is how
// TestMain knows it is one.
const instanceVariable = "AGENTIIK_API_TEST_INSTANCE"

// slowStopVariable makes a process of this test binary one that takes the first signal as the API
// does and then never finishes stopping.
const slowStopVariable = "AGENTIIK_API_TEST_SLOW_STOP"

func TestMain(m *testing.M) {
	if raw := os.Getenv(instanceVariable); raw != "" {
		os.Exit(instance(raw))
	}
	if os.Getenv(slowStopVariable) != "" {
		ctx, _ := signalled()
		fmt.Println("waiting")
		<-ctx.Done()
		fmt.Println("stopping")
		select {}
	}
	os.Exit(m.Run())
}

// instance is serve, as main runs it once the configuration is read: started the one way main
// starts it, and handed what reading would have given.
func instance(raw string) int {
	var c instanceConfig
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		fmt.Fprintf(os.Stderr, "the configuration a test handed over could not be read: %s\n", err)
		return exitUsage
	}
	s, err := c.settings()
	if err != nil {
		fmt.Fprintf(os.Stderr, "the configuration a test handed over could not be used: %s\n", err)
		return exitUsage
	}
	return untilSignalled(func(ctx context.Context) int {
		return start(ctx, s, os.Stderr)
	})
}

// output is what a process wrote, kept for a failure to show.
type output struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.String()
}

// eventually waits up to a deadline for ok, and fails naming what with what the process wrote.
func eventually(t *testing.T, within time.Duration, what string, ok func() bool, out *output) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %s, and the process wrote:\n%s", what, within, out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The API as a process of its own, stopped as systemd and docker stop it. At SIGTERM it takes no
// new connection, finishes the request it is answering, and exits 0: here an object a runner is
// uploading through a presigned URL, half of which has arrived when the signal does.
func TestStoppedBySIGTERMTheAPIFinishesWhatItIsAnsweringAndExitsZero(t *testing.T) {
	database := freshDatabase(t)
	if err := migrate(t.Context(), database, io.Discard); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "bus")
	if code := run(t.Context(), []string{"bus-init", dir}, empty, io.Discard, io.Discard); code != exitStopped {
		t.Fatal("bus-init failed")
	}
	c := installationConfig(t, database.Application, dir, natsFrom(t, dir))
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out := &output{}
	cmd := exec.Command(self, "-test.run=^$")
	cmd.Env = append(os.Environ(), instanceVariable+"="+string(raw))
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill() })

	address := regexp.MustCompile(`msg=serving address=(\S+)`)
	eventually(t, 30*time.Second, "the API serving", func() bool { return address.MatchString(out.String()) }, out)
	base := "http://" + address.FindStringSubmatch(out.String())[1]

	// A presigned URL for an object, signed with the installation's key as a redemption signs one.
	content := bytes.Repeat([]byte("an invoice line\n"), 4096)
	sum := sha256.Sum256(content)
	key := "finance/sha256/" + hex.EncodeToString(sum[:])
	signed, err := artifact.NewSigned(artifact.Dir(t.TempDir()), artifact.SignedOptions{Key: c.PresignKey, Base: base + "/objects"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := signed.Presign(t.Context(), http.MethodPut, key, agk.NewRunID(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	body, sending := io.Pipe()
	put, err := http.NewRequestWithContext(t.Context(), http.MethodPut, u, body)
	if err != nil {
		t.Fatal(err)
	}
	put.ContentLength = int64(len(content))
	answered := make(chan *http.Response, 1)
	go func() {
		resp, err := http.DefaultClient.Do(put)
		if err != nil {
			t.Errorf("the upload was cut: %s", err)
			answered <- nil
			return
		}
		resp.Body.Close()
		answered <- resp
	}()
	half := len(content) / 2
	if _, err := sending.Write(content[:half]); err != nil {
		t.Fatal(err)
	}
	// The write returns once the bytes are in the socket's buffers, which can be before the
	// server has accepted the connection, and a connection still waiting to be accepted when the
	// listener closes is reset. The file the store stages the object in is there once the
	// handler is reading the body, which is the upload under way this test is about.
	eventually(t, 10*time.Second, "the upload reaching the store", func() bool {
		staged, _ := filepath.Glob(filepath.Join(c.Objects, "finance", "sha256", ".staging-*"))
		return len(staged) > 0
	}, out)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// The listener is closed first, so a new connection is refused while the upload goes on.
	eventually(t, 10*time.Second, "the listener closing", func() bool {
		conn, err := net.DialTimeout("tcp", strings.TrimPrefix(base, "http://"), time.Second)
		if err == nil {
			conn.Close()
		}
		return err != nil
	}, out)
	select {
	case err := <-exited:
		t.Fatalf("the API exited with %v while an upload was half done:\n%s", err, out)
	default:
	}

	if _, err := sending.Write(content[half:]); err != nil {
		t.Fatal(err)
	}
	sending.Close()
	if resp := <-answered; resp == nil || resp.StatusCode != http.StatusCreated {
		t.Errorf("the upload under way at the signal was answered %v:\n%s", resp, out)
	}
	stored, err := os.ReadFile(filepath.Join(c.Objects, filepath.FromSlash(key)))
	if err != nil || !bytes.Equal(stored, content) {
		t.Errorf("the object is not in the store as it was sent: %v", err)
	}

	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("stopped with SIGTERM, the API exited with %v:\n%s", err, out)
		}
	case <-time.After(shutdownGrace + 10*time.Second):
		t.Fatalf("the API did not stop after SIGTERM:\n%s", out)
	}
}

// The first SIGTERM is a stop asked for, and the program takes it. The second is somebody for whom
// the way out is taking too long, and it ends the process as it would any other.
func TestASecondSignalEndsAProcessStillStopping(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out := &output{}
	cmd := exec.Command(self, "-test.run=^$")
	cmd.Env = append(os.Environ(), slowStopVariable+"=1")
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill() })

	for _, want := range []string{"waiting", "stopping"} {
		if want == "stopping" {
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		}
		eventually(t, 10*time.Second, "the process "+want, func() bool { return strings.Contains(out.String(), want) }, out)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
			t.Errorf("the second SIGTERM ended the process with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second SIGTERM did not end a process still stopping")
	}
}

// A stop asked for while the API is still reaching what it stands on is a stop, and exits 0: here
// a database that accepts the connection and never answers, as one still starting can.
func TestStoppedWhileStartingTheAPIExitsZero(t *testing.T) {
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()

	dir := filepath.Join(t.TempDir(), "bus")
	if code := run(t.Context(), []string{"bus-init", dir}, empty, io.Discard, io.Discard); code != exitStopped {
		t.Fatal("bus-init failed")
	}
	database := config.Database{URL: "postgres://agentiik@" + silent.Addr().String() + "/agentiik?sslmode=disable&connect_timeout=60", Role: "agentiik"}
	s := servingSettings(t, database, dir, "nats://127.0.0.1:1")

	ctx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	var stderr bytes.Buffer
	if code := start(ctx, s, &stderr); code != exitStopped {
		t.Errorf("stopped while it was reaching its database, the API exited %d: %s", code, stderr.String())
	}
}
