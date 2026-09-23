package secret_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/secret"
)

// Reading a secret through the namespace's declaration of it, from whichever store that names.

func declared(t *testing.T, pool *db.Pool, namespace string, d db.Declaration) {
	t.Helper()
	d.DeclaredBy = "alice"
	if err := pool.In(t.Context(), namespace, func(ctx context.Context, ns *db.NS) error {
		_, _, err := ns.Declare(ctx, d)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// attached is an installation holding both stores, wired the way the server's main package wires
// them, and answers what a redemption reads through and what a declaration's PUT writes into.
func attached(t *testing.T, pool *db.Pool, keys *secret.Keyring, lookup func(string) (string, bool)) (api.Secrets, api.Values) {
	t.Helper()
	var declarations api.DeclarationOptions
	var runners api.RunnerOptions
	if err := secret.Attach(secret.Options{Pool: pool, Keys: keys, Environment: developing, Lookup: lookup}, &declarations, &runners); err != nil {
		t.Fatal(err)
	}
	if declarations.Values == nil || runners.Secrets == nil || len(declarations.Environment) != len(developing) {
		t.Fatalf("attaching filled the declarations with %+v and the runners with %+v", declarations, runners)
	}
	return runners.Secrets, declarations.Values
}

// Each secret is read from the store its declaration names, and moving the declaration moves where
// it is read from.
func TestASecretIsReadFromTheStoreItsDeclarationNames(t *testing.T) {
	pool, _ := stored(t)
	_, lookup, _ := theAPIsEnvironment()
	secrets, values := attached(t, pool, keyring(t, master(t, "2026-09")), lookup)

	declared(t, pool, "finance", db.Declaration{Name: "billing", Provider: api.ProviderBuiltin})
	declared(t, pool, "finance", db.Declaration{Name: "ledger", Provider: api.ProviderEnv, Path: "AGENTIIK_SECRET_FINANCE_LEDGER"})
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return values.Write(ctx, ns, "billing", []byte("bk_live_notreal"))
	}); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{"billing": "bk_live_notreal", "ledger": "gl_live_notreal"} {
		got, err := secrets.Value(t.Context(), "finance", name)
		if err != nil || string(got) != want {
			t.Errorf("finance/%s read as %q, %v, want %q", name, got, err, want)
		}
	}

	// The same names in another namespace are that namespace's: team-ops declares neither.
	for _, name := range []string{"billing", "ledger"} {
		if got, err := secrets.Value(t.Context(), "team-ops", name); !errors.Is(err, api.ErrNoSecret) {
			t.Errorf("team-ops/%s, which team-ops never declared, read as %q, %v", name, got, err)
		}
	}

	declared(t, pool, "finance", db.Declaration{Name: "billing", Provider: api.ProviderEnv, Path: "AGENTIIK_SECRET_FINANCE_LEDGER"})
	if got, err := secrets.Value(t.Context(), "finance", "billing"); err != nil || string(got) != "gl_live_notreal" {
		t.Errorf("billing, moved to a variable, read as %q, %v", got, err)
	}
}

// A declaration naming a store this installation does not read is refused when it is read, whether
// the store is one Agentiik knows and this installation did not configure or one it does not know
// at all, and so is a name the namespace never declared. Every refusal names the secret, and none
// carries a value, although every store the installation does read holds one.
func TestAStoreTheInstallationDoesNotReadIsRefusedNamingTheSecret(t *testing.T) {
	pool, conn := stored(t)
	variables, lookup, _ := theAPIsEnvironment()
	keys := keyring(t, master(t, "2026-09"))
	both, values := attached(t, pool, keys, lookup)

	declared(t, pool, "finance", db.Declaration{Name: "billing", Provider: api.ProviderBuiltin})
	declared(t, pool, "finance", db.Declaration{Name: "ledger", Provider: api.ProviderEnv, Path: "AGENTIIK_SECRET_FINANCE_LEDGER"})
	declared(t, pool, "finance", db.Declaration{Name: "pager", Provider: api.ProviderVault, Path: "kv/data/finance/pager"})
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return values.Write(ctx, ns, "billing", []byte("bk_live_notreal"))
	}); err != nil {
		t.Fatal(err)
	}
	// A store this version of Agentiik does not know, as a declaration written by a later one
	// would leave behind after a downgrade. The table refuses one today, so the check that
	// refuses it goes first.
	for _, stmt := range []string{
		`alter table secret_declarations drop constraint secret_declarations_provider_check`,
		`insert into secret_declarations (namespace, name, provider, path, declared_by)
		 values ('finance', 'keychain', 'keychain', 'login/finance', 'alice')`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}

	nothing, err := secret.NewProviders(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		what    string
		secrets api.Secrets
		name    string
		says    string
	}{
		{"the built-in store, not configured", nothing, "billing", "reads no secret from builtin"},
		{"the environment, not configured", nothing, "ledger", "reads no secret from env"},
		{"vault, which nothing reads yet", both, "pager", "reads no secret from vault"},
		{"a store nobody knows", both, "keychain", "not a store this version of Agentiik reads"},
		{"a name never declared", both, "stripe", "declares no secret stripe"},
	} {
		got, err := c.secrets.Value(t.Context(), "finance", c.name)
		if err == nil {
			t.Errorf("%s was read, as %q", c.what, got)
			continue
		}
		said := err.Error()
		if !strings.Contains(said, c.says) || !strings.Contains(said, "finance") || !strings.Contains(said, c.name) {
			t.Errorf("the refusal of %s reads %q", c.what, said)
		}
		for _, value := range append([]string{"bk_live_notreal"}, variables["AGENTIIK_SECRET_FINANCE_LEDGER"], variables["AGENTIIK_DATABASE_URL"]) {
			if strings.Contains(said, value) {
				t.Errorf("the refusal of %s carries a value: %q", c.what, said)
			}
		}
	}
	if _, err := both.Value(t.Context(), "finance", "stripe"); !errors.Is(err, api.ErrNoSecret) {
		t.Errorf("a name never declared answered %v, which is not ErrNoSecret", err)
	}
}

// The stores a reader is given are the ones a declaration can name, each of them there.
func TestProvidersAreGivenOnlyStoresADeclarationCanName(t *testing.T) {
	_, lookup, _ := theAPIsEnvironment()
	env, err := secret.NewEnv(developing, lookup)
	if err != nil {
		t.Fatal(err)
	}
	for what, by := range map[string]map[string]secret.Provider{
		"a store no declaration can name": {"keychain": env},
		"a store named and not there":     {api.ProviderEnv: nil},
	} {
		if _, err := secret.NewProviders(&db.Pool{}, by); err == nil {
			t.Errorf("%s was taken", what)
		}
	}
	if _, err := secret.NewProviders(nil, map[string]secret.Provider{api.ProviderEnv: env}); err == nil {
		t.Error("a reader with no database to read declarations from was built")
	}
}

// Attaching fills both halves of the API from one configuration, or neither: an installation with
// no keyring has no built-in store to write a value into, one that did not opt in has no
// environment a declaration may name, and options that already hold a store are not given another.
func TestAttachingFillsBothHalvesOrNeither(t *testing.T) {
	pool := &db.Pool{}

	var declarations api.DeclarationOptions
	var runners api.RunnerOptions
	if err := secret.Attach(secret.Options{Pool: pool}, &declarations, &runners); err != nil {
		t.Fatal(err)
	}
	if declarations.Values != nil || declarations.Environment != nil || runners.Secrets == nil {
		t.Errorf("an installation with neither store attached %+v to the declarations and %+v to the runners", declarations, runners)
	}

	for what, o := range map[string]struct {
		declarations api.DeclarationOptions
		runners      api.RunnerOptions
	}{
		"declarations that already write somewhere": {declarations: api.DeclarationOptions{Values: api.NoValues{}}},
		"declarations that already name variables":  {declarations: api.DeclarationOptions{Environment: developing}},
		"runners that already read somewhere":       {runners: api.RunnerOptions{Secrets: api.NoSecrets{}}},
	} {
		if err := secret.Attach(secret.Options{Pool: pool, Environment: developing}, &o.declarations, &o.runners); err == nil {
			t.Errorf("%s were given a second store", what)
		}
	}
	for what, o := range map[string]secret.Options{
		"no database":                         {Keys: keyring(t, master(t, "2026-09"))},
		"an environment that confines nobody": {Pool: pool, Environment: api.Environment{"finance": "AGENTIIK_", "team-ops": "AGENTIIK_SECRET_"}},
	} {
		var declarations api.DeclarationOptions
		var runners api.RunnerOptions
		if err := secret.Attach(o, &declarations, &runners); err == nil {
			t.Errorf("an installation with %s was attached", what)
		}
		if declarations.Values != nil || declarations.Environment != nil || runners.Secrets != nil {
			t.Errorf("an installation with %s was refused and half attached anyway", what)
		}
	}
}
