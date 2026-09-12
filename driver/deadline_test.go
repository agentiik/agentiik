package driver

import (
	"archive/tar"
	"bytes"
	"slices"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// quiet is the log of a task nobody is keeping one for, which is what these rules need:
// they are about signals and exits, and the lines are read elsewhere.
func quiet() *taskLog { return newLog(nil, newMasker(), nil, 0, 0) }

// tarOf writes one file the way a container archive carries it.
func tarOf(t *testing.T, content string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: "brick.yaml", Mode: 0o444, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// started creates and starts one container on a fake daemon, and hands back the client,
// the daemon, the container and the wait that was opened before the start.
func started(t *testing.T, run func(dockertest.Container) (int, error), bs ...dockertest.Behaviour) (*docker.Client, *dockertest.Daemon, string, <-chan docker.Waited) {
	t.Helper()
	bs = append(bs, dockertest.With(dockertest.Options{
		Run:    run,
		Images: map[string]dockertest.Image{"brick": {Digest: imageDigest}},
	}))
	cli, daemon := withDaemon(t, bs...)

	created, err := cli.ContainerCreate(t.Context(), "", docker.Config{
		Image:  "brick",
		Labels: map[string]string{LabelTask: "01JMZ8V1P9C4/fetch/1"},
	}, docker.HostConfig{}, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("creating: %s", err)
	}
	waited, err := cli.ContainerWait(t.Context(), created.ID, docker.WaitNextExit)
	if err != nil {
		t.Fatalf("opening the wait: %s", err)
	}
	if err := cli.ContainerStart(t.Context(), created.ID); err != nil {
		t.Fatalf("starting: %s", err)
	}
	return cli, daemon, created.ID, waited
}

// "Enforce the step timeout with SIGTERM then SIGKILL after grace." The escalation is the
// daemon's own stop, so that it survives this process dying between the two signals.
func TestTheStepTimeoutIsSigtermThenSigkillAfterTheGrace(t *testing.T) {
	cli, daemon, id, waited := started(t, func(c dockertest.Container) (int, error) {
		// A container that takes its term and does not go is exactly the one
		// the escalation exists for.
		<-c.Signalled()
		time.Sleep(30 * time.Second)
		return 0, nil
	})

	w := newWatch(cli, id, "fetch", quiet(), time.Now().Add(20*time.Millisecond), 100*time.Millisecond, nil)
	e, err := w.await(t.Context(), waited)
	if err != nil {
		t.Fatalf("waiting on a container past its deadline: %s", err)
	}

	if !e.TimedOut {
		t.Error("the deadline fired and the exit does not say so")
	}
	if e.state() != agk.TaskTimedOut {
		t.Errorf("the state is %s, and an attempt stopped at its deadline is timed_out", e.state())
	}
	if e.Code != 137 {
		t.Errorf("the exit code is %d, and a container that was killed leaves 137", e.Code)
	}

	signals := daemon.Created()[0].Signals
	if !slices.Equal(signals, []string{"SIGTERM", "SIGKILL"}) {
		t.Errorf("the container was sent %v, and the rule is SIGTERM then SIGKILL after the grace", signals)
	}
}

// A deadline that did not fire leaves the exit alone: the code is the container's own and
// the state is the exit code table's.
func TestAnExitInsideItsDeadlineIsTheTablesToRead(t *testing.T) {
	cli, daemon, id, waited := started(t, func(dockertest.Container) (int, error) { return 120, nil })

	w := newWatch(cli, id, "fetch", quiet(), time.Now().Add(time.Hour), time.Second, nil)
	e, err := w.await(t.Context(), waited)
	if err != nil {
		t.Fatalf("waiting: %s", err)
	}
	if e.TimedOut || e.Stopped {
		t.Errorf("a container that exited on its own reads as %+v", e)
	}
	if e.Code != 120 || e.state() != agk.TaskFailed {
		t.Errorf("exit 120 reads as %d and %s, and the table has it as a permanent failure", e.Code, e.state())
	}
	if signals := daemon.Created()[0].Signals; len(signals) != 0 {
		t.Errorf("a container inside its deadline was sent %v", signals)
	}
}

// "follow the daemon event stream to catch an exit the driver did not cause, an
// out-of-memory kill included". On this one the wait never answers at all.
func TestAnExitTheWaitMissedIsCaughtOnTheEventStream(t *testing.T) {
	cli, _, id, waited := started(t,
		func(dockertest.Container) (int, error) { return 0, nil },
		dockertest.OOMKills)

	w := newWatch(cli, id, "fetch", quiet(), time.Time{}, time.Second, nil)

	// The one event goroutine of a Docker hands events to the watch of the task they
	// are about, which is what this stands in for.
	events, _ := cli.Events(t.Context(), time.Time{}, docker.Filters{}.Add("type", docker.EventTypeContainer))
	go func() {
		for e := range events {
			w.event(e)
		}
	}()

	e, err := w.await(t.Context(), waited)
	if err != nil {
		t.Fatalf("waiting on a container the wait never reported: %s", err)
	}
	if e.Source != "the daemon event stream" {
		t.Errorf("the exit was seen by %s, and the wait never sees this one", e.Source)
	}
	if e.Code != 137 {
		t.Errorf("the exit code is %d, and an out-of-memory kill leaves 137", e.Code)
	}
	if !e.OOM {
		t.Error("the kill was not reported as an out-of-memory one, and the oom event is the only place the reason is written")
	}
	if e.state() != agk.TaskFailed {
		t.Errorf("the state is %s, and 137 is read off the exit code table like any other code", e.state())
	}
}

// The inspect is the backstop under both: a wait that ended before the container did is a
// task that is slow and never one that is wrong.
func TestTheInspectIsTheBackstopWhenTheWaitEndedFirst(t *testing.T) {
	cli, _, id, _ := started(t, func(dockertest.Container) (int, error) { return 42, nil })

	// A wait that was closed with nothing on it, which is what a daemon closing the
	// connection under a running task leaves behind.
	broken := make(chan docker.Waited)
	close(broken)

	w := newWatch(cli, id, "fetch", quiet(), time.Time{}, time.Second, nil)
	e, err := w.await(t.Context(), broken)
	if err != nil {
		t.Fatalf("waiting with no wait: %s", err)
	}
	if e.Source != "an inspect" {
		t.Errorf("the exit was seen by %s, and with no wait and no stream the inspect is the only source left", e.Source)
	}
	if e.Code != 42 {
		t.Errorf("the exit code is %d, want 42", e.Code)
	}
}

// A stop that landed is cancelled, and one whose reason was the deadline is timed out.
// Neither reads the code the kill left behind, because that code says how a container was
// killed and not why.
func TestAStopThatLandedDecidesTheStateAndNotTheCode(t *testing.T) {
	for _, c := range []struct {
		name  string
		mark  func(*watch)
		state agk.TaskState
	}{
		{name: "cancelled", mark: func(w *watch) { w.stopping(agk.TaskCancelled) }, state: agk.TaskCancelled},
		{name: "timed out", mark: func(w *watch) { w.stopping(agk.TaskTimedOut) }, state: agk.TaskTimedOut},
	} {
		t.Run(c.name, func(t *testing.T) {
			cli, daemon, id, waited := started(t, func(c dockertest.Container) (int, error) {
				<-c.Signalled()
				return 137, nil
			})

			w := newWatch(cli, id, "fetch", quiet(), time.Time{}, 100*time.Millisecond, nil)
			c.mark(w)
			w.sendStop(t.Context())

			e, err := w.await(t.Context(), waited)
			if err != nil {
				t.Fatalf("waiting on a stopped container: %s", err)
			}
			if e.state() != c.state {
				t.Errorf("the state is %s, want %s", e.state(), c.state)
			}
			if signals := daemon.Created()[0].Signals; len(signals) == 0 || signals[0] != "SIGTERM" {
				t.Errorf("the container was sent %v, and a stop is SIGTERM first", signals)
			}
		})
	}
}

// A stop that landed before the container was running is sent again once there is a
// process to signal.
//
// The task is registered before its container is started, so that a stop can reach it at
// all, and the daemon answers a stop on a container it has not started with 304 Not
// Modified: nothing is signalled and nothing is remembered. Without the second send, a
// cancelled run keeps its container for the rest of its natural life, which is the one
// thing Stop exists to prevent.
func TestAStopThatLandedBeforeTheStartIsSentAgain(t *testing.T) {
	cli, daemon := withDaemon(t, dockertest.With(dockertest.Options{
		Run: func(c dockertest.Container) (int, error) {
			<-c.Signalled()
			return 137, nil
		},
		Images: map[string]dockertest.Image{"brick": {Digest: imageDigest}},
	}))

	created, err := cli.ContainerCreate(t.Context(), "", docker.Config{
		Image:  "brick",
		Labels: map[string]string{LabelTask: "01JMZ8V1P9C4/fetch/1"},
	}, docker.HostConfig{}, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("creating: %s", err)
	}
	waited, err := cli.ContainerWait(t.Context(), created.ID, docker.WaitNextExit)
	if err != nil {
		t.Fatalf("opening the wait: %s", err)
	}

	w := newWatch(cli, created.ID, "fetch", quiet(), time.Time{}, 100*time.Millisecond, nil)

	// The stop lands in the window: the container is created and is not started.
	// It is issued on this goroutine rather than through sendStop, so that the
	// daemon has certainly seen it before the start and the window is the window
	// rather than a race in the test.
	w.stopping(agk.TaskCancelled)
	if err := cli.ContainerStop(t.Context(), created.ID, 100*time.Millisecond); err != nil {
		t.Fatalf("stopping a container the daemon has not started: %s", err)
	}
	if signals := daemon.Created()[0].Signals; len(signals) != 0 {
		t.Fatalf("a container that was never started was sent %v", signals)
	}

	if err := cli.ContainerStart(t.Context(), created.ID); err != nil {
		t.Fatalf("starting: %s", err)
	}
	w.resendStop(t.Context())

	e, err := w.await(t.Context(), waited)
	if err != nil {
		t.Fatalf("waiting on a container stopped before it started: %s", err)
	}
	if e.state() != agk.TaskCancelled {
		t.Errorf("the state is %s, and work called off is cancelled", e.state())
	}
	if signals := daemon.Created()[0].Signals; len(signals) == 0 || signals[0] != "SIGTERM" {
		t.Errorf("the container was sent %v, and the stop that landed first was lost", signals)
	}
}

// The exit code a die event carries, read off the attribute the daemon writes it in.
func TestTheExitCodeOfADieEventIsRead(t *testing.T) {
	for _, c := range []struct {
		attributes map[string]string
		code       int
		ok         bool
	}{
		{attributes: map[string]string{"exitCode": "0"}, code: 0, ok: true},
		{attributes: map[string]string{"exitCode": "137"}, code: 137, ok: true},
		{attributes: map[string]string{"exitCode": "-1"}, code: -1, ok: true},
		{attributes: map[string]string{"exitCode": ""}, ok: false},
		{attributes: map[string]string{"exitCode": "none"}, ok: false},
		{attributes: nil, ok: false},
	} {
		code, ok := exitCodeOf(docker.Event{Actor: docker.EventActor{Attributes: c.attributes}})
		if ok != c.ok || (ok && code != c.code) {
			t.Errorf("%v read as %d %v, want %d %v", c.attributes, code, ok, c.code, c.ok)
		}
	}
}

// A task with no timeout has no deadline, and one with a timeout and no moment has the
// moment the dispatch fixed: "recording the dispatch is what fixes the task's deadline".
func TestTheDeadlineIsTheMomentOrTheDispatchPlusTheTimeout(t *testing.T) {
	dispatched := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	if got := deadlineOf(imageTask("brick"), dispatched); !got.IsZero() {
		t.Errorf("a task with neither a deadline nor a timeout has one at %s", got)
	}

	withTimeout := imageTask("brick")
	withTimeout.Timeout = graph.Duration(90 * time.Second)
	if got := deadlineOf(withTimeout, dispatched); !got.Equal(dispatched.Add(90 * time.Second)) {
		t.Errorf("the deadline is %s, and a timeout runs from the dispatch", got)
	}

	fixed := imageTask("brick")
	fixed.Timeout = graph.Duration(90 * time.Second)
	fixed.Deadline = dispatched.Add(time.Hour)
	if got := deadlineOf(fixed, dispatched); !got.Equal(fixed.Deadline) {
		t.Errorf("the deadline is %s, and the evaluator fixed one at %s", got, fixed.Deadline)
	}
}
