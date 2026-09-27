package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/jackc/pgx/v5"
)

// What the installation tells one principal of its own accord, listed in their GET /api/v1/me: the
// wire's $defs/notification. Not the events notifications.go keeps, which are about runs and become
// push messages; these are about who may read what, and are read where the principal asks who it
// is.
//
// "An administrator holds no implicit run:read_data. Reading another namespace's payloads means
// granting themselves access first, which is audited and notifies the namespace's owner: a
// notification in the owner's GET /api/v1/me, because an owner does not read the audit log, and the
// people whose data it is should hear of it from the installation itself." So are a grant an
// administrator writes there by the installation's power and a user an administrator puts in a group
// holding a role there, each told with the act and who did it. The second kind, a sign-in refused for
// a passkey's signature counter, is written by the passkey ceremonies, and read here as the first is. The third, the break-glass path used, is told to every administrator, so
// that the one way to a recovery code that no administrator vouches for is never taken silently.

// The kinds of notification, as the wire's $defs/notification names them.
const (
	// AdminAccessWidened is an administrator having widened access in a namespace: written a
	// grant there by the installation's power, put somebody in a group holding a role there, or
	// widened their own access there, giving a role to it or taking a deny from it. Told to the
	// namespace's owners, with the act and who did it.
	AdminAccessWidened = "admin_access_widened"

	// PasskeyCounterRefused is a sign-in refused because a passkey's signature counter did not
	// move forward, told to the passkey's user.
	PasskeyCounterRefused = "passkey_counter_refused"

	// BreakGlassRecovery is a recovery code issued to an administrator by the break-glass path,
	// agentiik-api recover, told to every administrator, the one recovered included.
	BreakGlassRecovery = "break_glass_recovery"
)

// The acts an AdminAccessWidened notification tells, as the wire's $defs/notification names them.
const (
	// ActGranted is a grant or a deny an administrator wrote by the installation's power, or a
	// role they gave their own access.
	ActGranted = "granted"
	// ActDenyLifted is a deny an administrator revoked from their own access.
	ActDenyLifted = "deny_lifted"
	// ActJoinedGroup is a user an administrator put in a group holding a role in the namespace,
	// themselves or somebody else.
	ActJoinedGroup = "joined_group"
	// ActLeftGroup is an administrator taking themselves out of a group whose deny applied in the
	// namespace.
	ActLeftGroup = "left_group"
	// ActGroupRemoved is an administrator removing a group they were in, whose deny applied in the
	// namespace.
	ActGroupRemoved = "group_removed"
)

// Widening is what an administrator did in a namespace that its owners are told of.
type Widening struct {
	// Grant is the grant written, the deny revoked, or the group's grant or deny a membership
	// brought or took away, kept whole rather than referred to, since it may be revoked before its
	// reader comes to read it, and what they are told is what was done.
	Grant access.Grant

	// Act is what was done, one of the Act constants, and By who did it, left out of those told,
	// since telling somebody what they have just done tells them nothing.
	Act string
	By  string

	// Member is the user put in the group, on ActJoinedGroup alone.
	Member string

	// At is when it was done, from which the 90 days it is kept are counted.
	At time.Time
}

// check refuses a widening the table would refuse, before any row is written for it.
func (what Widening) check() error {
	switch {
	case what.By == "":
		return errors.New("db: a widening nobody did")
	case !slices.Contains([]string{ActGranted, ActDenyLifted, ActJoinedGroup, ActLeftGroup, ActGroupRemoved}, what.Act):
		return fmt.Errorf("db: a widening by %q, and it is granted, deny_lifted, joined_group, left_group or group_removed", what.Act)
	case (what.Member != "") != (what.Act == ActJoinedGroup):
		return fmt.Errorf("db: a widening by %s names the member put in a group on joined_group, and on joined_group alone", what.Act)
	}
	return nil
}

// NotificationKept is how long a notification is kept from when it was written: "long enough to
// reach someone back from leave, and bounded so that GET /api/v1/me does not grow for ever".
const NotificationKept = 90 * 24 * time.Hour

// ErrNoNotification is no notification of that identifier kept for that principal: dismissed
// already, past its 90 days, or never theirs.
var ErrNoNotification = errors.New("db: no notification of that identifier for that principal")

// Notification is one thing told to one principal.
type Notification struct {
	ID        string
	Recipient string
	Kind      string

	// At is when it happened, from which the 90 days it is kept are counted.
	At time.Time

	// Namespace, Grant, Act and By are where an administrator widened access, the grant as
	// Widening keeps it, what they did and who they are, on AdminAccessWidened alone.
	Namespace string
	Grant     *access.Grant
	Act       string
	By        string

	// Credential is the passkey whose assertion was refused, by its credential ID, on
	// PasskeyCounterRefused alone.
	Credential string

	// Login is the administrator the break-glass path issued a recovery code, on
	// BreakGlassRecovery, and the user put in a group, on AdminAccessWidened by ActJoinedGroup,
	// named rather than referred to, so that removing the account leaves what the others were told.
	Login string
}

// TellOwners writes AdminAccessWidened, about what an administrator did in this namespace, to each
// of the namespace's owners but the one who did it, and answers who was told, by name.
//
// "The owner told is the principal the namespace's record names. A namespace from before v0.3.0
// names none: it becomes shared ... and every principal holding the owner role on it is told
// instead", by a grant on the namespace, not expired at what.At, a deny beside it leaving the role
// held. A group among them is told as each of its members, since a group reads nothing: one row for
// each, so that one member dismissing it dismisses it for nobody else. A service account holding
// the role is told as itself, since its token reads GET /api/v1/me as a user's does. Where that
// reaches nobody, no owner on the record and nobody holding the role, or only a group with no
// members, every administrator is told instead, suspended ones included, since each may be the one
// who comes back to read it: an administrator's act is never told to nobody but because it was the
// only owner's own.
//
// It is written in the transaction of the act, so that no act commits untold, and before the audit
// entry that records it, which is the last statement of the transaction.
func (n *NS) TellOwners(ctx context.Context, what Widening) ([]string, error) {
	return tellOwners(ctx, n.tx, n.namespace, what)
}

// TellOwners writes what NS.TellOwners does, in the namespace the grant's scope names, for a grant
// the installation's handle writes, as Wide.GrantAccess does.
func (w *Wide) TellOwners(ctx context.Context, what Widening) ([]string, error) {
	return tellOwners(ctx, w.tx, what.Grant.Scope.Namespace, what)
}

func tellOwners(ctx context.Context, tx pgx.Tx, namespace string, what Widening) ([]string, error) {
	if err := what.check(); err != nil {
		return nil, err
	}
	g := what.Grant
	grant, err := json.Marshal(g)
	if err != nil {
		return nil, fmt.Errorf("db: grant %s could not be written into a notification: %w", g.ID, err)
	}
	// The namespace is held first, for key share, as the row every notification about it refers to,
	// so that a removal of the namespace, which holds it for update and then takes the rows naming
	// it, the grant this act is about among them, waits for this act rather than for a row this act
	// took before; or has ended before it, when nobody is told of a namespace no longer there.
	held, err := tx.Query(ctx, `select name from namespaces where name = $1 for key share`, namespace)
	if err != nil {
		return nil, fmt.Errorf("db: namespace %s could not be held: %w", namespace, err)
	}
	names, err := pgx.CollectRows(held, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("db: namespace %s could not be held: %w", namespace, err)
	}
	if len(names) == 0 {
		return []string{}, nil
	}
	rows, err := tx.Query(ctx, `
		with record as (select owner from namespaces where name = $1),
		owners as (
		  select owner as principal from record where owner is not null
		  union
		  select principal from grants
		   where namespace = $1 and workflow is null and role = 'owner'
		     and (expires_at is null or expires_at > $2)
		     and not exists (select from record where owner is not null)
		),
		told as (
		  select principal from owners where principal not like 'group:%'
		  union
		  select m.login from owners o
		    join groups g on g.principal = o.principal
		    join group_members m on m.group_name = g.name
		),
		-- Nobody to tell, and the administrators are told instead; the one who acted among the
		-- owners is somebody, and leaves nobody else to tell.
		readers as (
		  select principal from told
		  union
		  select login from users where admin and not exists (select from told)
		)
		-- Each held for key share as the principal its notification refers to, in one order, so
		-- that one removed since the owners were read, whose removal this waits for, is told
		-- nothing rather than failing the act.
		select p.id from principals p join readers r on r.principal = p.id
		 where p.id <> $3 order by p.id for key share of p`,
		namespace, what.At, what.By)
	if err != nil {
		return nil, fmt.Errorf("db: the owners of %s could not be read: %w", namespace, err)
	}
	told, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("db: the owners of %s could not be read: %w", namespace, err)
	}
	for _, recipient := range told {
		if _, err := tx.Exec(ctx,
			`insert into notifications (id, recipient, kind, at, namespace, access_grant, act, acted_by, login)
			 values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			ulid.New(), recipient, AdminAccessWidened, what.At, namespace, grant, what.Act, what.By, nilIfEmpty(what.Member)); err != nil {
			return nil, fmt.Errorf("db: %s could not be told of grant %s: %w", recipient, g.ID, err)
		}
	}
	return told, nil
}

// TellAdministrators writes BreakGlassRecovery, about the recovery code the break-glass path issued
// the administrator login at at, to every administrator, login among them, and answers who was
// told, by login. Every one, suspended or holding no credential included, since each may be the one
// who comes back to read it, and the one recovered first of all: if they did not ask for it, whoever
// holds the host did.
//
// It is written in the transaction that issues the code, so that no break-glass code commits untold,
// after the code's row and before the audit entry: the code's row takes the recovered user's, and
// each notification its recipient's principal, which is the order removing an administrator takes
// them in, every administrator's user row before the principal it deletes.
func (w *Wide) TellAdministrators(ctx context.Context, login string, at time.Time) ([]string, error) {
	rows, err := w.tx.Query(ctx, `select login from users where admin order by login`)
	if err != nil {
		return nil, fmt.Errorf("db: the administrators could not be read: %w", err)
	}
	told, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("db: the administrators could not be read: %w", err)
	}
	for _, recipient := range told {
		if _, err := w.tx.Exec(ctx,
			`insert into notifications (id, recipient, kind, at, login) values ($1, $2, $3, $4, $5)`,
			ulid.New(), recipient, BreakGlassRecovery, at, login); err != nil {
			return nil, fmt.Errorf("db: %s could not be told of the recovery of %s: %w", recipient, login, err)
		}
	}
	return told, nil
}

// NotificationsOf answers what recipient is told as of now, newest first, and removes what it was
// told more than NotificationKept before now, which is kept no longer. Removed where it is read
// rather than by a sweep of its own: a notification nobody reads costs a row until its reader asks,
// or is removed with them, and one past its days is never answered either way.
func (w *Wide) NotificationsOf(ctx context.Context, recipient string, now time.Time) ([]Notification, error) {
	kept := now.Add(-NotificationKept)
	if _, err := w.tx.Exec(ctx, `delete from notifications where recipient = $1 and at <= $2`, recipient, kept); err != nil {
		return nil, fmt.Errorf("db: the notifications of %s past their days could not be removed: %w", recipient, err)
	}
	rows, err := w.tx.Query(ctx, `
		select id, recipient, kind, at, coalesce(namespace, ''), access_grant, coalesce(act, ''),
		       coalesce(acted_by, ''), coalesce(credential, ''), coalesce(login, '')
		  from notifications where recipient = $1
		 order by at desc, id desc`, recipient)
	if err != nil {
		return nil, fmt.Errorf("db: the notifications of %s could not be read: %w", recipient, err)
	}
	told, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Notification, error) {
		var t Notification
		var grant []byte
		if err := row.Scan(&t.ID, &t.Recipient, &t.Kind, &t.At, &t.Namespace, &grant, &t.Act, &t.By, &t.Credential, &t.Login); err != nil {
			return Notification{}, err
		}
		if grant != nil {
			t.Grant = new(access.Grant)
			if err := json.Unmarshal(grant, t.Grant); err != nil {
				return Notification{}, fmt.Errorf("notification %s holds a grant that does not read: %w", t.ID, err)
			}
		}
		return t, nil
	})
	if err != nil {
		return nil, fmt.Errorf("db: the notifications of %s could not be read: %w", recipient, err)
	}
	return told, nil
}

// DismissNotification removes one of recipient's notifications, which it lists no more, and is
// ErrNoNotification where recipient is told nothing of that identifier as of now.
func (w *Wide) DismissNotification(ctx context.Context, recipient, id string, now time.Time) error {
	tag, err := w.tx.Exec(ctx,
		`delete from notifications where recipient = $1 and id = $2 and at > $3`,
		recipient, id, now.Add(-NotificationKept))
	if err != nil {
		return fmt.Errorf("db: notification %s could not be dismissed: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoNotification
	}
	return nil
}

// TellOwnersIn writes AdminAccessWidened in namespace, as NS.TellOwners writes it in its handle's,
// from a transaction across the installation: a group's membership reaches every namespace its
// grants and denies are in, and an administrator putting somebody in a group widens access in each
// of those where it holds a role, as taking themselves out of one, or removing one they are in,
// widens their own in each of those where it holds a deny.
func (w *Wide) TellOwnersIn(ctx context.Context, namespace string, what Widening) ([]string, error) {
	return (&NS{tx: w.tx, namespace: namespace}).TellOwners(ctx, what)
}
