package secret_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/secret"
)

// The API's environment as a store, for development: read only for a namespace the installation
// opted in, only under the prefix it gave that namespace, and never a variable of the API's own.

// developing is an installation that opted both namespaces in, each under a prefix of its own.
var developing = api.Environment{"finance": "AGENTIIK_SECRET_FINANCE_", "team-ops": "AGENTIIK_SECRET_TEAM_OPS_"}

// theAPIsEnvironment is the whole environment of an API process, the variables a namespace may
// read beside the ones it must never reach, and the lookup it is read through, which records what
// it was asked for.
func theAPIsEnvironment() (map[string]string, func(string) (string, bool), *[]string) {
	variables := map[string]string{
		"AGENTIIK_SECRET_FINANCE_LEDGER":  "gl_live_notreal",
		"AGENTIIK_SECRET_FINANCE_EMPTY":   "",
		"AGENTIIK_SECRET_TEAM_OPS_PAGER":  "pg_live_notreal",
		"AGENTIIK_DATABASE_URL":           "postgres://agk:hunter2@db/agk",
		"AGENTIIK_SECRET_FINANCEX_LEDGER": "fx_live_notreal",
	}
	var asked []string
	return variables, func(name string) (string, bool) {
		asked = append(asked, name)
		value, set := variables[name]
		return value, set
	}, &asked
}

func inTheEnvironment(name, variable string) db.Declaration {
	return db.Declaration{Name: name, Provider: api.ProviderEnv, Path: variable}
}

// A namespace reads a variable under its own prefix, and no other: not another namespace's, not
// one that merely begins like its prefix, and not the API's own. A refusal is decided before the
// environment is asked anything, names the secret, and carries no value of any variable.
func TestTheEnvironmentIsReadOnlyUnderTheNamespacesPrefix(t *testing.T) {
	variables, lookup, asked := theAPIsEnvironment()
	env, err := secret.NewEnv(developing, lookup)
	if err != nil {
		t.Fatal(err)
	}

	got, err := env.Read(t.Context(), "finance", inTheEnvironment("ledger", "AGENTIIK_SECRET_FINANCE_LEDGER"))
	if err != nil || string(got) != "gl_live_notreal" {
		t.Fatalf("finance reading its own variable answered %q, %v", got, err)
	}
	got, err = env.Read(t.Context(), "team-ops", inTheEnvironment("pager", "AGENTIIK_SECRET_TEAM_OPS_PAGER"))
	if err != nil || string(got) != "pg_live_notreal" {
		t.Fatalf("team-ops reading its own variable answered %q, %v", got, err)
	}

	*asked = nil
	for what, c := range map[string]struct {
		namespace string
		d         db.Declaration
	}{
		"the API's own variable":                 {"finance", inTheEnvironment("database", "AGENTIIK_DATABASE_URL")},
		"another namespace's variable":           {"finance", inTheEnvironment("pager", "AGENTIIK_SECRET_TEAM_OPS_PAGER")},
		"a variable that begins like the prefix": {"finance", inTheEnvironment("ledger", "AGENTIIK_SECRET_FINANCEX_LEDGER")},
		"a namespace the installation left out":  {"payroll", inTheEnvironment("ledger", "AGENTIIK_SECRET_FINANCE_LEDGER")},
		"a name no variable can have":            {"finance", inTheEnvironment("ledger", "AGENTIIK_SECRET_FINANCE_LEDGER=1")},
		"a declaration of the built-in store":    {"finance", db.Declaration{Name: "ledger", Provider: api.ProviderBuiltin}},
		"a path in another store":                {"finance", db.Declaration{Name: "ledger", Provider: api.ProviderVault, Path: "AGENTIIK_SECRET_FINANCE_LEDGER"}},
	} {
		got, err := env.Read(t.Context(), c.namespace, c.d)
		if err == nil {
			t.Errorf("%s was read, as %q", what, got)
			continue
		}
		said := err.Error()
		if !strings.Contains(said, c.namespace+"/"+c.d.Name) {
			t.Errorf("the refusal of %s does not name the secret: %q", what, said)
		}
		for _, value := range variables {
			if value != "" && strings.Contains(said, value) {
				t.Errorf("the refusal of %s carries a value: %q", what, said)
			}
		}
	}
	if len(*asked) != 0 {
		t.Errorf("refusing what a namespace may not read asked the environment for %v", *asked)
	}

	// A variable of its own that is not set, or set to nothing, is not held: a step would
	// otherwise be given an empty file where it expects a credential.
	_, err = env.Read(t.Context(), "finance", inTheEnvironment("missing", "AGENTIIK_SECRET_FINANCE_MISSING"))
	if !errors.Is(err, api.ErrNoSecret) || !strings.Contains(err.Error(), "finance/missing") {
		t.Errorf("a variable the environment does not set answered %v", err)
	}
	if _, err := env.Read(t.Context(), "finance", inTheEnvironment("empty", "AGENTIIK_SECRET_FINANCE_EMPTY")); err == nil {
		t.Error("a variable set to nothing was read")
	}
	if !slices.Equal(*asked, []string{"AGENTIIK_SECRET_FINANCE_MISSING", "AGENTIIK_SECRET_FINANCE_EMPTY"}) {
		t.Errorf("the environment was asked for %v", *asked)
	}
}

// The environment is a store only where the installation opted in, and only with prefixes that
// keep each namespace to its own variables.
func TestAnEnvironmentThatConfinesNobodyIsNoStore(t *testing.T) {
	_, lookup, _ := theAPIsEnvironment()
	for what, e := range map[string]api.Environment{
		"no environment at all":                       nil,
		"an environment giving no namespace a prefix": {},
		"a prefix that begins another":                {"finance": "AGENTIIK_SECRET_FIN", "team-ops": "AGENTIIK_SECRET_FINANCE_"},
		"a prefix that is no variable's beginning":    {"finance": "AGENTIIK SECRET"},
	} {
		if _, err := secret.NewEnv(e, lookup); err == nil {
			t.Errorf("%s was taken", what)
		}
	}

	// And what it was built from is its own: the installation's map changed afterwards does not
	// widen what it reads.
	opted := api.Environment{"finance": "AGENTIIK_SECRET_FINANCE_"}
	env, err := secret.NewEnv(opted, lookup)
	if err != nil {
		t.Fatal(err)
	}
	opted["finance"] = "AGENTIIK_"
	if _, err := env.Read(t.Context(), "finance", inTheEnvironment("database", "AGENTIIK_DATABASE_URL")); err == nil {
		t.Error("the environment read a variable its own prefix never covered")
	}
}
