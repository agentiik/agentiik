package db

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A namespace's secret declarations, against a real PostgreSQL: what is kept, who sees it, and
// that nothing kept here could be a value.

func declare(t *testing.T, pool *Pool, namespace string, d Declaration) (bool, error) {
	t.Helper()
	var created bool
	err := pool.In(t.Context(), namespace, func(ctx context.Context, ns *NS) error {
		var err error
		created, err = ns.Declare(ctx, d)
		return err
	})
	return created, err
}

func declarationsOf(t *testing.T, pool *Pool, namespace string) []Declaration {
	t.Helper()
	var out []Declaration
	if err := pool.In(t.Context(), namespace, func(ctx context.Context, ns *NS) error {
		var err error
		out, err = ns.Declarations(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// A declaration is written, read back, moved and removed, one secret at a time.
func TestADeclarationRoundTrips(t *testing.T) {
	pool, _ := opened(t)

	for _, d := range []Declaration{
		{Name: "ledger", Provider: "env", Path: "AGENTIIK_SECRET_FINANCE_LEDGER", DeclaredBy: "alice"},
		{Name: "billing", Provider: "builtin", DeclaredBy: "alice"},
	} {
		created, err := declare(t, pool, "finance", d)
		if err != nil {
			t.Fatal(err)
		}
		if !created {
			t.Errorf("%s was declared for the first time and reported as replaced", d.Name)
		}
	}

	got := declarationsOf(t, pool, "finance")
	if len(got) != 2 || got[0].Name != "billing" || got[1].Name != "ledger" {
		t.Fatalf("the namespace declares %+v, want billing and ledger in that order", got)
	}
	if got[0].Provider != "builtin" || got[0].Path != "" {
		t.Errorf("billing reads back as %+v", got[0])
	}
	if got[1].Provider != "env" || got[1].Path != "AGENTIIK_SECRET_FINANCE_LEDGER" || got[1].DeclaredBy != "alice" || got[1].DeclaredAt.IsZero() {
		t.Errorf("ledger reads back as %+v", got[1])
	}

	// Declared again elsewhere is the same secret moved, and the answer says it was not new.
	created, err := declare(t, pool, "finance", Declaration{Name: "ledger", Provider: "vault", Path: "kv/data/finance/ledger", DeclaredBy: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("a secret declared a second time was reported as new")
	}
	var one Declaration
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		var err error
		one, err = ns.Declaration(ctx, "ledger")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if one.Provider != "vault" || one.Path != "kv/data/finance/ledger" || one.DeclaredBy != "bob" {
		t.Errorf("the declaration moved to %+v", one)
	}

	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.Undeclare(ctx, "ledger")
	}); err != nil {
		t.Fatal(err)
	}
	err = pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		_, err := ns.Declaration(ctx, "ledger")
		return err
	})
	if !errors.Is(err, ErrNoDeclaration) {
		t.Errorf("a removed declaration reads as %v", err)
	}
	err = pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.Undeclare(ctx, "ledger")
	})
	if !errors.Is(err, ErrNoDeclaration) {
		t.Errorf("removing a declaration twice answered %v", err)
	}
	if got := declarationsOf(t, pool, "finance"); len(got) != 1 || got[0].Name != "billing" {
		t.Errorf("after the removal the namespace declares %+v", got)
	}
}

// One namespace cannot see, move or remove another's declarations, and cannot plant one in it.
func TestAnotherNamespacesDeclarationsAreInvisible(t *testing.T) {
	pool, _ := opened(t)
	if _, err := declare(t, pool, "finance", Declaration{Name: "billing", Provider: "vault", Path: "kv/data/finance/billing", DeclaredBy: "alice"}); err != nil {
		t.Fatal(err)
	}

	if got := declarationsOf(t, pool, "team-ops"); len(got) != 0 {
		t.Fatalf("team-ops reads %+v, which finance declared", got)
	}
	err := pool.In(t.Context(), "team-ops", func(ctx context.Context, ns *NS) error {
		_, err := ns.Declaration(ctx, "billing")
		return err
	})
	if !errors.Is(err, ErrNoDeclaration) {
		t.Errorf("team-ops reading finance's declaration answered %v", err)
	}
	err = pool.In(t.Context(), "team-ops", func(ctx context.Context, ns *NS) error {
		return ns.Undeclare(ctx, "billing")
	})
	if !errors.Is(err, ErrNoDeclaration) {
		t.Errorf("team-ops removing finance's declaration answered %v", err)
	}

	// The same name declared in team-ops is team-ops' own, and finance's is left as it was.
	if _, err := declare(t, pool, "team-ops", Declaration{Name: "billing", Provider: "builtin", DeclaredBy: "bob"}); err != nil {
		t.Fatal(err)
	}
	if got := declarationsOf(t, pool, "finance"); len(got) != 1 || got[0].Provider != "vault" || got[0].DeclaredBy != "alice" {
		t.Errorf("finance's declaration became %+v when team-ops declared the same name", got)
	}

	// And a row naming another namespace, written past the methods, is refused by the policy.
	err = pool.In(t.Context(), "team-ops", func(ctx context.Context, ns *NS) error {
		_, err := ns.tx.Exec(ctx,
			`insert into secret_declarations (namespace, name, provider, path, declared_by)
			 values ('finance', 'planted', 'vault', 'kv/data/team-ops/mine', 'mallory')`)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("a declaration planted in another namespace answered %v", err)
	}
}

// A declaration into a namespace nobody created says so, rather than failing as a database error.
func TestADeclarationInANamespaceNobodyCreatedIsRefused(t *testing.T) {
	pool, _ := opened(t)
	_, err := declare(t, pool, "nowhere", Declaration{Name: "billing", Provider: "builtin", DeclaredBy: "alice"})
	if !errors.Is(err, ErrNoNamespace) {
		t.Fatalf("a declaration in a namespace nobody created answered %v", err)
	}
}

// The table has the columns of a declaration and no other, so there is no column a value could be
// written into; and what the columns hold is held by the table itself, whatever writes to it.
func TestTheDeclarationsTableHoldsNoValue(t *testing.T) {
	pool, super := opened(t)

	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	rows, err := conn.Query(t.Context(),
		`select column_name from information_schema.columns
		 where table_name = 'secret_declarations' order by column_name`)
	if err != nil {
		t.Fatal(err)
	}
	columns, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"declared_at", "declared_by", "name", "namespace", "path", "provider"}
	if !slices.Equal(columns, want) {
		t.Fatalf("secret_declarations has the columns %v, and a declaration is %v", columns, want)
	}

	for what, d := range map[string]Declaration{
		"a provider the installation does not know": {Name: "billing", Provider: "keychain", Path: "billing", DeclaredBy: "alice"},
		"the built-in store with a path":            {Name: "billing", Provider: "builtin", Path: "finance/billing", DeclaredBy: "alice"},
		"an environment variable with no name":      {Name: "billing", Provider: "env", DeclaredBy: "alice"},
		"a name that is a path":                     {Name: "kv/billing", Provider: "builtin", DeclaredBy: "alice"},
	} {
		if _, err := declare(t, pool, "finance", d); err == nil {
			t.Errorf("%s was declared", what)
		}
	}
}
