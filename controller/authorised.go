package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/db"
)

// Letting in only a run its principal may still start.
//
// "Authorisation is re-evaluated when a run is created, because a trigger armed months ago can fire
// long after the grant that armed it. From v0.3.0, a run whose principal no longer holds
// workflow:run at that moment ends cancelled before any task, with a reason naming the grant that
// lapsed." The API asks at the request, of a run somebody asked for. This asks again, of every run,
// on each pass before it is let in, whatever created it: a schedule, a webhook, an event and a
// workflow: step create runs with no request to be asked at, and a run waiting on its concurrency
// group is let in long after the request that created it was answered.
//
// Before the concurrency group, too, and not only on the pass that admits: a run with
// cancel_in_progress takes its group by cancelling the run holding it, and a run nobody may start
// would otherwise have ended somebody else's before being refused itself.
//
// The question is package access's, over the grants the database holds, as the API's is. Two
// answers to one question drift unless they are one function, and this package cannot import the
// API, which is an HTTP server.

// installationActor is who the audit log records as ending a run its principal may not start: the
// installation itself, whose acts are "attributed to no principal a grant names", since the
// controller acts with the installation's settings and holds no credential of anybody's.
const installationActor = "installation"

// refusal says why the principal e is attributed to may not start it now, in a sentence naming what
// lapsed, and nothing where it may.
//
// The principal is read as it stands, as the API reads it at a request: a user removed since, or
// suspended, holds nothing whatever their grants, and a service account removed since took its
// grants with it. The bootstrap operator holds what an owner holds in every namespace while the
// bootstrap token lasts, which is how a run the v0.2 operator left queued at the upgrade still runs,
// and nothing once it has ended, as the API answers it then.
func (co *Core) refusal(ctx context.Context, e db.Evaluation) (string, error) {
	at := access.Scope{Namespace: e.Namespace, Workflow: e.Workflow}
	now := co.now().UTC()
	who := e.TriggeredBy
	lacks := fmt.Sprintf("%s no longer holds %s on %s", who, access.WorkflowRun, at)
	var reason string
	err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		switch who {
		case "":
			reason = fmt.Sprintf("the run is attributed to nobody, and nobody holds %s on %s for it", access.WorkflowRun, at)
			return nil
		case access.BootstrapOperator:
			bootstrap, err := w.Bootstrap(ctx)
			if err != nil {
				return err
			}
			// While it lasts it holds what an owner holds, workflow:run among it.
			if bootstrap.Ended() {
				reason = fmt.Sprintf("%s: the bootstrap token ended at %s, when the first administrator enrolled a passkey", lacks, instant(bootstrap.EnrolledAt))
			}
			return nil
		}
		a, err := w.Attribution(ctx, e.Namespace, e.Workflow, who)
		if err != nil {
			return err
		}
		switch {
		case a.Kind == "" && strings.Contains(who, "/"):
			reason = fmt.Sprintf("%s: the service account %s was removed", lacks, who)
			return nil
		case a.Kind == "":
			reason = fmt.Sprintf("%s: the user %s was removed", lacks, who)
			return nil
		case a.Suspended:
			reason = fmt.Sprintf("%s: %s is suspended", lacks, who)
			return nil
		}
		held, err := access.Holds(a.Principal, a.Grants, access.WorkflowRun, at, now)
		if err != nil || held {
			return err
		}
		// A run somebody asked for was authorised when they asked, which is when it was created, so
		// what took the permission away ended since; one nobody asked for may have been armed
		// long before the grant that armed it ended. The API asks a moment before the run's row is
		// written, and a grant that ended in that moment is not named: the reason then says no
		// grant gives it, which is true, rather than naming a grant that might not be the one.
		var since time.Time
		if !e.Trigger.Unattended() {
			since = e.CreatedAt
		}
		reason, err = lapsed(ctx, w, a, lacks, at, now, since)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("controller: whether %s may still start run %s could not be read: %w", who, e.Run, err)
	}
	return reason, nil
}

// lapsed names what took workflow:run away from a principal still there to hold it: a deny that
// applies now, first, since it would take the permission away whatever gave it; then the grant that
// gave it and ended last, by its expiry or revoked, after since; and where none did, that none gives
// it.
//
// A grant that ended before since is not what the run is refused for: the run was authorised after
// it ended, through something else, a group its principal has left since for instance, which no
// grant's end records.
//
// A revocation is named by when and not by whom. The reason is read by whoever may read the run,
// and who took a grant back is the audit log's to say, to those who read it.
func lapsed(ctx context.Context, w *db.Wide, a db.Attribution, lacks string, at access.Scope, now, since time.Time) (string, error) {
	p := a.Principal
	for _, g := range a.Grants {
		if g.Takes(p, access.WorkflowRun, at) && !g.Expired(now) {
			return fmt.Sprintf("%s: grant %s denies %s %s on %s", lacks, g.ID, g.Principal, access.WorkflowRun, whereOf(g)), nil
		}
	}

	// The grant that ended last is the one whose end the principal is refused for. A grant
	// revoked after it expired had ended by its expiry, and is named for that.
	var last string
	ended := since
	for _, g := range a.Grants {
		if g.Expired(now) && g.Gives(p, access.WorkflowRun, at) && g.ExpiresAt.After(ended) {
			ended = *g.ExpiresAt
			last = fmt.Sprintf("%s: grant %s (%s) expired at %s", lacks, g.ID, grantOf(g, p), instant(ended))
		}
	}
	revoked, err := w.Revocations(ctx, at.Namespace, at.Workflow, p)
	if err != nil {
		return "", err
	}
	for _, r := range revoked {
		g := r.Grant
		switch {
		case !g.Gives(p, access.WorkflowRun, at):
		case g.Expired(r.At) && g.ExpiresAt.After(ended):
			ended = *g.ExpiresAt
			last = fmt.Sprintf("%s: grant %s (%s) expired at %s", lacks, g.ID, grantOf(g, p), instant(ended))
		case !g.Expired(r.At) && r.At.After(ended):
			ended = r.At
			last = fmt.Sprintf("%s: grant %s (%s) was revoked at %s", lacks, g.ID, grantOf(g, p), instant(ended))
		}
	}
	if last != "" {
		return last, nil
	}

	nothing := fmt.Sprintf("%s does not hold %s on %s: no grant gives it there", p.Ref, access.WorkflowRun, at)
	if p.Ref == at.Namespace+"/"+db.BuiltIn {
		nothing += ", and a namespace's built-in identity holds none until an owner gives it one"
	}
	return nothing, nil
}

// grantOf is a grant as a reason names it, beside its identifier: its role and where, and whom
// where it is not the principal's own.
func grantOf(g access.Grant, p access.Principal) string {
	if g.Principal == p.Ref {
		return fmt.Sprintf("%s on %s", g.Role, whereOf(g))
	}
	return fmt.Sprintf("%s to %s on %s", g.Role, g.Principal, whereOf(g))
}

// whereOf is where a grant is written, in words.
func whereOf(g access.Grant) string {
	if g.Scope.Workflow == "" {
		return "the namespace " + g.Scope.Namespace
	}
	return "the workflow " + g.Scope.String()
}

// instant writes a moment as the API writes one, in UTC.
func instant(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
