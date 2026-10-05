package db

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/audit"
	"github.com/jackc/pgx/v5"
)

// A namespace renamed carries everything at once: its workflows with their versions, runs, steps,
// tasks and artifacts, its grants, its secrets and their values, its service accounts with their
// tokens and their grants in other namespaces, and every record elsewhere naming it or one of its
// service accounts. Its storage name stays the one it was created with, the name it left is its
// former name, and the audit log is not written.
func TestARenamedNamespaceCarriesEveryRowNamingIt(t *testing.T) {
	pool, super := opened(t)
	conn := superuser(t, super)
	ctx := t.Context()
	for _, stmt := range []string{
		`update runs set state = 'succeeded', started_at = now(), finished_at = now()`,
		`insert into principals (id, kind) values ('alice', 'user'), ('bob', 'user')`,
		`insert into users (login, display_name) values ('alice', 'Alice'), ('bob', 'Bob')`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	administer := func(fn func(context.Context, *Wide) error) error {
		return pool.Installation(ctx, NamespaceAdministration, fn)
	}
	if err := administer(func(ctx context.Context, w *Wide) error {
		for _, s := range []ServiceAccount{{Namespace: "finance", Name: BuiltIn}, {Namespace: "finance", Name: "ci", CreatedBy: "alice"}} {
			if err := w.CreateServiceAccount(ctx, s); err != nil {
				return err
			}
		}
		for _, g := range []access.Grant{
			{ID: "01M2GRANTAAAAAAAAAAAAAAAAA", Principal: "bob", Scope: access.Scope{Namespace: "finance"}, Role: access.Viewer, GrantedBy: "finance/ci"},
			{ID: "01M2GRANTBBBBBBBBBBBBBBBBB", Principal: "finance/ci", Scope: access.Scope{Namespace: "team-ops"}, Role: access.Editor, GrantedBy: "alice"},
			{ID: "01M2GRANTCCCCCCCCCCCCCCCCC", Principal: "finance/agentiik", Scope: access.Scope{Namespace: "team-ops", Workflow: "nightly"}, Role: access.Viewer, GrantedBy: "alice"},
		} {
			if err := w.GrantAccess(ctx, g); err != nil {
				return err
			}
		}
		now := time.Now().UTC().Truncate(time.Second)
		if err := w.MintToken(ctx, APIToken{
			ID: "01M2T0KENAAAAAAAAAAAAAAAAA", Hash: make([]byte, 32), Principal: "finance/ci",
			Within: []string{"finance", "finance/monthly-invoicing", "team-ops"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			return err
		}
		if err := w.AuditIn(ctx, "finance", audit.Record{Actor: "alice", Action: audit.SecretWrite, Target: "ledger", Result: audit.Done}); err != nil {
			return err
		}
		return w.CreateRunnerPool(ctx, RunnerPool{Name: "finance-pool", AcceptedNamespaces: []string{"team-ops", "finance"}, CreatedBy: "alice"})
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.In(ctx, "finance", func(ctx context.Context, ns *NS) error {
		if _, _, err := ns.Declare(ctx, Declaration{Name: "ledger", Provider: "builtin", DeclaredBy: "finance/ci"}); err != nil {
			return err
		}
		return ns.WriteSealed(ctx, "ledger", func(version int) (SealedValue, error) {
			return SealedValue{Version: version, Master: "m1", Salt: []byte("s"), WrappedKey: []byte("k"), WrapNonce: []byte("n"), Ciphertext: []byte("c"), Nonce: []byte("n")}, nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`update runs set triggered_by = 'finance/ci' where namespace = 'team-ops'`,
		`insert into triggers (namespace, workflow, position, kind, commit, declared, hears, armed_by)
		   values ('team-ops', 'nightly', 0, 'event', repeat('b', 40), '{"namespace":"finance"}', 'finance', 'finance/agentiik')`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}

	var changed bool
	if err := administer(func(ctx context.Context, w *Wide) error {
		var err error
		changed, err = w.RenameNamespace(ctx, "finance", "accounting")
		return err
	}); err != nil || !changed {
		t.Fatalf("the rename answered %v, %v", changed, err)
	}

	for what, query := range map[string]string{
		"its workflow":                    `select count(*) from workflows where namespace = 'accounting' and name = 'monthly-invoicing'`,
		"its version":                     `select count(*) from workflow_versions where namespace = 'accounting'`,
		"its run":                         `select count(*) from runs where namespace = 'accounting'`,
		"its steps":                       `select count(*) / 2 from steps where namespace = 'accounting'`,
		"its task":                        `select count(*) from tasks where namespace = 'accounting'`,
		"its grant":                       `select count(*) from grants where namespace = 'accounting' and principal = 'bob' and granted_by = 'accounting/ci'`,
		"its service account's grant":     `select count(*) from grants where namespace = 'team-ops' and principal = 'accounting/ci'`,
		"its identity's grant":            `select count(*) from grants where namespace = 'team-ops' and principal = 'accounting/agentiik'`,
		"its secret's declaration":        `select count(*) from secret_declarations where namespace = 'accounting' and declared_by = 'accounting/ci'`,
		"its secret's value":              `select count(*) from secret_values where namespace = 'accounting' and version = 1`,
		"its service accounts":            `select count(*) / 2 from service_accounts where namespace = 'accounting' and principal like 'accounting/%'`,
		"their principals":                `select count(*) / 2 from principals where id in ('accounting/ci', 'accounting/agentiik')`,
		"the token":                       `select count(*) from api_tokens where principal = 'accounting/ci' and scope_within = '{accounting,accounting/monthly-invoicing,team-ops}'::grant_scope[]`,
		"a run its service account began": `select count(*) from runs where namespace = 'team-ops' and triggered_by = 'accounting/ci'`,
		"the event trigger hearing it":    `select count(*) from triggers where hears = 'accounting' and armed_by = 'accounting/agentiik'`,
		"the pool accepting it":           `select count(*) from runner_pools where accepted_namespaces = '{team-ops,accounting}'::namespace_name[]`,
		"its storage and former name":     `select count(*) from namespaces where name = 'accounting' and storage = 'finance' and former_names = '{finance}'`,
		"the audit log, as it was":        `select count(*) from audit_log where namespace = 'finance' and target = 'ledger'`,
	} {
		var n int
		if err := conn.QueryRow(ctx, query).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s: %d rows answer the new name, %v", what, n, err)
		}
	}
	var left int
	if err := conn.QueryRow(ctx, `
		select (select count(*) from principals where id like 'finance/%')
		     + (select count(*) from runs where namespace = 'finance')
		     + (select count(*) from grants where namespace = 'finance' or principal like 'finance/%' or granted_by like 'finance/%')
		     + (select count(*) from secret_values where namespace = 'finance')`).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d rows still name the old name: %v", left, err)
	}

	// The value still opens under what it was sealed for, and the namespace is found by the name it
	// left.
	if err := pool.In(ctx, "accounting", func(ctx context.Context, ns *NS) error {
		if _, err := ns.SealedValue(ctx, "ledger"); err != nil {
			return err
		}
		storage, err := ns.Storage(ctx)
		if err == nil && storage != "finance" {
			t.Errorf("the namespace is stored under %s", storage)
		}
		return err
	}); err != nil {
		t.Error(err)
	}
	for _, name := range []string{"finance", "accounting"} {
		if current, err := pool.CurrentName(ctx, name); err != nil || current != "accounting" {
			t.Errorf("%s answers to %q, %v", name, current, err)
		}
	}
	if current, err := pool.CurrentName(ctx, "nowhere"); err != nil || current != "nowhere" {
		t.Errorf("a name nobody holds answers to %q, %v", current, err)
	}

	// The name it left is held against every other namespace and login, and is its own to take back.
	var held *NameHeld
	err := administer(func(ctx context.Context, w *Wide) error {
		_, err := w.CreateNamespace(ctx, Namespace{Name: "finance"})
		return err
	})
	if !errors.As(err, &held) || !held.Former || held.Namespace != "accounting" || !errors.Is(err, ErrNameTaken) {
		t.Errorf("a namespace created under the former name was answered %v", err)
	}
	if err := administer(func(ctx context.Context, w *Wide) error {
		return w.CreateUser(ctx, User{Login: "finance", GivenName: "Finance"})
	}); !errors.Is(err, ErrNameTaken) {
		t.Errorf("a login taking the former name was answered %v", err)
	}
	if err := administer(func(ctx context.Context, w *Wide) error {
		_, err := w.RenameNamespace(ctx, "team-ops", "finance")
		return err
	}); !errors.As(err, &held) || !held.Former {
		t.Errorf("another namespace renamed to the former name was answered %v", err)
	}
	if err := administer(func(ctx context.Context, w *Wide) error {
		_, err := w.RenameNamespace(ctx, "accounting", "finance")
		return err
	}); err != nil {
		t.Fatalf("taking the former name back was answered %v", err)
	}
	var former []string
	if err := conn.QueryRow(ctx, `select former_names from namespaces where name = 'finance' and storage = 'finance'`).Scan(&former); err != nil || !slices.Equal(former, []string{"accounting"}) {
		t.Errorf("the namespace taken back holds the former names %v, %v", former, err)
	}
}

// A rename is refused, saying why: a personal namespace, a name a login, a namespace or another
// namespace's former name holds, and a namespace with a run going, a move waiting or a runner
// narrowed to it by name. Its own name changes nothing.
func TestARenameIsRefusedWhatItCannotCarry(t *testing.T) {
	pool, super := opened(t)
	conn := superuser(t, super)
	ctx := t.Context()
	for _, stmt := range []string{
		`insert into principals (id, kind) values ('alice', 'user')`,
		`insert into users (login, display_name) values ('alice', 'Alice')`,
		`insert into namespaces (name, kind, owner) values ('alice', 'personal', 'alice')`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	rename := func(from, to string) (bool, error) {
		var changed bool
		err := pool.Installation(ctx, NamespaceAdministration, func(ctx context.Context, w *Wide) error {
			var err error
			changed, err = w.RenameNamespace(ctx, from, to)
			return err
		})
		return changed, err
	}
	if _, err := rename("alice", "alicia"); !errors.Is(err, ErrPersonalRename) {
		t.Errorf("a personal namespace renamed was answered %v", err)
	}
	if _, err := rename("nowhere", "elsewhere"); !errors.Is(err, ErrNoNamespace) {
		t.Errorf("a namespace that does not exist renamed was answered %v", err)
	}
	var held *NameHeld
	if _, err := rename("team-ops", "alice"); !errors.As(err, &held) || !held.Login {
		t.Errorf("a rename to a personal namespace's name, its user's login, was answered %v", err)
	}
	if _, err := rename("team-ops", "finance"); !errors.As(err, &held) || held.Login || held.Former || held.Namespace != "finance" {
		t.Errorf("a rename to another namespace's name was answered %v", err)
	}
	if _, err := conn.Exec(ctx, `insert into principals (id, kind) values ('carol', 'user'); insert into users (login, display_name) values ('carol', 'Carol')`); err != nil {
		t.Fatal(err)
	}
	if _, err := rename("team-ops", "carol"); !errors.As(err, &held) || !held.Login {
		t.Errorf("a rename to a login was answered %v", err)
	}

	var waits *RenameWaits
	if _, err := rename("finance", "accounting"); !errors.As(err, &waits) || waits.Runs != 1 {
		t.Errorf("a namespace with a run queued was answered %v", err)
	}
	if _, err := conn.Exec(ctx, `update runs set state = 'succeeded', started_at = now(), finished_at = now()`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
		insert into runner_pools (name, created_by) values ('dedicated', 'alice');
		insert into runners (id, pool, cpu, memory_bytes, disk_bytes, architecture, agent_version, credential_hash, rotate_by, public_key, accepted_namespaces)
		  values ('ledger-host', 'dedicated', 1, 1, 1, 'amd64', '0.6.0', repeat('a', 64), now() + interval '1 day', convert_to(repeat('k', 32), 'UTF8'), '{finance}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := rename("finance", "accounting"); !errors.As(err, &waits) || len(waits.Runners) != 1 {
		t.Errorf("a namespace a runner narrows itself to was answered %v", err)
	}
	if _, err := conn.Exec(ctx, `update runners set state = 'revoked', revoked_at = now(), revoked_by = 'alice', results_accepted_until = now() + interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	if changed, err := rename("finance", "finance"); err != nil || changed {
		t.Errorf("a rename to its own name answered %v, %v", changed, err)
	}
	if changed, err := rename("finance", "accounting"); err != nil || !changed {
		t.Errorf("the rename answered %v, %v", changed, err)
	}
	var accepted []string
	if err := conn.QueryRow(ctx, `select accepted_namespaces::text[] from runners`).Scan(&accepted); err != nil || !slices.Equal(accepted, []string{"accounting"}) {
		t.Errorf("the revoked runner accepts %v, %v", accepted, err)
	}
}

// Every column naming a namespace is carried by a rename, or left as it is for a reason given here:
// a key that carries the rename through, onto the namespace's name or onto a column a rename carries,
// as a step's key onto its run's; a column RenameNamespace writes itself; or a record of what
// happened. A table added later with a column naming a namespace and none of those fails here,
// rather than every rename failing on its key, or leaving a row under a name the namespace no longer
// has.
func TestEveryColumnNamingANamespaceFollowsARename(t *testing.T) {
	_, super := opened(t)
	rows, err := superuser(t, super).Query(t.Context(), `
		select c.relname || '.' || a.attname,
		       coalesce(r.relname || '.' || ra.attname, ''),
		       coalesce(k.confupdtype = 'c', false)
		from pg_attribute a
		join pg_class c on c.oid = a.attrelid and c.relkind in ('r', 'p')
		join pg_namespace s on s.oid = c.relnamespace and s.nspname = current_schema()
		left join pg_constraint k on k.conrelid = c.oid and k.contype = 'f' and a.attnum = any(k.conkey)
		left join pg_class r on r.oid = k.confrelid
		left join pg_attribute ra on ra.attrelid = k.confrelid and ra.attnum = k.confkey[array_position(k.conkey, a.attnum)]
		where a.attnum > 0 and not a.attisdropped and c.relname <> 'namespaces'
		  and (a.attname = 'namespace' or a.atttypid in ('namespace_name'::regtype, 'namespace_name[]'::regtype))`)
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		column, onto string
		cascades     bool
	}
	keys, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (key, error) {
		var k key
		return k, r.Scan(&k.column, &k.onto, &k.cascades)
	})
	if err != nil {
		t.Fatal(err)
	}
	carried := map[string]bool{"namespaces.name": true}
	for changed := true; changed; {
		changed = false
		for _, k := range keys {
			if k.cascades && carried[k.onto] && !carried[k.column] {
				carried[k.column], changed = true, true
			}
		}
	}
	left := map[string]string{
		"runner_pools.accepted_namespaces": "a list of names, which RenameNamespace writes",
		"runners.accepted_namespaces":      "a list of names, which RenameNamespace writes, and a runner not revoked naming the namespace refuses the rename",
		"audit_log.namespace":              "history: an entry keeps the name it was recorded under, and is hashed with it",
	}
	for _, c := range namespaceColumnsUnkeyed {
		left[c.table+"."+c.column] = "written by RenameNamespace"
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if _, ok := left[k.column]; ok || carried[k.column] || seen[k.column] {
			continue
		}
		seen[k.column] = true
		t.Errorf("%s names a namespace, and no key carries a rename to it: give it one onto namespaces, or onto what names one, ON UPDATE CASCADE, or have RenameNamespace write it", k.column)
	}
}

// Every column holding a key onto a principal is written by a rename, or is a principal's own key,
// for a reason given here. A rename writes the principals of a namespace's service accounts anew
// and deletes those under the old name, so a table added later with a key onto principals and left
// out of principalColumns fails here, rather than every rename failing on its key, or deleting its
// rows where the key cascades, as collections did.
func TestEveryKeyOntoAPrincipalFollowsARename(t *testing.T) {
	_, super := opened(t)
	rows, err := superuser(t, super).Query(t.Context(), `
		select c.relname || '.' || a.attname
		from pg_constraint k
		join pg_class c on c.oid = k.conrelid
		join pg_namespace s on s.oid = c.relnamespace and s.nspname = current_schema()
		join pg_attribute ra on ra.attrelid = k.confrelid and ra.attname = 'id'
		join pg_attribute a on a.attrelid = k.conrelid and a.attnum = k.conkey[array_position(k.confkey, ra.attnum)]
		where k.contype = 'f' and k.confrelid = 'principals'::regclass
		order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	keyed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	own := map[string]string{
		"users.login":                "a login, which names no namespace",
		"groups.principal":           "a group, which names no namespace",
		"service_accounts.principal": "generated from the service account's namespace and name, which a key carries",
	}
	written := map[string]bool{}
	for _, c := range principalColumns {
		written[c.table+"."+c.column] = true
	}
	for _, column := range keyed {
		if _, ok := own[column]; !ok && !written[column] {
			t.Errorf("%s holds a key onto a principal, which a rename of a namespace writes anew for its service accounts and deletes under the old name: list it in principalColumns", column)
		}
	}
	// More than the principals' own keys, so that a query finding none of the others cannot pass.
	if len(keyed) < 5 {
		t.Fatalf("only %v hold a key onto a principal, and more do", keyed)
	}
}
