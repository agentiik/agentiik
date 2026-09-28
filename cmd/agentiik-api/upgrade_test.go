package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/jackc/pgx/v5"
)

// v025 is the last migration v0.2.5 carried, as db's own upgrade test names it.
const v025 = "0031_audit_verified.sql"

// An installation of v0.2.5, upgraded as its Compose file is: "the person may change compose.yaml
// and .env, and nothing else". The database is as v0.2.5 migrated and filled it, the operator
// token's hash is where v0.2.5's init kept it, and this release's init runs with the same token in
// the same setting, then serve. The token authenticates as the bootstrap operator, lists the pools,
// reads the run the operator started under v0.2.5, and pushes and starts a run, recorded as the
// operator's as the old one is. It creates the first administrator, who is handed the namespace
// v0.2.5 made and reads the old run with nothing shared by hand. Once the first administrator has
// enrolled, the same token is a 401 that says why, and init, run again with the line still set,
// says it is ignored and succeeds.
func TestAnInstallationOfV025KeepsItsOperatorTokenThroughTheUpgrade(t *testing.T) {
	database := freshDatabase(t)
	ctx := t.Context()
	admin, err := pgx.Connect(ctx, database.Admin.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.WithoutCancel(ctx))
	if _, err := db.MigrateThrough(ctx, admin, v025); err != nil {
		t.Fatalf("the database could not be migrated as v0.2.5 migrated it: %s", err)
	}
	const old = "01JMZ8V1P9C4XQ7K2N4D6F8H0A"
	for _, stmt := range []string{
		`insert into namespaces (name) values ('finance')`,
		`insert into workflows (namespace, name) values ('finance', 'monthly-invoicing')`,
		`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at)
		   values ('finance', 'monthly-invoicing', 'a3f9c1e', '{}', 'operator', now())`,
		`insert into runs (namespace, id, workflow, commit, state, trigger, triggered_by)
		   values ('finance', '` + old + `', 'monthly-invoicing', 'a3f9c1e', 'succeeded', 'manual', 'operator')`,
		`insert into steps (namespace, run_id, step, state) values ('finance', '` + old + `', 'invoice', 'succeeded')`,
		`insert into runner_pools (name, labels, accepted_namespaces, created_by)
		   values ('dmz', '{zone=dmz}', '{finance}', 'operator')`,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("filling the database as v0.2.5 would have: %s", err)
		}
	}
	d := aPreparedDirectory(t)
	sha := filepath.Join(d.dir, apiDir, "operator-token.sha256")
	if err := os.MkdirAll(filepath.Dir(sha), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sha, []byte(theHash+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// This release's init, with the setting v0.2.5's Compose file hands it.
	c := config.Init{
		Dir: d.dir, Host: "localhost", Namespace: "finance", OperatorToken: theToken,
		Admin: database.Admin, Application: config.Database{URL: database.Application.URL, Role: database.Application.Role},
	}
	if err := initialize(ctx, c, d.at(time.Now().UTC())); err != nil {
		t.Fatalf("init refused the installation v0.2.5 left: %s\n%s", err, d.out.String())
	}
	if !strings.Contains(d.out.String(), "applied 0032_") || !strings.Contains(d.out.String(), "wrote the hash of the bootstrap token set") ||
		!strings.Contains(d.out.String(), "gave namespace finance its built-in identity, finance/agentiik") {
		t.Errorf("init said:\n%s", d.out.String())
	}

	application := database.Application
	application.Password = config.Secret(strings.TrimSpace(d.read(t, apiDir, "database-password")))
	dir := filepath.Join(t.TempDir(), "bus")
	if code := run(ctx, []string{"bus-init", dir}, empty, io.Discard, io.Discard); code != exitStopped {
		t.Fatal("bus-init failed")
	}
	serving, stop := context.WithCancel(ctx)
	defer stop()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- serve(serving, servingSettings(t, application, dir, natsFrom(t, dir)), ln, slog.New(slog.DiscardHandler))
	}()
	cl := client{t: t, base: "http://" + ln.Addr().String(), served: served}

	code, answer := cl.do("GET", "/api/v1/runner-pools", theToken, nil)
	if code != http.StatusOK || !strings.Contains(fmt.Sprint(answer), "dmz") || !strings.Contains(fmt.Sprint(answer), "default") {
		t.Fatalf("the operator token listed the pools answering %d: %v", code, answer)
	}
	for _, path := range []string{"/api/v1/finance/runs/" + old, "/api/v1/runs/" + old} {
		if code, answer := cl.do("GET", path, theToken, nil); code != http.StatusOK || answer["triggered_by"] != "operator" || answer["state"] != "succeeded" {
			t.Errorf("the run the operator started under v0.2.5 read at %s answered %d: %v", path, code, answer)
		}
	}
	if code, answer := cl.do("PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/"+theCommit, theToken, aPush(t)); code != http.StatusOK {
		t.Fatalf("the operator token's push answered %d: %v", code, answer)
	}
	code, answer = cl.do("POST", "/api/v1/finance/workflows/monthly-invoicing/runs", theToken,
		api.Start{Commit: theCommit, Inputs: map[string]any{"orders": []any{}}})
	if code != http.StatusAccepted {
		t.Fatalf("the operator token's run answered %d: %v", code, answer)
	}
	started, _ := answer["run"].(string)
	if code, answer := cl.do("GET", "/api/v1/runs/"+started, theToken, nil); code != http.StatusOK || answer["triggered_by"] != "operator" {
		t.Errorf("the run started after the upgrade read answering %d: %v", code, answer)
	}
	if code, _ := cl.do("GET", "/api/v1/runner-pools", "", nil); code != http.StatusUnauthorized {
		t.Errorf("with no token, the pools answered %d", code)
	}

	// The token creates the first administrator, who is handed the namespace v0.2.5 made, which
	// no record names an owner of, and reads the operator's old run with nothing shared by hand.
	if code, answer := cl.do("POST", "/api/v1/users", theToken, map[string]any{"login": "dana", "admin": true}); code != http.StatusCreated {
		t.Fatalf("the operator token creating the first administrator answered %d: %v", code, answer)
	}
	var handed int
	if err := admin.QueryRow(ctx, `select count(*) from grants where namespace = 'finance' and workflow is null
	                                 and principal = 'dana' and role = 'owner' and granted_by = 'operator'`).Scan(&handed); err != nil {
		t.Fatal(err)
	}
	if handed != 1 {
		t.Errorf("the first administrator was handed finance %d times", handed)
	}
	pool, err := db.Open(ctx, application.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	danas := "agk_test_dana_upgraded"
	if err := pool.Installation(ctx, db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.MintToken(ctx, db.APIToken{ID: ulid.New(), Hash: hashOf(danas), Principal: "dana", CreatedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour)})
	}); err != nil {
		t.Fatal(err)
	}

	// The first administrator enrols, which ends the bootstrap token.
	if err := pool.Installation(ctx, db.Identity, func(ctx context.Context, w *db.Wide) error {
		_, err := w.EndBootstrap(ctx, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code, answer := cl.do("GET", "/api/v1/finance/runs/"+old, danas, nil); code != http.StatusOK || answer["triggered_by"] != "operator" {
		t.Errorf("the first administrator reading the operator's run of v0.2.5 answered %d: %v", code, answer)
	}
	code, answer = cl.do("GET", "/api/v1/runner-pools", theToken, nil)
	if said, _ := answer["error"].(string); code != http.StatusUnauthorized || !strings.Contains(said, "bootstrap token, that ended when the first administrator signed in") {
		t.Errorf("once the first administrator enrolled, the operator token answered %d: %v", code, answer)
	}
	d.out.Reset()
	if err := initialize(ctx, c, d.at(time.Now().UTC())); err != nil {
		t.Fatalf("init, with the line still set once the bootstrap ended, failed: %s\n%s", err, d.out.String())
	}
	if !strings.Contains(d.out.String(), "ignored the bootstrap token set") {
		t.Errorf("init, with the line still set once the bootstrap ended, said:\n%s", d.out.String())
	}
	if content, err := os.ReadFile(sha); err != nil || string(content) != theHash+"\n" {
		t.Errorf("the hash v0.2.5's init kept was changed or removed: %q, %v", content, err)
	}
}
