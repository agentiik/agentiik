package db

import (
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
