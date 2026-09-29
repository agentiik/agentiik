package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// WorkflowRecord is a workflow's repository as the API answers it: its name, its default branch and
// whether that branch is protected, who created it and when, and the commit its default branch
// points at. Its labels are not read here: they are what the default version's entry point writes,
// which the API reads from that version.
type WorkflowRecord struct {
	Namespace string
	Name      string

	DefaultBranch string
	// Protected is the default branch's protection, which is the repository's: only the default
	// branch is ever protected.
	Protected bool

	CreatedAt time.Time
	// CreatedBy is who created it by POST /api/v1/{ns}/workflows, empty for a workflow from
	// before v0.4.0 or one a tree push created.
	CreatedBy string

	// Head is the commit the default branch points at, empty while it is unborn.
	Head string
}

// ErrWorkflowExists is a workflow created under a name its namespace holds already.
var ErrWorkflowExists = errors.New("db: the namespace holds a workflow of that name")

// ErrWorkflowPurging is a workflow created under the name of one deleted whose purge has not ended:
// the name is free once the purge has removed the workflow's row, and not before.
var ErrWorkflowPurging = errors.New("db: a workflow deleted under that name is still being purged")

// ErrNoBranch is a default branch named that the repository does not hold, where it holds commits.
var ErrNoBranch = errors.New("db: the repository holds no such branch")

// CreateWorkflow records an empty repository: the workflow, and its default branch unborn, protected
// or not, so that whether pushing to it takes grant:manage is settled before its first push. A name
// the namespace holds is ErrWorkflowExists, whoever created it and however.
func (n *NS) CreateWorkflow(ctx context.Context, name, branch string, protected bool, by string, at time.Time) (WorkflowRecord, error) {
	head := "refs/heads/" + branch
	if err := checkRef(head); err != nil {
		return WorkflowRecord{}, err
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tag, err := n.tx.Exec(ctx,
		`insert into workflows (namespace, name, default_branch, created_by, created_at) values ($1, $2, $3, $4, $5)
		 on conflict (namespace, name) do nothing`,
		n.namespace, name, branch, by, at)
	if err != nil {
		return WorkflowRecord{}, fmt.Errorf("db: workflow %s could not be created: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		var purging bool
		if err := n.tx.QueryRow(ctx,
			`select deleted_at is not null from workflows where namespace = $1 and name = $2`,
			n.namespace, name).Scan(&purging); err == nil && purging {
			return WorkflowRecord{}, fmt.Errorf("%w: %s/%s", ErrWorkflowPurging, n.namespace, name)
		}
		return WorkflowRecord{}, fmt.Errorf("%w: %s/%s", ErrWorkflowExists, n.namespace, name)
	}
	if _, err := n.tx.Exec(ctx,
		`insert into workflow_refs (namespace, workflow, ref, protected) values ($1, $2, $3, $4)`,
		n.namespace, name, head, protected); err != nil {
		return WorkflowRecord{}, fmt.Errorf("db: the default branch of workflow %s could not be recorded: %w", name, err)
	}
	return n.WorkflowRecord(ctx, name)
}

// WorkflowRecord reads one workflow's repository, or ErrNoWorkflow.
func (n *NS) WorkflowRecord(ctx context.Context, name string) (WorkflowRecord, error) {
	w := WorkflowRecord{Namespace: n.namespace, Name: name}
	var by, head *string
	var protected *bool
	err := n.tx.QueryRow(ctx,
		`select w.default_branch, w.created_at, w.created_by, r.protected, r.commit
		 from workflows w
		 left join workflow_refs r on r.namespace = w.namespace and r.workflow = w.name
		   and r.ref = 'refs/heads/' || w.default_branch
		 where w.namespace = $1 and w.name = $2 and w.deleted_at is null`,
		n.namespace, name).Scan(&w.DefaultBranch, &w.CreatedAt, &by, &protected, &head)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowRecord{}, fmt.Errorf("%w: %s/%s", ErrNoWorkflow, n.namespace, name)
	}
	if err != nil {
		return WorkflowRecord{}, fmt.Errorf("db: workflow %s could not be read: %w", name, err)
	}
	w.CreatedBy, w.Head = deref(by), deref(head)
	w.Protected = protected != nil && *protected
	return w, nil
}

// SetDefault names a workflow's default branch, protects it or leaves it unprotected, or both, and
// answers the repository as it stood before and as it stands after. A nil field is left as it is.
//
// A branch named the default is one the repository holds, or ErrNoBranch, where the repository holds
// any commit; while it holds none, it is the branch its first push will create, and the unborn row
// is renamed to it. Protection moves with the default branch, since only the default branch is
// protected: the branch it leaves is unprotected, and an unborn one it leaves is removed, since the
// only ref held without a commit is the default branch nothing was pushed to yet.
//
// Under the lock every repository writer takes first, which a push takes to move its refs and under
// which it reads protection again (UpdateRefs): a push judged before a change of it is held to the
// change all the same.
func (n *NS) SetDefault(ctx context.Context, workflow string, branch *string, protected *bool) (WorkflowRecord, WorkflowRecord, error) {
	if err := n.HoldRepository(ctx, workflow); err != nil {
		return WorkflowRecord{}, WorkflowRecord{}, err
	}
	before, err := n.WorkflowRecord(ctx, workflow)
	if err != nil {
		return WorkflowRecord{}, WorkflowRecord{}, err
	}
	keep := before.Protected
	if protected != nil {
		keep = *protected
	}
	old := "refs/heads/" + before.DefaultBranch
	if branch != nil && *branch != before.DefaultBranch {
		head := "refs/heads/" + *branch
		if err := checkRef(head); err != nil {
			return WorkflowRecord{}, WorkflowRecord{}, err
		}
		var born bool
		if err := n.tx.QueryRow(ctx,
			`select exists (select 1 from workflow_refs where namespace = $1 and workflow = $2 and commit is not null)`,
			n.namespace, workflow).Scan(&born); err != nil {
			return WorkflowRecord{}, WorkflowRecord{}, fmt.Errorf("db: the refs of %s could not be read: %w", workflow, err)
		}
		if born {
			tag, err := n.tx.Exec(ctx,
				`update workflow_refs set protected = $4
				 where namespace = $1 and workflow = $2 and ref = $3 and commit is not null`,
				n.namespace, workflow, head, keep)
			if err != nil {
				return WorkflowRecord{}, WorkflowRecord{}, fmt.Errorf("db: the default branch of %s could not be changed: %w", workflow, err)
			}
			if tag.RowsAffected() == 0 {
				return WorkflowRecord{}, WorkflowRecord{}, fmt.Errorf("%w: %s", ErrNoBranch, head)
			}
			if _, err := n.tx.Exec(ctx,
				`delete from workflow_refs where namespace = $1 and workflow = $2 and ref = $3 and commit is null`,
				n.namespace, workflow, old); err != nil {
				return WorkflowRecord{}, WorkflowRecord{}, fmt.Errorf("db: the default branch of %s could not be changed: %w", workflow, err)
			}
			if _, err := n.tx.Exec(ctx,
				`update workflow_refs set protected = false where namespace = $1 and workflow = $2 and ref = $3`,
				n.namespace, workflow, old); err != nil {
				return WorkflowRecord{}, WorkflowRecord{}, fmt.Errorf("db: the default branch of %s could not be changed: %w", workflow, err)
			}
		} else if _, err := n.tx.Exec(ctx,
			`update workflow_refs set ref = $4, protected = $5 where namespace = $1 and workflow = $2 and ref = $3`,
			n.namespace, workflow, old, head, keep); err != nil {
			return WorkflowRecord{}, WorkflowRecord{}, fmt.Errorf("db: the default branch of %s could not be changed: %w", workflow, err)
		}
		if _, err := n.tx.Exec(ctx,
			`update workflows set default_branch = $3 where namespace = $1 and name = $2`,
			n.namespace, workflow, *branch); err != nil {
			return WorkflowRecord{}, WorkflowRecord{}, fmt.Errorf("db: the default branch of %s could not be changed: %w", workflow, err)
		}
	} else if protected != nil && *protected != before.Protected {
		if _, err := n.tx.Exec(ctx,
			`update workflow_refs set protected = $4 where namespace = $1 and workflow = $2 and ref = $3`,
			n.namespace, workflow, old, keep); err != nil {
			return WorkflowRecord{}, WorkflowRecord{}, fmt.Errorf("db: the protection of %s could not be changed: %w", workflow, err)
		}
	}
	after, err := n.WorkflowRecord(ctx, workflow)
	return before, after, err
}

// Listed is a version as a listing names it: its commit, its parent, who pushed it, when, and how.
type Listed struct {
	Commit    string
	Parent    string
	Author    string
	CreatedAt time.Time
	Source    string

	// Library says the commit is a library's, which nothing runs.
	Library bool
}

// VersionsAt reads, of the commits given, those that are versions of the workflow, by commit.
func (n *NS) VersionsAt(ctx context.Context, workflow string, commits []string) (map[string]Listed, error) {
	rows, err := n.tx.Query(ctx,
		`select commit, parent, author, created_at, source, coalesce((graph->>'library')::boolean, false)
		 from workflow_versions
		 where namespace = $1 and workflow = $2 and commit = any($3)`,
		n.namespace, workflow, commits)
	if err != nil {
		return nil, fmt.Errorf("db: the versions of %s could not be read: %w", workflow, err)
	}
	listed, err := pgx.CollectRows(rows, scanListed)
	if err != nil {
		return nil, fmt.Errorf("db: the versions of %s could not be read: %w", workflow, err)
	}
	out := make(map[string]Listed, len(listed))
	for _, l := range listed {
		out[l.Commit] = l
	}
	return out, nil
}

// TreeVersions reads up to limit versions a tree push recorded, newest first, from the one at from
// where it is given, and the commit of the one after them where there is one: the history of a
// workflow no git push has filled. ErrNoVersion where from is no such version.
func (n *NS) TreeVersions(ctx context.Context, workflow, from string, limit int) ([]Listed, string, error) {
	var after time.Time
	if from != "" {
		err := n.tx.QueryRow(ctx,
			`select created_at from workflow_versions
			 where namespace = $1 and workflow = $2 and commit = $3 and source = 'tree'`,
			n.namespace, workflow, from).Scan(&after)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", fmt.Errorf("%w: %s/%s@%s", ErrNoVersion, n.namespace, workflow, from)
		}
		if err != nil {
			return nil, "", fmt.Errorf("db: the versions of %s could not be read: %w", workflow, err)
		}
	}
	rows, err := n.tx.Query(ctx,
		`select commit, parent, author, created_at, source, coalesce((graph->>'library')::boolean, false)
		 from workflow_versions
		 where namespace = $1 and workflow = $2 and source = 'tree'
		   and ($3 = '' or (created_at, commit) <= ($4, $3))
		 order by created_at desc, commit desc
		 limit $5`,
		n.namespace, workflow, from, after, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("db: the versions of %s could not be read: %w", workflow, err)
	}
	listed, err := pgx.CollectRows(rows, scanListed)
	if err != nil {
		return nil, "", fmt.Errorf("db: the versions of %s could not be read: %w", workflow, err)
	}
	var next string
	if len(listed) > limit {
		next = listed[limit].Commit
		listed = listed[:limit]
	}
	return listed, next, nil
}

func scanListed(row pgx.CollectableRow) (Listed, error) {
	var l Listed
	var parent *string
	err := row.Scan(&l.Commit, &parent, &l.Author, &l.CreatedAt, &l.Source, &l.Library)
	l.Parent = deref(parent)
	return l, err
}
