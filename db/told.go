package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/jackc/pgx/v5"
)

// What the installation tells one principal of its own accord, listed in their GET /api/v1/me: the
// wire's $defs/notification. Not the events notifications.go keeps, which are about runs and become
// push messages; these are about who may read what, and are read where the principal asks who it is.
//
// "An administrator holds no implicit run:read_data. Reading another namespace's payloads means
// granting themselves access first, which is audited and notifies the namespace's owner: a
// notification in the owner's GET /api/v1/me, because an owner does not read the audit log, and the
// people whose data it is should hear of it from the installation itself." The second kind, a
// sign-in refused for a passkey's signature counter, is written by the passkey ceremonies, and read
// here as the first is.

// The kinds of notification, as the wire's $defs/notification names them.
const (
	// AdminAccessWidened is an administrator having written a grant for themselves, or for a
	// group they belong to, in a namespace, told to the namespace's owners.
	AdminAccessWidened = "admin_access_widened"

	// PasskeyCounterRefused is a sign-in refused because a passkey's signature counter did not
	// move forward, told to the passkey's user.
	PasskeyCounterRefused = "passkey_counter_refused"
)

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

	// Namespace and Grant are where an administrator widened their own access, and the grant as
	// it was written, on AdminAccessWidened alone. The grant is kept whole rather than referred
	// to, since it may be revoked before its reader comes to read it, and what they are told is
	// what was done.
	Namespace string
	Grant     *access.Grant

	// Credential is the passkey whose assertion was refused, by its credential ID, on
	// PasskeyCounterRefused alone.
	Credential string
}

// TellOwners writes AdminAccessWidened, about the grant g an administrator, actor, wrote for
// themselves in this namespace, to each of the namespace's owners but actor, and answers who was
// told, by name.
//
// "The owner told is the principal the namespace's record names. A namespace from before v0.3.0
// names none: it becomes shared ... and every principal holding the owner role on it is told
// instead", by a grant on the namespace, not expired at at, a deny beside it leaving the role held.
// A group among them is told as each of its members, since a group reads nothing: one row for each,
// so that one member dismissing it dismisses it for nobody else. A service account holding the role
// is told as itself, since its token reads GET /api/v1/me as a user's does. actor is left out, since
// telling somebody what they have just done tells them nothing.
//
// It is written in the transaction that writes the grant, so that no administrator's grant commits
// untold, and before the audit entry that records it, which is the last statement of the
// transaction.
func (n *NS) TellOwners(ctx context.Context, g access.Grant, actor string, at time.Time) ([]string, error) {
	grant, err := json.Marshal(g)
	if err != nil {
		return nil, fmt.Errorf("db: grant %s could not be written into a notification: %w", g.ID, err)
	}
	rows, err := n.tx.Query(ctx, `
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
		)
		select principal from told where principal <> $3 order by principal`,
		n.namespace, at, actor)
	if err != nil {
		return nil, fmt.Errorf("db: the owners of %s could not be read: %w", n.namespace, err)
	}
	told, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("db: the owners of %s could not be read: %w", n.namespace, err)
	}
	for _, recipient := range told {
		if _, err := n.tx.Exec(ctx,
			`insert into notifications (id, recipient, kind, at, namespace, access_grant)
			 values ($1, $2, $3, $4, $5, $6)`,
			ulid.New(), recipient, AdminAccessWidened, at, n.namespace, grant); err != nil {
			return nil, fmt.Errorf("db: %s could not be told of grant %s: %w", recipient, g.ID, err)
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
		select id, recipient, kind, at, coalesce(namespace, ''), access_grant, coalesce(credential, '')
		  from notifications where recipient = $1
		 order by at desc, id desc`, recipient)
	if err != nil {
		return nil, fmt.Errorf("db: the notifications of %s could not be read: %w", recipient, err)
	}
	told, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Notification, error) {
		var t Notification
		var grant []byte
		if err := row.Scan(&t.ID, &t.Recipient, &t.Kind, &t.At, &t.Namespace, &grant, &t.Credential); err != nil {
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
