package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/trigger"
)

// Replay is what POST /api/v1/runs/{id}/replay is asked with: the step the replay starts from, left
// out for a replay from the start.
type Replay struct {
	Step agk.Step `json:"step,omitempty"`
}

// replaying is a Replay as the API reads one.
type replaying struct{ step string }

func (r *replaying) field(b *body, name string) error {
	if name == "step" {
		return text(b, &r.step)
	}
	return unknown(name)
}

// replay starts a run replaying another from a step: "Replays from a named step, reusing
// everything upstream."
//
// A replay is a new run, since "a replay is a new run": of the commit the run it replays pinned,
// whatever its branch does next, over the inputs that run was started with as they were bound,
// attributed to whoever asks for it. The steps above the step named, the ones it reads from
// directly or through others, are reused, their verdicts and envelopes taken from the run it
// replays, and every other step runs. With no step it replays from the start and reuses nothing.
//
// Refused with 409 while the run is not over, since what its steps publish is not decided yet; and
// for a replay from a step, where the run is "replayable from the start only", an input of a step it
// could restart from having gone, or where its envelopes have gone with its retention. A step the
// run's version does not have is 422. Audited as run.trigger, the replay being a run started, with
// what it replays and from which step.
func (s *Server) replay(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	var ask replaying
	if err := readIfAny(r, &ask, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	of := agk.RunID(r.PathValue("run"))
	var d db.RunDetail
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		d, err = ns.RunDetail(ctx, of)
		return err
	})
	if errors.Is(err, db.ErrNoRun) {
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, "the run could not be read")
		return
	}
	if !d.State.Terminal() {
		fail(w, http.StatusConflict, fmt.Sprintf("run %s is %s, and a replay is of a run that is over: what its steps publish is not decided yet", of, d.State))
		return
	}
	g, err := s.versions.Graph(r.Context(), over.Namespace, d.Workflow, d.Commit)
	if errors.Is(err, db.ErrNoVersion) {
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, "the version could not be read")
		return
	}
	from := agk.Step(ask.step)
	if from != "" {
		if _, ok := g.Step(from); !ok {
			fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("%s@%s has no step %.200s to replay from", d.Workflow, d.Commit, ask.step))
			return
		}
		if d.ReplayFromStartOnly || d.EnvelopesPurged {
			fail(w, http.StatusConflict, fmt.Sprintf("run %s is replayable from the start only: an input of a step it could restart from has expired, and a replay from a step reuses what the steps above it produced", of))
			return
		}
		finished := map[agk.Step]bool{}
		for _, st := range d.Steps {
			finished[st.Step] = graph.Reusable(st.Verdict, d.State)
		}
		for _, up := range g.Upstream(from) {
			if !finished[up] {
				fail(w, http.StatusConflict, fmt.Sprintf("step %s, above %s, did not finish in run %s, so there is nothing of it to reuse: replay from the start", up, from, of))
				return
			}
		}
	}
	inputs, err := json.Marshal(orEmptyMap(d.Inputs))
	if err != nil {
		fail(w, http.StatusInternalServerError, "the run could not be read")
		return
	}

	// A replay is a manual run, "attributed to the caller", of the commit the run pinned and the
	// inputs it was started with as they were bound: the one path, with nothing left to resolve or
	// to bind.
	detail := map[string]any{"replay_of": string(of)}
	if from != "" {
		detail["step"] = string(from)
	}
	// And with what fired the run as it was frozen on it, "so a replay sees what fired it, not what
	// is true now".
	// And with the namespace's variables the run read when it was created, so that it reads what
	// the run read and not what is true now: none, where it read none, rather than what is set now.
	var fired db.TriggerContext
	var vars map[string]any
	if err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		if fired, err = ns.ContextOf(ctx, of); err != nil {
			return err
		}
		vars, err = ns.NamespaceVarsOf(ctx, of)
		return err
	}); err != nil {
		fail(w, http.StatusInternalServerError, "the run could not be read")
		return
	}
	started, err := s.starter.Start(r.Context(), trigger.Request{
		Namespace: over.Namespace, Workflow: d.Workflow,
		Kind: agk.TriggerManual, By: string(who),
		Commit: d.Commit, Bound: inputs,
		ReplayOf: of, ReplayFrom: from, Detail: detail,
		Context: fired, NamespaceVars: vars,
	})
	if s.refused(w, Target{Namespace: over.Namespace, Workflow: d.Workflow}, d.Commit, err) {
		return
	}
	run := started.Run
	w.Header().Set("Location", fmt.Sprintf("/api/v1/%s/runs/%s", over.Namespace, run))
	answer := map[string]any{"run": string(run), "state": agk.Queued.String(), "commit": d.Commit, "replay_of": string(of)}
	if from != "" {
		answer["replay_from"] = string(from)
	}
	write(w, http.StatusAccepted, answer)
}

// orEmptyMap is a run's inputs, or none written as an object.
func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
