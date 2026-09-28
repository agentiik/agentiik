package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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

// v030 is the last migration v0.3.0 carried.
const v030 = "0048_sessions_opened_by_a_credential.sql"

// An installation of v0.3.0, upgraded as its Compose file is, with nothing asked: its workflows,
// which agk push sent as trees, become empty repositories. Every version, run and counted object is
// kept as it was, each default branch is unborn and unprotected, so that an editor's agk push keeps
// landing, and a run of a version pushed before the upgrade still starts. A tree pushed after it is
// recorded as one, and moves no ref; a workflow a push creates from now on is a repository whose
// default branch is protected.
func TestAnInstallationOfV030KeepsItsWorkflowsAsEmptyRepositories(t *testing.T) {
	database := freshDatabase(t)
	ctx := t.Context()
	admin, err := pgx.Connect(ctx, database.Admin.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.WithoutCancel(ctx))
	if _, err := db.MigrateThrough(ctx, admin, v030); err != nil {
		t.Fatalf("the database could not be migrated as v0.3.0 migrated it: %s", err)
	}

	// Two versions agk push of v0.3.0 sent as trees, each a run, and an editor of the workflow, as
	// v0.3.0 stored them: the version's graph column in the shape package db writes it, its tree as
	// a manifest of counted objects.
	p := aPush(t)
	graph, err := json.Marshal(map[string]any{"entry": p.Entry, "document": p.Document, "manifests": p.Manifests})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(theWorkflow))
	digest := hex.EncodeToString(sum[:])
	tree := fmt.Sprintf(`[{"path": "agentiik.yaml", "sha256": %q, "size": %d, "mode": "0644"}]`, digest, len(theWorkflow))
	older, newer := strings.Repeat("1", 40), strings.Repeat("2", 40)
	const finished, running = "01JMZ8V1P9C4XQ7K2N4D6F8H0A", "01M2AAZ9G62NQXFAFCXKRPJEH5"
	for _, stmt := range []string{
		`insert into namespaces (name) values ('finance')`,
		`insert into principals (id, kind) values ('erin', 'user')`,
		`insert into users (login, display_name) values ('erin', 'Erin')`,
		`insert into workflows (namespace, name, default_branch) values ('finance', 'monthly-invoicing', 'master')`,
		`insert into grants (id, namespace, workflow, principal, role, granted_by)
		   values ('01JQ3M8T', 'finance', 'monthly-invoicing', 'erin', 'editor', 'operator')`,
		`insert into workflow_versions (namespace, workflow, commit, parent, graph, tree, author, created_at) values
		   ('finance', 'monthly-invoicing', '` + older + `', null, '` + string(graph) + `', '` + tree + `', 'erin', now() - interval '2 days'),
		   ('finance', 'monthly-invoicing', '` + newer + `', '` + older + `', '` + string(graph) + `', '` + tree + `', 'erin', now() - interval '1 day')`,
		`insert into artifact_objects (namespace, digest, size_bytes, media_type, refs)
		   values ('finance', 'sha256:` + digest + `', ` + fmt.Sprint(len(theWorkflow)) + `, 'application/octet-stream', 2)`,
		`insert into runs (namespace, id, workflow, commit, state, trigger, triggered_by, files_recorded,
		                   started_at, finished_at, expires_at) values
		   ('finance', '` + finished + `', 'monthly-invoicing', '` + older + `', 'succeeded', 'manual', 'erin', true,
		    now() - interval '2 days', now() - interval '2 days', now() + interval '28 days')`,
		`insert into runs (namespace, id, workflow, commit, state, trigger, triggered_by, files_recorded, started_at)
		   values ('finance', '` + running + `', 'monthly-invoicing', '` + newer + `', 'running', 'schedule', 'erin', true, now())`,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("filling the database as v0.3.0 would have: %s", err)
		}
	}
	held := func(table string) []string {
		t.Helper()
		rows, err := admin.Query(ctx, `select (to_jsonb(t) - 'source')::text from `+pgx.Identifier{table}.Sanitize()+` t order by 1`)
		if err != nil {
			t.Fatal(err)
		}
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := map[string][]string{}
	for _, table := range []string{"workflow_versions", "runs", "artifact_objects"} {
		before[table] = held(table)
	}

	d := aPreparedDirectory(t)
	c := config.Init{
		Dir: d.dir, Host: "localhost", Namespace: "finance", OperatorToken: theToken,
		Admin: database.Admin, Application: config.Database{URL: database.Application.URL, Role: database.Application.Role},
	}
	if err := initialize(ctx, c, d.at(time.Now().UTC())); err != nil {
		t.Fatalf("init refused the installation v0.3.0 left: %s\n%s", err, d.out.String())
	}
	if !strings.Contains(d.out.String(), "applied 0049_workflow_repositories.sql") {
		t.Errorf("init said:\n%s", d.out.String())
	}
	for table, was := range before {
		if now := held(table); !slices.Equal(now, was) {
			t.Errorf("the upgrade changed %s:\nbefore %q\nafter  %q", table, was, now)
		}
	}

	application := database.Application
	application.Password = config.Secret(strings.TrimSpace(d.read(t, apiDir, "database-password")))
	dir := filepath.Join(t.TempDir(), "bus")
	if code := run(ctx, []string{"bus-init", dir}, empty, io.Discard, io.Discard); code != exitStopped {
		t.Fatal("bus-init failed")
	}
	s := servingSettings(t, application, dir, natsFrom(t, dir))
	object := filepath.Join(s.API.Objects, "finance", "sha256", digest)
	if err := os.MkdirAll(filepath.Dir(object), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(object, []byte(theWorkflow), 0o444); err != nil {
		t.Fatal(err)
	}
	serving, stop := context.WithCancel(ctx)
	defer stop()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- serve(serving, s, ln, slog.New(slog.DiscardHandler)) }()
	cl := client{t: t, base: "http://" + ln.Addr().String(), served: served}

	pool, err := db.Open(ctx, application.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	erins := "agk_test_erin_upgraded"
	if err := pool.Installation(ctx, db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.MintToken(ctx, db.APIToken{ID: ulid.New(), Hash: hashOf(erins), Principal: "erin", CreatedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour)})
	}); err != nil {
		t.Fatal(err)
	}
	repository := func(workflow string) db.Repository {
		t.Helper()
		var r db.Repository
		if err := pool.In(ctx, "finance", func(ctx context.Context, n *db.NS) error {
			var err error
			r, err = n.Repository(ctx, workflow)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return r
	}
	unborn := []db.Ref{{Name: "refs/heads/master"}}
	if r := repository("monthly-invoicing"); r.DefaultBranch != "master" || !slices.Equal(r.Refs, unborn) || len(r.Packs) != 0 {
		t.Errorf("after the upgrade the workflow is the repository %+v", r)
	}

	// The runs v0.3.0 made read as they did, and one of a version pushed before the upgrade starts.
	for id, state := range map[string]string{finished: "succeeded", running: "running"} {
		if code, answer := cl.do("GET", "/api/v1/finance/runs/"+id, erins, nil); code != http.StatusOK || answer["state"] != state || answer["triggered_by"] != "erin" {
			t.Errorf("the run %s v0.3.0 made answered %d: %v", id, code, answer)
		}
	}
	if code, answer := cl.do("POST", "/api/v1/finance/workflows/monthly-invoicing/runs", erins,
		api.Start{Commit: older, Inputs: map[string]any{"orders": []any{}}}); code != http.StatusAccepted {
		t.Errorf("a run of a version pushed before the upgrade answered %d: %v", code, answer)
	}

	// The editor's agk push of v0.3.0 keeps landing, recorded as a tree, and moves no ref.
	if code, answer := cl.do("PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/"+theCommit, erins, p); code != http.StatusOK {
		t.Fatalf("the editor's push after the upgrade answered %d: %v", code, answer)
	}
	var source string
	if err := admin.QueryRow(ctx, `select source from workflow_versions where commit = $1`, theCommit).Scan(&source); err != nil || source != db.SourceTree {
		t.Errorf("a tree pushed after the upgrade was recorded as arrived by %q, %v", source, err)
	}
	if r := repository("monthly-invoicing"); !slices.Equal(r.Refs, unborn) {
		t.Errorf("a tree push moved the refs to %+v", r.Refs)
	}

	// A workflow a push creates from now on is a repository created from v0.4.0: unprotected, as
	// every repository is until an owner protects it.
	weekly := pushNaming(t, strings.Replace(theWorkflow, "name: monthly-invoicing", "name: weekly-invoicing", 1))
	if code, answer := cl.do("PUT", "/api/v1/finance/workflows/weekly-invoicing/versions/"+theCommit, theToken, weekly); code != http.StatusOK {
		t.Fatalf("the push creating a workflow answered %d: %v", code, answer)
	}
	if r := repository("weekly-invoicing"); r.DefaultBranch != "main" || !slices.Equal(r.Refs, []db.Ref{{Name: "refs/heads/main"}}) {
		t.Errorf("a workflow a push created after the upgrade is the repository %+v", r)
	}
}
