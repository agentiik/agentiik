package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
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
	objects  artifact.Objects
	now      func() time.Time
}

// ServerOptions are what a Server is given.
type ServerOptions struct {
	Pool     *db.Pool
	Versions *version.Store

	// Objects is where a pushed tree is written. "every step of every run sees it,
	// mounted read-only at /agk/repo", and a runner fetches it from here with its task's
	// grant, exactly as it fetches an artifact, rather than from the version row. Without
	// one a push is answered 503, because a tree with nowhere to go is the installation's
	// to fix and not the caller's.
	Objects artifact.Objects

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
	s := &Server{pool: o.Pool, versions: o.Versions, objects: o.Objects, now: o.Now}

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

	// Tree is the commit's tree, every file of it, as every step will see it under /agk/repo.
	// It travels in the push because there is nowhere else it could come from yet: a version
	// is a commit, and until the installation hosts the repository itself it holds no copy
	// of that commit to read the files out of.
	Tree map[string]PushFile `json:"tree"`

	Parent string `json:"parent,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// PushFile is one file of the tree.
type PushFile struct {
	Content []byte `json:"content"`

	// Mode is git's, 0644 or 0755 where the file is executable, and it is always written.
	// Git tracks that one bit and a container needs it: an entry point that arrives 0644 is
	// a step that will not run, and a mode left to a default is a mode somebody guessed.
	Mode string `json:"mode"`
}

// TreeMaxBytes is the largest tree a push carries.
//
// It is a limit of this push rather than a rule about repositories. The tree travels inline, in
// one JSON document and in base64, until the installation hosts the repository and a push is
// git's own smart HTTP, which v0.4.0 brings and which takes the limit away with the transport that
// needed it. Until then the whole request is held in memory on its way through, and four
// mebibytes of entry point, fragments and scripts is a great deal of workflow. A tree above it is
// usually carrying something that belongs in an image or in an artifact, and the refusal says so.
const TreeMaxBytes = 4 << 20

// commitName is a commit as the version table holds one, and a push is held to it before anything
// is written. Left to the table's own check, a commit that is not one was refused only by the
// insert, after the tree was already in the store with nothing counting it, and answered 500 for
// what was the caller's mistake.
var commitName = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// pushMaxBytes is how large a push body may be, which is larger than any other body the API
// reads because a push carries the tree.
//
// The arithmetic is the reason for the number. A tree at TreeMaxBytes is five and a third
// mebibytes once base64 has had it, and the entry point and its includes are files of that same
// tree carried a second time, so up to as much again. Sixteen leaves over five mebibytes for the
// brick manifests, the paths and the JSON around them, which is more than a workflow has.
const pushMaxBytes = 16 << 20

func (s *Server) push(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	if s.objects == nil {
		// Before the body is read, because nothing in it could change the answer: the tree
		// has nowhere to go, and that is the installation's to fix rather than the caller's.
		fail(w, http.StatusServiceUnavailable, "this installation has no object store attached, and a pushed tree has nowhere to go")
		return
	}
	var p Push
	if err := readAtMost(r, &p, pushMaxBytes); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a push is at most %d bytes, and this one is larger: the tree it carries is limited to %d until the installation hosts the repository and a push is a git push", pushMaxBytes, TreeMaxBytes))
			return
		}
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	commit := r.PathValue("commit")

	// Everything that can be refused without writing anything is refused first, so that a
	// push that fails leaves no object behind it.
	if !commitName.MatchString(commit) {
		fail(w, http.StatusBadRequest, fmt.Sprintf("%q is not a commit, and a version is one: a commit is named by seven to forty lowercase hexadecimal characters", commit))
		return
	}
	if p.Parent != "" && !commitName.MatchString(p.Parent) {
		fail(w, http.StatusBadRequest, fmt.Sprintf("the parent %q is not a commit: a commit is named by seven to forty lowercase hexadecimal characters", p.Parent))
		return
	}
	paths, status, err := checkTree(p.Tree)
	if err != nil {
		fail(w, status, err.Error())
		return
	}
	if err := checkAgreement(p); err != nil {
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	tree, blobs := manifestOf(paths, p.Tree)

	v := db.Version{
		Namespace: over.Namespace, Workflow: over.Workflow, Commit: commit, Parent: p.Parent,
		Entry: p.Entry, Document: p.Document, Includes: p.Includes, Manifests: p.Manifests,
		Tree: tree, Author: string(who), CreatedAt: s.now(),
	}
	// Built before it is written, so that a version that cannot be rebuilt is refused at the
	// push rather than discovered by the first run of it.
	if _, err := version.Build(v); err != nil {
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// The bytes before the row, so that a version that exists names objects that exist. A
	// push that dies between the two leaves objects nothing references, which the collector
	// never sees and which the next push of the same files reuses; the other order would
	// leave a version whose /agk/repo cannot be fetched.
	if err := s.storeTree(r.Context(), over.Namespace, blobs, false); err != nil {
		fail(w, http.StatusInternalServerError, "the tree could not be stored")
		return
	}

	var saved db.Saved
	err = s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		if err := ns.SaveWorkflow(ctx, over.Workflow, p.Branch); err != nil {
			return err
		}
		var err error
		saved, err = ns.SaveVersion(ctx, v)
		return err
	})
	if errors.Is(err, db.ErrOtherTree) {
		fail(w, http.StatusConflict, fmt.Sprintf("%s was already pushed at %s with other files, and a version is a commit: one commit names exactly one tree, permanently", over.Workflow, commit))
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, "the version could not be recorded")
		return
	}

	// And again for any object a sweep had claimed while this version was raising its
	// reference onto it: the reference is safe, and the bytes may be what the sweep is about
	// to delete.
	again := make(map[string][]byte, len(saved.MustWriteBytes))
	for _, digest := range saved.MustWriteBytes {
		again[digest] = blobs[digest]
	}
	if err := s.storeTree(r.Context(), over.Namespace, again, true); err != nil {
		fail(w, http.StatusInternalServerError, "the tree could not be stored")
		return
	}

	write(w, http.StatusOK, map[string]any{
		"namespace": over.Namespace, "workflow": over.Workflow, "commit": commit,
	})
}

// checkTree refuses a tree that could not be laid out under /agk/repo, and answers its paths in
// order along with the status a refusal is answered with.
//
// Sorted, because two pushes of one commit have to produce the same version and a map has no
// order.
func checkTree(files map[string]PushFile) ([]string, int, error) {
	if len(files) == 0 {
		return nil, http.StatusBadRequest, errors.New("a push carries the tree of its commit and this one carries none: every step of every run sees the repository under /agk/repo, and a version without it would start containers on an empty directory")
	}
	paths := make([]string, 0, len(files))
	var total int64
	for p, f := range files {
		if err := checkTreePath(p); err != nil {
			return nil, http.StatusBadRequest, err
		}
		if f.Mode != "0644" && f.Mode != "0755" {
			return nil, http.StatusBadRequest, fmt.Errorf("%s is pushed with mode %q, and a tree carries git's two, written out: 0644, or 0755 where the file is executable", p, f.Mode)
		}
		total += int64(len(f.Content))
		paths = append(paths, p)
	}
	if total > TreeMaxBytes {
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("this tree is %d bytes and a push carries at most %d until the installation hosts the repository and a push is a git push: a tree this size is usually carrying something that belongs in an image or in an artifact", total, TreeMaxBytes)
	}
	sort.Strings(paths)

	// A path that is a file and also the directory of another cannot be laid out: one of the
	// two would have to lose, and which one would depend on the order the runner wrote them.
	for _, p := range paths {
		for i := range len(p) {
			if p[i] != '/' {
				continue
			}
			if _, file := files[p[:i]]; file {
				return nil, http.StatusBadRequest, fmt.Errorf("%s is both a file and the directory %s is in, and a tree laid out on a disk can hold only one of the two", p[:i], p)
			}
		}
	}
	return paths, 0, nil
}

// checkAgreement refuses a push whose version and tree are not one commit.
//
// The entry point and its includes travel twice, once as what the graph is rebuilt from and once
// as files of the tree, and the two have to be the same bytes. A version whose document said one
// thing while /agk/repo/agentiik.yaml said another would be a run decided from a file no step can
// see, which is precisely what "a version is a commit" is there to rule out.
func checkAgreement(p Push) error {
	entry, held := p.Tree[p.Entry]
	switch {
	case !held:
		return fmt.Errorf("the entry point %q is not in the tree, and a version is the commit the tree is", p.Entry)
	case !bytes.Equal(entry.Content, p.Document):
		return fmt.Errorf("the entry point %s differs from the file of the same path in the tree, and a version is one commit rather than two", p.Entry)
	}
	includes := make([]string, 0, len(p.Includes))
	for name := range p.Includes {
		includes = append(includes, name)
	}
	sort.Strings(includes)
	for _, name := range includes {
		f, held := p.Tree[name]
		switch {
		case !held:
			return fmt.Errorf("%s is included and is not in the tree, and a version is the commit the tree is", name)
		case !bytes.Equal(f.Content, p.Includes[name]):
			return fmt.Errorf("%s is included with other bytes than the tree holds at that path, and a version is one commit rather than two", name)
		}
	}
	return nil
}

// manifestOf names every file by the digest of its bytes, and answers each distinct blob once.
//
// Content addressed like everything else, so a file that did not change between two commits is
// one object and a version costs what changed, and two identical files in one tree are one blob.
func manifestOf(paths []string, files map[string]PushFile) ([]db.TreeFile, map[string][]byte) {
	tree := make([]db.TreeFile, 0, len(paths))
	blobs := map[string][]byte{}
	for _, p := range paths {
		f := files[p]
		sum := sha256.Sum256(f.Content)
		digest := hex.EncodeToString(sum[:])
		blobs[digest] = f.Content
		tree = append(tree, db.TreeFile{Path: p, SHA256: digest, Size: int64(len(f.Content)), Mode: f.Mode})
	}
	return tree, blobs
}

// storeTree writes blobs as objects of the namespace.
//
// An object already held is skipped, since its key is the digest of its bytes, unless again says
// the object is one a sweep had claimed: then being held now says nothing about being held in a
// minute, and the bytes are written whatever the store says.
func (s *Server) storeTree(ctx context.Context, namespace string, blobs map[string][]byte, again bool) error {
	digests := make([]string, 0, len(blobs))
	for digest := range blobs {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	for _, digest := range digests {
		key := artifact.Key(namespace, digest)
		if !again {
			held, err := s.objects.Has(ctx, key)
			if err != nil {
				return err
			}
			if held {
				continue
			}
		}
		if err := s.objects.Put(ctx, key, bytes.NewReader(blobs[digest])); err != nil {
			return err
		}
	}
	return nil
}

// checkTreePath refuses a path a container could not be given, and one that leaves the tree.
//
// The mount is /agk/repo, so a path escaping it is a path writing somewhere else on the host that
// prepares the directory. It is refused here rather than there because here is where somebody is
// watching.
func checkTreePath(p string) error {
	switch {
	case p == "":
		return errors.New("a tree file with no path")
	case p == ".":
		return errors.New("a tree file named ., which is the root of the repository and a directory rather than a file")
	case path.IsAbs(p):
		return fmt.Errorf("%s is absolute, and a tree path is relative to the root of the repository", p)
	case path.Clean(p) != p:
		return fmt.Errorf("%s is not in its cleaned form", p)
	case p == ".." || strings.HasPrefix(p, "../"):
		return fmt.Errorf("%s leaves the repository", p)
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("%q carries a null byte", p)
	}
	// Any segment spelt .git, in any case. A commit's tree never holds one, since git refuses
	// it, and one laid out under /agk/repo would be a repository configuration, hooks and all,
	// that any git a step runs there obeys. In any case because the filesystem a runner lays
	// the tree out on may fold it, and .GIT is .git on such a disk.
	for _, segment := range strings.Split(p, "/") {
		if strings.EqualFold(segment, ".git") {
			return fmt.Errorf("%s has a segment named .git, which is git's own and never part of a commit's tree", p)
		}
	}
	return nil
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
	return readAtMost(r, into, 8<<20)
}

// readAtMost is read with a limit of the caller's, for the one route whose body is larger than
// the rest. Past the limit the error wraps *http.MaxBytesError, which is how a caller tells a
// request that is too large from one that is malformed.
func readAtMost(r *http.Request, into any, limit int64) error {
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, limit))
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
