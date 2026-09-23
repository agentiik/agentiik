package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// A workflow and its versions.
//
// "A version is a commit. finance/monthly-invoicing@a3f9c1e names exactly one tree, permanently,
// because that is what a commit already is." So a version is written once and never changed, and
// what is written is what it takes to rebuild it: a branch that moves afterwards changes nothing
// about a run pinned to that commit.

// Version is one commit of one workflow, as the database holds it.
type Version struct {
	Namespace string
	Workflow  string
	Commit    string
	Parent    string

	// Entry is the path of the entry point in the tree, Document is what it held, and
	// Includes are the files it pulled in, by the path each was named at. Manifests are the
	// brick manifests of every image the workflow names, by image reference.
	//
	// Together these are the version: loading them back gives the same workflow resolved the
	// same way, with nothing fetched from anywhere.
	Entry     string
	Document  []byte
	Includes  map[string][]byte
	Manifests map[string][]byte

	// Tree is the repository as the runner will see it, named rather than carried: "every
	// step of every run sees it, mounted read-only at /agk/repo". The bytes are objects in
	// the namespace's store, addressed by digest like everything else, so a file that did
	// not change between two commits is one object and a version costs what changed.
	//
	// Deliberately not the same thing as Includes, and held in a column of its own rather
	// than beside them. Those are what it takes to rebuild the graph with nothing in reach,
	// which has to keep working when the object store is unreachable. This is what a
	// container is given, and nothing rebuilds a graph from it.
	Tree []TreeFile

	Author    string
	CreatedAt time.Time
}

// TreeFile is one file of the repository, at the path the container sees it under /agk/repo.
//
// The field names are the wire's: a redemption answers path, mode and sha256 for each file, and a
// stored shape that spelt the digest differently would be a translation somebody has to remember.
type TreeFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`

	// Mode is git's, 0644 or 0755 where the file is executable, and it is always written.
	// Git tracks that one bit and a container needs it: an entry point that arrives 0644 is
	// a step that will not run.
	Mode string `json:"mode"`
}

// treeMediaType is what a file of the tree is recorded as on its object. A file is bytes, and
// what its name says it is belongs to whoever reads it rather than to the store.
const treeMediaType = "application/octet-stream"

// stored is the shape the graph column holds. Written out rather than reusing Version so that
// adding a field to the Go type is a decision about the stored shape rather than a silent change
// to it.
type stored struct {
	Entry     string            `json:"entry"`
	Document  []byte            `json:"document"`
	Includes  map[string][]byte `json:"includes,omitempty"`
	Manifests map[string][]byte `json:"manifests,omitempty"`
}

// ErrNoVersion is nothing of that commit.
var ErrNoVersion = errors.New("db: no version of that workflow at that commit")

// ErrNoTree is a version recorded without its tree.
//
// Separate from ErrNoVersion because the version is real and runs can be decided from it: what is
// missing is the directory a container is given, and answering that with an empty one would start
// a step on something that looks like a repository and is not one.
var ErrNoTree = errors.New("db: that version was recorded without its tree")

// ErrOtherTree is a commit already recorded with a different tree.
//
// "A version is a commit", and a commit names exactly one tree, so the same commit arriving with
// other files is a rewritten history or a push that says it is something it is not. Neither is a
// reason to change what a run pinned to that commit is already using.
var ErrOtherTree = errors.New("db: that commit is already recorded with another tree")

// Saved is what recording a version settled.
type Saved struct {
	// New is false where this commit was already recorded with this tree, which is the same
	// version pushed again and changes nothing.
	New bool

	// MustWriteBytes are the digests of tree files whose objects a sweep had claimed when this
	// version raised its references onto them. The references are safe either way; what is
	// not safe is assuming the bytes survived a sweep that had already decided to delete them,
	// so the caller writes those again. See the collection protocol in purge.go.
	MustWriteBytes []string
}

// SaveWorkflow records a workflow, or leaves the one that is there.
//
// Idempotent, because pushing a second version of a workflow that already exists is the ordinary
// case and is not a request to change anything about the workflow itself.
func (n *NS) SaveWorkflow(ctx context.Context, name, branch string) error {
	if branch == "" {
		branch = "main"
	}
	if _, err := n.tx.Exec(ctx,
		`insert into workflows (namespace, name, default_branch) values ($1, $2, $3)
		 on conflict (namespace, name) do nothing`,
		n.namespace, name, branch); err != nil {
		return fmt.Errorf("db: workflow %s could not be recorded: %w", name, err)
	}
	return nil
}

// SaveVersion records one commit of one workflow, and keeps the objects its tree names alive.
//
// A commit already written is left exactly as it was, and that is the point rather than a
// convenience: "a version is a commit", so the same commit pushed twice is the same version, and
// a second push that overwrote it would make a run pinned to that commit mean something else than
// it did when it started. The same commit with another tree is ErrOtherTree rather than silence,
// because it is the one case where leaving the row alone would tell the pusher their files were
// recorded when they were not.
//
// A version that is new raises one reference per distinct digest its tree names, in this
// transaction, which is what keeps the collector off those objects for as long as the version
// exists. A run pins its commit, "and neither does a replay of it six months on", which would mean
// nothing if the files under /agk/repo could be collected in between. The reference is raised by
// the same code an artifact and an envelope are counted by, so the three cannot disagree about
// what an object is kept for.
func (n *NS) SaveVersion(ctx context.Context, v Version) (Saved, error) {
	switch {
	case v.Workflow == "" || v.Commit == "":
		return Saved{}, fmt.Errorf("db: a version of %q at %q", v.Workflow, v.Commit)
	case v.Entry == "" || len(v.Document) == 0:
		return Saved{}, fmt.Errorf("db: version %s@%s carries no entry point, and what is stored is what it takes to rebuild it", v.Workflow, v.Commit)
	case v.Author == "":
		return Saved{}, fmt.Errorf("db: version %s@%s has no author", v.Workflow, v.Commit)
	}

	tree, err := sortedTree(v.Tree)
	if err != nil {
		return Saved{}, fmt.Errorf("db: version %s@%s: %w", v.Workflow, v.Commit, err)
	}
	body, err := json.Marshal(stored{
		Entry: v.Entry, Document: v.Document,
		Includes: v.Includes, Manifests: v.Manifests,
	})
	if err != nil {
		return Saved{}, fmt.Errorf("db: version %s@%s could not be written: %w", v.Workflow, v.Commit, err)
	}
	// A nil tree is null in the column rather than an empty list: an empty list would be a
	// commit with no files, and a version that said nothing about its files is not that.
	var column any
	if tree != nil {
		encoded, err := json.Marshal(tree)
		if err != nil {
			return Saved{}, fmt.Errorf("db: the tree of %s@%s could not be written: %w", v.Workflow, v.Commit, err)
		}
		column = encoded
	}
	created := v.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	tag, err := n.tx.Exec(ctx,
		`insert into workflow_versions (namespace, workflow, commit, parent, graph, tree, author, created_at)
		 values ($1, $2, $3, $4, $5, $6, $7, $8)
		 on conflict (namespace, workflow, commit) do nothing`,
		n.namespace, v.Workflow, v.Commit, nilIfEmpty(v.Parent), body, column, v.Author, created)
	if err != nil {
		return Saved{}, fmt.Errorf("db: version %s@%s could not be recorded: %w", v.Workflow, v.Commit, err)
	}

	if tag.RowsAffected() == 0 {
		// Already there. Compared by tree and by nothing else: the tree is the commit, and
		// the rest of the row is either read out of it or resolved around it, like the
		// manifests of the images it names, which may have moved since without the commit
		// having changed.
		held, err := readTree(ctx, n.tx, n.namespace, v.Workflow, v.Commit)
		if err != nil && !errors.Is(err, ErrNoTree) {
			return Saved{}, err
		}
		if !slices.Equal(held, tree) {
			return Saved{}, fmt.Errorf("%w: %s/%s@%s", ErrOtherTree, n.namespace, v.Workflow, v.Commit)
		}
		return Saved{}, nil
	}

	out := Saved{New: true}
	// One reference per distinct digest, in digest order. Per digest because a tree holding
	// two identical files names one object, and in order because each raise locks the row it
	// counts: two pushes sharing objects and locking them in two orders would deadlock.
	sizes := map[string]int64{}
	for _, f := range tree {
		sizes[f.SHA256] = f.Size
	}
	digests := make([]string, 0, len(sizes))
	for digest := range sizes {
		digests = append(digests, digest)
	}
	slices.Sort(digests)
	for _, digest := range digests {
		again, err := raise(ctx, n.tx, n.namespace, "sha256:"+digest, sizes[digest], treeMediaType)
		if err != nil {
			return Saved{}, err
		}
		if again {
			out.MustWriteBytes = append(out.MustWriteBytes, digest)
		}
	}
	return out, nil
}

// sortedTree checks a tree and answers a copy in path order.
//
// Sorted here as well as by whoever built it, because two pushes of one commit are compared by
// what they stored, and a tree whose order depended on the caller would make the same files a
// different tree.
func sortedTree(tree []TreeFile) ([]TreeFile, error) {
	if tree == nil {
		return nil, nil
	}
	out := slices.Clone(tree)
	slices.SortFunc(out, func(a, b TreeFile) int { return strings.Compare(a.Path, b.Path) })
	for i, f := range out {
		switch {
		case f.Path == "":
			return nil, errors.New("a tree file with no path")
		case i > 0 && out[i-1].Path == f.Path:
			return nil, fmt.Errorf("the tree names %s twice", f.Path)
		case !hexDigest.MatchString(f.SHA256):
			return nil, fmt.Errorf("%s is named by %q, and a digest is sixty-four lowercase hexadecimal characters", f.Path, f.SHA256)
		case f.Size < 0:
			return nil, fmt.Errorf("%s is %d bytes", f.Path, f.Size)
		case f.Mode != "0644" && f.Mode != "0755":
			return nil, fmt.Errorf("%s has mode %q, and a tree carries git's two: 0644 and 0755", f.Path, f.Mode)
		}
	}
	return out, nil
}

// Version reads one back, for a namespaced caller.
func (n *NS) Version(ctx context.Context, workflow, commit string) (Version, error) {
	return readVersion(ctx, n.tx, n.namespace, workflow, commit)
}

// Version reads one back, for the controller, which serves every namespace.
func (w *Wide) Version(ctx context.Context, namespace, workflow, commit string) (Version, error) {
	return readVersion(ctx, w.tx, namespace, workflow, commit)
}

// Tree reads the tree of one version and nothing else, for a redemption.
//
// Through the installation door because a runner works for several namespaces and learns which
// one a task belongs to from the grant, and narrowed to the one column because the redemption has
// no use for the rest: the document a graph is rebuilt from is the controller's business. It
// answers ErrNoVersion for a commit nobody recorded and ErrNoTree for one recorded without its
// tree, so that neither becomes an empty directory.
func (w *Wide) Tree(ctx context.Context, namespace, workflow, commit string) ([]TreeFile, error) {
	return readTree(ctx, w.tx, namespace, workflow, commit)
}

func readTree(ctx context.Context, tx pgx.Tx, namespace, workflow, commit string) ([]TreeFile, error) {
	var raw []byte
	err := tx.QueryRow(ctx,
		`select tree from workflow_versions
		 where namespace = $1 and workflow = $2 and commit = $3`,
		namespace, workflow, commit).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s/%s@%s", ErrNoVersion, namespace, workflow, commit)
	}
	if err != nil {
		return nil, fmt.Errorf("db: the tree of %s@%s could not be read: %w", workflow, commit, err)
	}
	return decodeTree(raw, namespace, workflow, commit)
}

// decodeTree turns the column back into files, and a null into ErrNoTree.
func decodeTree(raw []byte, namespace, workflow, commit string) ([]TreeFile, error) {
	if raw == nil {
		return nil, fmt.Errorf("%w: %s/%s@%s", ErrNoTree, namespace, workflow, commit)
	}
	var tree []TreeFile
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("db: the tree of %s@%s could not be read: %w", workflow, commit, err)
	}
	if tree == nil {
		tree = []TreeFile{}
	}
	return tree, nil
}

func readVersion(ctx context.Context, tx pgx.Tx, namespace, workflow, commit string) (Version, error) {
	v := Version{Namespace: namespace, Workflow: workflow, Commit: commit}
	var body, tree []byte
	var parent *string
	err := tx.QueryRow(ctx,
		`select parent, graph, tree, author, created_at from workflow_versions
		 where namespace = $1 and workflow = $2 and commit = $3`,
		namespace, workflow, commit).Scan(&parent, &body, &tree, &v.Author, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, fmt.Errorf("%w: %s/%s@%s", ErrNoVersion, namespace, workflow, commit)
	}
	if err != nil {
		return Version{}, fmt.Errorf("db: version %s@%s could not be read: %w", workflow, commit, err)
	}
	if parent != nil {
		v.Parent = *parent
	}

	var s stored
	if err := json.Unmarshal(body, &s); err != nil {
		return Version{}, fmt.Errorf("db: version %s@%s could not be read: %w", workflow, commit, err)
	}
	v.Entry, v.Document, v.Includes, v.Manifests = s.Entry, s.Document, s.Includes, s.Manifests

	// A version without its tree is still a version a run can be decided from, so a null
	// here is a nil Tree rather than a refusal. What refuses it is the redemption.
	if tree != nil {
		if v.Tree, err = decodeTree(tree, namespace, workflow, commit); err != nil {
			return Version{}, err
		}
	}
	return v, nil
}
