package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/bustest"
	"github.com/agentiik/agentiik/internal/ulid"
	natsserver "github.com/nats-io/nats-server/v2/server"
)

// The bus against a real NATS, for the reason every other real test in this module exists: what
// is under test is what JetStream does, and a fake would agree with whatever this code believes
// about retention, redelivery and durable consumers.
//
//	docker run -d --name agk-nats -p 44222:4222 nats:alpine -js
//	AGENTIIK_TEST_BUS_URL=nats://127.0.0.1:44222 go test ./bus/

func open(t *testing.T) *Bus {
	t.Helper()
	url := os.Getenv("AGENTIIK_TEST_BUS_URL")
	if url == "" {
		t.Skip("no NATS on this machine: set AGENTIIK_TEST_BUS_URL")
	}
	b, err := Open(t.Context(), Options{URL: url})
	if err != nil {
		t.Skipf("the bus at AGENTIIK_TEST_BUS_URL could not be reached: %s", err)
	}
	t.Cleanup(b.Close)

	// Each test starts from an empty queue, since what is under test is what happens to the
	// messages this test published and not what a previous one left behind.
	if err := b.stream.Purge(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := b.results.Purge(t.Context()); err != nil {
		t.Fatal(err)
	}

	// And from the consumers the control plane makes and no others. A NATS kept running
	// between suites still holds whatever consumers the code of an earlier day created, and a
	// WorkQueue stream refuses a second consumer on a subject one already filters on, so one
	// left behind under another name is a pool nobody can take from.
	names := b.stream.ConsumerNames(t.Context())
	for name := range names.Name() {
		if err := b.stream.DeleteConsumer(t.Context(), name); err != nil {
			t.Fatal(err)
		}
	}
	if err := names.Err(); err != nil {
		t.Fatal(err)
	}
	for _, pool := range []string{DefaultPool, "dmz"} {
		if err := b.Consumer(t.Context(), pool); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// step names a step after the test asking for it, so that every test publishes a task identifier
// of its own. The duplicate window is two minutes and a test run is faster than that, so two
// tests sharing an identifier would be one publish and one silence.
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

// aRun is minted once per test process rather than written down, because the stream's duplicate
// window is two minutes and a suite run is faster than that: a fixed identifier would make the
// second run of the day publish nothing and read as a bus that had stopped working.
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

// message is a task as the wire carries it, with the three things only the controller can add:
// the row it goes out under, its grant and its inputs by digest, of which it has none.
func message(step agk.Step, runsOn ...string) TaskMessage {
	return messageAs(rowOf(step), step, runsOn...)
}

// messageAs is the same task under a row of the caller's choosing, which is what a requeue after
// loss is. It is written out as package bus/control writes one, which its own tests hold to the
// wire's schema.
func messageAs(row string, step agk.Step, runsOn ...string) TaskMessage {
	task := aTask(step, runsOn...)
	if runsOn == nil {
		runsOn = []string{}
	}
	return TaskMessage{
		TaskID:         row,
		IdempotencyKey: string(task.ID),
		RunID:          string(task.Run),
		Namespace:      task.Namespace,
		Workflow:       task.Workflow + "@" + task.Commit,
		Step:           string(task.Step),
		Attempt:        task.Attempt,
		Image:          task.Image,
		Params:         map[string]any{},
		Secrets:        []SecretMount{},
		Inputs:         []Input{},
		Outputs:        []string{"ok"},
		Resources:      Resources{CPU: task.Resources.CPU, Memory: task.Resources.Memory, PIDs: task.Resources.PIDs},
		Network:        task.Network.String(),
		RunsOn:         runsOn,
		Deadline:       task.Deadline.Format(time.RFC3339Nano),
		Grant:          "agkgrant_" + row + "_dGFza2dyYW50ZXhhbXBsZTAxMjM0NTY3ODlhYmNkZWZnaGk",
	}
}

// rows are the task_id each step's dispatch goes out as, minted once per step and per test
// process for the reason aRun is: the row is what the stream deduplicates on, so one written down
// would make every publish after the first in a two-minute window publish nothing, and one minted
// on every call would make a publish repeated inside a test two messages.
var rows sync.Map

func rowOf(step agk.Step) string {
	row, _ := rows.LoadOrStore(step, ulid.New())
	return row.(string)
}

// The round trip, which is the whole contract: the control plane publishes to a pool and a runner
// of that pool takes it, whole.
func TestATaskIsTakenWholeByTheRunnersOfItsPool(t *testing.T) {
	b := open(t)

	// A param written 1.0 comes back 1.0, as agk run --local hands it to the container, and not
	// the 1 a float64 would print.
	sent := message(step(t), "zone=dmz", "arch=amd64")
	sent.Params = map[string]any{"ratio": json.Number("1.0"), "count": json.Number("3")}
	if err := b.Publish(t.Context(), "dmz", sent); err != nil {
		t.Fatal(err)
	}

	// A runner of another pool is offered nothing, which is what a filtered consumer is for.
	other, err := b.Take(t.Context(), "default", 8, 300*time.Millisecond)
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
	got := taken[0].Task
	if got.IdempotencyKey != string(aTask(step(t)).ID) || got.Namespace != "finance" || got.Image == "" {
		t.Errorf("the task came back as %+v", got)
	}
	if got.Grant == "" {
		t.Error("the task came back with no grant, which is the hinge the whole message turns on")
	}
	if got.Params["ratio"] != json.Number("1.0") || got.Params["count"] != json.Number("3") {
		t.Errorf("the params came back as %#v, want each number as it was written", got.Params)
	}
	if err := taken[0].Held(t.Context()); err != nil {
		t.Fatal(err)
	}

	// "a message is removed as soon as it has been consumed", so nobody takes it twice.
	again, err := b.Take(t.Context(), "dmz", 8, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("a task that had been taken and acknowledged was offered again: %+v", again)
	}
}

// The pool is the one the controller chose and not one read off the labels again: a task whose
// labels a pool of another name carries goes where it was sent.
func TestATaskGoesToThePoolItIsPublishedTo(t *testing.T) {
	b := open(t)
	if err := b.Publish(t.Context(), DefaultPool, message(step(t), "zone=dmz")); err != nil {
		t.Fatal(err)
	}
	other, err := b.Take(t.Context(), "dmz", 8, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("the dmz pool was offered %d tasks sent to the default pool", len(other))
	}
	taken, err := b.Take(t.Context(), DefaultPool, 8, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 1 {
		t.Fatalf("the default pool took %d tasks", len(taken))
	}
	taken[0].Held(t.Context())
}

// A runner that took work it cannot run puts it back, and somebody else gets it.
func TestATaskPutBackIsOfferedAgain(t *testing.T) {
	b := open(t)
	if err := b.Publish(t.Context(), DefaultPool, message(step(t))); err != nil {
		t.Fatal(err)
	}
	first, err := b.Take(t.Context(), DefaultPool, 8, 5*time.Second)
	if err != nil || len(first) != 1 {
		t.Fatalf("taking: %v, %d", err, len(first))
	}
	if err := first[0].Again(); err != nil {
		t.Fatal(err)
	}

	second, err := b.Take(t.Context(), DefaultPool, 8, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].Task.IdempotencyKey != first[0].Task.IdempotencyKey {
		t.Fatalf("a task put back came round as %+v", second)
	}
	second[0].Held(t.Context())
}

// A pool's consumer waits AckWait for a runner to acknowledge what it was handed before handing it
// to another, which is the bound on a take and a redemption and not on a task. AckWait is the
// figure the documentation gives, "The pool consumer's AckWait, one minute", and not whatever the
// constant happens to hold.
func TestAPoolWaitsAckWaitForARunnerToAcknowledge(t *testing.T) {
	if AckWait != time.Minute {
		t.Errorf("AckWait is %s, and the documentation gives the pool consumer's AckWait as one minute", AckWait)
	}
	b := open(t)
	for _, pool := range []string{DefaultPool, "dmz"} {
		consumer, err := b.js.Consumer(t.Context(), Stream, Durable(pool))
		if err != nil {
			t.Fatal(err)
		}
		info, err := consumer.Info(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if info.Config.AckWait != AckWait {
			t.Errorf("the consumer of pool %s waits %s for an acknowledgement, and AckWait is %s", pool, info.Config.AckWait, AckWait)
		}
	}
}

// A task is held once the server says the acknowledgement arrived, and not once it has left
// this side. A link that drops keeps the acknowledgement in the client's buffer, and the server
// hands the message to another runner of the pool when its wait runs out. That runner is refused
// the task at its redemption, but the runner holding it is owed the truth: told it held the task
// on the strength of the buffer, it would not know the message was coming round again.
func TestATaskIsHeldOnlyOnceTheServerHasTheAcknowledgement(t *testing.T) {
	b := open(t)
	if err := b.Publish(t.Context(), DefaultPool, message(step(t))); err != nil {
		t.Fatal(err)
	}

	link := linkTo(t, b.conn.ConnectedAddr())
	runner, err := OpenRunner(Options{URL: "nats://" + link.addr(), Name: "runner-cut-off"})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	taken, err := runner.Take(t.Context(), DefaultPool, 1, 5*time.Second)
	if err != nil || len(taken) != 1 {
		t.Fatalf("taking: %v, %d", err, len(taken))
	}

	link.cut()
	short, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := taken[0].Held(short); err == nil {
		t.Fatal("a task was held whose acknowledgement never reached the server")
	}
}

// link is a connection to the bus that can be cut from outside, as a network does it: both
// directions at once, and nothing listening where the client tries to connect again.
type link struct {
	ln net.Listener

	mu    sync.Mutex
	conns []net.Conn
}

func linkTo(t *testing.T, target string) *link {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &link{ln: ln}
	t.Cleanup(l.cut)
	go func() {
		for {
			near, err := ln.Accept()
			if err != nil {
				return
			}
			far, err := net.Dial("tcp", target)
			if err != nil {
				near.Close()
				continue
			}
			l.mu.Lock()
			l.conns = append(l.conns, near, far)
			l.mu.Unlock()
			go io.Copy(far, near)
			go io.Copy(near, far)
		}
	}()
	return l
}

func (l *link) addr() string { return l.ln.Addr().String() }

func (l *link) cut() {
	l.ln.Close()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.conns {
		c.Close()
	}
}

// "JetStream guarantees at-least-once delivery", so publishing the same task twice inside the
// duplicate window is one message rather than two: the task_id is what makes a retry free. One
// that names none is not published, since there would be nothing to deduplicate it on.
func TestPublishingOneTaskTwiceQueuesItOnce(t *testing.T) {
	b := open(t)
	for range 3 {
		if err := b.Publish(t.Context(), DefaultPool, message(step(t))); err != nil {
			t.Fatal(err)
		}
	}
	taken, err := b.Take(t.Context(), DefaultPool, 8, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 1 {
		t.Fatalf("one task published three times was offered %d times", len(taken))
	}
	taken[0].Held(t.Context())

	nameless := message(step(t) + "-nameless")
	nameless.TaskID = ""
	if err := b.Publish(t.Context(), DefaultPool, nameless); err == nil {
		t.Error("a task naming no task_id was published")
	}
}

// aResult is what the runner holding task sends back once it has succeeded, with one item on ok.
//
// Its task_id is minted per call, because the stream deduplicates a result on its dispatch for two
// minutes and a suite run is faster than that.
func aResult(task graph.Task) TaskResult {
	exit := 0
	started := time.Date(2026, 9, 10, 6, 41, 9, 104_000_000, time.UTC)
	log, _ := agk.NewLogURI(task.ID)
	return TaskResult{
		TaskID:         ulid.New(),
		IdempotencyKey: string(task.ID),
		Runner:         "runner-dmz-02",
		State:          agk.TaskSucceeded,
		ExitCode:       &exit,
		StartedAt:      started,
		FinishedAt:     started.Add(83 * time.Second),
		Outputs: []Output{{
			Port: "ok", Digest: "sha256:7c2e1f4a9b8c0d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c9f11", Items: 1,
		}},
		Log:   &Log{URI: log.String(), Lines: 412},
		Usage: &Usage{CPUSeconds: new(12.4), MaxRSSBytes: new(int64(198443008))},
	}
}

// heard is one result or progress message Reports handed on, and the runner whose subject it came
// on. progress is nil where it was a result.
type heard struct {
	sender   string
	result   TaskResult
	progress *TaskProgress
}

// reporting runs Reports until the test ends, handing each result and progress message to fn and
// on to the channel it answers.
func reporting(t *testing.T, b *Bus, fn func(heard) error) <-chan heard {
	t.Helper()
	ctx, stop := context.WithCancel(t.Context())
	got := make(chan heard, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := b.Reports(ctx, func(_ context.Context, sender string, r TaskResult) error {
			h := heard{sender: sender, result: r}
			got <- h
			return fn(h)
		}, func(_ context.Context, sender string, p TaskProgress) error {
			h := heard{sender: sender, progress: &p}
			got <- h
			return fn(h)
		}); err != nil && ctx.Err() == nil {
			t.Errorf("taking results: %s", err)
		}
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})
	return got
}

// "A requeue after loss keeps the idempotency key and takes a new task_id." Published again
// inside the duplicate window under its new task_id, it is a second message and not a duplicate
// of the dispatch that was lost.
func TestARequeueIsQueuedUnderTheKeyItWasLostUnder(t *testing.T) {
	b := open(t)
	lost := message(step(t))
	requeued := messageAs(ulid.New(), step(t))
	for _, m := range []TaskMessage{lost, requeued} {
		if err := b.Publish(t.Context(), DefaultPool, m); err != nil {
			t.Fatal(err)
		}
	}
	taken, err := b.Take(t.Context(), DefaultPool, 8, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 2 {
		t.Fatalf("a task and its requeue were offered %d times, and a requeue is a message of its own", len(taken))
	}
	for i, want := range []TaskMessage{lost, requeued} {
		got := taken[i].Task
		if got.TaskID != want.TaskID || got.IdempotencyKey != want.IdempotencyKey {
			t.Errorf("message %d is task_id %s under key %s, want %s under %s", i+1, got.TaskID, got.IdempotencyKey, want.TaskID, want.IdempotencyKey)
		}
		taken[i].Held(t.Context())
	}
}

// A result goes back and the controller takes it, once, whole, and from the runner whose subject it
// came on.
func TestAResultComesBackToTheController(t *testing.T) {
	b := open(t)
	result := aResult(aTask(step(t)))
	if err := b.Report(t.Context(), result); err != nil {
		t.Fatal(err)
	}

	got := reporting(t, b, func(heard) error { return nil })
	select {
	case h := <-got:
		if h.sender != "runner-dmz-02" {
			t.Errorf("the result came back from %s, and runner-dmz-02 published it", h.sender)
		}
		sent, _ := json.Marshal(result)
		back, _ := json.Marshal(h.result)
		if string(sent) != string(back) {
			t.Errorf("the result went out as\n%s\nand came back as\n%s", sent, back)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no result reached the controller")
	}

	select {
	case h := <-got:
		t.Errorf("the result was delivered twice: %+v", h.result)
	case <-time.After(500 * time.Millisecond):
	}
}

// "A requeue after loss keeps the idempotency key and takes a new task_id." The requeue's ending
// and a late one of the dispatch it replaced are two results under one key, and both reach the
// controller, which alone can say which of them is news. Each reaches it as the dispatch its
// task_id names, and a result that names no dispatch is not sent at all.
func TestTwoDispatchesOfOneKeyEachReportTheirEnding(t *testing.T) {
	b := open(t)
	task := aTask(step(t))
	sent := map[string]bool{}
	for range 2 {
		result := aResult(task)
		if err := b.Report(t.Context(), result); err != nil {
			t.Fatal(err)
		}
		sent[result.TaskID] = true
	}
	nameless := aResult(task)
	nameless.TaskID = ""
	if err := b.Report(t.Context(), nameless); err == nil {
		t.Error("a result naming no dispatch was published")
	}

	ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	got := make(chan TaskResult, 4)
	go func() {
		b.Reports(ctx, func(_ context.Context, _ string, r TaskResult) error {
			got <- r
			return nil
		}, func(context.Context, string, TaskProgress) error { return nil })
	}()
	rows := map[string]bool{}
	for len(rows) < 2 {
		select {
		case r := <-got:
			if !sent[r.TaskID] {
				t.Errorf("the controller was handed dispatch %s, and the two sent were %v", r.TaskID, sent)
			}
			rows[r.TaskID] = true
		case <-ctx.Done():
			t.Fatalf("the controller was handed %d of the two endings of one key", len(rows))
		}
	}
	select {
	case r := <-got:
		t.Errorf("a third result reached the controller: %+v", r)
	case <-time.After(500 * time.Millisecond):
	}
}

// The requeue of a task lost while its host was only cut off comes back to that host, which had
// run it to its end. The host takes it, and answers it with the ending it recorded: the message is
// acknowledged, so nobody else is handed it to run, and the ending reaches the controller as the
// requeue's, under the task_id the message carried and not the one of the dispatch the host ended.
// An ending of another key is no answer to it and is not sent.
func TestARequeueIsAnsweredWithTheEndingItsHostRecorded(t *testing.T) {
	b := open(t)
	task := aTask(step(t))
	requeue := messageAs(ulid.New(), step(t))
	if err := b.Publish(t.Context(), DefaultPool, requeue); err != nil {
		t.Fatal(err)
	}
	taken, err := b.Take(t.Context(), DefaultPool, 1, 5*time.Second)
	if err != nil || len(taken) != 1 {
		t.Fatalf("taking the requeue: %v, %d", err, len(taken))
	}

	// What the record holds carries no task_id of its own, and the one it was first reported
	// under is the dispatch that was lost.
	recorded := aResult(task)
	recorded.TaskID = rowOf(step(t))

	// Read off the pool's consumer rather than by taking again: a message nobody acknowledged
	// is only offered again once AckWait has passed, a minute, and a take that waited less
	// would find nothing either way.
	pending := func() int {
		t.Helper()
		consumer, err := b.js.Consumer(t.Context(), Stream, Durable(DefaultPool))
		if err != nil {
			t.Fatal(err)
		}
		info, err := consumer.Info(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return info.NumAckPending
	}

	other := aResult(aTask(step(t) + "-other"))
	if err := b.Ended(t.Context(), taken[0], other); err == nil {
		t.Error("the ending of another key was sent as the answer to the requeue")
	}
	if n := pending(); n != 1 {
		t.Errorf("an ending of another key was refused and the pool's consumer has NumAckPending %d, where the requeue still waits on its acknowledgement", n)
	}
	if err := b.Ended(t.Context(), taken[0], recorded); err != nil {
		t.Fatalf("answering the requeue with the recorded ending: %s", err)
	}
	if n := pending(); n != 0 {
		t.Errorf("the requeue was answered from the record and the pool's consumer has NumAckPending %d, and one left unacknowledged is handed on to a runner with no record of its key", n)
	}

	got := reporting(t, b, func(heard) error { return nil })
	select {
	case h := <-got:
		if r := h.result; r.TaskID != requeue.TaskID || r.IdempotencyKey != string(task.ID) || h.sender != recorded.Runner {
			t.Errorf("the controller was handed dispatch %s of %s from %s, want %s of %s from %s", r.TaskID, r.IdempotencyKey, h.sender, requeue.TaskID, task.ID, recorded.Runner)
		}
		if r := h.result; r.State != agk.TaskSucceeded || len(r.Outputs) != 1 || r.Log == nil || r.Log.Lines != 412 {
			t.Errorf("the recorded ending came back as %+v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the recorded ending never reached the controller")
	}
	select {
	case h := <-got:
		t.Errorf("a second result reached the controller: %+v", h.result)
	case <-time.After(500 * time.Millisecond):
	}
}

// A requeue answered from the record is acknowledged only once its ending is out. A report that
// did not go out leaves the message on the queue, so the host that could not say how the key ended
// has taken nothing from anybody: the message comes round once AckWait has passed, and is answered
// again once the report can go. Acknowledged first, it would have left the queue bound to nobody,
// where the heartbeat's sweep does not look, and the run would wait on it until its timeout.
func TestARequeueWhoseRecordedEndingDidNotGoOutStaysOnTheQueue(t *testing.T) {
	b := alone(t)
	const wait = 2 * time.Second
	if err := b.consumer(t.Context(), DefaultPool, wait); err != nil {
		t.Fatal(err)
	}
	requeue := messageAs(ulid.New(), step(t))
	if err := b.Publish(t.Context(), DefaultPool, requeue); err != nil {
		t.Fatal(err)
	}
	taken, err := b.Take(t.Context(), DefaultPool, 1, 5*time.Second)
	if err != nil || len(taken) != 1 {
		t.Fatalf("taking the requeue: %v, %d", err, len(taken))
	}
	recorded := aResult(aTask(step(t)))
	recorded.TaskID = rowOf(step(t))

	// With no stream to take it, a report is answered "no response from stream", as it is
	// where the link to the bus dropped between the take and the report.
	if err := b.js.DeleteStream(t.Context(), Results); err != nil {
		t.Fatal(err)
	}
	if err := b.Ended(t.Context(), taken[0], recorded); err == nil {
		t.Fatal("the recorded ending was said to be reported with no stream to take it")
	}
	if n := b.Outstanding(t, DefaultPool); n != 1 {
		t.Fatalf("a requeue whose ending did not go out leaves %d messages for the pool to hand out, and it is still owed an answer", n)
	}

	again, err := b.Take(t.Context(), DefaultPool, 1, 3*wait)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].Task.TaskID != requeue.TaskID {
		t.Fatalf("once AckWait had passed the pool handed out %+v, want the requeue %s again", again, requeue.TaskID)
	}
	reopened, err := Open(t.Context(), Options{URL: b.conn.ConnectedUrl()})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := b.Ended(t.Context(), again[0], recorded); err != nil {
		t.Fatalf("answering the requeue once the report could go: %s", err)
	}
	if n := b.Outstanding(t, DefaultPool); n != 0 {
		t.Errorf("the requeue was answered and the pool still has %d messages to hand out", n)
	}
	info, err := reopened.results.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 1 {
		t.Errorf("the result stream holds %d results, want the one ending", info.State.Msgs)
	}
}

// alone is a bus on a server of the test's own, with both streams and the consumers the control
// plane makes. It is for a test that takes something away from the bus, which on the server the
// other tests share would be taken from them too.
func alone(t *testing.T) *Bus {
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
	b, err := Open(t.Context(), Options{URL: server.ClientURL()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	for _, pool := range []string{DefaultPool, "dmz"} {
		if err := b.Consumer(t.Context(), pool); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// One dispatch delivered to two machines is redeemed by one of them, and the other may report the
// unreached failure the controller refuses from it, which it drops. The failure of the runner that
// holds it then follows under the same task_id and the same ending, and it still reaches the
// controller: two runners' results are two results, whatever they say. The holder publishing its
// own again, after an answer it never heard, is still one.
func TestTwoRunnersReportingOneDispatchAreBothHeard(t *testing.T) {
	b := open(t)
	task := aTask(step(t))
	row := rowOf(step(t))

	unreached := TaskResult{TaskID: row, IdempotencyKey: string(task.ID), Runner: "runner-lan-01", State: agk.TaskFailed}
	exit := 1
	started := time.Date(2026, 9, 10, 6, 41, 9, 0, time.UTC)
	held := TaskResult{
		TaskID: row, IdempotencyKey: string(task.ID), Runner: "runner-dmz-02", State: agk.TaskFailed,
		ExitCode: &exit, StartedAt: started, FinishedAt: started.Add(time.Second),
	}
	for _, r := range []TaskResult{unreached, held, held} {
		if err := b.Report(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}

	got := reporting(t, b, func(h heard) error {
		if h.sender != held.Runner {
			return Drop(fmt.Errorf("%s does not hold %s", h.sender, h.result.TaskID))
		}
		return nil
	})
	from := map[string]int{}
	for len(from) < 2 {
		select {
		case h := <-got:
			from[h.sender]++
		case <-time.After(10 * time.Second):
			t.Fatalf("the controller was handed results from %v, and the holder's failure never arrived", from)
		}
	}
	select {
	case h := <-got:
		from[h.sender]++
	case <-time.After(500 * time.Millisecond):
	}
	if from[unreached.Runner] != 1 || from[held.Runner] != 1 {
		t.Errorf("the controller was handed %v, want one result from each runner", from)
	}
}

// A result the controller could not record is left for the next delivery, which is what
// at-least-once buys.
func TestAResultTheControllerRefusesComesBack(t *testing.T) {
	b := open(t)
	if err := b.Report(t.Context(), aResult(aTask(step(t)))); err != nil {
		t.Fatal(err)
	}

	tries := 0
	got := reporting(t, b, func(heard) error {
		tries++
		if tries == 1 {
			return errTest
		}
		return nil
	})
	for want := 1; want <= 2; want++ {
		select {
		case <-got:
		case <-time.After(15 * time.Second):
			t.Fatalf("delivery %d never arrived, so a result the controller refused was lost", want)
		}
	}
}

// And not at once. The consumer delivers without limit, so a result the controller keeps failing to
// record, because the database is down or an envelope is not in the store, would otherwise come
// round as fast as the controller can refuse it, holding the controller and the store for as long
// as the cause lasts.
func TestAResultTheControllerCouldNotRecordWaitsBeforeComingBack(t *testing.T) {
	b := open(t)
	if err := b.Report(t.Context(), aResult(aTask(step(t)))); err != nil {
		t.Fatal(err)
	}

	got := reporting(t, b, func(heard) error { return errTest })
	select {
	case <-got:
	case <-time.After(15 * time.Second):
		t.Fatal("the result never reached the controller")
	}
	first := time.Now()
	deliveries := 1
	window := time.After(2500 * time.Millisecond)
	for waiting := true; waiting; {
		select {
		case <-got:
			deliveries++
			if deliveries == 2 {
				if gap := time.Since(first); gap < 900*time.Millisecond {
					t.Errorf("the result came back %s after the controller could not record it", gap)
				}
			}
		case <-window:
			waiting = false
		}
	}
	if deliveries > 3 {
		t.Errorf("the result came round %d times in two and a half seconds", deliveries)
	}
	if deliveries < 2 {
		t.Error("the result never came back, and a result the controller could not record is delivered again")
	}
}

// A result no delivery would change is taken off the queue and said out loud, as a message nobody
// can read is. Left for the next delivery it would come round for ever, since this consumer delivers
// without limit and nothing about the result changes in between. Which results those are is the
// controller's to say, and it says so with Drop, in words of its own that what is said keeps, so
// that a runner speaking for a task it does not hold is told apart from one that misread the wire.
func TestAResultNoDeliveryWouldChangeIsTakenOffAndReported(t *testing.T) {
	b := open(t)
	trouble := make(chan error, 8)
	b.Trouble = func(_ string, err error) { trouble <- err }

	task := aTask(step(t))
	if err := b.Report(t.Context(), aResult(task)); err != nil {
		t.Fatal(err)
	}
	seen := reporting(t, b, func(h heard) error {
		return Drop(fmt.Errorf("%w: %s is bound to runner-lan-01", errTest, h.result.IdempotencyKey))
	})

	select {
	case h := <-seen:
		if h.result.IdempotencyKey != string(task.ID) {
			t.Fatalf("the controller was handed %s", h.result.IdempotencyKey)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the result never reached the controller")
	}
	select {
	case err := <-trouble:
		if !errors.Is(err, errTest) || err.Error() != errTest.Error()+": "+string(task.ID)+" is bound to runner-lan-01" {
			t.Errorf("what was said reads %q", err)
		}
	case h := <-seen:
		t.Fatalf("it was delivered again rather than taken off the queue: %+v", h.result)
	case <-time.After(15 * time.Second):
		t.Fatal("nothing was said about a result taken off the queue")
	}

	// And it is off the queue rather than coming round for ever.
	select {
	case h := <-seen:
		t.Errorf("it came round again: %+v", h.result)
	case err := <-trouble:
		t.Errorf("it was said twice, the second time as %q", err)
	case <-time.After(2 * time.Second):
	}
}

// A result is the word of the runner whose subject it arrives on, and the controller is handed that
// runner beside it, whatever the result says. One naming another runner is a machine of the pool
// speaking for somebody else's task, which the controller refuses as a result from a runner that
// does not hold it, and it can only do that knowing who sent it.
func TestAResultIsHandedOnWithTheRunnerWhoseSubjectItCameOn(t *testing.T) {
	b := open(t)
	result := aResult(aTask(step(t)))
	body, err := result.encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.js.Publish(t.Context(), ResultSubject("runner-lan-01"), body); err != nil {
		t.Fatal(err)
	}

	seen := reporting(t, b, func(heard) error { return nil })
	select {
	case h := <-seen:
		if h.sender != "runner-lan-01" || h.result.Runner != "runner-dmz-02" {
			t.Errorf("a result runner-lan-01 published as runner-dmz-02 was handed on from %s as %s's", h.sender, h.result.Runner)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the result never reached the controller")
	}
}

// A take asking for a batch answers as soon as one task is there, with whatever else is already
// there, rather than holding the first until the batch fills or the wait runs out.
func TestATakeForABatchAnswersOnceOneTaskIsThere(t *testing.T) {
	b := open(t)
	m := message(step(t))
	go func() {
		time.Sleep(300 * time.Millisecond)
		if err := b.Publish(context.Background(), DefaultPool, m); err != nil {
			t.Error(err)
		}
	}()
	began := time.Now()
	taken, err := b.Take(t.Context(), DefaultPool, 8, 20*time.Second)
	if err != nil || len(taken) != 1 {
		t.Fatalf("a take for 8 answered %d tasks and %v", len(taken), err)
	}
	if waited := time.Since(began); waited > 5*time.Second {
		t.Errorf("a take for 8 held the one task there for %s", waited)
	}
}

// A take waits for work until its wait runs out or its context ends, whichever comes first, so an
// agent being stopped is not held for the rest of a long poll. A wait that runs out with nothing
// taken is no failure.
func TestATakeEndsWhenItsContextDoes(t *testing.T) {
	b := open(t)

	taken, err := b.Take(t.Context(), "dmz", 8, 300*time.Millisecond)
	if err != nil || len(taken) != 0 {
		t.Fatalf("a take that found nothing answered %d tasks and %v", len(taken), err)
	}

	// Stopped, as an agent is, rather than given a deadline, which a take reads as a shorter
	// wait.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	time.AfterFunc(300*time.Millisecond, cancel)
	began := time.Now()
	if _, err := b.Take(ctx, "dmz", 8, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("a take whose context was cancelled answered %v", err)
	}
	if waited := time.Since(began); waited > 5*time.Second {
		t.Errorf("a take whose context was cancelled after 300ms waited %s", waited)
	}
}

// A pool the control plane made no consumer for has nothing to take from, and Take says so
// rather than making one. A runner able to create a consumer could create one with no filter,
// and the credential a runner holds is refused the attempt anyway.
func TestTakingFromAPoolWithNoConsumerSaysSo(t *testing.T) {
	b := open(t)

	_, err := b.Take(t.Context(), "nobody", 8, 300*time.Millisecond)
	if err == nil {
		t.Fatal("a pool with no consumer was taken from")
	}
	if !strings.Contains(err.Error(), "pool nobody has no consumer") {
		t.Errorf("the refusal reads %q, and it names the pool", err)
	}

	names := b.stream.ConsumerNames(t.Context())
	for name := range names.Name() {
		if name != Durable(DefaultPool) && name != Durable("dmz") {
			t.Errorf("taking from a pool with no consumer created %s", name)
		}
	}
	if err := names.Err(); err != nil {
		t.Fatal(err)
	}
}

// What a pool may be called, refused where it would reach another pool's work, before anything
// is published.
func TestATaskIsNotPublishedToWhatIsNotARunnerPool(t *testing.T) {
	b := open(t)
	for _, c := range []struct {
		pool string
		why  string
	}{
		{"a.b", "a dot, which makes one pool's subject a prefix of another's"},
		{"*", "a wildcard, which makes it every pool's"},
		{">", "the other wildcard"},
		{"", "nothing at all"},
	} {
		if err := b.Publish(t.Context(), c.pool, message(step(t))); err == nil {
			t.Errorf("a task was published to %q, and it is %s", c.pool, c.why)
		}
	}
}

// "A step goes to the pool whose labels include every label of its runs_on", one pool and never
// two, and a step that names none goes to the pool default.
func TestATaskGoesToThePoolWhoseLabelsIncludeEveryOneOfItsOwn(t *testing.T) {
	pools := []Pool{
		{Name: DefaultPool},
		{Name: "dmz", Labels: []string{"zone=dmz", "arch=amd64"}},
		{Name: "dmz-arm", Labels: []string{"zone=dmz", "arch=arm64"}},
		{Name: "home", Labels: []string{"site=home"}},
	}
	for _, c := range []struct {
		runsOn []string
		want   string
	}{
		{nil, DefaultPool},
		{[]string{}, DefaultPool},
		{[]string{"zone=dmz", "arch=amd64"}, "dmz"},
		{[]string{"arch=arm64"}, "dmz-arm"},
		{[]string{"site=home"}, "home"},
	} {
		got, err := Route(c.runsOn, pools)
		if err != nil {
			t.Errorf("%v: %s", c.runsOn, err)
			continue
		}
		if got != c.want {
			t.Errorf("%v goes to the pool %q, want %q", c.runsOn, got, c.want)
		}
	}

	for _, c := range []struct {
		runsOn []string
		pools  []Pool
		want   []string
		why    string
	}{
		{[]string{"zone=dmz"}, pools, []string{"dmz", "dmz-arm"}, "two pools carry it, and a task on two queues runs twice"},
		{[]string{"zone=dmz", "gpu=true"}, pools, nil, "no pool carries every label, though one carries some"},
		{[]string{"zone=lan"}, pools, nil, "no pool carries it"},
		{nil, pools[1:], nil, "a task naming nothing goes to the pool default and nowhere else, and there is none"},
	} {
		_, err := Route(c.runsOn, c.pools)
		var unrouted *Unrouted
		if !errors.As(err, &unrouted) {
			t.Errorf("%v was routed, and %s: %v", c.runsOn, c.why, err)
			continue
		}
		if !slices.Equal(unrouted.Pools, c.want) {
			t.Errorf("%v is matched by %v, want %v", c.runsOn, unrouted.Pools, c.want)
		}
	}

	if _, err := Route([]string{"arch"}, pools); err == nil || errors.As(err, new(*Unrouted)) {
		t.Errorf("a label with no value was answered %v", err)
	}
}

var errTest = errTestType{}

type errTestType struct{}

func (errTestType) Error() string { return "the controller could not record it" }

// A message nobody can read is taken off the queue and said out loud. Swallowing it silently is
// how a wire that stopped matching becomes a queue that quietly eats everything on it, which is
// the shape of a bug that took an afternoon to find once.
func TestAMessageNobodyCanReadIsSaidOutLoud(t *testing.T) {
	b := open(t)
	var trouble []error
	b.Trouble = func(_ string, err error) { trouble = append(trouble, err) }

	if err := b.conn.Publish(Subject(DefaultPool), []byte("{not a task")); err != nil {
		t.Fatal(err)
	}
	flush, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if err := b.conn.FlushWithContext(flush); err != nil {
		t.Fatal(err)
	}

	taken, err := b.Take(t.Context(), DefaultPool, 8, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 0 {
		t.Fatalf("a message nobody can read was handed out as work: %+v", taken)
	}
	if len(trouble) != 1 {
		t.Fatalf("%d things were said about it, and it is worth saying once", len(trouble))
	}
	if !strings.Contains(trouble[0].Error(), "could not be read") {
		t.Errorf("what was said reads %q", trouble[0])
	}

	// And it is off the queue rather than coming round for ever.
	again, err := b.Take(t.Context(), DefaultPool, 8, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("it came round again: %+v", again)
	}
}

// A stop is not work. It goes to whoever is holding the task rather than onto the queue, where
// it would be invisible to the holder and would look like work to everybody else.
func TestAStopGoesToWhoeverIsHolding(t *testing.T) {
	b := open(t)

	sub, err := b.conn.SubscribeSync(StopSubject)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()

	task := aTask(step(t))
	if err := b.Stop(t.Context(), graph.Stop{Task: task.ID, Reason: graph.StopCancelled}); err != nil {
		t.Fatal(err)
	}

	msg, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("no stop arrived: %s", err)
	}
	if err := validates(t, wire(t, "stop"), msg.Data); err != nil {
		t.Errorf("the stop published, %s, is refused by the wire: %s", msg.Data, err)
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

	// And it is not on the work queue, where a runner with room would take it as a task.
	taken, err := b.Take(t.Context(), DefaultPool, 8, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 0 {
		t.Errorf("a stop was offered as work: %+v", taken)
	}
}
