package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// What the password sign-in keeps, against a real PostgreSQL: the step a TOTP code was accepted at,
// which no code of that step or of an earlier one gets past again, and the type of the credential
// that opened a session, which says what the session may do; and what setting a password keeps: the
// password set in place of the one held, a TOTP generator waiting for its first code, and whether a
// user spent a first administrator's link.

// A TOTP code's step is recorded once: of two sign-ins recording one step at once, one records it,
// and the other is ErrTOTPSpent, as is an earlier step afterwards; a later one is recorded. Only a
// TOTP's row holds a step, and a credential of another type or none is ErrNoCredential.
func TestATOTPStepIsRecordedOnceAndNeverGoesBack(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.CreateUser(ctx, User{Login: "bob", DisplayName: "Bob"}); err != nil {
			return err
		}
		for _, c := range []Credential{
			{ID: "bob-password", Login: "bob", Type: CredentialPassword, PasswordHash: "$argon2id$..."},
			{ID: "bob-totp", Login: "bob", Type: CredentialTOTP, TOTPSealed: []byte("sealed")},
		} {
			if err := w.AddCredential(ctx, c); err != nil {
				return err
			}
		}
		return nil
	})
	used := func(id string, step int64) error {
		return pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
			return w.TOTPUsed(ctx, id, step, now)
		})
	}

	recorded := make(chan error, 2)
	for range 2 {
		go func() { recorded <- used("bob-totp", 60_000_000) }()
	}
	first, second := <-recorded, <-recorded
	if (first == nil) == (second == nil) || !errors.Is(errors.Join(first, second), ErrTOTPSpent) {
		t.Errorf("two sign-ins recording one step answered %v and %v", first, second)
	}
	if err := used("bob-totp", 59_999_999); !errors.Is(err, ErrTOTPSpent) {
		t.Errorf("an earlier step answered %v", err)
	}
	if err := used("bob-totp", 60_000_001); err != nil {
		t.Errorf("a later step answered %v", err)
	}
	for _, id := range []string{"bob-password", "nobody's"} {
		if err := used(id, 60_000_002); !errors.Is(err, ErrNoCredential) {
			t.Errorf("recording a step of %s answered %v", id, err)
		}
	}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		c, err := w.Credential(ctx, "bob-totp")
		if err != nil {
			return err
		}
		if c.TOTPStep != 60_000_001 || !c.LastUsedAt.Equal(now) {
			t.Errorf("bob's generator holds the step %d, used at %s", c.TOTPStep, c.LastUsedAt)
		}
		return nil
	})
}

// A session answers the type of the credential that opened it, and none where a code opened it.
func TestASessionSaysWhatOpenedIt(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.CreateUser(ctx, User{Login: "alice", DisplayName: "Alice"}); err != nil {
			return err
		}
		if err := w.AddCredential(ctx, Credential{ID: "alice-password", Login: "alice", Type: CredentialPassword, PasswordHash: "$argon2id$..."}); err != nil {
			return err
		}
		if err := w.AddCredential(ctx, Credential{ID: "alice-passkey", Login: "alice", Type: CredentialPasskey, PublicKey: []byte{1}, AAGUID: make([]byte, 16)}); err != nil {
			return err
		}
		if _, err := w.IssueEnrolmentCode(ctx, EnrolmentCode{Hash: valueHash("alice-recovery"), Login: "alice", Kind: EnrolmentRecovery,
			IssuedBy: "carol", IssuedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			return err
		}
		for _, s := range []Session{
			{Hash: valueHash("by-password"), Login: "alice", Credential: "alice-password"},
			{Hash: valueHash("by-passkey"), Login: "alice", Credential: "alice-passkey"},
			{Hash: valueHash("by-code"), Login: "alice", EnrolmentCode: valueHash("alice-recovery")},
		} {
			s.CreatedAt, s.IdleExpiresAt = now, now.Add(time.Hour)
			if err := w.OpenSession(ctx, s); err != nil {
				return err
			}
		}
		for value, want := range map[string]string{"by-password": CredentialPassword, "by-passkey": CredentialPasskey, "by-code": ""} {
			s, err := w.SessionByHash(ctx, valueHash(value), now)
			if err != nil {
				return err
			}
			if s.CredentialType != want {
				t.Errorf("the session %s says it was opened by %q", value, s.CredentialType)
			}
		}
		return nil
	})
}

// A TOTP exists only alongside a password: one is refused to a user holding no password, a login
// nobody holds is refused as such, and removing the password removes the TOTP with it, as the user
// removed does both.
func TestATOTPExistsOnlyBesideAPassword(t *testing.T) {
	pool := identity(t)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, login := range []string{"alice", "bob"} {
			if err := w.CreateUser(ctx, User{Login: login, DisplayName: login}); err != nil {
				return err
			}
		}
		return w.AddCredential(ctx, Credential{ID: "bob-password", Login: "bob", Type: CredentialPassword, PasswordHash: "$argon2id$..."})
	})
	add := func(login, id string) error {
		return pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
			return w.AddCredential(ctx, Credential{ID: id, Login: login, Type: CredentialTOTP, TOTPSealed: []byte("sealed")})
		})
	}
	if err := add("alice", "alice-totp"); !errors.Is(err, ErrNoPassword) {
		t.Errorf("a TOTP for alice, holding no password, answered %v", err)
	}
	if err := add("nobody", "nobody-totp"); !errors.Is(err, ErrNoPrincipal) {
		t.Errorf("a TOTP for a login nobody holds answered %v", err)
	}
	if err := add("bob", "bob-totp"); err != nil {
		t.Fatalf("a TOTP beside bob's password answered %v", err)
	}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.RemoveCredential(ctx, "bob", "bob-password"); err != nil {
			return err
		}
		held, err := w.CredentialsOf(ctx, "bob")
		if len(held) != 0 {
			t.Errorf("once bob's password was removed he holds %+v", held)
		}
		return err
	})
	if err := add("bob", "bob-totp"); !errors.Is(err, ErrNoPassword) {
		t.Errorf("a TOTP for bob, his password removed, answered %v", err)
	}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.AddCredential(ctx, Credential{ID: "alice-password", Login: "alice", Type: CredentialPassword, PasswordHash: "$argon2id$..."}); err != nil {
			return err
		}
		if err := w.AddCredential(ctx, Credential{ID: "alice-totp", Login: "alice", Type: CredentialTOTP, TOTPSealed: []byte("sealed")}); err != nil {
			return err
		}
		return w.RemovePrincipal(ctx, "alice")
	})
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if held, err := w.CredentialsOf(ctx, "alice"); err != nil || len(held) != 0 {
			t.Errorf("once alice was removed, %d of her credentials are left: %v", len(held), err)
		}
		return nil
	})
}

// A password set where none is held is enrolled under the identifier given, at the time given; set
// again, it is replaced in its row, the identifier and the TOTP beside it kept, recorded as set then
// and used since by nobody, and the sessions it opened left for its caller to end, every one but the
// one kept; set for a login nobody holds, it is ErrNoPrincipal.
func TestAPasswordIsSetInPlaceOfTheOneHeld(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	later := now.Add(time.Hour)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.CreateUser(ctx, User{Login: "bob", DisplayName: "Bob"}); err != nil {
			return err
		}
		set, replaced, err := w.SetPassword(ctx, "bob", "bob-password", "$argon2id$first", now)
		if err != nil {
			return err
		}
		if replaced || set.ID != "bob-password" || set.Type != CredentialPassword || set.PasswordHash != "$argon2id$first" || !set.CreatedAt.Equal(now) {
			t.Errorf("bob's first password is %+v, replacing one: %v", set, replaced)
		}
		if err := w.AddCredential(ctx, Credential{ID: "bob-totp", Login: "bob", Type: CredentialTOTP, TOTPSealed: []byte("sealed"), TOTPStep: 60_000_000}); err != nil {
			return err
		}
		if err := w.CredentialUsed(ctx, "bob-password", now); err != nil {
			return err
		}
		for _, value := range []string{"setting", "other", "another"} {
			if err := w.OpenSession(ctx, Session{Hash: valueHash(value), Login: "bob", Credential: "bob-password", CreatedAt: now, IdleExpiresAt: later.Add(time.Hour)}); err != nil {
				return err
			}
		}

		set, replaced, err = w.SetPassword(ctx, "bob", "unused", "$argon2id$second", later)
		if err != nil {
			return err
		}
		if !replaced || set.ID != "bob-password" || set.PasswordHash != "$argon2id$second" || !set.CreatedAt.Equal(later) || !set.LastUsedAt.IsZero() {
			t.Errorf("bob's second password is %+v, replacing one: %v", set, replaced)
		}
		if c, err := w.Credential(ctx, "bob-totp"); err != nil || c.TOTPStep != 60_000_000 {
			t.Errorf("bob's generator after the password was set again is %+v: %v", c, err)
		}
		if _, err := w.SessionByHash(ctx, valueHash("other"), now); err != nil {
			t.Errorf("setting the password ended a session by itself: %v", err)
		}
		ended, err := w.EndSessionsOpenedBy(ctx, "bob", "bob-password", valueHash("setting"), later)
		if err != nil {
			return err
		}
		if ended != 2 {
			t.Errorf("%d sessions were ended", ended)
		}
		for value, live := range map[string]bool{"setting": true, "other": false, "another": false} {
			if _, err := w.SessionByHash(ctx, valueHash(value), later); (err == nil) != live {
				t.Errorf("the session %s is live: %v", value, err)
			}
		}
		return nil
	})
	err := pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
		_, _, err := w.SetPassword(ctx, "nobody", "nobody-password", "$argon2id$x", now)
		return err
	})
	if !errors.Is(err, ErrNoPrincipal) {
		t.Errorf("a password for a login nobody holds answered %v", err)
	}
}

// A TOTP generator started waits for its first code ten minutes at most, one for each user: started
// again, the one before is replaced; past its minutes, or forgotten under its identifier, it is
// ErrNoTOTPEnrolment; forgotten under another, it is kept. It goes with its user.
func TestATOTPGeneratorWaitsForItsFirstCode(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.CreateUser(ctx, User{Login: "bob", DisplayName: "Bob"}); err != nil {
			return err
		}
		started := TOTPEnrolment{Login: "bob", ID: "first", Sealed: []byte("sealed"), StartedAt: now, ExpiresAt: now.Add(TOTPEnrolmentLife)}
		if err := w.StartTOTP(ctx, started); err != nil {
			return err
		}
		started.ID, started.Sealed = "second", []byte("sealed again")
		if err := w.StartTOTP(ctx, started); err != nil {
			return err
		}
		e, err := w.TOTPEnrolmentOf(ctx, "bob", now.Add(TOTPEnrolmentLife-time.Microsecond))
		if err != nil || e.ID != "second" || string(e.Sealed) != "sealed again" || !e.ExpiresAt.Equal(now.Add(TOTPEnrolmentLife)) {
			t.Errorf("the generator waiting is %+v: %v", e, err)
		}
		if _, err := w.TOTPEnrolmentOf(ctx, "bob", now.Add(TOTPEnrolmentLife)); !errors.Is(err, ErrNoTOTPEnrolment) {
			t.Errorf("a generator past its minutes answered %v", err)
		}
		if err := w.EndTOTPEnrolment(ctx, "bob", "first"); err != nil {
			return err
		}
		if _, err := w.TOTPEnrolmentOf(ctx, "bob", now); err != nil {
			t.Errorf("forgetting the generator replaced forgot the one waiting: %v", err)
		}
		if err := w.EndTOTPEnrolment(ctx, "bob", "second"); err != nil {
			return err
		}
		if _, err := w.TOTPEnrolmentOf(ctx, "bob", now); !errors.Is(err, ErrNoTOTPEnrolment) {
			t.Errorf("the generator forgotten answered %v", err)
		}
		return nil
	})
	err := pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
		return w.StartTOTP(ctx, TOTPEnrolment{Login: "nobody", ID: "x", Sealed: []byte("s"), StartedAt: now, ExpiresAt: now.Add(time.Minute)})
	})
	if !errors.Is(err, ErrNoPrincipal) {
		t.Errorf("a generator for a login nobody holds answered %v", err)
	}
	err = pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
		return w.StartTOTP(ctx, TOTPEnrolment{Login: "bob", ID: "long", Sealed: []byte("s"), StartedAt: now, ExpiresAt: now.Add(TOTPEnrolmentLife + time.Second)})
	})
	if err == nil || !strings.Contains(err.Error(), "totp_enrolments_expiry") {
		t.Errorf("a generator waiting more than ten minutes answered %v", err)
	}
}

// Whether a user enrolled with a first administrator's link: not before it is spent, not for a new
// user's link spent, and so once one of theirs is.
func TestAFirstAdministratorIsKnownByTheLinkTheySpent(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, login := range []string{"alice", "bob"} {
			if err := w.CreateUser(ctx, User{Login: login, DisplayName: login}); err != nil {
				return err
			}
		}
		for login, kind := range map[string]string{"alice": EnrolmentFirstAdministrator, "bob": EnrolmentNewUser} {
			if _, err := w.IssueEnrolmentCode(ctx, EnrolmentCode{Hash: valueHash(login), Login: login, Kind: kind,
				IssuedBy: "operator", IssuedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
				return err
			}
		}
		if spent, err := w.SpentFirstAdministratorLink(ctx, "alice"); err != nil || spent {
			t.Errorf("before her link was spent, alice spent one: %v %v", spent, err)
		}
		for _, login := range []string{"alice", "bob"} {
			if _, err := w.UseEnrolmentCode(ctx, valueHash(login), now); err != nil {
				return err
			}
		}
		for login, want := range map[string]bool{"alice": true, "bob": false, "nobody": false} {
			if spent, err := w.SpentFirstAdministratorLink(ctx, login); err != nil || spent != want {
				t.Errorf("%s spent a first administrator's link: %v %v", login, spent, err)
			}
		}
		return nil
	})
}
