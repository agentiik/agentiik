package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/version"
)

// The routes, and the one thing they all have in common: none of them decides anything.
//
// "They share the database and nothing else. There is no remote call between them, in either
// direction." So the API writes a row and issues a notification, and the controller is what turns
// that into work. A route here that started a task would be a second scheduler.

// Server serves /api/v1.
type Server struct {
	pool     *db.Pool
	versions *version.Store
	now      func() time.Time
}

// ServerOptions are what a Server is given.
type ServerOptions struct {
	Pool     *db.Pool
	Versions *version.Store

	// Now is the clock, an argument so that a test has one.
	Now func() time.Time
}

// NewServer builds one and registers its routes on a router.
//
// The router is the caller's, because an installation may serve more than this: the MCP facades
// are "a facade over the API, and every call is executed as the principal that presented the
// token", so they register beside these rather than wrapping them.
func NewServer(rt *Router, o ServerOptions) (*Server, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and the API and the controller share the database and nothing else")
	case o.Versions == nil:
		return nil, errors.New("api: no version store, and a run is pinned to a version")
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	s := &Server{pool: o.Pool, versions: o.Versions, now: o.Now}

	for _, r := range []struct {
		method  string
		pattern string
		guard   Guard
		handler Handler
	}{
		{"PUT", "/api/v1/{namespace}/workflows/{workflow}/versions/{commit}",
			Needs{Permission: WorkflowWrite, Scope: Workflow}, s.push},
		{"POST", "/api/v1/{namespace}/workflows/{workflow}/runs",
			Needs{Permission: WorkflowRun, Scope: Workflow}, s.start},
		{"GET", "/api/v1/{namespace}/runs",
			Needs{Permission: RunRead, Scope: Namespace}, s.list},
		{"GET", "/api/v1/{namespace}/runs/{run}",
			Needs{Permission: RunRead, Scope: Namespace}, s.detail},
	} {
		if err := rt.Handle(r.method, r.pattern, r.guard, r.handler); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Push records one version of one workflow.
//
// PUT rather than POST, and the commit in the path rather than in the body, because "a version is
// a commit": pushing the same commit twice is the same version and has to be the same request.
type Push struct {
	// Entry is the path of the entry point in the tree, Document is what it holds, and
	// Includes are the files it pulls in. Together they are what the version is: enough to
	// rebuild it with no tree in reach.
	Entry     string            `json:"entry"`
	Document  []byte            `json:"document"`
	Includes  map[string][]byte `json:"includes,omitempty"`
	Manifests map[string][]byte `json:"manifests,omitempty"`

	Parent string `json:"parent,omitempty"`
	Branch string `json:"branch,omitempty"`
}

func (s *Server) push(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	var p Push
	if err := read(r, &p); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	commit := r.PathValue("commit")

	v := db.Version{
		Namespace: over.Namespace, Workflow: over.Workflow, Commit: commit, Parent: p.Parent,
		Entry: p.Entry, Document: p.Document, Includes: p.Includes, Manifests: p.Manifests,
		Author: string(who), CreatedAt: s.now(),
	}
	// Built before it is written, so that a version that cannot be rebuilt is refused at the
	// push rather than discovered by the first run of it.
	if _, err := version.Build(v); err != nil {
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		if err := ns.SaveWorkflow(ctx, over.Workflow, p.Branch); err != nil {
			return err
		}
		return ns.SaveVersion(ctx, v)
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the version could not be recorded")
		return
	}
	write(w, http.StatusOK, map[string]any{
		"namespace": over.Namespace, "workflow": over.Workflow, "commit": commit,
	})
}

// Start is a manual run: the inputs, and nothing else. What version it runs is the workflow's
// default branch resolved to a commit, which whoever pushed it named.
type Start struct {
	Commit string         `json:"commit"`
	Inputs map[string]any `json:"inputs,omitempty"`
}

func (s *Server) start(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	var start Start
	if err := read(r, &start); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if start.Commit == "" {
		fail(w, http.StatusBadRequest, "a run is pinned to a commit and this one names none")
		return
	}

	g, err := s.versions.Graph(r.Context(), over.Namespace, over.Workflow, start.Commit)
	if err != nil {
		if errors.Is(err, db.ErrNoVersion) {
			// The same answer an inaccessible one gets, for the same reason.
			fail(w, http.StatusNotFound, "no such thing, or not yours")
			return
		}
		fail(w, http.StatusInternalServerError, "the version could not be read")
		return
	}

	run := agk.NewRunID()
	err = s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		if err := ns.CreateRun(ctx, db.NewRun{
			ID: run, Workflow: over.Workflow, Commit: start.Commit,
			Trigger: agk.TriggerManual, TriggeredBy: string(who),
			Inputs: start.Inputs, Steps: g.Steps(),
		}); err != nil {
			return err
		}
		// In the same transaction, because PostgreSQL delivers the notification only
		// when it commits: the row and the wake-up are one fact rather than two.
		return ns.NotifyRun(ctx, run)
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the run could not be created")
		return
	}

	// 202 rather than 201: the run exists, and nothing has happened yet. What happens is the
	// controller's, and it has been told.
	w.Header().Set("Location", fmt.Sprintf("/api/v1/%s/runs/%s", over.Namespace, run))
	write(w, http.StatusAccepted, map[string]any{
		"run": string(run), "state": agk.Queued.String(),
	})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	q := db.RunQuery{
		Workflow: r.URL.Query().Get("workflow"),
		State:    r.URL.Query().Get("state"),
		Limit:    intOr(r.URL.Query().Get("limit"), 50),
	}
	var runs []db.RunSummary
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		runs, err = ns.Runs(ctx, q)
		return err
	})
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	write(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) detail(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	run := agk.RunID(r.PathValue("run"))
	var detail db.RunDetail
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		detail, err = ns.RunDetail(ctx, run)
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
	write(w, http.StatusOK, detail)
}

// read decodes a body, closed: a request carrying a field this does not know is refused rather
// than half understood.
func read(r *http.Request, into any) error {
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return fmt.Errorf("the request body: %w", err)
	}
	return nil
}

func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// fail answers a refusal that is about the request rather than about who asked.
func fail(w http.ResponseWriter, status int, message string) {
	refuse(w, status, message)
}

func intOr(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n < 1 {
		return fallback
	}
	return n
}
