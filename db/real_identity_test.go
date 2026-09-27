package db

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The typed paths over the identity tables, as the application's role reaches them.

// identity answers a pool on a database holding two namespaces and a workflow, as the
// application connects to it.
func identity(t *testing.T) *Pool {
	t.Helper()
	super, app := database(t)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	for _, stmt := range identityBase[:2] {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// wide runs fn through the installation's door for Identity, failing the test on an error.
func wide(t *testing.T, pool *Pool, fn func(context.Context, *Wide) error) {
	t.Helper()
	if err := pool.Installation(t.Context(), Identity, fn); err != nil {
		t.Fatal(err)
	}
}

// valueHash is what the API keeps of a token, a session or a code.
func valueHash(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

func TestAUserAGroupAndAServiceAccountAreEachOnePrincipal(t *testing.T) {
	pool := identity(t)
	ctx := t.Context()
	var groups []string
	var team Group
	var users []User
	var kinds []string
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, u := range []User{{Login: "bob", DisplayName: "Bob"}, {Login: "alice", DisplayName: "Alice", Admin: true}} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		if err := w.CreateGroup(ctx, "team-finance"); err != nil {
			return err
		}
		if added, err := w.AddMember(ctx, "team-finance", "alice"); err != nil || !added {
			t.Errorf("alice was put in the group as %v, %v", added, err)
		}
		if added, err := w.AddMember(ctx, "team-finance", "alice"); err != nil || added {
			t.Errorf("alice was put in the group a second time as %v, %v", added, err)
		}
		if err := w.CreateServiceAccount(ctx, ServiceAccount{Namespace: "finance", Name: "nightly-sync", CreatedBy: "alice"}); err != nil {
			return err
		}
		var err error
		if groups, err = w.GroupsOf(ctx, "alice"); err != nil {
			return err
		}
		if team, err = w.Group(ctx, "team-finance"); err != nil {
			return err
		}
		if users, err = w.Users(ctx); err != nil {
			return err
		}
		for _, p := range []string{"alice", "group:team-finance", "finance/nightly-sync"} {
			kind, err := w.PrincipalKind(ctx, p)
			if err != nil {
				return err
			}
			kinds = append(kinds, kind)
		}
		return nil
	})
	if !slices.Equal(kinds, []string{KindUser, KindGroup, KindServiceAccount}) {
		t.Errorf("the three principals are of the kinds %v", kinds)
	}
	if !slices.Equal(groups, []string{"team-finance"}) {
		t.Errorf("alice's groups are %v", groups)
	}
	if !slices.Equal(team.Members, []string{"alice"}) {
		t.Errorf("the group holds %v", team.Members)
	}
	if len(users) != 2 || users[0].Login != "alice" || !users[0].Admin || users[1].Admin {
		t.Errorf("the users are listed as %+v", users)
	}

	// One name, one principal, whatever kind the second would have been, and nothing of a
	// namespace nobody created.
	err := pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
		return w.CreateUser(ctx, User{Login: "alice", DisplayName: "Another Alice"})
	})
	if !errors.Is(err, ErrPrincipalExists) {
		t.Errorf("a second alice was answered %v", err)
	}
	err = pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
		return w.CreateServiceAccount(ctx, ServiceAccount{Namespace: "nowhere", Name: "deploy", CreatedBy: "alice"})
	})
	if !errors.Is(err, ErrNoNamespace) {
		t.Errorf("a service account of a namespace nobody created was answered %v", err)
	}
	err = pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
		_, err := w.AddMember(ctx, "team-ops", "alice")
		return err
	})
	if !errors.Is(err, ErrNoPrincipal) {
		t.Errorf("a member of a group nobody created was answered %v", err)
	}

	// What may change of a user changes, and the login stays.
	now := time.Now().UTC().Truncate(time.Microsecond)
	var bob User
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.UpdateUser(ctx, User{Login: "bob", DisplayName: "Robert", Suspended: true}); err != nil {
			return err
		}
		if err := w.SignedIn(ctx, "bob", now); err != nil {
			return err
		}
		var err error
		bob, err = w.User(ctx, "bob")
		return err
	})
	if bob.DisplayName != "Robert" || !bob.Suspended || bob.Admin || !bob.LastSignInAt.Equal(now) {
		t.Errorf("bob reads as %+v", bob)
	}

	// A principal goes with what it holds, unless a namespace is left owned by nobody.
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		_, err := w.tx.Exec(ctx, `update namespaces set owner = 'bob' where name = 'finance'`)
		return err
	})
	err = pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
		return w.RemovePrincipal(ctx, "bob")
	})
	if !errors.Is(err, ErrOwnsNamespace) {
		t.Errorf("removing the owner of a namespace was answered %v", err)
	}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.RemovePrincipal(ctx, "alice"); err != nil {
			return err
		}
		var err error
		team, err = w.Group(ctx, "team-finance")
		return err
	})
	if len(team.Members) != 0 {
		t.Errorf("the group still holds %v once alice is gone", team.Members)
	}
	err = pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
		return w.RemovePrincipal(ctx, "operator")
	})
	if !errors.Is(err, ErrNoPrincipal) {
		t.Errorf("operator, which names no principal, was removed as %v", err)
	}
}

// The groups are listed by name, each with its members by login and an empty one with none, and
// the namespaces a principal owns are the ones whose record names it.
func TestGroupsAreListedWithTheirMembersAndWhatAPrincipalOwnsIsFound(t *testing.T) {
	pool := identity(t)
	var groups []Group
	var owned, none []Namespace
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, u := range []string{"bob", "alice"} {
			if err := w.CreateUser(ctx, User{Login: u, DisplayName: u}); err != nil {
				return err
			}
		}
		for _, g := range []string{"team-finance", "finance-leads"} {
			if err := w.CreateGroup(ctx, g); err != nil {
				return err
			}
		}
		for _, u := range []string{"bob", "alice"} {
			if _, err := w.AddMember(ctx, "team-finance", u); err != nil {
				return err
			}
		}
		if _, err := w.tx.Exec(ctx, `update namespaces set owner = 'group:team-finance' where name in ('team-ops', 'finance')`); err != nil {
			return err
		}
		var err error
		if groups, err = w.Groups(ctx); err != nil {
			return err
		}
		if owned, err = w.NamespacesOwnedBy(ctx, "group:team-finance"); err != nil {
			return err
		}
		none, err = w.NamespacesOwnedBy(ctx, "alice")
		return err
	})
	if len(groups) != 2 || groups[0].Name != "finance-leads" || len(groups[0].Members) != 0 || groups[0].Members == nil ||
		groups[1].Name != "team-finance" || !slices.Equal(groups[1].Members, []string{"alice", "bob"}) || groups[1].CreatedAt.IsZero() {
		t.Errorf("the groups are listed as %+v", groups)
	}
	if len(owned) != 2 || owned[0].Name != "finance" || owned[1].Name != "team-ops" || owned[0].Owner != "group:team-finance" {
		t.Errorf("the group owns %+v", owned)
	}
	if len(none) != 0 {
		t.Errorf("alice, who owns nothing, owns %+v", none)
	}
}

// A first administrator's link and a new user's link are for an account that holds no credential,
// and a recovery code for one that lost theirs; the administrators are read with whether each can
// sign in, not suspended and holding a credential.
func TestALinkEnrolsTheFirstCredentialAndTheAdministratorsSayWhoCanSignIn(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	passkey := Credential{ID: "cGFzc2tleQ", Login: "alice", Type: CredentialPasskey, PublicKey: []byte{1}, AAGUID: make([]byte, 16)}
	var admins []Administrator
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, u := range []User{
			{Login: "alice", DisplayName: "Alice", Admin: true}, {Login: "bob", DisplayName: "Bob", Admin: true},
			{Login: "carol", DisplayName: "Carol"}, {Login: "dave", DisplayName: "Dave", Admin: true, Suspended: true},
		} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		if err := w.AddCredential(ctx, passkey); err != nil {
			return err
		}
		dave := passkey
		dave.ID, dave.Login = "ZGF2ZQ", "dave"
		if err := w.AddCredential(ctx, dave); err != nil {
			return err
		}
		var err error
		admins, err = w.Administrators(ctx)
		return err
	})
	want := []Administrator{{Login: "alice", SignsIn: true}, {Login: "bob"}, {Login: "dave"}}
	if !slices.Equal(admins, want) {
		t.Errorf("the administrators read as %+v, want %+v", admins, want)
	}

	for i, kind := range []string{EnrolmentFirstAdministrator, EnrolmentNewUser} {
		err := pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
			_, err := w.IssueEnrolmentCode(ctx, EnrolmentCode{Hash: valueHash("link" + kind), Login: "alice", Kind: kind,
				IssuedBy: "bob", IssuedAt: now.Add(time.Duration(i) * time.Second), ExpiresAt: now.Add(time.Hour)})
			return err
		})
		if !errors.Is(err, ErrEnrolled) {
			t.Errorf("a %s link for alice, who holds a passkey, was answered %v", kind, err)
		}
	}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if _, err := w.IssueEnrolmentCode(ctx, EnrolmentCode{Hash: valueHash("recovery"), Login: "alice", Kind: EnrolmentRecovery,
			IssuedBy: "bob", IssuedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Errorf("a recovery code for alice was answered %v", err)
		}
		if _, err := w.IssueEnrolmentCode(ctx, EnrolmentCode{Hash: valueHash("carol"), Login: "carol", Kind: EnrolmentNewUser,
			IssuedBy: "bob", IssuedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Errorf("a link for carol, who holds nothing, was answered %v", err)
		}
		return nil
	})
}

func TestACredentialIsARowOfItsOwn(t *testing.T) {
	pool := identity(t)
	ctx := t.Context()
	passkey := Credential{
		ID: "cGFzc2tleQ", Login: "alice", Type: CredentialPasskey, Label: "laptop",
		PublicKey: []byte{0xa5, 0x01, 0x02}, SignCount: 4294967294, AAGUID: make([]byte, 16),
		BackupEligible: true, BackupState: true,
	}
	password := Credential{ID: "password-alice", Login: "alice", Type: CredentialPassword, PasswordHash: "$pbkdf2-sha256$i=600000$c2FsdA$aGFzaA"}
	var read Credential
	var all []Credential
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, u := range []string{"alice", "bob"} {
			if err := w.CreateUser(ctx, User{Login: u, DisplayName: u}); err != nil {
				return err
			}
		}
		for _, c := range []Credential{passkey, password} {
			if err := w.AddCredential(ctx, c); err != nil {
				return err
			}
		}
		var err error
		if read, err = w.Credential(ctx, passkey.ID); err != nil {
			return err
		}
		all, err = w.CredentialsOf(ctx, "alice")
		return err
	})
	if !bytes.Equal(read.PublicKey, passkey.PublicKey) || read.SignCount != passkey.SignCount ||
		!bytes.Equal(read.AAGUID, passkey.AAGUID) || !read.BackupEligible || !read.BackupState || read.Label != "laptop" {
		t.Errorf("the passkey reads as %+v", read)
	}
	if len(all) != 2 || all[1].PasswordHash != password.PasswordHash || all[1].PublicKey != nil {
		t.Errorf("alice's credentials read as %+v", all)
	}

	for what, c := range map[string]Credential{
		"a second password": {ID: "password-alice-2", Login: "alice", Type: CredentialPassword, PasswordHash: "h"},
		"a passkey ID alice enrolled, for bob": {ID: passkey.ID, Login: "bob", Type: CredentialPasskey,
			PublicKey: []byte{1}, AAGUID: make([]byte, 16)},
	} {
		err := pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error { return w.AddCredential(ctx, c) })
		if !errors.Is(err, ErrCredentialExists) {
			t.Errorf("%s was answered %v", what, err)
		}
	}

	// An assertion moves the counter and the flag, and a session opened by the password goes with
	// it when the password is deleted, which is deleting its row.
	now := time.Now().UTC().Truncate(time.Microsecond)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.PasskeyUsed(ctx, passkey.ID, 4294967295, false, now); err != nil {
			return err
		}
		var err error
		if read, err = w.Credential(ctx, passkey.ID); err != nil {
			return err
		}
		if err := w.OpenSession(ctx, Session{Hash: valueHash("session"), Login: "alice", Credential: password.ID,
			CreatedAt: now, IdleExpiresAt: now.Add(time.Hour)}); err != nil {
			return err
		}
		if err := w.RemoveCredential(ctx, "bob", password.ID); !errors.Is(err, ErrNoCredential) {
			t.Errorf("bob removed alice's password, answered %v", err)
		}
		if err := w.RemoveCredential(ctx, "alice", password.ID); err != nil {
			return err
		}
		if all, err = w.CredentialsOf(ctx, "alice"); err != nil {
			return err
		}
		if _, err := w.SessionByHash(ctx, valueHash("session"), now); !errors.Is(err, ErrNoSession) {
			t.Errorf("the session the password opened outlived it, answered %v", err)
		}
		return nil
	})
	if read.SignCount != 4294967295 || read.BackupState || !read.LastUsedAt.Equal(now) {
		t.Errorf("the passkey reads as %+v after an assertion", read)
	}
	if len(all) != 1 || all[0].ID != passkey.ID {
		t.Errorf("alice's credentials read as %+v once her password is deleted", all)
	}
}

func TestATokenOpensNothingOnceRevokedOrExpired(t *testing.T) {
	pool := identity(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	token := APIToken{ID: "01JQ3M8T", Hash: valueHash("agk_alice"), Principal: "alice",
		Permissions: []string{"run:read"}, Within: []string{"finance/monthly-invoicing"}, DeviceLabel: "phone",
		CreatedAt: now, ExpiresAt: now.Add(90 * 24 * time.Hour)}
	var found APIToken
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.CreateUser(ctx, User{Login: "alice", DisplayName: "Alice"}); err != nil {
			return err
		}
		if err := w.CreateUser(ctx, User{Login: "bob", DisplayName: "Bob"}); err != nil {
			return err
		}
		if err := w.CreateGroup(ctx, "team-finance"); err != nil {
			return err
		}
		if err := w.MintToken(ctx, token); err != nil {
			return err
		}
		var err error
		found, err = w.TokenByHash(ctx, token.Hash, now)
		return err
	})
	if found.ID != token.ID || found.Principal != "alice" || !slices.Equal(found.Permissions, token.Permissions) ||
		!slices.Equal(found.Within, token.Within) || found.DeviceLabel != "phone" || !found.ExpiresAt.Equal(token.ExpiresAt) {
		t.Errorf("the token reads as %+v", found)
	}

	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if _, err := w.TokenByHash(ctx, token.Hash, token.ExpiresAt); !errors.Is(err, ErrNoToken) {
			t.Errorf("a token at its expiry was answered %v", err)
		}
		if _, err := w.RevokeToken(ctx, "bob", token.ID, now); !errors.Is(err, ErrNoToken) {
			t.Errorf("bob revoked alice's token, answered %v", err)
		}
		if revoked, err := w.RevokeToken(ctx, "alice", token.ID, now); err != nil || !revoked {
			t.Errorf("alice revoking her token was answered %v, %v", revoked, err)
		}
		if revoked, err := w.RevokeToken(ctx, "alice", token.ID, now.Add(time.Minute)); err != nil || revoked {
			t.Errorf("alice revoking her token again was answered %v, %v", revoked, err)
		}
		if _, err := w.TokenByHash(ctx, token.Hash, now); !errors.Is(err, ErrNoToken) {
			t.Errorf("a revoked token was answered %v", err)
		}
		listed, err := w.TokensOf(ctx, "alice", nil, now)
		if err != nil {
			return err
		}
		if len(listed) != 0 {
			t.Errorf("alice's revoked token is listed as %+v", listed)
		}
		return nil
	})

	for principal, want := range map[string]string{"group:team-finance": "holds no token", "carol": "no principal"} {
		err := pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
			return w.MintToken(ctx, APIToken{ID: "01JQ3M8V", Hash: valueHash(principal), Principal: principal,
				CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
		})
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte(want)) {
			t.Errorf("a token of %s was answered %v", principal, err)
		}
	}
}

func TestAnEnrolmentCodeIsSpentOnceAndAnIdleSessionIsNotOpenedAgain(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	first := EnrolmentCode{Hash: valueHash("first"), Login: "alice", Kind: EnrolmentFirstAdministrator,
		IssuedBy: "operator", IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
	again := first
	again.Hash, again.IssuedAt, again.ExpiresAt = valueHash("again"), now.Add(time.Minute), now.Add(time.Hour+time.Minute)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.CreateUser(ctx, User{Login: "alice", DisplayName: "Alice", Admin: true}); err != nil {
			return err
		}
		if replaced, err := w.IssueEnrolmentCode(ctx, first); err != nil || replaced {
			t.Errorf("the first link was issued as %v, %v", replaced, err)
		}
		// Issued again, the link before it opens nothing.
		if replaced, err := w.IssueEnrolmentCode(ctx, again); err != nil || !replaced {
			t.Errorf("the second link was issued as %v, %v", replaced, err)
		}
		if _, err := w.EnrolmentCodeByHash(ctx, first.Hash, now); !errors.Is(err, ErrNoEnrolmentCode) {
			t.Errorf("the link replaced was answered %v", err)
		}
		open, err := w.EnrolmentCodeByHash(ctx, again.Hash, now)
		if err != nil || open.Login != "alice" || open.Kind != EnrolmentFirstAdministrator || open.IssuedBy != "operator" {
			t.Errorf("the open link reads as %+v, %v", open, err)
		}
		if _, err := w.EnrolmentCodeByHash(ctx, again.Hash, again.ExpiresAt); !errors.Is(err, ErrNoEnrolmentCode) {
			t.Errorf("a link at its expiry was answered %v", err)
		}
		if used, err := w.UseEnrolmentCode(ctx, again.Hash, now.Add(2*time.Minute)); err != nil || !used.UsedAt.Equal(now.Add(2*time.Minute)) {
			t.Errorf("the link was spent as %+v, %v", used, err)
		}
		if _, err := w.UseEnrolmentCode(ctx, again.Hash, now.Add(3*time.Minute)); !errors.Is(err, ErrNoEnrolmentCode) {
			t.Errorf("the link was spent twice, answered %v", err)
		}
		// The passkey the link enrolled opens a session.
		if err := w.AddCredential(ctx, Credential{ID: "cGFzc2tleQ", Login: "alice", Type: CredentialPasskey, PublicKey: []byte{1}, AAGUID: make([]byte, 16)}); err != nil {
			return err
		}
		return w.OpenSession(ctx, Session{Hash: valueHash("signed-in"), Login: "alice", Credential: "cGFzc2tleQ",
			CreatedAt: now, IdleExpiresAt: now.Add(10 * time.Minute)})
	})

	var session Session
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		var err error
		if session, err = w.SessionByHash(ctx, valueHash("signed-in"), now); err != nil {
			return err
		}
		// Kept open while it is live, and never opened again once it has gone idle.
		if err := w.TouchSession(ctx, session.Hash, now, now.Add(20*time.Minute)); err != nil {
			return err
		}
		if err := w.TouchSession(ctx, session.Hash, now.Add(21*time.Minute), now.Add(40*time.Minute)); !errors.Is(err, ErrNoSession) {
			t.Errorf("a session idle past its expiry was kept open, answered %v", err)
		}
		if n, err := w.RevokeSessions(ctx, "alice", nil, now); err != nil || n != 1 {
			t.Errorf("alice's sessions were revoked as %d, %v", n, err)
		}
		if _, err := w.SessionByHash(ctx, session.Hash, now); !errors.Is(err, ErrNoSession) {
			t.Errorf("a revoked session was answered %v", err)
		}
		return nil
	})
	if session.Credential != "cGFzc2tleQ" || session.CredentialType != CredentialPasskey {
		t.Errorf("the session reads as opened by %q, a %q, and the passkey opened it", session.Credential, session.CredentialType)
	}
}

// A suspended account opens no session, and neither a session nor a token of theirs opens
// anything while the suspension lasts.
func TestASuspendedUserOpensNothing(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	passkey := Credential{ID: "cGFzc2tleQ", Login: "alice", Type: CredentialPasskey, PublicKey: []byte{1}, AAGUID: make([]byte, 16)}
	token := APIToken{ID: "01JQ3M8T", Hash: valueHash("agk_alice"), Principal: "alice", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	robot := APIToken{ID: "01JQ3M8V", Hash: valueHash("agk_robot"), Principal: "finance/nightly-sync", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	session := Session{Hash: valueHash("session"), Login: "alice", Credential: passkey.ID, CreatedAt: now, IdleExpiresAt: now.Add(time.Hour)}
	suspend := func(ctx context.Context, w *Wide, suspended bool) error {
		return w.UpdateUser(ctx, User{Login: "alice", DisplayName: "Alice", Suspended: suspended})
	}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.CreateUser(ctx, User{Login: "alice", DisplayName: "Alice"}); err != nil {
			return err
		}
		if err := w.CreateServiceAccount(ctx, ServiceAccount{Namespace: "finance", Name: "nightly-sync", CreatedBy: "alice"}); err != nil {
			return err
		}
		if err := w.AddCredential(ctx, passkey); err != nil {
			return err
		}
		for _, tk := range []APIToken{token, robot} {
			if err := w.MintToken(ctx, tk); err != nil {
				return err
			}
		}
		if err := w.OpenSession(ctx, session); err != nil {
			return err
		}
		if err := suspend(ctx, w, true); err != nil {
			return err
		}
		if _, err := w.TokenByHash(ctx, token.Hash, now); !errors.Is(err, ErrNoToken) {
			t.Errorf("a suspended user's token was answered %v", err)
		}
		if _, err := w.SessionByHash(ctx, session.Hash, now); !errors.Is(err, ErrNoSession) {
			t.Errorf("a suspended user's session was answered %v", err)
		}
		opened := session
		opened.Hash = valueHash("another")
		if err := w.OpenSession(ctx, opened); !errors.Is(err, ErrSessionRefused) {
			t.Errorf("a suspended user opened a session, answered %v", err)
		}
		// A service account is nobody's to suspend, and lifting the suspension gives back what
		// it held.
		if _, err := w.TokenByHash(ctx, robot.Hash, now); err != nil {
			t.Errorf("a service account's token was answered %v while a user was suspended", err)
		}
		if err := suspend(ctx, w, false); err != nil {
			return err
		}
		if _, err := w.TokenByHash(ctx, token.Hash, now); err != nil {
			t.Errorf("a token was answered %v once the suspension was lifted", err)
		}
		return nil
	})
}

// A suspended account's enrolment link and recovery code still open and are spent, since
// "enrolling is how an account suspended for having no passkey comes back", through the
// registration they start; a credential of the same account opens no session all the while, and a
// session it opened before the suspension is answered no more.
func TestASuspendedUserStillEnrolsThroughALink(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	link := EnrolmentCode{Hash: valueHash("link"), Login: "alice", Kind: EnrolmentNewUser, IssuedBy: "bob",
		IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
	recovery := EnrolmentCode{Hash: valueHash("recovery"), Login: "alice", Kind: EnrolmentRecovery, IssuedBy: "bob",
		IssuedAt: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour + time.Minute)}
	passkey := Credential{ID: "cGFzc2tleQ", Login: "alice", Type: CredentialPasskey, PublicKey: []byte{1}, AAGUID: make([]byte, 16)}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.CreateUser(ctx, User{Login: "alice", DisplayName: "Alice"}); err != nil {
			return err
		}
		if _, err := w.IssueEnrolmentCode(ctx, link); err != nil {
			return err
		}
		return w.UpdateUser(ctx, User{Login: "alice", DisplayName: "Alice", Suspended: true})
	})
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if _, err := w.EnrolmentCodeByHash(ctx, link.Hash, now.Add(time.Second)); err != nil {
			t.Errorf("a link issued before the suspension was answered %v once suspended", err)
		}
		if _, err := w.UseEnrolmentCode(ctx, link.Hash, now.Add(time.Second)); err != nil {
			t.Errorf("a suspended user's link was not spent: %v", err)
		}
		if err := w.AddCredential(ctx, passkey); err != nil {
			return err
		}

		// A recovery code issued during the suspension opens and is spent as well.
		if _, err := w.IssueEnrolmentCode(ctx, recovery); err != nil {
			return err
		}
		if _, err := w.UseEnrolmentCode(ctx, recovery.Hash, now.Add(2*time.Minute)); err != nil {
			t.Errorf("a suspended user's recovery code was not spent: %v", err)
		}

		// And a credential of the same account opens no session.
		err := w.OpenSession(ctx, Session{Hash: valueHash("by-passkey"), Login: "alice", Credential: passkey.ID,
			CreatedAt: now.Add(2 * time.Minute), IdleExpiresAt: now.Add(32 * time.Minute)})
		if !errors.Is(err, ErrSessionRefused) {
			t.Errorf("a suspended user's passkey opened a session, answered %v", err)
		}
		return nil
	})

	// A session the passkey opened before a suspension is answered no more once it comes.
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.UpdateUser(ctx, User{Login: "alice", DisplayName: "Alice"}); err != nil {
			return err
		}
		opened := now.Add(3 * time.Minute)
		if err := w.OpenSession(ctx, Session{Hash: valueHash("opened-before"), Login: "alice", Credential: passkey.ID,
			CreatedAt: opened, IdleExpiresAt: opened.Add(30 * time.Minute)}); err != nil {
			return err
		}
		if err := w.UpdateUser(ctx, User{Login: "alice", DisplayName: "Alice", Suspended: true}); err != nil {
			return err
		}
		if _, err := w.SessionByHash(ctx, valueHash("opened-before"), opened.Add(time.Second)); !errors.Is(err, ErrNoSession) {
			t.Errorf("a session opened before the suspension was answered %v once suspended", err)
		}
		return nil
	})
}

// A first administrator's link replaces the one before it whoever that was for, and none is open
// or issued once the first administrator has enrolled.
func TestAFirstAdministratorsLinkEndsWithTheBootstrapToken(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	link := func(login, value string) EnrolmentCode {
		return EnrolmentCode{Hash: valueHash(value), Login: login, Kind: EnrolmentFirstAdministrator, IssuedBy: "operator",
			IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
	}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, u := range []string{"alcie", "alice", "bob"} {
			if err := w.CreateUser(ctx, User{Login: u, DisplayName: u, Admin: true}); err != nil {
				return err
			}
		}
		if _, err := w.IssueEnrolmentCode(ctx, link("alcie", "mistyped")); err != nil {
			return err
		}
		if replaced, err := w.IssueEnrolmentCode(ctx, link("alice", "meant")); err != nil || !replaced {
			t.Errorf("a link for alice replaced the one for alcie as %v, %v", replaced, err)
		}
		if _, err := w.EnrolmentCodeByHash(ctx, valueHash("mistyped"), now); !errors.Is(err, ErrNoEnrolmentCode) {
			t.Errorf("the mistyped link was answered %v once another was issued", err)
		}
		// bob's link, issued last, is still open when the bootstrap token ends.
		if _, err := w.IssueEnrolmentCode(ctx, link("bob", "bobs")); err != nil {
			return err
		}
		if _, err := w.EndBootstrap(ctx, now); err != nil {
			return err
		}
		if _, err := w.UseEnrolmentCode(ctx, valueHash("bobs"), now); !errors.Is(err, ErrNoEnrolmentCode) {
			t.Errorf("a first administrator's link was used after the bootstrap token ended, answered %v", err)
		}
		if _, err := w.IssueEnrolmentCode(ctx, link("alice", "late")); !errors.Is(err, ErrBootstrapEnded) {
			t.Errorf("a first administrator's link was issued after the bootstrap token ended, answered %v", err)
		}
		recovery := link("alice", "recovery")
		recovery.Kind, recovery.IssuedBy = EnrolmentRecovery, "bob"
		if _, err := w.IssueEnrolmentCode(ctx, recovery); err != nil {
			t.Errorf("a recovery code was refused after the bootstrap token ended: %v", err)
		}
		return nil
	})
}

// The recovery codes the bootstrap token issued open nothing once it has ended, as its first
// administrators' links do, and nothing else it or anybody issued is closed by the end: an
// administrator's recovery code, the installation's, and a new user's link the token issued. The end
// writes to no recovery code's row, so that it waits on none a transaction holds; a code spent
// before it stays spent. Ended a second time, it changes nothing more.
func TestTheRecoveryCodesTheBootstrapTokenIssuedEndWithIt(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	code := func(login, kind, by, value string) EnrolmentCode {
		return EnrolmentCode{Hash: valueHash(value), Login: login, Kind: kind, IssuedBy: by, IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
	}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, u := range []User{{Login: "alice", Admin: true}, {Login: "alcie", Admin: true}, {Login: "bob"}, {Login: "carol", Admin: true}, {Login: "dan"}} {
			u.DisplayName = u.Login
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		for _, c := range []EnrolmentCode{
			code("alcie", EnrolmentRecovery, "operator", "the token's"),
			code("dan", EnrolmentRecovery, "operator", "spent"),
			code("bob", EnrolmentNewUser, "operator", "a new user's"),
			code("carol", EnrolmentRecovery, "alice", "an administrator's"),
			code("alice", EnrolmentRecovery, "installation", "the installation's"),
		} {
			if _, err := w.IssueEnrolmentCode(ctx, c); err != nil {
				return err
			}
		}
		if _, err := w.EnrolmentCodeByHash(ctx, valueHash("the token's"), now); err != nil {
			t.Errorf("a recovery code the bootstrap token issued was answered %v while it lives", err)
		}
		if _, err := w.UseEnrolmentCode(ctx, valueHash("spent"), now); err != nil {
			return err
		}
		if ended, err := w.EndBootstrap(ctx, now); err != nil || !ended {
			t.Errorf("the bootstrap token ended as %v, %v", ended, err)
		}
		if _, err := w.EnrolmentCodeByHash(ctx, valueHash("the token's"), now); !errors.Is(err, ErrNoEnrolmentCode) {
			t.Errorf("a recovery code the bootstrap token issued was answered %v once it ended", err)
		}
		if _, err := w.UseEnrolmentCode(ctx, valueHash("the token's"), now); !errors.Is(err, ErrNoEnrolmentCode) {
			t.Errorf("a recovery code the bootstrap token issued was spent once it ended: %v", err)
		}
		for _, value := range []string{"a new user's", "an administrator's", "the installation's"} {
			if _, err := w.EnrolmentCodeByHash(ctx, valueHash(value), now); err != nil {
				t.Errorf("%s code was answered %v once the bootstrap token ended", value, err)
			}
		}
		var revoked int
		if err := w.tx.QueryRow(ctx, `select count(*) from enrolment_codes where revoked_at is not null or (used_at is not null and login <> 'dan')`).Scan(&revoked); err != nil {
			return err
		}
		if revoked != 0 {
			t.Errorf("the end of the bootstrap token wrote to %d recovery codes or links", revoked)
		}
		if ended, err := w.EndBootstrap(ctx, now.Add(time.Minute)); err != nil || ended {
			t.Errorf("the bootstrap token ended a second time as %v, %v", ended, err)
		}
		return nil
	})
}

// A first administrator's link and a new user's link open nothing once their user holds a
// credential, however it came, since each enrols the first credential of an account that holds
// none: a link left open beside a recovery code that enrolled its user would enrol a second for
// whoever finds it. A recovery code opens its user's account whatever they hold.
func TestALinkOpensNothingOnceItsUserHoldsACredential(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, u := range []User{{Login: "alice", Admin: true}, {Login: "bob"}} {
			u.DisplayName = u.Login
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		for _, c := range []EnrolmentCode{
			{Hash: valueHash("alice's link"), Login: "alice", Kind: EnrolmentFirstAdministrator, IssuedBy: "operator"},
			{Hash: valueHash("bob's link"), Login: "bob", Kind: EnrolmentNewUser, IssuedBy: "alice"},
			{Hash: valueHash("bob's recovery"), Login: "bob", Kind: EnrolmentRecovery, IssuedBy: "alice"},
		} {
			c.IssuedAt, c.ExpiresAt = now, now.Add(time.Hour)
			if _, err := w.IssueEnrolmentCode(ctx, c); err != nil {
				return err
			}
		}
		for _, login := range []string{"alice", "bob"} {
			if err := w.AddCredential(ctx, Credential{ID: login + "-passkey", Login: login, Type: CredentialPasskey, PublicKey: []byte{1}, AAGUID: make([]byte, 16)}); err != nil {
				return err
			}
		}
		for _, value := range []string{"alice's link", "bob's link"} {
			if _, err := w.EnrolmentCodeByHash(ctx, valueHash(value), now); !errors.Is(err, ErrNoEnrolmentCode) {
				t.Errorf("%s was answered %v once its user held a passkey", value, err)
			}
			if _, err := w.UseEnrolmentCode(ctx, valueHash(value), now); !errors.Is(err, ErrNoEnrolmentCode) {
				t.Errorf("%s was spent once its user held a passkey: %v", value, err)
			}
		}
		if _, err := w.UseEnrolmentCode(ctx, valueHash("bob's recovery"), now); err != nil {
			t.Errorf("bob's recovery code was answered %v while he holds a passkey", err)
		}
		return nil
	})
}

// Two links issued for one user at once both succeed, the second replacing the first, rather than
// the second failing on the first.
func TestTwoLinksIssuedAtOnceTakeTurns(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		return w.CreateUser(ctx, User{Login: "alice", DisplayName: "Alice"})
	})
	errs := make(chan error, 2)
	for _, value := range []string{"one", "two"} {
		go func() {
			errs <- pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
				_, err := w.IssueEnrolmentCode(ctx, EnrolmentCode{Hash: valueHash(value), Login: "alice", Kind: EnrolmentRecovery,
					IssuedBy: "bob", IssuedAt: now, ExpiresAt: now.Add(time.Hour)})
				// Held open a moment, so that the other issue arrives while this one is uncommitted.
				time.Sleep(200 * time.Millisecond)
				return err
			})
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("one of two links issued at once was answered %v", err)
		}
	}
	open := 0
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, value := range []string{"one", "two"} {
			if _, err := w.EnrolmentCodeByHash(ctx, valueHash(value), now); err == nil {
				open++
			}
		}
		return nil
	})
	if open != 1 {
		t.Errorf("%d of two links issued at once are open, and the second replaces the first", open)
	}
}

// Logins and namespaces share one name space, and a signature counter only moves forward.
func TestANameIsALoginOrANamespaceAndACounterMovesForward(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	err := pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
		return w.CreateUser(ctx, User{Login: "finance", DisplayName: "Finance"})
	})
	if !errors.Is(err, ErrNameTaken) {
		t.Errorf("a login named after a namespace was answered %v", err)
	}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		return w.CreateUser(ctx, User{Login: "alice", DisplayName: "Alice"})
	})
	err = pool.Installation(t.Context(), NamespaceAdministration, func(ctx context.Context, w *Wide) error {
		_, err := w.CreateNamespace(ctx, Namespace{Name: "alice"})
		return err
	})
	if !errors.Is(err, ErrNameTaken) {
		t.Errorf("a namespace named after a login was answered %v", err)
	}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, c := range []Credential{
			{ID: "Y291bnRz", Login: "alice", Type: CredentialPasskey, PublicKey: []byte{1}, AAGUID: make([]byte, 16), SignCount: 7},
			{ID: "bm9uZQ", Login: "alice", Type: CredentialPasskey, PublicKey: []byte{1}, AAGUID: make([]byte, 16)},
		} {
			if err := w.AddCredential(ctx, c); err != nil {
				return err
			}
		}
		if err := w.PasskeyUsed(ctx, "Y291bnRz", 10, false, now); err != nil {
			t.Errorf("a counter moving from 7 to 10 was answered %v", err)
		}
		for _, behind := range []uint32{10, 8, 0} {
			if err := w.PasskeyUsed(ctx, "Y291bnRz", behind, false, now); !errors.Is(err, ErrSignCountBehind) {
				t.Errorf("a counter going from 10 to %d was answered %v", behind, err)
			}
		}
		if err := w.PasskeyUsed(ctx, "bm9uZQ", 0, false, now); err != nil {
			t.Errorf("an authenticator that counts nothing was answered %v", err)
		}
		if err := w.PasskeyUsed(ctx, "bm9uZSBhdCBhbGw", 1, false, now); !errors.Is(err, ErrNoCredential) {
			t.Errorf("a passkey nobody enrolled was answered %v", err)
		}
		return nil
	})
}

// A code is not spent, and opens no session, at or past the end of its hour, and what records a
// use or ends one session records that and nothing more.
func TestWhatIsSpentOrUsedIsRecordedAndNothingPastItsHour(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	link := EnrolmentCode{Hash: valueHash("welcome"), Login: "alice", Kind: EnrolmentNewUser, IssuedBy: "bob",
		IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
	password := Credential{ID: "password-alice", Login: "alice", Type: CredentialPassword, PasswordHash: "h"}
	token := APIToken{ID: "01JQ3M8T", Hash: valueHash("agk_alice"), Principal: "alice", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	var accounts []ServiceAccount
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.CreateUser(ctx, User{Login: "alice", DisplayName: "Alice"}); err != nil {
			return err
		}
		if _, err := w.IssueEnrolmentCode(ctx, link); err != nil {
			return err
		}
		if _, err := w.UseEnrolmentCode(ctx, link.Hash, link.ExpiresAt); !errors.Is(err, ErrNoEnrolmentCode) {
			t.Errorf("a code was spent at the end of its hour, answered %v", err)
		}
		if err := w.AddCredential(ctx, password); err != nil {
			return err
		}
		if err := w.CredentialUsed(ctx, password.ID, now); err != nil {
			return err
		}
		if err := w.MintToken(ctx, token); err != nil {
			return err
		}
		if err := w.TokenUsed(ctx, token.ID, now.Add(time.Minute)); err != nil {
			return err
		}
		for _, value := range []string{"kept", "ended"} {
			if err := w.OpenSession(ctx, Session{Hash: valueHash(value), Login: "alice", Credential: password.ID,
				CreatedAt: now, IdleExpiresAt: now.Add(time.Hour)}); err != nil {
				return err
			}
		}
		if n, err := w.RevokeSessions(ctx, "alice", valueHash("ended"), now); err != nil || n != 1 {
			t.Errorf("one session was revoked as %d, %v", n, err)
		}
		if _, err := w.SessionByHash(ctx, valueHash("kept"), now); err != nil {
			t.Errorf("the session left alone was answered %v", err)
		}
		if err := w.CreateServiceAccount(ctx, ServiceAccount{Namespace: "finance", Name: BuiltIn}); err != nil {
			return err
		}
		if err := w.CreateServiceAccount(ctx, ServiceAccount{Namespace: "finance", Name: "nightly-sync", CreatedBy: "alice"}); err != nil {
			return err
		}
		var err error
		accounts, err = w.ServiceAccounts(ctx, "finance")
		if err != nil {
			return err
		}
		used, err := w.Credential(ctx, password.ID)
		if err != nil {
			return err
		}
		if !used.LastUsedAt.Equal(now) {
			t.Errorf("the password reads as last used at %s", used.LastUsedAt)
		}
		tokens, err := w.TokensOf(ctx, "alice", nil, now)
		if err != nil {
			return err
		}
		if len(tokens) != 1 || !tokens[0].LastUsedAt.Equal(now.Add(time.Minute)) {
			t.Errorf("alice's token reads as %+v", tokens)
		}
		return nil
	})
	if len(accounts) != 2 || accounts[0].Principal() != "finance/agentiik" || accounts[0].CreatedBy != "" ||
		accounts[1].Principal() != "finance/nightly-sync" || accounts[1].CreatedBy != "alice" {
		t.Errorf("finance's service accounts are %+v", accounts)
	}

	for _, c := range []struct {
		what string
		do   func(context.Context, *Wide) error
		want error
	}{
		{"a session of nobody", func(ctx context.Context, w *Wide) error {
			return w.OpenSession(ctx, Session{Hash: valueHash("nobody"), Login: "carol", Credential: password.ID,
				CreatedAt: now, IdleExpiresAt: now.Add(time.Hour)})
		}, ErrNoPrincipal},
	} {
		if err := pool.Installation(t.Context(), Identity, c.do); !errors.Is(err, c.want) {
			t.Errorf("%s was answered %v", c.what, err)
		}
	}
	err := pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
		return w.CreateServiceAccount(ctx, ServiceAccount{Namespace: "finance", Name: "deploy"})
	})
	if err == nil || !strings.Contains(err.Error(), "not the built-in identity") {
		t.Errorf("a service account nobody created was answered %v", err)
	}
}
