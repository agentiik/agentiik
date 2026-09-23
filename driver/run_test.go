package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// runner is a Docker built on a fake daemon, with a store, a work root and an observer,
// which is the smallest thing that can run one task end to end.
type runner struct {
	*Docker
	daemon   *dockertest.Daemon
	work     string
	observed *recorder
}

// recorder keeps what the observer was told, which is where the transitions a heartbeat
// needs arrive and where the log reference and the artifacts leave.
type recorder struct {
	mu sync.Mutex
	es []Event
}

func (r *recorder) Observe(_ context.Context, e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.es = append(r.es, e)
}

func (r *recorder) states() []agk.TaskState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]agk.TaskState, 0, len(r.es))
	for _, e := range r.es {
		out = append(out, e.State)
	}
	return out
}

// secretSource answers with the values a task was given, as a runner redeeming its grant
// would.
type secretSource map[string]string

func (s secretSource) Value(_ context.Context, name string) ([]byte, error) {
	v, ok := s[name]
	if !ok {
		return nil, fmt.Errorf("no secret named %s", name)
	}
	return []byte(v), nil
}

// newRunner builds one, on a daemon told what its images are and what a container does.
func newRunner(t *testing.T, images map[string]dockertest.Image, run func(dockertest.Container) (int, error), bs ...dockertest.Behaviour) *runner {
	t.Helper()

	bs = append(bs, dockertest.With(dockertest.Options{Run: run, Images: images}))
	daemon, err := dockertest.NewDaemon(bs...)
	if err != nil {
		t.Fatalf("starting a fake daemon: %s", err)
	}
	t.Cleanup(func() { daemon.Close() })

	work := t.TempDir()
	store, err := artifact.New(artifact.Dir(t.TempDir()), "finance", agk.DefaultLimits())
	if err != nil {
		t.Fatalf("opening the store: %s", err)
	}
	observed := &recorder{}

	policy := DefaultPolicy()
	// The fake daemon does not remap, which is what Docker Desktop answers, so the
	// floor is lifted the way an operator lifts it on a machine that is not an
	// installation.
	policy.RequireUsernsRemap = RemapLifted
	policy.StopGrace = 200 * time.Millisecond
	// A secret written into the task's working directory rather than onto a tmpfs,
	// which is what a laptop with no /dev/shm does.
	policy.SecretsDir = ""

	d, err := New(Config{
		Socket:   daemon.Socket(),
		Store:    func(string) (*artifact.Store, error) { return store, nil },
		Repo:     func(context.Context, string, string, string) (string, error) { return t.TempDir(), nil },
		Secrets:  secretSource{"bearer": "s3cr3t-value"},
		Observer: observed,
		Policy:   policy,
		WorkRoot: work,
		Announce: func(string) {},
	})
	if err != nil {
		t.Fatalf("opening the driver: %s", err)
	}
	t.Cleanup(func() { d.Close() })

	return &runner{Docker: d, daemon: daemon, work: work, observed: observed}
}

// oneTask is one task of one brick, with an envelope on one input port and one declared
// output port.
func oneTask(ref string) graph.Task {
	return graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4", "fetch", 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4",
		Workflow:  "finance/monthly-invoicing@a3f9c1e",
		Namespace: "finance",
		Commit:    "a3f9c1e",
		Step:      "fetch",
		Attempt:   1,
		Image:     ref,
		Inputs: map[agk.Port]agk.Envelope{
			"in": {Items: []agk.Item{agk.NewItem(map[string]any{"url": "https://example.test"})}},
		},
		Outputs:    []agk.Port{"out"},
		Network:    graph.NetworkNone,
		Idempotent: true,
	}
}

// wrote lays one envelope down where a brick writes it, under /agk/out/ports/<port>.json,
// with the metadata a brick fills in for itself.
func wrote(c dockertest.Container, port agk.Port, items ...agk.Item) error {
	run := agk.RunID(c.Labels[LabelRun])
	step := agk.Step(c.Labels[LabelStep])
	e := agk.Empty(run, step, port, 1, time.Now().UTC())
	e.Items = items
	e.Meta.Count = len(items)

	f, err := os.Create(filepath.Join(c.Work, "ports", string(port)+".json"))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = e.Encode(f)
	return err
}

// oneImage is the map a fake daemon resolves one reference with.
func oneImage(ref, manifest string) map[string]dockertest.Image {
	return map[string]dockertest.Image{ref: {Digest: imageDigest, Manifest: []byte(manifest)}}
}

// The whole of one task: create, start, attach, wait, logs, remove, with the envelope on
// standard input and the port collected back off /agk/out.
func TestOneTaskRunsThroughTheDaemonEndToEnd(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	var onStdin string
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		b, err := io.ReadAll(c.Stdin)
		if err != nil {
			return 1, err
		}
		onStdin = string(b)
		fmt.Fprintln(c.Stderr, "fetching")

		return 0, wrote(c, "out", agk.NewItem(map[string]any{"status": 200}))
	})

	result, err := r.Run(t.Context(), oneTask(ref))
	if err != nil {
		t.Fatalf("running one task: %s", err)
	}

	if result.State != agk.TaskSucceeded {
		t.Errorf("the state is %s, and the container exited 0", result.State)
	}
	if result.ExitCode != 0 {
		t.Errorf("the exit code is %d", result.ExitCode)
	}
	if result.Task != agk.TaskID("01JMZ8V1P9C4/fetch/1") {
		t.Errorf("the result is for %s", result.Task)
	}
	if result.DispatchedAt.IsZero() {
		t.Error("nothing recorded the dispatch, and the dispatch is what fixes the deadline")
	}
	if result.StartedAt.IsZero() || result.FinishedAt.IsZero() {
		t.Errorf("the two moments are %s and %s, and they come from the daemon's own state", result.StartedAt, result.FinishedAt)
	}

	if !strings.Contains(onStdin, "https://example.test") {
		t.Errorf("standard input carried %q, and the envelope goes on it", onStdin)
	}
	out, ok := result.Outputs["out"]
	if !ok {
		t.Fatalf("the declared port was not published: %v", result.Outputs)
	}
	if len(out.Items) != 1 {
		t.Errorf("the port carries %d items", len(out.Items))
	}
	if out.Meta.Port != "out" || out.Meta.Step != "fetch" {
		t.Errorf("the metadata is %+v, and it is filled in by the runner", out.Meta)
	}
}

// "then destroy the container and the working directory", and the network with them.
func TestTheContainerTheNetworkAndTheWorkingDirectoryAreDestroyed(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	var work string
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		work = c.Work
		return 0, nil
	})

	task := oneTask(ref)
	task.Network = graph.NetworkInternal
	if _, err := r.Run(t.Context(), task); err != nil {
		t.Fatalf("running: %s", err)
	}

	if work == "" {
		t.Fatal("the container was never given a working directory")
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Errorf("%s survived the task, and a working directory is removed with the container", work)
	}

	// The container the task ran in, the reader container the manifest was read
	// through, and the task's own network.
	if len(r.daemon.Removed()) < 3 {
		t.Errorf("the daemon was asked to destroy %v, and a task leaves a container, a reader and a network behind it", r.daemon.Removed())
	}
}

// The transitions a heartbeat needs, which graph.Result has nowhere to carry.
func TestTheObserverIsToldTheTransitionsAResultCannotCarry(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := newRunner(t, oneImage(ref, goodManifest), func(dockertest.Container) (int, error) { return 0, nil })

	if _, err := r.Run(t.Context(), oneTask(ref)); err != nil {
		t.Fatalf("running: %s", err)
	}

	states := r.observed.states()
	for _, want := range []agk.TaskState{agk.TaskDispatched, agk.TaskRunning, agk.TaskPublishing, agk.TaskSucceeded} {
		found := false
		for _, got := range states {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the observer was told %v, and %s is one a heartbeat needs", states, want)
		}
	}
}

// "the runner refuses to start a container for a key that has already completed": a key
// delivered twice runs its brick once. By the time the second delivery arrives the first
// one's container has been collected and removed, which is the case adoption by label
// cannot reach and the record under the work root is for.
func TestTwoDeliveriesOfOneKeyRunTheBrickOnce(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	var mu sync.Mutex
	ran := 0
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		mu.Lock()
		ran++
		n := ran
		mu.Unlock()
		return 0, wrote(c, "out", agk.NewItem(map[string]any{"n": n}))
	})

	task := oneTask(ref)
	first, err := r.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("the first delivery: %s", err)
	}
	if first.State != agk.TaskSucceeded {
		t.Fatalf("the first delivery ended %s", first.State)
	}
	created := len(r.daemon.Created())

	_, err = r.Run(t.Context(), task)
	if !errors.Is(err, ErrCompleted) {
		t.Fatalf("the second delivery answered %v, and a key that has completed is refused", err)
	}
	if charge, decided := Charged(err); !decided || charge != ChargePlatform {
		t.Errorf("the refusal is charged to %s, and a key refused is not the brick's failure", charge)
	}
	if !strings.Contains(err.Error(), "ended succeeded") {
		t.Errorf("the refusal reads %q, and it says how the key ended", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if ran != 1 {
		t.Errorf("the brick ran %d times for two deliveries of one key", ran)
	}
	if n := len(r.daemon.Created()); n != created {
		t.Errorf("the refused delivery created %d containers", n-created)
	}
}

// A key is refused on the delivery after a runner that ended it died between writing the
// ending down and tidying: the ending is written before the container and the working
// directory are removed, so both can be left behind with the key recorded. The refusal is
// then the only delivery that will ever reach them, and it takes them away: "AutoRemove:
// false. The runner removes the container itself once logs and exit code are collected",
// and the working directory is "removed with the container, so no residue of one
// namespace survives into the next task on that host".
func TestWhatACompletedKeyLeftBehindIsTakenAwayByItsRefusal(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		return 0, wrote(c, "out", agk.NewItem(map[string]any{"n": 1}))
	})

	// The removal of the first delivery is recorded and refused, which is the container left
	// behind by a runner that died between the ending and the tidying.
	var mu sync.Mutex
	var removed []string
	r.daemon.Handle("DELETE", "/containers/{id}", func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		removed = append(removed, req.PathValue("id"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	asked := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(removed)
	}

	task := oneTask(ref)
	if _, err := r.Run(t.Context(), task); err != nil {
		t.Fatalf("the first delivery: %s", err)
	}
	first := asked()

	// The working directory the first delivery would have left behind had it died before its
	// own defers ran, which is the same process that left the container.
	dir := filepath.Join(r.work, "01JMZ8V1P9C4", "fetch", "1")
	if err := os.MkdirAll(filepath.Join(dir, "out", "ports"), 0o700); err != nil {
		t.Fatalf("putting the first delivery's working directory back: %s", err)
	}

	if _, err := r.Run(t.Context(), task); !errors.Is(err, ErrCompleted) {
		t.Fatalf("the second delivery answered %v, and a key that has completed is refused", err)
	}
	if asked() <= first {
		t.Error("the container a completed key left behind was never removed")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("%s is still there after the refusal, and a task's working directory is removed with its container", dir)
	}
}

// exitedFirstDelivery stages what a runner that died between the exit and the tidying
// leaves behind: the first delivery's container, run to its end and never removed, under
// the task's label, beside the working directory it was given.
func exitedFirstDelivery(t *testing.T, r *runner, task graph.Task) (container, root string) {
	t.Helper()
	container, root = stageFirstDelivery(t, r, task)
	waited, err := r.cli.ContainerWait(t.Context(), container, docker.WaitNextExit)
	if err != nil {
		t.Fatalf("opening the wait on the first delivery's container: %s", err)
	}
	if err := r.cli.ContainerStart(t.Context(), container); err != nil {
		t.Fatalf("starting the first delivery's container: %s", err)
	}
	if exit := <-waited; exit.Err != nil {
		t.Fatalf("waiting for the first delivery's container: %s", exit.Err)
	}
	return container, root
}

// An exited container is work already done. The delivery that adopts it collects its exit
// code and its ports as they stand and does not start it, because a daemon answers a start
// on an exited container by running the brick a second time.
func TestAnExitedContainerIsCollectedNotStartedAgain(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	var mu sync.Mutex
	ran := map[string]int{}
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		step := c.Labels[LabelStep]
		mu.Lock()
		ran[step]++
		mu.Unlock()
		if step == "check" {
			return 3, nil
		}
		return 0, wrote(c, "out", agk.NewItem(map[string]any{"from": "the first delivery"}))
	})

	fetch, check := oneTask(ref), oneTask(ref)
	check.ID, check.Step = agk.NewTaskID("01JMZ8V1P9C4", "check", 1, agk.Shard{}), "check"
	fetched, fetchedRoot := exitedFirstDelivery(t, r, fetch)
	checked, checkedRoot := exitedFirstDelivery(t, r, check)

	succeeded, err := r.Run(t.Context(), fetch)
	if err != nil {
		t.Fatalf("redelivering the task whose container exited 0: %s", err)
	}
	if succeeded.State != agk.TaskSucceeded || succeeded.ExitCode != 0 {
		t.Errorf("the redelivery reports %s with code %d, and the container exited 0", succeeded.State, succeeded.ExitCode)
	}
	if out := succeeded.Outputs["out"]; len(out.Items) != 1 || out.Items[0].Data["from"] != "the first delivery" {
		t.Errorf("the port carries %+v, and what the container left under /agk/out is collected", out)
	}

	failed, err := r.Run(t.Context(), check)
	if err != nil {
		t.Fatalf("redelivering the task whose container exited 3: %s", err)
	}
	if failed.State != agk.TaskFailed || failed.ExitCode != 3 {
		t.Errorf("the redelivery reports %s with code %d, and the container exited 3", failed.State, failed.ExitCode)
	}

	mu.Lock()
	if ran["fetch"] != 1 || ran["check"] != 1 {
		t.Errorf("the bricks ran %v times, and a container that has exited is collected rather than started again", ran)
	}
	mu.Unlock()

	removed := r.daemon.Removed()
	for _, left := range []struct{ container, root string }{{fetched, fetchedRoot}, {checked, checkedRoot}} {
		if !slices.Contains(removed, left.container) {
			t.Errorf("the collected container %s was never removed", left.container[:12])
		}
		if _, err := os.Stat(left.root); !os.IsNotExist(err) {
			t.Errorf("%s survived the delivery that collected its container", left.root)
		}
	}

	// The adoption writes each ending down as a delivery that created the container does.
	// The container is gone now, so the record is the only thing left to refuse the key,
	// and a restarted runner that adopts what it held when it died is the case it is for.
	for _, c := range []struct {
		task  graph.Task
		ended agk.TaskState
	}{{fetch, agk.TaskSucceeded}, {check, agk.TaskFailed}} {
		if e, found, err := r.keys.read(c.task.ID); err != nil || !found || e.State != c.ended {
			t.Errorf("the record of %s reads %+v, %v, %v, and its adopted container ended %s", c.task.ID, e, found, err, c.ended)
		}
		if _, err := r.Run(t.Context(), c.task); !errors.Is(err, ErrCompleted) {
			t.Errorf("%s was delivered again after its adopted container was collected and answered %v", c.task.ID, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if ran["fetch"] != 1 || ran["check"] != 1 {
		t.Errorf("the bricks ran %v times after a further delivery of each", ran)
	}
}

// A container its deadline stopped is timed_out, whichever way the redelivery finds it. The
// delivery that was watching it stopped it and died before it reported: a redelivery that
// reaches the container while the stop is still under way stops it itself and reports
// timed_out, and one that reaches it a moment later reads the same thing off the daemon
// rather than a failure for the code the stop left.
func TestAContainerStoppedAtItsDeadlineIsTimedOutHoweverTheRedeliveryFindsIt(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		// The brick takes its term and goes, which is what a stop at the deadline
		// asks of it.
		<-c.Signalled()
		return 143, nil
	})

	for _, exited := range []bool{false, true} {
		task := oneTask(ref)
		step := map[bool]agk.Step{false: "still-running", true: "exited"}[exited]
		task.ID, task.Step = agk.NewTaskID("01JMZ8V1P9C4", step, 1, agk.Shard{}), step

		container, _ := stageFirstDelivery(t, r, task)
		if err := r.cli.ContainerStart(t.Context(), container); err != nil {
			t.Fatalf("starting the first delivery's container: %s", err)
		}
		task.Deadline = time.Now()
		if exited {
			// The stop the first delivery's watch sent at the deadline, carried
			// through before the redelivery arrives.
			if err := r.cli.ContainerStop(t.Context(), container, time.Second); err != nil {
				t.Fatalf("stopping the first delivery's container: %s", err)
			}
		}

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		result, err := r.Run(ctx, task)
		cancel()
		if err != nil {
			t.Fatalf("the redelivery that found the container %s: %s", step, err)
		}
		if result.State != agk.TaskTimedOut {
			t.Errorf("the redelivery that found the container %s reports %s with code %d, and the container was stopped at its deadline", step, result.State, result.ExitCode)
		}
	}
}

// A container that ended before its deadline is read by its own code when it is collected,
// however little time was left: the deadline is a stop that did not have to happen.
func TestAContainerThatEndedInsideItsDeadlineIsReadByItsCode(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	r := newRunner(t, oneImage(ref, goodManifest), func(dockertest.Container) (int, error) { return 3, nil })

	task := oneTask(ref)
	exitedFirstDelivery(t, r, task)
	task.Deadline = time.Now().Add(50 * time.Millisecond)
	time.Sleep(100 * time.Millisecond)

	result, err := r.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("the redelivery: %s", err)
	}
	if result.State != agk.TaskFailed || result.ExitCode != 3 {
		t.Errorf("the redelivery reports %s with code %d, and the container exited 3 before its deadline", result.State, result.ExitCode)
	}
}

// What a container that had already exited wrote is read back off the daemon's log, into
// the task's log and into the standard output a script step publishes, and it is masked on
// the way as a watched container's output is. The values are the redelivery's own, redeemed
// again, and a secret the container printed is masked whichever delivery it printed under.
func TestWhatAnExitedContainerWroteIsReadBackMasked(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	var written strings.Builder
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		fmt.Fprintln(c.Stdout, "the token is s3cr3t-value")
		fmt.Fprintln(c.Stderr, "authorising with s3cr3t-value")
		return 0, nil
	})
	r.cfg.Logs = &sinkFor{b: &written}

	// A script step that writes no port file publishes its standard output on out.
	task := taskWithASecret(ref)
	task.Script = []string{`echo "the token is $(cat /agk/secrets/bearer)"`}
	exitedFirstDelivery(t, r, task)

	result, err := r.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("the redelivery: %s", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the redelivery reports %s with code %d, and the container exited 0", result.State, result.ExitCode)
	}
	out := result.Outputs["out"]
	if len(out.Items) != 1 {
		t.Fatalf("out carries %+v, and a script step that wrote no port file publishes its standard output there", out)
	}
	stdout, _ := out.Items[0].Data[StdoutField].(string)
	if strings.Contains(stdout, "s3cr3t-value") {
		t.Errorf("the published standard output carries the secret value: %q", stdout)
	}
	if !strings.Contains(stdout, "the token is "+maskToken) {
		t.Errorf("the published standard output is %q, and it is what the container wrote, read back off the daemon's log", stdout)
	}

	log := written.String()
	if strings.Contains(log, "s3cr3t-value") {
		t.Errorf("the log carries the secret value: %q", log)
	}
	for _, want := range []string{"the token is " + maskToken, "authorising with " + maskToken} {
		if !strings.Contains(log, want) {
			t.Errorf("the log does not carry %q, and it is what the container wrote, read back off the daemon's log: %q", want, log)
		}
	}
}

// A container that is still running is waited on, and no start is sent to it at all. A
// daemon answers a start on a running container 304 and does nothing, but one that reached
// it a moment after it exited would run the brick a second time.
func TestARunningContainerIsWaitedOnNotStartedAgain(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	release := make(chan struct{})
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		<-release
		return 0, wrote(c, "out", agk.NewItem(map[string]any{"from": "the first delivery"}))
	})

	task := oneTask(ref)
	container, _ := stageFirstDelivery(t, r, task)
	if err := r.cli.ContainerStart(t.Context(), container); err != nil {
		t.Fatalf("starting the first delivery's container: %s", err)
	}

	// Every start from here on is counted and answered as a daemon answers one on a
	// running container.
	var mu sync.Mutex
	starts := 0
	r.daemon.Handle("POST", "/containers/{id}/start", func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		starts++
		mu.Unlock()
		w.WriteHeader(http.StatusNotModified)
	})

	type outcome struct {
		result graph.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := r.Run(context.Background(), task)
		done <- outcome{result, err}
	}()

	// The container is let go once the redelivery is watching it, which is after the
	// inspect found it running and before anything a start would follow.
	deadline := time.Now().Add(10 * time.Second)
	for h := r.lookup(task.ID); h == nil || h.watching() == nil; h = r.lookup(task.ID) {
		if time.Now().After(deadline) {
			t.Fatal("the redelivery never joined the running container")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("the redelivery: %s", got.err)
		}
		if out := got.result.Outputs["out"]; got.result.State != agk.TaskSucceeded || len(out.Items) != 1 {
			t.Errorf("the redelivery reports %s with %+v", got.result.State, out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the redelivery never came back")
	}
	mu.Lock()
	defer mu.Unlock()
	if starts != 0 {
		t.Errorf("the adopted container was started %d times while it was running", starts)
	}
}

// A container the inspect found running can exit before the wait on it is opened. The wait
// is opened with condition=not-running, which answers with the exit that happened; a wait
// for the next exit would wait for one that only a start brings, and the redelivery would
// hang, or be stopped at its deadline and reported timed_out for work that had finished.
func TestAContainerThatExitsBetweenTheInspectAndTheWaitIsCollected(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	var mu sync.Mutex
	ran := 0
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		mu.Lock()
		ran++
		mu.Unlock()
		return 0, wrote(c, "out", agk.NewItem(map[string]any{"from": "the first delivery"}))
	})

	task := oneTask(ref)
	container, _ := exitedFirstDelivery(t, r, task)

	// The inspect the redelivery reads first finds the container running, as it was a
	// moment before it exited, and every inspect after it reads the daemon as it is.
	now, err := r.cli.ContainerInspect(t.Context(), container)
	if err != nil {
		t.Fatalf("inspecting the first delivery's container: %s", err)
	}
	before := now
	before.State.Status, before.State.Running = "running", true
	before.State.ExitCode, before.State.FinishedAt = 0, time.Time{}
	inspected := 0
	r.daemon.Handle("GET", "/containers/{id}/json", func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasPrefix(req.URL.Path, "/containers/"+container) {
			http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
			return
		}
		mu.Lock()
		inspected++
		answer := now
		if inspected == 1 {
			answer = before
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(answer)
	})

	// Bounded, because a wait for an exit that has already happened never answers.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := r.Run(ctx, task)
	if err != nil {
		t.Fatalf("the redelivery: %s", err)
	}
	if out := result.Outputs["out"]; result.State != agk.TaskSucceeded || result.ExitCode != 0 || len(out.Items) != 1 || out.Items[0].Data["from"] != "the first delivery" {
		t.Errorf("the redelivery reports %s with code %d and %+v, and the container exited 0 having written one item", result.State, result.ExitCode, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if ran != 1 {
		t.Errorf("the brick ran %d times, and the container was over before the redelivery reached it", ran)
	}
}

// A container that was created and never started is the one an adoption starts: the
// delivery that created it died before the start, so nothing has run in it and nothing was
// written on its standard input. It runs once, is given its envelope there as that delivery
// would have given it, and is collected and removed like any other.
func TestACreatedContainerIsStartedWhenAdopted(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	var mu sync.Mutex
	ran, onStdin := 0, ""
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		b, err := io.ReadAll(c.Stdin)
		mu.Lock()
		ran++
		onStdin = string(b)
		mu.Unlock()
		if err != nil {
			return 1, err
		}
		return 0, wrote(c, "out", agk.NewItem(map[string]any{"from": "the adopted container"}))
	})

	task := oneTask(ref)
	container, root := stageFirstDelivery(t, r, task)

	// Bounded, because a brick reading a standard input that nobody writes to or
	// closes waits on it for as long as the task is given.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, err := r.Run(ctx, task)
	if err != nil {
		t.Fatalf("the redelivered task: %s", err)
	}
	if out := result.Outputs["out"]; result.State != agk.TaskSucceeded || len(out.Items) != 1 || out.Items[0].Data["from"] != "the adopted container" {
		t.Errorf("the redelivery reports %s with %+v, and the container it started wrote one item", result.State, out)
	}

	mu.Lock()
	if ran != 1 {
		t.Errorf("the brick ran %d times, and a container that never started is started once", ran)
	}
	if !strings.Contains(onStdin, "https://example.test") {
		t.Errorf("standard input carried %q, and the envelope goes on it", onStdin)
	}
	mu.Unlock()

	if !slices.Contains(r.daemon.Removed(), container) {
		t.Errorf("the adopted container %s was never removed", container[:12])
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("%s survived the delivery that adopted its container", root)
	}
}

// A task delivered again while its first delivery is still in hand is refused, before it
// looks for anything or creates anything: two Runs carrying one container would each
// collect it and each remove it.
func TestAKeyInFlightIsNotRunTwice(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	running := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		once.Do(func() { close(running) })
		<-release
		return 0, nil
	})

	task := oneTask(ref)
	first := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), task)
		first <- err
	}()
	select {
	case <-running:
	case <-time.After(10 * time.Second):
		t.Fatal("the first delivery's container never ran")
	}
	created := len(r.daemon.Created())

	// Bounded, because a second delivery that was not refused joins the first one's
	// container and waits on it for as long as it runs.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := r.Run(ctx, task)
	if !errors.Is(err, ErrTaskInFlight) {
		t.Fatalf("the second delivery answered %v, and a task in flight on this runner is refused", err)
	}
	if n := len(r.daemon.Created()); n != created {
		t.Errorf("the refused delivery created %d containers", n-created)
	}
	if r.lookup(task.ID) == nil {
		t.Error("the refusal let go of the first delivery's hold, which is what a stop reaches the task through")
	}

	close(release)
	select {
	case err := <-first:
		if err != nil {
			t.Errorf("the first delivery: %s", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first delivery never came back")
	}
}

// "the runner removes the container itself once logs and exit code are collected, so that
// nothing is lost on a fast exit".
func TestNothingIsLostOnAFastExit(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	var written strings.Builder
	sink := &sinkFor{b: &written}
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		fmt.Fprintln(c.Stderr, "what it said before anybody was listening")
		return 0, nil
	}, dockertest.ExitsDuringAttach)
	r.cfg.Logs = sink

	if _, err := r.Run(t.Context(), oneTask(ref)); err != nil {
		t.Fatalf("running: %s", err)
	}
	if !strings.Contains(written.String(), "what it said before anybody was listening") {
		t.Errorf("the log reads %q, and the daemon still had what the container wrote", written.String())
	}
}

// "Mask secret values in the collected log by literal match against the values known to
// the task, before anything is written."
func TestASecretPrintedByAContainerNeverReachesTheLog(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	var written strings.Builder
	sink := &sinkFor{b: &written}
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		fmt.Fprintln(c.Stderr, "authorising with s3cr3t-value")
		return 0, nil
	})
	r.cfg.Logs = sink

	task := oneTask(ref)
	task.Secrets = []graph.SecretMount{{Name: "bearer", Mount: "/agk/secrets/bearer"}}

	if _, err := r.Run(t.Context(), task); err != nil {
		t.Fatalf("running: %s", err)
	}
	if strings.Contains(written.String(), "s3cr3t-value") {
		t.Errorf("the log carries the secret value: %q", written.String())
	}
	if !strings.Contains(written.String(), "authorising with") {
		t.Errorf("the log reads %q, and the line itself is not the secret", written.String())
	}
}

// "network: egress returns before anything is created", because a workflow must not be
// able to believe its allow list is being enforced when nothing is enforcing it.
func TestAnEgressStepIsRefusedBeforeAnythingIsCreated(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := newRunner(t, oneImage(ref, goodManifest), func(dockertest.Container) (int, error) { return 0, nil })

	task := oneTask(ref)
	task.Network = graph.NetworkEgress
	task.EgressAllow = []string{"api.example.test:443"}

	_, err := r.Run(t.Context(), task)
	if err == nil {
		t.Fatal("a step asking for egress was started")
	}
	if len(r.daemon.Created()) != 0 {
		t.Errorf("%d containers were created for a step that was refused", len(r.daemon.Created()))
	}
}

// A stop reaches a container by its label, and the Run that is blocked is the one that
// reports.
func TestAStopIsWhatTheBlockedRunReports(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	running := make(chan struct{})
	var once sync.Once
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		once.Do(func() { close(running) })
		<-c.Signalled()
		return 143, nil
	})

	task := oneTask(ref)
	done := make(chan graph.Result, 1)
	go func() {
		result, err := r.Run(context.Background(), task)
		if err != nil {
			t.Errorf("running: %s", err)
		}
		done <- result
	}()

	<-running
	// The container is resolved through the registry here, which is the fast path,
	// and through the label when this process is not holding it.
	if err := r.Stop(t.Context(), graph.Stop{Task: task.ID, Reason: graph.StopCancelled}); err != nil {
		t.Fatalf("stopping: %s", err)
	}

	select {
	case result := <-done:
		if result.State != agk.TaskCancelled {
			t.Errorf("the state is %s, and a stop that landed is cancelled", result.State)
		}
		if result.ExitCode != 0 {
			t.Errorf("the exit code is %d, and it is set for succeeded and failed and for no other state", result.ExitCode)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run never came back after the stop")
	}
}

// "Stopping a task this driver does not hold is not an error": at-least-once delivery
// means a stop can arrive for a task that already finished.
func TestStoppingATaskNobodyHoldsIsNotAnError(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := newRunner(t, oneImage(ref, goodManifest), func(dockertest.Container) (int, error) { return 0, nil })

	err := r.Stop(t.Context(), graph.Stop{Task: "01JMZ8V1P9C4/gone/1", Reason: graph.StopSuperseded})
	if err != nil {
		t.Errorf("stopping a task that already finished: %s", err)
	}
}

// A runner holds a key, redeems its grant and acknowledges its message before it calls Run, and
// from the redemption on a cancel names the task and the controller sends its one stop. A stop
// that lands in between, while the runner is still acknowledging, is kept: Run, when it comes,
// starts no container for the task, the brick never runs, and the task is reported cancelled.
func TestAStopBetweenTheHoldAndTheRunStartsNothing(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	bricks := &counting{}
	r := newRunner(t, oneImage(ref, goodManifest), bricks.run(func(string) int { return 0 }))
	task := oneTask(ref)

	if err := r.Hold(task.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(t.Context(), graph.Stop{Task: task.ID, Reason: graph.StopCancelled}); err != nil {
		t.Fatalf("stopping a task that was held and not yet run: %s", err)
	}
	result, err := r.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running a task stopped while it was held: %s", err)
	}
	if n := bricks.times("fetch"); n != 0 {
		t.Errorf("the brick ran %d times for a task stopped before Run began", n)
	}
	if result.State != agk.TaskCancelled {
		t.Errorf("the state is %s, and a stop that landed is cancelled", result.State)
	}
	if r.lookup(task.ID) != nil {
		t.Error("the task is still held once Run has returned")
	}
}

// A runner that holds a key and does not go on to run it, its redemption refused or the message
// put back, lets go of it, and a stop for the key afterwards is one for a task this driver does
// not hold, while the next delivery of the key may hold it again. A key a Run has taken is let go
// of by that Run alone, since the stop that reaches the task goes through it.
func TestAKeyIsLetGoOfOnlyByWhatHeldIt(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	running := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	r := newRunner(t, oneImage(ref, goodManifest), func(dockertest.Container) (int, error) {
		once.Do(func() { close(running) })
		<-release
		return 0, nil
	})
	task := oneTask(ref)

	if err := r.Hold(task.ID); err != nil {
		t.Fatal(err)
	}
	r.Release(task.ID)
	if r.lookup(task.ID) != nil {
		t.Fatal("the delivery holding the key let go and the key is still held")
	}

	if err := r.Hold(task.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), task)
		done <- err
	}()
	select {
	case <-running:
	case <-time.After(10 * time.Second):
		t.Fatal("the held task's container never ran")
	}
	r.Release(task.ID)
	if r.lookup(task.ID) == nil {
		t.Error("a release let go of a task a Run had taken, which is what a stop reaches it through")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("running: %s", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run never came back")
	}
	if r.lookup(task.ID) != nil {
		t.Error("the task is still held once Run has returned")
	}
}

// The version is negotiated with the daemon and not compiled in.
func TestTheDriverSpeaksTheVersionTheDaemonOffers(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := newRunner(t, oneImage(ref, goodManifest),
		func(dockertest.Container) (int, error) { return 0, nil },
		dockertest.APIVersion("1.44"))

	if got := r.APIVersion(); got != "1.44" {
		t.Errorf("the driver speaks %s to a daemon offering 1.44", got)
	}
}

// sinkFor is a log sink over a builder, which is what a runner that keeps logs somewhere
// stands in as here.
type sinkFor struct{ b *strings.Builder }

func (s *sinkFor) OpenLog(context.Context, agk.TaskID) (io.WriteCloser, error) {
	return nopCloser{s.b}, nil
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// unusedDocker keeps the docker import honest where the assertions above do not reach for
// a wire type directly.
var _ = docker.Ceiling
