package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/ulid"
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

// rows are the task_id each step's dispatch goes out as, minted once per step and per test
// process for the reason aRun is: the row is what the stream deduplicates on, so one written down
// would make every publish after the first in a two-minute window publish nothing, and one minted
// on every call would make a publish repeated inside a test two messages.
var rows sync.Map

func rowOf(step agk.Step) string {
	row, _ := rows.LoadOrStore(step, ulid.New())
	return row.(string)
}

// The round trip, which is the whole contract: the controller publishes and a runner of the pool
// the labels select takes it, whole.
func TestATaskGoesToThePoolItsLabelsSelect(t *testing.T) {
	b := open(t)

	if err := b.Publish(t.Context(), dispatch(step(t), "pool=dmz", "arch=amd64")); err != nil {
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

// A task that names no pool goes to the default one, which is what a step with no runs_on asks
// for.
func TestATaskWithNoPoolGoesToTheDefault(t *testing.T) {
	b := open(t)
	if err := b.Publish(t.Context(), dispatch(step(t))); err != nil {
		t.Fatal(err)
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
	if err := b.Publish(t.Context(), dispatch(step(t))); err != nil {
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

// A task is held once the server says the acknowledgement arrived, and not once it has left
// this side. A link that drops keeps the acknowledgement in the client's buffer, and the server
// hands the task to another runner of the pool when its wait runs out, so a runner told it held
// the task on the strength of the buffer would start the container beside that one.
func TestATaskIsHeldOnlyOnceTheServerHasTheAcknowledgement(t *testing.T) {
	b := open(t)
	if err := b.Publish(t.Context(), dispatch(step(t))); err != nil {
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
// duplicate window is one message rather than two: the task_id is what makes a retry free.
func TestPublishingOneTaskTwiceQueuesItOnce(t *testing.T) {
	b := open(t)
	for range 3 {
		if err := b.Publish(t.Context(), dispatch(step(t))); err != nil {
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
		Usage: &Usage{CPUSeconds: 12.4, MaxRSSBytes: 198443008},
	}
}

// answering runs Answers until the test ends, handing each result to fn and on to the channel it
// answers.
func answering(t *testing.T, b *Bus, fn func(controller.Answer) error) <-chan controller.Answer {
	t.Helper()
	ctx, stop := context.WithCancel(t.Context())
	got := make(chan controller.Answer, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := b.Answers(ctx, func(_ context.Context, a controller.Answer) error {
			got <- a
			return fn(a)
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
	lost := dispatch(step(t))
	requeued := dispatchAs(ulid.New(), step(t))
	for _, d := range []controller.Dispatch{lost, requeued} {
		if err := b.Publish(t.Context(), d); err != nil {
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
	for i, want := range []controller.Dispatch{lost, requeued} {
		got := taken[i].Task
		if got.TaskID != want.Row || got.IdempotencyKey != string(want.Task.ID) {
			t.Errorf("message %d is task_id %s under key %s, want %s under %s", i+1, got.TaskID, got.IdempotencyKey, want.Row, want.Task.ID)
		}
		taken[i].Held(t.Context())
	}
}

// A result goes back and the controller takes it, once.
func TestAResultComesBackToTheController(t *testing.T) {
	b := open(t)
	task := aTask(step(t))
	result := aResult(task)
	if err := b.Report(t.Context(), result); err != nil {
		t.Fatal(err)
	}

	got := answering(t, b, func(controller.Answer) error { return nil })
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
	got := make(chan controller.Answer, 4)
	go func() {
		b.Answers(ctx, func(_ context.Context, a controller.Answer) error {
			got <- a
			return nil
		})
	}()
	rows := map[string]bool{}
	for len(rows) < 2 {
		select {
		case a := <-got:
			if !sent[a.Row] {
				t.Errorf("the controller was handed dispatch %s, and the two sent were %v", a.Row, sent)
			}
			rows[a.Row] = true
		case <-ctx.Done():
			t.Fatalf("the controller was handed %d of the two endings of one key", len(rows))
		}
	}
	select {
	case a := <-got:
		t.Errorf("a third result reached the controller: %+v", a)
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
	requeue := dispatchAs(ulid.New(), step(t))
	if err := b.Publish(t.Context(), requeue); err != nil {
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

	other := aResult(aTask(step(t) + "-other"))
	if err := b.Ended(t.Context(), taken[0], other); err == nil {
		t.Error("the ending of another key was sent as the answer to the requeue")
	}
	if err := b.Ended(t.Context(), taken[0], recorded); err != nil {
		t.Fatalf("answering the requeue with the recorded ending: %s", err)
	}

	again, err := b.Take(t.Context(), DefaultPool, 1, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("a requeue answered from the record was offered again: %+v", again)
	}

	got := answering(t, b, func(controller.Answer) error { return nil })
	select {
	case a := <-got:
		if a.Row != requeue.Row || a.Result.Task != task.ID || a.Runner != recorded.Runner {
			t.Errorf("the controller was handed dispatch %s of %s from %s, want %s of %s from %s", a.Row, a.Result.Task, a.Runner, requeue.Row, task.ID, recorded.Runner)
		}
		if a.Result.State != agk.TaskSucceeded || len(a.Outputs) != 1 || a.LogLines != 412 {
			t.Errorf("the recorded ending came back as %+v", a)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the recorded ending never reached the controller")
	}
	select {
	case a := <-got:
		t.Errorf("a second result reached the controller: %+v", a)
	case <-time.After(500 * time.Millisecond):
	}
}

// One dispatch delivered to two machines is redeemed by one of them, and the other may report the
// unreached failure the controller refuses from it. The failure of the runner that holds it then
// follows under the same task_id and the same ending, and it still reaches the controller: two
// runners' results are two results, whatever they say. The holder publishing its own again, after
// an answer it never heard, is still one.
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

	got := answering(t, b, func(a controller.Answer) error {
		if a.Runner != held.Runner {
			return fmt.Errorf("%w: %w", controller.ErrNotAResult, controller.ErrNotTheHolder)
		}
		return nil
	})
	heard := map[string]int{}
	for len(heard) < 2 {
		select {
		case a := <-got:
			heard[a.Runner]++
		case <-time.After(10 * time.Second):
			t.Fatalf("the controller was handed results from %v, and the holder's failure never arrived", heard)
		}
	}
	select {
	case a := <-got:
		heard[a.Runner]++
	case <-time.After(500 * time.Millisecond):
	}
	if heard[unreached.Runner] != 1 || heard[held.Runner] != 1 {
		t.Errorf("the controller was handed %v, want one result from each runner", heard)
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
	got := answering(t, b, func(controller.Answer) error {
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

	got := answering(t, b, func(controller.Answer) error { return errTest })
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

// A result no controller could ever record is taken off the queue and said out loud, as a
// message nobody can read is. Left for the next delivery it would come round for ever, since
// this consumer delivers without limit and nothing about the result changes in between. The
// controller says which kind it is, and what is said keeps it, so that a runner speaking for a
// task it does not hold is told apart from one that misread the wire.
func TestAResultTheControllerWillNeverRecordIsTakenOffAndReported(t *testing.T) {
	b := open(t)
	trouble := make(chan error, 8)
	b.Trouble = func(_ string, err error) { trouble <- err }

	task := aTask(step(t))
	if err := b.Report(t.Context(), aResult(task)); err != nil {
		t.Fatal(err)
	}
	seen := answering(t, b, func(a controller.Answer) error {
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
	b := open(t)
	trouble := make(chan error, 8)
	b.Trouble = func(_ string, err error) { trouble <- err }

	result := aResult(aTask(step(t)))
	body, err := result.encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.js.Publish(t.Context(), ResultSubject("runner-lan-01"), body); err != nil {
		t.Fatal(err)
	}

	seen := answering(t, b, func(controller.Answer) error { return nil })
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

// What a pool may be called, refused where it would reach another pool's work.
func TestWhatIsNotARunnerPool(t *testing.T) {
	for _, c := range []struct {
		labels []string
		why    string
	}{
		{[]string{"pool=a.b"}, "a dot, which makes one pool's subject a prefix of another's"},
		{[]string{"pool=*"}, "a wildcard, which makes it every pool's"},
		{[]string{"pool=>"}, "the other wildcard"},
		{[]string{"pool="}, "nothing at all"},
		{[]string{"arch"}, "a label with no value"},
	} {
		if _, err := PoolOf(c.labels); err == nil {
			t.Errorf("%v was read as a pool, and it is %s", c.labels, c.why)
		}
	}
	for _, c := range []struct {
		labels []string
		want   string
	}{
		{nil, DefaultPool},
		{[]string{"arch=amd64"}, DefaultPool},
		{[]string{"pool=dmz"}, "dmz"},
		{[]string{"zone=lan", "pool=bare_metal-1"}, "bare_metal-1"},
	} {
		got, err := PoolOf(c.labels)
		if err != nil {
			t.Errorf("%v: %s", c.labels, err)
			continue
		}
		if got != c.want {
			t.Errorf("%v selects pool %q, want %q", c.labels, got, c.want)
		}
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
