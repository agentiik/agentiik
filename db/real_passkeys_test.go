package db

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// What the passkey ceremonies keep between the options and the verification, against a real
// PostgreSQL: a challenge answers one verification and none past its five minutes, a user's handle
// is minted once, and a sign-in refused for its counter is told to the passkey's user.

// A challenge is taken once: of two verifications presenting it at once, one takes it, and the
// other is answered as if it had never been issued. Past its five minutes nothing takes it, and the
// next challenge issued removes it. A registration's challenge names its user, and a code of another
// user's is refused.
func TestAChallengeIsTakenOnceAndNotPastItsMinutes(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	challenge := func(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, login := range []string{"alice", "bob"} {
			if err := w.CreateUser(ctx, User{Login: login, DisplayName: login}); err != nil {
				return err
			}
		}
		if _, err := w.IssueEnrolmentCode(ctx, EnrolmentCode{Hash: valueHash("bob-link"), Login: "bob", Kind: EnrolmentNewUser,
			IssuedBy: "operator", IssuedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			return err
		}
		for _, c := range []Challenge{
			{Value: challenge(1), Ceremony: CeremonyAssertion},
			{Value: challenge(2), Ceremony: CeremonyRegistration, Login: "bob", EnrolmentCode: valueHash("bob-link")},
		} {
			c.IssuedAt, c.ExpiresAt = now, now.Add(ChallengeLife)
			if err := w.IssueChallenge(ctx, c); err != nil {
				return err
			}
		}
		return nil
	})
	if err := pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
		return w.IssueChallenge(ctx, Challenge{Value: challenge(3), Ceremony: CeremonyRegistration, Login: "alice",
			EnrolmentCode: valueHash("bob-link"), IssuedAt: now, ExpiresAt: now.Add(ChallengeLife)})
	}); !errors.Is(err, ErrNoEnrolmentCode) {
		t.Errorf("a registration of alice held against bob's code was answered %v", err)
	}

	took := make(chan error, 2)
	for range 2 {
		go func() {
			took <- pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
				_, err := w.TakeChallenge(ctx, challenge(1), now.Add(time.Minute))
				return err
			})
		}()
	}
	first, second := <-took, <-took
	if (first == nil) == (second == nil) || !errors.Is(errors.Join(first, second), ErrNoChallenge) {
		t.Errorf("two verifications taking one challenge at once were answered %v and %v", first, second)
	}

	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if _, err := w.TakeChallenge(ctx, challenge(2), now.Add(ChallengeLife)); !errors.Is(err, ErrNoChallenge) {
			t.Errorf("a challenge five minutes old was answered %v", err)
		}
		if err := w.IssueChallenge(ctx, Challenge{Value: challenge(4), Ceremony: CeremonyAssertion,
			IssuedAt: now.Add(ChallengeLife), ExpiresAt: now.Add(2 * ChallengeLife)}); err != nil {
			return err
		}
		var kept int
		if err := w.tx.QueryRow(ctx, `select count(*) from webauthn_challenges`).Scan(&kept); err != nil {
			return err
		}
		if kept != 1 {
			t.Errorf("%d challenges are kept once the lapsed one should have gone", kept)
		}
		c, err := w.TakeChallenge(ctx, challenge(4), now.Add(ChallengeLife+time.Minute))
		if err != nil || c.Ceremony != CeremonyAssertion || c.Login != "" || c.EnrolmentCode != nil {
			t.Errorf("the challenge issued was taken as %+v, %v", c, err)
		}
		return nil
	})
	if err := pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
		return w.IssueChallenge(ctx, Challenge{Value: challenge(5), Ceremony: CeremonyAssertion, IssuedAt: now, ExpiresAt: now.Add(ChallengeLife + time.Second)})
	}); err == nil {
		t.Error("a challenge living longer than five minutes was issued")
	}
}

// A user's passkey handle is minted by the first registration started for them, and kept: a second
// asking with other bytes is answered the first. Two users are never given the same.
func TestAUsersPasskeyHandleIsMintedOnce(t *testing.T) {
	pool := identity(t)
	first, other := bytes.Repeat([]byte{7}, 32), bytes.Repeat([]byte{8}, 32)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, login := range []string{"alice", "bob"} {
			if err := w.CreateUser(ctx, User{Login: login, DisplayName: login}); err != nil {
				return err
			}
		}
		if h, err := w.PasskeyHandleOf(ctx, "alice"); err != nil || h != nil {
			t.Errorf("alice's handle before any registration is %x, %v", h, err)
		}
		for _, fresh := range [][]byte{first, other} {
			h, err := w.PasskeyHandle(ctx, "alice", fresh)
			if err != nil || !bytes.Equal(h, first) {
				t.Errorf("alice's handle, asked with %x, is %x, %v", fresh, h, err)
			}
		}
		if h, err := w.PasskeyHandleOf(ctx, "alice"); err != nil || !bytes.Equal(h, first) {
			t.Errorf("alice's handle reads %x, %v", h, err)
		}
		if _, err := w.PasskeyHandle(ctx, "nobody", other); !errors.Is(err, ErrNoPrincipal) {
			t.Errorf("a handle for a login nobody has was answered %v", err)
		}
		return nil
	})
	if err := pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
		_, err := w.PasskeyHandle(ctx, "bob", first)
		return err
	}); err == nil {
		t.Error("bob was given alice's handle")
	}
}

// An assertion's options ask for user verification where any policy requires it: the installation's,
// by default, or a namespace's that tightens an installation preferring it.
func TestUserVerificationIsRequiredWhereAnyPolicyRequiresIt(t *testing.T) {
	pool := identity(t)
	required := func() bool {
		t.Helper()
		var r bool
		wide(t, pool, func(ctx context.Context, w *Wide) error {
			var err error
			r, err = w.UserVerificationRequired(ctx)
			return err
		})
		return r
	}
	if !required() {
		t.Error("the default policy does not require user verification")
	}
	preferred := false
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		return w.SetInstallationPolicy(ctx, AuthPolicy{Password: "allowed", Passkey: "required", UserVerification: "preferred", DeviceBoundOnly: &preferred, MinPasskeys: 2}, time.Now())
	})
	if required() {
		t.Error("an installation preferring user verification requires it")
	}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if _, err := w.CreateNamespace(ctx, Namespace{Name: "finance"}); err != nil {
			return err
		}
		return w.SetNamespacePolicy(ctx, "finance", AuthPolicy{UserVerification: "required"}, time.Now())
	})
	if !required() {
		t.Error("a namespace requiring user verification is not asked for")
	}
}

// A sign-in refused for its counter is told to the passkey's user, naming the passkey, and stays
// told once the passkey is removed, which is what its user may do on reading it.
func TestARefusedCounterIsToldToThePasskeysUser(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.CreateUser(ctx, User{Login: "alice", DisplayName: "Alice"}); err != nil {
			return err
		}
		if err := w.AddCredential(ctx, Credential{ID: "a-passkey", Login: "alice", Type: CredentialPasskey, PublicKey: []byte{1}, AAGUID: make([]byte, 16)}); err != nil {
			return err
		}
		if err := w.TellPasskeyRefused(ctx, "alice", "a-passkey", now); err != nil {
			return err
		}
		return w.RemoveCredential(ctx, "alice", "a-passkey")
	})
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		var kind, credential string
		var at time.Time
		if err := w.tx.QueryRow(ctx, `select kind, credential, at from notifications where recipient = 'alice'`).Scan(&kind, &credential, &at); err != nil {
			return err
		}
		if kind != "passkey_counter_refused" || credential != "a-passkey" || !at.Equal(now) {
			t.Errorf("alice is told %s about %s at %s", kind, credential, at)
		}
		return nil
	})
}

// The bootstrap state held to be ended waits for another holding it, as a link issued with the
// bootstrap token holds it, rather than being read past.
func TestTheBootstrapStateHeldToBeEndedIsHeldAgainstALink(t *testing.T) {
	pool := identity(t)
	holding := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
			if _, err := w.HoldBootstrapToEnd(ctx); err != nil {
				close(holding)
				return err
			}
			close(holding)
			<-release
			_, err := w.EndBootstrap(ctx, time.Now())
			return err
		})
	}()
	<-holding
	read := make(chan Bootstrap, 1)
	go func() {
		var b Bootstrap
		_ = pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
			var err error
			b, err = w.BootstrapHeld(ctx)
			return err
		})
		read <- b
	}()
	select {
	case <-read:
		t.Error("the bootstrap state was read past the transaction holding it to be ended")
		close(release)
	case <-time.After(200 * time.Millisecond):
		close(release)
		if b := <-read; !b.Ended() {
			t.Error("the state read once the holder committed does not say the bootstrap ended")
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
