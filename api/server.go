package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

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
	rt.ServeRuns(runsIn{o.Pool})

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
		{"POST", "/api/v1/runs/{run}/cancel",
			OnRun{Permission: WorkflowRun}, s.cancel},
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
	// Includes are the files it pulls in. Together they are what the graph is rebuilt from,
	// with no repository and no object store in reach.
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

// TreeMaxBytes is the largest tree a push carries, counting its paths as well as its files.
//
// It is a limit of this push rather than a rule about repositories. The tree travels inline, in
// one JSON document and in base64, until the installation hosts the repository and a push is
// git's own smart HTTP, which v0.4.0 brings and which takes the limit away with the transport that
// needed it. Until then the whole request is held in memory on its way through, and four
// mebibytes of entry point, fragments and scripts is a great deal of workflow. A tree above it is
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
// read, so a tree of more is refused before its files are decoded.
const TreeMaxFiles = 4096

// TreeNameMaxBytes is the longest one segment of a tree path may be, which is NAME_MAX: 255 bytes is
// what the filesystems a runner lays a tree out on hold a name to. A longer name is a file no
// runner can create, and a version holding one is a version every run of which fails, so it is
// refused at the push rather than at each of them.
const TreeNameMaxBytes = 255

// TreePathMaxBytes is the longest a tree path may be.
//
// Linux holds a path to PATH_MAX, 4096 bytes with the null that ends it, and a tree is laid out
// below a directory twice over: the runner's own on the host, and /agk/repo in the container,
// where a step opens it by name. Half of PATH_MAX leaves the other half to whichever directory
// that is, and a workflow repository whose paths need more is not one anybody writes by hand.
const TreePathMaxBytes = 2048

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
		Tree: tree, Author: string(who), CreatedAt: s.now(),
	}
	// Built before it is written, so that a version that cannot be rebuilt is refused at the
	// push rather than discovered by the first run of it.
	if _, err := version.Build(v); err != nil {
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	otherTree := fmt.Sprintf("%s was already pushed at %s with other files, and a version is a commit: one commit names exactly one tree, permanently", over.Workflow, commit)

	// And compared with what is recorded under that commit, if anything is. Refused by
	// SaveVersion instead, a commit pushed again with other files had already put them in the
	// store, where nothing counts them, as often as anybody cared to push it.
	err = s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		return ns.CheckVersion(ctx, v)
	})
	if errors.Is(err, db.ErrOtherTree) {
		fail(w, http.StatusConflict, otherTree)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, "the version could not be read")
		return
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
	err = s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		if err := ns.SaveWorkflow(ctx, over.Workflow, p.Branch); err != nil {
			return err
		}
		var err error
		saved, err = ns.SaveVersion(ctx, v)
		return err
	})
	if errors.Is(err, db.ErrOtherTree) {
		// Two pushes of one commit that both compared before either recorded, and this one
		// lost. What it stored is uncounted, as it is for a push that dies before its row.
		fail(w, http.StatusConflict, otherTree)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, "the version could not be recorded")
		return
	}

	// And again for any object a sweep had claimed while this version was raising its
	// reference onto it: the reference is safe, and the bytes may be what the sweep is about
	// to delete. And for any this push skipped because the store held it and whose row the
	// version then had to create: a sweep can have collected it whole between the two, bytes
	// deleted and row confirmed gone, and the store's answer was about bytes that are not
	// there any more. The version's reference keeps any sweep away from it now.
	again := make(map[string][]byte, len(saved.MustWriteBytes))
	for _, digest := range saved.MustWriteBytes {
		again[digest] = blobs[digest]
	}
	for _, digest := range saved.Recorded {
		if skipped[digest] {
			again[digest] = blobs[digest]
		}
	}
	if _, err := s.storeTree(r.Context(), over.Namespace, again, true); err != nil {
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
	// Of at most TreeMaxFiles, which Push.field counts as the push is read.
	if len(files) == 0 {
		return nil, http.StatusBadRequest, errors.New("a push carries the tree of its commit and this one carries none: every step of every run sees the repository under /agk/repo, and a version without it would start containers on an empty directory")
	}
	paths := make([]string, 0, len(files))
	var total int64
	for p, f := range files {
		if err := CheckTreePath(p); err != nil {
			return nil, http.StatusBadRequest, err
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

// CheckTreePath refuses a path a container could not be given, and one that leaves the tree.
//
// The mount is /agk/repo, so a path escaping it is a path writing somewhere else on the host that
// prepares the directory. It is refused here rather than there because here is where somebody is
// watching. Exported because agk push applies it to every name of a commit before it reads a byte
// of the tree, so that a name the installation would refuse is refused before the tree is read
// and sent rather than after, and by the same rule rather than by a copy of it.
func CheckTreePath(p string) error {
	switch {
	case p == "":
		return errors.New("a tree file with no path")
	case len(p) > TreePathMaxBytes:
		return fmt.Errorf("%.64s... is a path of %d bytes, and a tree path is at most %d: laid out below a runner's directory and below /agk/repo it would be a path the host cannot name", p, len(p), TreePathMaxBytes)
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
	case !utf8.ValidString(p) || strings.ContainsRune(p, utf8.RuneError):
		// Git names a file with any bytes, and JSON carries UTF-8 only: a name that is not
		// arrives with U+FFFD where its bytes were, so the file would be laid out under a
		// name the commit does not give it, and two such names would arrive as one. The
		// character is what is left to see, and one that was really in a name cannot be told
		// from one that was not.
		return fmt.Errorf("%q holds U+FFFD, which is what JSON leaves where a name was not UTF-8, and a name that really holds one cannot be told from one that lost its bytes on the way: until the installation hosts the repository a push carries names without it", p)
	case strings.ContainsRune(p, '\\'):
		// A separator on Windows, where a\..\..\x is a path out of the tree and C:\x
		// and \\host\share are somewhere else entirely. A path that names one file on
		// one host and another on the next is not a path a version can promise.
		return fmt.Errorf("%q holds a backslash, which Windows reads as a separator: a tree path separates its directories with / alone, so that it names the same file on every host that lays it out", p)
	}
	for _, segment := range strings.Split(p, "/") {
		if len(segment) > TreeNameMaxBytes {
			return fmt.Errorf("%.64s... holds a name of %d bytes, and a filesystem holds a name to %d: no runner could create that file", p, len(segment), TreeNameMaxBytes)
		}
		// A segment that is .git, which a commit's tree never holds since git refuses it,
		// and which laid out under /agk/repo would be a repository configuration, hooks
		// and all, that any git a step runs there obeys.
		if dotGit(segment) {
			return fmt.Errorf("%q has a segment that is .git on some filesystem a tree is laid out on, and .git is git's own and never part of a commit's tree", p)
		}
	}
	return nil
}

// dotGit is whether a segment is .git on some filesystem a runner may lay a tree out on, which is
// the rule git applies itself, with core.protectNTFS and core.protectHFS, before it writes a name.
//
// A filesystem that folds case makes .GIT one. NTFS also drops the dots and spaces a name ends
// with, reads what follows a colon as a stream of the file before it, and gives .git the short
// name GIT~1, so .git., .git::$INDEX_ALLOCATION and GIT~1 are each .git there. HFS+ ignores a
// handful of invisible code points, so .g\u200cit is .git on it. On a Linux disk every one of
// these is an ordinary name, and a tree is laid out on whatever disk its runner has.
func dotGit(segment string) bool {
	s := strings.Map(func(r rune) rune {
		if hfsIgnores(r) {
			return -1
		}
		return r
	}, segment)
	if colon := strings.IndexByte(s, ':'); colon >= 0 {
		s = s[:colon]
	}
	s = strings.TrimRight(s, ". ")
	if strings.EqualFold(s, ".git") {
		return true
	}
	// GIT~1, and any other number, since which one NTFS gives depends on what the directory
	// held before.
	number, short := strings.CutPrefix(strings.ToLower(s), "git~")
	return short && number != "" && strings.Trim(number, "0123456789") == ""
}

// hfsIgnores is whether HFS+ leaves a code point out when it compares two names: the ones git's
// own is_hfs_dotgit skips.
func hfsIgnores(r rune) bool {
	switch {
	case r >= 0x200c && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x206a && r <= 0x206f, r == 0xfeff:
		return true
	}
	return false
}

// Start is a manual run: the inputs, and nothing else. What version it runs is the workflow's
// default branch resolved to a commit, which whoever pushed it named. It is what a client writes,
// and the API reads it as a starting.
type Start struct {
	Commit string         `json:"commit"`
	Inputs map[string]any `json:"inputs,omitempty"`
}

// starting is a Start as the API reads one, with its inputs kept as the JSON they were written in.
//
// Kept rather than decoded, because the API does nothing with them but write them down, and what
// decoding a document costs is set by how many values it holds rather than by its bytes: three
// bytes of {} are a map, and 8 MiB of inputs written [{},{},...] was 508 MiB once decoded. So
// they are counted, held to inputsMaxValues, and written to the run as they came.
type starting struct {
	commit string
	inputs jsontext.Value
}

// startMaxBytes is how large the body starting a run may be, which is what its inputs may weigh.
//
// The weight of one envelope, envelope_max_bytes at its default, because that is what they are:
// an input reaches a step as the envelope of a port it feeds, and the controller carries the
// inputs in the state it writes at every decision it takes on the run. The documentation lets the
// controller read an envelope's weight of payload and nothing larger, and a run started with more
// than that is one whose data belongs in an artifact.
const startMaxBytes = agk.DefaultEnvelopeMaxBytes

// inputsMaxValues is how many values the inputs of a run may hold, counting every object, array,
// string, number, boolean and null at any depth, and it is max_items at its default.
//
// Counted as well as weighed, because whoever decodes the inputs pays for their values rather than
// their bytes, a map for three bytes of {}, and the controller decodes them at every decision it
// takes on the run. An envelope is what the documentation lets the controller read, and max_items
// is how many items one carries, each of them several values: inputs held to that many values
// cost the controller no more than an envelope it may already read.
const inputsMaxValues = agk.DefaultMaxItems

func (s *starting) field(b *body, name string) error {
	switch name {
	case "commit":
		return text(b, &s.commit)
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
	if start.commit == "" {
		fail(w, http.StatusBadRequest, "a run is pinned to a commit and this one names none")
		return
	}

	g, err := s.versions.Graph(r.Context(), over.Namespace, over.Workflow, start.commit)
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
			ID: run, Workflow: over.Workflow, Commit: start.commit,
			Trigger: agk.TriggerManual, TriggeredBy: string(who),
			Inputs: json.RawMessage(start.inputs), Steps: g.Steps(),
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
// run state, per-step state, timings and log lines". operator holds the one and not the other, as
// does anybody denied run:read, and an answer saying how the run stood, or a status that changed
// once it had ended, would hand them what GET refuses them, at any moment and with no side effect
// on a run that has ended. Somebody holding both reads the state where run:read guards it.
//
// Asking again is asking once, and asking about a run that has ended changes nothing: "a
// principal asking twice, or asking about a run that finished while they were asking, has got
// what they wanted either way", as controller.Cancel puts it.
//
// Nothing is written to the audit log yet, since there is none: "manual trigger, approval,
// cancellation" are recorded there once #160 builds it, in the transaction that writes the
// request.
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
	var state agk.RunState
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		if state, err = ns.RequestCancel(ctx, run, s.now()); err != nil || state.Terminal() {
			return err
		}
		// In the same transaction, for the reason starting a run gives: the request and the
		// wake-up are one fact rather than two.
		return ns.NotifyRun(ctx, run)
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
