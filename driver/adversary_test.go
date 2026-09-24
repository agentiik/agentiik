package driver

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// The rules in this file are the ones an adversary would go looking for: a secret that
// reaches a log or a disk somebody else can read, a container left running after a
// failure, a working directory that survives, a stop that does not land, a mount that is
// writable where the figure says (ro), egress opened and called filtered.
//
// Each one is a rule the documentation states, held against what the driver actually
// does rather than against what it means to do.

// stageFirstDelivery builds the state a runner that died mid task leaves behind: a
// container carrying the task's label, and the working directory it was given, with the
// secret value written into it.
//
// It walks the same sequence Run does, which is the point: what a second delivery adopts
// has to be what the first one actually created.
func stageFirstDelivery(t *testing.T, r *runner, task graph.Task) (container, root string) {
	t.Helper()
	ctx := t.Context()

	store, err := r.store(ctx, task)
	if err != nil {
		t.Fatalf("opening the store: %s", err)
	}
	w, err := newWorkdir(r.cfg.WorkRoot, task.ID, r.cfg.Policy.SecretsDir)
	if err != nil {
		t.Fatalf("preparing the working directory: %s", err)
	}
	run, err := r.runOf(ctx, task)
	if err != nil {
		t.Fatalf("reading the run: %s", err)
	}
	repo, err := r.repo(ctx, task)
	if err != nil {
		t.Fatalf("preparing the repository: %s", err)
	}
	given, err := prepare(ctx, task, w, r.cfg.Policy, r.cfg.host(), store, run, repo, r.secrets(ctx))
	if err != nil {
		t.Fatalf("preparing what the container is given: %s", err)
	}
	entrypoint, cmd := scriptCommand(task, r.cfg.Policy.Shell)
	config := containerConfig(task, task.Image, "65532:65532", environment(task, deadlineOf(task, time.Now())), entrypoint, cmd)
	host, err := hostConfig(task, r.cfg.Policy, given, networkModeNone)
	if err != nil {
		t.Fatalf("composing the host configuration: %s", err)
	}
	created, err := r.cli.ContainerCreate(ctx, "", config, host, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("creating the container: %s", err)
	}
	return created.ID, w.Root
}

// taskWithASecret is one task that is given one secret, so that the working directory it
// leaves behind has a value in it.
func taskWithASecret(ref string) graph.Task {
	task := oneTask(ref)
	task.Secrets = []graph.SecretMount{{Name: "bearer", Mount: "/agk/secrets/bearer"}}
	return task
}

// A redelivered task inherits the first delivery's working directory, and that directory
// holds a secret value redeemed for the attempt before this one. "No residue of one
// namespace survives into the next task on that host", and a value left on the disk of a
// runner is exactly that residue.
func TestAnAdoptedTaskTakesTheFirstDeliverysSecretValueAway(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		return 0, wrote(c, "out", agk.NewItem(map[string]any{"n": 1}))
	})

	task := taskWithASecret(ref)
	container, root := stageFirstDelivery(t, r, task)

	secret := filepath.Join(root, "secrets", "bearer")
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("the first delivery wrote no secret at %s: %s", secret, err)
	}

	result, err := r.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("the redelivered task: %s", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the redelivered task ended %s", result.State)
	}

	// It has to be the adoption path and not a second container, or the rest of this
	// is about something else.
	tasked := 0
	for _, c := range r.daemon.Created() {
		if c.Labels[LabelTask] == string(task.ID) {
			tasked++
		}
	}
	if tasked != 1 {
		t.Fatalf("%d containers carry this task's label, and a redelivered task adopts the one it already started", tasked)
	}

	removed := false
	for _, id := range r.daemon.Removed() {
		if id == container {
			removed = true
		}
	}
	if !removed {
		t.Errorf("the adopted container %s was never removed: the daemon was asked to destroy %v", container[:12], r.daemon.Removed())
	}
	if _, err := os.Stat(secret); !os.IsNotExist(err) {
		t.Errorf("the secret value at %s survived the task", secret)
	}
}

// A stop that arrives while the task is still being prepared has to land.
//
// Three rules of the language call off work that is already running, fail_fast among
// them, and a task whose image is still being pulled is work that is already running: the
// evaluator has handed it out and is waiting for a Result. A stop that is answered nil
// and changes nothing leaves the container to be created afterwards and to run to its
// deadline, which is the shard fail_fast existed to stop.
func TestAStopThatArrivesBeforeTheContainerExistsStillLands(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	preparing := make(chan struct{})
	stopped := make(chan struct{})
	var once sync.Once

	var mu sync.Mutex
	ran := false

	r := newRunner(t, oneImage(ref, goodManifest), func(dockertest.Container) (int, error) {
		mu.Lock()
		ran = true
		mu.Unlock()
		return 0, nil
	})
	// The adoption lookup is held open, which is the moment after the image has been
	// resolved and before the container exists. Only the first one waits, so that the
	// stop itself is not held behind the run it is stopping.
	first := true
	r.daemon.Handle("GET", "/containers/json", func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		hold := first
		first = false
		mu.Unlock()
		if hold {
			once.Do(func() { close(preparing) })
			<-stopped
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	})

	task := oneTask(ref)
	result := make(chan graph.Result, 1)
	go func() {
		got, _ := r.Run(context.Background(), task)
		result <- got
	}()

	<-preparing
	if err := r.Stop(t.Context(), graph.Stop{Task: task.ID, Reason: graph.StopSiblingFailed}); err != nil {
		t.Fatalf("stopping: %s", err)
	}
	close(stopped)

	select {
	case got := <-result:
		mu.Lock()
		defer mu.Unlock()
		if ran {
			t.Errorf("the brick ran after the task was stopped, and the run came back %s", got.State)
		}
		if got.State != agk.TaskCancelled {
			t.Errorf("a stopped task came back %s", got.State)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the stopped task never came back")
	}
}

// A task whose caller gave up leaves nothing behind: "a cancelled task still has a
// container, a network and a directory to take away".
func TestATaskWhoseCallerGaveUpLeavesNothingRunning(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	var work string
	running := make(chan struct{})
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		work = c.Work
		close(running)
		<-c.Signalled()
		return 0, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		task := oneTask(ref)
		task.Network = graph.NetworkInternal
		r.Run(ctx, task)
	}()
	<-running
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the cancelled task never came back")
	}

	if work != "" {
		if _, err := os.Stat(work); !os.IsNotExist(err) {
			t.Errorf("%s survived a cancelled task", work)
		}
	}
	if removed := r.daemon.Removed(); len(removed) < 2 {
		t.Errorf("a cancelled task left %v on the daemon, and its container and its network both go with it", removed)
	}
}

// The kernel's out-of-memory killer is the exit no wait reports. 137 is in the band the
// table reads as an infrastructure failure, "charged to the runner and not to the brick",
// and the log has to say whose failure it was.
func TestAnOutOfMemoryKillIsReadOffTheTableEndToEnd(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	var written strings.Builder
	r := newRunner(t, oneImage(ref, goodManifest),
		func(dockertest.Container) (int, error) { return 0, nil },
		dockertest.OOMKills)
	r.cfg.Logs = &sinkFor{b: &written}

	result, err := r.Run(t.Context(), oneTask(ref))
	if err != nil {
		t.Fatalf("running: %s", err)
	}
	if result.State != agk.TaskFailed || result.ExitCode != 137 {
		t.Errorf("an out-of-memory kill came back %s with exit %d", result.State, result.ExitCode)
	}
	if !strings.Contains(written.String(), "charged to the runner and not to the brick") {
		t.Errorf("the log does not say whose failure 137 was: %s", written.String())
	}
}

// AGK_DEADLINE is "an RFC 3339 timestamp past which the container will be stopped". A step
// that carries a timeout is stopped at one, so it is told about one.
func TestAStepWithATimeoutIsToldWhenItWillBeStopped(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	r := newRunner(t, oneImage(ref, goodManifest), func(dockertest.Container) (int, error) { return 0, nil })

	task := oneTask(ref)
	task.Timeout = graph.Duration(30 * time.Second)
	before := time.Now()
	if _, err := r.Run(t.Context(), task); err != nil {
		t.Fatalf("running: %s", err)
	}

	created := createdFor(r, task.ID)
	value, ok := created.Env(EnvDeadline)
	if !ok {
		t.Fatalf("the container was told %v, and the step is stopped 30s after its dispatch", created.Config.Env)
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("%s reads %q, and the table says an RFC 3339 timestamp: %s", EnvDeadline, value, err)
	}
	if at.Before(before.Add(29*time.Second)) || at.After(before.Add(40*time.Second)) {
		t.Errorf("%s reads %s, and the container is stopped 30s after a dispatch at %s", EnvDeadline, at, before)
	}
}

// Every path the figure marks (ro) is bound read-only, and the one writable bind is the
// one the outputs are collected from. A mount that is writable where the figure says
// otherwise is an input a retry reads differently from the attempt before it.
func TestOnlyTheOutputTreeIsBoundWritable(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	r := newRunner(t, oneImage(ref, goodManifest), func(dockertest.Container) (int, error) { return 0, nil })

	task := taskWithASecret(ref)
	task.Files = []graph.FileSelector{{From: "certs/ca.pem", To: "/etc/ssl/certs/internal-ca.pem"}}
	if _, err := r.Run(t.Context(), task); err != nil {
		t.Fatalf("running: %s", err)
	}

	created := createdFor(r, task.ID)
	writable := []string{}
	for _, m := range created.HostConfig.Mounts {
		if !m.ReadOnly {
			writable = append(writable, m.Target)
		}
	}
	if len(writable) != 1 || writable[0] != "/agk/out" {
		t.Errorf("the writable binds are %v, and the only writable paths are /agk/out and /tmp", writable)
	}
	for _, target := range []string{"/agk/in/in", RepoDir, RunPath, ParamsPath, "/agk/secrets/bearer", "/etc/ssl/certs/internal-ca.pem"} {
		m, ok := created.Mount(target)
		if !ok {
			t.Errorf("nothing is bound at %s", target)
			continue
		}
		if !m.ReadOnly {
			t.Errorf("%s is bound writable", target)
		}
	}
}

// Policy.SecretsDir is /dev/shm on Linux, and /dev/shm is mode 1777: what the runner
// creates under it is not a path it owns until it has checked, because anything on the
// machine can get there first. A link left under that name would be followed and a secret
// value written through it, mode 0444, with the parent chain that was its whole protection
// belonging to somebody else.
func TestASecretIsNotWrittenThroughAPathSomebodyElseGotToFirst(t *testing.T) {
	shared := t.TempDir()
	if err := os.Chmod(shared, 0o1777); err != nil {
		t.Fatalf("making the shared directory world writable: %s", err)
	}
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(shared, secretsBase)); err != nil {
		t.Fatalf("planting the link: %s", err)
	}

	id := agk.NewTaskID("01JMZ8V1P9C4", "fetch", 1, agk.Shard{})
	w, err := newWorkdir(t.TempDir(), id, shared)
	if err != nil {
		if !strings.Contains(err.Error(), secretsBase) {
			t.Fatalf("the refusal does not name what it refused: %s", err)
		}
		return
	}
	if err := writeSecret(filepath.Join(w.Secrets, "bearer"), []byte("s3cr3t-value")); err != nil {
		t.Fatalf("writing the value: %s", err)
	}

	var landed []string
	filepath.Walk(elsewhere, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			landed = append(landed, path)
		}
		return nil
	})
	if len(landed) > 0 {
		t.Errorf("the secret value was written through a path somebody else controls: %v", landed)
	}
}

// The same question without a link. A directory already there, group and world writable,
// is not a directory a secret value may be written under: the value itself is readable by
// design, and the mode of its parents is the whole of what protects it.
func TestASecretIsNotWrittenUnderADirectoryAnybodyCanWriteTo(t *testing.T) {
	shared := t.TempDir()
	if err := os.Chmod(shared, 0o1777); err != nil {
		t.Fatalf("making the shared directory world writable: %s", err)
	}
	base := filepath.Join(shared, secretsBase)
	if err := os.Mkdir(base, 0o777); err != nil {
		t.Fatalf("planting the directory: %s", err)
	}
	if err := os.Chmod(base, 0o777); err != nil {
		t.Fatalf("planting the mode: %s", err)
	}

	id := agk.NewTaskID("01JMZ8V1P9C4", "fetch", 1, agk.Shard{})
	w, err := newWorkdir(t.TempDir(), id, shared)
	if err != nil {
		return
	}
	if err := writeSecret(filepath.Join(w.Secrets, "bearer"), []byte("s3cr3t-value")); err != nil {
		t.Fatalf("writing the value: %s", err)
	}

	for dir := w.Secrets; strings.HasPrefix(dir, base); dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("reading %s: %s", dir, err)
		}
		if info.Mode().Perm()&0o022 != 0 {
			t.Errorf("%s is mode %o, and a secret value under it is readable by every account on the host", dir, info.Mode().Perm())
		}
	}
}

// createdFor is the container one task was created with, read back off the daemon.
func createdFor(r *runner, id agk.TaskID) dockertest.Container {
	var created dockertest.Container
	for _, c := range r.daemon.Created() {
		if c.Labels[LabelTask] == string(id) {
			created = c
		}
	}
	return created
}

// "Removed with the container, so no residue of one namespace survives into the next task
// on that host." What a brick left that the runner cannot remove is said, naming the step,
// the task and where the removal stopped, and it changes nothing about the task's ending:
// the brick ran and succeeded whatever is left of its directory.
func TestAWorkingDirectoryThatCannotBeRemovedIsSaid(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, where every directory can be removed and there is nothing to say")
	}
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	var locked string
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		// A directory the brick made and left unwritable, as a brick running as an
		// account of the remapped range leaves one for a runner that cannot override
		// its mode.
		locked = filepath.Join(c.Work, "scratch")
		if err := os.MkdirAll(locked, 0o755); err != nil {
			return 1, err
		}
		if err := os.WriteFile(filepath.Join(locked, "left.txt"), []byte("residue"), 0o644); err != nil {
			return 1, err
		}
		if err := os.Chmod(locked, 0o500); err != nil {
			return 1, err
		}
		return 0, wrote(c, "out", agk.NewItem(map[string]any{"n": 1}))
	})
	t.Cleanup(func() {
		if locked != "" {
			os.Chmod(locked, 0o755)
		}
	})

	task := oneTask(ref)
	result, err := r.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("Run: %s", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the task ended %s, and a directory left behind changes nothing about how it ended", result.State)
	}
	said := r.said.count("left files on this host that were not removed with its container")
	if said != 1 {
		t.Fatalf("the directory left behind was said %d times: %v", said, r.said.s)
	}
	for _, want := range []string{string(task.Step), string(task.ID), locked} {
		if r.said.count(want) == 0 {
			t.Errorf("what was said does not name %s: %v", want, r.said.s)
		}
	}
}
