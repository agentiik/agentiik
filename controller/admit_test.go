package controller

import (
	"context"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// What queued waits on, played out. Until this, queued was a word a run passed through on its
// way to running without ever waiting for anything, which is a state machine with a state in it
// that means nothing.

const groupedWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
concurrency:
  group: monthly-invoicing
  cancel_in_progress: false
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: normalize, port: ok } }
steps:
  normalize:
    image: ` + theImage + `
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
`

const impatientWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
concurrency:
  group: monthly-invoicing
  cancel_in_progress: true
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: normalize, port: ok } }
steps:
  normalize:
    image: ` + theImage + `
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
`

const second agk.RunID = "01M2Z8V1P9C4XQ7K2N4D6F8H0D"

// createSecond queues a second run of the same workflow, which is what a trigger firing while a
// run is still going produces.
func createSecond(t *testing.T, pool *db.Pool) {
	t.Helper()
	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.CreateRun(ctx, db.NewRun{
			ID: second, Workflow: "monthly-invoicing", Commit: "a3f9c1e",
			Trigger: agk.TriggerSchedule, TriggeredBy: "schedule",
			Inputs: map[string]any{"orders": []any{}},
			Steps:  []agk.Step{"normalize"},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func runState(t *testing.T, co *Core, run agk.RunID) agk.RunState {
	t.Helper()
	var e db.Evaluation
	if err := co.controller.Fenced(t.Context(), co.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		e, err = w.Run(ctx, run)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return e.State
}

// "A trigger that fires while a run of the same workflow is still going follows the declared
// concurrency policy." With cancel_in_progress false, it waits.
func TestASecondRunWaitsForTheGroup(t *testing.T) {
	core, q, pool, _ := decidingOn(t, groupedWorkflow)
	createRun(t, pool)
	createSecond(t, pool)

	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.taken()
	if len(first) != 1 {
		t.Fatalf("the first run published %d tasks", len(first))
	}

	// The second is offered to the controller and stays where it is.
	if err := core.Decide(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if got := runState(t, core, second); got != agk.Queued {
		t.Fatalf("a run behind a held group is %s", got)
	}
	if got := q.taken(); len(got) != 0 {
		t.Fatalf("a run behind a held group published %+v", got)
	}

	// The holder ends, and the sweep lets the next one through.
	core.answer(t, succeeded(t, first[0], core.now()))
	if got := runState(t, core, decidedRun); got != agk.Succeeded {
		t.Fatalf("the first run ended in %s", got)
	}
	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	if got := runState(t, core, second); got != agk.Running {
		t.Errorf("once the group was free the waiting run is %s", got)
	}
	if got := q.taken(); len(got) != 1 {
		t.Fatalf("the waiting run published %d tasks once the group was free", len(got))
	}
}

// "concurrency: { group, cancel_in_progress }". With it true, the new run takes the group and
// the one in progress is cancelled rather than waited for.
func TestCancelInProgressTakesTheGroup(t *testing.T) {
	core, q, pool, _ := decidingOn(t, impatientWorkflow)
	createRun(t, pool)

	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	holder := q.taken()
	if len(holder) != 1 {
		t.Fatalf("the first run published %d tasks", len(holder))
	}

	createSecond(t, pool)
	if err := core.Decide(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if got := runState(t, core, decidedRun); got != agk.Cancelled {
		t.Fatalf("the run in progress is %s and the workflow asked for it to be cancelled", got)
	}
	stops := q.stops()
	if len(stops) != 1 || stops[0].Task != holder[0].ID {
		t.Fatalf("cancelling the holder stopped %+v", stops)
	}

	// The new run is not admitted in the same pass: two runs holding one group for as long
	// as a cancellation takes to reach the runners is the thing the group exists to prevent.
	if got := runState(t, core, second); got != agk.Queued {
		t.Errorf("the new run took the group in the same pass that cancelled the old one, and it is %s", got)
	}
	if err := core.Decide(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if got := runState(t, core, second); got != agk.Running {
		t.Errorf("on the next pass the new run is %s", got)
	}
	if got := q.taken(); len(got) != 1 {
		t.Errorf("the new run published %d tasks", len(got))
	}
}

// A run with no concurrency block waits for nothing, which is what most workflows declare.
func TestAWorkflowWithNoGroupWaitsForNobody(t *testing.T) {
	core, q, pool, _ := deciding(t)
	createRun(t, pool)
	createSecond(t, pool)

	for _, run := range []agk.RunID{decidedRun, second} {
		if err := core.Decide(t.Context(), run); err != nil {
			t.Fatal(err)
		}
		if got := runState(t, core, run); got != agk.Running {
			t.Errorf("run %s is %s, and nothing declared a group", run, got)
		}
	}
	if got := q.taken(); len(got) != 2 {
		t.Errorf("two runs with no group published %d tasks between them", len(got))
	}
}

// "max_concurrent_tasks: Caps how much of the runner fleet one namespace can hold at once, so a
// fan-out of ten thousand items cannot starve everyone else." A namespace at its ceiling slows
// down rather than failing, and what was held back goes out when a slot frees.
func TestANamespaceAtItsCeilingSlowsDown(t *testing.T) {
	core, q, pool, super := deciding(t)
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(),
		`update namespaces set max_concurrent_tasks = 1 where name = 'finance'`); err != nil {
		t.Fatal(err)
	}
	createRun(t, pool)
	createSecond(t, pool)

	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.taken()
	if len(first) != 1 {
		t.Fatalf("the first run published %d tasks", len(first))
	}

	// The second run starts, because a quota bounds what a namespace holds and not how many
	// runs it has going, and then hands out nothing.
	if err := core.Decide(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if got := runState(t, core, second); got != agk.Running {
		t.Errorf("the second run is %s, and a quota is about tasks rather than runs", got)
	}
	if got := q.taken(); len(got) != 0 {
		t.Fatalf("a namespace holding its one allowed task published %+v", got)
	}

	// A run whose message never went stays actionable, which is what brings the controller
	// back for it once a slot frees.
	core.answer(t, succeeded(t, first[0], core.now()))
	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	if got := q.taken(); len(got) != 1 {
		t.Fatalf("once a slot freed the held back task went out %d times", len(got))
	}
}
