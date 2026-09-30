package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/agentiik/agentiik/db"
)

// What a workflow has armed, "served to a principal holding workflow:read": each schedule, webhook
// and event subscription its default branch's head declares, and what has become of it. A schedule
// says when it comes round next, the occurrence it is due for and the instant it fires, its jitter
// drawn; a webhook, where it is served and how many signatures it has refused; and each, when it
// last fired and the run it started, or why it started none.

// armedOut is one trigger armed, as the route answers it.
type armedOut struct {
	Kind     string          `json:"kind"`
	Position int             `json:"position"`
	Declared json.RawMessage `json:"declared"`

	// URL is where a webhook is served, below the installation's own address.
	URL string `json:"url,omitempty"`

	// Next is a schedule's next occurrence, the instant trigger.scheduled_for will read, and
	// FiresAt when it fires, its jitter drawn.
	Next    *time.Time `json:"next,omitempty"`
	FiresAt *time.Time `json:"fires_at,omitempty"`

	FiredAt  *time.Time `json:"fired_at,omitempty"`
	FiredFor *time.Time `json:"fired_for,omitempty"`
	Run      string     `json:"run,omitempty"`
	Skipped  string     `json:"skipped,omitempty"`

	// Failures are a webhook's refused signatures, counted since it was first armed at its path
	// and method, the last at FailedAt.
	Failures *int64     `json:"failures,omitempty"`
	FailedAt *time.Time `json:"failed_at,omitempty"`

	ArmedBy string    `json:"armed_by"`
	ArmedAt time.Time `json:"armed_at"`
}

// listTriggers answers what a workflow has armed, and the commit that declares it, which is the
// default branch's head once it is armed and empty while nothing is.
func (s *Server) listTriggers(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	var commit string
	var armed []db.Armed
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		if commit, err = ns.ArmedCommit(ctx, over.Workflow); err != nil {
			return err
		}
		armed, err = ns.ArmedBy(ctx, over.Workflow)
		return err
	})
	if errors.Is(err, db.ErrNoWorkflow) {
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	}
	if err != nil {
		s.report(err)
		fail(w, http.StatusInternalServerError, "the triggers could not be read")
		return
	}
	out := make([]armedOut, 0, len(armed))
	for _, a := range armed {
		o := armedOut{
			Kind: a.Kind.String(), Position: a.Position, Declared: a.Declared,
			FiredAt: when(a.FiredAt), FiredFor: when(a.FiredFor), Run: string(a.FiredRun), Skipped: a.Skipped,
			ArmedBy: a.ArmedBy, ArmedAt: a.ArmedAt.UTC(),
		}
		switch a.Kind.String() {
		case "schedule":
			o.Next, o.FiresAt = when(a.DueAt), when(a.FireAt)
		case "webhook":
			o.URL = s.publicURL + "/hooks/" + over.Namespace + a.Path
			o.Failures, o.FailedAt = &a.Failures, when(a.FailedAt)
		}
		out = append(out, o)
	}
	write(w, http.StatusOK, map[string]any{"commit": commit, "triggers": out})
}

// when is a time the route leaves out where it is zero, in UTC where it is not.
func when(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}
