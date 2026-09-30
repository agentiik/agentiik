package controller

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/trigger"
	"github.com/jackc/pgx/v5"
)

// Calling a workflow from a workflow: "a workflow: step starts a run of the workflow it calls".

// byWorkflow answers each workflow's one graph, whichever commit is asked, and no version of a
// workflow it holds none of.
type byWorkflow map[string]*graph.Graph

func (b byWorkflow) Graph(_ context.Context, namespace, workflow, _ string) (*graph.Graph, error) {
	g, ok := b[namespace+"/"+workflow]
	if !ok {
		return nil, db.ErrNoVersion
	}
	return g, nil
}

// theCaller calls common with the cycle it was started with, and archives what the call answered.
const theCaller = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  cycle: { schema: { type: string } }
outputs:
  invoices: { from: { step: archive, port: ok } }
steps:
  remind:
    workflow: %s
    inputs:
      cycle: ${{ workflow.inputs.cycle }}
  archive:
    image: ` + theImage + `
    needs:
      - { step: remind, port: report, as: orders }
    outputs: [ok]
`

// theCallee is what theCaller calls: one step, fed the cycle it is handed, publishing the report.
const theCallee = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: common, namespace: finance }
inputs:
  cycle: { schema: { type: string }, required: true }
outputs:
  report: { from: { step: send, port: ok } }
steps:
  send:
    image: ` + theImage + `
    inputs:
      orders: '${{ [{"cycle": workflow.inputs.cycle}] }}'
    outputs: [ok]
`

const calledCommit = "c0ffee1"

// calling stands up a controller deciding runs of both, with the one path a run takes, and starts
// a run of the caller, by alice, over cycle; callee names what remind calls.
func calling(t *testing.T, callee string) (*Core, *fakeQueue, *pgx.Conn) {
	t.Helper()
	clock.set(time.Date(2026, 9, 14, 6, 0, 0, 0, time.UTC))
	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	for _, stmt := range []string{
		`insert into namespaces (name) values ('finance'), ('ops')`,
		`insert into workflows (namespace, name) values ('finance', 'monthly-invoicing'), ('finance', 'common'), ('ops', 'common')`,
		`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at) values
		   ('finance', 'monthly-invoicing', 'a3f9c1e', '{}', 'alice', now()),
		   ('finance', 'common', '` + calledCommit + `', '{}', 'alice', now()),
		   ('ops', 'common', '` + calledCommit + `', '{}', 'alice', now())`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	mayRun(t, conn, "alice", "finance")

	m, err := brick.ParseManifest([]byte(theManifest))
	if err != nil {
		t.Fatal(err)
	}
	built := func(document string) *graph.Graph {
		wf, err := graph.Parse([]byte(document))
		if err != nil {
			t.Fatal(err)
		}
		g, err := graph.Build(wf, map[string]brick.Manifest{theImage: m})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	calledGraph := built(theCallee)
	versions := byWorkflow{
		"finance/monthly-invoicing": built(strings.Replace(theCaller, "%s", callee, 1)),
		"finance/common":            calledGraph,
		"ops/common":                calledGraph,
	}

	c, err := New(pool, "calling")
	if err != nil {
		t.Fatal(err)
	}
	c.Trouble = func(_ agk.RunID, err error) { t.Logf("reported: %s", err) }
	term, err := pool.BeginTerm(t.Context(), "calling")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("", "agk-objects-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	starter, err := trigger.New(trigger.Options{Pool: pool, Versions: versions, Objects: artifact.Dir(root), Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	q := &fakeQueue{}
	core, err := NewCore(c, term, Options{
		Queue: q, Versions: versions, Objects: artifact.Dir(root), Now: clock.now, Starter: starter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.CreateRun(ctx, db.NewRun{
			ID: decidedRun, Workflow: "monthly-invoicing", Commit: "a3f9c1e",
			Trigger: agk.TriggerManual, TriggeredBy: "alice",
			Inputs: json.RawMessage(`{"cycle": "standard"}`), Steps: []agk.Step{"remind", "archive"},
		})
	}); err != nil {
		t.Fatal(err)
	}
	return core, q, conn
}

// child is the run remind's call started, as its row reads.
type child struct {
	id, namespace, workflow, trigger, by, from, step, state, inputs string
	depth                                                           int
}

func childOf(t *testing.T, conn *pgx.Conn) (child, bool) {
	t.Helper()
	var c child
	err := conn.QueryRow(t.Context(),
		`select id, namespace, workflow, trigger, coalesce(triggered_by, ''), caller_run, caller_step, state, inputs::text, depth
		 from runs where caller_run = $1`, string(decidedRun)).
		Scan(&c.id, &c.namespace, &c.workflow, &c.trigger, &c.by, &c.from, &c.step, &c.state, &c.inputs, &c.depth)
	if err == pgx.ErrNoRows {
		return child{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return c, true
}

func (co *Core) decide(t *testing.T, run agk.RunID) {
	t.Helper()
	if err := co.Decide(t.Context(), run); err != nil {
		t.Fatal(err)
	}
}

// A call starts a run of the workflow it names, attributed to whoever the calling run is, with the
// inputs it hands over, one call deep, and runs no container; the called run's end is its end, and
// what it published is the step's ports, which the step below reads.
func TestACallStartsARunAndEndsAsItDoes(t *testing.T) {
	core, q, conn := calling(t, "finance/common")
	core.decide(t, decidedRun)
	if taken := q.taken(); len(taken) != 0 {
		t.Fatalf("the call handed out %+v, and a call runs no container", taken)
	}
	c, ok := childOf(t, conn)
	if !ok {
		t.Fatal("the call started no run")
	}
	if c.namespace != "finance" || c.workflow != "common" || c.trigger != "workflow" || c.by != "alice" ||
		c.step != "remind" || c.depth != 1 || c.inputs != `{"cycle": "standard"}` {
		t.Errorf("the call started %+v", c)
	}
	var called, published string
	if err := conn.QueryRow(t.Context(),
		`select coalesce(called_run, ''), coalesce(published_at::text, '') from tasks where run_id = $1 and step = 'remind'`,
		string(decidedRun)).Scan(&called, &published); err != nil {
		t.Fatal(err)
	}
	if called != c.id || published != "" {
		t.Errorf("remind's dispatch names the run %q and was published at %q: a call holds no slot", called, published)
	}
	// A pass that finds the call made makes no second one.
	core.decide(t, decidedRun)
	var calls int
	if err := conn.QueryRow(t.Context(), `select count(*) from runs where caller_run = $1`, string(decidedRun)).Scan(&calls); err != nil || calls != 1 {
		t.Fatalf("%d runs were called: %v", calls, err)
	}

	// The called run runs, and ends; the caller is woken, and remind ends with what it published.
	core.decide(t, agk.RunID(c.id))
	sent := q.taken()
	if len(sent) != 1 || sent[0].Step != "send" {
		t.Fatalf("the called run handed out %+v", sent)
	}
	core.answer(t, succeeded(t, sent[0], core.now()))
	if c, _ := childOf(t, conn); c.state != "succeeded" {
		t.Fatalf("the called run is %s", c.state)
	}
	var wake *time.Time
	if err := conn.QueryRow(t.Context(), `select wake_at from runs where id = $1`, string(decidedRun)).Scan(&wake); err != nil || wake == nil {
		t.Errorf("the caller was not woken: %v", err)
	}
	core.decide(t, decidedRun)
	archive := q.taken()
	if len(archive) != 1 || archive[0].Step != "archive" {
		t.Fatalf("after the call the caller handed out %+v", archive)
	}
	fed := archive[0].Inputs["orders"]
	if len(fed.Items) != 1 || fed.Items[0].Data["customer_id"] != "C-1042" {
		t.Errorf("archive is fed %+v, what the called run published", fed)
	}
	var state string
	if err := conn.QueryRow(t.Context(), `select state from tasks where run_id = $1 and step = 'remind'`, string(decidedRun)).Scan(&state); err != nil || state != "succeeded" {
		t.Errorf("remind's dispatch is %s: %v", state, err)
	}
}

// A called run that fails fails the step that called it, as a brick's failure does, and with it the
// run, the step below never starting.
func TestACallFailsAsTheRunItCalledFails(t *testing.T) {
	core, q, conn := calling(t, "finance/common")
	core.decide(t, decidedRun)
	c, _ := childOf(t, conn)
	core.decide(t, agk.RunID(c.id))
	sent := q.taken()
	failed := succeeded(t, sent[0], core.now())
	failed.State, failed.ExitCode, failed.Outputs = agk.TaskFailed, 1, nil
	core.answer(t, failed)
	core.decide(t, decidedRun)
	if taken := q.taken(); len(taken) != 0 {
		t.Errorf("a failed call started %+v", taken)
	}
	if got := stateOf(t, core); got != agk.Failed {
		t.Errorf("the caller is %s", got)
	}
	var reason string
	if err := conn.QueryRow(t.Context(), `select coalesce(reason, '') from steps where run_id = $1 and step = 'remind'`, string(decidedRun)).Scan(&reason); err == nil && !strings.Contains(reason, "ended failed") {
		t.Logf("remind reads %q", reason)
	}
}

// "Require workflow:run on a callee in another namespace, and answer the same 404 if it is
// invisible": a workflow alice may not run and one that does not exist fail the call alike, and
// start nothing; and "one call past the limit fails at the call", not the run.
func TestACallThatCannotBeMadeFailsTheStepAndStartsNothing(t *testing.T) {
	var said []string
	for _, callee := range []string{"ops/common", "finance/nothing"} {
		core, q, conn := calling(t, callee)
		core.decide(t, decidedRun)
		if _, ok := childOf(t, conn); ok {
			t.Errorf("a call of %s started a run", callee)
		}
		if taken := q.taken(); len(taken) != 0 {
			t.Errorf("a call of %s handed out %+v", callee, taken)
		}
		if got := stateOf(t, core); got != agk.Failed {
			t.Errorf("a run calling %s is %s", callee, got)
		}
		said = append(said, strings.Replace(reasonOf(t, core), callee, "the workflow", 1))
	}
	if said[0] != said[1] || !strings.Contains(said[0], "names no workflow that alice may run") {
		t.Errorf("a call alice may not make fails with %q, and one of nothing with %q", said[0], said[1])
	}

	core, _, conn := calling(t, "finance/common")
	if _, err := conn.Exec(t.Context(),
		`update runs set trigger = 'workflow', caller_run = '01M2Z8V1P9C4XQ7K2N4D6F8H0D', caller_step = 'loop',
		   caller_task = 'far-up', depth = $2 where id = $1`, string(decidedRun), agk.DefaultMaxCallDepth); err != nil {
		t.Fatal(err)
	}
	core.decide(t, decidedRun)
	if _, ok := childOf(t, conn); ok {
		t.Error("a call past the limit started a run")
	}
	if why := reasonOf(t, core); !strings.Contains(why, "a chain of calls is at most 8 deep") {
		t.Errorf("a call past the limit fails with %q", why)
	}
}

// reasonOf is why remind's last dispatch failed, as the run's document holds it.
func reasonOf(t *testing.T, co *Core) string {
	t.Helper()
	var e db.Evaluation
	if err := co.controller.Fenced(t.Context(), co.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		e, err = w.Run(ctx, decidedRun)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		State graph.State `json:"state"`
	}
	if err := json.Unmarshal(e.Document, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.State.Steps["remind"].Reason
}

// A caller cancelled cancels the run its call made.
func TestACallerCancelledCancelsTheRunItCalled(t *testing.T) {
	core, _, conn := calling(t, "finance/common")
	core.decide(t, decidedRun)
	if _, err := conn.Exec(t.Context(), `update runs set cancel_requested_at = now() where id = $1`, string(decidedRun)); err != nil {
		t.Fatal(err)
	}
	core.decide(t, decidedRun)
	var asked *time.Time
	if err := conn.QueryRow(t.Context(), `select cancel_requested_at from runs where caller_run = $1`, string(decidedRun)).Scan(&asked); err != nil || asked == nil {
		t.Fatalf("the called run was not asked to cancel: %v", err)
	}
	c, _ := childOf(t, conn)
	core.decide(t, agk.RunID(c.id))
	if c, _ := childOf(t, conn); c.state != "cancelled" {
		t.Errorf("the called run is %s", c.state)
	}
}
