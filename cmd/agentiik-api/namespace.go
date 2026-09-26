package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/config"
)

// namespaceActor is who an audit entry of this verb names. v0.2.0 has one principal, the
// operator, and the verb runs where the API runs, as whoever holds the installation's database
// settings, which is that operator.
const namespaceActor = "operator"

// namespaceVerb is agentiik-api namespace create NAME and agentiik-api namespace remove NAME.
func namespaceVerb(ctx context.Context, lookup config.Lookup, action, name string, stdout, stderr io.Writer) int {
	// The name is checked before the settings are read, so that a name no namespace can have is
	// refused as that wherever the verb is run.
	if action == "create" {
		if err := api.NamespaceName(name); err != nil {
			fmt.Fprintf(stderr, "%s namespace create: %s\n", program, err)
			return exitFailed
		}
	}
	c, err := config.ReadMigration(lookup)
	if err != nil {
		fmt.Fprintf(stderr, "%s namespace %s: the configuration refuses the start:\n%s\n", program, action, err)
		return exitFailed
	}
	if err := namespace(ctx, c.Application, action, name, stdout); err != nil {
		fmt.Fprintf(stderr, "%s namespace %s: %s\n", program, action, err)
		return exitFailed
	}
	return exitStopped
}

// namespace creates or removes one namespace, as the role the API connects as, and records it in
// the audit log in the same transaction. It says what it did.
//
// It reads the settings migrate reads and runs where migrate runs, because v0.2.0 has no route
// that creates a namespace: this verb stands in for v0.3.0's, which an administrator reaches
// through the API. Creating a namespace that exists changes nothing and says so, so that an
// installation script run twice succeeds twice.
func namespace(ctx context.Context, d config.Database, action, name string, stdout io.Writer) error {
	pool, err := db.Open(ctx, d.ConnString())
	if err != nil {
		return fmt.Errorf("the database %s names could not be reached as %s: %w", config.DatabaseURL, d.Role, err)
	}
	defer pool.Close()

	var created bool
	err = pool.Installation(ctx, db.NamespaceAdministration, func(ctx context.Context, w *db.Wide) error {
		r := audit.Record{Actor: namespaceActor, Target: name, Result: audit.Done}
		switch action {
		case "create":
			made, err := w.CreateNamespace(ctx, name)
			if err != nil {
				return err
			}
			created = made
			r.Action = audit.NamespaceCreate
			if !created {
				r.Result = audit.Unchanged
			}
		case "remove":
			if err := w.RemoveNamespace(ctx, name); err != nil {
				return err
			}
			r.Action = audit.NamespaceDelete
		default:
			return fmt.Errorf("%q is not something done to a namespace, which is created or removed", action)
		}
		return w.Audit(ctx, r)
	})
	var holds *db.NamespaceHolds
	switch {
	case errors.Is(err, db.ErrNoNamespace):
		return fmt.Errorf("there is no namespace %s, so nothing was removed", name)
	case errors.As(err, &holds):
		return fmt.Errorf("%s, so it was not removed", holds.Held())
	case err != nil:
		return err
	}
	switch {
	case action == "remove":
		fmt.Fprintf(stdout, "removed namespace %s\n", name)
	case created:
		fmt.Fprintf(stdout, "created namespace %s\n", name)
	default:
		fmt.Fprintf(stdout, "namespace %s already exists, and was left as it was\n", name)
	}
	return nil
}
