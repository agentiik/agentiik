package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
			Trigger: agk.TriggerSchedule,
			Inputs: json.RawMessage(`{"orders": []}`),
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

// fanOutWorkflow shards normalize over every order it is given, one task per item, and bounds the
// shards with nothing of its own: no max_parallel.
var fanOutWorkflow = strings.Replace(groupedWorkflow, `concurrency:
  group: monthly-invoicing
  cancel_in_progress: false
`, "", 1) + `    strategy: { fan_out: item }
`

// "max_concurrent_tasks: Caps how much of the runner fleet one namespace can hold at once, so a
// fan-out of ten thousand items cannot starve everyone else." Played out with the ten thousand: a
// sweep hands finance's fan-out as many tasks as its quota lets it hold and no more, and team-ops'
// run, created after it, is handed its task on the same sweep rather than behind 9,980 shards
// still waiting for a slot of finance's. The shards held back are not refused: they wait,
// pending, for a slot.
func TestATenThousandItemFanOutStarvesNoOtherNamespace(t *testing.T) {
	core, q, pool, super := decidingOn(t, fanOutWorkflow)
	conn := dbtest.Superuser(t, super)
	for _, stmt := range []string{
		`insert into namespaces (name) values ('team-ops')`,
		`insert into workflows (namespace, name) values ('team-ops', 'monthly-invoicing')`,
		`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at)
		   values ('team-ops', 'monthly-invoicing', 'a3f9c1e', '{}', 'bob', now())`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	orders := make([]map[string]any, 10_000)
	for i := range orders {
		orders[i] = map[string]any{"customer_id": fmt.Sprintf("C-%d", i)}
	}
	inputs, err := json.Marshal(map[string]any{"orders": orders})
	if err != nil {
		t.Fatal(err)
	}
	const opsRun agk.RunID = "01M2Z8V1P9C4XQ7K2N4D6F8H0E"
	for _, r := range []struct {
		namespace string
		run       agk.RunID
		inputs    json.RawMessage
	}{
		{"finance", decidedRun, inputs},
		{"team-ops", opsRun, json.RawMessage(`{"orders": [{"customer_id": "C-1042"}]}`)},
	} {
		if err := pool.In(t.Context(), r.namespace, func(ctx context.Context, ns *db.NS) error {
			return ns.CreateRun(ctx, db.NewRun{
				ID: r.run, Workflow: "monthly-invoicing", Commit: "a3f9c1e",
				Trigger: agk.TriggerManual, TriggeredBy: "alice", Inputs: r.inputs,
				Steps: []agk.Step{"normalize"},
			})
		}); err != nil {
			t.Fatal(err)
		}
	}

	handed := func() map[string]int {
		by := map[string]int{}
		for _, task := range q.taken() {
			by[task.Namespace]++
		}
		return by
	}
	for pass := range 2 {
		if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
			t.Fatal(err)
		}
		by := handed()
		// Twenty is max_concurrent_tasks at its default, which both namespaces keep.
		want := map[string]int{"finance": 20, "team-ops": 1}
		if pass == 1 {
			want = map[string]int{}
		}
		if len(by) != len(want) || by["finance"] != want["finance"] || by["team-ops"] != want["team-ops"] {
			t.Fatalf("sweep %d handed out %v, want %v", pass+1, by, want)
		}
	}
	for run, want := range map[agk.RunID]agk.RunState{decidedRun: agk.Running, opsRun: agk.Running} {
		if got := runState(t, core, run); got != want {
			t.Errorf("run %s is %s", run, got)
		}
	}

	// And what finance was not handed waits for a slot rather than being refused.
	var e db.Evaluation
	if err := core.controller.Fenced(t.Context(), core.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		e, err = w.Run(ctx, decidedRun)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		State struct {
			Steps map[string]struct {
				Shards []struct {
					Task string `json:"task"`
				} `json:"shards"`
			} `json:"steps"`
		} `json:"state"`
	}
	if err := json.Unmarshal(e.Document, &doc); err != nil {
		t.Fatal(err)
	}
	states := map[string]int{}
	for _, sh := range doc.State.Steps["normalize"].Shards {
		states[sh.Task]++
	}
	if len(doc.State.Steps["normalize"].Shards) != len(orders) || states["pending"] != len(orders)-20 {
		t.Errorf("finance's fan-out holds %d shards, by state %v", len(doc.State.Steps["normalize"].Shards), states)
	}
}
