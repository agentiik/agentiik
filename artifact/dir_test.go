package artifact_test

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
)

func TestDirHoldsAnObjectUnderItsKey(t *testing.T) {
	root := t.TempDir()
	objects := artifact.Dir(root)
	key := artifact.Key("acme", sha256OfHello)

	switch held, err := objects.Has(t.Context(), key); {
	case err != nil:
		t.Fatalf("Has: %v", err)
	case held:
		t.Fatal("Has: an empty directory holds an object")
	}

	if _, err := objects.Open(t.Context(), key); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open of an absent key: got %v, want a missing object", err)
	}

	if err := objects.Put(t.Context(), key, strings.NewReader("hello")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	switch held, err := objects.Has(t.Context(), key); {
	case err != nil:
		t.Fatalf("Has: %v", err)
	case !held:
		t.Fatal("Has: the object just written is not held")
	}

	rc, err := objects.Open(t.Context(), key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("read %q, want %q", got, "hello")
	}

	// The key is a path under the root, so the objects of two namespaces sit in two
	// directories and neither can be reached by asking for the other's digest.
	path := filepath.Join(root, "acme", "sha256", sha256OfHello)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the object is not at %s: %v", path, err)
	}
	// An object is its digest, so its bytes never change again.
	if info.Mode().Perm()&0o222 != 0 {
		t.Errorf("mode: got %v, want an object nothing can write to", info.Mode().Perm())
	}
}

func TestDirWritesAKeyItAlreadyHoldsAgain(t *testing.T) {
	objects := artifact.Dir(t.TempDir())
	key := artifact.Key("acme", sha256OfHello)
	for range 2 {
		// Identical bytes under one key is the ordinary case here rather than a race to
		// be avoided, and the second write finds a read-only object in its way.
		if err := objects.Put(t.Context(), key, strings.NewReader("hello")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
}

func TestDirHoldsUpUnderConcurrentWritesOfOneKey(t *testing.T) {
	objects := artifact.Dir(t.TempDir())
	key := artifact.Key("acme", sha256OfHello)

	// Two steps computing identical bytes is the ordinary case here rather than a race
	// to be avoided, and a reader sees the whole object under the key or nothing at all.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := objects.Put(t.Context(), key, strings.NewReader("hello")); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Put: %v", err)
	}

	rc, err := objects.Open(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("the object holds %q", got)
	}
}

func TestDirRefusesAKeyThatLeavesItsRoot(t *testing.T) {
	root := t.TempDir()
	objects := artifact.Dir(root)
	tests := []string{
		"",
		"/acme/sha256/" + sha256OfHello,
		"../escape",
		"acme/../../escape",
		"acme//sha256/" + sha256OfHello,
		"acme/sha256/..",
	}
	for _, key := range tests {
		t.Run(key, func(t *testing.T) {
			if err := objects.Put(t.Context(), key, strings.NewReader("hello")); err == nil {
				t.Errorf("Put: accepted %q", key)
			}
			if _, err := objects.Has(t.Context(), key); err == nil {
				t.Errorf("Has: accepted %q", key)
			}
			if _, err := objects.Open(t.Context(), key); err == nil {
				t.Errorf("Open: accepted %q", key)
			}
		})
	}
}

// TestStoreOverADirectory is the shape agk run --local takes: a store with no object
// store behind it, only a directory.
func TestStoreOverADirectory(t *testing.T) {
	root := t.TempDir()
	s, err := artifact.New(artifact.Dir(root), "acme", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	file, err := s.Put(t.Context(), uri("normalize", "ok", "greeting.txt"), "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if file.SHA256 != sha256OfHello {
		t.Fatalf("SHA256: got %q", file.SHA256)
	}
	onDisk, err := os.ReadFile(filepath.Join(root, "acme", "sha256", file.SHA256))
	if err != nil {
		t.Fatalf("the object is not under its physical key: %v", err)
	}
	if string(onDisk) != "hello" {
		t.Fatalf("the object holds %q", onDisk)
	}

	rc, err := s.Open(t.Context(), file)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("read %q", got)
	}
}
