package api

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing/fstest"
	"time"
	"unicode/utf8"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/repo/store"
	"github.com/agentiik/agentiik/trigger"
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

	// publicURL is the address the installation is reached at, which a clone_url is made of.
	publicURL string

	// packs is where a repository's packs are kept, nil where the object store cannot read a
	// range of an object, which git's routes then answer 503 about.
	packs  *store.Store
	urls   artifact.Presigner
	limits agk.Limits
	now    func() time.Time

	// starter is the one path every run this server starts is created by.
	starter *trigger.Starter

	// router is the router the server's routes are on, which a webhook authenticating by a bearer
	// token asks who the token is and what it holds, as it asks for every guarded route; and hooks
	// seals and opens the secrets webhooks sign with, nil where no master key is attached.
	router *Router
	hooks  HookSecrets

	// logs tells the step log streams this server answers that their log moved on, streaming is
	// how they spend their time, and stopping ends them.
	logs      *logWatch
	streaming streamTiming
	stopping  <-chan struct{}
	trouble   func(error)
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

	// URLs mints the presigned URL an artifact with no fetch budget is redirected to. Without
	// one such an artifact is answered 503, for the reason a push with no object store is.
	URLs artifact.Presigner

	// Limits are what an output's envelope is read back under, and default to the
	// documentation's.
	Limits agk.Limits

	// Now is the clock, an argument so that a test has one.
	Now func() time.Time

	// Stopping ends every log stream open when it closes, without the event that says a step's
	// log is over, so that its reader reconnects, to another API where this one is going away,
	// and resumes there: "open log streams reconnect elsewhere and resume from their last
	// position". Without it a stop waits for streams that may never end on their own.
	Stopping <-chan struct{}

	// PublicURL is the address the installation is reached at, which a repository's clone_url is
	// made of: git clones <PublicURL>/<namespace>/<name>.git.
	PublicURL string

	// Hooks seals and opens the secrets webhooks sign with. Without it a secret written is refused
	// with 503, and a request to a webhook whose secret an installation holding a master key wrote is
	// answered 500 and said through Trouble, since what the installation holds it cannot open.
	Hooks HookSecrets

	// Trouble is where a log stream says that a chunk the API wrote could not be read back, which
	// its reader is shown as a gap and whoever runs the installation has to explain. One with
	// nowhere to put it drops it, as RunnerOptions.Trouble does.
	Trouble func(err error)
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
	if o.Limits == (agk.Limits{}) {
		o.Limits = agk.DefaultLimits()
	}
	s := &Server{
		pool: o.Pool, versions: o.Versions, objects: o.Objects, urls: o.URLs, limits: o.Limits, now: o.Now,
		logs: &logWatch{pool: o.Pool, sweep: defaultStreamTiming.sweep}, streaming: defaultStreamTiming, stopping: o.Stopping, trouble: o.Trouble,
		publicURL: o.PublicURL, router: rt, hooks: o.Hooks,
	}
	starter, err := trigger.New(trigger.Options{Pool: o.Pool, Versions: o.Versions, Objects: o.Objects, Report: s.report, Now: o.Now})
	if err != nil {
		return nil, err
	}
	s.starter = starter
	rt.ServeRuns(runsIn{o.Pool})
	if o.Objects != nil {
		// An object store that cannot read a range, which no installation's is, leaves the
		// repositories unserved rather than the API unstarted: the tree push works on it.
		if packs, err := store.New(o.Objects); err == nil {
			s.packs = packs
		}
	}
	if err := s.registerGit(rt); err != nil {
		return nil, err
	}
	if err := s.registerWorkflows(rt); err != nil {
		return nil, err
	}

	// A webhook, on whichever method its file declares, authenticated per trigger by its handler.
	if err := rt.Handle("", "/hooks/{namespace}/{path...}", Public{Why: webhooksWhy}, s.webhook); err != nil {
		return nil, err
	}

	for _, r := range []struct {
		method  string
		pattern string
		guard   Guard
		handler Handler
	}{
		// And secret:use where the version names a secret, and workflow:read on each workflow
		// it includes, which only the push can tell.
		{"PUT", "/api/v1/{namespace}/workflows/{workflow}/versions/{commit}",
			Needs{Permission: WorkflowWrite, Scope: Workflow, Also: SecretUse, Includes: true}, s.push},
		// What a repository's pushes are judged against beyond their tree, written under what
		// registering a version of it takes, and read under what reading it takes.
		{"GET", "/api/v1/{namespace}/workflows/{workflow}/images",
			Needs{Permission: WorkflowRead, Scope: Workflow}, s.listImages},
		{"POST", "/api/v1/{namespace}/workflows/{workflow}/images",
			Needs{Permission: WorkflowWrite, Scope: Workflow}, s.recordImages},
		{"POST", "/api/v1/{namespace}/workflows/{workflow}/runs",
			Needs{Permission: WorkflowRun, Scope: Workflow}, s.start},
		// What the default branch's head has armed, read under what reading the workflow takes.
		{"GET", "/api/v1/{namespace}/workflows/{workflow}/triggers",
			Needs{Permission: WorkflowRead, Scope: Workflow}, s.listTriggers},
		// What a webhook checks its caller against, written under what changing the workflow
		// takes, since whoever may push it may already say auth: none.
		{"PUT", "/api/v1/{namespace}/workflows/{workflow}/webhooks/{method}/{path...}",
			Needs{Permission: WorkflowWrite, Scope: Workflow}, s.writeHookCredential},
		// An event published into the namespace, under workflow:run held there: publishing starts
		// runs, and "a grant on one workflow does not count, since an event reaches every workflow
		// listening to the namespace".
		{"POST", "/api/v1/{namespace}/events",
			Needs{Permission: WorkflowRun, Scope: Namespace}, s.publish},
		// The run by the path a Location names it by, authorised over its own workflow
		// rather than over the namespace, so that run:read held on that workflow alone reads
		// it and a deny of run:read on that workflow refuses it: "a workflow-scope grant only
		// adds; only an explicit deny removes, and it wins over any allow at any scope".
		{"GET", "/api/v1/{namespace}/runs/{run}",
			OnRun{Permission: RunRead, Reveals: RunReadData}, s.detail},
		{"POST", "/api/v1/runs/{run}/cancel",
			OnRun{Permission: WorkflowRun}, s.cancel},
		{"POST", "/api/v1/runs/{run}/replay",
			OnRun{Permission: WorkflowRun}, s.replay},
		// The run by its identifier alone, which is all a push notification carries, read
		// exactly as the namespaced route reads it: the router found the namespace.
		{"GET", "/api/v1/runs/{run}",
			OnRun{Permission: RunRead, Reveals: RunReadData}, s.detail},
		{"GET", "/api/v1/runs/{run}/outputs/{name}",
			OnRun{Permission: RunReadData}, s.output},
		{"GET", "/api/v1/runs/{run}/steps/{step}/outputs/{port}",
			OnRun{Permission: RunReadData}, s.stepOutput},
		{"GET", "/api/v1/runs/{run}/steps/{step}/inputs/{port}",
			OnRun{Permission: RunReadData}, s.stepInput},
		{"GET", "/api/v1/runs/{run}/steps/{step}/logs",
			OnRun{Permission: RunRead}, s.stepLog},
		{"GET", "/api/v1/artifacts/{uri}",
			OnArtifact{Permission: RunReadData}, s.artifactOf},
	} {
		if err := rt.Handle(r.method, r.pattern, r.guard, r.handler); err != nil {
			return nil, err
		}
	}
	// One namespace's runs are the listing across the installation narrowed to it, asked about
	// each workflow for the reason the run is authorised over its own.
	for _, pattern := range []string{"/api/v1/{namespace}/runs", "/api/v1/runs"} {
		if err := rt.HandleAcross("GET", pattern, Across{Permission: RunRead}, s.across); err != nil {
			return nil, err
		}
	}
	// A namespace's runs as series count what its listing lists, and are asked about each workflow
	// the same way: an aggregate over runs discloses the runs.
	if err := rt.HandleAcross("GET", "/api/v1/{namespace}/stats/runs", Across{Permission: RunRead}, s.runStatistics); err != nil {
		return nil, err
	}
	// And one workflow's steps and what they published, the workflow named in the query and asked
	// about the same way.
	if err := rt.HandleAcross("GET", "/api/v1/{namespace}/stats/steps", Across{Permission: RunRead}, s.stepStatistics); err != nil {
		return nil, err
	}
	if err := rt.HandleAcross("GET", "/api/v1/{namespace}/stats/ports", Across{Permission: RunRead}, s.portStatistics); err != nil {
		return nil, err
	}
	return s, nil
}

// Push records one version of one workflow.
//
// PUT rather than POST, and the commit in the path rather than in the body, because "a version is
// a commit": pushing the same commit twice is the same version and has to be the same request.
type Push struct {
	// Entry is the path of the entry point in the tree, Document is what it holds, and
	// Includes are the files it pulls in. Together they are what the graph is rebuilt from,
	// with no repository and no object store in reach.
	Entry     string            `json:"entry"`
	Document  []byte            `json:"document"`
	Includes  map[string][]byte `json:"includes,omitempty"`
	Manifests map[string][]byte `json:"manifests,omitempty"`

	// Images are the references the workflow names by tag, each with the digest agk push
	// resolved it to, name@sha256:<hex>, which every run of the version names in its place.
	// A tag is resolved where the image is, on the machine that built or pulled it, because
	// the installation reaches no registry to do it with and a tag resolved at each run
	// would be whatever it pointed at that day.
	Images map[string]string `json:"images,omitempty"`

	// Tree is the commit's tree, every file of it, as every step will see it under /agk/repo.
	// It travels in the push because there is nowhere else it could come from yet: a version
	// is a commit, and until the installation hosts the repository itself it holds no copy
	// of that commit to read the files out of.
	Tree map[string]PushFile `json:"tree"`

	Parent string `json:"parent,omitempty"`
	Branch string `json:"branch,omitempty"`
}

func (p *Push) field(b *body, name string) error {
	switch name {
	case "entry":
		return text(b, &p.Entry)
	case "document":
		return b.bytes(&p.Document)
	case "includes":
		// Every include is a file of the tree as well, which checkAgreement holds it to, so
		// a push carrying more of them than a tree holds files is refused either way, and
		// here before they have been decoded.
		return files(b, &p.Includes, TreeMaxFiles, "the include", fmt.Sprintf("this push includes more files than the %d a tree holds, and every include is a file of the tree", TreeMaxFiles))
	case "manifests":
		// One per image the workflow names, and bounded as the files of the tree are: a
		// workflow is a few files naming a few images, one naming thousands is not a workflow
		// anybody reviews, and without a count a push of a million empty manifests cost a
		// million entries of a map before anything could refuse it.
		return files(b, &p.Manifests, TreeMaxFiles, "the manifest of", fmt.Sprintf("this push carries more image manifests than the %d it may, one per image the workflow names", TreeMaxFiles))
	case "images":
		// One per image the workflow names, and bounded as the manifests are.
		tooMany := fmt.Sprintf("this push resolves more images than the %d it may, one per image the workflow names by tag", TreeMaxFiles)
		return b.object(TreeMaxFiles, tooMany, func(ref string) error {
			if _, held := p.Images[ref]; held {
				return twice("the image", ref)
			}
			var pinned string
			if err := text(b, &pinned); err != nil {
				return err
			}
			if p.Images == nil {
				p.Images = map[string]string{}
			}
			p.Images[ref] = pinned
			return nil
		})
	case "tree":
		// Counted as the files arrive, so that a tree of too many is refused at the first
		// one past the limit rather than once every one of them is an entry of a map.
		tooMany := fmt.Sprintf("this tree has more files than the %d a push carries until the installation hosts the repository and a push is a git push: a tree of this many is usually carrying dependencies that belong in an image", TreeMaxFiles)
		return b.object(TreeMaxFiles, tooMany, func(path string) error {
			if _, held := p.Tree[path]; held {
				return twice("the tree file", path)
			}
			var f PushFile
			if err := b.fields(&f); err != nil {
				return err
			}
			if p.Tree == nil {
				p.Tree = map[string]PushFile{}
			}
			p.Tree[path] = f
			return nil
		})
	case "parent":
		return text(b, &p.Parent)
	case "branch":
		return text(b, &p.Branch)
	}
	return unknown(name)
}

// files reads an object of paths to bytes, as a push carries its includes and its manifests.
func files(b *body, into *map[string][]byte, most int, what, tooMany string) error {
	return b.object(most, tooMany, func(path string) error {
		if _, held := (*into)[path]; held {
			return twice(what, path)
		}
		var content []byte
		if err := b.bytes(&content); err != nil {
			return err
		}
		if *into == nil {
			*into = map[string][]byte{}
		}
		(*into)[path] = content
		return nil
	})
}

// PushFile is one file of the tree.
type PushFile struct {
	Content []byte `json:"content"`

	// Mode is git's, 0644 or 0755 where the file is executable, and it is always written.
	// Git tracks that one bit and a container needs it: an entry point that arrives 0644 is
	// a step that will not run, and a mode left to a default is a mode somebody guessed.
	Mode string `json:"mode"`
}

func (f *PushFile) field(b *body, name string) error {
	switch name {
	case "content":
		return b.bytes(&f.Content)
	case "mode":
		return text(b, &f.Mode)
	}
	return unknown(name)
}

// Pushed is what a push is answered with: the version, and the digest each image it names by tag
// is recorded with.
type Pushed struct {
	Namespace string `json:"namespace"`
	Workflow  string `json:"workflow"`
	Commit    string `json:"commit"`

	// Images are what the version records, which is not always what the push carried. A
	// commit pushed again after one of its tags moved is the version its first push
	// recorded, and every run of it names the digests that push resolved, so the answer
	// says which those are rather than leaving the pusher to believe the new ones were
	// taken.
	Images map[string]string `json:"images"`
}

// TreeMaxBytes is the largest tree a push carries, counting its paths as well as its files.
//
// It is a limit of this push rather than a rule about repositories. The tree travels inline, in
// one JSON document and in base64, the whole request held in memory on its way through; a push over
// git's own smart HTTP, which the installation serves from v0.4.0 and agk push speaks, carries no
// such limit, which stays with the transport that needed it. Four mebibytes of entry point,
// fragments and scripts is a great deal of workflow. A tree above it is
// usually carrying something that belongs in an image or in an artifact, and the refusal says so.
//
// The paths count because they are the part of a tree that is paid for again after the push:
// every redemption of every task of the version names every file, and a tree of long names and
// no content would otherwise cost nothing here and a great deal there.
const TreeMaxBytes = 4 << 20

// TreeMaxFiles is the most files a push carries, and a limit of the same push for the same reason.
//
// Bytes alone do not bound a tree: three hundred thousand empty files fit in a push, and each of
// them is an entry with a URL of a few hundred bytes in every redemption of every task that
// version runs, held in the API's memory while it is answered. At TreeMaxBytes this many files is
// a kibibyte each on average, which is smaller than a script usually is, so a workflow reaches it
// only by carrying a dependency tree, and that belongs in an image. It is counted as the push is
// read, so a tree of more is refused before its files are decoded. A git push holds each version it
// makes to it too, for the same reason, for as long as every task is sent every file's URL.
const TreeMaxFiles = 4096

// errGitHosts is a tree pushed as a new version of a repository git hosts.
var errGitHosts = errors.New("git hosts the repository")

// gitHosts answers errGitHosts where a git push has given the repository a branch or a tag.
func gitHosts(ctx context.Context, ns *db.NS, workflow string) error {
	hosted, err := ns.GitHosted(ctx, workflow)
	if err != nil {
		return err
	}
	if hosted {
		return errGitHosts
	}
	return nil
}

// hostedByGit is the refusal of a tree pushed as a new version of a repository git hosts. A commit
// already a version is still answered, as it always is, since that push makes nothing.
func hostedByGit(over Target) string {
	return fmt.Sprintf("git hosts %s/%s since a git push gave it a branch or a tag, and from then on a version is a commit pushed with git: a tree pushed here would be a version no ref reaches and no clone holds", over.Namespace, over.Workflow)
}

// commitName is a commit as a push names one, and a push is held to it before anything is written.
// Left to the table's own check, a commit that is not one was refused only by the insert, after
// the tree was already in the store with nothing counting it, and answered 500 for what was the
// caller's mistake.
//
// Whole, and never abbreviated, although the table holds seven characters and more. A version is
// recorded under exactly the name it was pushed as, so a3f9c1e and the forty characters it
// abbreviates were two versions, each free to hold its own tree: "one commit names exactly one
// tree" was checked against the name and not the commit, and anybody allowed to push could record
// another tree under the abbreviation of a commit somebody had reviewed. The whole hash is what
// git gives agk push anyway.
var commitName = regexp.MustCompile(`^[0-9a-f]{40}$`)

// workflowName is the grammar a workflow is named on, which is the one every name of the workflow
// file is written on, and a push is held to it and to agk.IdentifierMaxBytes before anything is
// written for the reason it is held to commitName: the table's domain refuses a name off either
// too, but only at the insert, after the tree was already in the store with nothing counting it,
// and answered 500 for what was the caller's mistake.
var workflowName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

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
		if errors.As(err, new(*http.MaxBytesError)) {
			fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a push is at most %d bytes, and this one is larger: the tree it carries is limited to %d until the installation hosts the repository and a push is a git push", pushMaxBytes, TreeMaxBytes))
			return
		}
		fail(w, statusOf(err), err.Error())
		return
	}
	commit := r.PathValue("commit")

	// Everything that can be refused without writing anything is refused first, so that a
	// push that fails leaves no object behind it.
	if len(over.Workflow) > agk.IdentifierMaxBytes {
		fail(w, http.StatusBadRequest, fmt.Sprintf("a workflow name is at most %d characters and this one is %d: one name has to survive a URL, a directory and a tool list unchanged, and no directory holds a longer one", agk.IdentifierMaxBytes, len(over.Workflow)))
		return
	}
	if !workflowName.MatchString(over.Workflow) {
		fail(w, http.StatusBadRequest, fmt.Sprintf("%q is not a workflow name: a workflow is named the way the workflow file names everything, letters, digits, hyphens and underscores beginning with a letter or a digit, so that one name survives a URL, a directory and a tool list unchanged", over.Workflow))
		return
	}
	if !commitName.MatchString(commit) {
		fail(w, http.StatusBadRequest, fmt.Sprintf("%q is not a commit, and a version is one: a version is pushed under the whole of its commit's hash, forty lowercase hexadecimal characters, since an abbreviation is a name another commit can come to share", commit))
		return
	}
	if p.Parent != "" && !commitName.MatchString(p.Parent) {
		fail(w, http.StatusBadRequest, fmt.Sprintf("the parent %q is not a commit: a parent is named by the whole of its hash, forty lowercase hexadecimal characters", p.Parent))
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
		Images: p.Images, Tree: tree, Author: string(who), CreatedAt: s.now(),
	}
	otherTree := fmt.Sprintf("%s was already pushed at %s with other files, and a version is a commit: one commit names exactly one tree, permanently", over.Workflow, commit)

	// Compared first with what is recorded under that commit, if anything is. Refused by
	// SaveVersion instead, a commit pushed again with other files had already put them in the
	// store, where nothing counts them, as often as anybody cared to push it. And a commit
	// recorded with these same files is that version already, which this push is answered as,
	// unchanged: it is not judged again by a rule added after it was stored, so that an agk of
	// the release that stored it, pushing it again after an upgrade, meets no refusal it did not
	// meet then. The rules that stood then, secret:use and the inputs' declaration, still apply.
	//
	// A commit that is not a version yet is refused once git hosts the repository, before
	// anything is written: from its first branch or tag on, a version is a commit a git push
	// carries, and a tree pushed here would be one no ref reaches and no clone holds.
	var stored bool
	err = s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		if err := ns.CheckVersion(ctx, v); err != nil {
			return err
		}
		_, err := ns.Version(ctx, over.Workflow, commit)
		stored = err == nil
		if !errors.Is(err, db.ErrNoVersion) {
			return err
		}
		return gitHosts(ctx, ns, over.Workflow)
	})
	if errors.Is(err, db.ErrOtherTree) {
		fail(w, http.StatusConflict, otherTree)
		return
	}
	if errors.Is(err, errGitHosts) {
		fail(w, http.StatusConflict, hostedByGit(over))
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, "the version could not be read")
		return
	}

	// Judged before it is written, by the one validation agk validate, agk push and a hook make,
	// so that a version that cannot be rebuilt is refused at the push rather than discovered by
	// the first run of it. That includes a tag no digest was resolved for, which is a push from
	// an agk that resolves none, and every rule a version is held to where it is made: a port or
	// a workflow output past agk.PortMaxBytes, a name the workflow's metadata gives other than
	// the one it is pushed under, and a secret the namespace does not declare. A commit already
	// stored is judged by the rules it was stored under and none added since.
	//
	// A version naming a secret is accepted only from a caller holding secret:use: "secret:use is
	// checked when a version is pushed, against whoever pushes it, and never when the workflow
	// runs". Writing a secret's name into a workflow is what sends its value into a container, so
	// whoever writes it answers for it, and whoever runs the version later needs workflow:run
	// alone. The permission comes from the namespace's grants only, and is asked over this
	// workflow so that a deny of it here refuses as a deny does anywhere. A 403 rather than the
	// 404 of a refused route, since the caller holds workflow:write here and learns nothing.
	// Every secret it names is then one the namespace declares, refused with 422 before anything
	// is stored rather than at the first redemption of a step naming it, and asked after
	// secret:use, so that only a caller allowed to write a secret's name into a workflow learns
	// whether the namespace declares it.
	checked, err := version.Check(r.Context(), pushedTree(p.Tree), version.Checking{
		Entry: p.Entry, Commit: commit, Committed: true,
		Namespace: over.Namespace, Repository: over.Workflow, Stored: stored,
		Resolvers: version.Resolvers{
			Pin:      pinnedBy(p),
			Manifest: manifestsCarried(p),
			Include:  s.includeOf(r),
			Secrets: func(ctx context.Context) ([]string, error) {
				var declared []db.Declaration
				err := s.pool.In(ctx, over.Namespace, func(ctx context.Context, ns *db.NS) error {
					var err error
					declared, err = ns.Declarations(ctx)
					return err
				})
				if err != nil {
					return nil, &pushFault{"the namespace's secret declarations could not be read", err}
				}
				names := make([]string, 0, len(declared))
				for _, d := range declared {
					names = append(names, d.Name)
				}
				return names, nil
			},
			SecretUse: func(ctx context.Context, _ []string) (bool, error) {
				held, err := HoldsAlso(r)(ctx)
				if err != nil {
					return false, &pushFault{"the push could not be authorised", err}
				}
				return held, nil
			},
		},
	})
	var fault *pushFault
	var unusable *version.SecretsNotUsable
	switch {
	case errors.As(err, &fault):
		s.report(fault.err)
		fail(w, http.StatusInternalServerError, fault.said)
		return
	case errors.As(err, &unusable):
		noun := "the secret"
		if len(unusable.Named) > 1 {
			noun = "the secrets"
		}
		fail(w, http.StatusForbidden, fmt.Sprintf("this version names %s %s, and a version naming a secret is accepted only from someone holding secret:use on %s, which a grant on the namespace %s gives and a deny on the workflow takes away, and you do not hold it there: whoever writes a secret's name into a workflow answers for its value going into a container, and running the version afterwards takes workflow:run alone", noun, strings.Join(unusable.Named, ", "), over.Workflow, over.Namespace))
		return
	case err != nil:
		fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("version: %s@%s: %s", over.Workflow, commit, err))
		return
	}
	// A digest for a tag no step names is a version whose file names one image while the push
	// says another, and its reviewers would be reading about something that never runs.
	for _, ref := range slices.Sorted(maps.Keys(p.Images)) {
		if _, named := checked.Version.Images[ref]; !named {
			fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("version: %s@%s records a digest for %s, which none of its steps names by a tag", over.Workflow, commit, ref))
			return
		}
	}
	// What is recorded is what the version was judged over: every file resolution read, the
	// manifest of every image a brick step runs and the digest every tag a step names was
	// resolved to, and nothing else the push carried, so that a rebuild reads what was accepted.
	v.Document, v.Includes, v.Manifests, v.Images = checked.Version.Document, checked.Version.Includes, checked.Version.Manifests, checked.Version.Images
	v.Library, v.Libraries = checked.Version.Library, checked.Version.Libraries

	// A new version records the pins it was judged with in its repository's store, where a git
	// push of the next commit finds them. A commit already stored is that version pushed again,
	// and records nothing: its pins may be long stale.
	var pins db.Images
	if !stored {
		pins = versionPins(v)
	}
	// The bytes before the row, so that a version that exists names objects that exist. A
	// push that dies between the two leaves objects nothing references, which the collector
	// never sees and which the next push of the same files reuses; the other order would
	// leave a version whose /agk/repo cannot be fetched.
	skipped, err := s.storeTree(r.Context(), over.Namespace, blobs, false)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the tree could not be stored")
		return
	}

	var saved db.Saved
	recorded := v.Images
	err = s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		if err := ns.SaveWorkflow(ctx, over.Workflow, p.Branch); err != nil {
			return err
		}
		// The repository's lock before the version's rows, in the order a git push takes them,
		// so that the two never wait on each other, and so that a git push giving the
		// repository its first ref cannot land between asking whether git hosts it and the
		// version's row.
		if err := ns.HoldRepository(ctx, over.Workflow); err != nil {
			return err
		}
		hosted := gitHosts(ctx, ns, over.Workflow)
		if hosted != nil && !errors.Is(hosted, errGitHosts) {
			return hosted
		}
		var err error
		if saved, err = ns.SaveVersion(ctx, v); err != nil {
			return err
		}
		if saved.New && hosted != nil {
			return hosted
		}
		if saved.New {
			// While no git push has given the repository a branch, the version a tree push
			// recorded last is what a run naming no ref runs, and what arms its triggers.
			if err := trigger.Reconcile(ctx, ns, over.Workflow, string(who), v.CreatedAt); err != nil {
				return err
			}
			if len(pins.Pins) == 0 {
				return nil
			}
			recorded, err := ns.RecordImages(ctx, over.Workflow, string(who), v.CreatedAt, pins)
			if err != nil {
				return err
			}
			return auditImages(ctx, ns, who, over.Workflow, recorded)
		}
		// The same tree pushed again, which is the version already recorded, and its
		// images are the ones its first push resolved rather than these.
		held, err := ns.Version(ctx, over.Workflow, commit)
		recorded = held.Images
		return err
	})
	if errors.Is(err, db.ErrOtherTree) {
		// Two pushes of one commit that both compared before either recorded, and this one
		// lost. What it stored is uncounted, as it is for a push that dies before its row.
		fail(w, http.StatusConflict, otherTree)
		return
	}
	if errors.Is(err, errGitHosts) {
		// A git push gave the repository its first ref after this one looked. What it stored
		// is uncounted, as it is for a push that dies before its row.
		fail(w, http.StatusConflict, hostedByGit(over))
		return
	}
	if errors.Is(err, db.ErrWorkflowMoving) {
		fail(w, http.StatusConflict, movingSentence(over))
		return
	}
	if errors.As(err, new(*db.HookTaken)) {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, "the version could not be recorded")
		return
	}

	// And again for any object a sweep had claimed while this version was raising its
	// reference onto it: the reference is safe, and the bytes may be what the sweep is about
	// to delete. And for any whose row the version then had to create, where this push skipped
	// it because the store held it, or the store does not hold it now: a sweep can have
	// collected it whole between the two, bytes deleted and row confirmed gone, and the store's
	// answer was about bytes that are not there any more; and one that died after deleting the
	// bytes and before confirming leaves a row the next sweep deletes again, with the bytes this
	// push wrote meanwhile. The version's reference keeps any sweep away from it now, so the
	// store's answer from here on is one to trust.
	again := make(map[string][]byte, len(saved.MustWriteBytes))
	for _, digest := range saved.MustWriteBytes {
		again[digest] = blobs[digest]
	}
	for _, digest := range saved.Recorded {
		held := !skipped[digest]
		if held {
			var err error
			if held, err = s.objects.Has(r.Context(), artifact.Key(over.Namespace, digest)); err != nil {
				fail(w, http.StatusInternalServerError, "the tree could not be stored")
				return
			}
		}
		if !held {
			again[digest] = blobs[digest]
		}
	}
	if _, err := s.storeTree(r.Context(), over.Namespace, again, true); err != nil {
		fail(w, http.StatusInternalServerError, "the tree could not be stored")
		return
	}

	if recorded == nil {
		recorded = map[string]string{}
	}
	write(w, http.StatusOK, Pushed{Namespace: over.Namespace, Workflow: over.Workflow, Commit: commit, Images: recorded})
}

// pushFault is a store the push had to ask that could not answer, which is the installation's
// failure and not the pusher's: said answers it, and err is what is reported.
type pushFault struct {
	said string
	err  error
}

func (f *pushFault) Error() string { return f.said + ": " + f.err.Error() }

// pinnedBy answers the digest a push resolved a tag to, which is what the installation holds for
// it until a namespace keeps its own pins.
func pinnedBy(p Push) func(context.Context, string, agk.Step) (string, error) {
	return func(_ context.Context, reference string, _ agk.Step) (string, error) {
		if digest, ok := p.Images[reference]; ok {
			return digest, nil
		}
		return "", version.ErrNotHeld
	}
}

// manifestsCarried answers the manifest a push carries for an image, by the digest a step now
// names it by: the push keys each by the reference the workflow writes, which is a tag where the
// push resolved one. Two tags resolved to one digest are one image, and a push carrying two
// manifests for it is refused rather than one of them chosen.
func manifestsCarried(p Push) func(context.Context, string, agk.Step) ([]byte, error) {
	return func(_ context.Context, image string, _ agk.Step) ([]byte, error) {
		var body []byte
		for _, ref := range slices.Sorted(maps.Keys(p.Manifests)) {
			key := ref
			if pinned, held := p.Images[ref]; held {
				key = pinned
			}
			if key != image {
				continue
			}
			if body != nil && !bytes.Equal(body, p.Manifests[ref]) {
				return nil, fmt.Errorf("the push carries two manifests for %s, one image under two tags", image)
			}
			body = p.Manifests[ref]
		}
		if body == nil {
			return nil, version.ErrNotHeld
		}
		return body, nil
	}
}

// pushedTree is the tree a push carries, as version.Check judges it and an input's schema resolves
// a reference against.
func pushedTree(files map[string]PushFile) version.Files {
	tree := make(version.Files, len(files))
	for p, f := range files {
		tree[p] = &fstest.MapFile{Data: f.Content, Mode: 0o444}
	}
	return tree
}

// checkTree refuses a tree that could not be laid out under /agk/repo, and answers its paths in
// order along with the status a refusal is answered with.
//
// Sorted, because two pushes of one commit have to produce the same version and a map has no
// order.
func checkTree(files map[string]PushFile) ([]string, int, error) {
	// Of at most TreeMaxFiles, which Push.field counts as the push is read.
	if len(files) == 0 {
		return nil, http.StatusBadRequest, errors.New("a push carries the tree of its commit and this one carries none: every step of every run sees the repository under /agk/repo, and a version without it would start containers on an empty directory")
	}
	paths := make([]string, 0, len(files))
	var total int64
	for p, f := range files {
		if err := version.TreePath(p); err != nil {
			return nil, http.StatusBadRequest, err
		}
		if strings.ContainsRune(p, utf8.RuneError) {
			// JSON leaves U+FFFD where a name was not UTF-8, and a name that really holds
			// one cannot be told from one that lost its bytes on the way: until the
			// installation hosts the repository a push carries names without it.
			return nil, http.StatusBadRequest, fmt.Errorf("%q holds U+FFFD, which is what JSON leaves where a name was not UTF-8, and a name that really holds one cannot be told from one that lost its bytes on the way: until the installation hosts the repository a push carries names without it", p)
		}
		if f.Mode != "0644" && f.Mode != "0755" {
			return nil, http.StatusBadRequest, fmt.Errorf("%s is pushed with mode %q, and a tree carries git's two, written out: 0644, or 0755 where the file is executable", p, f.Mode)
		}
		total += int64(len(p) + len(f.Content))
		paths = append(paths, p)
	}
	if total > TreeMaxBytes {
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("this tree is %d bytes with its paths and a push carries at most %d until the installation hosts the repository and a push is a git push: a tree this size is usually carrying something that belongs in an image or in an artifact", total, TreeMaxBytes)
	}
	sort.Strings(paths)

	// A path that is a file and also the directory of another cannot be laid out: one of the
	// two would have to lose, and which one would depend on the order the runner wrote them.
	//
	// Found by searching the sorted paths for each one with a slash after it, rather than by
	// looking every directory of every path up among the files. That was a hash of the whole
	// prefix at each slash, so a single path of a few mebibytes, most of them slashes, cost the
	// square of its length: under a minute of processor for one request, from anybody allowed
	// to push. The files below p sort together, immediately at or after p+"/", so one search
	// finds the first of them if there is any.
	for _, p := range paths {
		below := p + "/"
		if i := sort.SearchStrings(paths, below); i < len(paths) && strings.HasPrefix(paths[i], below) {
			return nil, http.StatusBadRequest, fmt.Errorf("%s is both a file and the directory %s is in, and a tree laid out on a disk can hold only one of the two", p, paths[i])
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

// storeTree writes blobs as objects of the namespace, and answers the digests it did not write.
//
// An object already held is skipped, since its key is the digest of its bytes, unless again says
// the object is one a sweep may have taken: then being held now says nothing about being held in
// a minute, and the bytes are written whatever the store says. What was skipped is answered
// because the store's word is only as good as the moment it was given, and the push asks again
// for any of them whose row the version had to create.
func (s *Server) storeTree(ctx context.Context, namespace string, blobs map[string][]byte, again bool) (map[string]bool, error) {
	digests := make([]string, 0, len(blobs))
	for digest := range blobs {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	skipped := map[string]bool{}
	for _, digest := range digests {
		key := artifact.Key(namespace, digest)
		if !again {
			held, err := s.objects.Has(ctx, key)
			if err != nil {
				return nil, err
			}
			if held {
				skipped[digest] = true
				continue
			}
		}
		if err := s.objects.Put(ctx, key, bytes.NewReader(blobs[digest])); err != nil {
			return nil, err
		}
	}
	return skipped, nil
}

// Start is a manual run: the inputs, and nothing else. What version it runs is the workflow's
// default branch resolved to a commit, which whoever pushed it named. It is what a client writes,
// and the API reads it as a starting.
type Start struct {
	Commit string         `json:"commit,omitempty"`
	Ref    string         `json:"ref,omitempty"`
	Inputs map[string]any `json:"inputs,omitempty"`
}

// starting is a Start as the API reads one, with its inputs kept as the JSON they were written in
// until they are bound.
//
// Counted before anything decodes them, because what decoding a document costs is set by how many
// values it holds rather than by its bytes: three bytes of {} are a map, and 8 MiB of inputs
// written [{},{},...] was 508 MiB once decoded. So they are held to inputsMaxValues as they are
// read, and only then decoded to be bound against the version's declaration.
type starting struct {
	commit, ref string
	inputs      jsontext.Value
}

// startMaxBytes is how large the body starting a run may be, which is what its inputs may weigh:
// one envelope, as package trigger holds the inputs it binds.
const startMaxBytes = trigger.InputsMaxBytes

// inputsMaxValues is how many values the inputs of a run may hold, counted as they are read so that
// a document that is not worth decoding is refused before it is: max_items at its default, as
// package trigger holds the inputs it binds.
const inputsMaxValues = trigger.InputsMaxValues

func (s *starting) field(b *body, name string) error {
	switch name {
	case "commit":
		return text(b, &s.commit)
	case "ref":
		return text(b, &s.ref)
	case "inputs":
		// An object naming each input, told apart before it is read, so that a document that is
		// not one is refused before it is counted rather than after.
		switch k := b.d.PeekKind(); k {
		case jsontext.KindNull:
			_, err := b.d.ReadToken()
			return malformed(err)
		case jsontext.KindBeginObject:
		default:
			b.d.SkipValue()
			return b.mistyped(k, "an object naming each input")
		}
		var err error
		s.inputs, err = b.document(inputsMaxValues, fmt.Sprintf("the inputs hold more than the %d values a run's inputs may hold, as many as one envelope may carry items: a run's data belongs in an artifact", inputsMaxValues))
		return err
	}
	return unknown(name)
}

func (s *Server) start(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	var start starting
	if err := readAtMost(r, &start, startMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	supplied, err := decodeInputs(start.inputs)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// Manual: "started from the API, the command line, the console or a mobile app by a principal
	// holding workflow:run", which the route asked, with the inputs supplied explicitly. A ref other
	// than the default branch is resolved once, when the run is asked for.
	started, err := s.starter.Start(r.Context(), trigger.Request{
		Namespace: over.Namespace, Workflow: over.Workflow,
		Kind: agk.TriggerManual, By: string(who),
		Commit: start.commit, Ref: start.ref, Inputs: supplied,
	})
	if s.refused(w, over, cmp.Or(started.Commit, start.commit, start.ref), err) {
		return
	}
	run, commit := started.Run, started.Commit

	// 202 rather than 201: the run exists, and nothing has happened yet. What happens is the
	// controller's, and it has been told.
	w.Header().Set("Location", fmt.Sprintf("/api/v1/%s/runs/%s", over.Namespace, run))
	// The commit too, since a run asked for by a ref, or by none, is pinned to one its caller
	// did not name.
	write(w, http.StatusAccepted, map[string]any{
		"run": string(run), "state": agk.Queued.String(), "commit": commit,
	})
}

// detail answers one run: "run state, per-step state, envelope digests" to whoever holds
// run:read, and the inputs it was started with only to whoever also holds run:read_data.
//
// The inputs are what the run's first steps are handed as the envelopes of the ports they feed,
// which makes them "envelope contents", and seeing those is what run:read_data is, "not only state
// and digests". So they are left out of the answer rather than blanked, as the console hides a
// payload pane rather than disabling it: a caller who may not see them is answered a run that
// names no inputs, which is also how a run started with none reads. The outputs stay, since what
// the run records of each is the step and the port it is a view of, and the envelope behind it is
// GET /api/v1/runs/{id}/outputs/{name}, guarded by run:read_data of its own.
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
	// Asked about the run's own workflow, which the namespaced route was not authorised over, so
	// that a deny of run:read_data on that workflow is in the answer; and asked once the
	// transaction is over, since an authorizer may read the database itself.
	seeing, err := Revealing(r)(r.Context(), Target{Namespace: over.Namespace, Workflow: detail.Workflow})
	if err != nil {
		refuse(w, http.StatusInternalServerError, "the request could not be authorised")
		return
	}
	if !seeing {
		detail.Inputs = nil
		for i := range detail.Tasks {
			detail.Tasks[i].Params = nil
		}
	}
	// What the version the run pinned declares of each step, from the graph the store keeps of
	// it. A version that can no longer be built leaves them out rather than the run unread.
	if s.versions != nil {
		if g, err := s.versions.Graph(r.Context(), over.Namespace, detail.Workflow, detail.Commit); err == nil {
			declared(&detail, g)
		}
	}
	write(w, http.StatusOK, detail)
}

// declared writes onto each step of a run what its version declares of it: the image it runs, by
// digest, none for a workflow step, which runs no container; the input ports its edges and its
// inputs keyword feed, sorted; and its output ports, in the order the file writes them.
func declared(d *db.RunDetail, g *graph.Graph) {
	for i, st := range d.Steps {
		step, ok := g.Step(st.Step)
		if !ok {
			continue
		}
		if step.Call == nil {
			d.Steps[i].Image = step.Image
		}
		in := map[agk.Port]bool{}
		for _, e := range step.Needs {
			in[e.As] = true
		}
		for p := range step.Inputs {
			in[p] = true
		}
		d.Steps[i].InputPorts = slices.Sorted(maps.Keys(in))
		d.Steps[i].OutputPorts = slices.Clone(step.Outputs)
	}
}

// cancel asks for a run to be cancelled, and answers that it was asked.
//
// "Cancels pending tasks and sends SIGTERM to running containers." Neither happens here. The API
// writes the request on the run and notifies, in one transaction as it does for a run it starts,
// and the controller, which reads it there, ends the run and stops what it holds: the two share
// the database and nothing else, and a route that stopped a container would be a second
// controller. So the answer is 202, pointing at the run.
//
// It says nothing of how the run stands, and its status is the same whether the run is going or
// has ended. The route is guarded by workflow:run, and a run's state is what run:read guards: "See
// run state, per-step state, timings and log lines". A token narrowed to workflow:run holds the one
// and not the other, as does anybody denied run:read, and an answer saying how the run stood, or a
// status that changed once it had ended, would hand them what GET refuses them, at any moment and
// with no side effect on a run that has ended. Somebody holding both reads the state where run:read
// guards it.
//
// Asking again is asking once, and asking about a run that has ended changes nothing: "a
// principal asking twice, or asking about a run that finished while they were asking, has got
// what they wanted either way", as controller.Cancel puts it.
//
// The request is recorded in the audit log in the transaction that writes it, a request about a
// run that has ended as well, whose entry says it changed nothing: who asked is part of what
// happened either way.
func (s *Server) cancel(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	// Nothing to say beyond which run, which the path names, so no body is the ordinary
	// request. One carrying a field nobody knows, a reason for one, is refused rather than
	// half understood, however it was sent.
	if err := readIfAny(r, nothingAsked{}, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}

	// The run the router found, in the namespace and of the workflow it authorised.
	run := agk.RunID(r.PathValue("run"))
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		state, first, err := ns.RequestCancel(ctx, run, s.now())
		if err != nil {
			return err
		}
		if !state.Terminal() {
			// In the same transaction, for the reason starting a run gives: the request and
			// the wake-up are one fact rather than two.
			if err := ns.NotifyRun(ctx, run); err != nil {
				return err
			}
		}
		// Only the first request changes anything: the moment is the first one's, and a run
		// that has ended stays ended.
		result := audit.Unchanged
		if first {
			result = audit.Done
		}
		return ns.Audit(ctx, audit.Record{
			Actor: string(who), Action: audit.RunCancel, Target: string(run), Result: result,
			Detail: map[string]any{"workflow": over.Workflow},
		})
	})
	if errors.Is(err, db.ErrNoRun) {
		// Found by the router a moment ago and not there now, as a run is once its workflow
		// has been deleted in between, and answered as the absence it is.
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, "the run could not be asked to cancel")
		return
	}

	w.Header().Set("Location", fmt.Sprintf("/api/v1/%s/runs/%s", over.Namespace, run))
	write(w, http.StatusAccepted, map[string]any{"run": string(run)})
}

// runsIn finds the namespace and workflow of a run for the router, from its identifier alone.
//
// Across the installation, for the one reason db.RunRoute names: the path of a route about a run
// names nothing else, and what is found goes to the authorizer and nowhere else. The handler then
// reads and writes through In, in the namespace that was authorised.
type runsIn struct{ pool *db.Pool }

func (f runsIn) RunOf(ctx context.Context, run string) (Target, error) {
	var of Target
	err := f.pool.Installation(ctx, db.RunRoute, func(ctx context.Context, w *db.Wide) error {
		var err error
		of.Namespace, of.Workflow, err = w.Locate(ctx, agk.RunID(run))
		return err
	})
	if errors.Is(err, db.ErrNoRun) {
		return Target{}, ErrNoRun
	}
	return of, err
}

func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// report says one thing, through whatever Trouble was given.
func (s *Server) report(err error) {
	if s.trouble != nil {
		s.trouble(err)
	}
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
