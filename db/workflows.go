package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// A workflow's record, as its repository is answered: what POST /api/v1/{ns}/workflows creates and
// GET /api/v1/{ns}/workflows/{name} reads, and the versions it holds, newest first, which is what a
// workflow no git push has filled lists in place of a history.

// ErrWorkflowExists is a workflow of that name in the namespace already.
var ErrWorkflowExists = errors.New("db: the namespace holds a workflow of that name already")

// Workflow is one workflow's repository, as its record reads at one moment.
type Workflow struct {
	Namespace string
	Name      string

	// DefaultBranch is the branch HEAD names, without refs/heads/, and Protected whether pushing to
	// it takes grant:manage, which its ref says: false where it has none, a branch no push could
	// create, which only an API call written by hand gave a v0.2 or v0.3 push.
	DefaultBranch string
	Protected     bool

	// Head is the commit the default branch points at, and empty while it is unborn.
	Head string

	// CreatedAt is when the workflow was recorded, and CreatedBy who created it through the API,
	// empty for one a tree push created, before v0.4.0 or since.
	CreatedAt time.Time
	CreatedBy string
}

// CreateWorkflow records an empty repository, as POST /api/v1/{ns}/workflows creates one: its
// default branch unborn, protected where w says, and who created it. It answers the workflow as
// recorded, ErrWorkflowExists where the namespace holds one of that name, and ErrNoNamespace where
// there is no namespace to hold it.
//
// Two creations of one name at once are answered one each way, by the primary key: the second
// insert finds the first's row, and does nothing.
func (n *NS) CreateWorkflow(ctx context.Context, w Workflow) (Workflow, error) {
	head := "refs/heads/" + w.DefaultBranch
	switch {
	case w.Name == "":
		return Workflow{}, errors.New("db: a workflow created with no name")
	case w.CreatedBy == "":
		return Workflow{}, fmt.Errorf("db: workflow %s created by nobody", w.Name)
	case w.DefaultBranch == "":
		return Workflow{}, fmt.Errorf("db: workflow %s created with no default branch", w.Name)
	}
	if err := checkRef(head); err != nil {
		return Workflow{}, fmt.Errorf("db: the default branch of workflow %s: %w", w.Name, err)
	}
	if err := n.Present(ctx, ""); err != nil {
		return Workflow{}, err
	}
	w.Namespace = n.namespace
	err := n.tx.QueryRow(ctx,
		`insert into workflows (namespace, name, default_branch, created_by) values ($1, $2, $3, $4)
		 on conflict (namespace, name) do nothing
		 returning created_at`,
		n.namespace, w.Name, w.DefaultBranch, w.CreatedBy).Scan(&w.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Workflow{}, fmt.Errorf("%w: %s/%s", ErrWorkflowExists, n.namespace, w.Name)
	}
	if err != nil {
		return Workflow{}, fmt.Errorf("db: workflow %s could not be recorded: %w", w.Name, err)
	}
	if _, err := n.tx.Exec(ctx,
		`insert into workflow_refs (namespace, workflow, ref, protected) values ($1, $2, $3, $4)`,
		n.namespace, w.Name, head, w.Protected); err != nil {
		return Workflow{}, fmt.Errorf("db: the default branch of workflow %s could not be recorded: %w", w.Name, err)
	}
	w.Head = ""
	return w, nil
}

// Workflow reads one workflow's record, and ErrNoWorkflow where the namespace holds none of that
// name.
func (n *NS) Workflow(ctx context.Context, name string) (Workflow, error) {
	w := Workflow{Namespace: n.namespace, Name: name}
	var head *string
	err := n.tx.QueryRow(ctx,
		`select w.default_branch, w.created_at, coalesce(w.created_by, ''), coalesce(r.protected, false), r.commit
		   from workflows w
		   left join workflow_refs r
		     on r.namespace = w.namespace and r.workflow = w.name and r.ref = 'refs/heads/' || w.default_branch
		  where w.namespace = $1 and w.name = $2`,
		n.namespace, name).Scan(&w.DefaultBranch, &w.CreatedAt, &w.CreatedBy, &w.Protected, &head)
	if errors.Is(err, pgx.ErrNoRows) {
		return Workflow{}, fmt.Errorf("%w: %s/%s", ErrNoWorkflow, n.namespace, name)
	}
	if err != nil {
		return Workflow{}, fmt.Errorf("db: workflow %s could not be read: %w", name, err)
	}
	w.Head = deref(head)
	return w, nil
}

// Versions reads a workflow's versions, newest first by when each was recorded, and by commit
// where two were recorded at one instant: at most limit of them, from the one recorded under from,
// itself included, or from the newest where from is empty. A from that is no version of the
// workflow is ErrNoVersion. Each carries its commit, its parent, its author, when it was recorded
// and how it arrived, and nothing a graph is rebuilt from, which Version reads.
//
// It is what a workflow no git push has filled lists in place of its history, a page at a time,
// and what a run naming no ref runs until then, the newest of them.
func (n *NS) Versions(ctx context.Context, workflow, from string, limit int) ([]Version, error) {
	if limit < 1 {
		return nil, fmt.Errorf("db: %d versions of %s asked for", limit, workflow)
	}
	var rows pgx.Rows
	var err error
	if from == "" {
		rows, err = n.tx.Query(ctx,
			`select commit, coalesce(parent, ''), author, created_at, source from workflow_versions
			  where namespace = $1 and workflow = $2
			  order by created_at desc, commit desc
			  limit $3`,
			n.namespace, workflow, limit)
	} else {
		var at time.Time
		err = n.tx.QueryRow(ctx,
			`select created_at from workflow_versions where namespace = $1 and workflow = $2 and commit = $3`,
			n.namespace, workflow, from).Scan(&at)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s/%s@%s", ErrNoVersion, n.namespace, workflow, from)
		}
		if err != nil {
			return nil, fmt.Errorf("db: the versions of %s could not be read: %w", workflow, err)
		}
		rows, err = n.tx.Query(ctx,
			`select commit, coalesce(parent, ''), author, created_at, source from workflow_versions
			  where namespace = $1 and workflow = $2 and (created_at, commit) <= ($3, $4)
			  order by created_at desc, commit desc
			  limit $5`,
			n.namespace, workflow, at, from, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("db: the versions of %s could not be read: %w", workflow, err)
	}
	versions, err := pgx.CollectRows(rows, n.versionRow(workflow))
	if err != nil {
		return nil, fmt.Errorf("db: the versions of %s could not be read: %w", workflow, err)
	}
	return versions, nil
}

// VersionsAmong reads which of commits are versions of workflow, by commit, each as Versions reads
// it, for a history that marks the commits that are.
func (n *NS) VersionsAmong(ctx context.Context, workflow string, commits []string) (map[string]Version, error) {
	rows, err := n.tx.Query(ctx,
		`select commit, coalesce(parent, ''), author, created_at, source from workflow_versions
		  where namespace = $1 and workflow = $2 and commit = any($3)`,
		n.namespace, workflow, commits)
	if err != nil {
		return nil, fmt.Errorf("db: the versions of %s could not be read: %w", workflow, err)
	}
	versions, err := pgx.CollectRows(rows, n.versionRow(workflow))
	if err != nil {
		return nil, fmt.Errorf("db: the versions of %s could not be read: %w", workflow, err)
	}
	out := make(map[string]Version, len(versions))
	for _, v := range versions {
		out[v.Commit] = v
	}
	return out, nil
}

// versionRow reads a version as Versions answers it.
func (n *NS) versionRow(workflow string) pgx.RowToFunc[Version] {
	return func(row pgx.CollectableRow) (Version, error) {
		v := Version{Namespace: n.namespace, Workflow: workflow}
		err := row.Scan(&v.Commit, &v.Parent, &v.Author, &v.CreatedAt, &v.Source)
		return v, err
	}
}
