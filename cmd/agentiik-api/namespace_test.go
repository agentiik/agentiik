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
	want := []string{"operator namespace.create finance done", "operator namespace.create finance unchanged"}
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

// With nothing configured, namespace refuses and names the settings migrate reads.
func TestNamespaceReadsTheSettingsMigrateReads(t *testing.T) {
	for _, action := range []string{"create", "remove"} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), []string{"namespace", action, "finance"}, empty, &stdout, &stderr); code != exitFailed {
			t.Errorf("%s with nothing configured exited %d, want %d", action, code, exitFailed)
		}
		for _, variable := range []string{config.MigrateDatabaseURL, config.DatabaseURL} {
			if !strings.Contains(stderr.String(), variable) {
				t.Errorf("%s's refusal does not name %s:\n%s", action, variable, stderr.String())
			}
		}
	}
}

// A namespace that holds nothing is removed, and one that holds a workflow, a run or a secret is
// refused, left as it was, and told what it holds. A namespace that does not exist is refused too.
func TestNamespaceRemoveRemovesOnlyAnEmptyNamespace(t *testing.T) {
	database, admin := namespaced(t)
	for _, name := range []string{"empty", "with-workflow", "with-secret", "with-object"} {
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
	} {
		if _, err := admin.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}

	var out bytes.Buffer
	if err := namespace(t.Context(), database.Application, "remove", "empty", &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "removed namespace empty\n" || exists(t, admin, "empty") {
		t.Errorf("removing said %q, and the namespace is still there: %v", out.String(), exists(t, admin, "empty"))
	}

	for name, held := range map[string]string{"with-workflow": "1 workflow", "with-secret": "1 secret", "with-object": "rows of artifact_objects"} {
		out.Reset()
		err := namespace(t.Context(), database.Application, "remove", name, &out)
		if err == nil || !strings.Contains(err.Error(), held) || !strings.Contains(err.Error(), "not removed") {
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
	if len(got) != 5 || got[4] != "operator namespace.delete empty done" {
		t.Errorf("the audit log holds %q", got)
	}
}
