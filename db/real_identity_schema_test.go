package db

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// What 0032 holds the identity tables to, whoever writes them: the shapes wire.schema.json gives
// the access records, and the rules the documentation states about them.

// identityBase is what every case below starts from: two namespaces, a workflow, two users, a
// group, a service account, a passkey of alice's and a password of bob's.
var identityBase = []string{
	`insert into namespaces (name) values ('finance'), ('team-ops')`,
	`insert into workflows (namespace, name) values ('finance', 'monthly-invoicing')`,
	`insert into principals (id, kind) values ('alice', 'user'), ('bob', 'user'),
	   ('group:team-finance', 'group'), ('finance/nightly-sync', 'service_account')`,
	`insert into users (login, display_name) values ('alice', 'Alice'), ('bob', 'Bob')`,
	`insert into groups (name) values ('team-finance')`,
	`insert into group_members (group_name, login) values ('team-finance', 'alice')`,
	`insert into service_accounts (namespace, name, created_by) values ('finance', 'nightly-sync', 'alice')`,
	`insert into credentials (id, login, type, public_key, sign_count, aaguid, backup_eligible, backup_state)
	   values ('cGFzc2tleQ', 'alice', 'passkey', '\xa501', 0, '\x00000000000000000000000000000000', true, true)`,
	`insert into credentials (id, login, type, password_hash) values ('password-bob', 'bob', 'password', '$pbkdf2-sha256$i=600000$c2FsdA$aGFzaA')`,
}

const aHash = `'\x0101010101010101010101010101010101010101010101010101010101010101'`

func TestTheIdentityTablesHoldTheirRules(t *testing.T) {
	super, _ := database(t)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, stmt := range identityBase {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}

	type rule struct{ what, stmt, refusedBy string }
	cases := []rule{
		// One string names a principal, on the grammar of its kind.
		{"a login", `insert into principals (id, kind) values ('carol', 'user')`, ""},
		{"operator as a login, which names the v0.2 operator on old rows", `insert into principals (id, kind) values ('operator', 'user')`, "principals_id_check"},
		{"a login in capitals", `insert into principals (id, kind) values ('Carol', 'user')`, "principals_id_check"},
		{"a group's name written as a login", `insert into principals (id, kind) values ('group:ops', 'user')`, "principals_id_check"},
		{"a group", `insert into principals (id, kind) values ('group:team-ops', 'group')`, ""},
		{"a service account", `insert into principals (id, kind) values ('finance/deploy', 'service_account')`, ""},
		{"a service account named with two slashes", `insert into principals (id, kind) values ('finance/deploy/eu', 'service_account')`, "principals_id_check"},
		{"a login past 255 characters", `insert into principals (id, kind) values ('` + strings.Repeat("a", 256) + `', 'user')`, "principals_id_check"},

		// A principal is one kind, and each kind's table holds only its own.
		{"a user whose principal is a group", `insert into users (login, display_name) values ('group:team-finance', 'Team')`, "users_login_kind_fkey"},
		{"a group inside a group", `insert into group_members (group_name, login) values ('team-finance', 'group:team-finance')`, "group_members_login_fkey"},
		{"a service account of a namespace nobody created", `insert into principals (id, kind) values ('nowhere/deploy', 'service_account');
		   insert into service_accounts (namespace, name, created_by) values ('nowhere', 'deploy', 'alice')`, "service_accounts_namespace_fkey"},

		// A credential is one of three types and carries that type's material alone.
		{"a passkey with no public key", `insert into credentials (id, login, type, sign_count, aaguid, backup_eligible, backup_state)
		   values ('bm8ta2V5', 'bob', 'passkey', 0, '\x00000000000000000000000000000000', false, false)`, "credentials_one_type"},
		{"a password carrying a public key", `insert into credentials (id, login, type, password_hash, public_key) values ('password-alice', 'alice', 'password', 'h', '\x01')`, "credentials_one_type"},
		{"a second password", `insert into credentials (id, login, type, password_hash) values ('password-bob-2', 'bob', 'password', 'h')`, "credentials_one_password"},
		{"a TOTP of its own", `insert into credentials (id, login, type, totp_sealed) values ('totp-bob', 'bob', 'totp', '\x01')`, ""},
		{"backed up and not eligible", `insert into credentials (id, login, type, public_key, sign_count, aaguid, backup_eligible, backup_state)
		   values ('Ym9i', 'bob', 'passkey', '\x01', 0, '\x00000000000000000000000000000000', false, true)`, "credentials_backup_state"},
		{"a credential ID another user holds", `insert into credentials (id, login, type, public_key, sign_count, aaguid, backup_eligible, backup_state)
		   values ('cGFzc2tleQ', 'bob', 'passkey', '\x01', 0, '\x00000000000000000000000000000000', false, false)`, "credentials_pkey"},

		// A token belongs to a user or a service account, expires within a year, and narrows.
		{"a token of a user", `insert into api_tokens (id, hash, principal, principal_kind, created_at, expires_at, scope_within)
		   values ('01JQ3M8T', ` + aHash + `, 'alice', 'user', now(), now() + interval '90 days', '{finance/monthly-invoicing}')`, ""},
		{"a token of a service account", `insert into api_tokens (id, hash, principal, principal_kind, created_at, expires_at)
		   values ('01JQ3M8T', ` + aHash + `, 'finance/nightly-sync', 'service_account', now(), now() + interval '1 year')`, ""},
		{"a token of a group", `insert into api_tokens (id, hash, principal, principal_kind, created_at, expires_at)
		   values ('01JQ3M8T', ` + aHash + `, 'group:team-finance', 'group', now(), now() + interval '90 days')`, "api_tokens_principal_kind_check"},
		{"a token claiming a kind its principal is not", `insert into api_tokens (id, hash, principal, principal_kind, created_at, expires_at)
		   values ('01JQ3M8T', ` + aHash + `, 'alice', 'service_account', now(), now() + interval '90 days')`, "api_tokens_principal_principal_kind_fkey"},
		{"a token for more than a year", `insert into api_tokens (id, hash, principal, principal_kind, created_at, expires_at)
		   values ('01JQ3M8T', ` + aHash + `, 'alice', 'user', now(), now() + interval '1 year 1 day')`, "api_tokens_expiry"},
		{"a token narrowed to nothing", `insert into api_tokens (id, hash, principal, principal_kind, created_at, expires_at, scope_permissions)
		   values ('01JQ3M8T', ` + aHash + `, 'alice', 'user', now(), now() + interval '90 days', '{}')`, "api_tokens_scope_permissions_check"},
		{"a token narrowed to a permission nobody has", `insert into api_tokens (id, hash, principal, principal_kind, created_at, expires_at, scope_permissions)
		   values ('01JQ3M8T', ` + aHash + `, 'alice', 'user', now(), now() + interval '90 days', '{workflow:admin}')`, "permission_check"},

		// A session was opened by one credential or one code, of its own user.
		{"a session opened by a passkey", `insert into sessions (hash, login, credential, idle_expires_at) values (` + aHash + `, 'alice', 'cGFzc2tleQ', now() + interval '1 hour')`, ""},
		{"a session opened by somebody else's passkey", `insert into sessions (hash, login, credential, idle_expires_at) values (` + aHash + `, 'bob', 'cGFzc2tleQ', now() + interval '1 hour')`, "sessions_credential_login_fkey"},
		{"a session opened by nothing", `insert into sessions (hash, login, idle_expires_at) values (` + aHash + `, 'alice', now() + interval '1 hour')`, "sessions_opened_by"},

		// An enrolment code is single use, one open at a time, and good for an hour.
		{"an enrolment code for more than an hour", `insert into enrolment_codes (hash, login, kind, issued_by, issued_at, expires_at)
		   values (` + aHash + `, 'alice', 'recovery', 'bob', now(), now() + interval '61 minutes')`, "enrolment_codes_expiry"},
		{"a second open recovery code", `insert into enrolment_codes (hash, login, kind, issued_by, issued_at, expires_at)
		   values (` + aHash + `, 'alice', 'recovery', 'bob', now(), now() + interval '1 hour'),
		          ('\x0202020202020202020202020202020202020202020202020202020202020202', 'alice', 'recovery', 'bob', now(), now() + interval '1 hour')`, "enrolment_codes_one_open"},
		{"a recovery code issued again once the first was revoked", `insert into enrolment_codes (hash, login, kind, issued_by, issued_at, expires_at, revoked_at)
		   values (` + aHash + `, 'alice', 'recovery', 'bob', now(), now() + interval '1 hour', now()),
		          ('\x0202020202020202020202020202020202020202020202020202020202020202', 'alice', 'recovery', 'bob', now(), now() + interval '1 hour', null)`, ""},
		{"a code both used and revoked", `insert into enrolment_codes (hash, login, kind, issued_by, issued_at, expires_at, used_at, revoked_at)
		   values (` + aHash + `, 'alice', 'recovery', 'bob', now(), now() + interval '1 hour', now(), now())`, "enrolment_codes_one_end"},

		// A grant is a role or a denied permission, on a namespace or on one of its workflows.
		{"a role on a namespace", `insert into grants (id, namespace, principal, role, granted_by) values ('01JQ3M8T', 'finance', 'group:team-finance', 'viewer', 'operator')`, ""},
		{"a deny on a workflow", `insert into grants (id, namespace, workflow, principal, deny, granted_by) values ('01JQ3M8T', 'finance', 'monthly-invoicing', 'alice', 'run:read_data', 'bob')`, ""},
		{"a role and a deny at once", `insert into grants (id, namespace, principal, role, deny, granted_by) values ('01JQ3M8T', 'finance', 'alice', 'viewer', 'run:read_data', 'bob')`, "grants_role_or_deny"},
		{"neither a role nor a deny", `insert into grants (id, namespace, principal, granted_by) values ('01JQ3M8T', 'finance', 'alice', 'bob')`, "grants_role_or_deny"},
		{"a role nobody defined", `insert into grants (id, namespace, principal, role, granted_by) values ('01JQ3M8T', 'finance', 'alice', 'admin', 'bob')`, "grants_role_check"},
		{"a denied role rather than a permission", `insert into grants (id, namespace, principal, deny, granted_by) values ('01JQ3M8T', 'finance', 'alice', 'owner', 'bob')`, "permission_check"},
		{"a grant on a workflow that is not there", `insert into grants (id, namespace, workflow, principal, role, granted_by) values ('01JQ3M8T', 'finance', 'nightly', 'alice', 'viewer', 'bob')`, "grants_namespace_workflow_fkey"},
		{"a grant to nobody", `insert into grants (id, namespace, principal, role, granted_by) values ('01JQ3M8T', 'finance', 'carol', 'viewer', 'bob')`, "grants_principal_fkey"},

		// A namespace is personal to the user it is named after, or shared.
		{"a personal namespace named after its owner", `insert into namespaces (name, kind, owner) values ('alice', 'personal', 'alice')`, ""},
		{"a shared namespace owned by a group", `insert into namespaces (name, kind, owner) values ('ledger', 'shared', 'group:team-finance')`, ""},
		{"a personal namespace owned by a group", `insert into namespaces (name, kind, owner) values ('ledger', 'personal', 'group:team-finance')`, "namespaces_personal_owner"},
		{"a personal namespace named after somebody else", `insert into namespaces (name, kind, owner) values ('ledger', 'personal', 'alice')`, "namespaces_personal_owner"},
		{"a personal namespace owned by nobody", `insert into namespaces (name, kind) values ('ledger', 'personal')`, "namespaces_personal_owner"},
		{"a namespace owned by nobody who exists", `update namespaces set owner = 'carol' where name = 'finance'`, "namespaces_owner_fkey"},
		{"removing a principal who owns a namespace", `update namespaces set owner = 'alice' where name = 'finance'; delete from principals where id = 'alice'`, "namespaces_owner_fkey"},
		{"an empty list of pools", `update namespaces set allowed_runner_pools = '{}' where name = 'finance'`, "namespaces_allowed_runner_pools_check"},
		{"a pool nobody could name", `update namespaces set allowed_runner_pools = '{Default}' where name = 'finance'`, "runner_pool_name_check"},
		{"a run duration off the timeout grammar", `update namespaces set max_run_duration = '90 minutes' where name = 'finance'`, "namespaces_max_run_duration_check"},
		{"no runs an hour", `update namespaces set max_runs_per_hour = 0 where name = 'finance'`, "namespaces_max_runs_per_hour_check"},

		// One installation policy, whole, and namespaces that set only what they tighten.
		{"a namespace tightening one setting", `insert into auth_policy (namespace, device_bound_only) values ('finance', true)`, ""},
		{"a second installation policy", `insert into auth_policy (password, passkey, user_verification, device_bound_only, min_passkeys)
		   values ('allowed', 'required', 'required', false, 2)`, "auth_policy_installation"},
		{"an installation policy missing a setting", `update auth_policy set min_passkeys = null where namespace is null`, "auth_policy_installation_whole"},
		{"no passkey needed before the password goes", `insert into auth_policy (namespace, min_passkeys) values ('finance', 0)`, "auth_policy_min_passkeys_check"},

		// The bootstrap token ends once, for good.
		{"the bootstrap token written", `update bootstrap set token_hash = ` + aHash, ""},
		{"the bootstrap token ended", `update bootstrap set token_hash = ` + aHash + `; update bootstrap set enrolled_at = now(), token_hash = null`, ""},
		{"an ended bootstrap token keeping its hash", `update bootstrap set token_hash = ` + aHash + `; update bootstrap set enrolled_at = now()`, "bootstrap_ended_keeps_no_hash"},
		{"a bootstrap token written after it ended", `update bootstrap set enrolled_at = now(); update bootstrap set token_hash = ` + aHash, "bootstrap_ended_keeps_no_hash"},
		{"a bootstrap token started again", `update bootstrap set enrolled_at = now(); update bootstrap set enrolled_at = null`, "never starts again"},
		{"the bootstrap state removed", `delete from bootstrap`, "is kept"},
	}
	// Every word the API routes on, as a login, a group and a service account.
	for _, word := range agk.ReservedNamespaces {
		for _, id := range []struct{ id, kind string }{{word, "user"}, {"group:" + word, "group"}, {"finance/" + word, "service_account"}} {
			cases = append(cases, rule{"the reserved word " + id.id + " as a " + id.kind,
				`insert into principals (id, kind) values ('` + id.id + `', '` + id.kind + `')`, "principals_id_check"})
		}
	}

	for _, c := range cases {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, c.stmt)
		tx.Rollback(ctx)
		var pg *pgconn.PgError
		refused := errors.As(err, &pg)
		switch {
		case err != nil && !refused:
			t.Fatalf("%s: %s", c.what, err)
		case c.refusedBy == "" && refused:
			t.Errorf("%s was refused: %s (%s)", c.what, pg.Message, pg.ConstraintName)
		case c.refusedBy != "" && !refused:
			t.Errorf("%s was accepted, and %s refuses it", c.what, c.refusedBy)
		case c.refusedBy != "" && pg.ConstraintName != c.refusedBy && !strings.Contains(pg.Message, c.refusedBy):
			t.Errorf("%s was refused by %q (%s), and not by %s", c.what, pg.Message, pg.ConstraintName, c.refusedBy)
		}
	}
}

// Removing a principal removes everything it holds, and removing a workflow the grants on it, so
// that a principal or a workflow created again under the same name starts with nothing.
func TestWhatAPrincipalOrAWorkflowHoldsGoesWithIt(t *testing.T) {
	super, _ := database(t)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, stmt := range append(identityBase,
		`insert into grants (id, namespace, principal, role, granted_by) values ('01JQ3M8A', 'finance', 'alice', 'editor', 'operator')`,
		`insert into grants (id, namespace, workflow, principal, role, granted_by) values ('01JQ3M8B', 'finance', 'monthly-invoicing', 'bob', 'operator', 'alice')`,
		`insert into api_tokens (id, hash, principal, principal_kind, created_at, expires_at) values ('01JQ3M8C', `+aHash+`, 'alice', 'user', now(), now() + interval '90 days')`,
		`insert into sessions (hash, login, credential, idle_expires_at) values (`+aHash+`, 'alice', 'cGFzc2tleQ', now() + interval '1 hour')`,
		`delete from principals where id = 'alice'`,
		`delete from workflows where namespace = 'finance' and name = 'monthly-invoicing'`,
	) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	for table, where := range map[string]string{
		"users": "login = 'alice'", "credentials": "login = 'alice'", "group_members": "login = 'alice'",
		"api_tokens": "principal = 'alice'", "sessions": "login = 'alice'", "grants": "true",
	} {
		var left int
		if err := conn.QueryRow(ctx, `select count(*) from `+table+` where `+where).Scan(&left); err != nil {
			t.Fatal(err)
		}
		if left != 0 {
			t.Errorf("%d rows of %s outlived what they belonged to", left, table)
		}
	}
	// And nothing more: bob, his password and the group are still there.
	var kept int
	if err := conn.QueryRow(ctx, `select (select count(*) from users) + (select count(*) from credentials) + (select count(*) from groups)`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 3 {
		t.Errorf("%d rows of users, credentials and groups are left, and bob, his password and the group are 3", kept)
	}
}

// What one namespace grants is not another's to read, and cannot be planted in another either.
func TestOneNamespaceCannotSeeAnothersGrants(t *testing.T) {
	super, app := database(t)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, stmt := range append(identityBase,
		`insert into grants (id, namespace, principal, role, granted_by) values ('01JQ3M8A', 'finance', 'alice', 'editor', 'operator'),
		   ('01JQ3M8B', 'team-ops', 'alice', 'viewer', 'operator')`) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	pool, err := Open(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var seen []string
	err = pool.In(ctx, "finance", func(ctx context.Context, n *NS) error {
		rows, err := n.tx.Query(ctx, `select id from grants order by id`)
		if err != nil {
			return err
		}
		seen, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seen, []string{"01JQ3M8A"}) {
		t.Errorf("finance reads the grants %v, and holds only 01JQ3M8A", seen)
	}

	err = pool.In(ctx, "finance", func(ctx context.Context, n *NS) error {
		_, err := n.tx.Exec(ctx, `insert into grants (id, namespace, principal, role, granted_by) values ('01JQ3M8C', 'team-ops', 'alice', 'owner', 'alice')`)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Errorf("a grant was written into another namespace, or refused for another reason: %v", err)
	}
}

// agentiik_reserved refuses the words the API routes on, which agk.ReservedNamespaces lists: no
// fewer, so that no login takes a route, and no more, so that no login is refused for a word the
// API does not route on.
func TestTheReservedWordsAreTheAPIs(t *testing.T) {
	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	var body string
	for _, m := range all {
		if i := strings.Index(m.SQL, "create function agentiik_reserved"); i >= 0 {
			body = m.SQL[i:]
			body = body[:strings.Index(body, "$$;")]
		}
	}
	if body == "" {
		t.Fatal("no migration creates agentiik_reserved")
	}
	var listed []string
	for _, m := range regexp.MustCompile(`'([a-z-]+)'`).FindAllStringSubmatch(body, -1) {
		listed = append(listed, m[1])
	}
	want := slices.Clone(agk.ReservedNamespaces)
	slices.Sort(listed)
	slices.Sort(want)
	if !slices.Equal(listed, want) {
		t.Errorf("agentiik_reserved refuses %v, and the API routes on %v", listed, want)
	}
}
