// Package dbtest opens a real PostgreSQL for a test, or skips it.
//
// Everything that touches the database is tested against a real one, for the reason the
// driver's tests are run against a real daemon: what is under test is what PostgreSQL does
// when the Go is wrong, and a fake would agree with whatever the code believes. Row level
// security in particular is not a thing a stub can have an opinion about.
//
// A test using this skips when AGENTIIK_TEST_DATABASE_URL is unset, so continuous integration
// on a machine with nothing installed stays green and a machine with a database tests for
// real.
//
//	docker run -d --name agk-pg -e POSTGRES_PASSWORD=agk -e POSTGRES_DB=agk \
//	  -p 55432:5432 postgres:17-alpine
//	AGENTIIK_TEST_DATABASE_URL=postgres://postgres:agk@127.0.0.1:55432/agk go test ./...
//
// The variable names a superuser, because a test creates the schema and the unprivileged role
// the application uses. What comes back is opened as that unprivileged role, which is the
// whole point: a superuser walks through every policy, and db.Open refuses one.
package dbtest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/db"
	"github.com/jackc/pgx/v5"
)

// The unprivileged role is created per database and named after it, by db.Provision, which is
// how an installation's migrate step creates it: a test runs as the role an installation runs
// as, with its grants and nothing more. Per database because a role is a property of the
// cluster and not of a database: one role shared by every test is a role that two packages
// testing at once each drop while the other is using it, which fails as a dependency error
// nobody reads as a shared name.

// Open prepares a database of this test's own and answers a pool opened on it, together with
// the superuser address for the setting up a test has to do behind the policies.
//
// One database per test, so that two tests cannot see each other's rows and a failure leaves
// something a person can open afterwards.
func Open(t *testing.T) (pool *db.Pool, super string) {
	t.Helper()
	super = Migrated(t)
	pool, err := db.Open(t.Context(), withCredentials(super, roleOf(super), "test"))
	if err != nil {
		t.Fatalf("the pool could not be opened: %s", err)
	}
	t.Cleanup(pool.Close)
	return pool, super
}

// Migrated prepares the database and the role without opening a pool, for a test that wants to
// open one itself or that is testing the opening.
func Migrated(t *testing.T) string {
	t.Helper()
	url := os.Getenv("AGENTIIK_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("no PostgreSQL on this machine: set AGENTIIK_TEST_DATABASE_URL")
	}

	ctx := t.Context()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Skipf("the database at AGENTIIK_TEST_DATABASE_URL could not be reached: %s", err)
	}
	defer conn.Close(ctx)

	name := "agk_" + strings.ToLower(strings.NewReplacer("/", "_", " ", "_", "-", "_").Replace(t.Name()))
	if len(name) > 60 {
		name = name[:60]
	}
	// A role left by a run that never tidied up goes too, once the database that granted it
	// something has, so that the role a test starts from is one it created.
	for _, stmt := range []string{
		fmt.Sprintf(`drop database if exists %s with (force)`, name),
		`drop role if exists ` + name,
		fmt.Sprintf(`create database %s`, name),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	// Cleanups run in the reverse of the order they are registered, and a role cannot be
	// dropped while a database still grants it anything, so the role's is registered first
	// and therefore runs last.
	t.Cleanup(func() { drop(ctx, url, `drop role if exists `+name) })
	t.Cleanup(func() {
		drop(ctx, url, fmt.Sprintf(`drop database if exists %s with (force)`, name))
	})

	super := withDatabase(url, name)
	sc, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close(ctx)
	if _, err := db.Provision(ctx, sc, name, "test"); err != nil {
		t.Fatalf("the database could not be provisioned: %s", err)
	}
	return super
}

// drop runs one tidying statement and says nothing if it cannot: a test that has finished is
// not made to fail by a cluster that is already gone.
func drop(ctx context.Context, url, stmt string) {
	ctx = context.WithoutCancel(ctx)
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		return
	}
	defer c.Close(ctx)
	c.Exec(ctx, stmt)
}

// roleOf is the role that goes with a database, which is the database's own name.
func roleOf(super string) string {
	name := super
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.Index(name, "?"); i >= 0 {
		name = name[:i]
	}
	return name
}

// Superuser connects as the superuser, for the setting up and the reading back that a test has
// to do from outside the policies.
func Superuser(t *testing.T, super string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.WithoutCancel(t.Context())) })
	return conn
}

func withDatabase(url, name string) string {
	if i := strings.LastIndex(url, "/"); i > 0 {
		if j := strings.Index(url[i:], "?"); j > 0 {
			return url[:i+1] + name + url[i+j:]
		}
		return url[:i+1] + name
	}
	return url
}

func withCredentials(url, user, password string) string {
	i := strings.Index(url, "://")
	rest := url[i+3:]
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return url[:i+3] + user + ":" + password + "@" + rest
}
