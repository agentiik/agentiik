package db

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// What the installation tells a principal: an administrator's grant to themselves, told to the
// owners of the namespace it was written in, with the act and who did it, and read and dismissed by
// each of them.

func TestAnAdministratorsGrantIsToldToTheOwnersOfItsNamespace(t *testing.T) {
	pool := identity(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, u := range []User{
			{Login: "alice", Profile: Profile{GivenName: "Alice"}}, {Login: "carol", Profile: Profile{GivenName: "Carol"}, Admin: true}, {Login: "dave", Profile: Profile{GivenName: "Dave"}},
			{Login: "erin", Profile: Profile{GivenName: "Erin"}}, {Login: "frank", Profile: Profile{GivenName: "Frank"}}, {Login: "gina", Profile: Profile{GivenName: "Gina"}},
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
			told, err = n.TellOwners(ctx, Widening{Grant: g, Act: ActGranted, By: "carol", At: at})
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
		g.Scope != finance || g.Role != access.Editor || !g.GrantedAt.Equal(now) || !dave[1].At.Equal(now) ||
		dave[1].Act != ActGranted || dave[1].By != "carol" || dave[1].Login != "" {
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
		"a widening with no grant": `insert into notifications (id, recipient, kind, at, namespace, act, acted_by)
			values ('01JQ4Z', 'gina', 'admin_access_widened', now(), 'finance', 'granted', 'carol')`,
		"a widening by no act": `insert into notifications (id, recipient, kind, at, namespace, access_grant, acted_by)
			values ('01JQ4Z', 'gina', 'admin_access_widened', now(), 'finance', '{}', 'carol')`,
		"a widening by nobody": `insert into notifications (id, recipient, kind, at, namespace, access_grant, act)
			values ('01JQ4Z', 'gina', 'admin_access_widened', now(), 'finance', '{}', 'granted')`,
		"a widening by an act nobody tells": `insert into notifications (id, recipient, kind, at, namespace, access_grant, act, acted_by)
			values ('01JQ4Z', 'gina', 'admin_access_widened', now(), 'finance', '{}', 'shared', 'carol')`,
		"a group joined naming no member": `insert into notifications (id, recipient, kind, at, namespace, access_grant, act, acted_by)
			values ('01JQ4Z', 'gina', 'admin_access_widened', now(), 'finance', '{}', 'joined_group', 'carol')`,
		"a grant naming a member": `insert into notifications (id, recipient, kind, at, namespace, access_grant, act, acted_by, login)
			values ('01JQ4Z', 'gina', 'admin_access_widened', now(), 'finance', '{}', 'granted', 'carol', 'alice')`,
		"a passkey's refusal by somebody": `insert into notifications (id, recipient, kind, at, credential, act, acted_by)
			values ('01JQ4Z', 'gina', 'passkey_counter_refused', now(), 'aVBob25lUGFzc2tleQ', 'granted', 'carol')`,
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

// Where a namespace's record names no owner and nobody holds its owner role, or only a group with no
// members does, an administrator's act there is told to every other administrator, a suspended one
// included, since the act is told to nobody otherwise; so is the act that makes somebody its owner,
// the administrator themselves or anybody, as the namespace stood before it, and the owner it made
// is told too. Once the one who acted is the only owner, nobody is told. A membership's widening
// names the member put in.
func TestAnActInANamespaceNobodyOwnsIsToldToTheOtherAdministrators(t *testing.T) {
	pool := identity(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, u := range []User{
			{Login: "carol", Profile: Profile{GivenName: "Carol"}, Admin: true}, {Login: "dan", Profile: Profile{GivenName: "Dan"}, Admin: true, Suspended: true},
			{Login: "erin", Profile: Profile{GivenName: "Erin"}, Admin: true}, {Login: "gina", Profile: Profile{GivenName: "Gina"}},
		} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		return w.CreateGroup(ctx, "nobody")
	})
	finance, ops := access.Scope{Namespace: "finance"}, access.Scope{Namespace: "team-ops"}
	// team-ops's owner role is held by a group with no members, and gina edits finance.
	for _, g := range []access.Grant{
		{ID: "01JQ6A", Principal: "group:nobody", Scope: ops, Role: access.Owner, GrantedBy: "carol", GrantedAt: now},
		{ID: "01JQ6B", Principal: "gina", Scope: finance, Role: access.Editor, GrantedBy: "carol", GrantedAt: now},
	} {
		if err := pool.In(ctx, g.Scope.Namespace, func(ctx context.Context, n *NS) error { return n.GrantAccess(ctx, g) }); err != nil {
			t.Fatal(err)
		}
	}
	tell := func(what Widening) []string {
		t.Helper()
		var told []string
		wide(t, pool, func(ctx context.Context, w *Wide) error {
			var err error
			told, err = w.TellOwnersIn(ctx, what.Grant.Scope.Namespace, what)
			return err
		})
		return told
	}
	editor := access.Grant{ID: "01JQ6C", Principal: "carol", Scope: finance, Role: access.Editor, GrantedBy: "carol", GrantedAt: now}
	if told := tell(Widening{Grant: editor, Act: ActGranted, By: "carol", At: now}); !slices.Equal(told, []string{"dan", "erin"}) {
		t.Errorf("carol's grant in finance, which nobody owns, was told to %q", told)
	}
	joined := access.Grant{ID: "01JQ6D", Principal: "group:auditors", Scope: ops, Role: access.Viewer, GrantedBy: "erin", GrantedAt: now.Add(-time.Hour)}
	if told := tell(Widening{Grant: joined, Act: ActJoinedGroup, By: "carol", Member: "gina", At: now}); !slices.Equal(told, []string{"dan", "erin"}) {
		t.Errorf("gina put in a group holding a role in team-ops, whose owner is a group of nobody, was told to %q", told)
	}
	var dan []Notification
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		var err error
		dan, err = w.NotificationsOf(ctx, "dan", now)
		return err
	})
	if len(dan) != 2 || dan[0].Namespace != "team-ops" || dan[0].Act != ActJoinedGroup || dan[0].By != "carol" || dan[0].Login != "gina" ||
		dan[1].Namespace != "finance" || dan[1].Act != ActGranted || dan[1].By != "carol" || dan[1].Login != "" {
		t.Errorf("dan is told %+v", dan)
	}

	// carol joining the group of nobody that owns team-ops makes her its owner, which the others
	// hear of as the namespace stood before.
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		_, err := w.AddMember(ctx, "nobody", "carol")
		return err
	})
	owning := access.Grant{ID: "01JQ6A", Principal: "group:nobody", Scope: ops, Role: access.Owner, GrantedBy: "carol", GrantedAt: now}
	if told := tell(Widening{Grant: owning, Act: ActJoinedGroup, By: "carol", Member: "carol", At: now}); !slices.Equal(told, []string{"dan", "erin"}) {
		t.Errorf("carol joining the group that owns team-ops, of which it had no member, was told to %q", told)
	}
	// gina put in it next joins somebody who owned team-ops already, carol, and is told of it as
	// an owner now.
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		_, err := w.AddMember(ctx, "nobody", "gina")
		return err
	})
	if told := tell(Widening{Grant: owning, Act: ActJoinedGroup, By: "carol", Member: "gina", At: now}); !slices.Equal(told, []string{"gina"}) {
		t.Errorf("gina put in the group carol owns team-ops through was told to %q", told)
	}
	// carol giving herself the owner role on finance, which nobody owned, is told to the others;
	// once she is its one owner, what she does there is told to nobody; and her giving gina the
	// role is told to gina, as an owner now.
	owner := access.Grant{ID: "01JQ6E", Principal: "carol", Scope: finance, Role: access.Owner, GrantedBy: "carol", GrantedAt: now}
	if err := pool.In(ctx, "finance", func(ctx context.Context, n *NS) error { return n.GrantAccess(ctx, owner) }); err != nil {
		t.Fatal(err)
	}
	if told := tell(Widening{Grant: owner, Act: ActGranted, By: "carol", At: now}); !slices.Equal(told, []string{"dan", "erin"}) {
		t.Errorf("carol's owner role on finance, which nobody owned, was told to %q", told)
	}
	if told := tell(Widening{Grant: editor, Act: ActGranted, By: "carol", At: now}); len(told) != 0 {
		t.Errorf("carol's grant in finance, which she alone owns, was told to %q", told)
	}
	ginaOwns := access.Grant{ID: "01JQ6F", Principal: "gina", Scope: finance, Role: access.Owner, GrantedBy: "carol", GrantedAt: now}
	if err := pool.In(ctx, "finance", func(ctx context.Context, n *NS) error { return n.GrantAccess(ctx, ginaOwns) }); err != nil {
		t.Fatal(err)
	}
	if told := tell(Widening{Grant: ginaOwns, Act: ActGranted, By: "carol", At: now}); !slices.Equal(told, []string{"gina"}) {
		t.Errorf("carol giving gina the owner role on finance, which carol owns, was told to %q", told)
	}

	for what, refused := range map[string]Widening{
		"by nobody":                  {Grant: editor, Act: ActGranted, At: now},
		"by an act nobody tells":     {Grant: editor, Act: "shared", By: "carol", At: now},
		"joining naming no member":   {Grant: joined, Act: ActJoinedGroup, By: "carol", At: now},
		"a grant naming a member":    {Grant: editor, Act: ActGranted, By: "carol", Member: "gina", At: now},
		"a deny lifted with members": {Grant: editor, Act: ActDenyLifted, By: "carol", Member: "gina", At: now},
	} {
		err := pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
			_, err := w.TellOwners(ctx, refused)
			return err
		})
		if err == nil {
			t.Errorf("a widening %s was told", what)
		}
	}
}

// notificationActs is the migration that gives a notification its act and who did it.
const notificationActs = "0046_notification_acts.sql"

// A notification a build of v0.3.0 wrote before migration 0046 is kept where its row says who acted:
// a grant told at the instant it was written was written then, by its granted_by, and is told as
// granted by them. One told later, a deny lifted or a membership changed, names nobody who acted and
// is removed. The other kinds are left as they were. Migrated as a managed PostgreSQL's
// administrator, which owns the database and is no superuser, and so reads the notifications behind
// their row security only through the installation's scope.
func TestANotificationFromBeforeItsActIsKeptWhereItSaysWhoActed(t *testing.T) {
	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(all, func(m Migration) bool { return m.Name == notificationActs })
	if i < 1 {
		t.Fatalf("no migration %s after another", notificationActs)
	}
	super, role := blank(t)
	ctx := t.Context()
	url := os.Getenv("AGENTIIK_TEST_DATABASE_URL")
	admin := role[:min(len(role), maxIdentifier-len("_admin"))] + "_admin"
	// The administrator owns the database, so it is dropped once the database is: this runs
	// before the cleanups blank registered.
	t.Cleanup(func() {
		drop(ctx, url, `drop database if exists `+role+` with (force)`)
		drop(ctx, url, `drop role if exists `+admin)
	})
	conn := connect(t, super)
	for _, stmt := range []string{
		`drop role if exists ` + admin,
		`create role ` + admin + ` login createrole nosuperuser nobypassrls password 'test'`,
		`alter database ` + role + ` owner to ` + admin,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	as := connect(t, withCredentials(super, admin, "test"))
	if _, err := MigrateThrough(ctx, as, all[i-1].Name); err != nil {
		t.Fatalf("the database could not be migrated as far as %s as its administrator: %s", all[i-1].Name, err)
	}
	for _, stmt := range []string{
		`insert into namespaces (name) values ('finance')`,
		`insert into principals (id, kind) values ('dave', 'user')`,
		`insert into notifications (id, recipient, kind, at, namespace, access_grant) values
		   ('01JQ7A', 'dave', 'admin_access_widened', '2026-09-27T14:00:00.123456Z', 'finance',
		    '{"id":"01JQ70","principal":"carol","scope":"finance","role":"editor","granted_by":"carol","granted_at":"2026-09-27T14:00:00.123456Z"}'),
		   ('01JQ7B', 'dave', 'admin_access_widened', '2026-09-27T15:00:00Z', 'finance',
		    '{"id":"01JQ71","principal":"group:auditors","scope":"finance","deny":"run:read_data","granted_by":"frank","granted_at":"2026-09-27T14:00:00Z"}')`,
		`insert into notifications (id, recipient, kind, at, credential) values ('01JQ7C', 'dave', 'passkey_counter_refused', now(), 'aVBob25l')`,
		`insert into notifications (id, recipient, kind, at, login) values ('01JQ7D', 'dave', 'break_glass_recovery', now(), 'carol')`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("filling the database as a build before migration %s would have: %s", notificationActs, err)
		}
	}
	if _, err := MigrateThrough(ctx, as, ""); err != nil {
		t.Fatalf("the notifications a build before migration %s wrote were refused: %s", notificationActs, err)
	}
	rows, err := conn.Query(ctx, `select concat_ws(' ', id, kind, act, acted_by) from notifications order by id`)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kept, []string{
		"01JQ7A admin_access_widened granted carol", "01JQ7C passkey_counter_refused", "01JQ7D break_glass_recovery",
	}) {
		t.Errorf("after migration %s the notifications are %q", notificationActs, kept)
	}
}

// The break-glass path is told to every administrator, suspended or holding no credential, the one
// recovered among them, naming the account and when, and to nobody else; the account is named
// rather than referred to, so that removing it leaves what the others were told; and a notice of it
// carries the account and nothing of the other kinds.
func TestTheBreakGlassPathIsToldToEveryAdministrator(t *testing.T) {
	pool := identity(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, u := range []User{
			{Login: "alice", Profile: Profile{GivenName: "Alice"}}, {Login: "carol", Profile: Profile{GivenName: "Carol"}, Admin: true},
			{Login: "dan", Profile: Profile{GivenName: "Dan"}, Admin: true, Suspended: true}, {Login: "erin", Profile: Profile{GivenName: "Erin"}, Admin: true},
		} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		return nil
	})
	var told []string
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		var err error
		told, err = w.TellAdministrators(ctx, "dan", now)
		return err
	})
	if !slices.Equal(told, []string{"carol", "dan", "erin"}) {
		t.Errorf("the recovery of dan was told to %q", told)
	}
	read := func(recipient string) []Notification {
		t.Helper()
		var notes []Notification
		wide(t, pool, func(ctx context.Context, w *Wide) error {
			var err error
			notes, err = w.NotificationsOf(ctx, recipient, now.Add(time.Second))
			return err
		})
		return notes
	}
	for _, admin := range []string{"carol", "dan", "erin"} {
		if n := read(admin); len(n) != 1 || n[0].Kind != BreakGlassRecovery || n[0].Login != "dan" || !n[0].At.Equal(now) ||
			n[0].Namespace != "" || n[0].Grant != nil || n[0].Credential != "" {
			t.Errorf("%s is told %+v", admin, n)
		}
	}
	if n := read("alice"); len(n) != 0 {
		t.Errorf("alice, who administers nothing, is told %+v", n)
	}

	for what, refused := range map[string]struct{ stmt, by string }{
		"a recovery naming nobody": {`insert into notifications (id, recipient, kind, at)
			values ('01JQ5A', 'carol', 'break_glass_recovery', now())`, "notifications_one_kind"},
		"a recovery naming a namespace": {`insert into notifications (id, recipient, kind, at, namespace, login)
			values ('01JQ5A', 'carol', 'break_glass_recovery', now(), 'finance', 'dan')`, "notifications_one_kind"},
		"a recovery naming a passkey": {`insert into notifications (id, recipient, kind, at, credential, login)
			values ('01JQ5A', 'carol', 'break_glass_recovery', now(), 'aVBob25lUGFzc2tleQ', 'dan')`, "notifications_one_kind"},
		"a recovery naming no login a user can have": {`insert into notifications (id, recipient, kind, at, login)
			values ('01JQ5A', 'carol', 'break_glass_recovery', now(), 'Dan')`, "notifications_login_check"},
		"a passkey's refusal naming an account": {`insert into notifications (id, recipient, kind, at, credential, login)
			values ('01JQ5A', 'carol', 'passkey_counter_refused', now(), 'aVBob25lUGFzc2tleQ', 'dan')`, "notifications_one_kind"},
		"a widening naming an account": {`insert into notifications (id, recipient, kind, at, namespace, access_grant, act, acted_by, login)
			values ('01JQ5A', 'carol', 'admin_access_widened', now(), 'finance', '{}', 'deny_lifted', 'erin', 'dan')`, "notifications_one_kind"},
		"a recovery by somebody": {`insert into notifications (id, recipient, kind, at, login, act, acted_by)
			values ('01JQ5A', 'carol', 'break_glass_recovery', now(), 'dan', 'granted', 'erin')`, "notifications_one_kind"},
		"a kind nobody tells": {`insert into notifications (id, recipient, kind, at, login)
			values ('01JQ5A', 'carol', 'break_glass', now(), 'dan')`, "notifications_kind_check"},
	} {
		err := pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
			_, err := w.tx.Exec(ctx, refused.stmt)
			return err
		})
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.ConstraintName != refused.by {
			t.Errorf("%s was answered %v, and %s refuses it", what, err, refused.by)
		}
	}

	// dan is removed: what he was told goes with him, and what the others were told stays.
	wide(t, pool, func(ctx context.Context, w *Wide) error { return w.RemovePrincipal(ctx, "dan") })
	if n := read("carol"); len(n) != 1 || n[0].Login != "dan" {
		t.Errorf("once dan is removed, carol is told %+v", n)
	}
	var left int
	if err := pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
		return w.tx.QueryRow(ctx, `select count(*) from notifications where recipient = 'dan'`).Scan(&left)
	}); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d notifications of dan outlived him", left)
	}
}
