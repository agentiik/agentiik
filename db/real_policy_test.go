package db

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/jackc/pgx/v5/pgconn"
)

// The authentication policy's reach and what it suspends, as the application's role reaches them.

// A suspension the policy makes records why, and only a lifting for that reason lifts it: one made
// for no reason recorded, or already there, is kept as it was. A reason is never written on an
// account that is not suspended, and a user written as not suspended loses the reason with the
// suspension.
func TestASuspensionKeepsItsReasonAndIsLiftedForItAlone(t *testing.T) {
	pool := identity(t)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, u := range []User{{Login: "alice", DisplayName: "Alice"}, {Login: "bob", DisplayName: "Bob", Suspended: true}} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		if made, err := w.Suspend(ctx, "alice", SuspendedNoPasskey); err != nil || !made {
			t.Errorf("suspending alice answered %t, %v", made, err)
		}
		if made, err := w.Suspend(ctx, "bob", SuspendedNoPasskey); err != nil || made {
			t.Errorf("suspending bob, suspended already, answered %t, %v", made, err)
		}
		alice, err := w.User(ctx, "alice")
		if err != nil || !alice.Suspended || alice.SuspendedFor != SuspendedNoPasskey {
			t.Errorf("alice reads %+v, %v", alice, err)
		}
		if bob, err := w.User(ctx, "bob"); err != nil || bob.SuspendedFor != "" {
			t.Errorf("bob reads %+v, %v", bob, err)
		}
		if lifted, err := w.LiftSuspension(ctx, "bob", SuspendedNoPasskey); err != nil || lifted {
			t.Errorf("lifting bob's suspension, recorded for no reason, answered %t, %v", lifted, err)
		}
		if lifted, err := w.LiftSuspension(ctx, "alice", SuspendedNoPasskey); err != nil || !lifted {
			t.Errorf("lifting alice's suspension answered %t, %v", lifted, err)
		}
		if alice, err := w.User(ctx, "alice"); err != nil || alice.Suspended || alice.SuspendedFor != "" {
			t.Errorf("alice reads %+v, %v, once her suspension is lifted", alice, err)
		}
		if _, err := w.Suspend(ctx, "alice", SuspendedNoPasskey); err != nil {
			return err
		}
		if err := w.UpdateUser(ctx, User{Login: "alice", DisplayName: "Alice"}); err != nil {
			t.Errorf("writing alice as not suspended answered %v", err)
		}
		if alice, err := w.User(ctx, "alice"); err != nil || alice.SuspendedFor != "" {
			t.Errorf("alice written as not suspended reads %+v, %v", alice, err)
		}
		return nil
	})
	for _, stmt := range []string{
		`update users set suspended_for = 'no_passkey' where login = 'alice'`,
		`update users set suspended = true, suspended_for = 'another' where login = 'alice'`,
	} {
		err := pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
			_, err := w.tx.Exec(ctx, stmt)
			return err
		})
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != checkViolation {
			t.Errorf("%s answered %v", stmt, err)
		}
	}
}

// A namespace's policy reaches the users holding a grant carrying a role in it at the moment asked,
// their own or a group's, on the namespace or on one of its workflows; not one whose grant has
// expired, nor one holding only a deny there, nor one holding nothing there.
func TestANamespacesPolicyReachesWhoHoldsARoleInIt(t *testing.T) {
	pool := identity(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	gone := now.Add(-time.Minute)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, login := range []string{"alice", "bob", "carol", "dave", "erin", "frank"} {
			if err := w.CreateUser(ctx, User{Login: login, DisplayName: login}); err != nil {
				return err
			}
		}
		if err := w.CreateGroup(ctx, "auditors"); err != nil {
			return err
		}
		if _, err := w.AddMember(ctx, "auditors", "bob"); err != nil {
			return err
		}
		for _, g := range []access.Grant{
			{ID: "01M2T1AAAAAAAAAAAAAAAAAAA1", Principal: "alice", Scope: access.Scope{Namespace: "finance"}, Role: access.Viewer},
			{ID: "01M2T1AAAAAAAAAAAAAAAAAAA2", Principal: "group:auditors", Scope: access.Scope{Namespace: "finance"}, Role: access.Viewer},
			{ID: "01M2T1AAAAAAAAAAAAAAAAAAA3", Principal: "carol", Scope: access.Scope{Namespace: "finance", Workflow: "monthly-invoicing"}, Role: access.Operator},
			{ID: "01M2T1AAAAAAAAAAAAAAAAAAA4", Principal: "dave", Scope: access.Scope{Namespace: "finance"}, Deny: access.RunReadData},
			{ID: "01M2T1AAAAAAAAAAAAAAAAAAA5", Principal: "erin", Scope: access.Scope{Namespace: "finance"}, Role: access.Viewer, ExpiresAt: &gone},
			{ID: "01M2T1AAAAAAAAAAAAAAAAAAA6", Principal: "frank", Scope: access.Scope{Namespace: "team-ops"}, Role: access.Owner},
		} {
			g.GrantedBy = "alice"
			if err := w.GrantAccess(ctx, g); err != nil {
				return err
			}
		}
		reached, err := w.UsersUnderPolicy(ctx, "finance", now)
		if err != nil {
			return err
		}
		if !slices.Equal(reached, []string{"alice", "bob", "carol"}) {
			t.Errorf("finance's policy reaches %q", reached)
		}
		all, err := w.Logins(ctx)
		if err != nil {
			return err
		}
		if !slices.Equal(all, []string{"alice", "bob", "carol", "dave", "erin", "frank"}) {
			t.Errorf("the installation's reaches %q", all)
		}
		return nil
	})
}

// Holding a user's row, as every act on an account does and forbidding passwords does of every
// account, keeps no row referring to it from being written, a membership, a credential or a
// challenge: an act holding many users' rows would otherwise wait on a group created with two of
// them as members in the other order, while the group waits on it.
func TestHoldingAUserLetsARowReferringToItBeWritten(t *testing.T) {
	pool := identity(t)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.CreateUser(ctx, User{Login: "alice", DisplayName: "Alice", Admin: true}); err != nil {
			return err
		}
		return w.CreateGroup(ctx, "auditors")
	})
	for name, hold := range map[string]func(context.Context, *Wide) error{
		"HoldUser":       func(ctx context.Context, w *Wide) error { _, err := w.HoldUser(ctx, "alice"); return err },
		"Administrators": func(ctx context.Context, w *Wide) error { _, err := w.Administrators(ctx); return err },
	} {
		held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			done <- pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
				if err := hold(ctx, w); err != nil {
					return err
				}
				close(held)
				<-release
				return nil
			})
		}()
		<-held
		err := pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
			if _, err := w.tx.Exec(ctx, `set local lock_timeout = '2s'`); err != nil {
				return err
			}
			if _, err := w.AddMember(ctx, "auditors", "alice"); err != nil {
				return err
			}
			_, err := w.RemoveMember(ctx, "auditors", "alice")
			return err
		})
		close(release)
		if err != nil {
			t.Errorf("a membership of alice, her row held by %s, answered %v", name, err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
