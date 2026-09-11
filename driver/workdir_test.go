package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

const shardedTask = agk.TaskID("01JMZ8V1P9C4/invoice/2/3/8")

// The task's identity is the path, spelled as directories rather than as an identifier
// with its separators replaced, so that two tasks can no more share a directory than
// they can share an identity.
func TestTheWorkingDirectoryIsTheTasksIdentity(t *testing.T) {
	root := t.TempDir()
	w, err := newWorkdir(root, shardedTask, "")
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	defer w.remove()

	want := filepath.Join(root, "01JMZ8V1P9C4", "invoice", "2", "3-8")
	if w.Root != want {
		t.Fatalf("the working directory is %s, want %s", w.Root, want)
	}
	for _, dir := range []string{w.Root, w.In, w.Out, w.Secrets, filepath.Join(w.Out, "ports"), filepath.Join(w.Out, "files")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("%s: %s", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
	}
}

// A step with no fan-out has no shard, so its directory has no segment for one, on the
// same reading AGK_SHARD takes.
func TestAStepWithNoFanOutHasNoShardSegment(t *testing.T) {
	root := t.TempDir()
	w, err := newWorkdir(root, "01JMZ8V1P9C4/invoice/1", "")
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	defer w.remove()
	if w.Root != filepath.Join(root, "01JMZ8V1P9C4", "invoice", "1") {
		t.Fatalf("the working directory is %s", w.Root)
	}
}

// The one permissive mode in the tree, and the reason it has to be: a container runs as
// the account its image declares, which this driver never resolves, and /agk/out is
// where the contract says it writes.
func TestTheOutputTreeIsWritableByWhateverAccountTheImageDeclares(t *testing.T) {
	w, err := newWorkdir(t.TempDir(), shardedTask, "")
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	defer w.remove()

	for _, dir := range []string{w.Out, filepath.Join(w.Out, "ports"), filepath.Join(w.Out, "files")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("%s: %s", dir, err)
		}
		if perm := info.Mode().Perm(); perm != outMode {
			t.Fatalf("%s is %04o, and a container whose account is not the runner's could not write it: want %04o", dir, perm, outMode)
		}
	}
	// Everything else is the runner's own, which is what makes the permissive leaf
	// safe: it is reachable only through parents nobody else can enter.
	info, err := os.Stat(w.Root)
	if err != nil {
		t.Fatalf("%s: %s", w.Root, err)
	}
	if perm := info.Mode().Perm(); perm != workdirMode {
		t.Fatalf("the task's directory is %04o, want %04o", perm, workdirMode)
	}
}

// "Created fresh." A directory left by a process that died between creating it and
// creating the container is removed rather than reused, because a brick would otherwise
// read an envelope from the attempt before this one and never know.
func TestAWorkingDirectoryIsCreatedFresh(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, "01JMZ8V1P9C4", "invoice", "2", "3-8", "in", "in")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatalf("preparing a stale directory: %s", err)
	}
	if err := os.WriteFile(filepath.Join(stale, "envelope.json"), []byte(`{"meta":{},"items":[]}`), 0o644); err != nil {
		t.Fatalf("preparing a stale envelope: %s", err)
	}

	w, err := newWorkdir(root, shardedTask, "")
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	defer w.remove()
	if _, err := os.Stat(filepath.Join(stale, "envelope.json")); !os.IsNotExist(err) {
		t.Fatalf("the envelope of an earlier attempt survived into this one: %v", err)
	}
}

// An identifier that could not have been composed is refused before anything is created.
func TestAnIdentifierThatIsNotATaskIsRefused(t *testing.T) {
	if _, err := newWorkdir(t.TempDir(), "not-a-task", ""); err == nil {
		t.Fatalf("a working directory was created for something that is not a task identifier")
	}
}

// Where the platform has a tmpfs, the values live on it and not under the task's own
// tree, and both are removed with the container.
func TestSecretsLiveOnTheirOwnFilesystemWhenThereIsOne(t *testing.T) {
	root, shm := t.TempDir(), t.TempDir()
	w, err := newWorkdir(root, shardedTask, shm)
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	if !strings.HasPrefix(w.Secrets, shm) {
		t.Fatalf("the secrets directory is %s, and the tmpfs is %s", w.Secrets, shm)
	}
	if _, err := os.Stat(w.Secrets); err != nil {
		t.Fatalf("the secrets directory was not created: %s", err)
	}

	w.remove()
	for _, dir := range []string{w.Root, w.Secrets} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s survived the task: %v", dir, err)
		}
	}
}

// "Removed with the container, so no residue of one namespace survives into the next
// task on that host."
func TestRemovingTakesTheWholeTreeAway(t *testing.T) {
	w, err := newWorkdir(t.TempDir(), shardedTask, "")
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	if err := os.WriteFile(filepath.Join(w.Out, "ports", "out.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("writing an output: %s", err)
	}
	w.remove()
	if _, err := os.Stat(w.Root); !os.IsNotExist(err) {
		t.Fatalf("the working directory survived: %v", err)
	}
	// A directory that is already gone is the outcome asked for, so a second
	// removal is not a failure. remove runs in a defer on every path out of a task.
	w.remove()
}

// Giving the tree to the account it is already owned by is what a chown to the remapped
// range does on a daemon whose range happens to be this process's own, and it has to
// work rather than being refused for being a no-op.
func TestOwnAcceptsTheAccountTheTreeAlreadyHas(t *testing.T) {
	w, err := newWorkdir(t.TempDir(), shardedTask, "")
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	defer w.remove()
	if err := w.own(os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("own: %s", err)
	}
}

// "A chown that cannot be done refuses the task naming the uid it tried and /etc/subuid,
// rather than creating a container that will silently fail to write its outputs."
func TestOwnRefusesWhatItCannotDoAndSaysWhatItTried(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, where every chown is permitted and there is nothing to refuse")
	}
	w, err := newWorkdir(t.TempDir(), shardedTask, "")
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	defer w.remove()

	err = w.own(165536, 165536)
	if err == nil {
		t.Fatalf("a chown into a range this process does not hold was reported as done")
	}
	for _, want := range []string{"165536", "/etc/subuid", "userns-remap"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q: %s", want, err)
		}
	}
}
