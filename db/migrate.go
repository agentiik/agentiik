package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// migrations are the schema, in order, carried in the binary.
//
// Embedded rather than read from a directory so that an installation upgrades with the
// binary it is upgrading to, and a migration cannot be missing because somebody copied one
// file and not another.
//
//go:embed migrations/*.sql
var migrations embed.FS

// Migration is one file of the schema.
type Migration struct {
	// Name is the file, which begins with the number that orders it.
	Name string

	// SQL is what it does.
	SQL string
}

// Migrations are the schema in the order it is applied.
//
// The order is the file name's leading number, and it is lexical rather than numeric on
// purpose: the names are zero padded, so the two agree, and a name that broke the padding
// would sort visibly wrong in a directory listing rather than invisibly wrong here.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("db: the migrations could not be read: %w", err)
	}
	var out []Migration
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		body, err := fs.ReadFile(migrations, "migrations/"+entry.Name())
		if err != nil {
			return nil, fmt.Errorf("db: %s could not be read: %w", entry.Name(), err)
		}
		out = append(out, Migration{Name: entry.Name(), SQL: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// schemaTable records what has been applied. It is created by the migrator rather than by
// a migration, because a migration that created it would be the one migration nothing
// could record having applied.
const schemaTable = `
create table if not exists schema_migrations (
  name       text primary key,
  applied_at timestamptz not null default now()
)`

// Migrate applies what has not been applied, in order, and answers with what it did.
//
// Each migration runs in a transaction of its own and is recorded in the same one, so a
// migration that fails leaves the schema exactly as it was and the record agrees: an
// installation is upgraded rather than rebuilt, which is what this exists for.
//
// It takes a connection rather than a Pool because it is the one thing that legitimately
// runs as a role that may create tables, and that role is not the one the application
// connects as. Open would refuse it. An installation calls Provision, which calls this and
// then creates the role the application does connect as.
func Migrate(ctx context.Context, conn *pgx.Conn) ([]string, error) {
	if _, err := conn.Exec(ctx, schemaTable); err != nil {
		return nil, fmt.Errorf("db: the migration record could not be created: %w", err)
	}

	applied := map[string]bool{}
	rows, err := conn.Query(ctx, `select name from schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("db: the migration record could not be read: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: the migration record could not be read: %w", err)
		}
		applied[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: the migration record could not be read: %w", err)
	}

	all, err := Migrations()
	if err != nil {
		return nil, err
	}

	var ran []string
	for _, m := range all {
		if applied[m.Name] {
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return ran, fmt.Errorf("db: %s could not be begun: %w", m.Name, err)
		}
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			tx.Rollback(context.WithoutCancel(ctx))
			return ran, fmt.Errorf("db: %s failed and nothing it did was kept: %w", m.Name, err)
		}
		if _, err := tx.Exec(ctx, `insert into schema_migrations (name) values ($1)`, m.Name); err != nil {
			tx.Rollback(context.WithoutCancel(ctx))
			return ran, fmt.Errorf("db: %s could not be recorded, so it was not applied: %w", m.Name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return ran, fmt.Errorf("db: %s could not be committed: %w", m.Name, err)
		}
		ran = append(ran, m.Name)
	}
	return ran, nil
}
