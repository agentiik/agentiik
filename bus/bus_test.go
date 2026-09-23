package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/graph"
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
	return controller.Dispatch{
		Task: aTask(step, runsOn...),
		Row:  "01M2AAZ9G62NQXFAFCXKRPJEH5",
		Grant: "agkgrant_01M2AAZ9G62NQXFAFCXKRPJEH5_" +
			"dGFza2dyYW50ZXhhbXBsZTAxMjM0NTY3ODlhYmNkZWZnaGk",
		Inputs: map[agk.Port]controller.InputRef{},
	}
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
	if err := taken[0].Held(); err != nil {
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
	taken[0].Held()
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
	second[0].Held()
}

// "JetStream guarantees at-least-once delivery", so publishing the same task twice inside the
// duplicate window is one message rather than two: the key is what makes a retry free.
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
	taken[0].Held()
}

// A result goes back and the controller takes it, once.
func TestAResultComesBackToTheController(t *testing.T) {
	b := open(t)
	task := aTask(step(t))
	answer := controller.Answer{
		Result: graph.Result{
			Task: task.ID, State: agk.TaskSucceeded,
			Outputs: map[agk.Port]agk.Envelope{},
		},
		Runner:   "runner-dmz-02",
		LogLines: 412,
		Usage:    map[string]any{"cpu_seconds": 12.4},
	}
	if err := b.Report(t.Context(), answer); err != nil {
		t.Fatal(err)
	}

	ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	got := make(chan controller.Answer, 4)
	go func() {
		if err := b.Answers(ctx, func(_ context.Context, a controller.Answer) error {
			got <- a
			return nil
		}); err != nil && ctx.Err() == nil {
			t.Errorf("taking results: %s", err)
		}
	}()

	select {
	case a := <-got:
		if a.Result.Task != task.ID || a.Runner != "runner-dmz-02" || a.LogLines != 412 {
			t.Errorf("the result came back as %+v", a)
		}
		if a.Usage["cpu_seconds"] != 12.4 {
			t.Errorf("the usage came back as %v", a.Usage)
		}
	case <-ctx.Done():
		t.Fatal("no result reached the controller")
	}

	select {
	case a := <-got:
		t.Errorf("the result was delivered twice: %+v", a)
	case <-time.After(500 * time.Millisecond):
	}
}

// A result the controller could not record is left for the next delivery, which is what
// at-least-once buys.
func TestAResultTheControllerRefusesComesBack(t *testing.T) {
	b := open(t)
	task := aTask(step(t))
	if err := b.Report(t.Context(), controller.Answer{
		Result: graph.Result{Task: task.ID, State: agk.TaskSucceeded},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, stop := context.WithTimeout(t.Context(), 15*time.Second)
	defer stop()
	seen := make(chan int, 8)
	tries := 0
	go func() {
		b.Answers(ctx, func(_ context.Context, a controller.Answer) error {
			tries++
			seen <- tries
			if tries == 1 {
				return errTest
			}
			return nil
		})
	}()

	for want := 1; want <= 2; want++ {
		select {
		case got := <-seen:
			if got != want {
				t.Fatalf("delivery %d arrived as %d", want, got)
			}
		case <-ctx.Done():
			t.Fatalf("delivery %d never arrived, so a result the controller refused was lost", want)
		}
	}
}

// A result no controller could ever record is taken off the queue and said out loud, as a
// message nobody can read is. Left for the next delivery it would come round for ever, since
// this consumer delivers without limit and nothing about the result changes in between.
func TestAResultThatIsNotAnEndingIsTakenOffAndReported(t *testing.T) {
	b := open(t)
	trouble := make(chan error, 8)
	b.Trouble = func(_ string, err error) { trouble <- err }

	task := aTask(step(t))
	if err := b.Report(t.Context(), controller.Answer{
		Result: graph.Result{Task: task.ID, State: agk.TaskRunning},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, stop := context.WithTimeout(t.Context(), 15*time.Second)
	defer stop()
	seen := make(chan controller.Answer, 8)
	go func() {
		b.Answers(ctx, func(_ context.Context, a controller.Answer) error {
			seen <- a
			// Wrapped, as Core.Answer wraps it.
			return fmt.Errorf("%w: %s is %s", controller.ErrNotAResult, a.Result.Task, a.Result.State)
		})
	}()

	select {
	case a := <-seen:
		if a.Result.Task != task.ID {
			t.Fatalf("the controller was handed %s", a.Result.Task)
		}
	case <-ctx.Done():
		t.Fatal("the result never reached the controller")
	}
	select {
	case err := <-trouble:
		if !errors.Is(err, controller.ErrNotAResult) {
			t.Errorf("what was said reads %q", err)
		}
	case a := <-seen:
		t.Fatalf("it was delivered again rather than taken off the queue: %+v", a)
	case <-ctx.Done():
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
