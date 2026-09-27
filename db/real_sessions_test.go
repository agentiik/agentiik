package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A fresh link ends the session of the one it replaces even where that one was spent as its session
// opened, which a code may be: a link that leaked and was opened first is shut by issuing another.
// The sessions of another user's codes, and of codes of another kind, are left as they are, since
// a fresh link replaces only its own kind; a first administrator's replaces every one of its kind,
// whoever it was for.
func TestAFreshLinkEndsTheSessionOfOneSpentAsItOpened(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	code := func(login, value, kind string) EnrolmentCode {
		return EnrolmentCode{Hash: valueHash(value), Login: login, Kind: kind, IssuedBy: "operator", IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
	}
	// opened spends c and opens a session with it, as an enrolment page may.
	opened := func(ctx context.Context, w *Wide, c EnrolmentCode, value string) error {
		if _, err := w.IssueEnrolmentCode(ctx, c); err != nil {
			return err
		}
		if _, err := w.UseEnrolmentCode(ctx, c.Hash, now); err != nil {
			return err
		}
		return w.OpenSession(ctx, Session{Hash: valueHash(value), Login: c.Login, EnrolmentCode: c.Hash, CreatedAt: now, IdleExpiresAt: now.Add(30 * time.Minute)})
	}
	live := func(ctx context.Context, w *Wide, value string) bool {
		t.Helper()
		_, err := w.SessionByHash(ctx, valueHash(value), now.Add(time.Minute))
		if err != nil && !errors.Is(err, ErrNoSession) {
			t.Fatal(err)
		}
		return err == nil
	}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, login := range []string{"alcie", "alice", "bob"} {
			if err := w.CreateUser(ctx, User{Login: login, DisplayName: login}); err != nil {
				return err
			}
		}
		for _, o := range []struct {
			c     EnrolmentCode
			value string
		}{
			{code("alice", "alice-recovery", EnrolmentRecovery), "by-alice-recovery"},
			{code("alice", "alice-link", EnrolmentNewUser), "by-alice-link"},
			{code("bob", "bob-recovery", EnrolmentRecovery), "by-bob-recovery"},
			{code("alcie", "mistyped", EnrolmentFirstAdministrator), "by-mistyped"},
		} {
			if err := opened(ctx, w, o.c, o.value); err != nil {
				return err
			}
		}
		for _, value := range []string{"by-alice-recovery", "by-alice-link", "by-bob-recovery", "by-mistyped"} {
			if !live(ctx, w, value) {
				t.Fatalf("the session %s is not live once its code was spent as it opened", value)
			}
		}

		fresh := code("alice", "alice-recovery-again", EnrolmentRecovery)
		fresh.IssuedAt = now.Add(time.Second)
		if _, err := w.IssueEnrolmentCode(ctx, fresh); err != nil {
			return err
		}
		if live(ctx, w, "by-alice-recovery") {
			t.Error("a recovery code issued again left open the session of the one it replaced, spent as that session opened")
		}
		if !live(ctx, w, "by-alice-link") || !live(ctx, w, "by-bob-recovery") {
			t.Error("a recovery code issued for alice ended the session of another kind of code, or of another user's")
		}

		meant := code("alice", "meant", EnrolmentFirstAdministrator)
		meant.IssuedAt = now.Add(time.Second)
		if _, err := w.IssueEnrolmentCode(ctx, meant); err != nil {
			return err
		}
		if live(ctx, w, "by-mistyped") {
			t.Error("a first administrator's link for alice left open the session the mistyped one for alcie opened")
		}
		return nil
	})
}

// A link is used once: a code spent before a session opens, as a registration spends it with no
// session behind it, opens none afterwards, and one spent as its session opens opens that one.
func TestACodeSpentBeforeASessionOpensOpensNone(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	code := func(value string) EnrolmentCode {
		return EnrolmentCode{Hash: valueHash(value), Login: "alice", Kind: EnrolmentRecovery, IssuedBy: "bob", IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
	}
	session := func(value string, c EnrolmentCode, at time.Time) Session {
		return Session{Hash: valueHash(value), Login: "alice", EnrolmentCode: c.Hash, CreatedAt: at, IdleExpiresAt: at.Add(30 * time.Minute)}
	}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.CreateUser(ctx, User{Login: "alice", DisplayName: "Alice"}); err != nil {
			return err
		}
		spent := code("spent")
		if _, err := w.IssueEnrolmentCode(ctx, spent); err != nil {
			return err
		}
		if _, err := w.UseEnrolmentCode(ctx, spent.Hash, now.Add(time.Minute)); err != nil {
			return err
		}
		if err := w.OpenSession(ctx, session("after", spent, now.Add(10*time.Minute))); !errors.Is(err, ErrSessionRefused) {
			t.Errorf("a code spent ten minutes before opened a session, answered %v", err)
		}

		// Issued again, and spent as its session opens.
		again := code("again")
		again.IssuedAt = now.Add(11 * time.Minute)
		again.ExpiresAt = again.IssuedAt.Add(time.Hour)
		if _, err := w.IssueEnrolmentCode(ctx, again); err != nil {
			return err
		}
		opened := now.Add(12 * time.Minute)
		if _, err := w.UseEnrolmentCode(ctx, again.Hash, opened); err != nil {
			return err
		}
		if err := w.OpenSession(ctx, session("as-spent", again, opened)); err != nil {
			t.Errorf("a code spent as its session opened opened none: %v", err)
		}
		return nil
	})
}
