package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/internal/config"
	"github.com/jackc/pgx/v5"
)

// namespaced is a fresh database, migrated, and a connection to it as the role migrate runs as,
// for a test to read and write what no route reaches.
func namespaced(t *testing.T) (config.Migration, *pgx.Conn) {
	t.Helper()
	database := freshDatabase(t)
	if err := migrate(t.Context(), database, io.Discard); err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.Connect(t.Context(), database.Admin.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.WithoutCancel(t.Context())) })
	return database, admin
}

// exists says whether a namespace of that name is in the database.
func exists(t *testing.T, admin *pgx.Conn, name string) bool {
	t.Helper()
	var n int
	if err := admin.QueryRow(t.Context(), `select count(*) from namespaces where name = $1`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// audited is every entry of the audit log, as action, target and result.
func audited(t *testing.T, admin *pgx.Conn) []string {
	t.Helper()
	rows, err := admin.Query(t.Context(), `select actor || ' ' || action || ' ' || target || ' ' || result from audit_log order by seq`)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// A namespace is created once, and creating it again changes nothing and says so, so that an
// installation script that creates it runs twice. Both are recorded, the second as unchanged.
func TestNamespaceCreateCreatesItOnceAndSaysWhatItDid(t *testing.T) {
	database, admin := namespaced(t)

	var out bytes.Buffer
	if err := namespace(t.Context(), database.Application, "create", "finance", &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "created namespace finance\n" || !exists(t, admin, "finance") {
		t.Errorf("creating said %q, and the namespace exists: %v", out.String(), exists(t, admin, "finance"))
	}
	out.Reset()
	if err := namespace(t.Context(), database.Application, "create", "finance", &out); err != nil {
		t.Fatalf("creating it again failed: %s", err)
	}
	if !strings.Contains(out.String(), "already exists") {
		t.Errorf("creating it again said %q", out.String())
	}
	want := []string{"installation namespace.create finance done", "installation namespace.create finance unchanged"}
	if got := audited(t, admin); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the audit log holds %q, want %q", got, want)
	}
}

// A name the API would refuse is refused before any setting is read: out of grammar, too long, or
// one of the words the API routes on.
func TestNamespaceCreateRefusesANameTheAPIRefuses(t *testing.T) {
	for _, name := range []string{"Finance", "team_ops", "-finance", "finance-", strings.Repeat("a", 256), "runs", "runner-pools", "namespaces"} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), []string{"namespace", "create", name}, empty, &stdout, &stderr); code != exitFailed {
			t.Errorf("creating %.20q exited %d, want %d", name, code, exitFailed)
		}
		if strings.Contains(stderr.String(), config.MigrateDatabaseURL) || stdout.Len() > 0 {
			t.Errorf("creating %.20q went on to read the settings, or said it did something:\n%s%s", name, stdout.String(), stderr.String())
		}
	}
	var stderr bytes.Buffer
	run(t.Context(), []string{"namespace", "create", "runs"}, empty, io.Discard, &stderr)
	if !strings.Contains(stderr.String(), "routes on") {
		t.Errorf("a reserved name is refused as %q", stderr.String())
	}
}

// stats, reserved from v0.3.0 for GET /api/v1/stats/pools, is refused as a new namespace's name,
// as the API refuses it, once the database says no namespace carries it; one v0.2 created under it
// is left as any namespace created again is, so that a script or an init that names it goes on
// working through the upgrade that reserved the word.
func TestNamespaceCreateKeepsANamespaceCreatedBeforeItsWordWasReserved(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"namespace", "create", "stats"}, empty, io.Discard, &stderr); code != exitFailed || !strings.Contains(stderr.String(), config.DatabaseURL) {
		t.Errorf("stats was refused before the database was asked, exiting %d:\n%s", code, stderr.String())
	}

	database, admin := namespaced(t)
	var out bytes.Buffer
	err := namespace(t.Context(), database.Application, "create", "stats", &out)
	if err == nil || !strings.Contains(err.Error(), "stats is a word the API routes on from v0.6.0, for GET /api/v1/stats/pools") || exists(t, admin, "stats") {
		t.Errorf("creating stats answered %v, and the namespace exists: %v", err, exists(t, admin, "stats"))
	}
	if _, err := admin.Exec(t.Context(), `insert into namespaces (name) values ('stats')`); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := namespace(t.Context(), database.Application, "create", "stats", &out); err != nil {
		t.Fatalf("creating stats where v0.2 created it failed: %s", err)
	}
	if out.String() != "namespace stats already exists, and was left as it was\n" {
		t.Errorf("creating stats where v0.2 created it said %q", out.String())
	}
	if got := audited(t, admin); strings.Join(got, "\n") != "installation namespace.create stats unchanged" {
		t.Errorf("the audit log holds %q", got)
	}
}

// A namespace named after a user's login is refused, since a login is also the name of its user's
// personal namespace, and saying so names the collision rather than the table that refused it.
func TestNamespaceCreateRefusesALogin(t *testing.T) {
	database, admin := namespaced(t)
	for _, stmt := range []string{
		`insert into principals (id, kind) values ('alice', 'user')`,
		`insert into users (login, display_name) values ('alice', 'Alice')`,
	} {
		if _, err := admin.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	var out bytes.Buffer
	err := namespace(t.Context(), database.Application, "create", "alice", &out)
	if err == nil || !strings.Contains(err.Error(), "alice is a user's login") || out.Len() > 0 || exists(t, admin, "alice") {
		t.Errorf("creating a namespace named after a login answered %v, said %q", err, out.String())
	}
}

// With nothing configured, namespace refuses and names the database setting the API reads, and not
// the privileged role migrate connects as, which it never uses.
func TestNamespaceReadsTheAPIsDatabaseSettingAlone(t *testing.T) {
	for _, action := range []string{"create", "remove"} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), []string{"namespace", action, "finance"}, empty, &stdout, &stderr); code != exitFailed {
			t.Errorf("%s with nothing configured exited %d, want %d", action, code, exitFailed)
		}
		if !strings.Contains(stderr.String(), config.DatabaseURL) || strings.Contains(stderr.String(), config.MigrateDatabaseURL) {
			t.Errorf("%s's refusal names the wrong settings:\n%s", action, stderr.String())
		}
	}
}

// A namespace that holds nothing but its built-in identity is removed, the identity with it, and
// one that holds a workflow, a secret, a stored object or another service account is refused, left
// as it was, and told what it holds. A namespace that does not exist is refused too, and so is a
// user's personal namespace, which goes only with its user.
func TestNamespaceRemoveRemovesOnlyAnEmptyNamespace(t *testing.T) {
	database, admin := namespaced(t)
	for _, name := range []string{"empty", "with-workflow", "with-secret", "with-object", "with-account"} {
		if err := namespace(t.Context(), database.Application, "create", name, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range []string{
		`insert into workflows (namespace, name) values ('with-workflow', 'monthly-invoicing')`,
		`insert into secret_declarations (namespace, name, provider, declared_by) values ('with-secret', 'billing', 'builtin', 'operator')`,
		// An object outlives the run that wrote it until it is collected, and a namespace
		// holding one is not empty either.
		`insert into artifact_objects (namespace, digest, size_bytes, media_type) values ('with-object', 'sha256:` + strings.Repeat("0", 64) + `', 0, 'application/json')`,
		`insert into principals (id, kind) values ('with-account/deploy', 'service_account')`,
		`insert into service_accounts (namespace, name, created_by) values ('with-account', 'deploy', 'alice')`,
		`insert into principals (id, kind) values ('alice', 'user')`,
		`insert into users (login, display_name) values ('alice', 'Alice')`,
		`insert into namespaces (name, kind, owner) values ('alice', 'personal', 'alice')`,
	} {
		if _, err := admin.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	principal := func(id string) bool {
		var n int
		if err := admin.QueryRow(t.Context(), `select count(*) from principals where id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if !principal("empty/agentiik") {
		t.Fatal("a namespace was created without its built-in identity")
	}

	var out bytes.Buffer
	if err := namespace(t.Context(), database.Application, "remove", "empty", &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "removed namespace empty\n" || exists(t, admin, "empty") || principal("empty/agentiik") {
		t.Errorf("removing said %q, and the namespace is still there: %v, its built-in identity: %v", out.String(), exists(t, admin, "empty"), principal("empty/agentiik"))
	}

	for name, held := range map[string]string{
		"with-workflow": "1 workflow", "with-secret": "1 secret", "with-object": "1 stored object",
		"with-account": "1 service account besides with-account/agentiik", "alice": "the personal namespace of the user alice",
	} {
		out.Reset()
		err := namespace(t.Context(), database.Application, "remove", name, &out)
		// Counted, and said as what it is rather than as the table a removal ran into.
		if err == nil || !strings.Contains(err.Error(), held) || !strings.Contains(err.Error(), "not removed") || strings.Contains(err.Error(), "rows of") {
			t.Errorf("removing %s answered %v, and it holds %s", name, err, held)
		}
		if out.Len() > 0 || !exists(t, admin, name) {
			t.Errorf("removing %s said %q, and it is there: %v", name, out.String(), exists(t, admin, name))
		}
	}

	if err := namespace(t.Context(), database.Application, "remove", "nowhere", io.Discard); err == nil || !strings.Contains(err.Error(), "no namespace nowhere") {
		t.Errorf("removing a namespace that does not exist answered %v", err)
	}

	// The removal is recorded, and the refusals are not acts.
	got := audited(t, admin)
	if len(got) != 6 || got[5] != "installation namespace.delete empty done" {
		t.Errorf("the audit log holds %q", got)
	}
}
