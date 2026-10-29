package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/audit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// renameUser gives the user from the login to, answering whether their personal namespace followed.
func renameUser(t *testing.T, pool *Pool, from, to string) (bool, error) {
	t.Helper()
	var personal bool
	err := pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
		var err error
		personal, err = w.RenameUser(ctx, from, to)
		return err
	})
	return personal, err
}

// A user given a new login carries everything naming them at once: their credentials, sessions,
// enrolment links, agk login's codes, challenges and TOTP enrolments, their memberships, the grants
// they hold and those they wrote, their tokens, what they are told, the namespaces they own, their
// collections, and who did what wherever they did something, a password set by a recovery code they
// issued among it; their personal namespace is renamed with them, its service account and a token
// narrowed to it following it, its storage name the one it was created with and the old login its
// former name. Their session and their token still answer, now as the new login. The audit log, and
// what a notification told of what they did, keep the login they named.
func TestAUserGivenANewLoginCarriesEveryRowNamingThem(t *testing.T) {
	pool, super := opened(t)
	conn := superuser(t, super)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := conn.Exec(ctx, `update runs set state = 'succeeded', started_at = now(), finished_at = now()`); err != nil {
		t.Fatal(err)
	}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, u := range []User{
			{Login: "mcp", Profile: Profile{GivenName: "Marie-Claire", FamilyName: "Petit"}, Email: "mc@example.com"},
			{Login: "bob", Profile: Profile{GivenName: "Bob"}},
			{Login: "carol", Profile: Profile{GivenName: "Carol"}, Admin: true},
		} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		// Each with its built-in identity, NS/agentiik, which its creation gives it.
		for _, n := range []Namespace{{Name: "mcp", Kind: NamespacePersonal, Owner: "mcp"}, {Name: "ledger", Kind: NamespaceShared, Owner: "mcp"}} {
			if _, err := w.CreateNamespace(ctx, n); err != nil {
				return err
			}
		}
		for _, c := range []Credential{
			{ID: "mcp-passkey", Login: "mcp", Type: CredentialPasskey, PublicKey: []byte{1}, AAGUID: make([]byte, 16)},
			{ID: "mcp-password", Login: "mcp", Type: CredentialPassword, PasswordHash: "$argon2id$x"},
			{ID: "bob-password", Login: "bob", Type: CredentialPassword, PasswordHash: "$argon2id$y"},
		} {
			if err := w.AddCredential(ctx, c); err != nil {
				return err
			}
		}
		if err := w.OpenSession(ctx, Session{Hash: valueHash("a session"), Login: "mcp", Credential: "mcp-passkey", CreatedAt: now, IdleExpiresAt: now.Add(time.Hour)}); err != nil {
			return err
		}
		if err := w.MintToken(ctx, APIToken{
			ID: "01M2T0KENMCPAAAAAAAAAAAAAA", Hash: valueHash("a token"), Principal: "mcp",
			Within: []string{"mcp", "finance/monthly-invoicing"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			return err
		}
		if err := w.CreateGroup(ctx, "auditors"); err != nil {
			return err
		}
		if _, err := w.AddMember(ctx, "auditors", "mcp"); err != nil {
			return err
		}
		for _, g := range []access.Grant{
			{ID: "01M2GRANTMCPAAAAAAAAAAAAAA", Principal: "mcp", Scope: access.Scope{Namespace: "finance"}, Role: access.Viewer, GrantedBy: "carol"},
			{ID: "01M2GRANTMCPBBBBBBBBBBBBBB", Principal: "bob", Scope: access.Scope{Namespace: "finance"}, Role: access.Editor, GrantedBy: "mcp"},
		} {
			if err := w.GrantAccess(ctx, g); err != nil {
				return err
			}
		}
		if _, err := w.CreateCollection(ctx, "mcp", KindUser, "01M2CAAAAAAAAAAAAAAAAAAAAA", "daily", ""); err != nil {
			return err
		}
		// mcp is told of a code carol issued them, and bob of one mcp issued him.
		if err := w.TellRecoveryIssued(ctx, "mcp", "carol", now); err != nil {
			return err
		}
		if err := w.TellRecoveryIssued(ctx, "bob", "mcp", now); err != nil {
			return err
		}
		return w.Audit(ctx, audit.Record{Actor: "mcp", Action: audit.UserProfile, Target: "mcp", Result: audit.Done})
	})
	for _, stmt := range []string{
		`update credentials set recovered_by = 'mcp' where id = 'bob-password'`,
		`update runs set triggered_by = 'mcp' where namespace = 'finance'`,
		`update workflows set created_by = 'mcp' where namespace = 'finance'`,
		`insert into namespace_variables (namespace, name, value, value_bytes, value_count, visibility, updated_by)
		   values ('finance', 'region', '"eu"', 4, 1, 'all', 'mcp')`,
		`insert into enrolment_codes (hash, login, kind, issued_by, issued_at, expires_at)
		   values (sha256('a link'), 'mcp', 'enrolment', 'carol', now(), now() + interval '1 hour')`,
		`insert into exchange_codes (hash, login, credential, code_challenge, issued_at, expires_at)
		   values (sha256('a code'), 'mcp', 'mcp-passkey', repeat('c', 43), now(), now() + interval '1 minute')`,
		`insert into webauthn_challenges (challenge, ceremony, login, enrolment_code, issued_at, expires_at)
		   values (sha256('a challenge'), 'registration', 'mcp', sha256('a link'), now(), now() + interval '5 minutes')`,
		`insert into totp_enrolments (login, id, totp_sealed, started_at, expires_at)
		   values ('mcp', 'mcp-totp', 'sealed', now(), now() + interval '10 minutes')`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}

	if personal, err := renameUser(t, pool, "mcp", "mc-petit"); err != nil || !personal {
		t.Fatalf("the new login answered %v, %v", personal, err)
	}

	for what, query := range map[string]string{
		"their user":                      `select count(*) from users where login = 'mc-petit' and email = 'mc@example.com' and given_name = 'Marie-Claire'`,
		"their principal":                 `select count(*) from principals where id = 'mc-petit' and kind = 'user'`,
		"their credentials":               `select count(*) / 2 from credentials where login = 'mc-petit'`,
		"their session":                   `select count(*) from sessions where login = 'mc-petit' and credential = 'mcp-passkey'`,
		"their enrolment link":            `select count(*) from enrolment_codes where login = 'mc-petit'`,
		"their agk login code":            `select count(*) from exchange_codes where login = 'mc-petit' and credential = 'mcp-passkey'`,
		"their challenge":                 `select count(*) from webauthn_challenges where login = 'mc-petit'`,
		"their TOTP enrolment":            `select count(*) from totp_enrolments where login = 'mc-petit'`,
		"their membership":                `select count(*) from group_members where login = 'mc-petit' and group_name = 'auditors'`,
		"the grant they hold":             `select count(*) from grants where principal = 'mc-petit' and granted_by = 'carol'`,
		"the grant they wrote":            `select count(*) from grants where principal = 'bob' and granted_by = 'mc-petit'`,
		"their token":                     `select count(*) from api_tokens where principal = 'mc-petit' and scope_within = '{mc-petit,finance/monthly-invoicing}'::grant_scope[]`,
		"what they are told":              `select count(*) from notifications where recipient = 'mc-petit' and acted_by = 'carol'`,
		"what bob was told they did":      `select count(*) from notifications where recipient = 'bob' and acted_by = 'mcp'`,
		"their personal namespace":        `select count(*) from namespaces where name = 'mc-petit' and kind = 'personal' and owner = 'mc-petit' and storage = 'mcp' and former_names = '{mcp}'`,
		"its built-in identity":           `select count(*) from service_accounts where namespace = 'mc-petit' and principal = 'mc-petit/agentiik'`,
		"the namespace they own":          `select count(*) from namespaces where name = 'ledger' and owner = 'mc-petit'`,
		"their collection":                `select count(*) from collections where principal = 'mc-petit'`,
		"the run they started":            `select count(*) from runs where namespace = 'finance' and triggered_by = 'mc-petit'`,
		"the workflow they created":       `select count(*) from workflows where namespace = 'finance' and created_by = 'mc-petit'`,
		"the variable they wrote":         `select count(*) from namespace_variables where updated_by = 'mc-petit'`,
		"the password their code set":     `select count(*) from credentials where id = 'bob-password' and recovered_by = 'mc-petit'`,
		"the audit log, as it was":        `select count(*) from audit_log where actor = 'mcp' and target = 'mcp'`,
		"nothing left under the old name": `select 1 - count(*) from principals where id = 'mcp' or id like 'mcp/%'`,
	} {
		var n int
		if err := conn.QueryRow(ctx, query).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s: %d rows answer the new login, %v", what, n, err)
		}
	}
	var left int
	if err := conn.QueryRow(ctx, `
		select (select count(*) from users where login = 'mcp')
		     + (select count(*) from credentials where login = 'mcp' or recovered_by = 'mcp')
		     + (select count(*) from sessions where login = 'mcp')
		     + (select count(*) from grants where principal = 'mcp' or granted_by = 'mcp')
		     + (select count(*) from api_tokens where principal = 'mcp')
		     + (select count(*) from notifications where recipient = 'mcp')
		     + (select count(*) from namespaces where name = 'mcp' or owner = 'mcp')`).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d rows still name the old login: %v", left, err)
	}

	// The session and the token answer as the new login, and the old login is the namespace's,
	// which nobody else takes.
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if s, err := w.SessionByHash(ctx, valueHash("a session"), now.Add(time.Minute)); err != nil || s.Login != "mc-petit" {
			t.Errorf("the session reads %+v, %v", s, err)
		}
		if tok, err := w.TokenByHash(ctx, valueHash("a token"), now.Add(time.Minute)); err != nil || tok.Principal != "mc-petit" {
			t.Errorf("the token reads %+v, %v", tok, err)
		}
		if current, err := w.CurrentName(ctx, "mcp"); err != nil || current != "mc-petit" {
			t.Errorf("mcp answers to %q, %v", current, err)
		}
		return nil
	})
	if err := pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
		return w.CreateUser(ctx, User{Login: "mcp", GivenName: "Other"})
	}); !errors.Is(err, ErrNameTaken) {
		t.Errorf("a user created under the old login was answered %v", err)
	}
}

// A new login is refused, saying why, and changes nothing: no user by the old login; a login, a
// namespace's name or a namespace's former name holding the new one, the user's own personal
// namespace's among them; and, where they have a personal namespace, what its rename waits for. A
// user with no personal namespace, who has not signed in yet, is renamed with none. And the table
// holds the rest of the rule itself: a personal namespace is renamed with its user's login alone,
// and a login is changed to no namespace's name.
func TestANewLoginIsRefusedWhatItCannotCarry(t *testing.T) {
	pool, super := opened(t)
	conn := superuser(t, super)
	ctx := t.Context()
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, u := range []User{{Login: "mcp", GivenName: "Marie"}, {Login: "hooks", GivenName: "Hugo"}, {Login: "bob", GivenName: "Bob"}} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		_, err := w.CreateNamespace(ctx, Namespace{Name: "mcp", Kind: NamespacePersonal, Owner: "mcp"})
		return err
	})
	if _, err := conn.Exec(ctx, `update runs set state = 'succeeded', started_at = now(), finished_at = now()`); err != nil {
		t.Fatal(err)
	}
	if err := pool.Installation(ctx, NamespaceAdministration, func(ctx context.Context, w *Wide) error {
		_, err := w.RenameNamespace(ctx, "team-ops", "operations")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := renameUser(t, pool, "nobody", "somebody"); !errors.Is(err, ErrNoPrincipal) {
		t.Errorf("a user who does not exist given a login was answered %v", err)
	}
	var held *NameHeld
	if _, err := renameUser(t, pool, "mcp", "bob"); !errors.As(err, &held) || !held.Login {
		t.Errorf("a new login another user holds was answered %v", err)
	}
	if _, err := renameUser(t, pool, "mcp", "finance"); !errors.As(err, &held) || held.Login || held.Former || held.Namespace != "finance" {
		t.Errorf("a new login a namespace is named was answered %v", err)
	}
	if _, err := renameUser(t, pool, "mcp", "team-ops"); !errors.As(err, &held) || !held.Former || held.Namespace != "operations" {
		t.Errorf("a new login a namespace held before it was renamed was answered %v", err)
	}
	var waits *RenameWaits
	for _, stmt := range []string{
		`insert into workflows (namespace, name) values ('mcp', 'daily')`,
		`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at) values ('mcp', 'daily', 'c0ffee1', '{}', 'mcp', now())`,
		`insert into runs (namespace, id, workflow, commit, trigger) values ('mcp', '01M2RNMCPAAAAAAAAAAAAAAAAA', 'daily', 'c0ffee1', 'manual')`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	if _, err := renameUser(t, pool, "mcp", "mc-petit"); !errors.As(err, &waits) || waits.Runs != 1 || waits.Namespace != "mcp" {
		t.Errorf("a user whose personal namespace has a run queued was answered %v", err)
	}
	var users int
	if err := conn.QueryRow(ctx, `select count(*) from users where login in ('mcp', 'hooks') and exists (select from namespaces where name = 'mcp' and owner = 'mcp')`).Scan(&users); err != nil || users != 2 {
		t.Errorf("a refusal changed the users: %d left, %v", users, err)
	}

	// hooks has never signed in, and has no personal namespace to take along.
	if personal, err := renameUser(t, pool, "hooks", "hugo"); err != nil || personal {
		t.Errorf("a user with no personal namespace given a login answered %v, %v", personal, err)
	}

	// The user's own personal namespace's former names are held against them too.
	if _, err := conn.Exec(ctx, `update runs set state = 'succeeded', started_at = now(), finished_at = now()`); err != nil {
		t.Fatal(err)
	}
	if _, err := renameUser(t, pool, "mcp", "mc-petit"); err != nil {
		t.Fatal(err)
	}
	if _, err := renameUser(t, pool, "mc-petit", "mcp"); !errors.As(err, &held) || !held.Former || held.Namespace != "mc-petit" {
		t.Errorf("a login taking back their own namespace's former name was answered %v", err)
	}

	for what, refused := range map[string]struct{ stmt, by string }{
		"a personal namespace renamed on its own": {`update namespaces set name = 'elsewhere' where name = 'mc-petit'`, "personal_namespaces_keep_their_name"},
		"a personal namespace given another user": {`update namespaces set name = 'bob', owner = 'bob' where name = 'mc-petit'`, "personal_namespaces_keep_their_name"},
		"a login changed to a namespace's name":   {`update users set login = 'finance' where login = 'bob'`, "logins_and_namespaces"},
		"a login changed to a former name":        {`update users set login = 'team-ops' where login = 'bob'`, "logins_and_namespaces"},
	} {
		_, err := conn.Exec(ctx, refused.stmt)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.ConstraintName != refused.by {
			t.Errorf("%s was answered %v, and %s refuses it", what, err, refused.by)
		}
	}
}

// Every key naming a user by their login, or a credential or an enrolment link together with its
// user's login, carries a new login through, so that a table added later with one that does not fails
// here rather than every new login failing on it.
func TestEveryKeyNamingAUserFollowsANewLogin(t *testing.T) {
	_, super := opened(t)
	rows, err := superuser(t, super).Query(t.Context(), `
		select k.conrelid::regclass::text || '.' || k.conname, k.confupdtype = 'c'
		from pg_constraint k
		join pg_namespace s on s.oid = k.connamespace and s.nspname = current_schema()
		where k.contype = 'f' and k.confrelid in ('users'::regclass, 'credentials'::regclass, 'enrolment_codes'::regclass)
		order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		name     string
		cascades bool
	}
	keys, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (key, error) {
		var k key
		return k, r.Scan(&k.name, &k.cascades)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if !k.cascades {
			t.Errorf("%s names a user by their login and does not carry a new login through: make it ON UPDATE CASCADE", k.name)
		}
	}
	// The seven tables keyed onto users and the three composite keys, so that a query finding none
	// cannot pass.
	if len(keys) < 10 {
		t.Fatalf("only %v name a user by their login, and more do", keys)
	}
}
