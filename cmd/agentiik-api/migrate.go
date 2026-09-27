package main

import (
	"context"
	"fmt"
	"io"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/config"
)

// migrateVerb is agentiik-api migrate.
func migrateVerb(ctx context.Context, lookup config.Lookup, stdout, stderr io.Writer) int {
	c, err := config.ReadMigration(lookup)
	if err != nil {
		fmt.Fprintf(stderr, "%s migrate: the configuration refuses the start:\n%s\n", program, err)
		return exitFailed
	}
	if err := migrateAndBootstrap(ctx, c, stdout); err != nil {
		fmt.Fprintf(stderr, "%s migrate: %s\n", program, err)
		return exitFailed
	}
	return exitStopped
}

// migrateAndBootstrap is the whole of migrate: the migrations and the role, then the built-in
// identity of every namespace made before v0.3.0, a line for each named after a word reserved
// since, the artifact files of the runs v0.2 finished,
// where AGK_OBJECTS_DIR names the store holding their envelopes, and the bootstrap token's hash, as
// init gives, records and keeps them, for an installation that runs no init.
//
// Homebrew's server and one put together by hand run migrate where a Compose file runs init, at
// every start, so migrate keeps the hash of the token their settings set, as init does. And an
// installation of v0.2 upgraded that way has the hash its operator token left in the file
// AGK_OPERATOR_TOKEN_FILE names and nowhere else, since no init ran to write it where the API reads
// it now: migrate imports it, once, so that the token goes on working through an upgrade that
// changes nothing but the programs.
func migrateAndBootstrap(ctx context.Context, c config.Migration, stdout io.Writer) error {
	if err := migrate(ctx, c, stdout); err != nil {
		return err
	}
	// As the application role, as init writes the hash, rather than over the connection that
	// migrated: db.Open refuses a role that walks through the policies, since a superuser is for
	// migrations and nothing else. migrate is run where the API runs, with its environment, as
	// namespace create is, which signs in as that role too.
	pool, err := db.Open(ctx, c.Application.ConnString())
	if err != nil {
		return fmt.Errorf("the database %s names could not be reached as %s: %w", config.DatabaseURL, c.Application.Role, err)
	}
	defer pool.Close()
	if err := builtInIdentities(ctx, pool, stdout); err != nil {
		return err
	}
	reservedLater(ctx, pool, "migrate", "", stdout)
	if err := unrecordedArtifacts(ctx, pool, c.Objects, "migrate", stdout); err != nil {
		return err
	}
	var imported func() ([]byte, error)
	if c.OperatorTokenFile != "" {
		imported = c.OperatorTokenHash
	}
	return bootstrapToken(ctx, pool, "migrate", c.OperatorToken, imported, stdout)
}

// migrate applies the migrations as the role that may change the schema, and creates or narrows
// the role the API and the controller connect as, which is what db.Provision does. It says what it
// applied, one migration a line, and which role is ready.
//
// A verb of the API rather than a program of its own, because it is the API's schema and the API's
// release that carries it: an installation upgrading runs the new release's migrate, then its
// serve. Running it twice applies nothing the second time and leaves the role as the first left it.
func migrate(ctx context.Context, c config.Migration, stdout io.Writer) error {
	conn, err := db.Connect(ctx, c.Admin.ConnString())
	if err != nil {
		return fmt.Errorf("the database %s names could not be reached: %w", config.MigrateDatabaseURL, err)
	}
	defer conn.Close(context.WithoutCancel(ctx))

	ran, err := db.Provision(ctx, conn, c.Application.Role, string(c.Application.Password))
	for _, name := range ran {
		fmt.Fprintf(stdout, "applied %s\n", name)
	}
	if err != nil {
		return err
	}
	if len(ran) == 0 {
		fmt.Fprintln(stdout, "every migration was already applied")
	}
	fmt.Fprintf(stdout, "%s is the role the API and the controller connect as, NOSUPERUSER NOBYPASSRLS\n", c.Application.Role)
	return nil
}
