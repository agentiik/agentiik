package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installHelper writes a helper where the image or the release would install one, and answers with
// its path.
func installHelper(t *testing.T, body string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "agk-helper")
	if err := os.WriteFile(src, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return src
}

// The copy is under the work root, which the daemon resolves at the same path in both forms, and
// not at the installed path, which in the container form is inside the agent's image.
func TestTheInstalledHelperIsLaidDownUnderTheWorkRootWhereTheDaemonFindsIt(t *testing.T) {
	src := installHelper(t, "\x7fELF the helper")
	work := t.TempDir()

	got, ok, err := LayHelper(src, work)
	if err != nil || !ok {
		t.Fatalf("LayHelper answered %q, %v, %v", got, ok, err)
	}
	if want := filepath.Join(work, HelperDir, "agk"); got != want {
		t.Errorf("the helper was laid down at %s, want %s", got, want)
	}
	if b, err := os.ReadFile(got); err != nil || string(b) != "\x7fELF the helper" {
		t.Errorf("the copy holds %q, %v", b, err)
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o555 {
		t.Errorf("the copy's mode is %o, want 555: executable by the account a container runs as, writable by nobody", mode)
	}
	// Nothing else is left beside it, a staged file least of all.
	entries, err := os.ReadDir(filepath.Join(work, HelperDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("%s holds %d entries, want the copy alone", HelperDir, len(entries))
	}
}

func TestNoInstalledHelperLaysNothingDownAndIsNoRefusal(t *testing.T) {
	work := t.TempDir()
	got, ok, err := LayHelper(filepath.Join(t.TempDir(), "agk-helper"), work)
	if err != nil || ok || got != "" {
		t.Fatalf("LayHelper answered %q, %v, %v, want nothing and no error", got, ok, err)
	}
	if _, err := os.Stat(filepath.Join(work, HelperDir)); !os.IsNotExist(err) {
		t.Errorf("%s was created for a helper that is not installed: %v", HelperDir, err)
	}
}

func TestADirectoryWhereTheHelperIsInstalledIsRefused(t *testing.T) {
	src := t.TempDir()
	_, ok, err := LayHelper(src, t.TempDir())
	if err == nil || ok {
		t.Fatalf("a directory at the helper's path was taken, ok %v", ok)
	}
	if !strings.Contains(err.Error(), src+" is where the static helper is installed, and it is not a file") {
		t.Errorf("the refusal does not say what is wrong: %s", err)
	}
}

// An upgraded agent lays its own helper down over the last one's, and a restart of the same one
// finds its copy already there.
func TestACopyOfOtherBytesIsReplacedAndOneOfTheseBytesIsKept(t *testing.T) {
	work := t.TempDir()
	first, _, err := LayHelper(installHelper(t, "0.2.0"), work)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}

	// The same bytes keep the file, and its mode is set again.
	if err := os.Chmod(first, 0o400); err != nil {
		t.Fatal(err)
	}
	again, _, err := LayHelper(installHelper(t, "0.2.0"), work)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(again)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("a copy that already held these bytes was replaced")
	}
	if mode := after.Mode().Perm(); mode != 0o555 {
		t.Errorf("the kept copy's mode is %o, want 555", mode)
	}

	// Other bytes replace it with a new file, so a container that has the old one bound
	// keeps what it was given.
	upgraded, _, err := LayHelper(installHelper(t, "0.3.0"), work)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(upgraded); string(b) != "0.3.0" {
		t.Errorf("the copy holds %q after an upgrade", b)
	}
	replaced, err := os.Stat(upgraded)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, replaced) {
		t.Error("the copy was rewritten in place rather than replaced")
	}
}

// A link left under the copy's name is replaced, never followed: comparing through it, or setting
// its mode, would touch whatever it points at.
func TestALinkUnderTheCopysNameIsReplacedRatherThanFollowed(t *testing.T) {
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, HelperDir), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("the helper"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(work, HelperDir, "agk")); err != nil {
		t.Fatal(err)
	}

	got, _, err := LayHelper(installHelper(t, "the helper"), work)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(got)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("the copy is %s, want a regular file", info.Mode())
	}
	if t2, err := os.Stat(target); err != nil || t2.Mode().Perm() != 0o600 {
		t.Errorf("the link's target was touched: %v, %v", t2.Mode(), err)
	}
	if b, _ := os.ReadFile(target); !bytes.Equal(b, []byte("the helper")) {
		t.Errorf("the link's target now holds %q", b)
	}
}
