package db

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"slices"
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
	if !slices.Equal(groups, []string{"group:team-finance"}) {
		t.Errorf("alice's groups are %v, as a grant names them", groups)
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

func TestACredentialIsARowOfItsOwn(t *testing.T) {
	pool := identity(t)
	ctx := t.Context()
	passkey := Credential{
		ID: "cGFzc2tleQ", Login: "alice", Type: CredentialPasskey, Label: "laptop",
		PublicKey: []byte{0xa5, 0x01, 0x02}, SignCount: 4294967295, AAGUID: make([]byte, 16),
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
		if err := w.PasskeyUsed(ctx, passkey.ID, 7, false, now); err != nil {
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
	if read.SignCount != 7 || read.BackupState || !read.LastUsedAt.Equal(now) {
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
		listed, err := w.TokensOf(ctx, "alice")
		if err != nil {
			return err
		}
		if len(listed) != 1 || !listed[0].RevokedAt.Equal(now) {
			t.Errorf("alice's tokens are listed as %+v", listed)
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

func TestAnEnrolmentCodeIsSpentOnceAndASessionIsNotOpenedAgain(t *testing.T) {
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
		return w.OpenSession(ctx, Session{Hash: valueHash("enrolling"), Login: "alice", EnrolmentCode: again.Hash,
			CreatedAt: now, IdleExpiresAt: now.Add(10 * time.Minute)})
	})

	var session Session
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		var err error
		if session, err = w.SessionByHash(ctx, valueHash("enrolling"), now); err != nil {
			return err
		}
		if used, err := w.UseEnrolmentCode(ctx, again.Hash, now.Add(2*time.Minute)); err != nil || !used.UsedAt.Equal(now.Add(2*time.Minute)) {
			t.Errorf("the link was spent as %+v, %v", used, err)
		}
		if _, err := w.UseEnrolmentCode(ctx, again.Hash, now.Add(3*time.Minute)); !errors.Is(err, ErrNoEnrolmentCode) {
			t.Errorf("the link was spent twice, answered %v", err)
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
	if session.Credential != "" || !bytes.Equal(session.EnrolmentCode, again.Hash) {
		t.Errorf("the session reads as opened by %q and %x, and an enrolment code opened it", session.Credential, session.EnrolmentCode)
	}
}
