package driver

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// This file is the one that needs a daemon that is actually there. It is skipped where
// there is none, so that a machine with nothing installed still runs the rest, and it runs
// where there is one. CI has one, pulls the image and sets AGENTIIK_TEST_REQUIRE_DOCKER, so
// there a test that cannot run fails rather than skips.
//
// What it holds that the fake cannot is that the sequence works against the daemon rather
// than against this project's reading of it: the bind mounts land, the account the image
// declares can write into /agk/out, the hijacked attach carries standard input, and the
// exit code comes back through a wait that was opened before the start.

// realDriver opens a driver on the daemon of this machine, or ends the test through
// dockertest.Unavailable.
//
// alpine:3.21 comes first because it is the image every other real test and fixture
// names, and the one CI pulls: a tag that moves, as latest does, would change what these
// tests run on under a branch nobody touched.
func realDriver(t *testing.T, images ...string) (*Docker, string) {
	t.Helper()
	policy := DefaultPolicy()
	// Docker Desktop does not offer user namespace remapping, and this is the machine
	// the floor is lifted for.
	policy.RequireUsernsRemap = RemapLifted
	policy.RequireSecretsTmpfs = SecretsTmpfsLifted
	policy.SecretsDir = ""
	policy.StopGrace = 2 * time.Second
	return realDriverWith(t, policy, images...)
}

// realDriverWith opens a driver under a policy of the test's own, or skips.
func realDriverWith(t *testing.T, policy Policy, images ...string) (*Docker, string) {
	t.Helper()
	socket, ok := dockertest.Socket()
	if !ok {
		dockertest.Unavailable(t, "no Docker daemon on this machine")
	}

	cli, err := docker.Dial(socket)
	if err != nil {
		dockertest.Unavailable(t, "the daemon at %s did not answer: %v", socket, err)
	}
	image := ""
	for _, ref := range append(images, "alpine:3.21", "alpine:latest", "busybox:latest") {
		if _, err := cli.ImageInspect(t.Context(), ref); err == nil {
			image = ref
			break
		}
	}
	cli.Close()
	if image == "" {
		dockertest.Unavailable(t, "no small image on this machine to run a script step in: docker pull alpine:3.21")
	}

	store, err := artifact.New(artifact.Dir(t.TempDir()), "finance", agk.DefaultLimits())
	if err != nil {
		t.Fatalf("opening the store: %s", err)
	}

	d, err := New(Config{
		Socket:   socket,
		Store:    func(string) (*artifact.Store, error) { return store, nil },
		Repo:     func(context.Context, string, string, string) (string, error) { return t.TempDir(), nil },
		Policy:   policy,
		WorkRoot: t.TempDir(),
		Announce: func(s string) { t.Log(s) },
	})
	if err != nil {
		dockertest.Unavailable(t, "opening a driver on %s: %v", socket, err)
	}
	t.Cleanup(func() { d.Close() })
	return d, image
}

// TestARealContainerRunsAScriptStepEndToEnd is the whole of one task against the daemon:
// the mounts, the environment, the shell, the port written under /agk/out/ports/, the
// exit code and the removal.
func TestARealContainerRunsAScriptStepEndToEnd(t *testing.T) {
	d, image := realDriver(t)

	task := graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4", "fetch", 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4",
		Workflow:  "finance/monthly-invoicing@a3f9c1e",
		Namespace: "finance",
		Commit:    "a3f9c1e",
		Step:      "fetch",
		Attempt:   1,
		Image:     image,
		Script: []string{
			`test -f /agk/params.json`,
			`test -d /agk/repo`,
			`test -n "$AGK_RUN_ID"`,
			`printf '{"meta":{"run_id":"%s","step":"%s","port":"out","attempt":1,"count":1,"produced_at":"2026-01-01T00:00:00Z"},"items":[{"id":"01JMZ8V1P9C5","data":{"ok":true},"files":[]}]}' "$AGK_RUN_ID" "$AGK_STEP" > /agk/out/ports/out.json`,
		},
		Outputs: []agk.Port{"out"},
		Network: graph.NetworkNone,
	}

	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running a script step in %s: %s", image, err)
	}
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the state is %s with exit code %d", result.State, result.ExitCode)
	}
	out, ok := result.Outputs["out"]
	if !ok {
		t.Fatalf("the declared port was not published: %v", result.Outputs)
	}
	if len(out.Items) != 1 {
		t.Errorf("the port carries %d items", len(out.Items))
	}
	if result.StartedAt.IsZero() || result.FinishedAt.IsZero() {
		t.Errorf("the two moments are %s and %s, and they come from the daemon", result.StartedAt, result.FinishedAt)
	}
}

// TestARealContainerThatFailsIsReadOffTheExitCodeTable, and the failure is the brick's
// rather than an error out of Run: a container that exited reported.
func TestARealContainerThatFailsIsReadOffTheExitCodeTable(t *testing.T) {
	d, image := realDriver(t)

	task := graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4", "fail", 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4",
		Namespace: "finance",
		Step:      "fail",
		Attempt:   1,
		Image:     image,
		Script:    []string{"exit 120"},
		Outputs:   []agk.Port{"out"},
		Network:   graph.NetworkNone,
	}

	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running: %s", err)
	}
	if result.State != agk.TaskFailed {
		t.Errorf("the state is %s, and exit 120 is a failure", result.State)
	}
	if result.ExitCode != 120 {
		t.Errorf("the exit code is %d, want 120", result.ExitCode)
	}
	if band := agk.Band(result.ExitCode); band.Retryable() {
		t.Errorf("exit 120 reads as %s and is retryable, and the table has it as permanent", band)
	}
}

// TestARealContainerIsStoppedAtItsDeadline holds the timeout against the daemon: SIGTERM,
// then SIGKILL after the grace, and a result that says timed out rather than failed.
func TestARealContainerIsStoppedAtItsDeadline(t *testing.T) {
	d, image := realDriver(t)

	task := graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4", "slow", 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4",
		Namespace: "finance",
		Step:      "slow",
		Attempt:   1,
		Image:     image,
		Script:    []string{"trap '' TERM; sleep 300"},
		Outputs:   []agk.Port{"out"},
		Network:   graph.NetworkNone,
		Timeout:   graph.Duration(2 * time.Second),
	}

	started := time.Now()
	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running: %s", err)
	}
	if result.State != agk.TaskTimedOut {
		t.Errorf("the state is %s with exit code %d, and an attempt stopped at its deadline is timed_out", result.State, result.ExitCode)
	}
	if result.ExitCode != 0 {
		t.Errorf("the exit code is %d, and it is set for succeeded and failed and for no other state", result.ExitCode)
	}
	if took := time.Since(started); took > 60*time.Second {
		t.Errorf("the deadline took %s to land", took)
	}
}

// TestTheSettingsTableIsReadBackOffTheContainerTheDaemonHolds reads every row of
// "settings applied to every container" off the container the daemon is actually holding
// rather than off the struct this package sent it.
//
// A struct test says what was composed; this says what a daemon made of it, which is the
// only place a field spelled wrongly, dropped by a marshaller or refused by a version
// shows up. The container is inspected while it runs, because the driver removes it once
// the exit code is collected.
func TestTheSettingsTableIsReadBackOffTheContainerTheDaemonHolds(t *testing.T) {
	d, image := realDriver(t)

	socket, _ := dockertest.Socket()
	cli, err := docker.Dial(socket)
	if err != nil {
		dockertest.Unavailable(t, "dialing %s to read the container back: %v", socket, err)
	}
	t.Cleanup(func() { cli.Close() })

	task := graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4", "settings", 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4",
		Namespace: "finance",
		Step:      "settings",
		Attempt:   1,
		Image:     image,
		Script:    []string{"sleep 3"},
		Outputs:   []agk.Port{"out"},
		Network:   graph.NetworkNone,
		Resources: graph.Resources{CPU: "0.5", Memory: "64Mi"},
		Timeout:   graph.Duration(30 * time.Second),
	}

	held := make(chan docker.Inspected, 1)
	go func() {
		for range 200 {
			found, err := cli.ContainerList(t.Context(), docker.Filters{}.Add("label", LabelTask+"="+string(task.ID)))
			if err == nil && len(found) > 0 {
				if in, err := cli.ContainerInspect(t.Context(), found[0].ID); err == nil && in.HostConfig != nil {
					held <- in
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		close(held)
	}()

	if _, err := d.Run(t.Context(), task); err != nil {
		t.Fatalf("running: %s", err)
	}
	in, ok := <-held
	if !ok {
		t.Fatal("the container was never found under its task label while it ran")
	}
	h := in.HostConfig

	if h.NetworkMode != networkModeNone {
		t.Errorf("NetworkMode is %q, and network: none is the default of the table and of the language", h.NetworkMode)
	}
	if !h.ReadonlyRootfs {
		t.Error("ReadonlyRootfs is false, and the table says true")
	}
	if len(h.CapDrop) != 1 || h.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop is %v, and the table says ALL", h.CapDrop)
	}
	if len(h.CapAdd) != 0 {
		t.Errorf("CapAdd is %v, and it is refused unless the runner policy allows it explicitly", h.CapAdd)
	}
	if !slices.Contains(h.SecurityOpt, "no-new-privileges:true") && !slices.Contains(h.SecurityOpt, "no-new-privileges") {
		t.Errorf("SecurityOpt is %v, with no no-new-privileges in it", h.SecurityOpt)
	}
	if h.AutoRemove {
		t.Error("AutoRemove is true, and the runner removes the container itself once the logs and the exit code are collected")
	}
	if h.Memory != 64<<20 {
		t.Errorf("Memory is %d, and the step asked for 64Mi", h.Memory)
	}
	if h.NanoCPUs != 500_000_000 {
		t.Errorf("NanoCpus is %d, and the step asked for 0.5 of a core", h.NanoCPUs)
	}
	if h.PidsLimit == nil || *h.PidsLimit != DefaultPolicy().PidsLimit {
		t.Errorf("PidsLimit is %v, and the table's default is %d", h.PidsLimit, DefaultPolicy().PidsLimit)
	}
	limits := map[string]docker.Ulimit{}
	for _, u := range h.Ulimits {
		limits[u.Name] = u
	}
	for _, name := range []string{"nofile", "nproc"} {
		if _, ok := limits[name]; !ok {
			t.Errorf("Ulimits carry %v, and the table names nofile and nproc", h.Ulimits)
		}
	}
	options, ok := h.Tmpfs[TmpDir]
	if !ok {
		t.Errorf("Tmpfs is %v, and %s is one of the two writable paths", h.Tmpfs, TmpDir)
	}
	for _, flag := range []string{"noexec", "nosuid", "nodev", "size="} {
		if !strings.Contains(options, flag) {
			t.Errorf("the %s tmpfs is mounted %q, with no %s", TmpDir, options, flag)
		}
	}

	// The mounts of the figure, read back off the container rather than off the
	// slice that asked for them.
	bound := map[string]docker.Mount{}
	for _, m := range h.Mounts {
		bound[m.Target] = m
	}
	for _, target := range []string{RepoDir, RunPath, ParamsPath} {
		m, ok := bound[target]
		if !ok {
			t.Errorf("nothing is bound at %s: %v", target, bound)
			continue
		}
		if !m.ReadOnly {
			t.Errorf("%s is bound writable, and the contract gives it read-only", target)
		}
	}
	if m, ok := bound[brick.OutDir]; !ok || m.ReadOnly {
		t.Errorf("%s is bound %+v, and it is where the brick writes", brick.OutDir, m)
	}
}

// TestARealContainerRunsUnderTheSeccompProfileRunnerTomlNames is the path from the file to
// the kernel: a profile named by seccomp_profile in runner.toml is read by LoadPolicy,
// carried on the create as its JSON, which is what the daemon decodes, and applied to the
// container. The profile refuses mkdir and mkdirat and allows everything else, so a
// directory the brick cannot make on a /tmp it can write to is the profile and nothing
// else. A daemon handed the path instead of the JSON refuses to start the container.
func TestARealContainerRunsUnderTheSeccompProfileRunnerTomlNames(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "seccomp.json")
	// Both names, because arm64 has no mkdir system call and the runtime passes over
	// a name the architecture does not have, as it does for Docker's own profile.
	if err := os.WriteFile(profile, []byte(`{
  "defaultAction": "SCMP_ACT_ALLOW",
  "syscalls": [
    { "names": ["mkdir", "mkdirat"], "action": "SCMP_ACT_ERRNO", "errnoRet": 1 }
  ]
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "runner.toml")
	if err := os.WriteFile(file, []byte(`# Docker Desktop does not offer the remapping, and this is the machine it is lifted for.
require_userns_remap = false
stop_grace = "2s"
seccomp_profile = "`+profile+`"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	policy, err := LoadPolicy(file)
	if err != nil {
		t.Fatalf("LoadPolicy: %s", err)
	}
	policy.SecretsDir = ""
	policy.RequireSecretsTmpfs = SecretsTmpfsLifted
	d, image := realDriverWith(t, policy)

	task := graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4", "seccomp", 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4",
		Namespace: "finance",
		Step:      "seccomp",
		Attempt:   1,
		Image:     image,
		Script: []string{
			`echo "seccomp: $(grep '^Seccomp:' /proc/self/status | awk '{print $2}')"`,
			`touch /tmp/file && echo "tmp: writable"`,
			`mkdir /tmp/dir 2>/tmp/err && echo "mkdir: allowed" || echo "mkdir: $(cat /tmp/err)"`,
		},
		Outputs: []agk.Port{"out"},
		Network: graph.NetworkNone,
	}
	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running under the profile runner.toml names: %v", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the state is %s with exit code %d", result.State, result.ExitCode)
	}
	got, _ := result.Outputs["out"].Items[0].Data["stdout"].(string)
	t.Logf("the container reports:\n%s", got)
	for _, want := range []string{
		// Seccomp: 2 is SECCOMP_MODE_FILTER.
		"seccomp: 2",
		"tmp: writable",
		// EPERM, which is errnoRet 1, and not the read-only root's EROFS.
		"Operation not permitted",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the container does not report %q", want)
		}
	}
	if strings.Contains(got, "mkdir: allowed") {
		t.Errorf("mkdir succeeded, so the container did not run under the profile the file names")
	}
}
