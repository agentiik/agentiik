package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// The loop, end to end: a run created in a namespace, decided by a controller that serves every
// namespace, handed to a queue, answered, and decided again until it ends. The queue is fake
// and the database is real, which is the split the whole design rests on: what a fake cannot be
// wrong about is the part that is in Go, and what a fake would be wrong about is exactly the
// part PostgreSQL enforces.

const theImage = "ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc"

const theWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: archive, port: ok } }
steps:
  normalize:
    image: ` + theImage + `
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
  archive:
    image: ` + theImage + `
    needs:
      - { step: normalize, port: ok, as: orders }
    outputs: [ok]
`

const theManifest = `
apiVersion: agentiik.dev/v1
kind: Brick
metadata: { name: invoice, version: 1.0.0 }
spec:
  inputs:
    orders: {}
  outputs:
    ok: {}
    rejected: {}
  runtime: { user: "65532:65532" }
`

// oneVersion answers the same graph for every commit, which is what a Versions implementation
// does once a version is resolved: a graph is immutable for the life of one.
type oneVersion struct{ g *graph.Graph }

func (o oneVersion) Graph(context.Context, string, string, string) (*graph.Graph, error) {
	return o.g, nil
}

// fakeQueue is the whole of the bus as this package sees it.
type fakeQueue struct {
	mu        sync.Mutex
	published []graph.Task
	stopped   []graph.Stop
	refuse    error
}

func (q *fakeQueue) Publish(_ context.Context, t graph.Task) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.refuse != nil {
		return q.refuse
	}
	q.published = append(q.published, t)
	return nil
}

func (q *fakeQueue) Stop(_ context.Context, s graph.Stop) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.stopped = append(q.stopped, s)
	return nil
}

func (q *fakeQueue) taken() []graph.Task {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.published
	q.published = nil
	return out
}

// deciding stands up everything a Core needs: a database with a namespace and a workflow, an
// object store in a directory, a graph, a fake queue and a term.
func deciding(t *testing.T) (*Core, *fakeQueue, *db.Pool, string) {
	t.Helper()
	pool, super := dbtest.Open(t)

	conn := dbtest.Superuser(t, super)
	for _, stmt := range []string{
		`insert into namespaces (name) values ('finance')`,
		`insert into workflows (namespace, name) values ('finance', 'monthly-invoicing')`,
		`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at)
		   values ('finance', 'monthly-invoicing', 'a3f9c1e', '{}', 'alice', now())`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}

	wf, err := graph.Parse([]byte(theWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	m, err := brick.ParseManifest([]byte(theManifest))
	if err != nil {
		t.Fatal(err)
	}
	manifests := map[string]brick.Manifest{}
	for _, image := range graph.Images(wf) {
		manifests[image] = m
	}
	g, err := graph.Build(wf, manifests)
	if err != nil {
		t.Fatal(err)
	}

	c, err := New(pool, "deciding")
	if err != nil {
		t.Fatal(err)
	}
	var trouble []error
	c.Trouble = func(_ agk.RunID, err error) { trouble = append(trouble, err) }
	t.Cleanup(func() {
		for _, err := range trouble {
			t.Logf("reported: %s", err)
		}
	})

	term, err := pool.BeginTerm(t.Context(), "deciding")
	if err != nil {
		t.Fatal(err)
	}

	q := &fakeQueue{}
	root, err := os.MkdirTemp("", "agk-objects-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	at := time.Date(2026, 9, 14, 6, 0, 0, 0, time.UTC)
	core, err := NewCore(c, term, Options{
		Queue: q, Versions: oneVersion{g}, Objects: artifact.Dir(root),
		Now: func() time.Time { return at },
	})
	if err != nil {
		t.Fatal(err)
	}
	return core, q, pool, super
}

const decidedRun agk.RunID = "01M2Z8V1P9C4XQ7K2N4D6F8H0C"

func jsonOf(v any) ([]byte, error) { return json.Marshal(v) }

// resumeOn builds a second controller against the same database, holding a term of its own, with
// nothing carried over from the first but what is written down.
func resumeOn(t *testing.T, pool *db.Pool, super string, like *Core) (*Core, *fakeQueue, *db.Pool, string) {
	t.Helper()
	c, err := New(pool, "resumed")
	if err != nil {
		t.Fatal(err)
	}
	c.Trouble = func(_ agk.RunID, err error) { t.Logf("reported: %s", err) }
	term, err := pool.BeginTerm(t.Context(), "resumed")
	if err != nil {
		t.Fatal(err)
	}
	q := &fakeQueue{}
	core, err := NewCore(c, term, Options{
		Queue: q, Versions: like.versions, Objects: like.objects, Now: like.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return core, q, pool, super
}

func createRun(t *testing.T, pool *db.Pool) {
	t.Helper()
	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.CreateRun(ctx, db.NewRun{
			ID: decidedRun, Workflow: "monthly-invoicing", Commit: "a3f9c1e",
			Trigger: agk.TriggerManual, TriggeredBy: "alice",
			Inputs: map[string]any{"orders": []any{map[string]any{"customer_id": "C-1042"}}},
			Steps:  []agk.Step{"normalize", "archive"},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The first pass, which is the documentation's own sequence: the controller evaluates the graph
// and finds the steps whose every declared input port is satisfied, creates one task per shard,
// and publishes it. archive is not among them, because its port is not satisfied.
func TestTheFirstPassPublishesWhatIsReady(t *testing.T) {
	core, q, pool, _ := deciding(t)
	createRun(t, pool)

	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}

	taken := q.taken()
	if len(taken) != 1 || taken[0].Step != "normalize" {
		t.Fatalf("the first pass published %d tasks: %+v", len(taken), taken)
	}
	if taken[0].Run != decidedRun || taken[0].Namespace != "finance" {
		t.Errorf("the task says run %s of namespace %s", taken[0].Run, taken[0].Namespace)
	}
	if taken[0].Image != theImage {
		t.Errorf("the task names image %q", taken[0].Image)
	}

	// And the message is stamped as gone, so the outbox has nothing to repair.
	if err := pool.Installation(t.Context(), db.ControllerSweep, func(ctx context.Context, w *db.Wide) error {
		left, err := w.Unpublished(ctx, "finance", decidedRun)
		if err != nil {
			return err
		}
		if len(left) != 0 {
			t.Errorf("after publishing, %d tasks are still unstamped: %v", len(left), left)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// answer feeds one result back the way a runner would, by writing it into the state through the
// evaluator. It is the half of the loop the bus group will bring, faked here so that the rest of
// it can be run end to end today.
func (co *Core) answer(t *testing.T, r graph.Result) {
	t.Helper()
	var e db.Evaluation
	if err := co.controller.Fenced(t.Context(), co.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		e, err = w.Run(ctx, decidedRun)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	g, err := co.versions.Graph(t.Context(), e.Namespace, e.Workflow, e.Commit)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := co.resume(t.Context(), e, g, co.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Record(r, co.now()); err != nil {
		t.Fatal(err)
	}
	state := ev.State()
	doc, err := Elide(t.Context(), state, e.Namespace, co.objects)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := jsonOf(doc)
	if err != nil {
		t.Fatal(err)
	}
	steps, tasks := project(state)
	if err := co.controller.Fenced(t.Context(), co.term, func(ctx context.Context, w *db.Wide) error {
		return w.SaveDecision(ctx, db.Decision{
			Namespace: e.Namespace, Run: decidedRun, Was: e.Seq, Seq: state.Seq,
			Document: encoded, State: state.Run.State,
			StartedAt: state.Run.StartedAt, FinishedAt: state.Run.FinishedAt,
			Steps: steps, Tasks: tasks,
		})
	}); err != nil {
		t.Fatal(err)
	}
}

// succeeded is what a runner sends back for a task that worked, with one item on ok.
func succeeded(t *testing.T, task graph.Task, at time.Time) graph.Result {
	t.Helper()
	outputs := map[agk.Port]agk.Envelope{}
	for _, port := range task.Outputs {
		e := agk.Empty(task.Run, task.Step, port, task.Attempt, at)
		if port == "ok" {
			e.Items = []agk.Item{agk.NewItem(map[string]any{"customer_id": "C-1042"})}
			e.Meta.Count = 1
		}
		outputs[port] = e
	}
	return graph.Result{
		Task: task.ID, State: agk.TaskSucceeded, ExitCode: 0, Outputs: outputs,
		DispatchedAt: at, StartedAt: at, FinishedAt: at,
	}
}

// The whole path, in the documentation's own order: steps three to seven repeat until nothing is
// runnable, at which point the run reaches a terminal state.
func TestARunGoesFromTheFirstStepToATerminalState(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)

	for pass := 1; pass <= 6; pass++ {
		if err := core.Decide(t.Context(), decidedRun); err != nil {
			t.Fatalf("pass %d: %s", pass, err)
		}
		taken := q.taken()
		if len(taken) == 0 {
			break
		}
		for _, task := range taken {
			core.answer(t, succeeded(t, task, core.now()))
		}
	}

	var state agk.RunState
	var outputs map[string]any
	if err := core.controller.Fenced(t.Context(), core.term, func(ctx context.Context, w *db.Wide) error {
		e, err := w.Run(ctx, decidedRun)
		if err != nil {
			return err
		}
		state = e.State
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if state != agk.Succeeded {
		t.Fatalf("the run ended in %s", state)
	}

	// The projection says the same thing. Read as the superuser, because a namespaced
	// handle hands out no way to run a statement and that is the point of it: what a test
	// wants here is to look behind the doors rather than through them.
	conn := dbtest.Superuser(t, super)
	rows, err := conn.Query(t.Context(),
		`select step, state from steps where run_id = $1 order by step`, string(decidedRun))
	if err != nil {
		t.Fatal(err)
	}
	verdicts := map[string]string{}
	for rows.Next() {
		var step, verdict string
		if err := rows.Scan(&step, &verdict); err != nil {
			t.Fatal(err)
		}
		verdicts[step] = verdict
	}
	rows.Close()
	if verdicts["normalize"] != "succeeded" || verdicts["archive"] != "succeeded" {
		t.Errorf("the steps ended %v", verdicts)
	}

	// And one task row per step, each carrying the exit code of a container that decided
	// something.
	var tasks, coded int
	if err := conn.QueryRow(t.Context(),
		`select count(*), count(exit_code) from tasks where run_id = $1`, string(decidedRun)).
		Scan(&tasks, &coded); err != nil {
		t.Fatal(err)
	}
	if tasks != 2 || coded != 2 {
		t.Errorf("the run left %d tasks, %d of them carrying an exit code", tasks, coded)
	}
	_ = outputs
}

// "Failover is a state resume, never a rebuild: the state lives in the database, not in the
// process." So a controller that throws everything away and reads the run back gets the plan the
// one that died would have got.
func TestAFailoverResumesRatherThanRebuilds(t *testing.T) {
	first, q, pool, super := deciding(t)
	createRun(t, pool)

	if err := first.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	taken := q.taken()
	if len(taken) != 1 {
		t.Fatalf("the first pass published %d tasks", len(taken))
	}
	first.answer(t, succeeded(t, taken[0], first.now()))

	// Everything the first controller held is now gone. A second one takes the term and
	// picks the run up with nothing but what is written down.
	second, q2, _, _ := resumeOn(t, pool, super, first)
	if err := second.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	after := q2.taken()
	if len(after) != 1 || after[0].Step != "archive" {
		t.Fatalf("the resumed controller published %d tasks: %+v", len(after), after)
	}

	// And it published the step whose input port the first controller's work satisfied,
	// with the items that work produced, which is the whole of what a resume has to carry.
	in, ok := after[0].Inputs["orders"]
	if !ok {
		t.Fatalf("the resumed task has inputs %v", after[0].Inputs)
	}
	if len(in.Items) != 1 {
		t.Errorf("the resumed task's input port carries %d items, and the step it came from published one", len(in.Items))
	}
	if err := in.Validate(agk.DefaultLimits()); err != nil {
		t.Errorf("the resumed task's input envelope does not hold together: %s", err)
	}
}

// "Envelopes and logs are not stored in the database: it keeps only their digests and URIs." So
// what is in the column holds no items, and what comes back out of it does.
func TestTheStoredDocumentHoldsNoItems(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)

	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	for _, task := range q.taken() {
		core.answer(t, succeeded(t, task, core.now()))
	}

	conn := dbtest.Superuser(t, super)
	var stored []byte
	if err := conn.QueryRow(t.Context(),
		`select evaluation from runs where id = $1`, string(decidedRun)).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	// The customer identifier is in the run's declared inputs as well, which the chapter
	// puts in the database on purpose, so what is looked for here is an item: an envelope
	// item carries an identifier of its own and a files list, and neither belongs in a
	// column.
	if bytes.Contains(stored, []byte(`"files"`)) {
		t.Error("the stored document carries an envelope item, and the database keeps only digests and URIs")
	}

	var doc Document
	if err := json.Unmarshal(stored, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Envelopes) == 0 {
		t.Fatal("the document names no envelopes, and a step published two ports")
	}
	for _, ref := range doc.Envelopes {
		if len(ref.Digest) != 64 || ref.Size <= 0 {
			t.Errorf("an envelope reference reads %+v", ref)
		}
	}

	// The hollow envelopes in it are recognisable as hollow rather than as empty batches.
	hollowed := 0
	for _, st := range doc.State.Steps {
		for _, e := range envelopesOf(st) {
			if len(e.Items) != 0 {
				t.Errorf("an envelope of %s was stored with its items", e.Meta.Step)
			}
			if e.Meta.Count == 0 {
				continue
			}
			hollowed++
			if e.Validate(agk.DefaultLimits()) == nil {
				t.Errorf("a hollow envelope of %s on %s validates, so nothing would catch a state that was never put back together", e.Meta.Step, e.Meta.Port)
			}
		}
	}
	if hollowed == 0 {
		t.Error("no envelope in the document was hollowed, and a step published one with an item in it")
	}

	// And putting it back gives the items again.
	state, err := Rehydrate(t.Context(), doc, "finance", core.objects, agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, st := range state.Steps {
		for _, e := range envelopesOf(st) {
			if len(e.Items) > 0 {
				found++
			}
			if err := e.Validate(agk.DefaultLimits()); err != nil {
				t.Errorf("a rehydrated envelope does not hold together: %s", err)
			}
		}
	}
	if found == 0 {
		t.Error("nothing came back with items in it")
	}
}

// Elide is deterministic, because a document written twice from one state has to be the same
// bytes twice: a jsonb column rewritten with the same content and a different key order is a
// diff nobody can read and a write nobody needed.
func TestElidingTwiceWritesTheSameDocument(t *testing.T) {
	core, q, pool, _ := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	for _, task := range q.taken() {
		core.answer(t, succeeded(t, task, core.now()))
	}

	var e db.Evaluation
	if err := core.controller.Fenced(t.Context(), core.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		e, err = w.Run(ctx, decidedRun)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var doc Document
	if err := json.Unmarshal(e.Document, &doc); err != nil {
		t.Fatal(err)
	}
	state, err := Rehydrate(t.Context(), doc, "finance", core.objects, agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	var first []byte
	for i := range 3 {
		again, err := Elide(t.Context(), state, "finance", core.objects)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(again)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = b
			continue
		}
		if !bytes.Equal(first, b) {
			t.Fatalf("eliding the same state twice wrote two documents:\n%s\n%s", first, b)
		}
	}

	// And the state it was given is untouched: the evaluator holds that pointer.
	for _, st := range state.Steps {
		for port, env := range st.Ports {
			if env.Meta.Count > 0 && len(env.Items) == 0 {
				t.Errorf("eliding hollowed the caller's own state on %s", port)
			}
		}
	}
}

// A state whose envelope is gone from the store is an error and not an empty batch: a run
// resumed without the bytes of a published port would schedule against nothing and call it
// success.
func TestAResumeRefusesAnEnvelopeThatIsGone(t *testing.T) {
	core, q, pool, _ := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	for _, task := range q.taken() {
		core.answer(t, succeeded(t, task, core.now()))
	}

	var e db.Evaluation
	if err := core.controller.Fenced(t.Context(), core.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		e, err = w.Run(ctx, decidedRun)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var doc Document
	if err := json.Unmarshal(e.Document, &doc); err != nil {
		t.Fatal(err)
	}
	// A digest nothing holds, which is what a collected object looks like from here.
	doc.Envelopes[0].Digest = strings.Repeat("0", 64)
	if _, err := Rehydrate(t.Context(), doc, "finance", core.objects, agk.DefaultLimits()); err == nil {
		t.Fatal("a state whose envelope is gone was resumed anyway")
	}

	// And a document of a version this process does not write is refused rather than read.
	doc.Version = DocumentVersion + 1
	if _, err := Rehydrate(t.Context(), doc, "finance", core.objects, agk.DefaultLimits()); err == nil {
		t.Fatal("a document of another version was read")
	}
}

// A queue that refuses leaves the message unsent and the decision standing, which is what the
// outbox is for: the sweep finds the task and publishes it again.
func TestAQueueThatRefusesLeavesTheSweepSomethingToDo(t *testing.T) {
	core, q, pool, _ := deciding(t)
	createRun(t, pool)

	q.refuse = errors.New("the bus is not there")
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatalf("a decision failed because the bus did: %s", err)
	}

	var waiting []agk.TaskID
	var actionable []agk.RunID
	if err := core.controller.Fenced(t.Context(), core.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		if waiting, err = w.Unpublished(ctx, "finance", decidedRun); err != nil {
			return err
		}
		actionable, err = w.Actionable(ctx, core.now().Add(time.Hour), 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 1 {
		t.Fatalf("after a refused publish, %d tasks are waiting to be sent", len(waiting))
	}
	if !holds(actionable, decidedRun) {
		t.Error("a run holding a task whose message never went is not actionable, and nothing else will ever send it")
	}

	// The bus comes back, and the sweep sends it.
	q.refuse = nil
	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	if got := q.taken(); len(got) != 1 {
		t.Fatalf("the sweep published %d tasks", len(got))
	}
}

// envelopesOf is every envelope a step state holds, published or per shard. A shard's outputs
// sit there until the next pass publishes the step, so a test looking only at the publication
// looks too early.
func envelopesOf(st graph.StepState) []agk.Envelope {
	var out []agk.Envelope
	for _, e := range st.Ports {
		out = append(out, e)
	}
	for _, sh := range st.Shards {
		for _, e := range sh.Ports {
			out = append(out, e)
		}
	}
	return out
}

func holds(ids []agk.RunID, want agk.RunID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
