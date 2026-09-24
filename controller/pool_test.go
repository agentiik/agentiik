package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// Each pool's policy, applied at dispatch: "A step whose pool does not accept the run's namespace,
// or names no pool that exists, is not published: it fails on the infrastructure's account and
// names the pool. Resources are capped to the pool's ceilings on the task message."

// onPool is the workflow with its first step aimed at the pools carrying a label, and asking for
// resources, written as a step writes them.
func onPool(label, resources string) string {
	return strings.Replace(theWorkflow, `    outputs: [ok, rejected]
`, `    outputs: [ok, rejected]
    runs_on: [`+label+`]
    resources: `+resources+`
`, 1)
}

// Real PostgreSQL: the step fails without a message, on the infrastructure's account, and says
// which pool, or why no one pool.
func TestAStepWhosePoolWillNotRunTheNamespaceFailsUnpublished(t *testing.T) {
	for _, c := range []struct {
		name     string
		document string
		pools    string
		says     []string
	}{
		{
			"a pool that does not accept finance", onPool("site=ops", `{ cpu: "1" }`),
			`insert into runner_pools (name, labels, accepted_namespaces, created_by) values ('ops', '{site=ops}', '{team-ops}', 'admin')`,
			[]string{"runner pool ops", "does not accept the namespace finance"},
		},
		{
			"labels no pool carries", onPool("site=nowhere", `{ cpu: "1" }`),
			`insert into runner_pools (name, labels, created_by) values ('ops', '{site=ops}', 'admin')`,
			[]string{"[site=nowhere]", "no runner pool carries every one of those labels"},
		},
		{
			"labels two pools carry", onPool("site=ops", `{ cpu: "1" }`),
			`insert into runner_pools (name, labels, created_by) values ('ops', '{site=ops}', 'admin'), ('ops-arm', '{site=ops,arch=arm64}', 'admin')`,
			[]string{"[site=ops]", "runner pools ops and ops-arm each carry every one of those labels"},
		},
		{
			"no label, and no pool default", theWorkflow,
			`delete from runner_pools where name = 'default'`,
			[]string{"names no runner label", "runner pool default, which does not exist"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			core, q, pool, super := decidingOn(t, c.document)
			conn := dbtest.Superuser(t, super)
			if _, err := conn.Exec(t.Context(), c.pools); err != nil {
				t.Fatal(err)
			}
			createRun(t, pool)

			if err := core.Decide(t.Context(), decidedRun); err != nil {
				t.Fatal(err)
			}
			if published := q.dispatched(); len(published) != 0 {
				t.Fatalf("a task no runner may be handed was published: %+v", published)
			}

			// Failed on the infrastructure's account, 125, which no retry names, and no grant was
			// issued for a task that went nowhere.
			var state string
			var exit *int
			var grants int
			if err := conn.QueryRow(t.Context(), `
				select t.state, t.exit_code, (select count(*) from task_grants g where g.task_id = t.id)
				from tasks t where t.run_id = $1 and t.step = 'normalize'`, decidedRun).
				Scan(&state, &exit, &grants); err != nil {
				t.Fatal(err)
			}
			if state != "failed" || exit == nil || *exit != 125 {
				t.Errorf("the refused task is %s, exiting %v", state, exit)
			}
			if grants != 0 {
				t.Errorf("the refused task was issued %d grants", grants)
			}

			// And the step says why, and the run has ended on it.
			var reason string
			if err := conn.QueryRow(t.Context(),
				`select evaluation->'state'->'steps'->'normalize'->>'reason' from runs where id = $1`, decidedRun).
				Scan(&reason); err != nil {
				t.Fatal(err)
			}
			for _, says := range c.says {
				if !strings.Contains(reason, says) {
					t.Errorf("the step's reason is %q, and it does not say %q", reason, says)
				}
			}
			if got := stateOf(t, core); got != agk.Failed {
				t.Errorf("the run is %s", got)
			}
		})
	}
}

// "A step goes to the pool whose labels include every label of its runs_on": the pool the dispatch
// names, which is the queue the bus publishes it on, is the one pool carrying every label the step
// asked for, the use cases' site=home among them, and a step that names none goes to the pool
// default the installation was created with.
func TestAStepGoesToThePoolWhoseLabelsIncludeItsRunsOn(t *testing.T) {
	for _, c := range []struct {
		name     string
		document string
		want     string
	}{
		{"a label one pool carries among others", onPool("site=home", `{ cpu: "1" }`), "home"},
		{"no label at all", theWorkflow, "default"},
	} {
		t.Run(c.name, func(t *testing.T) {
			core, q, pool, super := decidingOn(t, c.document)
			if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `
				insert into runner_pools (name, labels, created_by) values
				  ('home', '{site=home,arch=arm64}', 'admin'), ('dmz', '{zone=dmz,arch=arm64}', 'admin')`); err != nil {
				t.Fatal(err)
			}
			createRun(t, pool)
			if err := core.Decide(t.Context(), decidedRun); err != nil {
				t.Fatal(err)
			}
			published := q.dispatched()
			if len(published) != 1 {
				t.Fatalf("the first pass published %d tasks", len(published))
			}
			if got := published[0].Pool; got != c.want {
				t.Errorf("the task went to the pool %q, want %q", got, c.want)
			}
		})
	}
}

// A step's pool is the step's, so a fan-out refused is refused whole on the pass that finds it,
// every shard, and not a slice at a time as max_parallel and the quota hand them out: one pass per
// slice, each writing every task of the run again, would hold the controller on one run for as
// long as a fan-out of ten thousand takes. Nor does a namespace with no slot free keep a refused
// step waiting for one it would never use.
func TestARefusedFanOutFailsWholeWithoutASlot(t *testing.T) {
	document := strings.Replace(onPool("site=nowhere", `{ cpu: "1" }`), `    runs_on: [site=nowhere]
`, `    runs_on: [site=nowhere]
    strategy: { fan_out: item, max_parallel: 1 }
`, 1)
	core, q, pool, super := decidingOn(t, document)
	conn := dbtest.Superuser(t, super)
	// The namespace's one slot is held by a task of another run.
	createSecond(t, pool)
	if _, err := conn.Exec(t.Context(), `
		update namespaces set max_concurrent_tasks = 1 where name = 'finance';
		insert into tasks (namespace, id, run_id, step, attempt, state, published_at)
		values ('finance', '01M2H0AAAAAAAAAAAAAAAAAAAA', '`+string(second)+`', 'normalize', 1, 'dispatched', now())`); err != nil {
		t.Fatal(err)
	}
	orders := make([]map[string]any, 40)
	for i := range orders {
		orders[i] = map[string]any{"customer_id": fmt.Sprintf("C-%d", i)}
	}
	inputs, err := json.Marshal(map[string]any{"orders": orders})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.CreateRun(ctx, db.NewRun{
			ID: decidedRun, Workflow: "monthly-invoicing", Commit: "a3f9c1e",
			Trigger: agk.TriggerManual, TriggeredBy: "alice", Inputs: inputs,
			Steps: []agk.Step{"normalize", "archive"},
		})
	}); err != nil {
		t.Fatal(err)
	}

	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	if published := q.dispatched(); len(published) != 0 {
		t.Fatalf("%d tasks no runner may be handed were published", len(published))
	}
	var failed, other int
	if err := conn.QueryRow(t.Context(), `
		select count(*) filter (where state = 'failed' and exit_code = 125),
		       count(*) filter (where not (state = 'failed' and exit_code = 125))
		from tasks where run_id = $1 and step = 'normalize'`, decidedRun).Scan(&failed, &other); err != nil {
		t.Fatal(err)
	}
	if failed != len(orders) || other != 0 {
		t.Errorf("after one pass %d shards failed on the pool and %d did not", failed, other)
	}
	if got := stateOf(t, core); got != agk.Failed {
		t.Errorf("the run is %s", got)
	}
}

// The pools are read again in the transaction that issues the grant, so a pool that stopped running
// the namespace after the pass read the pools, or one created since that carries the step's labels
// too, gives the task no credential and publishes nothing.
func TestAPoolChangedBeforeTheGrantIssuesNone(t *testing.T) {
	for _, c := range []struct {
		name   string
		change string
		says   string
	}{
		{"a pool that stopped accepting the namespace", `update runner_pools set accepted_namespaces = '{team-ops}' where name = 'ops'`, "runner pool ops"},
		{"a second pool carrying the labels", `insert into runner_pools (name, labels, created_by) values ('ops-2', '{site=ops}', 'admin')`, "runner pools ops and ops-2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			core, q, pool, super := decidingOn(t, onPool("site=ops", `{ cpu: "1" }`))
			conn := dbtest.Superuser(t, super)
			if _, err := conn.Exec(t.Context(),
				`insert into runner_pools (name, labels, accepted_namespaces, created_by) values ('ops', '{site=ops}', '{finance}', 'admin')`); err != nil {
				t.Fatal(err)
			}
			createRun(t, pool)
			if err := core.Decide(t.Context(), decidedRun); err != nil {
				t.Fatal(err)
			}
			published := q.dispatched()
			if len(published) != 1 || published[0].Pool != "ops" {
				t.Fatalf("the first pass published %+v", published)
			}

			if _, err := conn.Exec(t.Context(), c.change); err != nil {
				t.Fatal(err)
			}
			_, err := core.dispatchOf(t.Context(), "finance", published[0].Task)
			if !errors.As(err, new(unpublishable)) || !strings.Contains(err.Error(), c.says) {
				t.Errorf("preparing the task again answered %v", err)
			}
			var grants int
			if err := conn.QueryRow(t.Context(), `select count(*) from task_grants`).Scan(&grants); err != nil {
				t.Fatal(err)
			}
			if grants != 1 {
				t.Errorf("the task holds %d grants, and the refusal issued one", grants)
			}
		})
	}
}

// "Resources are capped to the pool's ceilings on the task message": an ask above a ceiling is
// capped, an ask below one is left alone, and a step that asks for nothing where the pool sets a
// ceiling is given the ceiling, since nothing is more than any ceiling.
func TestAnOversizedAskIsCappedOnTheMessage(t *testing.T) {
	core, q, pool, super := decidingOn(t, onPool("size=small", `{ cpu: "8", memory: "256Mi" }`))
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `
		insert into runner_pools (name, labels, accepted_namespaces, ceiling_cpu, ceiling_memory, ceiling_pids, created_by)
		values ('small', '{size=small}', '{finance}', '2', '1Gi', 128, 'admin')`); err != nil {
		t.Fatal(err)
	}
	createRun(t, pool)

	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	published := q.dispatched()
	if len(published) != 1 {
		t.Fatalf("the first pass published %d tasks", len(published))
	}
	want := graph.Resources{CPU: "2", Memory: "256Mi", PIDs: 128}
	if got := published[0].Task.Resources; got != want {
		t.Errorf("the message asks for %+v, want %+v", got, want)
	}
}

// Each kind against its own ceiling, compared as numbers of its kind rather than as text, and an
// ask that is not a number of its kind left for the runner to refuse on the brick's account.
func TestACeilingCapsEachKindOfAskOnItsOwn(t *testing.T) {
	ceiling := db.Ceilings{CPU: "2", Memory: "1Gi", PIDs: 128}
	for _, c := range []struct {
		why     string
		asked   graph.Resources
		ceiling db.Ceilings
		want    graph.Resources
	}{
		{"an ask above every ceiling", graph.Resources{CPU: "8", Memory: "2Gi", PIDs: 4096}, ceiling, graph.Resources{CPU: "2", Memory: "1Gi", PIDs: 128}},
		{"an ask below every ceiling", graph.Resources{CPU: "0.5", Memory: "512Mi", PIDs: 64}, ceiling, graph.Resources{CPU: "0.5", Memory: "512Mi", PIDs: 64}},
		{"an ask equal to every ceiling, written differently", graph.Resources{CPU: "2.0", Memory: "1024Mi", PIDs: 128}, ceiling, graph.Resources{CPU: "2.0", Memory: "1024Mi", PIDs: 128}},
		{"no ask at all", graph.Resources{}, ceiling, graph.Resources{CPU: "2", Memory: "1Gi", PIDs: 128}},
		{"a memory ask in a larger unit that is below", graph.Resources{Memory: "1Ti"}, db.Ceilings{Memory: "4096Gi"}, graph.Resources{Memory: "1Ti"}},
		{"a memory ask in a smaller unit that is above", graph.Resources{Memory: "2048Mi"}, db.Ceilings{Memory: "1Gi"}, graph.Resources{Memory: "1Gi"}},
		{"a memory ask no host has", graph.Resources{Memory: "99999999999999999999Ti"}, db.Ceilings{Memory: "8Gi"}, graph.Resources{Memory: "8Gi"}},
		{"a pool with no ceiling", graph.Resources{CPU: "64", Memory: "1Ti"}, db.Ceilings{}, graph.Resources{CPU: "64", Memory: "1Ti"}},
		{"an ask that is not a number of its kind", graph.Resources{CPU: "lots", Memory: "512M"}, ceiling, graph.Resources{CPU: "lots", Memory: "512M", PIDs: 128}},
	} {
		if got := capped(c.asked, c.ceiling); got != c.want {
			t.Errorf("%s: capped to %+v, want %+v", c.why, got, c.want)
		}
	}
}
