package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/repo"
	"github.com/agentiik/agentiik/version"
)

// A workflow's repository, as the API answers it: created empty, read with the version a run naming
// no ref runs and a page of its history, its tree read at a ref, its default branch named and
// protected, and deleted. Renaming and moving one are to come.

// WorkflowCreate is what POST /api/v1/{ns}/workflows reads, openapi.json's workflowCreate.
type WorkflowCreate struct {
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch,omitempty"`
	Protected     bool   `json:"protected,omitempty"`

	// branchGiven is whether the body names a default branch, so that one it names empty is
	// refused as the branch git could not name that it is, rather than taken for main.
	branchGiven bool
}

func (c *WorkflowCreate) field(b *body, name string) error {
	switch name {
	case "name":
		return text(b, &c.Name)
	case "default_branch":
		c.branchGiven = true
		return text(b, &c.DefaultBranch)
	case "protected":
		return flag(b, &c.Protected)
	}
	return unknown(name)
}

// WorkflowUpdate is what PATCH /api/v1/{ns}/workflows/{name} reads, openapi.json's workflowUpdate:
// each member it holds, and nothing it leaves out.
type WorkflowUpdate struct {
	Name          *string `json:"name,omitempty"`
	Namespace     *string `json:"namespace,omitempty"`
	DefaultBranch *string `json:"default_branch,omitempty"`
	Protected     *bool   `json:"protected,omitempty"`
}

func (u *WorkflowUpdate) field(b *body, name string) error {
	var s string
	switch name {
	case "name":
		u.Name = &s
		return text(b, u.Name)
	case "namespace":
		u.Namespace = &s
		return text(b, u.Namespace)
	case "default_branch":
		u.DefaultBranch = &s
		return text(b, u.DefaultBranch)
	case "protected":
		u.Protected = new(bool)
		return flag(b, u.Protected)
	}
	return unknown(name)
}

// Repository is a workflow's repository, wire.schema.json's repository.
type Repository struct {
	Namespace     string            `json:"namespace"`
	Name          string            `json:"name"`
	DefaultBranch string            `json:"default_branch"`
	Protected     bool              `json:"protected"`
	Labels        map[string]string `json:"labels"`
	CreatedAt     time.Time         `json:"created_at"`
	// Head is the commit the default branch points at, null while it is unborn.
	Head     *string `json:"head"`
	CloneURL string  `json:"clone_url"`
}

// Version is a commit accepted as a version, wire.schema.json's version.
type Version struct {
	Commit    string    `json:"commit"`
	Parent    string    `json:"parent,omitempty"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
	Source    string    `json:"source"`

	// Library is set where the commit is a library's, which other workflows include and nothing
	// runs, and is absent otherwise.
	Library bool `json:"library,omitempty"`
}

// HistoryEntry is one commit of the default branch's first-parent history, openapi.json's
// historyEntry, and the version it is where it is one.
type HistoryEntry struct {
	Commit     string       `json:"commit"`
	Parent     string       `json:"parent,omitempty"`
	Author     *repo.Author `json:"author,omitempty"`
	AuthoredAt string       `json:"authored_at,omitempty"`
	Subject    string       `json:"subject,omitempty"`
	Version    *Version     `json:"version,omitempty"`
}

// WorkflowDetail is what GET /api/v1/{ns}/workflows/{name} answers, openapi.json's workflowDetail.
type WorkflowDetail struct {
	Repository Repository      `json:"repository"`
	Version    *Version        `json:"version,omitempty"`
	Graph      json.RawMessage `json:"graph,omitempty"`
	History    []HistoryEntry  `json:"history"`
	Next       string          `json:"next,omitempty"`
}

// Tree is the files of the commit a ref names, openapi.json's tree.
type Tree struct {
	Commit  string     `json:"commit"`
	Entries []TreeFile `json:"entries"`
}

// TreeFile is one file of a tree, openapi.json's treeEntry.
type TreeFile struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// workflowBodyMaxBytes bounds what the create and change routes read: a name, a branch and a
// flag, a few hundred bytes, with room for a branch of the 1,024 bytes a ref may be.
const workflowBodyMaxBytes = 8 << 10

// historyDefault and historyMost are how many commits a page of history lists where limit is left
// out and at the most, as GET /api/v1/runs lists runs.
const (
	historyDefault = 50
	historyMost    = 500
)

// wholeCommit is a commit named in full, as a version is.
var wholeCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// registerWorkflows registers the repository's routes.
func (s *Server) registerWorkflows(rt *Router) error {
	for _, r := range []struct {
		method, pattern string
		guard           Guard
		handler         Handler
	}{
		// What registering a version takes, at the namespace, so that an editor creates the
		// repository its first push needs.
		{"POST", "/api/v1/{namespace}/workflows",
			Needs{Permission: WorkflowWrite, Scope: Namespace}, s.createWorkflow},
		{"GET", "/api/v1/{namespace}/workflows/{workflow}",
			Needs{Permission: WorkflowRead, Scope: Workflow}, s.readWorkflow},
		// grant:manage besides, for the default branch and its protection, which decide who
		// may move what production runs; a caller without it is answered as one who cannot
		// read the workflow is. And who owns which namespace, for a move between two.
		{"PATCH", "/api/v1/{namespace}/workflows/{workflow}",
			Needs{Permission: WorkflowRead, Scope: Workflow, Also: GrantManage, Asks: []Permission{WorkflowWrite}, Owning: true}, s.updateWorkflow},
		{"GET", "/api/v1/{namespace}/workflows/{workflow}/tree/{ref...}",
			Needs{Permission: WorkflowRead, Scope: Workflow}, s.readTree},
		{"DELETE", "/api/v1/{namespace}/workflows/{workflow}",
			Needs{Permission: WorkflowDelete, Scope: Workflow}, s.deleteWorkflow},
	} {
		if err := rt.Handle(r.method, r.pattern, r.guard, r.handler); err != nil {
			return err
		}
	}
	return nil
}

// repositoryOut is a workflow's repository as the wire writes it, its labels those the version at
// commit writes, the version a run naming no ref runs, and none where commit is empty.
func (s *Server) repositoryOut(ctx context.Context, w db.WorkflowRecord, commit string) Repository {
	out := Repository{
		Namespace: w.Namespace, Name: w.Name, DefaultBranch: w.DefaultBranch, Protected: w.Protected,
		Labels: s.labelsAt(ctx, w.Namespace, w.Name, commit), CreatedAt: w.CreatedAt.UTC(),
		CloneURL: strings.TrimRight(s.publicURL, "/") + "/" + w.Namespace + "/" + w.Name + ".git",
	}
	if w.Head != "" {
		head := w.Head
		out.Head = &head
	}
	return out
}

// labelsAt is what metadata.labels writes in the entry point of the version at commit, which is
// what the wire answers as a repository's labels: labels a search finds a workflow by, read from the
// version they were pushed in rather than kept beside it, so that they cannot say something else.
// None where there is no such version, it writes none, or it could not be read, since labels mean
// nothing to the engine and a repository is answered without them rather than not at all.
func (s *Server) labelsAt(ctx context.Context, namespace, workflow, commit string) map[string]string {
	if commit == "" || s.versions == nil {
		return map[string]string{}
	}
	g, err := s.versions.Graph(ctx, namespace, workflow, commit)
	if err != nil {
		return map[string]string{}
	}
	return labelsOf(g)
}

func labelsOf(g *graph.Graph) map[string]string {
	if labels := g.Workflow().Metadata.Labels; labels != nil {
		return labels
	}
	return map[string]string{}
}

func versionOut(l db.Listed) *Version {
	return &Version{Commit: l.Commit, Parent: l.Parent, Author: l.Author, CreatedAt: l.CreatedAt.UTC(), Source: l.Source, Library: l.Library}
}

// checkWorkflowName refuses a name a workflow cannot have, as the tree push refuses one.
func checkWorkflowName(name string) error {
	switch {
	case len(name) > agk.IdentifierMaxBytes:
		return fmt.Errorf("a workflow name is at most %d characters and this one is %d: one name has to survive a URL, a directory and a tool list unchanged, and no directory holds a longer one", agk.IdentifierMaxBytes, len(name))
	case !workflowName.MatchString(name):
		return fmt.Errorf("%.64q is not a workflow name: a workflow is named the way the workflow file names everything, letters, digits, hyphens and underscores beginning with a letter or a digit, so that one name survives a URL, a directory and a tool list unchanged", name)
	}
	return nil
}

// checkBranch refuses a default branch git would refuse, or one past the bound a ref is held to. A
// ref named refs/heads/-x or refs/heads/@ is one git keeps, but neither is a branch anybody checks
// out by name: git reads -x as an option and @ as HEAD, and git check-ref-format --branch refuses
// both, as the wire's branch grammar does.
func checkBranch(branch string) error {
	switch {
	case branch == "":
		return errors.New("default_branch is empty, and a branch has a name: leave the member out for main")
	case strings.HasPrefix(branch, "-"):
		return fmt.Errorf("%.64q is not a branch git could name: git reads a name beginning with - as an option", branch)
	case branch == "@":
		return errors.New(`"@" is not a branch git could name: git reads @ alone as HEAD`)
	}
	if err := db.CheckRef("refs/heads/" + branch); err != nil {
		return fmt.Errorf("%.64q is not a branch git could name: %w", branch, err)
	}
	return nil
}

// createWorkflow answers POST /api/v1/{ns}/workflows: an empty repository, its default branch unborn.
func (s *Server) createWorkflow(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	var c WorkflowCreate
	if err := readAtMost(r, &c, workflowBodyMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if !c.branchGiven {
		c.DefaultBranch = "main"
	}
	if err := checkWorkflowName(c.Name); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := checkBranch(c.DefaultBranch); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	var created db.WorkflowRecord
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		if created, err = ns.CreateWorkflow(ctx, c.Name, c.DefaultBranch, c.Protected, string(who), s.now()); err != nil {
			return err
		}
		return ns.Audit(ctx, audit.Record{Actor: string(who), Action: audit.WorkflowCreate, Target: c.Name, Result: audit.Done,
			Detail: map[string]any{"default_branch": c.DefaultBranch, "protected": c.Protected}})
	})
	if errors.Is(err, db.ErrWorkflowExists) {
		fail(w, http.StatusConflict, fmt.Sprintf("%s holds a workflow named %s already: a name is one workflow in its namespace", over.Namespace, c.Name))
		return
	}
	if errors.Is(err, db.ErrWorkflowPurging) {
		fail(w, http.StatusConflict, fmt.Sprintf("a workflow named %s was deleted from %s and is still being purged: the name is free once its runs, versions and packs are gone, a day after its deletion at the soonest", c.Name, over.Namespace))
		return
	}
	if err != nil {
		s.report(fmt.Errorf("api: workflow %s/%s could not be created: %w", over.Namespace, c.Name, err))
		fail(w, http.StatusInternalServerError, "the workflow could not be created")
		return
	}
	write(w, http.StatusCreated, s.repositoryOut(r.Context(), created, ""))
}

// readWorkflow answers GET /api/v1/{ns}/workflows/{name}: the repository, the version a run naming
// no ref runs with its graph, and a page of the default branch's history.
func (s *Server) readWorkflow(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	q := r.URL.Query()
	from := q.Get("from")
	if from != "" && !wholeCommit.MatchString(from) {
		fail(w, http.StatusBadRequest, fmt.Sprintf("from is %.64q, and a page of history starts at a commit named in full, forty lowercase hexadecimal characters", from))
		return
	}
	limit := historyDefault
	if written := q.Get("limit"); written != "" {
		n, err := strconv.Atoi(written)
		if err != nil || n < 1 || n > historyMost {
			fail(w, http.StatusBadRequest, fmt.Sprintf("limit is %.16q, and a page lists from 1 to %d commits", written, historyMost))
			return
		}
		limit = n
	}

	var record db.WorkflowRecord
	var repository db.Repository
	var head *db.Listed
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		if record, err = ns.WorkflowRecord(ctx, over.Workflow); err != nil {
			return err
		}
		if record.Head == "" {
			latest, _, err := ns.TreeVersions(ctx, over.Workflow, "", 1)
			if err == nil && len(latest) == 1 {
				head = &latest[0]
			}
			return err
		}
		if repository, err = ns.Repository(ctx, over.Workflow); err != nil {
			return err
		}
		at, err := ns.VersionsAt(ctx, over.Workflow, []string{record.Head})
		if v, held := at[record.Head]; held {
			head = &v
		}
		return err
	})
	if errors.Is(err, db.ErrNoWorkflow) {
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	}
	if err != nil {
		s.report(err)
		fail(w, http.StatusInternalServerError, "the workflow could not be read")
		return
	}

	detail := WorkflowDetail{Repository: s.repositoryOut(r.Context(), record, ""), History: []HistoryEntry{}}
	if head != nil {
		detail.Version = versionOut(*head)
		if g, err := s.versions.Graph(r.Context(), over.Namespace, over.Workflow, head.Commit); err == nil {
			detail.Repository.Labels = labelsOf(g)
			if resolved, err := g.Resolved(head.Commit); err == nil {
				detail.Graph = resolved
			}
		}
	}
	var status int
	if record.Head == "" {
		detail.History, detail.Next, status, err = s.treeHistory(r.Context(), over, from, limit)
	} else {
		detail.History, detail.Next, status, err = s.gitHistory(r.Context(), over, repository, record.Head, from, limit)
	}
	if err != nil {
		if status == http.StatusInternalServerError {
			// What went wrong is the installation's to read, in its log: a pack that could
			// not be read names the store and the object, which the caller has no use for.
			s.report(fmt.Errorf("api: the history of %s/%s: %w", over.Namespace, over.Workflow, err))
			fail(w, status, "the history could not be read")
			return
		}
		fail(w, status, err.Error())
		return
	}
	write(w, http.StatusOK, detail)
}

// errNotInHistory is a page asked from a commit the history listed does not hold.
var errNotInHistory = errors.New("from names no commit of the default branch's history: a page starts at a commit the page before named as next")

// treeHistory is a page of the versions a tree push recorded, newest first: the history of a
// workflow no git push has filled.
func (s *Server) treeHistory(ctx context.Context, over Target, from string, limit int) ([]HistoryEntry, string, int, error) {
	var listed []db.Listed
	var next string
	err := s.pool.In(ctx, over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		listed, next, err = ns.TreeVersions(ctx, over.Workflow, from, limit)
		return err
	})
	if errors.Is(err, db.ErrNoVersion) {
		return nil, "", http.StatusNotFound, errNotInHistory
	}
	if err != nil {
		return nil, "", http.StatusInternalServerError, errors.New("the history could not be read")
	}
	history := make([]HistoryEntry, 0, len(listed))
	for _, l := range listed {
		history = append(history, HistoryEntry{Commit: l.Commit, Parent: l.Parent, Version: versionOut(l)})
	}
	return history, next, http.StatusOK, nil
}

// gitHistory is a page of the default branch's first-parent history, newest first, from from or its
// head, each commit that is a version marked.
//
// from has to be on that history, which is found by walking it from the head: a page is asked from
// the next the page before named, and a commit off the branch, or of another repository, is not a
// page of this one.
func (s *Server) gitHistory(ctx context.Context, over Target, repository db.Repository, head, from string, limit int) ([]HistoryEntry, string, int, error) {
	if s.packs == nil {
		return nil, "", http.StatusServiceUnavailable, errors.New(noPacks)
	}
	objects, err := s.objectsOf(repository)
	if err != nil {
		return nil, "", http.StatusInternalServerError, errors.New("the history could not be read")
	}
	defer objects.Close()

	at, err := repo.ParseID(head)
	if err != nil {
		return nil, "", http.StatusInternalServerError, errors.New("the history could not be read")
	}
	commitOf := func(id repo.ID) (*repo.Commit, error) {
		_, data, err := repo.ReadObject(ctx, objects, id, repo.MaxParsedBytes)
		if err != nil {
			return nil, err
		}
		return repo.ParseCommit(data)
	}
	if from != "" {
		want, err := repo.ParseID(from)
		if err != nil {
			return nil, "", http.StatusBadRequest, err
		}
		for at != want {
			c, err := commitOf(at)
			if err != nil {
				return nil, "", http.StatusInternalServerError, fmt.Errorf("the history could not be read: %w", err)
			}
			if len(c.Parents) == 0 {
				return nil, "", http.StatusNotFound, errNotInHistory
			}
			at = c.Parents[0]
		}
	}

	var history []HistoryEntry
	var next string
	for !at.IsZero() {
		if len(history) == limit {
			next = at.String()
			break
		}
		c, err := commitOf(at)
		if err != nil {
			return nil, "", http.StatusInternalServerError, fmt.Errorf("the history could not be read: %w", err)
		}
		e := HistoryEntry{
			Commit:     at.String(),
			Author:     ptr(c.Author.Author()),
			AuthoredAt: authoredAt(c.Author),
			Subject:    subjectOf(c.Message),
		}
		at = repo.ID{}
		if len(c.Parents) > 0 {
			e.Parent = c.Parents[0].String()
			at = c.Parents[0]
		}
		history = append(history, e)
	}

	commits := make([]string, len(history))
	for i, e := range history {
		commits[i] = e.Commit
	}
	var versions map[string]db.Listed
	err = s.pool.In(ctx, over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		versions, err = ns.VersionsAt(ctx, over.Workflow, commits)
		return err
	})
	if err != nil {
		return nil, "", http.StatusInternalServerError, errors.New("the history could not be read")
	}
	for i := range history {
		if v, held := versions[history[i].Commit]; held {
			history[i].Version = versionOut(v)
		}
	}
	if history == nil {
		history = []HistoryEntry{}
	}
	return history, next, http.StatusOK, nil
}

// authoredAt is when a commit says it was written, in the offset it was written in, as git log shows
// it.
func authoredAt(s repo.Signature) string {
	offset := 0
	if len(s.Zone) == 5 {
		hours, err1 := strconv.Atoi(s.Zone[1:3])
		minutes, err2 := strconv.Atoi(s.Zone[3:5])
		if err1 == nil && err2 == nil {
			offset = hours*3600 + minutes*60
			if s.Zone[0] == '-' {
				offset = -offset
			}
		}
	}
	return time.Unix(s.When, 0).In(time.FixedZone(s.Zone, offset)).Format(time.RFC3339)
}

// subjectOf is a commit message's first line, as git log --oneline shows it.
func subjectOf(message string) string {
	message = strings.TrimLeft(message, "\n")
	subject, _, _ := strings.Cut(message, "\n")
	return strings.TrimRight(subject, " \t\r")
}

// updateWorkflow answers PATCH /api/v1/{ns}/workflows/{name}: the workflow renamed, its default
// branch named, protected or left unprotected, or any of them at once.
func (s *Server) updateWorkflow(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	var u WorkflowUpdate
	if err := readAtMost(r, &u, workflowBodyMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	branching := u.DefaultBranch != nil || u.Protected != nil
	moving := u.Namespace != nil && *u.Namespace != over.Namespace
	if u.Name == nil && !branching && !moving {
		fail(w, http.StatusBadRequest, "the request names nothing to change: name, namespace, default_branch, protected, or any of them")
		return
	}
	if moving {
		if err := NamespaceRef(*u.Namespace); err != nil {
			fail(w, http.StatusBadRequest, fmt.Sprintf("namespace is %.64q: %v", *u.Namespace, err))
			return
		}
	}
	if u.Name != nil {
		if err := checkWorkflowName(*u.Name); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if u.DefaultBranch != nil {
		if err := checkBranch(*u.DefaultBranch); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	// "Each field under its own permission, every one of them held or nothing changed": a
	// rename under workflow:write, the default branch and its protection under grant:manage, a
	// move under workflow:write and ownership of both namespaces, "move between namespaces the
	// principal owns on both sides". A caller lacking one is answered as a workflow it cannot
	// see is, as the page says, since what it would change is decided by whoever holds that
	// permission; and a target it does not own as one that does not exist, so that asking
	// teaches nobody which namespaces there are.
	owns := func(namespace string) func(context.Context) (bool, error) {
		return func(ctx context.Context) (bool, error) { return Owns(r)(ctx, namespace) }
	}
	for _, needed := range []struct {
		asked bool
		holds func(context.Context) (bool, error)
	}{
		{u.Name != nil || moving, func(ctx context.Context) (bool, error) { return HoldsOn(r)(ctx, WorkflowWrite) }},
		{branching, HoldsAlso(r)},
		{moving, owns(over.Namespace)},
	} {
		if !needed.asked {
			continue
		}
		held, err := needed.holds(r.Context())
		if err != nil {
			fail(w, http.StatusInternalServerError, "the change could not be authorised")
			return
		}
		if !held {
			fail(w, http.StatusNotFound, "no such thing, or not yours")
			return
		}
	}
	if moving {
		if held, err := owns(*u.Namespace)(r.Context()); err != nil {
			fail(w, http.StatusInternalServerError, "the change could not be authorised")
			return
		} else if !held {
			fail(w, http.StatusNotFound, fmt.Sprintf("there is no namespace %s you own: a workflow moves between namespaces its mover owns on both sides", *u.Namespace))
			return
		}
		if !s.movable(w, r.Context(), over, *u.Namespace) {
			return
		}
	}

	// What the request changes besides a move, in the transaction the move is asked in where it
	// names one, so that a move refused refuses the request and changes nothing.
	var before, after db.WorkflowRecord
	change := func(ctx context.Context, ns *db.NS) (string, []audit.Record, error) {
		// What the change records, appended once everything else is done: an append takes the
		// head of the audit chain until the transaction ends, and holding it while the
		// branches are locked would keep every other act of the installation waiting.
		var records []audit.Record
		name := over.Workflow
		if u.Name != nil && *u.Name != over.Workflow {
			if err := ns.RenameWorkflow(ctx, over.Workflow, *u.Name); err != nil {
				return "", nil, err
			}
			name = *u.Name
			records = append(records, audit.Record{Actor: string(who), Action: audit.WorkflowUpdate, Target: name, Result: audit.Done,
				Detail: map[string]any{"name": name, "was": map[string]any{"name": over.Workflow}}})
		}
		var err error
		if !branching {
			after, err = ns.WorkflowRecord(ctx, name)
			return name, records, err
		}
		if before, after, err = ns.SetDefault(ctx, name, u.DefaultBranch, u.Protected); err != nil {
			return "", nil, err
		}
		if before.DefaultBranch != after.DefaultBranch {
			records = append(records, audit.Record{Actor: string(who), Action: audit.WorkflowUpdate, Target: name, Result: audit.Done,
				Detail: map[string]any{"default_branch": after.DefaultBranch, "was": map[string]any{"default_branch": before.DefaultBranch}}})
		}
		// Each branch whose protection changed, as it was and as it is: the one that is the
		// default now, which carries the repository's protection over from the one it
		// replaces, and the one it replaced, which loses it.
		was := before.Protected
		if before.DefaultBranch != after.DefaultBranch {
			was = false
			if before.Protected {
				records = append(records, audit.Record{Actor: string(who), Action: audit.RefProtect, Target: name, Result: audit.Done,
					Detail: map[string]any{"ref": "refs/heads/" + before.DefaultBranch, "protected": false, "was": true}})
			}
		}
		if was != after.Protected {
			records = append(records, audit.Record{Actor: string(who), Action: audit.RefProtect, Target: name, Result: audit.Done,
				Detail: map[string]any{"ref": "refs/heads/" + after.DefaultBranch, "protected": after.Protected, "was": was}})
		}
		return name, records, nil
	}
	// refused answers what the change was refused for, and false where it was not.
	refused := func(err error) bool {
		switch {
		case err == nil:
			return false
		case errors.Is(err, db.ErrNoWorkflow):
			fail(w, http.StatusNotFound, "no such thing, or not yours")
		case errors.Is(err, db.ErrWorkflowMoving):
			fail(w, http.StatusConflict, movingSentence(over))
		case errors.Is(err, db.ErrWorkflowExists):
			fail(w, http.StatusConflict, fmt.Sprintf("%s holds a workflow named %s already: a name is one workflow in its namespace", over.Namespace, *u.Name))
		case errors.Is(err, db.ErrWorkflowPurging):
			fail(w, http.StatusConflict, fmt.Sprintf("a workflow named %s was deleted from %s and is still being purged: the name is free once its runs, versions and packs are gone", *u.Name, over.Namespace))
		case errors.Is(err, db.ErrNoBranch):
			fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("%s holds no branch %s: the default branch is one the repository holds, since HEAD names it and a clone checks it out, and only a repository nothing was pushed to names the branch its first push will create", over.Workflow, *u.DefaultBranch))
		default:
			s.report(fmt.Errorf("api: workflow %s/%s could not be changed: %w", over.Namespace, over.Workflow, err))
			fail(w, http.StatusInternalServerError, "the workflow could not be changed")
		}
		return true
	}

	if moving {
		name := over.Workflow
		if u.Name != nil {
			name = *u.Name
		}
		var besides db.Change
		if u.Name != nil || branching {
			besides = change
		}
		s.askMove(w, r.Context(), who, over, name, *u.Namespace, besides, refused)
		return
	}
	name := over.Workflow
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var records []audit.Record
		var err error
		if name, records, err = change(ctx, ns); err != nil {
			return err
		}
		return appendAll(ctx, ns, records)
	})
	if refused(err) {
		return
	}
	head := after.Head
	if head == "" {
		head, _ = s.defaultCommit(r.Context(), Target{Namespace: over.Namespace, Workflow: name})
	}
	write(w, http.StatusOK, s.repositoryOut(r.Context(), after, head))
}

// appendAll appends records to the audit log in the order given, as the last statements of the
// transaction.
func appendAll(ctx context.Context, ns *db.NS, records []audit.Record) error {
	for _, rec := range records {
		if err := ns.Audit(ctx, rec); err != nil {
			return err
		}
	}
	return nil
}

// deleteWorkflow answers DELETE /api/v1/{ns}/workflows/{name}: the workflow absent from the answer
// on, its runs still going asked to cancel, the data of every run expiring now, and the rest the
// leading controller's to purge, which is why the answer is 202 and says nothing more.
func (s *Server) deleteWorkflow(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	if n, _ := io.ReadFull(io.LimitReader(r.Body, 1), make([]byte, 1)); n > 0 {
		fail(w, http.StatusBadRequest, "DELETE reads no body, and this request sends one: what is deleted is the workflow the path names, whole")
		return
	}
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		cancelled, err := ns.DeleteWorkflow(ctx, over.Workflow, string(who), s.now())
		if err != nil {
			return err
		}
		for _, run := range cancelled {
			if err := ns.NotifyRun(ctx, run); err != nil {
				return err
			}
		}
		return ns.Audit(ctx, audit.Record{Actor: string(who), Action: audit.WorkflowDelete, Target: over.Workflow, Result: audit.Done,
			Detail: map[string]any{"runs_cancelled": len(cancelled)}})
	})
	switch {
	case errors.Is(err, db.ErrNoWorkflow):
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	case errors.Is(err, db.ErrWorkflowMoving):
		fail(w, http.StatusConflict, movingSentence(over))
		return
	case err != nil:
		s.report(fmt.Errorf("api: workflow %s/%s could not be deleted: %w", over.Namespace, over.Workflow, err))
		fail(w, http.StatusInternalServerError, "the workflow could not be deleted")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// readTree answers GET /api/v1/{ns}/workflows/{name}/tree/{ref}: the files of the version a ref
// names, or with ?path= one file's bytes.
func (s *Server) readTree(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	ref := r.PathValue("ref")
	var commit string
	var files []db.TreeFile
	status := http.StatusOK
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		// Whether the workflow is there first, so that a ref asked of one that is not is
		// answered as the workflow is, whatever the ref.
		if _, err := ns.WorkflowRecord(ctx, over.Workflow); err != nil {
			return err
		}
		var why error
		commit, status, why = resolveRef(ctx, ns, over.Workflow, ref)
		if why != nil {
			return why
		}
		var err error
		files, err = ns.Tree(ctx, over.Workflow, commit)
		if errors.Is(err, db.ErrNoVersion) || errors.Is(err, db.ErrNoTree) {
			status = http.StatusNotFound
			return fmt.Errorf("%.200s names %s, which is no version whose files are kept", ref, commit)
		}
		return err
	})
	if errors.Is(err, db.ErrNoWorkflow) {
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	}
	if err != nil {
		if status == http.StatusOK {
			s.report(err)
			fail(w, http.StatusInternalServerError, "the tree could not be read")
			return
		}
		fail(w, status, err.Error())
		return
	}
	slices.SortFunc(files, func(a, b db.TreeFile) int { return strings.Compare(a.Path, b.Path) })

	path, asked := r.URL.Query()["path"]
	if !asked {
		tree := Tree{Commit: commit, Entries: make([]TreeFile, 0, len(files))}
		for _, f := range files {
			tree.Entries = append(tree.Entries, TreeFile{Path: f.Path, Mode: f.Mode, Size: f.Size, SHA256: f.SHA256})
		}
		write(w, http.StatusOK, tree)
		return
	}
	name := path[0]
	if err := version.TreePath(name); err != nil {
		fail(w, http.StatusBadRequest, fmt.Sprintf("path is %.200q, which no tree holds: %v", name, err))
		return
	}
	for _, f := range files {
		if f.Path != name {
			continue
		}
		if s.objects == nil {
			fail(w, http.StatusServiceUnavailable, "this installation has no object store attached, and a file of a tree is read from it")
			return
		}
		body, err := s.objects.Open(r.Context(), artifact.Key(over.Namespace, f.SHA256))
		if err != nil {
			s.report(fmt.Errorf("api: %s of %s/%s@%s: %w", name, over.Namespace, over.Workflow, commit, err))
			fail(w, http.StatusInternalServerError, "the file could not be read")
			return
		}
		defer body.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
		w.Header().Set("ETag", `"`+f.SHA256+`"`)
		w.WriteHeader(http.StatusOK)
		io.Copy(w, body)
		return
	}
	for _, f := range files {
		if strings.HasPrefix(f.Path, name+"/") {
			fail(w, http.StatusNotFound, fmt.Sprintf("%.200s is a directory of the tree, and path names a file: the listing names every file of the tree", name))
			return
		}
	}
	fail(w, http.StatusNotFound, fmt.Sprintf("the tree at %.200s holds no file %.200s", ref, name))
}

// resolveRef is the commit a ref names in a workflow's repository: a branch or a tag by its short
// name or in full, or a commit named in full that is a version. It answers the status and the
// sentence of a ref naming nothing, or two things.
func resolveRef(ctx context.Context, ns *db.NS, workflow, ref string) (string, int, error) {
	if wholeCommit.MatchString(ref) {
		if _, err := ns.Version(ctx, workflow, ref); err != nil {
			if errors.Is(err, db.ErrNoVersion) {
				return "", http.StatusNotFound, fmt.Errorf("%s is no version of the workflow", ref)
			}
			return "", http.StatusOK, err
		}
		return ref, http.StatusOK, nil
	}
	repository, err := ns.Repository(ctx, workflow)
	if err != nil {
		return "", http.StatusOK, err
	}
	held := map[string]db.Ref{}
	for _, r := range repository.Refs {
		held[r.Name] = r
	}
	candidates := []string{ref}
	if !strings.HasPrefix(ref, "refs/") {
		candidates = []string{"refs/heads/" + ref, "refs/tags/" + ref}
	}
	var found []db.Ref
	for _, name := range candidates {
		if r, ok := held[name]; ok && r.Commit != "" {
			found = append(found, r)
		}
	}
	switch len(found) {
	case 0:
		return "", http.StatusNotFound, fmt.Errorf("%.200s names no branch, no tag and no version of the workflow", ref)
	case 1:
		return found[0].Commit, http.StatusOK, nil
	}
	return "", http.StatusBadRequest, fmt.Errorf("%.200s is both a branch and a tag of the workflow: name the one meant in full, refs/heads/%.200s or refs/tags/%.200s", ref, ref, ref)
}

// defaultCommit is the commit a run naming none runs: the default branch's head, or while it is
// unborn the latest version a tree push recorded, or ErrNoVersion where there is neither.
func (s *Server) defaultCommit(ctx context.Context, over Target) (string, error) {
	var commit string
	err := s.pool.In(ctx, over.Namespace, func(ctx context.Context, ns *db.NS) error {
		record, err := ns.WorkflowRecord(ctx, over.Workflow)
		if err != nil {
			return err
		}
		if record.Head != "" {
			commit = record.Head
			return nil
		}
		latest, _, err := ns.TreeVersions(ctx, over.Workflow, "", 1)
		if err != nil {
			return err
		}
		if len(latest) == 0 {
			return fmt.Errorf("%w: %s/%s has no version to run", db.ErrNoVersion, over.Namespace, over.Workflow)
		}
		commit = latest[0].Commit
		return nil
	})
	return commit, err
}

// ptr is a value's address, for a member the wire leaves out where it is nil.
func ptr[T any](v T) *T { return &v }
