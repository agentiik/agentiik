package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A rename carries everything at once: the version, the run pinned to it, the refs, the grants on
// the workflow, its pins, and the scope of its tasks' grants, which name the version a task runs by
// the workflow's name, all answer at the new name, and the old one is free. A name held, or one a
// deleted workflow still holds while it is purged, is refused.
func TestARenameCarriesEveryRowToTheNewName(t *testing.T) {
	pool, super := opened(t)
	conn := superuser(t, super)
	for _, stmt := range []string{
		`insert into workflow_refs (namespace, workflow, ref, commit, moved_by, moved_at) values ('finance', 'monthly-invoicing', 'refs/heads/release', repeat('a', 40), 'alice', now())`,
		`insert into principals (id, kind) values ('bob', 'user') on conflict do nothing`,
		`insert into grants (id, namespace, workflow, principal, role, granted_by) values ('01M2GRANTAAAAAAAAAAAAAAAAA', 'finance', 'monthly-invoicing', 'bob', 'viewer', 'alice')`,
		`insert into task_grants (namespace, task_id, hash, expires_at, scope) values ('finance', '01M2T1AAAAAAAAAAAAAAAAAAAA', repeat('a', 64), now() + interval '1 hour',
		   '{"run":"` + financeRun + `","step":"archive","workflow":"monthly-invoicing","commit":"a3f9c1e"}')`,
		`insert into workflows (namespace, name) values ('finance', 'payroll'), ('finance', 'gone')`,
		`update workflows set deleted_at = now(), deleted_by = 'carol' where namespace = 'finance' and name = 'gone'`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	rename := func(to string) error {
		return pool.In(t.Context(), "finance", func(ctx context.Context, n *NS) error {
			return n.RenameWorkflow(ctx, "monthly-invoicing", to)
		})
	}
	if err := rename("payroll"); !errors.Is(err, ErrWorkflowExists) {
		t.Errorf("a rename to a name held answered %v", err)
	}
	if err := rename("gone"); !errors.Is(err, ErrWorkflowPurging) {
		t.Errorf("a rename to a name being purged answered %v", err)
	}
	if err := rename("invoicing"); err != nil {
		t.Fatal(err)
	}
	for table, query := range map[string]string{
		"workflow_versions": `select count(*) from workflow_versions where namespace = 'finance' and workflow = 'invoicing'`,
		"runs":              `select count(*) from runs where namespace = 'finance' and workflow = 'invoicing'`,
		"workflow_refs":     `select count(*) from workflow_refs where namespace = 'finance' and workflow = 'invoicing'`,
		"grants":            `select count(*) from grants where namespace = 'finance' and workflow = 'invoicing'`,
		"task_grants":       `select count(*) from task_grants where namespace = 'finance' and scope->>'workflow' = 'invoicing'`,
	} {
		var n int
		if err := conn.QueryRow(t.Context(), query).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s holds %d rows at the new name: %v", table, n, err)
		}
	}
	var old int
	if err := conn.QueryRow(t.Context(), `select (select count(*) from workflows where name = 'monthly-invoicing')
	                                           + (select count(*) from runs where workflow = 'monthly-invoicing')`).Scan(&old); err != nil || old != 0 {
		t.Errorf("%d rows still name the old name: %v", old, err)
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, n *NS) error {
		_, err := n.CreateWorkflow(ctx, "monthly-invoicing", "main", false, "alice", time.Time{})
		return err
	}); err != nil {
		t.Errorf("the old name is not free: %v", err)
	}
}
