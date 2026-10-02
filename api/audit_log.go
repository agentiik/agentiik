package api

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
)

// auditPageSize is how many entries a page of the audit log holds where the query names no limit,
// a screenful, and auditPageMost the most it may name: few enough that a page is answered at once,
// however far back a narrowing reads for it.
const (
	auditPageSize = 50
	auditPageMost = 200
)

// auditAction is how an action is written, a thing and a verb: an action written otherwise is no
// action the log holds, and is refused rather than answered with nothing.
var auditAction = regexp.MustCompile(`^[a-z_]+\.[a-z_]+$`)

// auditPage is what GET /api/v1/auth/audit answers, as openapi.json describes it: the entries as the
// export writes them, and where the chain stands.
type auditPage struct {
	Entries  []audit.Entry `json:"entries"`
	Head     int64         `json:"head"`
	Verified int64         `json:"verified"`
}

// auditLog answers GET /api/v1/auth/audit, to administrators alone: the audit log, the newest
// entries first, narrowed as the query says, a page at a time. The log is every namespace's and the
// installation's at once, which is why no other principal reads it: an owner hears of what an
// administrator did in their namespace through a notification.
//
// Under auth, reserved since v0.2, rather than a word of its own, which would have to be reserved a
// release before the route is served so that a namespace already holding it keeps its routes; the
// log is the record of what each authority was used for. Reading it is not recorded in it, as no
// read is.
func (s *RunnerAPI) auditLog(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	q, err := readAuditQuery(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	out := auditPage{Entries: []audit.Entry{}}
	err = s.pool.Installation(r.Context(), db.AuditLog, func(ctx context.Context, wide *db.Wide) error {
		entries, head, verified, err := wide.AuditPage(ctx, q)
		if err != nil {
			return err
		}
		if entries != nil {
			out.Entries = entries
		}
		out.Head, out.Verified = head, verified
		return nil
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the audit log could not be read")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	write(w, http.StatusOK, out)
}

// readAuditQuery reads what a page of the audit log is narrowed to.
func readAuditQuery(r *http.Request) (db.AuditQuery, error) {
	query := r.URL.Query()
	q := db.AuditQuery{Limit: auditPageSize}
	if written := query.Get("limit"); written != "" {
		n, err := strconv.Atoi(written)
		if err != nil || n < 1 || n > auditPageMost {
			return db.AuditQuery{}, fmt.Errorf("limit is %q, and a page holds from 1 to %d entries", written, auditPageMost)
		}
		q.Limit = n
	}
	if written := query.Get("before"); written != "" {
		n, err := strconv.ParseInt(written, 10, 64)
		if err != nil || n < 1 {
			return db.AuditQuery{}, fmt.Errorf("before is %q, and it is the seq of an entry, a whole number from 1", written)
		}
		q.Before = n
	}
	q.Actor, q.Target = query.Get("actor"), query.Get("target")
	if q.Action = query.Get("action"); q.Action != "" && !auditAction.MatchString(q.Action) {
		return db.AuditQuery{}, fmt.Errorf("action is %q, and an action is a thing and a verb, such as run.cancel", q.Action)
	}
	// The acts on the installation name no namespace, which a name cannot match: - asks for them,
	// a character no namespace's name holds alone.
	if q.Namespace = query.Get("namespace"); q.Namespace == "-" {
		q.Namespace, q.Installation = "", true
	}
	for _, c := range []struct {
		name string
		into *time.Time
	}{{"since", &q.Since}, {"until", &q.Until}} {
		written := query.Get(c.name)
		if written == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, written)
		if err != nil {
			return db.AuditQuery{}, fmt.Errorf("%s is %q, which is not a time: one is written in RFC 3339, 2026-10-02T06:00:00Z", c.name, written)
		}
		*c.into = at.UTC()
	}
	if !q.Since.IsZero() && !q.Until.IsZero() && q.Since.After(q.Until) {
		return db.AuditQuery{}, fmt.Errorf("since is %s and until is %s, and a span runs from an instant to a later one", stamp(q.Since), stamp(q.Until))
	}
	return q, nil
}
