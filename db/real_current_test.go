package db

import (
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A program refuses a database that lacks a migration it carries, naming the first, rather than
// serving and answering 500 on whatever reaches what the migration changed: a namespace created on a
// database without 0071_unbounded_retention.sql writes a null its column still refuses.
func TestAProgramRefusesADatabaseLackingAMigrationItCarries(t *testing.T) {
	super, app := database(t)
	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Current(t.Context()); err != nil {
		t.Fatalf("a database holding every migration was refused: %s", err)
	}

	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	last := all[len(all)-1].Name
	if _, err := conn.Exec(t.Context(), `delete from schema_migrations where name = $1`, last); err != nil {
		t.Fatal(err)
	}
	err = pool.Current(t.Context())
	if err == nil || !strings.Contains(err.Error(), "the database lacks "+last+", a migration this binary carries: run agentiik-api init") {
		t.Fatalf("a database lacking %s was answered %v, want it named with what to run", last, err)
	}

	if _, err := conn.Exec(t.Context(), `delete from schema_migrations where name = $1`, all[len(all)-2].Name); err != nil {
		t.Fatal(err)
	}
	err = pool.Current(t.Context())
	if want := "the database lacks " + all[len(all)-2].Name + " and 1 more of the migrations"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("a database lacking two migrations was answered %v, want %q", err, want)
	}
}

// A program given a connection that may change the schema applies what its database lacks before it
// starts, as an installation upgraded by replacing the API and the controller alone needs, and finds
// nothing to apply on the next start. Given none, it refuses as before, and changes nothing.
func TestAProgramAppliesWhatItsDatabaseLacksBeforeItStarts(t *testing.T) {
	super, role := blank(t)
	ctx := t.Context()
	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := MigrateThrough(ctx, conn, all[len(all)-3].Name); err != nil {
		t.Fatal(err)
	}
	// The role as the release before made it, holding what Current asks of it and nothing more.
	quoted := pgx.Identifier{role}.Sanitize()
	for _, stmt := range []string{
		`create role ` + quoted + ` login password 'test'`,
		`grant usage on schema public to ` + quoted,
		`grant select on schema_migrations to ` + quoted,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := Open(ctx, withCredentials(super, role, "test"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	lacking := all[len(all)-2:]
	ran, err := pool.CatchUp(ctx, "", role, "test")
	if want := "the database lacks " + lacking[0].Name + " and 1 more"; err == nil || !strings.Contains(err.Error(), want) || len(ran) != 0 {
		t.Fatalf("a program given no connection to migrate as applied %v and answered %v, want %q", ran, err, want)
	}

	ran, err = pool.CatchUp(ctx, super, role, "test")
	if err != nil {
		t.Fatalf("a program given a connection to migrate as was refused: %s", err)
	}
	if want := []string{lacking[0].Name, lacking[1].Name}; !slices.Equal(ran, want) {
		t.Fatalf("a program applied %v, want %v", ran, want)
	}
	if err := pool.Current(ctx); err != nil {
		t.Fatalf("the database is still refused after the program applied what it lacked: %s", err)
	}

	ran, err = pool.CatchUp(ctx, super, role, "test")
	if err != nil || len(ran) != 0 {
		t.Fatalf("the next start applied %v and answered %v, want nothing", ran, err)
	}
}
