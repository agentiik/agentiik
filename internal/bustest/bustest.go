// Package bustest holds what a test starting a NATS server of its own needs and the server does
// not give it: a JetStream store it can take back once the server has stopped.
package bustest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// StoreDir answers a directory for the JetStream store of a server the test starts, and takes it
// back when the test ends.
//
// Call it before the server's Shutdown is registered with t.Cleanup, so that the directory is
// taken back after the server has stopped: cleanups run last first.
//
// t.TempDir alone fails now and then with "directory not empty", on a test that passed. Shutdown
// does not wait for a consumer's flusher, which is a goroutine nothing joins: the consumer store's
// Stop waits at most 100 ms for it, and not at all once it has taken the state it is writing. So
// the state file a test's last acknowledgement caused can be created after Shutdown returned,
// inside a directory the removal has already listed, and the removal fails on it. There is nothing
// to wait for, so the store is moved aside in one rename instead: a write that came before the
// rename is moved with the rest and removed with it, and one that comes after finds its directory
// gone and fails, which is what a write to a stopped server should do.
func StoreDir(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	dir := filepath.Join(parent, "store")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Into the same temporary directory, whose own cleanup, which runs after this one,
	// removes what was moved there.
	t.Cleanup(func() {
		if err := os.Rename(dir, filepath.Join(parent, "stopped")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the JetStream store could not be moved aside to be removed: %v", err)
		}
	})
	return dir
}
