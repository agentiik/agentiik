package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// This file is the other half of real_test.go: what a real daemon can be held to that a
// fake of one cannot, run against a brick built here rather than against a base image.
//
// The brick is under testdata and is built by the test, because the point of it is to be
// a plain image that was never pushed anywhere: it carries a manifest at /agk/brick.yaml,
// declares a non-root account, and reports on standard error everything the contract
// promised it, so that the envelope on standard input, the envelope under /agk/in, the
// environment table, the repository, the parameters, the secret mount and the account it
// runs as are each read back out of the container rather than out of this project's
// reading of the daemon.

// probeImage is the brick built from testdata/probe.
const probeImage = "agk-probe-brick:test"

type memLogs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (m *memLogs) OpenLog(context.Context, agk.TaskID) (io.WriteCloser, error) {
	return nopWriteCloser{m}, nil
}

func (m *memLogs) Write(b []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buf.Write(b)
}

func (m *memLogs) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buf.String()
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

type fixedSecrets map[string][]byte

func (f fixedSecrets) Value(_ context.Context, name string) ([]byte, error) {
	v, ok := f[name]
	if !ok {
		return nil, errors.New("no such secret")
	}
	return v, nil
}

// TestARealDaemonWithoutTheRemappingIsRefusedByDefault is the first of the two decisions, run
// against the daemon of this machine rather than against a fake of one.
func TestARealDaemonWithoutTheRemappingIsRefusedByDefault(t *testing.T) {
	socket, ok := dockertest.Socket()
	if !ok {
		t.Skip("no Docker daemon on this machine")
	}
	_, err := New(Config{
		Socket:   socket,
		Policy:   DefaultPolicy(),
		WorkRoot: t.TempDir(),
	})
	if err == nil {
		t.Skip("this daemon remaps user namespaces, so the floor is met and there is nothing to refuse")
	}
	if !errors.Is(err, ErrUsernsRemapRequired) {
		t.Fatalf("the refusal is %v, and the floor refuses with ErrUsernsRemapRequired", err)
	}
	t.Logf("refused: %v", err)
	for _, want := range []string{"require_userns_remap = false", PolicyPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q", want)
		}
	}
}

// TestARealBrickIsGivenWhatTheContractPromises builds the smallest brick as a plain
// image and runs it: the envelope on standard input and at /agk/in/<port>/envelope.json,
// the environment table, the read-only repository, the parameters, a secret on a mount,
// the ports collected and the secret value masked out of the log.
func TestARealBrickIsGivenWhatTheContractPromises(t *testing.T) {
	socket, ok := dockertest.Socket()
	if !ok {
		t.Skip("no Docker daemon on this machine")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker command to build the probe image with")
	}
	buildProbe(t, probeImage, "probe")

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "workflow.yaml"), []byte("name: probe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := artifact.New(artifact.Dir(t.TempDir()), "finance", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	policy := DefaultPolicy()
	policy.RequireUsernsRemap = RemapLifted
	policy.SecretsDir = ""

	var said []string
	logs := &memLogs{}
	d, err := New(Config{
		Socket:   socket,
		Store:    func(string) (*artifact.Store, error) { return store, nil },
		Repo:     func(context.Context, string, string, string) (string, error) { return repo, nil },
		Secrets:  fixedSecrets{"bearer": []byte("s3cr3t-value-9f2a")},
		Logs:     logs,
		Policy:   policy,
		WorkRoot: t.TempDir(),
		Announce: func(s string) { said = append(said, s); t.Log(s) },
	})
	if err != nil {
		t.Fatalf("opening a driver on %s: %v", socket, err)
	}
	defer d.Close()

	// The lifted floor says, once, what is given up.
	lifted := false
	for _, s := range said {
		if strings.Contains(s, "user namespace remapping is off") && strings.Contains(s, "owned by a real uid on the host") {
			lifted = true
		}
	}
	if !lifted {
		t.Errorf("the lifted floor said %q, and it has to name what is given up", said)
	}

	in := agk.Envelope{
		Meta: agk.Meta{
			RunID: "01JMZ8V1P9C4XQ7K2N4D6F8H0A", Step: "upstream", Port: "in",
			Attempt: 1, Count: 1, ProducedAt: time.Unix(0, 0).UTC(),
		},
		Items: []agk.Item{{ID: "01JMZ8V1P9C4XQ7K2N4D6F8H0B", Data: map[string]any{"order": "A-1"}, Files: []agk.File{}}},
	}

	task := graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4XQ7K2N4D6F8H0A", "probe", 1, agk.Shard{Index: 3, Of: 8}),
		Run:       "01JMZ8V1P9C4XQ7K2N4D6F8H0A",
		Workflow:  "finance/monthly-invoicing@a3f9c1e",
		Namespace: "finance",
		Commit:    "a3f9c1e",
		Step:      "probe",
		Attempt:   1,
		Shard:     agk.Shard{Index: 3, Of: 8},
		Image:     probeImage,
		Params:    map[string]any{"endpoint": "https://example.test"},
		Secrets:   []graph.SecretMount{{Name: "bearer", Mount: "/agk/secrets/bearer"}},
		Inputs:    map[agk.Port]agk.Envelope{"in": in},
		Outputs:   []agk.Port{"out"},
		Network:   graph.NetworkNone,
		Timeout:   graph.Duration(60 * time.Second),
	}

	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running the probe brick: %v", err)
	}
	raw := logs.String()
	t.Logf("the task log reads:\n%s", raw)
	// The log is one JSON object per line, so what the container actually wrote is
	// read back out of the text field rather than out of the escaped document.
	log := logText(t, raw)

	if result.State != agk.TaskSucceeded {
		t.Fatalf("the state is %s with exit code %d", result.State, result.ExitCode)
	}

	out, ok := result.Outputs["out"]
	if !ok {
		t.Fatalf("the declared port was not collected: %v", result.Outputs)
	}
	if len(out.Items) != 1 {
		t.Fatalf("the port carries %d items", len(out.Items))
	}
	item := out.Items[0]

	// The envelope on standard input: the brick counted the bytes it read.
	var encoded bytes.Buffer
	if _, err := in.Encode(&encoded); err != nil {
		t.Fatal(err)
	}
	if got, want := jsonNumber(t, item.Data["stdin_bytes"]), int64(encoded.Len()); got != want {
		t.Errorf("the brick read %d bytes on standard input and the envelope is %d bytes", got, want)
	}

	// The envelope under /agk/in/<port>/envelope.json: the brick echoed it.
	if !strings.Contains(log, `"port":"in"`) || !strings.Contains(log, `"order":"A-1"`) {
		t.Errorf("the mounted envelope is not in the log, so /agk/in/in/envelope.json did not carry it")
	}

	// The environment table, every variable of it.
	for _, want := range []string{
		"AGK_RUN_ID=01JMZ8V1P9C4XQ7K2N4D6F8H0A",
		"AGK_WORKFLOW=finance/monthly-invoicing@a3f9c1e",
		"AGK_NAMESPACE=finance",
		"AGK_STEP=probe",
		"AGK_ATTEMPT=1",
		"AGK_SHARD=3/8",
		"AGK_REPO=/agk/repo",
		"AGK_COMMIT=a3f9c1e",
		"AGK_OUT_PORTS=out",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the container did not see %s", want)
		}
	}
	if !strings.Contains(log, "AGK_DEADLINE=20") {
		t.Errorf("AGK_DEADLINE is not an RFC 3339 moment in the log")
	}

	// The repository tree, the parameters and the run context.
	if !strings.Contains(log, "repo carries workflow.yaml") {
		t.Errorf("/agk/repo does not carry the tree")
	}
	if !strings.Contains(log, `"endpoint":"https://example.test"`) {
		t.Errorf("/agk/params.json does not carry the resolved parameters")
	}

	// The manifest was read out of the image, and the account it declares is the one
	// the container ran as.
	if !strings.Contains(log, "whoami=65532:65532") {
		t.Errorf("the container did not run as the account the manifest declares")
	}

	// Masking is a literal match against the values the task was given.
	if strings.Contains(log, "s3cr3t-value-9f2a") {
		t.Errorf("the secret value is in the log in the clear")
	}
	if !strings.Contains(log, "the secret reads") {
		t.Errorf("the secret was not mounted at /agk/secrets/bearer")
	}

	// The file the brick wrote beside its envelope was uploaded and addressed.
	if len(item.Files) != 1 {
		t.Fatalf("the item carries %d files", len(item.Files))
	}
	if item.Files[0].Name != "report.txt" || item.Files[0].URI.Name == "" {
		t.Errorf("the file is %+v, and one collected from /agk/out/files carries its agk:// URI", item.Files[0])
	}
}

// TestARealScriptThatWroteNothingPublishesItsStandardOutput is the shorthand, run
// against the daemon.
func TestARealScriptThatWroteNothingPublishesItsStandardOutput(t *testing.T) {
	d, image := realDriver(t)

	task := graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4XQ7K2N4D6F8H0A", "quiet", 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4XQ7K2N4D6F8H0A",
		Namespace: "finance",
		Step:      "quiet",
		Attempt:   1,
		Image:     image,
		Script:    []string{"echo hello from the shorthand"},
		Outputs:   []agk.Port{"out"},
		Network:   graph.NetworkNone,
	}

	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running: %v", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the state is %s with exit code %d", result.State, result.ExitCode)
	}
	out := result.Outputs["out"]
	if len(out.Items) != 1 {
		t.Fatalf("the shorthand published %d items: %+v", len(out.Items), out)
	}
	t.Logf("the shorthand published %+v", out.Items[0].Data)
}

// TestNetworkEgressIsRefusedBeforeAnythingIsCreated is the second decision, against the
// daemon: nothing is created and the refusal says why.
func TestNetworkEgressIsRefusedBeforeAnythingIsCreated(t *testing.T) {
	d, image := realDriver(t)

	task := graph.Task{
		ID:          agk.NewTaskID("01JMZ8V1P9C4XQ7K2N4D6F8H0A", "reach", 1, agk.Shard{}),
		Run:         "01JMZ8V1P9C4XQ7K2N4D6F8H0A",
		Namespace:   "finance",
		Step:        "reach",
		Attempt:     1,
		Image:       image,
		Script:      []string{"true"},
		Outputs:     []agk.Port{"out"},
		Network:     graph.NetworkEgress,
		EgressAllow: []string{"api.example.com:443"},
	}

	_, err := d.Run(t.Context(), task)
	if err == nil {
		t.Fatal("network: egress ran, and there is no proxy on this runner to enforce the allow list")
	}
	if !errors.Is(err, ErrEgressProxyMissing) {
		t.Fatalf("the refusal is %v", err)
	}
	t.Logf("refused: %v", err)
	if !strings.Contains(err.Error(), "api.example.com:443") {
		t.Errorf("the refusal does not name what the step asked to reach")
	}
}

// TestNetworkInternalGivesARealTaskANetworkOfItsOwn creates the network on the daemon and
// takes it away again.
func TestNetworkInternalGivesARealTaskANetworkOfItsOwn(t *testing.T) {
	d, image := realDriver(t)

	id := agk.NewTaskID("01JMZ8V1P9C4XQ7K2N4D6F8H0A", "internal", 1, agk.Shard{})
	task := graph.Task{
		ID:        id,
		Run:       "01JMZ8V1P9C4XQ7K2N4D6F8H0A",
		Namespace: "finance",
		Step:      "internal",
		Attempt:   1,
		Image:     image,
		// A network with no route out: the loopback is there and the world is not.
		Script:  []string{"ip -o addr show 2>&1 | head -5 >&2", "true"},
		Outputs: []agk.Port{"out"},
		Network: graph.NetworkInternal,
	}

	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running on an internal network: %v", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the state is %s with exit code %d", result.State, result.ExitCode)
	}

	out, err := exec.Command("docker", "network", "ls", "--format", "{{.Name}}").Output()
	if err != nil {
		t.Skipf("docker network ls: %v", err)
	}
	if strings.Contains(string(out), networkName(id)) {
		t.Errorf("the task's own network %s outlived the task", networkName(id))
	}
}

// buildProbe builds one of the bricks under testdata, and skips where it cannot.
//
// It is built rather than pulled because the point of it is to be a plain image that was
// never pushed anywhere, which is the image agk run --local meets. An image the daemon
// already holds is reused, so a second test in the same run does not rebuild it.
func buildProbe(t *testing.T, image, dir string) {
	t.Helper()
	if err := exec.Command("docker", "image", "inspect", image).Run(); err == nil {
		return
	}
	out, err := exec.Command("docker", "build", "-t", image, filepath.Join("testdata", dir)).CombinedOutput()
	if err != nil {
		t.Skipf("the probe brick in testdata/%s could not be built: %v\n%s", dir, err, out)
	}
}

// logText is what the container wrote, out of the timestamped indexed log the driver
// keeps.
func logText(t *testing.T, raw string) string {
	t.Helper()
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var entry struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("the log line %q is not one JSON object: %v", line, err)
		}
		b.WriteString(entry.Text)
		b.WriteString("\n")
	}
	return b.String()
}

func jsonNumber(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			t.Fatal(err)
		}
		return i
	default:
		t.Fatalf("%v is not a number", v)
		return 0
	}
}

// TestTheSettingsTableIsWhatARealContainerLivesUnder reads the settings back from
// inside a real container: the read-only root filesystem, the dropped capabilities,
// no-new-privileges, the pids ceiling and the two writable paths.
func TestTheSettingsTableIsWhatARealContainerLivesUnder(t *testing.T) {
	d, image := realDriver(t)

	task := graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4XQ7K2N4D6F8H0A", "settings", 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4XQ7K2N4D6F8H0A",
		Namespace: "finance",
		Step:      "settings",
		Attempt:   1,
		Image:     image,
		Script: []string{
			`touch /probe 2>/dev/null && echo "rootfs: writable" || echo "rootfs: read only"`,
			`echo "caps: $(grep CapEff /proc/self/status | awk '{print $2}')"`,
			`echo "nnp: $(grep NoNewPrivs /proc/self/status | awk '{print $2}')"`,
			`touch /tmp/probe && echo "tmp: writable"`,
			`touch /agk/out/probe && echo "out: writable"`,
			`echo "tmpfs: $(grep ' /tmp ' /proc/mounts)"`,
			`echo "socket: $(test -S /var/run/docker.sock && echo mounted || echo absent)"`,
		},
		Outputs: []agk.Port{"out"},
		Network: graph.NetworkNone,
	}

	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running: %v", err)
	}
	got, _ := result.Outputs["out"].Items[0].Data["stdout"].(string)
	t.Logf("the container reports:\n%s", got)

	for _, want := range []string{
		"rootfs: read only",
		// CapDrop: ALL, so the effective set is empty.
		"caps: 0000000000000000",
		// SecurityOpt: no-new-privileges.
		"nnp: 1",
		"tmp: writable",
		"out: writable",
		"noexec",
		"nosuid",
		"nodev",
		// "The daemon socket is never mounted inside a brick."
		"socket: absent",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the container does not report %q", want)
		}
	}
}

// TestARealManifestDeclaringRootIsRefusedBeforeAContainerExists, against the daemon and
// against an image that really declares it.
func TestARealManifestDeclaringRootIsRefusedBeforeAContainerExists(t *testing.T) {
	d, _ := realDriver(t)
	const rootImage = "agk-probe-root:test"
	buildProbe(t, rootImage, "probe-root")

	task := graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4XQ7K2N4D6F8H0A", "asroot", 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4XQ7K2N4D6F8H0A",
		Namespace: "finance",
		Step:      "asroot",
		Attempt:   1,
		Image:     rootImage,
		Outputs:   []agk.Port{"out"},
		Network:   graph.NetworkNone,
	}

	_, err := d.Run(t.Context(), task)
	if err == nil {
		t.Fatal("a manifest declaring a root user ran")
	}
	t.Logf("refused: %v", err)

	out, listErr := exec.Command("docker", "ps", "-a", "--filter", "label=dev.agentiik.task="+string(task.ID), "--format", "{{.ID}}").Output()
	if listErr == nil && strings.TrimSpace(string(out)) != "" {
		t.Errorf("a container was created for a manifest that declares root: %s", out)
	}
}

// TestARealExit125IsReadAsAnInfrastructureFailure is the exit code table's last row against the
// daemon: "125 and above: read as an infrastructure failure, charged to the runner and
// not to the brick".
func TestARealExit125IsReadAsAnInfrastructureFailure(t *testing.T) {
	d, image := realDriver(t)

	task := graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4XQ7K2N4D6F8H0A", "infra", 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4XQ7K2N4D6F8H0A",
		Namespace: "finance",
		Step:      "infra",
		Attempt:   1,
		Image:     image,
		Script:    []string{"exit 125"},
		Outputs:   []agk.Port{"out"},
		Network:   graph.NetworkNone,
	}

	logs := &memLogs{}
	d.cfg.Logs = logs

	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running: %v", err)
	}
	if result.ExitCode != 125 {
		t.Errorf("the exit code is %d, and a code is reported exactly as the container exited", result.ExitCode)
	}
	if band := agk.Band(result.ExitCode); band != agk.BandRuntimeFailure {
		t.Errorf("exit 125 reads as %s, and the table has it reserved for the runtime", band)
	}
	// The charge is said where a person reads it, which is the second half of
	// charging a code to the runtime.
	if got := logText(t, logs.String()); !strings.Contains(got, "charged to the runner and not to the brick") {
		t.Errorf("the log does not say whose failure exit 125 was:\n%s", got)
	}
}

// TestAfterScriptRunsInARealContainerWhenScriptFailed, against the daemon: "after_script runs in the
// same container even when script failed, so that a diagnostic dump survives a failure,
// and its own exit code does not change the step's verdict".
func TestAfterScriptRunsInARealContainerWhenScriptFailed(t *testing.T) {
	d, image := realDriver(t)

	task := graph.Task{
		ID:           agk.NewTaskID("01JMZ8V1P9C4XQ7K2N4D6F8H0A", "after", 1, agk.Shard{}),
		Run:          "01JMZ8V1P9C4XQ7K2N4D6F8H0A",
		Namespace:    "finance",
		Step:         "after",
		Attempt:      1,
		Image:        image,
		BeforeScript: []string{"echo before >&2"},
		Script:       []string{"echo during >&2", "exit 42"},
		AfterScript:  []string{"echo after >&2"},
		Outputs:      []agk.Port{"out"},
		Network:      graph.NetworkNone,
	}

	logs := &memLogs{}
	d.cfg.Logs = logs

	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running: %v", err)
	}
	got := logText(t, logs.String())
	t.Logf("the log reads:\n%s", got)

	if result.State != agk.TaskFailed || result.ExitCode != 42 {
		t.Errorf("the step is %s with code %d, and the verdict is the script's own 42", result.State, result.ExitCode)
	}
	for _, want := range []string{"before", "during", "after"} {
		if !strings.Contains(got, want) {
			t.Errorf("the log does not carry %q, and after_script runs even when script failed", want)
		}
	}
}

// TestARedeliveredTaskAdoptsTheRealContainerItAlreadyStarted, against the daemon: a
// container carrying this task's label is joined rather than a second one started, and
// what it wrote is collected off the mount it was given.
func TestARedeliveredTaskAdoptsTheRealContainerItAlreadyStarted(t *testing.T) {
	d, image := realDriver(t)

	id := agk.NewTaskID("01JMZ8V1P9C4XQ7K2N4D6F8H0A", "adopted", 1, agk.Shard{})
	task := graph.Task{
		ID:        id,
		Run:       "01JMZ8V1P9C4XQ7K2N4D6F8H0A",
		Namespace: "finance",
		Step:      "adopted",
		Attempt:   1,
		Image:     image,
		Script:    []string{"true"},
		Outputs:   []agk.Port{"out"},
		Network:   graph.NetworkNone,
	}

	// The first delivery's container, started outside this driver, exactly as one a
	// runner that died between the start and the report would have left behind.
	w, err := newWorkdir(d.cfg.WorkRoot, id, d.cfg.Policy.SecretsDir)
	if err != nil {
		t.Fatal(err)
	}
	script := `sleep 1; printf '{"meta":{"run_id":"01JMZ8V1P9C4XQ7K2N4D6F8H0A","step":"adopted","port":"out","attempt":1,"count":1,"produced_at":"2026-01-01T00:00:00Z"},"items":[{"id":"01JMZ8V1P9C4XQ7K2N4D6F8H0C","data":{"first":"delivery"},"files":[]}]}' > /agk/out/ports/out.json`
	started, err := exec.Command("docker", "run", "-d",
		"--label", LabelTask+"="+string(id),
		"--mount", "type=bind,source="+w.Out+",target=/agk/out",
		image, "/bin/sh", "-e", "-c", script).Output()
	if err != nil {
		t.Skipf("could not start the first delivery's container: %v", err)
	}
	container := strings.TrimSpace(string(started))
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", container).Run() })

	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("the redelivered task: %v", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the state is %s with exit code %d", result.State, result.ExitCode)
	}
	out := result.Outputs["out"]
	if len(out.Items) != 1 || out.Items[0].Data["first"] != "delivery" {
		t.Fatalf("the adopted container's own output was not collected: %+v", out)
	}

	// One container and not two: a redelivery that started a second one would run
	// the step twice.
	listed, err := exec.Command("docker", "ps", "-a", "--filter", "label="+LabelTask+"="+string(id), "--format", "{{.ID}}").Output()
	if err != nil {
		t.Skipf("docker ps: %v", err)
	}
	if left := strings.TrimSpace(string(listed)); left != "" {
		t.Errorf("a container carrying the task label outlived the redelivery: %s", left)
	}
}

// TestAStopEndsARealTaskInFlight, against the daemon: the container is stopped and the
// Run that was blocked on it comes back cancelled.
func TestAStopEndsARealTaskInFlight(t *testing.T) {
	d, image := realDriver(t)

	id := agk.NewTaskID("01JMZ8V1P9C4XQ7K2N4D6F8H0A", "stopped", 1, agk.Shard{})
	task := graph.Task{
		ID:        id,
		Run:       "01JMZ8V1P9C4XQ7K2N4D6F8H0A",
		Namespace: "finance",
		Step:      "stopped",
		Attempt:   1,
		Image:     image,
		Script:    []string{"echo running >&2", "sleep 300"},
		Outputs:   []agk.Port{"out"},
		Network:   graph.NetworkNone,
	}

	type outcome struct {
		result graph.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := d.Run(context.Background(), task)
		done <- outcome{r, err}
	}()

	// Wait for the container to be in flight rather than guessing at a delay.
	deadline := time.Now().Add(30 * time.Second)
	for d.lookup(id) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the task never reached the driver's registry")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := d.Stop(t.Context(), graph.Stop{Task: id, Reason: graph.StopCancelled}); err != nil {
		t.Fatalf("stopping: %v", err)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("the stopped Run answered with an error: %v", got.err)
		}
		if got.result.State != agk.TaskCancelled {
			t.Fatalf("the state is %s, and work called off is cancelled", got.result.State)
		}
		t.Logf("the stop came back as %s after %s", got.result.State, got.result.FinishedAt.Sub(got.result.StartedAt))
	case <-time.After(60 * time.Second):
		t.Fatal("the stopped Run never came back")
	}
}
