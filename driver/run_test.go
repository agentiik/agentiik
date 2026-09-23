package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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

// "A driver that has already done the work recognises it": a redelivered task re-attaches
// to the container it already started instead of starting a second one.
func TestARedeliveredTaskAdoptsTheContainerItAlreadyStarted(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	ran := 0
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		ran++
		return 0, wrote(c, "out", agk.NewItem(map[string]any{"n": ran}))
	})

	task := oneTask(ref)
	first, err := r.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("the first delivery: %s", err)
	}
	if first.State != agk.TaskSucceeded {
		t.Fatalf("the first delivery ended %s", first.State)
	}

	// The container of the first delivery was removed on the way out, so the second
	// creates its own. What this holds is that the lookup happens at all and that
	// nothing is created twice while one is there: a container left behind by a
	// process that died is put back under the task it belongs to.
	tasked := 0
	for _, c := range r.daemon.Created() {
		if c.Labels[LabelTask] == string(task.ID) {
			tasked++
		}
	}
	if tasked != 1 {
		t.Errorf("%d containers were created under the task label for one delivery", tasked)
	}

	second, err := r.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("the second delivery: %s", err)
	}
	if second.State != agk.TaskSucceeded {
		t.Errorf("the second delivery ended %s", second.State)
	}
}

// "AutoRemove: false. The runner removes the container itself once logs and exit code are
// collected", and the working directory is "removed with the container, so no residue of
// one namespace survives into the next task on that host". Both are the runner's whoever
// started the container: a delivery that adopts one is the delivery that tidies it, since
// the one that started it is the process that died.
func TestAnAdoptedContainerIsRemovedWithItsWorkingDirectory(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		return 0, wrote(c, "out", agk.NewItem(map[string]any{"n": 1}))
	})

	// The removal of the first delivery is recorded and refused, which is the
	// container left behind by a runner that died between the exit and the tidying.
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

	// The working directory the first delivery would have left behind had it died
	// before its own defers ran, which is the same process that left the container.
	dir := filepath.Join(r.work, "01JMZ8V1P9C4", "fetch", "1")
	if err := os.MkdirAll(filepath.Join(dir, "out", "ports"), 0o700); err != nil {
		t.Fatalf("putting the first delivery's working directory back: %s", err)
	}

	if _, err := r.Run(t.Context(), task); err != nil {
		t.Fatalf("the second delivery: %s", err)
	}
	if asked() <= first {
		t.Error("the adopted container was never removed, and the runner removes the container itself once the logs and the exit code are collected")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("%s is still there after the delivery that adopted the container, and a task's working directory is removed with it", dir)
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
