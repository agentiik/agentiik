package api_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/version"
)

// Cancelling a run: asked of the API, written on the run, and carried out by the controller,
// which is the only thing that reads the request.

// oneRun is an installation holding one run, pushed and started through the API by an
// administrator, with what a second router or a controller needs to share it.
type oneRun struct {
	pool    *db.Pool
	super   string
	store   *version.Store
	objects artifact.Objects
	run     string
}

func withOneRun(t *testing.T) oneRun {
	t.Helper()
	pool, super := dbtest.Open(t)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `insert into namespaces (name) values ('finance'), ('team-ops')`); err != nil {
		t.Fatal(err)
	}
	store, err := version.New(pool, version.Options{})
	if err != nil {
		t.Fatal(err)
	}
	o := oneRun{pool: pool, super: super, store: store, objects: artifact.Dir(t.TempDir())}

	h := o.servedTo(t, everything{who: "admin"})
	if w, _ := call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/"+aCommit, "admin", aPush(t)); w.Code != http.StatusOK {
		t.Fatalf("the push answered %d: %s", w.Code, w.Body)
	}
	w, started := call(t, h, "POST", "/api/v1/finance/workflows/monthly-invoicing/runs", "admin",
		api.Start{Commit: aCommit, Inputs: map[string]any{"orders": []any{}}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("starting a run answered %d: %s", w.Code, w.Body)
	}
	o.run, _ = started["run"].(string)
	return o
}

// servedTo is the API over the same database, deciding with auth.
func (o oneRun) servedTo(t *testing.T, auth api.Authorizer) http.Handler {
	t.Helper()
	rt, err := api.NewRouter(auth, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewServer(rt, api.ServerOptions{Pool: o.pool, Versions: o.store, Objects: o.objects}); err != nil {
		t.Fatal(err)
	}
	return rt
}

func (o oneRun) cancel() string { return "/api/v1/runs/" + o.run + "/cancel" }

// requested is when the run was asked to cancel, read past the API, and nil while nobody has.
func (o oneRun) requested(t *testing.T) *time.Time {
	t.Helper()
	var at *time.Time
	if err := dbtest.Superuser(t, o.super).QueryRow(t.Context(),
		`select cancel_requested_at from runs where id = $1`, o.run).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// "workflow:run: Start a manual run, cancel it, replay it, approve or reject a waiting run." Held
// on the run's workflow it cancels; held on another workflow, on a workflow of the same name in
// another namespace, or anything else held on this one, is the 404 an absent run gets, and writes
// nothing.
func TestCancellingARunNeedsWorkflowRunOnItsWorkflow(t *testing.T) {
	o := withOneRun(t)
	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}

	for _, c := range []struct {
		who  string
		auth api.Authorizer
		want int
	}{
		{"", holder{who: "bob", what: api.WorkflowRun, over: invoicing}, http.StatusUnauthorized},
		{"alice", api.DenyAll{}, http.StatusNotFound},
		{"carol", holder{who: "carol", what: api.WorkflowRun, over: api.Target{Namespace: "finance", Workflow: "payroll"}}, http.StatusNotFound},
		{"frank", holder{who: "frank", what: api.WorkflowRun, over: api.Target{Namespace: "team-ops", Workflow: "monthly-invoicing"}}, http.StatusNotFound},
		{"dave", holder{who: "dave", what: api.RunRead, over: invoicing}, http.StatusNotFound},
		{"erin", holder{who: "erin", what: api.WorkflowWrite, over: invoicing}, http.StatusNotFound},
	} {
		if w, _ := call(t, o.servedTo(t, c.auth), "POST", o.cancel(), c.who, nil); w.Code != c.want {
			t.Errorf("%q asking to cancel answered %d, want %d: %s", c.who, w.Code, c.want, w.Body)
		}
	}
	if at := o.requested(t); at != nil {
		t.Fatalf("the run was asked to cancel at %s by somebody who may not", at)
	}

	h := o.servedTo(t, holder{who: "bob", what: api.WorkflowRun, over: invoicing})
	if refused, _ := call(t, h, "POST", "/api/v1/runs/01M2ZZZZZZZZZZZZZZZZZZZZZZ/cancel", "bob", nil); refused.Code != http.StatusNotFound {
		t.Errorf("a run nobody started answered %d", refused.Code)
	}

	w, answer := call(t, h, "POST", o.cancel(), "bob", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("bob, who holds workflow:run on the run's workflow, asking to cancel answered %d: %s", w.Code, w.Body)
	}
	if len(answer) != 1 || answer["run"] != o.run {
		t.Errorf("the answer reads %v", answer)
	}
	if got := w.Header().Get("Location"); got != "/api/v1/finance/runs/"+o.run {
		t.Errorf("the run is at %q", got)
	}
	if o.requested(t) == nil {
		t.Error("an accepted request left nothing on the run for the controller to read")
	}
}

// An identifier no run was minted with is no run, answered as one without asking PostgreSQL, which
// refuses U+0000 and bytes that are not UTF-8 with an error: a 500 for anybody holding a credential,
// even one that holds nothing.
func TestCancellingARunNobodyCouldHaveMintedIsNoRun(t *testing.T) {
	o := withOneRun(t)
	for _, auth := range []api.Authorizer{api.DenyAll{}, everything{who: "alice"}} {
		h := o.servedTo(t, auth)
		for _, run := range []string{"%00", "%ff", "01M2%00", "not-a-run", "01m2z8v1p9c4xq7k2n4d6f8h0c"} {
			if w, _ := call(t, h, "POST", "/api/v1/runs/"+run+"/cancel", "alice", nil); w.Code != http.StatusNotFound {
				t.Errorf("asking to cancel %s under %T answered %d: %s", run, auth, w.Code, w.Body)
			}
		}
	}
}

// Asking twice is asking once, and each time the controller is told. A body saying why is refused
// rather than kept, since nothing keeps it.
func TestCancellingTwiceIsAskingOnce(t *testing.T) {
	o := withOneRun(t)
	h := o.servedTo(t, everything{who: "alice"})

	listening := dbtest.Superuser(t, o.super)
	if _, err := listening.Exec(t.Context(), `listen `+db.RunChannel); err != nil {
		t.Fatal(err)
	}
	told := func() bool {
		t.Helper()
		waiting, stop := context.WithTimeout(t.Context(), 5*time.Second)
		defer stop()
		note, err := listening.WaitForNotification(waiting)
		return err == nil && note.Payload == o.run
	}

	if w := sent(t, h, "POST", o.cancel(), "alice", `{"reason":"the figures are wrong"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a cancellation carrying a reason answered %d: %s", w.Code, w.Body)
	}
	if at := o.requested(t); at != nil {
		t.Fatalf("a refused request asked the run to cancel at %s", at)
	}

	if w := sent(t, h, "POST", o.cancel(), "alice", `{}`); w.Code != http.StatusAccepted {
		t.Fatalf("asking to cancel answered %d: %s", w.Code, w.Body)
	}
	if !told() {
		t.Error("the controller was not told the first time")
	}
	first := o.requested(t)
	if first == nil {
		t.Fatal("the request is not on the run")
	}

	if w, _ := call(t, h, "POST", o.cancel(), "alice", nil); w.Code != http.StatusAccepted {
		t.Errorf("asking a second time answered %d: %s", w.Code, w.Body)
	}
	if !told() {
		t.Error("the controller was not told the second time")
	}
	if again := o.requested(t); again == nil || !again.Equal(*first) {
		t.Errorf("asking again moved the request from %s to %v", first, again)
	}
}

// The answer says nothing of how the run stands, in its body or its status: the route is guarded
// by workflow:run, and a run's state is what run:read guards. operator holds the first and not the
// second, and reads the same answer about a run going and one that failed, while GET refuses it
// both. A run that has ended is asked nothing.
func TestCancellingSaysNothingOfHowTheRunStands(t *testing.T) {
	o := withOneRun(t)
	h := o.servedTo(t, holder{who: "olivia", what: api.WorkflowRun, over: api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}})
	if w, _ := call(t, h, "GET", "/api/v1/finance/runs/"+o.run, "olivia", nil); w.Code != http.StatusNotFound {
		t.Fatalf("reading the run without run:read answered %d, so the case under test is not an operator's", w.Code)
	}

	going, _ := call(t, h, "POST", o.cancel(), "olivia", nil)
	if _, err := dbtest.Superuser(t, o.super).Exec(t.Context(),
		`update runs set state = 'failed', started_at = now(), finished_at = now(), cancel_requested_at = null where id = $1`, o.run); err != nil {
		t.Fatal(err)
	}
	ended, _ := call(t, h, "POST", o.cancel(), "olivia", nil)
	if going.Code != http.StatusAccepted || ended.Code != going.Code || ended.Body.String() != going.Body.String() {
		t.Errorf("a run going answered %d %s, and once it had failed %d %s", going.Code, going.Body, ended.Code, ended.Body)
	}
	if at := o.requested(t); at != nil {
		t.Errorf("a run that had ended was asked to cancel at %s", at)
	}
}

// heard is the bus as the controller sees it, keeping what it was handed rather than carrying it.
type heard struct {
	mu      sync.Mutex
	sent    []controller.Dispatch
	stopped []graph.Stop
}

func (q *heard) Publish(_ context.Context, d controller.Dispatch) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sent = append(q.sent, d)
	return nil
}

func (q *heard) Stop(_ context.Context, s graph.Stop) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.stopped = append(q.stopped, s)
	return nil
}

// The whole way, with nothing between the API and the controller but the database: the API
// writes the request and notifies, the controller, woken as the notification wakes it, ends the
// run and stops the task it had handed out, and the run then reads cancelled through the API.
func TestACancelledRunStopsWhatItHolds(t *testing.T) {
	o := withOneRun(t)
	h := o.servedTo(t, everything{who: "alice"})

	c, err := controller.New(o.pool, "cancelling")
	if err != nil {
		t.Fatal(err)
	}
	term, err := o.pool.BeginTerm(t.Context(), "cancelling")
	if err != nil {
		t.Fatal(err)
	}
	q := &heard{}
	core, err := controller.NewCore(c, term, controller.Options{Queue: q, Versions: o.store, Objects: o.objects})
	if err != nil {
		t.Fatal(err)
	}
	run := agk.RunID(o.run)
	if err := core.Decide(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if len(q.sent) != 1 {
		t.Fatalf("the controller handed out %d tasks", len(q.sent))
	}
	handed := q.sent[0].Task.ID

	if w, _ := call(t, h, "POST", o.cancel(), "alice", nil); w.Code != http.StatusAccepted {
		t.Fatalf("asking to cancel a running run answered %d: %s", w.Code, w.Body)
	}
	if err := core.Wake(t.Context(), controller.Wake{Run: run}); err != nil {
		t.Fatal(err)
	}
	if len(q.stopped) != 1 || q.stopped[0].Task != handed || q.stopped[0].Reason != graph.StopCancelled {
		t.Fatalf("cancelling stopped %+v, and the run held %s", q.stopped, handed)
	}

	w, detail := call(t, h, "GET", "/api/v1/finance/runs/"+o.run, "alice", nil)
	if w.Code != http.StatusOK || detail["state"] != "cancelled" {
		t.Fatalf("the run reads %d: %s", w.Code, w.Body)
	}
	tasks, _ := detail["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("the run holds %d tasks", len(tasks))
	}
	if state := tasks[0].(map[string]any)["state"]; state != "cancelled" {
		t.Errorf("the task the run held reads %v", state)
	}

	// And asked again, nothing else is stopped.
	if w, _ := call(t, h, "POST", o.cancel(), "alice", nil); w.Code != http.StatusAccepted {
		t.Errorf("asking to cancel a cancelled run answered %d: %s", w.Code, w.Body)
	}
	if err := core.Wake(t.Context(), controller.Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	if len(q.stopped) != 1 {
		t.Errorf("a cancelled run was stopped again: %+v", q.stopped)
	}
}
