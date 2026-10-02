package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// What a console shows says that it changed, for GET /api/v1/me/live: the triggers of migration
// 0067 say it on LiveChannel, in the transaction that changed it, and WatchLive hears it.

// LiveChannel is the channel a run, a step, a task, a notification, a runner or a pool says it
// changed on.
//
// A payload names what changed and nothing of it, "a notification carries only an identifier", as
// on the run channel and the log channel: what it names is read through the route that answers it,
// under that route's permissions. It is a latency optimisation in the same way: a change said while
// nothing listened is one a console reads when it connects again, since it reads everything it
// shows then.
const LiveChannel = "agentiik_live"

// ChangeKind is what kind of thing a LiveChange says changed.
type ChangeKind string

const (
	// ChangedRun is a run created or moved on, or one of its steps or tasks: Namespace, Workflow
	// and Run name it.
	ChangedRun ChangeKind = "run"

	// ChangedNotifications is a notification told to Recipient, or dismissed.
	ChangedNotifications ChangeKind = "notifications"

	// ChangedRunners is a runner or a pool changed, a heartbeat included.
	ChangedRunners ChangeKind = "runners"

	// ChangedAll is everything, said where a change may have been missed: when WatchLive starts
	// listening, since what changed before was said to nobody, and for a payload it cannot read.
	ChangedAll ChangeKind = "all"
)

// LiveChange is one notification on LiveChannel, read.
type LiveChange struct {
	Kind      ChangeKind
	Namespace string
	Workflow  string
	Run       string
	Recipient string
}

// ParseLiveChange reads a payload the triggers of migration 0067 sent.
func ParseLiveChange(payload string) (LiveChange, error) {
	f := strings.Fields(payload)
	switch {
	case len(f) == 4 && f[0] == string(ChangedRun):
		return LiveChange{Kind: ChangedRun, Namespace: f[1], Workflow: f[2], Run: f[3]}, nil
	case len(f) == 2 && f[0] == string(ChangedNotifications):
		return LiveChange{Kind: ChangedNotifications, Recipient: f[1]}, nil
	case len(f) == 1 && f[0] == string(ChangedRunners):
		return LiveChange{Kind: ChangedRunners}, nil
	}
	return LiveChange{}, fmt.Errorf("db: %.100q is no change the live channel says", payload)
}

// WatchLive calls on with every change said on LiveChannel, and with ChangedAll once listening. It
// blocks until ctx is done, or the connection it listens on fails, and answers why.
//
// It takes a session of its own, since LISTEN is a property of a connection. A payload it cannot
// read is said as ChangedAll rather than dropped, since what it meant is then read again whatever
// it was.
func (p *Pool) WatchLive(ctx context.Context, on func(LiveChange)) error {
	if on == nil {
		return errors.New("db: WatchLive needs something to call")
	}
	return p.Session(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		if _, err := conn.Exec(ctx, `listen `+LiveChannel); err != nil {
			return fmt.Errorf("db: the live channel could not be listened to: %w", err)
		}
		defer func() {
			unlisten, stop := context.WithTimeout(context.WithoutCancel(ctx), unlistenWithin)
			defer stop()
			conn.Exec(unlisten, `unlisten `+LiveChannel)
		}()
		on(LiveChange{Kind: ChangedAll})
		for {
			note, err := conn.Conn().WaitForNotification(ctx)
			switch {
			case err == nil:
				change, err := ParseLiveChange(note.Payload)
				if err != nil {
					change = LiveChange{Kind: ChangedAll}
				}
				on(change)
			case ctx.Err() != nil:
				return ctx.Err()
			default:
				return fmt.Errorf("db: the connection listening for changes failed: %w", err)
			}
		}
	})
}
