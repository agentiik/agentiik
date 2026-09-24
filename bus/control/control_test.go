package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/bustest"
	"github.com/agentiik/agentiik/internal/ulid"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// The controller's half of the bus against a real NATS, for the reason package bus gives: what is
// under test is what JetStream does with what this package hands it, and a fake would agree with
// whatever this code believes.
//
// A server of the test's own rather than the one AGENTIIK_TEST_BUS_URL names. Package bus's tests
// empty that server's streams and remake its consumers before each test, and go test runs packages
// side by side, so a second package on the same server would take messages away from the first in
// the middle of its tests, and have its own taken away in turn.

// served is a bus on a server of the test's own, opened as the control plane opens it, with the
// consumers the control plane makes, and the address a runner reaches it at.
func served(t *testing.T) (*bus.Bus, string) {
	t.Helper()
	server, err := natsserver.NewServer(&natsserver.Options{
		Port: -1, JetStream: true, StoreDir: bustest.StoreDir(t), NoLog: true, NoSigs: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go server.Start()
	if !server.ReadyForConnections(10 * time.Second) {
		t.Fatal("the server did not come up")
	}
	t.Cleanup(server.Shutdown)
	b, err := bus.Open(t.Context(), bus.Options{URL: server.ClientURL(), Name: "controller"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	for _, pool := range []string{bus.DefaultPool, "dmz"} {
		if err := b.Consumer(t.Context(), pool); err != nil {
			t.Fatal(err)
		}
	}
	return b, server.ClientURL()
}

// step names a step after the test asking for it, so that every test publishes a task identifier
// of its own.
func step(t *testing.T) agk.Step {
	t.Helper()
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, t.Name())
	return agk.Step(name)
}

// aRun is minted once per test process, as package bus mints its own.
var aRun = agk.NewRunID()

func aTask(step agk.Step, runsOn ...string) graph.Task {
	return graph.Task{
		ID:        agk.NewTaskID(aRun, step, 1, agk.Shard{}),
		Run:       aRun,
		Namespace: "finance",
		Workflow:  "monthly-invoicing",
		Commit:    "a3f9c1e",
		Step:      step,
		Attempt:   1,
		Image:     "ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc",
		Outputs:   []agk.Port{"ok"},
		Resources: graph.Resources{CPU: "1", Memory: "512Mi", PIDs: 256},
		RunsOn:    runsOn,
		Deadline:  time.Date(2026, 9, 10, 6, 12, 0, 0, time.UTC),
	}
}

// dispatch is a task with the three things only the controller can add.
func dispatch(step agk.Step, runsOn ...string) controller.Dispatch {
	return dispatchAs(rowOf(step), step, runsOn...)
}

// dispatchAs is the same task under a row of the caller's choosing, which is what a requeue
// after loss is.
func dispatchAs(row string, step agk.Step, runsOn ...string) controller.Dispatch {
	return controller.Dispatch{
		Task:   aTask(step, runsOn...),
		Row:    row,
		Grant:  "agkgrant_" + row + "_dGFza2dyYW50ZXhhbXBsZTAxMjM0NTY3ODlhYmNkZWZnaGk",
		Inputs: map[agk.Port]controller.InputRef{},
	}
}

// rows are the task_id each step's dispatch goes out as, minted once per step, so that a dispatch
// published twice inside a test is one dispatch published twice.
var rows sync.Map

func rowOf(step agk.Step) string {
	row, _ := rows.LoadOrStore(step, ulid.New())
	return row.(string)
}

// aResult is what the runner holding task sends back once it has succeeded, with one item on ok.
func aResult(task graph.Task) bus.TaskResult {
	exit := 0
	started := time.Date(2026, 9, 10, 6, 41, 9, 104_000_000, time.UTC)
	log, _ := agk.NewLogURI(task.ID)
	return bus.TaskResult{
		TaskID:         ulid.New(),
		IdempotencyKey: string(task.ID),
		Runner:         "runner-dmz-02",
		State:          agk.TaskSucceeded,
		ExitCode:       &exit,
		StartedAt:      started,
		FinishedAt:     started.Add(83 * time.Second),
		Outputs: []bus.Output{{
			Port: "ok", Digest: "sha256:7c2e1f4a9b8c0d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c9f11", Items: 1,
		}},
		Log:   &bus.Log{URI: log.String(), Lines: 412},
		Usage: &bus.Usage{CPUSeconds: new(12.4), MaxRSSBytes: new(int64(198443008))},
	}
}

// answering runs Answers until the test ends, handing each answer to fn and on to the channel it
// answers, and taking every progress message as recorded.
func answering(t *testing.T, q *Queue, fn func(controller.Answer) error) <-chan controller.Answer {
	t.Helper()
	answers, _ := hearing(t, q, fn, func(controller.Progress) error { return nil })
	return answers
}

// hearing runs Answers until the test ends, handing each answer to fn and each progress message to
// progress, and each on to the channel of its kind.
func hearing(t *testing.T, q *Queue, fn func(controller.Answer) error, progress func(controller.Progress) error) (<-chan controller.Answer, <-chan controller.Progress) {
	t.Helper()
	ctx, stop := context.WithCancel(t.Context())
	got := make(chan controller.Answer, 16)
	moved := make(chan controller.Progress, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := q.Answers(ctx, func(_ context.Context, a controller.Answer) error {
			got <- a
			return fn(a)
		}, func(_ context.Context, p controller.Progress) error {
			moved <- p
			return progress(p)
		}); err != nil && ctx.Err() == nil {
			t.Errorf("taking results: %s", err)
		}
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})
	return got, moved
}

// publishing is a connection that publishes a result on whatever subject it is told, which is what
// a runner's credential would refuse and what a test of the refusal after it needs.
func publishing(t *testing.T, url string) jetstream.JetStream {
	t.Helper()
	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	return js
}

var errTest = errors.New("the controller could not record it")

// The round trip, which is the whole of controller.Queue: the controller publishes a dispatch, and a
// runner of the pool its labels select takes the message messageOf writes, grant and all.
func TestADispatchGoesToThePoolItsLabelsSelect(t *testing.T) {
	b, _ := served(t)
	q := New(b)

	d := dispatch(step(t), "pool=dmz", "arch=amd64")
	if err := q.Publish(t.Context(), d); err != nil {
		t.Fatal(err)
	}

	other, err := b.Take(t.Context(), bus.DefaultPool, 8, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("the default pool was offered %d tasks meant for dmz", len(other))
	}
	taken, err := b.Take(t.Context(), "dmz", 8, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 1 {
		t.Fatalf("the dmz pool took %d tasks", len(taken))
	}
	want, err := messageOf(d)
	if err != nil {
		t.Fatal(err)
	}
	sent, _ := json.Marshal(want)
	got, _ := json.Marshal(taken[0].Task)
	if string(sent) != string(got) {
		t.Errorf("the dispatch went out as\n%s\nand was taken as\n%s", sent, got)
	}
	if taken[0].Task.TaskID != d.Row || taken[0].Task.Grant != d.Grant {
		t.Errorf("the task was taken as %s with grant %q, and it went out as %s with %q", taken[0].Task.TaskID, taken[0].Task.Grant, d.Row, d.Grant)
	}
	if err := taken[0].Held(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// A dispatch missing what only the controller can supply is refused before anything is published,
// so no runner is handed a task it could not fetch the inputs for.
func TestADispatchWithoutItsGrantIsNotPublished(t *testing.T) {
	b, _ := served(t)
	q := New(b)

	d := dispatch(step(t))
	d.Grant = ""
	if err := q.Publish(t.Context(), d); err == nil {
		t.Fatal("a dispatch with no grant was published")
	}
	taken, err := b.Take(t.Context(), bus.DefaultPool, 8, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 0 {
		t.Errorf("a dispatch refused was handed out as %+v", taken[0].Task)
	}
}

// "A requeue after loss keeps the idempotency key and takes a new task_id." The task_id is the
// dispatch's row, so the requeue is a second message and not a duplicate of the dispatch it
// replaces.
func TestARequeueIsQueuedUnderTheKeyItWasLostUnder(t *testing.T) {
	b, _ := served(t)
	q := New(b)
	lost := dispatch(step(t))
	requeued := dispatchAs(ulid.New(), step(t))
	for _, d := range []controller.Dispatch{lost, requeued} {
		if err := q.Publish(t.Context(), d); err != nil {
			t.Fatal(err)
		}
	}
	taken, err := b.Take(t.Context(), bus.DefaultPool, 8, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 2 {
		t.Fatalf("a task and its requeue were offered %d times, and a requeue is a message of its own", len(taken))
	}
	for i, want := range []controller.Dispatch{lost, requeued} {
		got := taken[i].Task
		if got.TaskID != want.Row || got.IdempotencyKey != string(want.Task.ID) {
			t.Errorf("message %d is task_id %s under key %s, want %s under %s", i+1, got.TaskID, got.IdempotencyKey, want.Row, want.Task.ID)
		}
		taken[i].Held(t.Context())
	}
}

// A result goes back and the controller takes it once, as the answer it records: the dispatch by
// its row, the runner, the envelopes by digest alone, the log and the usage under the wire's names.
func TestAResultComesBackAsTheAnswerTheControllerTakes(t *testing.T) {
	b, _ := served(t)
	task := aTask(step(t))
	result := aResult(task)
	if err := b.Report(t.Context(), result); err != nil {
		t.Fatal(err)
	}

	got := answering(t, New(b), func(controller.Answer) error { return nil })
	select {
	case a := <-got:
		if a.Result.Task != task.ID || a.Row != result.TaskID || a.Runner != "runner-dmz-02" || a.LogLines != 412 {
			t.Errorf("the result came back as %+v", a)
		}
		if len(a.Outputs) != 1 || a.Outputs[0] != (controller.Output{Port: "ok", Digest: "7c2e1f4a9b8c0d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c9f11", Items: 1}) {
			t.Errorf("the outputs came back as %+v", a.Outputs)
		}
		if a.Usage["cpu_seconds"] != 12.4 {
			t.Errorf("the usage came back as %v", a.Usage)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no result reached the controller")
	}

	select {
	case a := <-got:
		t.Errorf("the result was delivered twice: %+v", a)
	case <-time.After(500 * time.Millisecond):
	}
}

// A result the controller could not record is left for the next delivery, which is what
// at-least-once buys. Only controller.ErrNotAResult takes one off the queue.
func TestAResultTheControllerCouldNotRecordComesBack(t *testing.T) {
	b, _ := served(t)
	trouble := make(chan error, 8)
	b.Trouble = func(_ string, err error) { trouble <- err }
	if err := b.Report(t.Context(), aResult(aTask(step(t)))); err != nil {
		t.Fatal(err)
	}

	tries := 0
	got := answering(t, New(b), func(controller.Answer) error {
		tries++
		if tries == 1 {
			return errTest
		}
		return nil
	})
	for want := 1; want <= 2; want++ {
		select {
		case <-got:
		case err := <-trouble:
			t.Fatalf("a result the controller could not record yet was taken off the queue as %q", err)
		case <-time.After(15 * time.Second):
			t.Fatalf("delivery %d never arrived, so a result the controller refused was lost", want)
		}
	}
}

// A result no controller could ever record is taken off the queue and said out loud, as a message
// nobody can read is. Left for the next delivery it would come round for ever, since the bus
// delivers without limit and nothing about the result changes in between. The controller says
// which kind it is, and what is said keeps it, so that a runner speaking for a task it does not
// hold is told apart from one that misread the wire.
func TestAResultTheControllerWillNeverRecordIsTakenOffAndReported(t *testing.T) {
	b, _ := served(t)
	trouble := make(chan error, 8)
	b.Trouble = func(_ string, err error) { trouble <- err }

	task := aTask(step(t))
	if err := b.Report(t.Context(), aResult(task)); err != nil {
		t.Fatal(err)
	}
	seen := answering(t, New(b), func(a controller.Answer) error {
		// Wrapped, as Core.Answer wraps it.
		return fmt.Errorf("%w: %w: %s is bound to runner-lan-01", controller.ErrNotAResult, controller.ErrNotTheHolder, a.Result.Task)
	})

	select {
	case a := <-seen:
		if a.Result.Task != task.ID {
			t.Fatalf("the controller was handed %s", a.Result.Task)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the result never reached the controller")
	}
	select {
	case err := <-trouble:
		if !errors.Is(err, controller.ErrNotAResult) || !errors.Is(err, controller.ErrNotTheHolder) {
			t.Errorf("what was said reads %q", err)
		}
	case a := <-seen:
		t.Fatalf("it was delivered again rather than taken off the queue: %+v", a)
	case <-time.After(15 * time.Second):
		t.Fatal("nothing was said about a result taken off the queue")
	}

	// And it is off the queue rather than coming round for ever.
	select {
	case a := <-seen:
		t.Errorf("it came round again: %+v", a)
	case err := <-trouble:
		t.Errorf("it was said twice, the second time as %q", err)
	case <-time.After(2 * time.Second):
	}
}

// A result is the word of the runner whose subject it arrives on, and one naming another runner is
// a machine of the pool speaking for somebody else's task. It is taken off the queue and said out
// loud as a result from a runner that does not hold the task, and the controller never sees it.
func TestAResultNamingAnotherRunnerIsTakenOffAndReported(t *testing.T) {
	b, url := served(t)
	trouble := make(chan error, 8)
	b.Trouble = func(_ string, err error) { trouble <- err }

	body, err := json.Marshal(aResult(aTask(step(t))))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publishing(t, url).Publish(t.Context(), bus.ResultSubject("runner-lan-01"), body); err != nil {
		t.Fatal(err)
	}

	seen := answering(t, New(b), func(controller.Answer) error { return nil })
	select {
	case err := <-trouble:
		if !errors.Is(err, controller.ErrNotAResult) || !errors.Is(err, controller.ErrNotTheHolder) {
			t.Errorf("what was said reads %q", err)
		}
	case a := <-seen:
		t.Fatalf("the controller was handed a result runner-lan-01 published as runner-dmz-02: %+v", a)
	case <-time.After(15 * time.Second):
		t.Fatal("nothing was said about a result published under another runner's name")
	}
	select {
	case a := <-seen:
		t.Errorf("the controller was handed it after all: %+v", a)
	case err := <-trouble:
		t.Errorf("it was said twice, the second time as %q", err)
	case <-time.After(2 * time.Second):
	}
}

// A runner's progress comes back as the controller takes it: the key, the dispatch its task_id
// names, the state, and the runner whose subject it came on. It is not taken for an answer.
func TestProgressComesBackAsTheControllerTakesIt(t *testing.T) {
	b, _ := served(t)
	task := aTask(step(t))
	sent := bus.TaskProgress{TaskID: ulid.New(), IdempotencyKey: string(task.ID), Runner: "runner-dmz-02", Progress: agk.TaskPublishing}
	if err := b.Progress(t.Context(), sent); err != nil {
		t.Fatal(err)
	}

	answers, moved := hearing(t, New(b), func(controller.Answer) error { return nil }, func(controller.Progress) error { return nil })
	select {
	case p := <-moved:
		if want := (controller.Progress{Task: task.ID, Row: sent.TaskID, State: agk.TaskPublishing, Runner: "runner-dmz-02"}); p != want {
			t.Errorf("the progress came back as %+v, want %+v", p, want)
		}
	case a := <-answers:
		t.Fatalf("progress came back as an answer: %+v", a)
	case <-time.After(10 * time.Second):
		t.Fatal("no progress reached the controller")
	}
}

// Progress is held to the rules a result is held to: one naming a runner other than the one whose
// subject it came on is never shown to the controller, and one the controller refuses with
// controller.ErrNotAResult is not delivered again. Both are taken off the queue and said out loud.
func TestProgressNoDeliveryWouldChangeIsTakenOffAndReported(t *testing.T) {
	b, url := served(t)
	trouble := make(chan error, 8)
	b.Trouble = func(_ string, err error) { trouble <- err }

	task := aTask(step(t))
	impostor, err := json.Marshal(bus.TaskProgress{TaskID: ulid.New(), IdempotencyKey: string(task.ID), Runner: "runner-dmz-02", Progress: agk.TaskRunning})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publishing(t, url).Publish(t.Context(), bus.ResultSubject("runner-lan-01"), impostor); err != nil {
		t.Fatal(err)
	}
	refused := bus.TaskProgress{TaskID: ulid.New(), IdempotencyKey: string(task.ID), Runner: "runner-dmz-02", Progress: agk.TaskRunning}
	if err := b.Progress(t.Context(), refused); err != nil {
		t.Fatal(err)
	}

	_, moved := hearing(t, New(b), func(controller.Answer) error { return nil }, func(p controller.Progress) error {
		return fmt.Errorf("%w: %w: %s is bound to runner-lan-01", controller.ErrNotAResult, controller.ErrNotTheHolder, p.Task)
	})
	heard := 0
	for said := 0; said < 2; {
		select {
		case err := <-trouble:
			if !errors.Is(err, controller.ErrNotAResult) || !errors.Is(err, controller.ErrNotTheHolder) {
				t.Errorf("what was said reads %q", err)
			}
			said++
		case p := <-moved:
			if p.Row != refused.TaskID {
				t.Fatalf("the controller was handed progress runner-lan-01 published as runner-dmz-02: %+v", p)
			}
			if heard++; heard > 1 {
				t.Fatalf("progress the controller refused was delivered again: %+v", p)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("%d refusals were said, of two", said)
		}
	}
	select {
	case p := <-moved:
		t.Errorf("progress came round again: %+v", p)
	case err := <-trouble:
		t.Errorf("something more was said: %q", err)
	case <-time.After(2 * time.Second):
	}
}

// A stop the controller asks for goes where package bus puts one, to whoever is holding the task,
// and not onto the queue.
func TestAStopGoesToWhoeverIsHolding(t *testing.T) {
	b, url := served(t)
	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sub, err := conn.SubscribeSync(bus.StopSubject)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}

	task := aTask(step(t))
	if err := New(b).Stop(t.Context(), graph.Stop{Task: task.ID, Reason: graph.StopCancelled}); err != nil {
		t.Fatal(err)
	}
	msg, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("no stop arrived: %s", err)
	}
	var got struct {
		Task   agk.TaskID `json:"task"`
		Reason string     `json:"reason"`
	}
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Task != task.ID || got.Reason != "cancelled" {
		t.Errorf("the stop reads %+v", got)
	}
}
