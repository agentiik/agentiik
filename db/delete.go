package db

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// Deleting a workflow: "answered as absent from then on, its runs still going cancelled and the
// data of every run expiring at once; the leading controller purges the runs, then removes the
// versions, the refs and the packs, and the name is free once that is done."
//
// DeleteWorkflow is the answer's part, one transaction under the lock every writer of the
// repository takes; PurgeDeleted is the controller's, a batch at a time, in the order the rows name
// one another: a run names its version, a version its workflow, a pack its repository. Nothing
// here deletes a byte: the envelopes, logs and artifacts of the runs go by the purges they always
// went by, their expiry brought to the deletion, and the packs by the collection, which keeps a
// fetch that began before the deletion whole.

// DeleteWorkflow marks a workflow deleted, by and at, asks to cancel every run of it still going,
// brings the expiry of every run's envelopes, logs and artifacts to at, and answers the runs it
// asked to cancel, for the controller to be woken for each. ErrNoWorkflow where the namespace holds
// no such workflow, or one deleted already.
func (n *NS) DeleteWorkflow(ctx context.Context, workflow, by string, at time.Time) ([]agk.RunID, error) {
	if by == "" {
		return nil, fmt.Errorf("db: workflow %s deleted by nobody", workflow)
	}
	if err := n.HoldRepository(ctx, workflow); err != nil {
		return nil, err
	}
	if _, err := n.tx.Exec(ctx,
		`update workflows set deleted_at = $3, deleted_by = $4, armed = null where namespace = $1 and name = $2`,
		n.namespace, workflow, at, by); err != nil {
		return nil, fmt.Errorf("db: workflow %s could not be deleted: %w", workflow, err)
	}
	// Its triggers go with it at once, not with the purge that removes its row: a schedule of a
	// workflow deleted would otherwise go on starting runs of something nobody can read. The
	// workflow.delete entry is the act, and records none of them one by one.
	if _, err := n.tx.Exec(ctx,
		`delete from triggers where namespace = $1 and workflow = $2`, n.namespace, workflow); err != nil {
		return nil, fmt.Errorf("db: the triggers of %s could not be disarmed: %w", workflow, err)
	}
	// And what its webhooks checked a caller against, so that a workflow created again under the
	// name is not armed with a credential written for the one deleted.
	if _, err := n.tx.Exec(ctx,
		`delete from webhook_credentials where namespace = $1 and workflow = $2`, n.namespace, workflow); err != nil {
		return nil, fmt.Errorf("db: the credentials of the webhooks of %s could not be removed: %w", workflow, err)
	}
	rows, err := n.tx.Query(ctx,
		`update runs set cancel_requested_at = $3
		 where namespace = $1 and workflow = $2 and state in ('queued', 'running', 'waiting')
		   and cancel_requested_at is null
		 returning id`,
		n.namespace, workflow, at)
	if err != nil {
		return nil, fmt.Errorf("db: the runs of %s could not be cancelled: %w", workflow, err)
	}
	cancelled, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("db: the runs of %s could not be cancelled: %w", workflow, err)
	}
	if err := n.expireRuns(ctx, workflow, at); err != nil {
		return nil, err
	}
	out := make([]agk.RunID, len(cancelled))
	for i, id := range cancelled {
		out[i] = agk.RunID(id)
	}
	return out, nil
}

// expireRuns brings to at the expiry of every finished run of workflow, and of every live artifact
// of its runs, where it is later: what the purges then act on.
func (n *NS) expireRuns(ctx context.Context, workflow string, at time.Time) error {
	if _, err := n.tx.Exec(ctx,
		`update runs set expires_at = $3
		 where namespace = $1 and workflow = $2 and state not in ('queued', 'running', 'waiting')
		   and (expires_at is null or expires_at > $3)`,
		n.namespace, workflow, at); err != nil {
		return fmt.Errorf("db: the runs of %s could not be expired: %w", workflow, err)
	}
	if _, err := n.tx.Exec(ctx,
		`update artifacts a set expires_at = $3
		 from runs r
		 where r.namespace = $1 and r.workflow = $2
		   and a.namespace = r.namespace and a.run_id = r.id
		   and a.status = 'live' and a.expires_at > $3`,
		n.namespace, workflow, at); err != nil {
		return fmt.Errorf("db: the artifacts of %s could not be expired: %w", workflow, err)
	}
	return nil
}

// DeletedPurge is what one call of PurgeDeleted removed: runs, versions, packs handed to the collection,
// and workflows gone whole, whose names are free from then on.
type DeletedPurge struct {
	Runs      int
	Versions  int
	Packs     int
	Workflows int
}

// full says whether any part of the call took a whole batch, so that it is called again.
func (d DeletedPurge) full(batch int) bool {
	return d.Runs == batch || d.Versions == batch || d.Packs == batch || d.Workflows == batch
}

// PurgeDeleted takes the deleted workflows a step further, each part up to batch rows in a
// transaction of its own, and answers what it removed and whether any part was full:
//
//   - a run of one that finished since the deletion has its expiry, and its artifacts', brought to
//     now, as the deletion brought those of the runs that had finished;
//   - a run whose envelopes and logs the purges have let go of, and none of whose artifacts is live,
//     is deleted, its steps, tasks, grants and notifications with it;
//   - once none of its runs is left, each version lets go of the objects its tree named, as a run's
//     envelopes do at their expiry, and is deleted;
//   - its packs are superseded, which the collection deletes the grace after, as it does a repack's,
//     so that a fetch that began before the deletion reads on;
//   - and once nothing names it, the workflow's row is deleted, its refs, grants, pins and manifests
//     with it, and its name is free.
func (p *Pool) PurgeDeleted(ctx context.Context, batch int) (DeletedPurge, bool, error) {
	batch, err := batchOf(batch)
	if err != nil {
		return DeletedPurge{}, false, err
	}
	var out DeletedPurge
	for _, part := range []func(context.Context, *Wide) error{
		func(ctx context.Context, w *Wide) error { return w.expireDeleted(ctx, batch) },
		func(ctx context.Context, w *Wide) (err error) { out.Runs, err = w.deleteRuns(ctx, batch); return err },
		func(ctx context.Context, w *Wide) (err error) {
			out.Versions, err = w.deleteVersions(ctx, batch)
			return err
		},
		func(ctx context.Context, w *Wide) (err error) {
			out.Packs, err = w.supersedePacks(ctx, batch)
			return err
		},
		func(ctx context.Context, w *Wide) (err error) {
			out.Workflows, err = w.deleteWorkflows(ctx, batch)
			return err
		},
	} {
		if err := p.Installation(ctx, Purge, part); err != nil {
			return out, false, fmt.Errorf("db: the purge of deleted workflows: %w", err)
		}
	}
	return out, out.full(batch), nil
}

// expireDeleted brings the expiry of the runs of deleted workflows that finished since, and of their
// live artifacts, to now.
func (w *Wide) expireDeleted(ctx context.Context, batch int) error {
	if _, err := w.tx.Exec(ctx, `
		update runs set expires_at = now()
		where (namespace, id) in (
		  select r.namespace, r.id from runs r
		  join workflows f on f.namespace = r.namespace and f.name = r.workflow
		  where f.deleted_at is not null and r.state not in ('queued', 'running', 'waiting')
		    and (r.expires_at is null or r.expires_at > now())
		  limit $1
		  for update of r skip locked
		)`, batch); err != nil {
		return err
	}
	_, err := w.tx.Exec(ctx, `
		update artifacts set expires_at = now()
		where (namespace, run_id, step, port, name) in (
		  select a.namespace, a.run_id, a.step, a.port, a.name from artifacts a
		  join runs r on r.namespace = a.namespace and r.id = a.run_id
		  join workflows f on f.namespace = r.namespace and f.name = r.workflow
		  where f.deleted_at is not null and a.status = 'live' and a.expires_at > now()
		  limit $1
		  for update of a skip locked
		)`, batch)
	return err
}

// deleteRuns deletes the runs of deleted workflows the purges are done with: ended, their envelopes
// and logs let go of, and none of their artifacts live. Their steps, tasks, grants, logs' index,
// approvals and notifications go with them.
func (w *Wide) deleteRuns(ctx context.Context, batch int) (int, error) {
	tag, err := w.tx.Exec(ctx, `
		delete from runs
		where (namespace, id) in (
		  select r.namespace, r.id from runs r
		  join workflows f on f.namespace = r.namespace and f.name = r.workflow
		  where f.deleted_at is not null
		    and r.state not in ('queued', 'running', 'waiting')
		    and r.envelopes_purged_at is not null and r.logs_purged_at is not null
		    and not exists (select 1 from artifacts a
		                    where a.namespace = r.namespace and a.run_id = r.id and a.status = 'live')
		  limit $1
		  for update of r skip locked
		)`, batch)
	return int(tag.RowsAffected()), err
}

// deleteVersions lets go of the objects the versions of deleted workflows name, once none of their
// runs is left, and deletes the versions.
func (w *Wide) deleteVersions(ctx context.Context, batch int) (int, error) {
	rows, err := w.tx.Query(ctx, `
		select v.namespace, v.workflow, v.commit, coalesce(v.tree, '[]'::jsonb) from workflow_versions v
		join workflows f on f.namespace = v.namespace and f.name = v.workflow
		where f.deleted_at is not null
		  and not exists (select 1 from runs r where r.namespace = v.namespace and r.workflow = v.workflow)
		order by v.namespace, v.workflow, v.commit
		limit $1
		for update of v skip locked`, batch)
	if err != nil {
		return 0, err
	}
	type version struct{ namespace, workflow, commit string }
	type lowering struct{ namespace, digest string }
	var versions []version
	var lowerings []lowering
	for rows.Next() {
		var v version
		var tree []byte
		if err := rows.Scan(&v.namespace, &v.workflow, &v.commit, &tree); err != nil {
			rows.Close()
			return 0, err
		}
		var files []TreeFile
		if err := json.Unmarshal(tree, &files); err != nil {
			rows.Close()
			return 0, fmt.Errorf("the tree of %s/%s@%s could not be read: %w", v.namespace, v.workflow, v.commit, err)
		}
		// One reference per distinct digest, as SaveVersions raised them.
		seen := map[string]bool{}
		for _, f := range files {
			if !seen[f.SHA256] {
				seen[f.SHA256] = true
				lowerings = append(lowerings, lowering{namespace: v.namespace, digest: f.SHA256})
			}
		}
		versions = append(versions, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	// In digest order, as SaveVersions raises them, since each lowering locks the row it counts.
	slices.SortFunc(lowerings, func(a, b lowering) int {
		if a.namespace != b.namespace {
			return compare(a.namespace, b.namespace)
		}
		return compare(a.digest, b.digest)
	})
	for _, l := range lowerings {
		if err := lower(ctx, w.tx, l.namespace, "sha256:"+l.digest, ""); err != nil {
			return 0, err
		}
	}
	for _, v := range versions {
		if _, err := w.tx.Exec(ctx,
			`delete from workflow_versions where namespace = $1 and workflow = $2 and commit = $3`,
			v.namespace, v.workflow, v.commit); err != nil {
			return 0, err
		}
	}
	return len(versions), nil
}

// supersedePacks hands the packs of deleted workflows to the collection, which deletes them the grace
// after, as it deletes those a repack superseded.
func (w *Wide) supersedePacks(ctx context.Context, batch int) (int, error) {
	tag, err := w.tx.Exec(ctx, `
		update git_packs set state = 'superseded', superseded_at = now()
		where (namespace, repository, name) in (
		  select g.namespace, g.repository, g.name from git_packs g
		  join workflows f on f.namespace = g.namespace and f.repository = g.repository
		  where f.deleted_at is not null and g.state in ('receiving', 'live')
		  limit $1
		  for update of g skip locked
		)`, batch)
	return int(tag.RowsAffected()), err
}

// deleteWorkflows deletes the deleted workflows nothing names any more: no run, no version, no pack.
func (w *Wide) deleteWorkflows(ctx context.Context, batch int) (int, error) {
	tag, err := w.tx.Exec(ctx, `
		delete from workflows
		where (namespace, name) in (
		  select f.namespace, f.name from workflows f
		  where f.deleted_at is not null
		    and not exists (select 1 from runs r where r.namespace = f.namespace and r.workflow = f.name)
		    and not exists (select 1 from workflow_versions v where v.namespace = f.namespace and v.workflow = f.name)
		    and not exists (select 1 from git_packs g where g.namespace = f.namespace and g.repository = f.repository)
		  order by f.deleted_at
		  limit $1
		  for update of f skip locked
		)`, batch)
	return int(tag.RowsAffected()), err
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Deleted says whether workflow is one deleted and not yet purged, which is answered as absent: the
// authorizer holds nothing over it for anybody.
func (n *NS) Deleted(ctx context.Context, workflow string) (bool, error) {
	var deleted bool
	err := n.tx.QueryRow(ctx,
		`select exists (select 1 from workflows where namespace = $1 and name = $2 and deleted_at is not null)`,
		n.namespace, workflow).Scan(&deleted)
	if err != nil {
		return false, fmt.Errorf("db: whether %s is deleted could not be read: %w", workflow, err)
	}
	return deleted, nil
}

// DeletedAmong answers which of the workflows named, each a namespace and a name, are deleted and not
// yet purged, as Deleted answers one.
func (w *Wide) DeletedAmong(ctx context.Context, workflows [][2]string) (map[[2]string]bool, error) {
	out := map[[2]string]bool{}
	if len(workflows) == 0 {
		return out, nil
	}
	namespaces := make([]string, len(workflows))
	names := make([]string, len(workflows))
	for i, wf := range workflows {
		namespaces[i], names[i] = wf[0], wf[1]
	}
	rows, err := w.tx.Query(ctx, `
		select f.namespace, f.name from workflows f
		join unnest($1::text[], $2::text[]) as t(namespace, name) on f.namespace = t.namespace and f.name = t.name
		where f.deleted_at is not null`, namespaces, names)
	if err != nil {
		return nil, fmt.Errorf("db: which workflows are deleted could not be read: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ns, name string
		if err := rows.Scan(&ns, &name); err != nil {
			return nil, err
		}
		out[[2]string{ns, name}] = true
	}
	return out, rows.Err()
}
