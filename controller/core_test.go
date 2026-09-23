package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
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
	"github.com/agentiik/agentiik/internal/token"
	"github.com/jackc/pgx/v5"
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
	published []Dispatch
	stopped   []graph.Stop
	refuse    error
}

func (q *fakeQueue) Publish(_ context.Context, d Dispatch) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.refuse != nil {
		return q.refuse
	}
	q.published = append(q.published, d)
	issued.Store(d.Row, d.Grant)
	return nil
}

// issued is every grant that went out, by the row it was issued for, which is what a runner taking
// the message has in its hands. Kept outside any one queue because a failover hands the answer to
// a controller that did not publish the task, and by row because a row is minted once and never
// shared between two tests.
var issued sync.Map

func (q *fakeQueue) Stop(_ context.Context, s graph.Stop) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.stopped = append(q.stopped, s)
	return nil
}

func (q *fakeQueue) stops() []graph.Stop {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.stopped
	q.stopped = nil
	return out
}

// taken answers the tasks that went out, which is what most of these tests are about. The
// dispatch around each one is checked where it matters rather than everywhere.
func (q *fakeQueue) taken() []graph.Task {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]graph.Task, len(q.published))
	for i, d := range q.published {
		out[i] = d.Task
	}
	q.published = nil
	return out
}

// dispatched answers the whole of what went out, for the tests that are about the grant and the
// input digests rather than about the task.
func (q *fakeQueue) dispatched() []Dispatch {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.published
	q.published = nil
	return out
}

// deciding stands up everything a Core needs: a database with a namespace and a workflow, an
// object store in a directory, a graph, a fake queue and a term.
func deciding(t *testing.T) (*Core, *fakeQueue, *db.Pool, string) {
	return decidingOn(t, theWorkflow)
}

func decidingOn(t *testing.T, document string) (*Core, *fakeQueue, *db.Pool, string) {
	t.Helper()
	clock.set(time.Date(2026, 9, 14, 6, 0, 0, 0, time.UTC))
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

	wf, err := graph.Parse([]byte(document))
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

	core, err := NewCore(c, term, Options{
		Queue: q, Versions: oneVersion{g}, Objects: artifact.Dir(root),
		Now: clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return core, q, pool, super
}

// clock is the one a test moves. A backoff places an attempt at a moment in the future and a
// deadline places the end of a run at one, so a test that could not move time would be a test
// that could only watch the first half of both.
var clock = &movable{at: time.Date(2026, 9, 14, 6, 0, 0, 0, time.UTC)}

type movable struct {
	mu sync.Mutex
	at time.Time
}

func (m *movable) now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.at
}

func (m *movable) set(at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.at = at
}

func (m *movable) advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.at = m.at.Add(d)
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
			Inputs: json.RawMessage(`{"orders": [{"customer_id": "C-1042"}]}`),
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

// row is the task_id of the latest dispatch of a key, which is the one the runner answering it
// in these tests took.
func (co *Core) row(t *testing.T, key agk.TaskID) string {
	t.Helper()
	var row string
	if err := co.controller.Fenced(t.Context(), co.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		row, err = w.TaskRow(ctx, "finance", key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return row
}

// answer feeds one result back the way the bus will, through the door a bus consumer calls.
func (co *Core) answer(t *testing.T, r graph.Result) {
	t.Helper()
	if err := co.Answer(t.Context(), co.answerOf(t, r)); err != nil {
		t.Fatal(err)
	}
}

// theRunner is the machine these tests have holding their tasks.
const theRunner = "runner-dmz-02"

// answerOf is what the runner holding a task says about r, as the bus hands it on.
//
// The runner holds it because it redeemed its grant, and redeems it here as theRunner where nobody
// has yet; a task somebody else redeemed is answered by whoever did. Its envelopes are uploaded
// first and named by digest, and the instant it was handed out is left out, since that is the
// controller's to know and not the runner's to say.
func (co *Core) answerOf(t *testing.T, r graph.Result) Answer {
	t.Helper()
	ctx := t.Context()
	log, err := agk.NewLogURI(r.Task)
	if err != nil {
		t.Fatal(err)
	}
	a := Answer{
		Runner: theRunner,
		Log:    log, LogLines: 412,
		Usage: map[string]any{"cpu_seconds": 12.4, "max_rss_bytes": 198443008, "image_pull_ms": 0},
	}
	for _, port := range sortedPorts(r.Outputs) {
		e := r.Outputs[port]
		digest, _, err := artifact.PutEnvelope(ctx, co.objects, "finance", e)
		if err != nil {
			t.Fatal(err)
		}
		a.Outputs = append(a.Outputs, Output{Port: port, Digest: digest, Items: e.Meta.Count})
	}
	r.Outputs, r.DispatchedAt = nil, time.Time{}
	a.Result = r

	if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		row, err := w.TaskRow(ctx, "finance", r.Task)
		if err != nil {
			return err
		}
		a.Row = row
		holder, err := w.HeldBy(ctx, "finance", r.Task, row)
		if err != nil || holder != "" {
			a.Runner = holder
			return err
		}
		grant, ok := issued.Load(row)
		if !ok {
			return fmt.Errorf("no grant went out for %s, so no runner can hold it", r.Task)
		}
		_, err = w.Redeem(ctx, grant.(string), r.Task, theRunner, co.now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return a
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

	// One decision starts it, and every answer decides it again, which is the loop the page
	// describes: steps three to seven repeat until nothing is runnable.
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 6; pass++ {
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

	// Everything the first controller held is now gone, and the result of the work it
	// planned comes back to a second one that never saw it planned. That is the failover
	// worth testing: the instance that takes the answer is not the instance that asked.
	second, q2, _, _ := resumeOn(t, pool, super, first)
	second.answer(t, succeeded(t, taken[0], second.now()))
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

// What a result carries that the evaluator has no use for, recorded where a person can read it:
// who held the task, where its log went, how many lines there were and what it cost.
func TestAResultRecordsWhatOnlyTheRunnerKnows(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	taken := q.taken()
	if len(taken) != 1 {
		t.Fatalf("the first pass published %d tasks", len(taken))
	}
	core.answer(t, succeeded(t, taken[0], core.now()))

	conn := dbtest.Superuser(t, super)
	var runner, uri string
	var lines int
	var cut bool
	var usage []byte
	if err := conn.QueryRow(t.Context(),
		`select runner, log_uri, log_lines, log_truncated, usage from tasks
		 where idempotency_key = $1`, string(taken[0].ID)).
		Scan(&runner, &uri, &lines, &cut, &usage); err != nil {
		t.Fatal(err)
	}
	if runner != "runner-dmz-02" || lines != 412 || cut {
		t.Errorf("the task says runner %q, %d lines, cut %v", runner, lines, cut)
	}
	want, err := agk.NewLogURI(taken[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if uri != want.String() {
		t.Errorf("the log is at %q and the task wrote %q", uri, want)
	}
	var measured map[string]any
	if err := json.Unmarshal(usage, &measured); err != nil {
		t.Fatal(err)
	}
	if measured["cpu_seconds"] != 12.4 {
		t.Errorf("the usage came back as %v", measured)
	}

	// And a result for an attempt that is over changes nothing, because a bus is allowed to
	// deliver twice. No decision is written and the run is not decided again, so nothing is
	// published either: the first delivery already handed out what it made runnable.
	if got := q.taken(); len(got) != 1 || got[0].Step != "archive" {
		t.Fatalf("the result published %+v, want archive", got)
	}
	before := seqOf(t, conn)
	core.answer(t, succeeded(t, taken[0], core.now()))
	if after := seqOf(t, conn); after != before {
		t.Errorf("a result delivered twice took the run from seq %d to %d, and the second delivery is a duplicate rather than news", before, after)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("a result delivered twice published %+v again", got)
	}
	var tasks int
	if err := conn.QueryRow(t.Context(),
		`select count(*) from tasks where run_id = $1 and step = 'normalize'`, string(decidedRun)).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 {
		t.Errorf("a result delivered twice left %d task rows", tasks)
	}
}

// "A port produces exactly one envelope, once, when the emitting step ends", and what the
// database keeps of it is the digest. That is what makes the envelope purge able to act.
func TestAPublishedPortIsRecordedAsADigest(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	for _, task := range q.taken() {
		core.answer(t, succeeded(t, task, core.now()))
	}

	var ports db.Ports
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		var err error
		ports, err = ns.PublishedPorts(ctx, decidedRun, "normalize")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(ports) != 2 {
		t.Fatalf("normalize declares two ports and published %d: %+v", len(ports), ports)
	}
	ok, found := ports["ok"]
	if !found || len(ok.Digest) != 64 || ok.Size <= 0 {
		t.Fatalf("the ok port reads %+v", ok)
	}
	if ok.Items != 1 {
		t.Errorf("the ok port carries %d items, and the step published one", ok.Items)
	}
	// "An output port declared but never written by the container publishes an empty
	// envelope. That is not an error."
	if rejected := ports["rejected"]; rejected.Items != 0 || len(rejected.Digest) != 64 {
		t.Errorf("the port nothing was written to reads %+v", rejected)
	}

	// And every envelope the run references is counted, so the collector can see the
	// objects the controller put in the store.
	conn := dbtest.Superuser(t, super)
	var objects, refs int
	if err := conn.QueryRow(t.Context(),
		`select count(*), coalesce(sum(refs), 0) from artifact_objects where namespace = 'finance'`).
		Scan(&objects, &refs); err != nil {
		t.Fatal(err)
	}
	if objects == 0 || refs == 0 {
		t.Fatalf("the run left %d counted objects with %d references between them, and an object nothing counts is one the collector never sees", objects, refs)
	}
}

// The envelope purge, end to end from a decision: a run that has expired loses the count on
// every envelope it referenced, and what is left of them is collectable.
func TestTheEnvelopePurgeActsOnWhatTheRunReferenced(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 6; pass++ {
		taken := q.taken()
		if len(taken) == 0 {
			break
		}
		for _, task := range taken {
			core.answer(t, succeeded(t, task, core.now()))
		}
	}

	conn := dbtest.Superuser(t, super)
	var before int
	if err := conn.QueryRow(t.Context(),
		`select coalesce(sum(refs), 0) from artifact_objects where namespace = 'finance'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("the run referenced nothing")
	}

	if _, err := conn.Exec(t.Context(),
		`update runs set expires_at = now() - interval '1 minute' where id = $1`, string(decidedRun)); err != nil {
		t.Fatal(err)
	}
	purged, err := pool.PurgeEnvelopes(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 1 {
		t.Fatalf("the purge took %d runs", purged)
	}

	var after, collectable int
	if err := conn.QueryRow(t.Context(),
		`select coalesce(sum(refs), 0), count(*) filter (where collectable_at is not null)
		 from artifact_objects where namespace = 'finance'`).Scan(&after, &collectable); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Errorf("after the purge the run's envelopes are still counted %d times", after)
	}
	if collectable == 0 {
		t.Error("nothing became collectable, so the bytes would sit in the store for ever")
	}
}

func seqOf(t *testing.T, conn *pgx.Conn) int {
	t.Helper()
	var seq int
	if err := conn.QueryRow(t.Context(), `select seq from runs where id = $1`, string(decidedRun)).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

const retainingWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices:
    from: { step: archive, port: ok }
    retain: 90d
defaults:
  retain: 7d
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

// withAFile is a result whose envelope references an artifact, which is what a runner produces
// when a value goes past inline_max_bytes and is spilled to the store.
func withAFile(t *testing.T, task graph.Task, at time.Time, digest string) graph.Result {
	t.Helper()
	r := succeeded(t, task, at)
	e := r.Outputs["ok"]
	item := agk.NewItem(map[string]any{"customer_id": "C-1042"})
	item.Files = []agk.File{{
		Name:      "purchase-order.pdf",
		URI:       agk.URI{Run: task.Run, Step: task.Step, Port: "ok", Name: "purchase-order.pdf"},
		MediaType: "application/pdf",
		Size:      481233,
		SHA256:    digest,
	}}
	e.Items = []agk.Item{item}
	e.Meta.Count = 1
	r.Outputs["ok"] = e
	return r
}

// "retain is written on a workflow output or in defaults, never on a step. Everything travelling
// between two steps is an intermediate and lives by the workflow's default." So two artifacts on
// two ports of one run get two different expiries, and the controller is what knows which is
// which because it is what has the graph.
func TestAnArtifactLivesAsLongAsTheWorkflowDeclared(t *testing.T) {
	core, q, pool, super := decidingOn(t, retainingWorkflow)
	createRun(t, pool)

	intermediate, output := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 6; pass++ {
		taken := q.taken()
		if len(taken) == 0 {
			break
		}
		for _, task := range taken {
			digest := intermediate
			if task.Step == "archive" {
				digest = output
			}
			core.answer(t, withAFile(t, task, core.now(), digest))
		}
	}

	conn := dbtest.Superuser(t, super)
	for _, c := range []struct {
		step   string
		digest string
		days   float64
	}{
		// normalize publishes an intermediate, which lives by defaults.retain.
		{"normalize", intermediate, 7},
		// archive publishes the workflow output invoices, which declared its own.
		{"archive", output, 90},
	} {
		var lives float64
		var stored, media string
		var size int64
		if err := conn.QueryRow(t.Context(),
			`select extract(epoch from (expires_at - created_at)) / 86400, digest, media_type, size_bytes
			 from artifacts where run_id = $1 and step = $2`,
			string(decidedRun), c.step).Scan(&lives, &stored, &media, &size); err != nil {
			t.Fatalf("the artifact of %s: %s", c.step, err)
		}
		if lives < c.days-0.01 || lives > c.days+0.01 {
			t.Errorf("the artifact of %s lives %.2f days and the workflow declared %.0f", c.step, lives, c.days)
		}
		if stored != "sha256:"+c.digest || media != "application/pdf" || size != 481233 {
			t.Errorf("the artifact of %s reads %s, %s, %d bytes", c.step, stored, media, size)
		}
	}

	// And the object behind each is counted, so expiring the reference is what eventually
	// lets the bytes go and nothing else does.
	var counted int
	if err := conn.QueryRow(t.Context(),
		`select count(*) from artifact_objects where namespace = 'finance' and digest in ($1, $2)`,
		"sha256:"+intermediate, "sha256:"+output).Scan(&counted); err != nil {
		t.Fatal(err)
	}
	if counted != 2 {
		t.Errorf("%d of the two artifacts are counted", counted)
	}
}

// What leaves the controller carries the three things only the controller can add: the row the
// task is known by, the grant that turns its names into values, and the digest of every input.
func TestWhatLeavesCarriesItsGrantAndItsDigests(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.dispatched()
	if len(first) != 1 {
		t.Fatalf("the first pass dispatched %d tasks", len(first))
	}
	d := first[0]
	if d.Row == "" || d.Grant == "" {
		t.Fatalf("the dispatch reads %+v", d)
	}
	if task, ok := token.TaskOf(d.Grant); !ok || task != d.Row {
		t.Errorf("the grant names %q and the row is %q", task, d.Row)
	}

	// The grant is stored hashed and expires with its task: a copy of the table is not a set
	// of working credentials.
	conn := dbtest.Superuser(t, super)
	var hashed string
	var expires time.Time
	if err := conn.QueryRow(t.Context(),
		`select hash, expires_at from task_grants where task_id = $1`, d.Row).Scan(&hashed, &expires); err != nil {
		t.Fatal(err)
	}
	if hashed == d.Grant || len(hashed) != 64 {
		t.Errorf("the grant is stored as %q", hashed)
	}
	if !expires.After(core.now()) {
		t.Errorf("the grant expires at %s and the clock says %s", expires, core.now())
	}

	// And it can be redeemed once, by its own value and for its own task, and by nothing else.
	if err := core.controller.Fenced(t.Context(), core.term, func(ctx context.Context, w *db.Wide) error {
		got, err := w.Redeem(ctx, d.Grant, d.Task.ID, "runner-1", core.now())
		if err != nil {
			t.Errorf("the grant was refused for its own task: %s", err)
		} else if got.Namespace != "finance" {
			t.Errorf("the grant resolved to namespace %q", got.Namespace)
		}
		// And it says what the task was dispatched with, which is what the redemption
		// will be allowed to answer.
		if got.Scope.Run != d.Task.Run || got.Scope.Step != d.Task.Step {
			t.Errorf("the scope names run %s step %s", got.Scope.Run, got.Scope.Step)
		}
		// Including the version the run pinned, which is what names the tree the task
		// sees under /agk/repo: the runner is handed that commit's files and cannot ask
		// for another's.
		if got.Scope.Workflow != "monthly-invoicing" || got.Scope.Commit != "a3f9c1e" {
			t.Errorf("the scope names version %s@%s, and the run pinned monthly-invoicing@a3f9c1e", got.Scope.Workflow, got.Scope.Commit)
		}
		if _, err := w.Redeem(ctx, d.Grant, "01M2ZZZZZZZZZZZZZZZZZZZZZZ/other/1", "runner-1", core.now()); !errors.Is(err, db.ErrNoGrant) {
			t.Errorf("a grant redeemed for another task answered %v", err)
		}
		if _, err := w.Redeem(ctx, d.Grant, d.Task.ID, "runner-1", expires.Add(time.Second)); !errors.Is(err, db.ErrNoGrant) {
			t.Errorf("a grant past its expiry answered %v", err)
		}
		if _, err := w.Redeem(ctx, d.Grant+"x", d.Task.ID, "runner-1", core.now()); !errors.Is(err, db.ErrNoGrant) {
			t.Errorf("a grant that is nearly right answered %v", err)
		}
		// The task is held by the machine that redeemed it, and a second machine is
		// told so rather than starting a container for it.
		if _, err := w.Redeem(ctx, d.Grant, d.Task.ID, "runner-2", core.now()); !errors.Is(err, db.ErrTaskHeld) {
			t.Errorf("a second runner redeeming the same grant answered %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The second step's inputs are named by digest, and the bytes are in the store. The
	// first dispatch was drained above, so the answer is given against what it held.
	core.answer(t, succeeded(t, d.Task, core.now()))
	second := q.dispatched()
	if len(second) != 1 || second[0].Task.Step != "archive" {
		t.Fatalf("the second pass dispatched %+v", second)
	}
	in, ok := second[0].Inputs["orders"]
	if !ok || len(in.Digest) != 64 || in.Items != 1 {
		t.Fatalf("the input reads %+v", second[0].Inputs)
	}
	held, err := core.objects.Has(t.Context(), artifact.Key("finance", in.Digest))
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Error("the input envelope the message names is not in the store, so a runner redeeming the grant would be handed a name for nothing")
	}
}

// diesOnPublishing publishes and then stands for a controller that dies before it records the
// dispatch: the pass's context ends the moment the message has gone.
type diesOnPublishing struct {
	fakeQueue
	cancel context.CancelFunc
}

func (d *diesOnPublishing) Publish(ctx context.Context, dispatch Dispatch) error {
	if err := d.fakeQueue.Publish(ctx, dispatch); err != nil {
		return err
	}
	d.cancel()
	return nil
}

// A pass that published a task and died before recording the dispatch leaves the task to the next
// pass, which plans it again and publishes it under the same row with another grant. The bus
// deduplicates a task on its row, so the message a runner takes may well be the first, and the
// grant it carries still redeems. Whichever grant is redeemed first binds the task, the other
// answers that the work is somebody else's, and the ending is taken from the runner that redeemed.
func TestATaskPublishedAgainKeepsTheGrantAlreadyOut(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)
	ctx, cancel := context.WithCancel(t.Context())
	died := &diesOnPublishing{cancel: cancel}
	dead, err := NewCore(core.controller, core.term, Options{
		Queue: died, Versions: core.versions, Objects: core.objects, Now: core.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dead.Decide(ctx, decidedRun); err == nil {
		t.Fatal("a pass that died after publishing answered as if it had recorded the dispatch")
	}
	first := died.dispatched()
	if len(first) != 1 {
		t.Fatalf("the pass that died published %d tasks", len(first))
	}

	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	second := q.dispatched()
	if len(second) != 1 || second[0].Row != first[0].Row || second[0].Grant == first[0].Grant {
		t.Fatalf("the next pass published %+v after %+v, and the case under test is the same row with another grant", second, first)
	}

	if err := core.redeem(t, first[0], "runner-1"); err != nil {
		t.Fatalf("the grant of the message the bus kept was refused once the task was published again: %s", err)
	}
	if err := core.redeem(t, second[0], "runner-2"); !errors.Is(err, db.ErrTaskHeld) {
		t.Errorf("the second message's grant, redeemed by another runner, answered %v", err)
	}
	if err := core.redeem(t, second[0], "runner-1"); err != nil {
		t.Errorf("the second message's grant, redeemed by the runner holding the task, answered %v", err)
	}

	if err := core.Answer(t.Context(), Answer{
		Result: failed(first[0].Task, 1, core.now()), Row: first[0].Row, Runner: "runner-1",
	}); err != nil {
		t.Fatalf("the ending of the runner that redeemed was refused: %s", err)
	}
	conn := dbtest.Superuser(t, super)
	if got, want := dispatchesOf(t, conn, first[0].Task.ID), []string{"0 failed runner-1"}; !slices.Equal(got, want) {
		t.Errorf("the key holds %q, want %q", got, want)
	}
}
