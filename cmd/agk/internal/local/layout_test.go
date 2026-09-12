package local

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

func TestALayoutNamesEveryPathOneRunWrites(t *testing.T) {
	root := t.TempDir()
	l, err := NewLayout(root)
	if err != nil {
		t.Fatalf("the layout: %s", err)
	}
	if l.Root != root {
		t.Errorf("the root is %s, want %s", l.Root, root)
	}
	// The two directories every run shares are made when the layout is, because two
	// runs over the same inputs write the same objects.
	for _, dir := range []string{l.Objects(), l.WorkRoot()} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Errorf("%s is not a directory: %v", dir, err)
		}
	}

	run := agk.RunID("01JMZ8V1P9C4")
	if got, want := l.RunFile(run), filepath.Join(root, "runs", string(run), "run.json"); got != want {
		t.Errorf("the run record is %s, want %s", got, want)
	}
	if got, want := l.State(run), filepath.Join(root, "runs", string(run), "state.json"); got != want {
		t.Errorf("the state is %s, want %s", got, want)
	}
	if got, want := l.Outputs(run), filepath.Join(root, "runs", string(run), "outputs"); got != want {
		t.Errorf("the outputs are in %s, want %s", got, want)
	}
	// The work root is not per run: the driver names a task's directory run/step/attempt
	// beneath it, so the run is already the first segment below.
	if got, want := l.Work(run), filepath.Join(l.WorkRoot(), string(run)); got != want {
		t.Errorf("the work of one run is in %s, want %s", got, want)
	}
}

// The work root is outside the tree, and this is the test of the one reason it is.
//
// The tree a local run is started in is bound read-only at /agk/repo in every container of
// the run, and .agk defaults to sitting inside it. A task's working directory is where the
// driver writes that task's secret values where the platform has no tmpfs, so a work root
// under .agk would put one step's secret at
// /agk/repo/.agk/work/<run>/<step>/<attempt>/secrets/<name>, readable by a step that
// declares none: a bind mount is not obliged to carry a host mode into a container, and on
// Docker Desktop a container reads a 0700 directory of the host as its own.
func TestTheWorkRootIsOutsideTheTreeBoundAtTheRepoMount(t *testing.T) {
	tree := t.TempDir()
	l, err := NewLayout(filepath.Join(tree, DefaultDir))
	if err != nil {
		t.Fatalf("the layout: %s", err)
	}

	// Not under the tree, which is what is bound, and not under the working directory
	// either, since that is inside the tree by default.
	for _, outside := range []string{tree, l.Root} {
		if under(l.WorkRoot(), outside) {
			t.Errorf("the work root %s is under %s, which is bound read-only at /agk/repo: a task's secret value would be readable by every other task of the run", l.WorkRoot(), outside)
		}
	}

	// And every record stays where a person looks for it.
	for _, inside := range []string{l.Objects(), l.Bin(), l.Runs()} {
		if !under(inside, l.Root) {
			t.Errorf("%s is not under %s, and the records of a run belong beside the workflow that ran", inside, l.Root)
		}
	}

	// Two layouts over one working directory name one work root, and two over different
	// ones name two: the driver is given this path, and two runs sharing a task directory
	// would share a task.
	same, err := NewLayout(filepath.Join(tree, DefaultDir))
	if err != nil {
		t.Fatalf("the second layout: %s", err)
	}
	if same.WorkRoot() != l.WorkRoot() {
		t.Errorf("one working directory named two work roots, %s and %s", l.WorkRoot(), same.WorkRoot())
	}
	other, err := NewLayout(filepath.Join(t.TempDir(), DefaultDir))
	if err != nil {
		t.Fatalf("the layout of another tree: %s", err)
	}
	if other.WorkRoot() == l.WorkRoot() {
		t.Errorf("two working directories named one work root, %s", l.WorkRoot())
	}

	// Private, because the mode of the parents is now the whole of what protects a value
	// that is readable by design.
	info, err := os.Stat(l.WorkRoot())
	if err != nil {
		t.Fatalf("the work root: %s", err)
	}
	if got := info.Mode().Perm(); got != workMode {
		t.Errorf("the work root is %#o, want %#o", got, workMode)
	}

	// And under this user's own directory rather than the shared temporary one, since the
	// name below it is derived from the working directory and is therefore a path anybody
	// on the host can work out.
	if cache, err := os.UserCacheDir(); err == nil && !under(l.WorkRoot(), cache) {
		t.Errorf("the work root %s is not under %s: a name anybody can work out, in a directory anybody can write, is a symlink somebody can plant before the first run", l.WorkRoot(), cache)
	}
}

// under says whether one path sits inside another, which is the question a bind mount asks.
func under(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func TestALogIsNamedByTheTaskItBelongsTo(t *testing.T) {
	l, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatalf("the layout: %s", err)
	}
	run := agk.RunID("01JMZ8V1P9C4")

	plain, err := l.Log(agk.NewTaskID(run, "fetch", 1, agk.Shard{}))
	if err != nil {
		t.Fatalf("naming the log of a task with no shard: %s", err)
	}
	if want := filepath.Join(l.Logs(run), "fetch", "1.log"); plain != want {
		t.Errorf("the log is %s, want %s", plain, want)
	}

	sharded, err := l.Log(agk.NewTaskID(run, "fetch", 2, agk.Shard{Index: 3, Of: 8}))
	if err != nil {
		t.Fatalf("naming the log of a shard: %s", err)
	}
	if want := filepath.Join(l.Logs(run), "fetch", "2-3-8.log"); sharded != want {
		t.Errorf("the log of a shard is %s, want %s, which is the spelling the driver gives its working directory", sharded, want)
	}

	if _, err := l.Log("not a task"); err == nil {
		t.Errorf("an identifier that could not have been composed was accepted")
	}
}

func TestALogIsOpenedWhereTheLayoutNamesIt(t *testing.T) {
	l, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatalf("the layout: %s", err)
	}
	id := agk.NewTaskID("01JMZ8V1P9C4", "fetch", 1, agk.Shard{Index: 1, Of: 2})
	w, err := logs{l}.OpenLog(t.Context(), id)
	if err != nil {
		t.Fatalf("opening the log: %s", err)
	}
	if _, err := w.Write([]byte("a line the container wrote\n")); err != nil {
		t.Fatalf("writing the log: %s", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing the log: %s", err)
	}
	path, err := l.Log(id)
	if err != nil {
		t.Fatalf("naming the log: %s", err)
	}
	doc, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the log was not written where the layout names it: %s", err)
	}
	if !strings.Contains(string(doc), "a line the container wrote") {
		t.Errorf("the log holds %q", doc)
	}
}
