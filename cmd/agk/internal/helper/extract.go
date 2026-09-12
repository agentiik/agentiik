package helper

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// helperMode is what the laid down binary is written with: readable and executable by
// everybody, writable by nobody.
//
// A container runs as the account its image declares, which this side never resolves, so a
// mode naming an account would be a program some images cannot execute. It is not writable
// because the file is bound read-only anyway and a writable copy of a binary in a working
// directory is a thing somebody can replace between the check and the mount.
const helperMode = 0o555

// Extract lays down the embedded binary for one platform and answers with its path.
//
// Extracting rather than binding the bytes directly is not avoidable: a bind mount needs a path
// on the host and bytes inside a binary have none.
//
// It is written once per directory rather than once per run, because the bytes are the same
// every time and a run that rewrote them would be a run racing the run beside it. A file that
// is already there and already these bytes is left alone; one that differs is replaced, which
// is what an upgraded agk laying down an older copy's path has to do.
//
// A build that carries no binary for this platform answers ok false and no error. That is the
// documented behaviour and not a failure: the helper "is a convenience, never a requirement",
// so nothing is bound and the caller says so once.
func Extract(dir, ostype, arch string) (string, bool, error) {
	name := carriedName(ostype, arch)
	if name == "" {
		return "", false, nil
	}
	want, err := carried.ReadFile(embeddedDir + "/" + name)
	if err != nil {
		// Nothing at that name is a build that skipped the helper stage, or a daemon
		// whose platform this release does not ship for.
		return "", false, nil
	}
	if dir == "" {
		return "", false, fmt.Errorf("helper: there is nowhere to lay %s down: it is extracted into the run's working directory, because a bind mount needs a path on the host", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, fmt.Errorf("helper: %s could not be prepared: %w", dir, err)
	}

	path := filepath.Join(dir, name)
	if got, err := os.ReadFile(path); err == nil && bytes.Equal(got, want) {
		// Already there and already these bytes. The mode is set again rather than
		// trusted, because a copy nobody may execute is a copy that makes every script
		// step say agk: not found.
		if err := os.Chmod(path, helperMode); err != nil {
			return "", false, fmt.Errorf("helper: %s could not be made executable: %w", path, err)
		}
		return path, true, nil
	}

	// Through a temporary file and a rename, because the path may be being read by a run
	// beside this one: a rename is the one write that is not half done.
	tmp, err := os.CreateTemp(dir, name+".*")
	if err != nil {
		return "", false, fmt.Errorf("helper: %s could not be written: %w", path, err)
	}
	temporary := tmp.Name()
	if _, err := tmp.Write(want); err != nil {
		tmp.Close()
		os.Remove(temporary)
		return "", false, fmt.Errorf("helper: %s could not be written: %w", path, err)
	}
	if err := tmp.Chmod(helperMode); err != nil {
		tmp.Close()
		os.Remove(temporary)
		return "", false, fmt.Errorf("helper: %s could not be made executable: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(temporary)
		return "", false, fmt.Errorf("helper: %s could not be written: %w", path, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		os.Remove(temporary)
		return "", false, fmt.Errorf("helper: %s could not be written: %w", path, err)
	}
	return path, true, nil
}

// carriedBytes is the embedded binary for one platform, for a test that wants to know what is
// being laid down without laying it down.
func carriedBytes(ostype, arch string) ([]byte, error) {
	name := carriedName(ostype, arch)
	if name == "" {
		return nil, fs.ErrNotExist
	}
	return carried.ReadFile(embeddedDir + "/" + name)
}
