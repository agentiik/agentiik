package db

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
)

// What the installation tells a principal: an administrator's grant to themselves, told to the
// owners of the namespace it was written in, and read and dismissed by each of them.

func TestAnAdministratorsGrantIsToldToTheOwnersOfItsNamespace(t *testing.T) {
	pool := identity(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, u := range []User{
			{Login: "alice", DisplayName: "Alice"}, {Login: "carol", DisplayName: "Carol", Admin: true}, {Login: "dave", DisplayName: "Dave"},
			{Login: "erin", DisplayName: "Erin"}, {Login: "frank", DisplayName: "Frank"}, {Login: "gina", DisplayName: "Gina"},
		} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		if err := w.CreateGroup(ctx, "leads"); err != nil {
			return err
		}
		for _, login := range []string{"dave", "erin", "carol"} {
			if _, err := w.AddMember(ctx, "leads", login); err != nil {
				return err
			}
		}
		return w.CreateServiceAccount(ctx, ServiceAccount{Namespace: "finance", Name: "nightly", CreatedBy: "carol"})
	})
	ended := now.Add(-time.Minute)
	grant := func(namespace string, gs ...access.Grant) {
		t.Helper()
		err := pool.In(ctx, namespace, func(ctx context.Context, n *NS) error {
			for i, g := range gs {
				g.ID, g.GrantedBy = "01JQ4"+strings.ToUpper(namespace[:1])+string(rune('A'+i)), "carol"
				if err := n.GrantAccess(ctx, g); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	finance := access.Scope{Namespace: "finance"}
	// finance's record names no owner, as a namespace from v0.2 names none: whoever holds the
	// owner role on it is told, a group as its members and a service account as itself, and not
	// whoever holds it on a workflow, holds it no longer, or holds another role.
	grant("finance",
		access.Grant{Principal: "frank", Scope: finance, Role: access.Owner},
		access.Grant{Principal: "frank", Scope: finance, Deny: access.RunReadData},
		access.Grant{Principal: "group:leads", Scope: finance, Role: access.Owner},
		access.Grant{Principal: "finance/nightly", Scope: finance, Role: access.Owner},
		access.Grant{Principal: "gina", Scope: finance, Role: access.Owner, ExpiresAt: &ended},
		access.Grant{Principal: "gina", Scope: access.Scope{Namespace: "finance", Workflow: "monthly-invoicing"}, Role: access.Owner},
		access.Grant{Principal: "alice", Scope: finance, Role: access.Editor},
	)
	// team-ops's names group:leads, whose members are told, and not frank, who holds the role
	// beside it.
	grant("team-ops", access.Grant{Principal: "frank", Scope: access.Scope{Namespace: "team-ops"}, Role: access.Owner})
	if err := pool.Installation(ctx, NamespaceAdministration, func(ctx context.Context, w *Wide) error {
		return w.SetOwner(ctx, "team-ops", "group:leads")
	}); err != nil {
		t.Fatal(err)
	}

	widened := access.Grant{ID: "01JQ4W", Principal: "carol", Scope: finance, Role: access.Editor, GrantedBy: "carol", GrantedAt: now}
	tell := func(namespace string, g access.Grant, at time.Time) []string {
		t.Helper()
		var told []string
		err := pool.In(ctx, namespace, func(ctx context.Context, n *NS) error {
			var err error
			told, err = n.TellOwners(ctx, g, "carol", at)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return told
	}
	if told := tell("finance", widened, now); !slices.Equal(told, []string{"dave", "erin", "finance/nightly", "frank"}) {
		t.Errorf("carol's grant in finance was told to %q", told)
	}
	opsGrant := widened
	opsGrant.ID, opsGrant.Scope = "01JQ4X", access.Scope{Namespace: "team-ops"}
	later := now.Add(time.Second)
	if told := tell("team-ops", opsGrant, later); !slices.Equal(told, []string{"dave", "erin"}) {
		t.Errorf("carol's grant in team-ops was told to %q", told)
	}

	read := func(recipient string, at time.Time) []Notification {
		t.Helper()
		var told []Notification
		wide(t, pool, func(ctx context.Context, w *Wide) error {
			var err error
			told, err = w.NotificationsOf(ctx, recipient, at)
			return err
		})
		return told
	}
	dave := read("dave", later)
	if len(dave) != 2 || dave[0].Namespace != "team-ops" || dave[1].Namespace != "finance" {
		t.Fatalf("dave is told %+v, newest first", dave)
	}
	if g := dave[1].Grant; dave[1].Kind != AdminAccessWidened || g == nil || g.ID != "01JQ4W" || g.Principal != "carol" ||
		g.Scope != finance || g.Role != access.Editor || !g.GrantedAt.Equal(now) || !dave[1].At.Equal(now) {
		t.Errorf("dave is told of the grant in finance as %+v, %+v", dave[1], dave[1].Grant)
	}
	if told := read("carol", later); len(told) != 0 {
		t.Errorf("carol is told of her own grant: %+v", told)
	}

	// Dismissed by its reader alone, once; and past 90 days neither answered nor kept.
	dismiss := func(recipient, id string, at time.Time) error {
		return pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
			return w.DismissNotification(ctx, recipient, id, at)
		})
	}
	if err := dismiss("erin", dave[0].ID, later); !errors.Is(err, ErrNoNotification) {
		t.Errorf("erin dismissing dave's notification was answered %v", err)
	}
	if err := dismiss("dave", dave[0].ID, later); err != nil {
		t.Fatal(err)
	}
	if err := dismiss("dave", dave[0].ID, later); !errors.Is(err, ErrNoNotification) {
		t.Errorf("a notification dismissed twice was answered %v", err)
	}
	if told := read("erin", later); len(told) != 2 {
		t.Errorf("dave's dismissal took erin's with it: she is told %+v", told)
	}
	past := now.Add(NotificationKept)
	if err := dismiss("erin", read("erin", later)[1].ID, past); !errors.Is(err, ErrNoNotification) {
		t.Errorf("a notification past its 90 days was dismissed, answered %v", err)
	}
	if told := read("erin", past); len(told) != 1 || told[0].Namespace != "team-ops" {
		t.Errorf("90 days after the grant in finance, erin is told %+v", told)
	}
	var left int
	if err := pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
		return w.tx.QueryRow(ctx, `select count(*) from notifications where recipient = 'erin'`).Scan(&left)
	}); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Errorf("erin's notification past its 90 days is kept: %d rows", left)
	}

	// A passkey's refusal is read the same way, with the passkey and nothing of a namespace; and
	// nobody is told as a group.
	if err := pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
		_, err := w.tx.Exec(ctx, `insert into notifications (id, recipient, kind, at, credential)
			values ('01JQ4Y', 'gina', 'passkey_counter_refused', $1, 'aVBob25lUGFzc2tleQ')`, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if told := read("gina", later); len(told) != 1 || told[0].Kind != PasskeyCounterRefused || told[0].Credential != "aVBob25lUGFzc2tleQ" ||
		told[0].Grant != nil || told[0].Namespace != "" {
		t.Errorf("gina is told %+v", told)
	}
	for what, stmt := range map[string]string{
		"a group": `insert into notifications (id, recipient, kind, at, credential)
			values ('01JQ4Z', 'group:leads', 'passkey_counter_refused', now(), 'aVBob25lUGFzc2tleQ')`,
		"a passkey's refusal naming a namespace": `insert into notifications (id, recipient, kind, at, namespace, credential)
			values ('01JQ4Z', 'gina', 'passkey_counter_refused', now(), 'finance', 'aVBob25lUGFzc2tleQ')`,
		"a widening with no grant": `insert into notifications (id, recipient, kind, at, namespace)
			values ('01JQ4Z', 'gina', 'admin_access_widened', now(), 'finance')`,
	} {
		err := pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
			_, err := w.tx.Exec(ctx, stmt)
			return err
		})
		if err == nil {
			t.Errorf("a notification to %s was written", what)
		}
	}

	// Removing a namespace removes what anybody was told about it.
	if err := pool.Installation(ctx, NamespaceAdministration, func(ctx context.Context, w *Wide) error {
		_, err := w.tx.Exec(ctx, `delete from namespaces where name = 'team-ops'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if told := read("erin", later); len(told) != 0 {
		t.Errorf("erin is still told of a namespace removed: %+v", told)
	}
}
