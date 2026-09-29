package runner

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// "Trees are content-addressed and cached on the runner by commit, so a fleet fetches a commit
// once, not once per task": a second task of one commit, and a second assembly of one task, lay
// their trees out from what the first fetched, and fetch nothing.
func TestATreeIsFetchedOnceForEveryTaskOfItsCommit(t *testing.T) {
	s := newObjectStore(t)
	m, r := s.taskFor(t, nil, map[string]file{
		"agentiik.yaml":    {"version: 1\n", "0644"},
		"scripts/build.sh": {"#!/bin/sh\nmake\n", "0755"},
		"config/a.json":    {"{}\n", "0644"},
	}, nil)
	work := t.TempDir()
	first, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: work})
	if err != nil {
		t.Fatal(err)
	}
	fetched := s.gets.Load()
	if fetched != 3 {
		t.Fatalf("the first tree fetched %d objects of three", fetched)
	}

	other := m
	other.IdempotencyKey, other.Step = string(storeRun)+"/invoice-copy/1", "invoice-copy"
	second, err := Assemble(t.Context(), other, r, Assembly{WorkRoot: work})
	if err != nil {
		t.Fatal(err)
	}
	if n := s.gets.Load() - fetched; n != 0 {
		t.Errorf("a second task of the commit fetched %d objects", n)
	}
	for _, tree := range []string{first.Sources.Repo, second.Sources.Repo} {
		for path, want := range map[string]string{"agentiik.yaml": "version: 1\n", "scripts/build.sh": "#!/bin/sh\nmake\n"} {
			if b, err := os.ReadFile(filepath.Join(tree, filepath.FromSlash(path))); err != nil || string(b) != want {
				t.Errorf("%s of %s reads %q: %v", path, tree, b, err)
			}
		}
	}

	// A tree taken away with its task leaves what the cache keeps, and the next task finds it.
	if err := first.Remove(); err != nil {
		t.Fatal(err)
	}
	if err := second.Remove(); err != nil {
		t.Fatal(err)
	}
	third, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: work})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { third.Remove() })
	if n := s.gets.Load() - fetched; n != 0 {
		t.Errorf("a task after the others were removed fetched %d objects", n)
	}
}

// A file that is not executable is linked to the object the cache keeps, costing no copy, and an
// executable one is copied, since a link's mode is the object's own and every tree linking it
// would become executable with it.
func TestAKeptObjectIsLinkedUnlessTheTreeMakesItExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a link count and a mode bit are what this is about")
	}
	s := newObjectStore(t)
	m, r := s.taskFor(t, nil, map[string]file{
		"plain.txt": {"same bytes\n", "0644"},
		"run.sh":    {"same bytes\n", "0755"},
	}, nil)
	work := t.TempDir()
	a, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: work})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Remove() })
	kept, err := os.Stat(filepath.Join(work, ObjectsDir, "finance", r.Tree[0].SHA256))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := os.Stat(filepath.Join(a.Sources.Repo, "plain.txt"))
	run, _ := os.Stat(filepath.Join(a.Sources.Repo, "run.sh"))
	if !os.SameFile(kept, plain) || plain.Mode().Perm() != treeFileMode {
		t.Errorf("the plain file is not the kept object, or is %o", plain.Mode().Perm())
	}
	if os.SameFile(kept, run) || run.Mode().Perm() != treeExecMode || kept.Mode().Perm() != treeFileMode {
		t.Errorf("the executable file is the kept object, or is %o and the object %o", run.Mode().Perm(), kept.Mode().Perm())
	}
	if n := s.gets.Load(); n != 1 {
		t.Errorf("one object under two names was fetched %d times", n)
	}
}

// A namespace fetches what it names itself, and never lays a tree out from what another fetched:
// how fast a tree arrived would tell one namespace what another's repository holds.
func TestAKeptObjectNeverCrossesANamespace(t *testing.T) {
	s := newObjectStore(t)
	entries, o := treeOf(t, s, map[string]file{"a.txt": {"shared\n", "0644"}})
	work := t.TempDir()
	finance, err := cacheOf(work, "finance")
	if err != nil {
		t.Fatal(err)
	}
	payroll, err := cacheOf(work, "payroll")
	if err != nil {
		t.Fatal(err)
	}
	if finance.dir == payroll.dir {
		t.Fatalf("two namespaces keep their objects in %s", finance.dir)
	}
	if err := layOutTree(t.Context(), o, "finance", finance, emptyDir(t), entries, agk.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(payroll.dir, entries[0].SHA256)); err == nil {
		t.Error("an object finance fetched is kept for payroll")
	}
	for _, ns := range []string{"", ".", "..", "fin/ance", "fin\\ance"} {
		if _, err := cacheOf(work, ns); err == nil {
			t.Errorf("a cache was named after the namespace %q", ns)
		}
	}
}

// An object no tree has named for objectsKept goes, and so does a fetch that died, while one a
// tree named since stays; a tree linking an object keeps its bytes when the cache lets it go.
func TestTheCacheLetsGoOfWhatNoTreeNamesAnyMore(t *testing.T) {
	s := newObjectStore(t)
	entries, o := treeOf(t, s, map[string]file{"old.txt": {"old\n", "0644"}, "new.txt": {"new\n", "0644"}})
	work := t.TempDir()
	kept, err := cacheOf(work, "finance")
	if err != nil {
		t.Fatal(err)
	}
	tree := emptyDir(t)
	if err := layOutTree(t.Context(), o, "finance", kept, tree, entries, agk.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	var old string
	for _, e := range entries {
		if e.Path == "old.txt" {
			old = filepath.Join(kept.dir, e.SHA256)
		}
	}
	long := time.Now().Add(-objectsKept - time.Hour)
	if err := os.Chtimes(old, long, long); err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(kept.dir, ".fetching-123")
	if err := os.WriteFile(dead, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dead, time.Now().Add(-25*time.Hour), time.Now().Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}

	top := filepath.Join(work, ObjectsDir)
	pruned.Delete(top)
	pruneObjects(top, time.Now())
	left, _ := os.ReadDir(kept.dir)
	if len(left) != 1 || filepath.Join(kept.dir, left[0].Name()) == old {
		t.Errorf("the cache keeps %v", left)
	}
	if b, err := os.ReadFile(filepath.Join(tree, "old.txt")); err != nil || string(b) != "old\n" {
		t.Errorf("the tree linking the object let go of reads %q: %v", b, err)
	}

	// And once an hour: a second prune within it looks at nothing.
	if err := os.Chtimes(filepath.Join(kept.dir, left[0].Name()), long, long); err != nil {
		t.Fatal(err)
	}
	pruneObjects(top, time.Now())
	if again, _ := os.ReadDir(kept.dir); len(again) != 1 {
		t.Errorf("a second prune within the hour took %v", again)
	}
}

// The cache is private to the agent, as the trees are: a tree's permissive modes sit behind a
// directory nothing else on the host can enter.
func TestTheCacheIsPrivateToTheAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a mode bit is what this is about")
	}
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	work := t.TempDir()
	kept, err := cacheOf(work, "finance")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(work, ObjectsDir), kept.dir} {
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != treesMode {
			t.Errorf("%s is %v: %v", dir, info.Mode(), err)
		}
	}
}
