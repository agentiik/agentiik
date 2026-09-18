package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/agentiik/agentiik/db"
)

// The administrator's half of the runner surface: the pools, and the tokens that let a machine
// into one.
//
// "An administrator creates a runner pool with its labels, its accepted namespaces and its
// resource ceilings, then issues a join token." Both of those are here, and both are refused to
// everybody but an administrator, for the reason the inventory is: a pool is installation-wide
// and a namespace has no business reading another's.

// TokenLife is the longest a join token may be asked to live.
//
// "One hour after it was issued is the default, and it is short because the token only has to
// survive the minutes between an administrator copying it and a machine presenting it." A day is
// already generous for that, and a token good for a week is a credential lying around in a
// terminal history for a week.
const TokenLife = 24 * time.Hour

// Pool is what an administrator creates.
type Pool struct {
	Name   string   `json:"pool"`
	Labels []string `json:"labels,omitempty"`

	AcceptedNamespaces []string `json:"accepted_namespaces,omitempty"`
	MaxCPU             int      `json:"max_cpu,omitempty"`
	MaxMemoryBytes     int64    `json:"max_memory_bytes,omitempty"`
	MaxDiskBytes       int64    `json:"max_disk_bytes,omitempty"`
}

func (s *RunnerAPI) createPool(w http.ResponseWriter, r *http.Request, who Principal, _ Target) {
	var p Pool
	if err := read(r, &p); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	var created db.RunnerPool
	err := s.pool.Installation(r.Context(), db.RunnerInventory, func(ctx context.Context, wide *db.Wide) error {
		if err := wide.CreateRunnerPool(ctx, db.RunnerPool{
			Name: p.Name, Labels: p.Labels, AcceptedNamespaces: p.AcceptedNamespaces,
			MaxCPU: p.MaxCPU, MaxMemoryBytes: p.MaxMemoryBytes, MaxDiskBytes: p.MaxDiskBytes,
			CreatedBy: string(who),
		}); err != nil {
			return err
		}
		var err error
		created, err = wide.RunnerPoolNamed(ctx, p.Name)
		return err
	})
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	write(w, http.StatusCreated, created)
}

func (s *RunnerAPI) pools(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	var pools []db.RunnerPool
	err := s.pool.Installation(r.Context(), db.RunnerInventory, func(ctx context.Context, wide *db.Wide) error {
		var err error
		pools, err = wide.RunnerPools(ctx)
		return err
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the runner pools could not be read")
		return
	}
	if pools == nil {
		pools = []db.RunnerPool{}
	}
	write(w, http.StatusOK, map[string]any{"runner_pools": pools})
}

// Issue asks for a join token: which labels the machine it is meant for may claim, and how long
// the administrator needs to get it there.
type Issue struct {
	Labels           []string `json:"labels,omitempty"`
	ExpiresInSeconds int      `json:"expires_in_seconds,omitempty"`
}

func (s *RunnerAPI) issue(w http.ResponseWriter, r *http.Request, who Principal, _ Target) {
	var ask Issue
	if err := read(r, &ask); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	life := time.Hour
	if ask.ExpiresInSeconds > 0 {
		life = time.Duration(ask.ExpiresInSeconds) * time.Second
	}
	if life > TokenLife {
		fail(w, http.StatusBadRequest, "a join token only has to survive the minutes between an administrator copying it and a machine presenting it, and this one asks for longer than a day")
		return
	}

	pool := r.PathValue("pool")
	var issued db.JoinToken
	err := s.pool.Installation(r.Context(), db.RunnerInventory, func(ctx context.Context, wide *db.Wide) error {
		var err error
		issued, err = wide.IssueJoinToken(ctx, pool, ask.Labels, string(who), s.now().Add(life))
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoRunnerPool):
		// A pool is installation-wide and the caller is already an administrator, so there
		// is nothing to hide: 404 here means the pool, not the route.
		fail(w, http.StatusNotFound, "no runner pool of that name")
		return
	case err != nil:
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// "It is shown once, here", which is the whole reason this answer exists.
	write(w, http.StatusCreated, map[string]any{
		"id":         issued.ID,
		"pool":       issued.Pool,
		"labels":     orEmpty(issued.Labels),
		"token":      issued.Clear,
		"expires_at": issued.ExpiresAt.Format(time.RFC3339Nano),
	})
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
