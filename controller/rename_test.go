package controller

import (
	"context"
	"testing"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// "A task dispatched before redeems after": and a run going on when its workflow is renamed plans
// its next tasks under the new name, which is the one its version is found by, rather than the one
// its document was written with when it started.
func TestARunGoingOnWhenItsWorkflowIsRenamedGoesOnUnderTheNewName(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.taken()
	if len(first) != 1 || first[0].Step != "normalize" {
		t.Fatalf("the first pass handed out %+v", first)
	}

	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.RenameWorkflow(ctx, "monthly-invoicing", "invoicing")
	}); err != nil {
		t.Fatal(err)
	}
	core.answer(t, succeeded(t, first[0], core.now()))

	var archive bool
	for _, task := range q.taken() {
		if task.Step != "archive" {
			continue
		}
		archive = true
		if task.Workflow != "invoicing" {
			t.Errorf("the step below was planned for the workflow %q", task.Workflow)
		}
		var scoped string
		if err := dbtest.Superuser(t, super).QueryRow(t.Context(),
			`select g.scope->>'workflow' from task_grants g join tasks k on k.namespace = g.namespace and k.id = g.task_id
			 where k.run_id = $1 and k.step = 'archive'`, string(decidedRun)).Scan(&scoped); err != nil {
			t.Fatal(err)
		}
		if scoped != "invoicing" {
			t.Errorf("the step below's grant names the workflow %q, which no version is found under", scoped)
		}
	}
	if !archive {
		t.Fatal("the step below was not handed out")
	}
}
