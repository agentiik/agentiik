package runner

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	dir := filepath.Join(t.TempDir(), "tree")
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
			dir := filepath.Join(t.TempDir(), "tree")
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
		if err := layOutTree(t.Context(), o, "finance", filepath.Join(t.TempDir(), "tree"), entries, agk.DefaultLimits()); err == nil {
			t.Fatal("a path named twice was laid out")
		}
	})
	t.Run("a file under a file", func(t *testing.T) {
		entries, o := treeOf(t, s, map[string]file{"a": {"x", "0644"}, "a/b": {"y", "0644"}})
		if err := layOutTree(t.Context(), o, "finance", filepath.Join(t.TempDir(), "tree"), entries, agk.DefaultLimits()); err == nil {
			t.Fatal("a file was laid out as a directory of another")
		}
	})
}

// What an earlier delivery of the task laid out is replaced, not built on: a file this delivery
// does not name is gone, and one it names is the bytes it names.
func TestATreeIsLaidOutFresh(t *testing.T) {
	s := newObjectStore(t)
	entries, o := treeOf(t, s, map[string]file{"agentiik.yaml": {"version: 1\n", "0644"}})
	dir := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(dir, "stale"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agentiik.yaml"), []byte("version: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := layOutTree(t.Context(), o, "finance", dir, entries, agk.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("what an earlier delivery left is still there: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "agentiik.yaml")); string(b) != "version: 1\n" {
		t.Errorf("the entry point reads %q", b)
	}
}

// A file longer than any object of the store may be is refused as it arrives.
func TestATreeFileAboveTheObjectLimitIsRefused(t *testing.T) {
	s := newObjectStore(t)
	entries, o := treeOf(t, s, map[string]file{"big": {strings.Repeat("x", 64), "0644"}})
	l := agk.DefaultLimits()
	l.ArtifactMaxBytes = 32
	err := layOutTree(t.Context(), o, "finance", filepath.Join(t.TempDir(), "tree"), entries, l)
	if !errors.Is(err, ErrNotAsNamed) {
		t.Errorf("a file above artifact_max_bytes answered %v", err)
	}
}

// A task's tree sits beside the task directories under the work root, in none of them, and is
// named after the task.
func TestATreeIsNamedAfterItsTask(t *testing.T) {
	dir, err := treeDir("/var/lib/agentiik/work", "01JMZ8V1P9C4/invoice/2/3/8")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.FromSlash("/var/lib/agentiik/work/.trees/01JMZ8V1P9C4/invoice/2/3/8"); dir != want {
		t.Errorf("the tree is at %s, want %s", dir, want)
	}
	for _, id := range []agk.TaskID{"../../etc", "01JMZ8V1P9C4/invoice", ""} {
		if _, err := treeDir("/var/lib/agentiik/work", id); err == nil {
			t.Errorf("a tree was named after %q", id)
		}
	}
	if _, err := treeDir("", "01JMZ8V1P9C4/invoice/2"); err == nil {
		t.Error("a tree was named with no work root")
	}
}
