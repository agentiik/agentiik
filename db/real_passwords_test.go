package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// What the password sign-in keeps, against a real PostgreSQL: the step a TOTP code was accepted at,
// which no code of that step or of an earlier one gets past again, and the type of the credential
// that opened a session, which says what the session may do.

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
