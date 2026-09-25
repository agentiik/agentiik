package driver

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/dockertest"
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
	if err := w.remove(); err != nil {
		t.Fatalf("removing the working directory: %s", err)
	}
	if _, err := os.Stat(w.Root); !os.IsNotExist(err) {
		t.Fatalf("the working directory survived: %v", err)
	}
	// A directory that is already gone is the outcome asked for, so a second
	// removal is not a failure. remove runs in a defer on every path out of a task.
	if err := w.remove(); err != nil {
		t.Fatalf("removing a working directory that is already gone: %s", err)
	}
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

// A tree that cannot be removed is reported rather than dropped, naming where the removal
// stopped, and the secrets directory is removed whatever became of the working directory,
// so that a brick that left an unreadable directory behind does not keep the values with
// it. A directory of mode 0500 is what a brick running as another account leaves a runner
// without CAP_DAC_OVERRIDE.
func TestARemovalThatFailsSaysWhereAndStillTakesTheSecretsAway(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, where every directory can be removed and there is nothing to report")
	}
	w, err := newWorkdir(t.TempDir(), shardedTask, t.TempDir())
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	if err := writeSecret(filepath.Join(w.Secrets, "bearer"), []byte("s3cr3t-value")); err != nil {
		t.Fatalf("writing the value: %s", err)
	}
	locked := filepath.Join(w.Out, "files", "nested")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "left.txt"), []byte("residue"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	err = w.remove()
	if err == nil {
		t.Fatalf("a working directory that could not be removed was reported as removed")
	}
	if !strings.Contains(err.Error(), locked) {
		t.Errorf("the report does not name where the removal stopped, %s: %s", locked, err)
	}
	if strings.Contains(err.Error(), "s3cr3t-value") {
		t.Errorf("the report carries the secret value: %s", err)
	}
	if _, err := os.Stat(w.Secrets); !os.IsNotExist(err) {
		t.Errorf("the secrets directory survived a working directory that could not be removed: %v", err)
	}
}

// A task's directory goes with its container and the run, step and attempt directories above
// it stay, one set per step ever run on the host, on the work root and on the secrets tmpfs.
// The sweep is what takes them away, and the directories it sweeps under stay.
func TestASweepTakesAwayTheParentsTasksLeftEmpty(t *testing.T) {
	root, shm := t.TempDir(), t.TempDir()
	for _, id := range []agk.TaskID{shardedTask, "01JMZ8V1P9C4/invoice/1", "01JMZ8V1P9C5/pay/1"} {
		w, err := newWorkdir(root, id, shm)
		if err != nil {
			t.Fatalf("newWorkdir: %s", err)
		}
		if err := w.remove(); err != nil {
			t.Fatalf("removing %s: %s", id, err)
		}
	}
	if left := dirsUnder(t, root); len(left) == 0 {
		t.Fatalf("the tasks left nothing on the work root to sweep, which is not what this is about")
	}
	record(t, root, "01JMZ8V1P9C4", "01JMZ8V1P9C5")

	sweep(root, shm, time.Now().Add(time.Minute))

	if left := tasksUnder(t, root); len(left) > 0 {
		t.Errorf("the work root still holds %v after the sweep", left)
	}
	if left := dirsUnder(t, filepath.Join(shm, secretsBase)); len(left) > 0 {
		t.Errorf("the secrets directory still holds %v after the sweep", left)
	}
	for _, dir := range []string{root, filepath.Join(shm, secretsBase)} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s was taken away with what was under it: %v", dir, err)
		}
	}
}

// Emptied a moment ago is the parent the next shard or attempt of the same step names next,
// and on Docker Desktop a path removed and created again is refused as a bind source for about
// a second after. The bound is what leaves it alone.
func TestASweepLeavesWhatWentEmptyWithinTheBound(t *testing.T) {
	root, shm := t.TempDir(), t.TempDir()
	w, err := newWorkdir(root, shardedTask, shm)
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	if err := w.remove(); err != nil {
		t.Fatal(err)
	}
	record(t, root, "01JMZ8V1P9C4")

	sweep(root, shm, time.Now().Add(-emptyKept))

	for _, dir := range []string{filepath.Dir(w.Root), filepath.Dir(w.Secrets)} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s was taken away, and it went empty within the last %s: %v", dir, emptyKept, err)
		}
	}
}

// Old enough and not the sweep's: the record and the trees under the work root, a directory
// somebody else put there, a run the record knows nothing of, a task's directory that could
// not be removed, and whatever is below a task's own directory. A work root is a directory
// somebody chose, the root of a filesystem mounted for it holding an empty lost+found, and
// the sweep takes only runs the record has and only what holds nothing.
func TestASweepTakesOnlyEmptyDirectoriesATaskCouldHaveLeft(t *testing.T) {
	root := t.TempDir()
	record(t, root, "01JMZ8V1P9C4", "01JMZ8V1P9C6")
	stay := []string{
		filepath.Join(root, KeysDir, "01JMZ8V1P9C4", "invoice"),
		filepath.Join(root, "lost+found"),
		filepath.Join(root, "cache", "v1", "2"),
		filepath.Join(root, "01JMZ8V1P9C7", "invoice", "1"),
		filepath.Join(root, "not a run", "invoice", "1"),
		filepath.Join(root, "01JMZ8V1P9C4", "not a step"),
		filepath.Join(root, "01JMZ8V1P9C4", "invoice", "02"),
		filepath.Join(root, "01JMZ8V1P9C4", "invoice", "2", "3-8", "out", "files"),
		filepath.Join(root, "01JMZ8V1P9C4", "invoice", "1", "in"),
	}
	for _, dir := range stay {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	residue := filepath.Join(root, "01JMZ8V1P9C6", "pay", "1", "params.json")
	if err := os.MkdirAll(filepath.Dir(residue), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(residue, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	sweep(root, "", time.Now().Add(time.Minute))

	for _, path := range append(stay, residue) {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was taken away: %v", path, err)
		}
	}
}

// A task given no secret has an empty secrets directory for as long as it runs, so emptiness
// does not tell a parent from a running task there. Its working directory, on the work root,
// does.
func TestASweepLeavesTheSecretsDirectoryOfATaskStillRunning(t *testing.T) {
	root, shm := t.TempDir(), t.TempDir()
	w, err := newWorkdir(root, "01JMZ8V1P9C4/invoice/1", shm)
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	defer w.remove()
	record(t, root, "01JMZ8V1P9C4")

	sweep(root, shm, time.Now().Add(time.Minute))

	for _, dir := range []string{w.Root, w.In, w.Secrets} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s was taken away from a task that is still running: %v", dir, err)
		}
	}
}

// The race the lock is for. MkdirAll finds a parent and then creates the directory below it,
// and a sweep that removed the parent between the two would refuse a sibling's task. Siblings
// of one step are created and removed over and over while a sweep that takes anything empty
// runs beside them, under the lock ended runs it under, and not one of them is refused.
func TestASweepNeverTakesAParentFromUnderASiblingBeingCreated(t *testing.T) {
	root, shm := t.TempDir(), t.TempDir()
	d := &Docker{cfg: Config{WorkRoot: root, Policy: Policy{SecretsDir: shm}}, keys: &keys{root: root}}
	record(t, root, "01JMZ8V1P9C4")

	done := make(chan struct{})
	swept := make(chan int)
	go func() {
		n := 0
		defer func() { swept <- n }()
		for {
			select {
			case <-done:
				return
			default:
			}
			d.keys.mu.Lock()
			sweep(root, shm, time.Now().Add(time.Hour))
			d.keys.mu.Unlock()
			n++
		}
	}()

	const shards, rounds = 8, 150
	var wg sync.WaitGroup
	refused := make(chan error, shards*rounds)
	for i := range shards {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := agk.NewTaskID("01JMZ8V1P9C4", "invoice", 1, agk.Shard{Index: i + 1, Of: shards})
			for range rounds {
				w, err := d.freshWorkdir(id)
				if err != nil {
					refused <- err
					continue
				}
				w.remove()
			}
		}()
	}
	wg.Wait()
	close(done)
	if n := <-swept; n == 0 {
		t.Fatalf("the sweep never ran beside the tasks, so nothing was tested")
	}
	close(refused)
	if n := len(refused); n > 0 {
		t.Errorf("%d of %d tasks were refused their working directory while a sweep ran beside them, the first with: %s", n, shards*rounds, <-refused)
	}
}

// A secrets directory somebody else made, or opened to others, is one a link could be swapped
// into between the walk and the removal, so the sweep leaves it alone, as ownedDir refuses a
// task's directory there.
func TestASweepLeavesASecretsDirectoryThatIsNoLongerTheRunnersAlone(t *testing.T) {
	root, shm := t.TempDir(), t.TempDir()
	w, err := newWorkdir(root, shardedTask, shm)
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	if err := w.remove(); err != nil {
		t.Fatal(err)
	}
	record(t, root, "01JMZ8V1P9C4")
	base := filepath.Join(shm, secretsBase)
	if err := os.Chmod(base, 0o777); err != nil {
		t.Fatal(err)
	}

	sweep(root, shm, time.Now().Add(time.Minute))

	if _, err := os.Stat(filepath.Dir(w.Secrets)); err != nil {
		t.Errorf("the sweep walked a secrets directory open to every account on the host: %v", err)
	}
	if left := tasksUnder(t, root); len(left) > 0 {
		t.Errorf("the work root still holds %v, and it is the runner's own", left)
	}
}

// record writes down under the work root that the record has the given runs.
func record(t *testing.T, root string, runs ...string) {
	t.Helper()
	for _, run := range runs {
		if err := os.MkdirAll(filepath.Join(root, KeysDir, run), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// tasksUnder is dirsUnder less the record, which the sweep is not about.
func tasksUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, path := range dirsUnder(t, dir) {
		if !strings.HasPrefix(path, filepath.Join(dir, KeysDir)) {
			out = append(out, path)
		}
	}
	return out
}

// dirsUnder names every directory below dir, so that a failure says what was left.
func dirsUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != dir {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The sweep runs where the record is pruned, when an ending is written, the first one a driver
// writes included, and what an earlier run left an hour and more ago goes. The task that just
// ended still has its directory at that moment, which goes with its container after.
func TestAnEndingSweepsWhatTasksLeftEmptyLongAgo(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := newRunner(t, oneImage(ref, goodManifest), func(dockertest.Container) (int, error) { return 0, nil })

	skeleton := filepath.Join(r.work, "01JMZ8V1P9C3", "invoice", "1")
	if err := os.MkdirAll(skeleton, 0o700); err != nil {
		t.Fatal(err)
	}
	// The record's directory of the run with nothing left in it, as a key forgotten on the
	// way out of a task that never reached its container leaves it, or a week of silence.
	// The prune that comes first leaves it while the run is still on the work root.
	record(t, r.work, "01JMZ8V1P9C3")
	long := time.Now().Add(-emptyKept - time.Minute)
	for dir := skeleton; dir != r.work; dir = filepath.Dir(dir) {
		if err := os.Chtimes(dir, long, long); err != nil {
			t.Fatal(err)
		}
	}

	task := stepTask(ref, "fetch")
	if _, err := r.Run(t.Context(), task); err != nil {
		t.Fatalf("running the task: %s", err)
	}

	if _, err := os.Stat(filepath.Join(r.work, "01JMZ8V1P9C3")); !os.IsNotExist(err) {
		t.Errorf("the run an earlier run left empty %s ago is still on the work root: %v", emptyKept+time.Minute, err)
	}
	w, err := workdirFor(r.work, task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Root); !os.IsNotExist(err) {
		t.Errorf("the task's own directory is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(w.Root)); err != nil {
		t.Errorf("the step of the task that just ended was swept with it: %v", err)
	}
}
