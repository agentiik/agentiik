// Package dbtemplate keeps, on the PostgreSQL the tests share, one database migrated to the schema
// they carry, which each test's own database is copied from.
//
// A test has a database of its own, so that two tests cannot see each other's rows, and building
// one migration by migration took about 650 ms in 2026 against about 150 ms for a copy of one
// already built: the api package runs some five hundred tests that each start from a migrated
// database, and the migrations were most of its time. What a copy holds is what the migrations
// write into an empty database, which is what a test built one from before, so nothing a test can
// observe changes but how long it waited.
//
// It is a package of its own, with nothing of the application behind it, because package db tests
// itself against a real PostgreSQL too and cannot import dbtest, which imports it.
package dbtemplate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// prefix begins the name of every template, so that one built for a schema since changed is found
// and removed.
const prefix = "agk_template_"

// building is the advisory lock one test binary holds on the cluster's maintenance database while
// it looks for the template and builds it where it is missing, so that the packages go test runs at
// once build it once between them and every other one waits for it rather than building its own.
const building int64 = 0x61676b74706c // "agktpl"

// stale is how long a template of another schema is kept. Two checkouts of different branches
// testing at once on one machine each need theirs, and a template removed under a run that is
// still copying it fails every test after; one no run has built in a day is one no run is using.
const stale = 24 * time.Hour

var (
	mu    sync.Mutex
	built = map[string]string{}
)

// Named answers the name of the template for schema, building it on the cluster url names where it
// is not there yet: schema is what the migrations are, and names the template, so that a change to
// any of them builds another; migrate applies them to the connection it is given, which is the
// template's.
//
// The template is marked as one and refuses connections once built, since a copy is refused while
// anybody is connected to what it copies.
func Named(ctx context.Context, url string, schema []byte, migrate func(context.Context, *pgx.Conn) error) (string, error) {
	sum := sha256.Sum256(schema)
	name := prefix + hex.EncodeToString(sum[:])[:16]

	mu.Lock()
	defer mu.Unlock()
	if built[name] != "" {
		return name, nil
	}

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return "", err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if _, err := conn.Exec(ctx, `select pg_advisory_lock($1)`, building); err != nil {
		return "", fmt.Errorf("the lock on building %s could not be taken: %w", name, err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), `select pg_advisory_unlock($1)`, building)

	var ready bool
	switch err := conn.QueryRow(ctx, `select datistemplate from pg_database where datname = $1`, name).Scan(&ready); {
	case err == nil && ready:
		built[name] = name
		return name, nil
	case err == nil:
		// A build a killed run left half done, which is never marked, and is built again.
		if _, err := conn.Exec(ctx, `drop database `+name+` with (force)`); err != nil {
			return "", fmt.Errorf("the half-built %s could not be dropped: %w", name, err)
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("whether %s is built could not be read: %w", name, err)
	}

	if err := forgetStale(ctx, conn, name); err != nil {
		return "", err
	}
	if _, err := conn.Exec(ctx, `create database `+name); err != nil {
		return "", fmt.Errorf("%s could not be created: %w", name, err)
	}
	tc, err := pgx.Connect(ctx, withDatabase(url, name))
	if err != nil {
		return "", err
	}
	err = migrate(ctx, tc)
	tc.Close(context.WithoutCancel(ctx))
	if err != nil {
		return "", fmt.Errorf("%s could not be migrated: %w", name, err)
	}
	for _, stmt := range []string{
		fmt.Sprintf(`comment on database %s is '%s'`, name, time.Now().UTC().Format(time.RFC3339)),
		`alter database ` + name + ` with is_template true allow_connections false`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return "", fmt.Errorf("%s: %w", stmt, err)
		}
	}
	built[name] = name
	return name, nil
}

// forgetStale drops the templates of other schemas built longer ago than stale, their comment
// saying when, and those with no time to read, which no build of this package left.
func forgetStale(ctx context.Context, conn *pgx.Conn, keep string) error {
	rows, err := conn.Query(ctx, `
		select datname, coalesce(shobj_description(oid, 'pg_database'), '')
		from pg_database
		where datname like $1 and datname <> $2`, prefix+"%", keep)
	if err != nil {
		return fmt.Errorf("the templates of other schemas could not be listed: %w", err)
	}
	var gone []string
	for rows.Next() {
		var name, comment string
		if err := rows.Scan(&name, &comment); err != nil {
			rows.Close()
			return err
		}
		if at, err := time.Parse(time.RFC3339, comment); err != nil || time.Since(at) > stale {
			gone = append(gone, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, name := range gone {
		for _, stmt := range []string{
			`alter database ` + name + ` with is_template false`,
			`drop database if exists ` + name + ` with (force)`,
		} {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}
	}
	return nil
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
