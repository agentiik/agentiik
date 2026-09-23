package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// Deliberately not the same thing as Includes. Those are what it takes to rebuild the
	// graph with nothing in reach, and that has to keep working when the object store is
	// unreachable or the objects are long collected. This is what a container is given.
	Tree []TreeFile

	Author    string
	CreatedAt time.Time
}

// TreeFile is one file of the repository, at the path the container sees it under /agk/repo.
type TreeFile struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`

	// Mode is the one bit a tree carries that matters to a container: 0755 where the file
	// is executable, absent otherwise. Git tracks no more than that and neither does this.
	Mode string `json:"mode,omitempty"`
}

// stored is the shape the column holds. Written out rather than reusing Version so that adding a
// field to the Go type is a decision about the stored shape rather than a silent change to it.
type stored struct {
	Entry     string            `json:"entry"`
	Document  []byte            `json:"document"`
	Includes  map[string][]byte `json:"includes,omitempty"`
	Manifests map[string][]byte `json:"manifests,omitempty"`
	Tree      []TreeFile        `json:"tree,omitempty"`
}

// ErrNoVersion is nothing of that commit.
var ErrNoVersion = errors.New("db: no version of that workflow at that commit")

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

// SaveVersion records one commit of one workflow.
//
// A commit already written is left exactly as it was, and that is the point rather than a
// convenience: "a version is a commit", so the same commit pushed twice is the same version, and
// a second push that overwrote it would make a run pinned to that commit mean something else than
// it did when it started.
func (n *NS) SaveVersion(ctx context.Context, v Version) error {
	switch {
	case v.Workflow == "" || v.Commit == "":
		return fmt.Errorf("db: a version of %q at %q", v.Workflow, v.Commit)
	case v.Entry == "" || len(v.Document) == 0:
		return fmt.Errorf("db: version %s@%s carries no entry point, and what is stored is what it takes to rebuild it", v.Workflow, v.Commit)
	case v.Author == "":
		return fmt.Errorf("db: version %s@%s has no author", v.Workflow, v.Commit)
	}

	body, err := json.Marshal(stored{
		Entry: v.Entry, Document: v.Document,
		Includes: v.Includes, Manifests: v.Manifests, Tree: v.Tree,
	})
	if err != nil {
		return fmt.Errorf("db: version %s@%s could not be written: %w", v.Workflow, v.Commit, err)
	}
	created := v.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	if _, err := n.tx.Exec(ctx,
		`insert into workflow_versions (namespace, workflow, commit, parent, graph, author, created_at)
		 values ($1, $2, $3, $4, $5, $6, $7)
		 on conflict (namespace, workflow, commit) do nothing`,
		n.namespace, v.Workflow, v.Commit, nilIfEmpty(v.Parent), body, v.Author, created); err != nil {
		return fmt.Errorf("db: version %s@%s could not be recorded: %w", v.Workflow, v.Commit, err)
	}
	return nil
}

// Version reads one back, for a namespaced caller.
func (n *NS) Version(ctx context.Context, workflow, commit string) (Version, error) {
	return readVersion(ctx, n.tx, n.namespace, workflow, commit)
}

// Version reads one back, for the controller, which serves every namespace.
func (w *Wide) Version(ctx context.Context, namespace, workflow, commit string) (Version, error) {
	return readVersion(ctx, w.tx, namespace, workflow, commit)
}

func readVersion(ctx context.Context, tx pgx.Tx, namespace, workflow, commit string) (Version, error) {
	v := Version{Namespace: namespace, Workflow: workflow, Commit: commit}
	var body []byte
	var parent *string
	err := tx.QueryRow(ctx,
		`select parent, graph, author, created_at from workflow_versions
		 where namespace = $1 and workflow = $2 and commit = $3`,
		namespace, workflow, commit).Scan(&parent, &body, &v.Author, &v.CreatedAt)
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
	v.Tree = s.Tree
	return v, nil
}
