package controller

import (
	"context"
	"encoding/json"
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

// onPool is the workflow with its first step aimed at a pool and asking for resources, written as a
// step writes them.
func onPool(pool, resources string) string {
	return strings.Replace(theWorkflow, `    outputs: [ok, rejected]
`, `    outputs: [ok, rejected]
    runs_on: [pool=`+pool+`]
    resources: `+resources+`
`, 1)
}

// Real PostgreSQL: the step fails without a message, on the infrastructure's account, and says
// which pool.
func TestAStepWhosePoolWillNotRunTheNamespaceFailsUnpublished(t *testing.T) {
	for _, c := range []struct {
		name string
		pool string
		says string
	}{
		{"a pool that does not accept finance", "ops", "does not accept the namespace finance"},
		{"a pool nobody created", "nowhere", "does not exist"},
	} {
		t.Run(c.name, func(t *testing.T) {
			core, q, pool, super := decidingOn(t, onPool(c.pool, `{ cpu: "1" }`))
			conn := dbtest.Superuser(t, super)
			if _, err := conn.Exec(t.Context(),
				`insert into runner_pools (name, accepted_namespaces, created_by) values ('ops', '{team-ops}', 'admin')`); err != nil {
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

			// And the step says why, naming the pool, and the run has ended on it.
			var reason string
			if err := conn.QueryRow(t.Context(),
				`select evaluation->'state'->'steps'->'normalize'->>'reason' from runs where id = $1`, decidedRun).
				Scan(&reason); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(reason, "runner pool "+c.pool) || !strings.Contains(reason, c.says) {
				t.Errorf("the step's reason is %q", reason)
			}
			if got := stateOf(t, core); got != agk.Failed {
				t.Errorf("the run is %s", got)
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
	document := strings.Replace(onPool("nowhere", `{ cpu: "1" }`), `    runs_on: [pool=nowhere]
`, `    runs_on: [pool=nowhere]
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

// "Resources are capped to the pool's ceilings on the task message": an ask above a ceiling is
// capped, an ask below one is left alone, and a step that asks for nothing where the pool sets a
// ceiling is given the ceiling, since nothing is more than any ceiling.
func TestAnOversizedAskIsCappedOnTheMessage(t *testing.T) {
	core, q, pool, super := decidingOn(t, onPool("small", `{ cpu: "8", memory: "256Mi" }`))
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `
		insert into runner_pools (name, accepted_namespaces, ceiling_cpu, ceiling_memory, ceiling_pids, created_by)
		values ('small', '{finance}', '2', '1Gi', 128, 'admin')`); err != nil {
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
