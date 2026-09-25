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
	if err := migrate(ctx, c, stdout); err != nil {
		fmt.Fprintf(stderr, "%s migrate: %s\n", program, err)
		return exitFailed
	}
	return exitStopped
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
