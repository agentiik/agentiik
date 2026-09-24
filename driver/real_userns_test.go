package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
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

// This file holds the driver to what a runner is on the machine it is installed on: a
// daemon set to userns-remap, an agent that is not root and holds CAP_CHOWN, CAP_FOWNER and
// CAP_DAC_OVERRIDE and nothing else, and a secrets directory on a tmpfs mounted
// noexec,nosuid,nodev. Every floor is held, none is lifted.
//
// Neither Docker Desktop nor the daemon of a CI runner remaps, so these tests skip on both
// and say so. The userns job in CI sets the daemon to userns-remap, mounts the tmpfs, runs
// them under setpriv with the three capabilities, and sets requireUserns so that a test
// that cannot run there fails rather than skips.

// requireUserns names the variable that turns every skip in this file into a failure, and
// secretsDirVariable the tmpfs the job mounted.
const (
	requireUserns      = "AGENTIIK_TEST_REQUIRE_USERNS"
	secretsDirVariable = "AGENTIIK_TEST_SECRETS_DIR"
)

// usernsUnavailable skips, or fails where the userns job said it would not.
func usernsUnavailable(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv(requireUserns) == "1" {
		t.Fatalf("%s is 1, so this fails where it would have skipped: %s", requireUserns, fmt.Sprintf(format, args...))
	}
	t.Skipf(format, args...)
}

// events keeps what the observer heard, for a test that looks at the host while the
// container runs.
type events struct {
	mu    sync.Mutex
	on    func(Event)
	heard []Event
}

func (e *events) Observe(_ context.Context, ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.heard = append(e.heard, ev)
	if e.on != nil {
		e.on(ev)
	}
}

// remappedDriver opens a driver as agk-runner serve opens one, every floor held, on the
// daemon of this machine, or ends the test through usernsUnavailable.
func remappedDriver(t *testing.T, observer Observer, image string) (*Docker, string) {
	t.Helper()
	socket, ok := dockertest.Socket()
	if !ok {
		usernsUnavailable(t, "no Docker daemon on this machine")
	}
	daemon, err := Probe(t.Context(), socket)
	if err != nil {
		usernsUnavailable(t, "the daemon at %s did not answer: %v", socket, err)
	}
	if !daemon.UsernsRemapped {
		usernsUnavailable(t, "the daemon at %s does not remap user namespaces, and what is under test is a runner on one that does: the userns job in CI runs it", socket)
	}
	secrets := os.Getenv(secretsDirVariable)
	if secrets == "" {
		usernsUnavailable(t, "%s names no tmpfs mounted noexec,nosuid,nodev for the secret values", secretsDirVariable)
	}

	cli, err := docker.Dial(socket)
	if err != nil {
		usernsUnavailable(t, "the daemon at %s did not answer: %v", socket, err)
	}
	_, err = cli.ImageInspect(t.Context(), image)
	cli.Close()
	if err != nil {
		usernsUnavailable(t, "%s is not on this daemon, and a remapped daemon keeps its images apart from the ones it held before the remapping: the userns job pulls or builds it after the restart", image)
	}

	store, err := artifact.New(artifact.Dir(t.TempDir()), "finance", agk.DefaultLimits())
	if err != nil {
		t.Fatalf("opening the store: %s", err)
	}
	policy := DefaultPolicy()
	policy.SecretsDir = secrets
	policy.StopGrace = 2 * time.Second
	d, err := New(Config{
		Socket:   socket,
		Store:    func(string) (*artifact.Store, error) { return store, nil },
		Repo:     func(context.Context, string, string, string) (string, error) { return t.TempDir(), nil },
		Secrets:  secretSource{"bearer": "s3cr3t-value"},
		Observer: observer,
		Policy:   policy,
		WorkRoot: t.TempDir(),
		Announce: func(s string) { t.Log(s) },
	})
	if errors.Is(err, ErrOwnershipCapabilities) || errors.Is(err, ErrSecretsTmpfsRequired) {
		usernsUnavailable(t, "this is not a runner's host: %v", err)
	}
	if err != nil {
		t.Fatalf("a runner's host was refused: %s", err)
	}
	t.Cleanup(func() { d.Close() })
	return d, secrets
}

// The two images a task in the range runs from: alpine as its own root, which is the base
// of the range on the host, and the same image as 65532, the account a brick is required
// to run as, which is another account of the range and owns nothing it was given. The
// userns job builds the second after the restart.
const (
	rootImage    = "alpine:3.21"
	nonRootImage = "agentiik-test/nonroot:3.21"
)

// inRange is one task that reports, on its port out, what it found: the owners of what it
// was given as the container reads them, whether its input and its secret read back, and
// the flags of its secret mount. It leaves a nested directory it closed behind it, which a
// runner without CAP_DAC_OVERRIDE could not remove.
func inRange(image string) graph.Task {
	return graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4", "invoice", 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4",
		Workflow:  "finance/monthly-invoicing@a3f9c1e",
		Namespace: "finance",
		Commit:    "a3f9c1e",
		Step:      "invoice",
		Attempt:   1,
		Image:     image,
		Inputs: map[agk.Port]agk.Envelope{
			"in": {Items: []agk.Item{agk.NewItem(map[string]any{"url": "https://example.test"})}},
		},
		Secrets: []graph.SecretMount{{Name: "bearer", Mount: "/agk/secrets/bearer"}},
		Script: []string{
			`set -eu`,
			`grep -q example.test /agk/in/in/envelope.json`,
			`test "$(cat /agk/secrets/bearer)" = s3cr3t-value`,
			`input=$(stat -c %u:%g /agk/in/in/envelope.json)`,
			`secret=$(stat -c %u:%g /agk/secrets/bearer)`,
			`out=$(stat -c %u:%g /agk/out)`,
			`flags=$(awk '$5 == "/agk/secrets/bearer" { print $6 }' /proc/self/mountinfo)`,
			`mkdir -p /agk/out/scratch/nested`,
			`echo residue > /agk/out/scratch/nested/left.txt`,
			`chmod 0500 /agk/out/scratch/nested /agk/out/scratch`,
			`printf '{"meta":{"run_id":"%s","step":"%s","port":"out","attempt":1,"count":1,"produced_at":"2026-01-01T00:00:00Z"},"items":[{"id":"01JMZ8V1P9C5","data":{"input":"%s","secret":"%s","out":"%s","flags":"%s"},"files":[]}]}' "$AGK_RUN_ID" "$AGK_STEP" "$input" "$secret" "$out" "$flags" > /agk/out/ports/out.json`,
		},
		Outputs: []agk.Port{"out"},
		Network: graph.NetworkNone,
	}
}

// found is what inRange reported.
type found struct {
	Input, Secret, Out, Flags string
}

func runInRange(t *testing.T, d *Docker, task graph.Task) found {
	t.Helper()
	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running a task on a remapped daemon: %s", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the task ended %s with exit code %d, and it exits non-zero where it could not read its input or its secret", result.State, result.ExitCode)
	}
	out := result.Outputs["out"]
	if len(out.Items) != 1 {
		t.Fatalf("the port carries %d items", len(out.Items))
	}
	var f found
	b, err := json.Marshal(out.Items[0].Data)
	if err == nil {
		err = json.Unmarshal(b, &f)
	}
	if err != nil {
		t.Fatalf("reading what the task found: %s", err)
	}
	return f
}

// "The runner prepares each task's working directory with ownership inside the remapped
// range before it creates the container", and removes it with the container. The task's
// files belong to the base of the range on the host, which the container reads as its own
// root; the brick reads its input and its secret and writes its output; and the working
// directory, the secret and the nested directory the brick closed are gone once the task
// has ended.
func TestARealRemappedDaemonRunsATaskInsideItsRange(t *testing.T) {
	for _, image := range []string{rootImage, nonRootImage} {
		t.Run(image, func(t *testing.T) {
			seen := &events{}
			d, secretsDir := remappedDriver(t, seen, image)
			task := inRange(image)
			w, err := workdirFor(d.cfg.WorkRoot, task.ID, secretsDir)
			if err != nil {
				t.Fatal(err)
			}
			// The owner is read on the host while the container runs, between the chown
			// and the removal.
			var owner string
			seen.on = func(e Event) {
				if e.State != agk.TaskRunning {
					return
				}
				info, err := os.Lstat(w.Root)
				if err != nil {
					owner = err.Error()
					return
				}
				if uid, ok := ownerOf(info); ok {
					owner = fmt.Sprint(uid)
				}
			}

			f := runInRange(t, d, task)
			if want := fmt.Sprint(d.floor.UID); owner != want {
				t.Errorf("the working directory belonged to %s on the host while the container ran, and the base of the range is %s", owner, want)
			}
			for what, got := range map[string]string{"its input": f.Input, "its secret": f.Secret, "/agk/out": f.Out} {
				if got != "0:0" {
					t.Errorf("the container read %s as owned by %s, and what belongs to the base of the range is the container's root, 0:0", what, got)
				}
			}
			for _, tree := range w.trees() {
				if _, err := os.Lstat(tree); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s survived the task: %v", tree, err)
				}
			}
		})
	}
}

// "Tmpfs: /tmp and the secret mount points, with noexec,nosuid,nodev." A secret is bound
// from the runner's tmpfs rather than mounted as one, and a bind keeps the flags of the
// mount its source sits on, so the container's own mount table carries the three.
func TestARealRemappedDaemonGivesTheSecretMountTheTmpfsFlags(t *testing.T) {
	d, _ := remappedDriver(t, nil, rootImage)
	f := runInRange(t, d, inRange(rootImage))
	flags := strings.Split(f.Flags, ",")
	for _, want := range []string{"noexec", "nosuid", "nodev"} {
		if !slices.Contains(flags, want) {
			t.Errorf("the secret mount is %q in /proc/self/mountinfo, without %s", f.Flags, want)
		}
	}
}

// A runner holding CAP_FOWNER can change the mode of a directory it does not own, so the
// chmod that once refused a secrets base somebody else made first would now close it and
// keep it, that account still its owner. The owner is read instead, and the base refused.
// Only a process holding CAP_CHOWN can make a directory another account owns, so this runs
// where the userns job gives it the three.
func TestARealRunnerHostRefusesASecretsBaseAnotherAccountOwns(t *testing.T) {
	held, err := effectiveCapabilities()
	if err != nil || held&(1<<0|1<<3) != 1<<0|1<<3 {
		usernsUnavailable(t, "this process does not hold CAP_CHOWN and CAP_FOWNER, so it can neither make a directory another account owns nor close one: %v", err)
	}
	secrets := t.TempDir()
	base := secrets + "/" + secretsBase
	if err := os.Mkdir(base, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(base, os.Geteuid()+1, os.Getegid()); err != nil {
		t.Fatalf("giving %s to another account: %s", base, err)
	}
	t.Cleanup(func() { os.Chown(base, os.Geteuid(), os.Getegid()) })

	_, err = newWorkdir(t.TempDir(), shardedTask, secrets)
	if err == nil {
		t.Fatalf("a secrets base another account owns was taken")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("belongs to uid %d", os.Geteuid()+1)) {
		t.Errorf("the refusal does not name the account that owns it: %s", err)
	}
}
