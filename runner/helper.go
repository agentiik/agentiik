package runner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// HelperPath is where the static helper is installed beside the agent: the runner's image holds
// it there, and a bare metal host installs it there beside /usr/local/bin/agk-runner.
//
// It is what serve binds at /agk/bin/agk for a script step where runner.toml names no helper,
// since "a static helper for scripts that want to be precise rather than lucky" is part of what a
// runner offers and not something each operator should have to remember to switch on. It is
// installed from the same release as the agent, for the same architecture, and the agent drives
// the daemon of the machine it runs on, so the helper is the machine a container runs natively.
const HelperPath = "/usr/local/lib/agentiik/agk-helper"

// HelperDir is the directory under the work root the helper is laid down in.
//
// The dot keeps it apart from the task directories beside it, whose first segment is a run
// identifier, a ULID, which never begins with one; driver.KeysDir is kept apart the same way.
const HelperDir = ".bin"

// helperName is the laid down copy's name, the program's name inside the container.
const helperName = "agk"

// helperMode is the copy's mode: readable and executable by everybody, since a container runs as
// an account of its image that this side never resolves, and writable by nobody, since it is
// bound read-only and a writable copy is one that could be replaced between two tasks.
const helperMode = 0o555

// LayHelper copies the helper installed at src under the work root, and answers with the copy's
// path, which is what the driver binds.
//
// The copy is not avoidable. The daemon resolves a bind's source on the host, and in the container
// form the agent's own filesystem is not the host's: a path in the image is not there for the
// daemon, which refuses the mount, so every script step would fail on the platform's account. The
// work root is the one directory both forms mount at the same path on both sides, so a copy there
// is a file the daemon finds, whichever form the agent runs in.
//
// A src that is not there answers ok false and no error, since the helper "is a convenience, never
// a requirement": the driver binds nothing and a script step uses jq and a redirect instead. A src
// that is there and is not a file, or cannot be read, is an installation gone wrong and refused.
//
// A copy that is already there with these bytes is left as it is, and its mode set again; one
// that differs, as after an upgrade, is replaced by a rename, so a container that has the old copy
// bound keeps the file it was given.
func LayHelper(src, workDir string) (string, bool, error) {
	in, err := os.Open(src)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("runner: %s, the static helper, cannot be read: %w", src, err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return "", false, fmt.Errorf("runner: %s, the static helper, cannot be read: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("runner: %s is where the static helper is installed, and it is not a file: remove it, or install the agk-helper of this release there", src)
	}
	want, err := io.ReadAll(in)
	if err != nil {
		return "", false, fmt.Errorf("runner: %s, the static helper, cannot be read: %w", src, err)
	}

	dir := filepath.Join(workDir, HelperDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, fmt.Errorf("runner: the static helper cannot be laid down under the work root: %w", err)
	}
	dst := filepath.Join(dir, helperName)
	// Lstat rather than Stat, so that a link left under that name is replaced rather than
	// followed and compared.
	if info, err := os.Lstat(dst); err == nil && info.Mode().IsRegular() {
		if got, err := os.ReadFile(dst); err == nil && bytes.Equal(got, want) {
			// The mode is set again rather than trusted, because a copy nobody may
			// execute is one that makes every script step say agk: not found.
			if err := os.Chmod(dst, helperMode); err != nil {
				return "", false, fmt.Errorf("runner: %s cannot be made executable: %w", dst, err)
			}
			return dst, true, nil
		}
	}

	tmp, err := os.CreateTemp(dir, ".agk-*")
	if err != nil {
		return "", false, fmt.Errorf("runner: the static helper cannot be laid down under the work root: %w", err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(want)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), helperMode)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		return "", false, fmt.Errorf("runner: the static helper cannot be laid down at %s: %w", dst, err)
	}
	return dst, true, nil
}
