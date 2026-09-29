package db

import (
	"context"
	"fmt"
)

// RenameWorkflow gives a workflow another name within its namespace: "a rename carries everything at
// once: versions, refs, runs, grants and triggers answer at the new name from the answer on, the old
// name is free". One statement on the workflow's row, which every key naming it carries through
// (migration 0053), and the grants of the tasks of its runs, which name the version their task runs
// by the workflow's name in their scope, so that a task dispatched before the rename redeems after
// it. Under the lock every writer of the repository takes, so that a push and a rename are one
// before the other.
//
// ErrNoWorkflow where the namespace holds no such workflow, or one deleted; ErrWorkflowExists where
// it holds one under the new name, and ErrWorkflowPurging where one deleted under it is still being
// purged.
func (n *NS) RenameWorkflow(ctx context.Context, workflow, to string) error {
	if err := n.HoldRepository(ctx, workflow); err != nil {
		return err
	}
	if err := nameKept(ctx, n.tx, n.namespace, to); err != nil {
		return err
	}
	var held, purging bool
	if err := n.tx.QueryRow(ctx,
		`select count(*) > 0, coalesce(bool_or(deleted_at is not null), false) from workflows where namespace = $1 and name = $2`,
		n.namespace, to).Scan(&held, &purging); err != nil {
		return fmt.Errorf("db: whether %s is taken could not be read: %w", to, err)
	}
	switch {
	case purging:
		return fmt.Errorf("%w: %s/%s", ErrWorkflowPurging, n.namespace, to)
	case held:
		return fmt.Errorf("%w: %s/%s", ErrWorkflowExists, n.namespace, to)
	}
	if _, err := n.tx.Exec(ctx,
		`update workflows set name = $3 where namespace = $1 and name = $2`,
		n.namespace, workflow, to); err != nil {
		return fmt.Errorf("db: workflow %s could not be renamed %s: %w", workflow, to, err)
	}
	if _, err := n.tx.Exec(ctx,
		`update task_grants set scope = jsonb_set(scope, '{workflow}', to_jsonb($3::text))
		 where namespace = $1 and scope->>'workflow' = $2`,
		n.namespace, workflow, to); err != nil {
		return fmt.Errorf("db: the grants of the tasks of %s could not follow its rename: %w", workflow, err)
	}
	return nil
}
