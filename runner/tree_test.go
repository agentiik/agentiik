package runner

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact/granted"
)

// treeOf stores the files of a tree and answers its entries, with the objects that fetch them.
func treeOf(t *testing.T, s *objectStore, files map[string]file) ([]TreeEntry, *granted.Objects) {
	t.Helper()
	_, r := s.taskFor(t, nil, files, nil)
	o, err := objectsOf("finance", r, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r.Tree, o
}

// An entry point the tree gives an executable bit runs where it is laid out, as the account a
// remapped container runs as would run it: executable and readable by anybody, writable by
// nobody.
func TestAnExecutableTreeFileStaysExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a mode bit is what this is about, and Windows has none")
	}
	s := newObjectStore(t)
	entries, o := treeOf(t, s, map[string]file{
		"scripts/entry.sh": {"#!/bin/sh\necho laid out\n", "0755"},
		"scripts/lib.sh":   {"echo sourced\n", "0644"},
		"scripts/run.sh":   {"#!/bin/sh\necho by owner alone\n", "0700"},
	})
	dir := emptyDir(t)
	if err := layOutTree(t.Context(), o, "finance", dir, entries, agk.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"entry.sh": 0o555, "lib.sh": 0o444, "run.sh": 0o555} {
		info, err := os.Stat(filepath.Join(dir, "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s is %o, want %o", name, got, want)
		}
	}
	out, err := exec.Command(filepath.Join(dir, "scripts", "entry.sh")).Output()
	if err != nil || strings.TrimSpace(string(out)) != "laid out" {
		t.Errorf("the entry point ran as %q: %v", out, err)
	}
}

// A path that leaves /agk/repo, names it twice or is not written as it cleans to is refused, and
// nothing of the tree is left.
func TestATreePathOutsideTheRepositoryIsRefused(t *testing.T) {
	s := newObjectStore(t)
	for _, path := range []string{"../escape", "a/../../escape", "/etc/passwd", "a//b", "./a", "a/", ".", ""} {
		t.Run(path, func(t *testing.T) {
			entries, o := treeOf(t, s, map[string]file{"a": {"x", "0644"}})
			entries[0].Path = path
			dir := emptyDir(t)
			if err := layOutTree(t.Context(), o, "finance", dir, entries, agk.DefaultLimits()); err == nil {
				t.Fatal("the tree was laid out")
			}
			if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("a refused tree left %s: %v", dir, err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("a path climbing out of the tree was written: %v", err)
			}
		})
	}
	t.Run("twice", func(t *testing.T) {
		entries, o := treeOf(t, s, map[string]file{"a": {"x", "0644"}})
		entries = append(entries, entries[0])
		if err := layOutTree(t.Context(), o, "finance", emptyDir(t), entries, agk.DefaultLimits()); err == nil {
			t.Fatal("a path named twice was laid out")
		}
	})
	t.Run("a file under a file", func(t *testing.T) {
		entries, o := treeOf(t, s, map[string]file{"a": {"x", "0644"}, "a/b": {"y", "0644"}})
		if err := layOutTree(t.Context(), o, "finance", emptyDir(t), entries, agk.DefaultLimits()); err == nil {
			t.Fatal("a file was laid out as a directory of another")
		}
	})
}

// A task assembled again, as it is when its message comes round to its holder or when the agent
// restarts under its running container, lays out a tree of its own, and the tree the running
// container was given is neither rewritten nor taken away, by the assembly or by its removal.
func TestEachAssemblyOfATaskLaysOutATreeOfItsOwn(t *testing.T) {
	s := newObjectStore(t)
	m, r := s.taskFor(t, nil, map[string]file{"agentiik.yaml": {"version: 1\n", "0644"}}, nil)
	work := t.TempDir()
	first, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: work})
	if err != nil {
		t.Fatal(err)
	}
	again, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: work})
	if err != nil {
		t.Fatal(err)
	}
	if first.Sources.Repo == again.Sources.Repo {
		t.Fatalf("two assemblies of one task share the tree %s", first.Sources.Repo)
	}
	if err := again.Remove(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(first.Sources.Repo, "agentiik.yaml")); err != nil || string(b) != "version: 1\n" {
		t.Errorf("the first tree reads %q once the second is removed: %v", b, err)
	}
	if err := first.Remove(); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(filepath.Join(work, TreesDir)); len(left) != 0 {
		t.Errorf("the trees left behind: %v", left)
	}
}

// Every directory of the tree is open to the container, whatever the agent's umask closes: the
// container reads the tree as an account the files do not belong to.
func TestATreeIsOpenToTheContainerWhateverTheUmask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a mode bit is what this is about, and Windows has none")
	}
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	s := newObjectStore(t)
	entries, o := treeOf(t, s, map[string]file{"src/pkg/main.py": {"print(1)\n", "0644"}, "src/deep/er/x": {"x", "0644"}})
	dir := emptyDir(t)
	if err := layOutTree(t.Context(), o, "finance", dir, entries, agk.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".", "src", "src/pkg", "src/deep", "src/deep/er"} {
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != treeDirMode {
			t.Errorf("%s is %o, want %o", rel, got, treeDirMode)
		}
	}
}

// A file longer than any object of the store may be is refused as it arrives.
func TestATreeFileAboveTheObjectLimitIsRefused(t *testing.T) {
	s := newObjectStore(t)
	entries, o := treeOf(t, s, map[string]file{"big": {strings.Repeat("x", 64), "0644"}})
	l := agk.DefaultLimits()
	l.ArtifactMaxBytes = 32
	err := layOutTree(t.Context(), o, "finance", emptyDir(t), entries, l)
	if !errors.Is(err, ErrNotAsNamed) {
		t.Errorf("a file above artifact_max_bytes answered %v", err)
	}
}

// A task's tree sits beside the task directories under the work root, in none of them, in a
// directory private to the agent, and is named after the task.
func TestATreeIsNamedAfterItsTask(t *testing.T) {
	work := t.TempDir()
	dir, err := newTreeDir(work, "01JMZ8V1P9C4/invoice/2/3/8")
	if err != nil {
		t.Fatal(err)
	}
	if parent, base := filepath.Dir(dir), filepath.Base(dir); parent != filepath.Join(work, TreesDir) || !strings.HasPrefix(base, "01JMZ8V1P9C4.invoice.2.3-8.") {
		t.Errorf("the tree is at %s", dir)
	}
	if info, err := os.Stat(filepath.Join(work, TreesDir)); err != nil || info.Mode().Perm() != treesMode {
		t.Errorf("the trees directory is %v: %v", info.Mode(), err)
	}
	for _, id := range []agk.TaskID{"../../etc", "01JMZ8V1P9C4/invoice", "01JMZ8V1P9C4/invoice/02", ""} {
		if _, err := newTreeDir(work, id); err == nil {
			t.Errorf("a tree was named after %q", id)
		}
	}
	if _, err := newTreeDir("", "01JMZ8V1P9C4/invoice/2"); err == nil {
		t.Error("a tree was named with no work root")
	}
}

// emptyDir is a new, empty directory for a tree, as newTreeDir makes one.
func emptyDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(t.TempDir(), "tree.")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
