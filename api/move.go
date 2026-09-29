package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/version"
)

// Moving a workflow to another namespace, asked at PATCH /api/v1/{ns}/workflows/{name} with
// namespace: "moves it to another namespace, namespace under workflow:write and ownership of both
// namespaces", and "a move answers 202 with Location, whatever else the body names". The move is
// judged and asked here, and carried out by the controller that leads (db.Pool.CompleteMove).

// movingSentence is what a request writing a workflow being moved is answered.
func movingSentence(over Target) string {
	return fmt.Sprintf("%s/%s is being moved to another namespace, and is frozen until the move is carried out: its pushes, runs, replays and changes are refused meanwhile, and it answers at its new place once it is done", over.Namespace, over.Workflow)
}

// movable judges a move of over, which will be called name, to target, and answers false once it has
// said why it is refused.
//
// The secrets first: "refused where it names a secret the target does not declare", which the
// versions its branches and tags point at say, or the latest version a tree push recorded while no
// git push has given it any: what a run naming no commit, or naming a branch or a tag, runs. A
// commit named by its hash, which no ref points at, is judged at its redemption as a version is
// after a declaration it names is removed. Then everything db.Pool.CheckMove refuses.
func (s *Server) movable(w http.ResponseWriter, ctx context.Context, over Target, name, target string) bool {
	missing, err := s.undeclaredIn(ctx, over, target)
	if err != nil {
		s.report(fmt.Errorf("api: the secrets %s/%s names could not be read: %w", over.Namespace, over.Workflow, err))
		fail(w, http.StatusInternalServerError, "the move could not be judged")
		return false
	}
	if len(missing) > 0 {
		them := "it"
		if len(missing) > 1 {
			them = "them"
		}
		fail(w, http.StatusConflict, fmt.Sprintf("%s/%s names %s, which %s does not declare: a workflow can name only the secrets its own namespace declares, so that moving it elsewhere breaks the reference rather than carrying access along. Declare %s in %s, and ask again",
			over.Namespace, over.Workflow, plural(missing, "the secret", "the secrets"), target, them, target))
		return false
	}
	return s.moveRefusal(w, s.pool.CheckMove(ctx, over.Namespace, over.Workflow, name, target), over, name, target)
}

// askMove asks the move of over to target, and answers 202 with where the workflow will be read
// once it is carried out.
func (s *Server) askMove(w http.ResponseWriter, ctx context.Context, who Principal, over Target, target string) {
	if !s.moveRefusal(w, s.pool.AskMove(ctx, over.Namespace, over.Workflow, target, string(who), s.now()), over, over.Workflow, target) {
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/api/v1/%s/workflows/%s", target, over.Workflow))
	w.WriteHeader(http.StatusAccepted)
}

// moveRefusal answers what a move was refused for, and false, or true where it was not.
func (s *Server) moveRefusal(w http.ResponseWriter, err error, over Target, name, target string) bool {
	var shared *db.SharedOutputs
	var room *db.NoRoomToMove
	switch {
	case err == nil:
		return true
	case errors.Is(err, db.ErrNoWorkflow):
		fail(w, http.StatusNotFound, "no such thing, or not yours")
	case errors.Is(err, db.ErrNoNamespace):
		fail(w, http.StatusNotFound, fmt.Sprintf("there is no namespace %s you own: a workflow moves between namespaces its mover owns on both sides", target))
	case errors.Is(err, db.ErrWorkflowMoving):
		fail(w, http.StatusConflict, movingSentence(over))
	case errors.Is(err, db.ErrRunsGoing):
		fail(w, http.StatusConflict, fmt.Sprintf("a run of %s/%s has not finished, queued, running or waiting, and a workflow moves once every run of it has: the document a run is decided from names the namespace it started in, and the tasks it plans would be sent there. Ask again once it ends, or cancel it", over.Namespace, over.Workflow))
	case errors.As(err, &shared):
		fail(w, http.StatusConflict, fmt.Sprintf("run %s republished outputs run %s made, from the cache of %s, and one of them is of another workflow: a hit names the files the first run made, and a move would leave one of the two runs naming files in a namespace it is not in. Ask again once either run's retention has passed", shared.Run, shared.From, over.Namespace))
	case errors.Is(err, db.ErrWorkflowExists):
		fail(w, http.StatusConflict, fmt.Sprintf("%s holds a workflow named %s already, or one moving there does: a name is one workflow in its namespace", target, name))
	case errors.Is(err, db.ErrWorkflowPurging):
		fail(w, http.StatusConflict, fmt.Sprintf("a workflow named %s was deleted from %s and is still being purged: the name is free once its runs, versions and packs are gone", name, target))
	case errors.As(err, &room):
		fail(w, http.StatusConflict, fmt.Sprintf("%s holds %d bytes of the %d its max_artifact_bytes allows, and the live artifacts of %s/%s bring %d more: a move is refused where the target's quotas cannot hold what moves", target, room.Held, room.Limit, over.Namespace, over.Workflow, room.Moving))
	default:
		s.report(fmt.Errorf("api: the move of %s/%s to %s: %w", over.Namespace, over.Workflow, target, err))
		fail(w, http.StatusInternalServerError, "the move could not be asked")
	}
	return false
}

// undeclaredIn answers the secrets the versions over's refs point at name and target does not
// declare, sorted.
func (s *Server) undeclaredIn(ctx context.Context, over Target, target string) ([]string, error) {
	var commits []string
	err := s.pool.In(ctx, over.Namespace, func(ctx context.Context, ns *db.NS) error {
		repository, err := ns.Repository(ctx, over.Workflow)
		if err != nil {
			return err
		}
		for _, ref := range repository.Refs {
			if ref.Commit != "" && !slices.Contains(commits, ref.Commit) {
				commits = append(commits, ref.Commit)
			}
		}
		if len(commits) == 0 {
			latest, _, err := ns.TreeVersions(ctx, over.Workflow, "", 1)
			if err != nil {
				return err
			}
			for _, l := range latest {
				commits = append(commits, l.Commit)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var named []string
	for _, commit := range commits {
		g, err := s.versions.Graph(ctx, over.Namespace, over.Workflow, commit)
		switch {
		case errors.Is(err, version.ErrLibrary), errors.Is(err, db.ErrNoVersion):
			continue
		case err != nil:
			return nil, err
		}
		for _, secret := range version.SecretsNamed(g.Workflow()) {
			if !slices.Contains(named, secret) {
				named = append(named, secret)
			}
		}
	}
	if len(named) == 0 {
		return nil, nil
	}
	var declared []db.Declaration
	if err := s.pool.In(ctx, target, func(ctx context.Context, ns *db.NS) error {
		var err error
		declared, err = ns.Declarations(ctx)
		return err
	}); err != nil {
		return nil, err
	}
	var missing []string
	for _, secret := range named {
		if !slices.ContainsFunc(declared, func(d db.Declaration) bool { return d.Name == secret }) {
			missing = append(missing, secret)
		}
	}
	slices.Sort(missing)
	return missing, nil
}

// plural is one or the other, and the names after it.
func plural(names []string, one, several string) string {
	if len(names) == 1 {
		return one + " " + names[0]
	}
	return several + " " + strings.Join(names, ", ")
}
