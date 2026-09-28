package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/config"
)

// namespaceActor is who an audit entry of this verb names, and of init's own acts: installation,
// the installation itself, as the author of the pool default is written. The verb runs where the
// API runs, as whoever holds the installation's database settings, which is no principal a grant
// names; and operator names the bootstrap token from v0.3.0, which an act of init's is not, before
// the first administrator has enrolled or after.
const namespaceActor = "installation"

// namespaceVerb is agentiik-api namespace create NAME and agentiik-api namespace remove NAME.
func namespaceVerb(ctx context.Context, lookup config.Lookup, action, name string, stdout, stderr io.Writer) int {
	// The name is checked before the settings are read, so that a name no namespace can have is
	// refused as that wherever the verb is run. A word reserved late is decided once the database
	// says whether a namespace carries it already (namespace).
	if action == "create" {
		if err := api.NamespaceRef(name); err != nil {
			fmt.Fprintf(stderr, "%s namespace create: %s\n", program, err)
			return exitFailed
		}
	}
	d, err := config.ReadNamespace(lookup)
	if err != nil {
		fmt.Fprintf(stderr, "%s namespace %s: the configuration refuses the start:\n%s\n", program, action, err)
		return exitFailed
	}
	if err := namespace(ctx, d, action, name, stdout); err != nil {
		fmt.Fprintf(stderr, "%s namespace %s: %s\n", program, action, err)
		return exitFailed
	}
	return exitStopped
}

// loginHoldsName is a namespace not created because a user's login is its name.
type loginHoldsName string

// reservedName is a namespace not created because its name is a word reserved late, which no
// namespace of the installation carries yet: api.NamespaceName's refusal.
type reservedName struct{ error }

func (r reservedName) Unwrap() error { return r.error }

func (name loginHoldsName) Error() string {
	return fmt.Sprintf("%s is a user's login, and logins and namespace names share one name space, so no namespace %s was created", string(name), string(name))
}

// namespace creates or removes one namespace, as the role the API connects as, and records it in
// the audit log in the same transaction. It says what it did.
//
// It reads AGK_DATABASE_URL and AGK_DATABASE_PASSWORD_FILE, as serve does, and runs where the API
// runs, for an installation script and for init, which create a namespace before anybody could ask
// the API for one; an administrator creates and removes them through /api/v1/namespaces. Both write
// through the same store and refuse the same names, and a namespace is created with its built-in
// identity either way. Creating a namespace that exists changes nothing and says so, so that an
// installation script run twice succeeds twice.
//
// A word reserved late (agk.LateReservations) is refused as a new namespace's name, as the API
// refuses it, and a namespace created under it before it was reserved is one that exists: left as
// it was, so that an installation whose init names it goes on starting after the upgrade that
// reserved the word.
func namespace(ctx context.Context, d config.Database, action, name string, stdout io.Writer) error {
	pool, err := db.Open(ctx, d.ConnString())
	if err != nil {
		return fmt.Errorf("the database %s names could not be reached as %s: %w", config.DatabaseURL, d.Role, err)
	}
	defer pool.Close()

	var created bool
	switch action {
	case "create":
		err = pool.Installation(ctx, db.NamespaceAdministration, func(ctx context.Context, w *db.Wide) error {
			if _, late := agk.ReservedLate(name); late {
				_, err := w.NamespaceNamed(ctx, name)
				switch {
				case errors.Is(err, db.ErrNoNamespace):
					return reservedName{api.NamespaceName(name)}
				case err != nil:
					return err
				}
			}
			var err error
			if created, err = w.CreateNamespace(ctx, db.Namespace{Name: name}); err != nil {
				return err
			}
			r := audit.Record{Actor: namespaceActor, Action: audit.NamespaceCreate, Target: name, Result: audit.Done}
			if !created {
				r.Result = audit.Unchanged
			}
			return w.Audit(ctx, r)
		})
	case "remove":
		err = api.RemoveNamespace(ctx, pool, name, namespaceActor)
	default:
		return fmt.Errorf("%q is not something done to a namespace, which is created or removed", action)
	}
	var holds *db.NamespaceHolds
	switch {
	case errors.Is(err, db.ErrNoNamespace):
		return fmt.Errorf("there is no namespace %s, so nothing was removed", name)
	case errors.Is(err, api.ErrPersonalNamespace):
		return fmt.Errorf("%s, so it was not removed", api.PersonalRefusal(name))
	case errors.Is(err, db.ErrNameTaken):
		return loginHoldsName(name)
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

// builtInIdentities gives every namespace that has no built-in identity, NS/agentiik, its own, as
// the role the API connects as, and records each in the audit log, in the namespace, by
// installation. It says which it gave one, and nothing where none lacked it.
//
// A namespace is created with its built-in identity from v0.3.0, so the ones that lack it are those
// v0.2 made, which an upgrade keeps: init and migrate run this at every run, since one of the two
// runs wherever an installation is upgraded, and the runs nobody started there are attributed to
// that identity. Nothing is granted to it.
func builtInIdentities(ctx context.Context, pool *db.Pool, stdout io.Writer) error {
	var given []string
	err := pool.Installation(ctx, db.Identity, func(ctx context.Context, w *db.Wide) error {
		var err error
		if given, err = w.GiveBuiltInIdentities(ctx); err != nil {
			return err
		}
		for _, name := range given {
			if err := w.AuditIn(ctx, name, audit.Record{
				Actor: namespaceActor, Action: audit.ServiceAccountCreate, Target: name + "/" + db.BuiltIn, Result: audit.Done,
				Detail: map[string]any{"built_in": true},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("the namespaces made before v0.3.0 could not be given their built-in identities: %w", err)
	}
	for _, name := range given {
		fmt.Fprintf(stdout, "gave namespace %s its built-in identity, %s/%s, which holds no grant until an owner gives it one\n", name, name, db.BuiltIn)
	}
	return nil
}

// reservedLater says, in one line for each namespace named after a word reserved since it was
// created (agk.LateReservations), that it keeps its name and is served as before, with nothing to
// do: nothing renames a namespace, an upgrade asks nothing beyond compose.yaml and .env, and the
// route the word was reserved for is served at its own path alone, which no route of a namespace
// takes. init and migrate say it at every run while the namespace exists, since one of the two runs
// wherever an installation is upgraded, so that whoever reads either knows why that name is
// refused to anything new.
//
// It never fails: what it reads decides nothing, and a failed init keeps every service of the
// installation from starting. A read that fails is said, and the next run tries again.
func reservedLater(ctx context.Context, pool *db.Pool, verb string, stdout io.Writer) {
	var held []agk.LateReservation
	err := pool.Installation(ctx, db.NamespaceAdministration, func(ctx context.Context, w *db.Wide) error {
		held = held[:0]
		for _, r := range agk.LateReservations {
			_, err := w.NamespaceNamed(ctx, r.Word)
			switch {
			case errors.Is(err, db.ErrNoNamespace):
			case err != nil:
				return err
			default:
				held = append(held, r)
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stdout, "could not tell whether a namespace is named after a word reserved since it was created, and %s goes on: %v\n", verb, err)
		return
	}
	for _, r := range held {
		fmt.Fprintf(stdout, "namespace %s keeps its name and is served as before, with nothing to do: %s is reserved from %s, so no new namespace, login, group or service account takes it\n", r.Word, r.Word, r.Since)
	}
}
