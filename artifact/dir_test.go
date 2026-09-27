package artifact_test

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
			if _, err := objects.Remove(t.Context(), key); err == nil {
				t.Errorf("Remove: accepted %q", key)
			}
		})
	}
}

// Remove deletes an object, answers nil for one already gone, and takes with its last object each
// directory the key made below its first two segments: a log's run, task and dispatch go, and the
// namespace's logs and sha256 directories stay, as does a directory still holding something.
func TestDirRemovesAnObjectAndTheDirectoriesItLeavesEmpty(t *testing.T) {
	root := t.TempDir()
	objects := artifact.Dir(root)
	artifactKey := artifact.Key("acme", sha256OfHello)
	logs := []string{
		"acme/logs/run-1/task-a/dispatch-1/0000000001-" + sha256OfHello,
		"acme/logs/run-1/task-a/dispatch-1/0000000002-" + sha256OfHello,
		"acme/logs/run-1/task-b/dispatch-1/0000000001-" + sha256OfHello,
	}
	for _, key := range append([]string{artifactKey}, logs...) {
		if err := objects.Put(t.Context(), key, strings.NewReader("hello")); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		return err == nil
	}

	if removed, err := objects.Remove(t.Context(), artifactKey); err != nil || !removed {
		t.Fatalf("removing an object answered %t, %v", removed, err)
	}
	if exists(artifactKey) || !exists("acme/sha256") {
		t.Errorf("after removing the only artifact: the object held %t, acme/sha256 held %t", exists(artifactKey), exists("acme/sha256"))
	}
	if removed, err := objects.Remove(t.Context(), artifactKey); err != nil || removed {
		t.Errorf("removing an object already gone answered %t, %v", removed, err)
	}

	if _, err := objects.Remove(t.Context(), logs[0]); err != nil {
		t.Fatal(err)
	}
	if exists(logs[0]) || !exists("acme/logs/run-1/task-a/dispatch-1") {
		t.Error("removing one chunk of two took the other's directory, or left the chunk")
	}
	if _, err := objects.Remove(t.Context(), logs[1]); err != nil {
		t.Fatal(err)
	}
	if exists("acme/logs/run-1/task-a") || !exists("acme/logs/run-1/task-b/dispatch-1") {
		t.Error("removing a task's last chunk left its directories, or took another task's")
	}
	if _, err := objects.Remove(t.Context(), logs[2]); err != nil {
		t.Fatal(err)
	}
	if exists("acme/logs/run-1") || !exists("acme/logs") {
		t.Errorf("removing a run's last chunk: the run's directory held %t, acme/logs held %t", exists("acme/logs/run-1"), exists("acme/logs"))
	}
}

// A walk answers the objects of one namespace, each once, a batch at a time, with its size and when
// it was written, and nothing else in its directory: not a write being staged, a link, a directory,
// a name that is not a digest, a log or another namespace's object.
func TestDirWalksTheObjectsOfOneNamespaceAndNothingElse(t *testing.T) {
	root := t.TempDir()
	objects := artifact.Dir(root)
	walkable, ok := objects.(artifact.Walkable)
	if !ok {
		t.Fatal("the directory store cannot be walked")
	}
	want := map[string]int64{}
	for i := range 5 {
		content := strings.Repeat("x", i+1)
		digest := fmt.Sprintf("%064x", i+1)
		if err := objects.Put(t.Context(), artifact.Key("acme", digest), strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
		want[digest] = int64(len(content))
	}
	old := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	aged := fmt.Sprintf("%064x", 1)
	if err := os.Chtimes(filepath.Join(root, "acme", "sha256", aged), old, old); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		artifact.Key("other", fmt.Sprintf("%064x", 9)),
		"acme/logs/run-1/task-a/dispatch-1/0000000001-" + sha256OfHello,
	} {
		if err := objects.Put(t.Context(), key, strings.NewReader("not acme's object")); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(root, "acme", "sha256")
	for _, name := range []string{".staging-123", "README", strings.Repeat("A", 64)} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not an object"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, fmt.Sprintf("%064x", 7)), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("somebody else's"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, fmt.Sprintf("%064x", 8))); err != nil {
		t.Fatal(err)
	}

	walk, err := walkable.Walk("acme")
	if err != nil {
		t.Fatal(err)
	}
	defer walk.Close()
	got := map[string]artifact.Stored{}
	for calls := 0; ; calls++ {
		if calls > 20 {
			t.Fatal("the walk never ended")
		}
		found, more, err := walk.Next(t.Context(), 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(found) > 2 {
			t.Fatalf("a walk asked for 2 answered %d", len(found))
		}
		for _, s := range found {
			if _, twice := got[s.Digest]; twice {
				t.Errorf("%s was answered twice", s.Digest)
			}
			got[s.Digest] = s
		}
		if !more {
			break
		}
	}
	if len(got) != len(want) {
		t.Errorf("the walk answered %d objects, and acme holds %d: %v", len(got), len(want), got)
	}
	for digest, size := range want {
		if s, ok := got[digest]; !ok || s.Size != size {
			t.Errorf("the object %s of %d bytes was answered as %+v", digest, size, s)
		}
	}
	if !got[aged].Written.Equal(old) {
		t.Errorf("an object written three days ago was answered as written %s", got[aged].Written)
	}

	none, err := walkable.Walk("nobody")
	if err != nil {
		t.Fatal(err)
	}
	if found, more, err := none.Next(t.Context(), 10); len(found) != 0 || more || err != nil {
		t.Errorf("a namespace holding no object answered %v, %t, %v", found, more, err)
	}
	for _, namespace := range []string{"", "..", "a/b"} {
		if _, err := walkable.Walk(namespace); err == nil {
			t.Errorf("a walk of namespace %q was opened", namespace)
		}
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
