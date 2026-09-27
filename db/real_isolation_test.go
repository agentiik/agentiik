package db

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// "Object keys are prefixed by namespace, and every query path carries the namespace so that a
// missing filter fails closed rather than returning another tenant's rows", asked of a database the
// migrations built rather than of the files they are written in, so that a table added later by a
// migration nobody reads with this sentence in mind is found the day it arrives.

// namespacePolicy is the one predicate a table naming a namespace is read and written under, as
// PostgreSQL spells it back.
const namespacePolicy = `((namespace = agentiik_namespace()) OR agentiik_installation())`

// Every table naming a namespace, by a column of that name or by a reference to namespaces, has row
// level security enabled and forced, the owner included, and one policy for every command whose
// reading and writing halves are both the namespace's predicate. No such table may stand outside it:
// a table read across the installation is read through the installation's door, which the
// predicate's second arm opens, so none needs to be exempt, and one that is is a table whose rows a
// handle on one namespace reads for every namespace.
//
// A table naming namespaces as a list, the ones a pool or a runner accepts or a token reaches, is the
// installation's and says so here, with why: it belongs to no one namespace for a policy to keep it
// to, and a column of such names added anywhere else fails this test until somebody decides.
func TestEveryTableNamingANamespaceIsBehindItsPolicy(t *testing.T) {
	super, _ := database(t)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx, `
		select c.relname, c.relrowsecurity, c.relforcerowsecurity,
		       exists (select from pg_attribute a
		                where a.attrelid = c.oid and a.attname = 'namespace' and a.attnum > 0 and not a.attisdropped),
		       coalesce((select array_agg(p.polcmd::text || ' ' || p.polpermissive::text || ' ' ||
		                                  coalesce(pg_get_expr(p.polqual, p.polrelid), '') || ' ' ||
		                                  coalesce(pg_get_expr(p.polwithcheck, p.polrelid), ''))
		                   from pg_policy p where p.polrelid = c.oid), '{}')
		  from pg_class c join pg_namespace n on n.oid = c.relnamespace
		 where c.relkind in ('r', 'p') and n.nspname = current_schema()
		   and (exists (select from pg_attribute a
		                 where a.attrelid = c.oid and a.attname = 'namespace' and a.attnum > 0 and not a.attisdropped)
		        or exists (select from pg_constraint k
		                    where k.conrelid = c.oid and k.contype = 'f' and k.confrelid = 'namespaces'::regclass))
		 order by c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	type table struct {
		name                 string
		enabled, forced, col bool
		policies             []string
	}
	tables, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (table, error) {
		var tb table
		err := row.Scan(&tb.name, &tb.enabled, &tb.forced, &tb.col, &tb.policies)
		return tb, err
	})
	if err != nil {
		t.Fatal(err)
	}
	// The ones the Storage chapter names as a namespace's, and the four read across the installation
	// that name one all the same: a table dropped from the catalog, or renamed, is one this test no
	// longer asks about, and has to be seen to be.
	for _, want := range []string{"workflows", "runs", "tasks", "artifacts", "grants", "secret_declarations",
		"audit_log", "auth_policy", "notifications", "service_accounts"} {
		if !strings.Contains(" "+joinNames(tables, func(tb table) string { return tb.name })+" ", " "+want+" ") {
			t.Errorf("%s names no namespace in the catalog, and this test expects it to", want)
		}
	}
	// The columns of namespace names and scopes outside a table under the policy, and why each is.
	decided := map[string]string{
		"runner_pools.accepted_namespaces": "a pool serves the namespaces it accepts, and its inventory is an administrator's",
		"runners.accepted_namespaces":      "a runner serves the namespaces it accepts, and its inventory is an administrator's",
		"api_tokens.scope_within":          "a token names its principal before any namespace is in question, and may reach several",
	}
	named, err := conn.Query(ctx, `
		select c.relname || '.' || a.attname
		  from pg_attribute a join pg_class c on c.oid = a.attrelid join pg_namespace n on n.oid = c.relnamespace
		  join pg_type ty on ty.oid = a.atttypid
		 where n.nspname = current_schema() and c.relkind in ('r', 'p') and a.attnum > 0 and not a.attisdropped
		   and ty.typname in ('namespace_name', '_namespace_name', 'grant_scope', '_grant_scope')
		   and not c.relforcerowsecurity
		 order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	columns, err := pgx.CollectRows(named, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range columns {
		if decided[column] == "" {
			t.Errorf("%s names namespaces in a table outside the namespace's policy, and nobody decided why", column)
		}
	}
	if len(columns) != len(decided) {
		t.Errorf("the columns naming namespaces outside the policy are %v, and this test decided about %d", columns, len(decided))
	}

	whole := "* true " + namespacePolicy + " " + namespacePolicy
	for _, tb := range tables {
		switch {
		case !tb.col:
			t.Errorf("%s refers to namespaces with no column named namespace, which the policy reads", tb.name)
		case !tb.enabled || !tb.forced:
			t.Errorf("%s names a namespace and its row level security is enabled %v, forced %v: a handle on one namespace reads every namespace's rows of it", tb.name, tb.enabled, tb.forced)
		case len(tb.policies) != 1 || tb.policies[0] != whole:
			t.Errorf("%s is read and written under %q, and a table naming a namespace is under the namespace's policy alone, %q", tb.name, tb.policies, whole)
		}
	}
}

func joinNames[T any](all []T, name func(T) string) string {
	names := make([]string, len(all))
	for i, one := range all {
		names[i] = name(one)
	}
	return strings.Join(names, " ")
}

// The four tables read across the installation, each holding rows of two namespaces and of none,
// are read through a handle on one namespace as that namespace's rows alone, and a write through
// it naming another namespace is refused: a statement somebody writes against one of them through
// the wrong door reads nothing of anybody else.
func TestATableReadAcrossTheInstallationShowsOneNamespaceItsOwnRows(t *testing.T) {
	super, app := database(t)
	seed(t, super)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, stmt := range []string{
		`insert into principals (id, kind) values ('alice', 'user')`,
		`insert into users (login, display_name) values ('alice', 'Alice')`,
		`insert into principals (id, kind) values ('finance/nightly', 'service_account'), ('team-ops/nightly', 'service_account')`,
		`insert into service_accounts (namespace, name, created_by) values ('finance', 'nightly', 'alice'), ('team-ops', 'nightly', 'alice')`,
		`insert into auth_policy (namespace, min_passkeys) values ('finance', 3), ('team-ops', 3)`,
		`insert into notifications (id, recipient, kind, at, namespace, access_grant, act, acted_by) values
		   ('01M2AAAAAAAAAAAAAAAAAAAAA1', 'alice', 'admin_access_widened', now(), 'finance', '{}', 'granted', 'carol'),
		   ('01M2AAAAAAAAAAAAAAAAAAAAA2', 'alice', 'admin_access_widened', now(), 'team-ops', '{}', 'granted', 'carol')`,
		`insert into notifications (id, recipient, kind, at, credential) values
		   ('01M2AAAAAAAAAAAAAAAAAAAAA3', 'alice', 'passkey_counter_refused', now(), 'c3ZIeQ')`,
		`insert into audit_log (actor, action, namespace, target, result, detail) values
		   ('alice', 'run.cancel', 'finance', 'r1', 'done', '{}'),
		   ('alice', 'run.cancel', 'team-ops', 'r2', 'done', '{}'),
		   ('alice', 'runner.revoke', null, 'r3', 'done', '{}')`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}

	pool, err := Open(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// Each table holds a row of finance and one of team-ops, and each but service_accounts one of
	// no namespace, which only the installation's door reads.
	for table, held := range map[string]int{"audit_log": 3, "auth_policy": 3, "notifications": 3, "service_accounts": 2} {
		var own, all int
		err := pool.In(ctx, "finance", func(ctx context.Context, ns *NS) error {
			// No namespace in the statement: the policy is what filters.
			return ns.tx.QueryRow(ctx,
				`select count(*) filter (where namespace = 'finance'), count(*) from `+pgx.Identifier{table}.Sanitize()).Scan(&own, &all)
		})
		if err != nil {
			t.Fatal(err)
		}
		if own != 1 || all != 1 {
			t.Errorf("through finance's handle %s reads %d rows, %d of them finance's, and it holds one of finance's", table, all, own)
		}
		var every int
		err = pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
			return w.tx.QueryRow(ctx, `select count(*) from `+pgx.Identifier{table}.Sanitize()).Scan(&every)
		})
		if err != nil {
			t.Fatal(err)
		}
		if every != held {
			t.Errorf("through the installation's door %s reads %d rows, and it holds %d", table, every, held)
		}
	}

	for _, c := range []struct{ what, stmt string }{
		{"an audit entry", `insert into audit_log (actor, action, namespace, target, result, detail)
			values ('alice', 'run.cancel', 'team-ops', 'r4', 'done', '{}')`},
		{"a notification", `insert into notifications (id, recipient, kind, at, namespace, access_grant, act, acted_by)
			values ('01M2AAAAAAAAAAAAAAAAAAAAA4', 'alice', 'admin_access_widened', now(), 'team-ops', '{}', 'granted', 'carol')`},
		{"a namespace's policy", `update auth_policy set min_passkeys = 4 where namespace = 'team-ops'`},
		{"a service account", `update service_accounts set created_by = 'mallory' where namespace = 'team-ops'`},
	} {
		var changed int64
		err := pool.In(ctx, "finance", func(ctx context.Context, ns *NS) error {
			tag, err := ns.tx.Exec(ctx, c.stmt)
			changed = tag.RowsAffected()
			return err
		})
		if err == nil && changed != 0 {
			t.Errorf("%s was written into team-ops through finance's handle", c.what)
		}
	}
}
