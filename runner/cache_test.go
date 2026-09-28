package runner

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/driver"
)

// aCommit is the tree the tests below run one commit of: an entry point, a script that has to run
// and a second name for the entry point's bytes.
func aCommit() map[string]file {
	return map[string]file{
		"agentiik.yaml":      {"apiVersion: agentiik.dev/v1\nkind: Workflow\n", "0644"},
		"scripts/run.sh":     {"#!/bin/sh\necho run\n", "0755"},
		"fragments/copy.yml": {"apiVersion: agentiik.dev/v1\nkind: Workflow\n", "0644"},
	}
}

// anotherStep is the same message, of the same run and commit, for another step.
func anotherStep(m bus.TaskMessage, step string) bus.TaskMessage {
	m.IdempotencyKey, m.Step = string(storeRun)+"/"+step+"/1", step
	return m
}

// "Trees are content-addressed and cached on the runner by commit, so a fleet fetches a commit
// once, not once per task." A second task of a commit this runner has laid out fetches nothing, and
// its tree is the same files, not copies of them. An agent that starts again keeps what it had.
func TestASecondTaskOfTheSameCommitFetchesNothing(t *testing.T) {
	s := newObjectStore(t)
	m, r := s.taskFor(t, nil, aCommit(), nil)
	work := t.TempDir()

	first, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: work})
	if err != nil {
		t.Fatal(err)
	}
	// One request for each distinct object, the entry point's twice-named bytes once.
	if n := s.gets.Load(); n != 2 {
		t.Fatalf("the first task fetched %d objects for two distinct ones", n)
	}

	second, err := Assemble(t.Context(), anotherStep(m, "archive"), r, Assembly{WorkRoot: work})
	if err != nil {
		t.Fatal(err)
	}
	if n := s.gets.Load(); n != 2 {
		t.Errorf("the second task of the commit fetched %d objects more", n-2)
	}
	for path := range aCommit() {
		a, err := os.Stat(filepath.Join(first.Sources.Repo, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.Stat(filepath.Join(second.Sources.Repo, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(a, b) {
			t.Errorf("%s is two files in two trees of one commit", path)
		}
	}

	// Once the first task is over, its tree goes and the second's stays whole.
	if err := first.Remove(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(second.Sources.Repo, "scripts", "run.sh")); err != nil || string(b) != "#!/bin/sh\necho run\n" {
		t.Errorf("the second tree reads %q once the first is gone: %v", b, err)
	}

	// An agent that starts again reads the cache off the disk rather than fetching it anew.
	forgetCaches(t)
	if _, err := Assemble(t.Context(), anotherStep(m, "report"), r, Assembly{WorkRoot: work}); err != nil {
		t.Fatal(err)
	}
	if n := s.gets.Load(); n != 2 {
		t.Errorf("a task assembled after a restart fetched %d objects more", n-2)
	}
}

// Tasks of one commit assembled at once, as a runner holding several does, fetch each object once
// between them: the second to want one waits for the first to put it in place.
func TestTasksOfOneCommitAssembledAtOnceFetchEachFileOnce(t *testing.T) {
	s := newObjectStore(t)
	m, r := s.taskFor(t, nil, aCommit(), nil)
	work := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() {
			_, err := Assemble(t.Context(), anotherStep(m, fmt.Sprintf("shard-%d", i)), r, Assembly{WorkRoot: work})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := s.gets.Load(); n != 2 {
		t.Errorf("eight tasks of one commit fetched %d objects for two distinct ones", n)
	}
}

// "Keep a cached tree inside its namespace." Two namespaces committing the same bytes each fetch
// them with their own grant and keep them apart: a task of one is never laid out from a file the
// other fetched.
func TestTwoNamespacesNeverShareACachedFile(t *testing.T) {
	s := newObjectStore(t)
	work := t.TempDir()
	m, r := s.taskFor(t, nil, aCommit(), nil)
	finance, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: work})
	if err != nil {
		t.Fatal(err)
	}
	before := s.gets.Load()

	om, or := s.taskIn(t, "ops", aCommit())
	ops, err := Assemble(t.Context(), om, or, Assembly{WorkRoot: work})
	if err != nil {
		t.Fatal(err)
	}
	if n := s.gets.Load() - before; n != 2 {
		t.Errorf("ops fetched %d objects for its two, and finance held them all", n)
	}
	for path := range aCommit() {
		a, err := os.Stat(filepath.Join(finance.Sources.Repo, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.Stat(filepath.Join(ops.Sources.Repo, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(a, b) {
			t.Errorf("%s of ops is the file finance fetched", path)
		}
	}
	for _, ns := range []string{"finance", "ops"} {
		held, _ := os.ReadDir(filepath.Join(work, TreesDir, ns, objectsDir))
		if len(held) != 3 {
			t.Errorf("the cache of %s holds %d files, and its trees name two objects, one of them executable too", ns, len(held))
		}
	}
}

// "A file in the tree is bytes, never a template: no interpolation, no expression evaluation, no
// per-run rendering." A file written like an expression is laid out as it was committed, byte for
// byte, placed elsewhere as much as under /agk/repo.
func TestATreeFileIsLaidOutAsTheBytesThatWereCommitted(t *testing.T) {
	const committed = "region: ${{ inputs.region }}\nkey: ${{ secrets.billing }}\nparam: $AGK_PARAM_ENDPOINT\n\x00\xff\r\n"
	s := newObjectStore(t)
	m, r := s.taskFor(t, nil, map[string]file{"config/template.yaml": {committed, "0644"}}, nil)
	m.Params = map[string]any{"endpoint": "https://api.example.com"}
	m.Files = []bus.File{{From: "config/template.yaml"}, {From: "./config/template.yaml", To: "/etc/app/template.yaml"}}
	placed := r.Tree[0]
	placed.To = "/etc/app/template.yaml"
	r.Tree = append(r.Tree, placed)

	a, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(a.Sources.Repo, "config", "template.yaml"))
	if err != nil || !bytes.Equal(b, []byte(committed)) {
		t.Errorf("the tree reads %q: %v", b, err)
	}
	if len(a.Sources.Placed) != 1 {
		t.Fatalf("placed %+v", a.Sources.Placed)
	}
	if b, err := os.ReadFile(a.Sources.Placed[0].Source); err != nil || !bytes.Equal(b, []byte(committed)) {
		t.Errorf("the placed file reads %q: %v", b, err)
	}
}

// A file the long form places elsewhere is laid out for the driver to copy, with the mode its
// entry carries, and not under /agk/repo, where the step asked for it somewhere else instead.
func TestAPlacedFileIsLaidOutForTheDriverAndNotUnderTheRepository(t *testing.T) {
	s := newObjectStore(t)
	m, r := s.taskFor(t, nil, map[string]file{
		"sql/orders.sql":        {"select 1;\n", "0644"},
		"certs/internal-ca.pem": {"-----BEGIN CERTIFICATE-----\n", "0644"},
	}, nil)
	m.Files = []bus.File{{From: "./sql/**"}, {From: "./certs/internal-ca.pem", To: "/etc/ssl/certs/internal-ca.pem", Mode: "0444"}}
	for i, e := range r.Tree {
		if e.Path == "certs/internal-ca.pem" {
			r.Tree[i].To, r.Tree[i].Mode = "/etc/ssl/certs/internal-ca.pem", "0444"
		}
	}

	a, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.Sources.Repo, "certs")); !os.IsNotExist(err) {
		t.Errorf("the relocated certificate is under /agk/repo too: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.Sources.Repo, "sql", "orders.sql")); err != nil {
		t.Errorf("the SQL is not under /agk/repo: %v", err)
	}
	want := []driver.Placed{{Source: filepath.Join(filepath.Dir(a.Sources.Repo), placedDir, sha([]byte("-----BEGIN CERTIFICATE-----\n"))), To: "/etc/ssl/certs/internal-ca.pem", Mode: "0444"}}
	if !slices.Equal(a.Sources.Placed, want) {
		t.Errorf("placed %+v, want %+v", a.Sources.Placed, want)
	}
	if b, err := os.ReadFile(want[0].Source); err != nil || string(b) != "-----BEGIN CERTIFICATE-----\n" {
		t.Errorf("the placed file reads %q: %v", b, err)
	}

	// And a redemption placing nothing places nothing, which leaves the step's files to the
	// driver, as on a laptop.
	plain, rp := s.taskFor(t, nil, map[string]file{"sql/orders.sql": {"select 1;\n", "0644"}}, nil)
	b, err := Assemble(t.Context(), plain, rp, Assembly{WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if b.Sources.Placed != nil {
		t.Errorf("a redemption placing nothing placed %+v", b.Sources.Placed)
	}
}

// The cache keeps to its bound: past it, the files used least recently go, and a tree laid out
// from one of them keeps reading it, since its link is its own. The next task of that commit
// fetches it again.
func TestTheCacheLetsTheLeastRecentlyUsedGoPastItsBound(t *testing.T) {
	s := newObjectStore(t)
	work := t.TempDir()
	cache := treeCacheAt(work)
	cache.bound = 64

	old, ro := s.taskFor(t, nil, map[string]file{"old.txt": {string(bytes.Repeat([]byte("o"), 40)), "0644"}}, nil)
	kept, err := Assemble(t.Context(), old, ro, Assembly{WorkRoot: work})
	if err != nil {
		t.Fatal(err)
	}
	fresh, rf := s.taskFor(t, nil, map[string]file{"new.txt": {string(bytes.Repeat([]byte("n"), 40)), "0644"}}, nil)
	fresh = anotherStep(fresh, "archive")
	if _, err := Assemble(t.Context(), fresh, rf, Assembly{WorkRoot: work}); err != nil {
		t.Fatal(err)
	}

	held, _ := os.ReadDir(filepath.Join(work, TreesDir, "finance", objectsDir))
	if len(held) != 1 || held[0].Name() != rf.Tree[0].SHA256 {
		t.Errorf("past its bound the cache holds %v, and the newer file alone fits", held)
	}
	if cache.total > cache.bound {
		t.Errorf("the cache counts %d bytes, past its bound of %d", cache.total, cache.bound)
	}
	if b, err := os.ReadFile(filepath.Join(kept.Sources.Repo, "old.txt")); err != nil || len(b) != 40 {
		t.Errorf("a tree laid out from a file the cache let go reads %q: %v", b, err)
	}

	before := s.gets.Load()
	if _, err := Assemble(t.Context(), anotherStep(old, "report"), ro, Assembly{WorkRoot: work}); err != nil {
		t.Fatal(err)
	}
	if n := s.gets.Load() - before; n != 1 {
		t.Errorf("a task of the commit whose file went fetched %d objects", n)
	}
}

// What an agent that stopped halfway through a fetch left is taken away at the next start, and
// the file it was fetching is fetched whole.
func TestAFetchCutShortLeavesNothingInTheCache(t *testing.T) {
	s := newObjectStore(t)
	work := t.TempDir()
	m, r := s.taskFor(t, nil, map[string]file{"agentiik.yaml": {"version: 1\n", "0644"}}, nil)
	objects := filepath.Join(work, TreesDir, "finance", objectsDir)
	if err := os.MkdirAll(objects, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(objects, tempPrefix+"123")
	if err := os.WriteFile(stale, []byte("vers"), 0o600); err != nil {
		t.Fatal(err)
	}
	forgetCaches(t)
	a, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: work})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a fetch cut short is still there: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(a.Sources.Repo, "agentiik.yaml")); err != nil || string(b) != "version: 1\n" {
		t.Errorf("the tree reads %q: %v", b, err)
	}
}

// forgetCaches is an agent starting again: the process holds no cache, and reads each off the disk.
func forgetCaches(t *testing.T) {
	t.Helper()
	cachesMu.Lock()
	defer cachesMu.Unlock()
	clear(caches)
}

// taskIn is taskFor in another namespace: the tree stored under its keys, the URLs and the upload
// policy minted for it.
func (s *objectStore) taskIn(t *testing.T, namespace string, tree map[string]file) (bus.TaskMessage, Redemption) {
	t.Helper()
	m, r := s.taskFor(t, nil, nil, nil)
	m.Namespace = namespace
	p, err := s.signed.Policy(t.Context(), namespace, storeRun, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	r.Uploads = Uploads{URL: p.URL, Fields: p.Fields, KeyPrefix: p.KeyPrefix}
	for path, f := range tree {
		d := sha([]byte(f.content))
		if err := s.signed.Store(t.Context(), artifact.Key(namespace, d), bytes.NewReader([]byte(f.content))); err != nil {
			t.Fatal(err)
		}
		u, err := s.signed.Presign(t.Context(), artifact.MethodGet, artifact.Key(namespace, d), storeRun, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		r.Tree = append(r.Tree, TreeEntry{Path: path, Mode: f.mode, SHA256: d, URL: u})
	}
	return m, r
}

// A file of the cache named as many times as its filesystem allows is copied rather than linked
// once more, with its mode, so a tree is laid out whole however many trees name the same bytes.
func TestAFileOutOfLinksIsCopiedIntoTheTree(t *testing.T) {
	s := newObjectStore(t)
	m, r := s.taskFor(t, nil, aCommit(), nil)
	hardLink = func(string, string) error { return &os.LinkError{Op: "link", Err: syscall.EMLINK} }
	t.Cleanup(func() { hardLink = os.Link })

	a, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for path, f := range aCommit() {
		name := filepath.Join(a.Sources.Repo, filepath.FromSlash(path))
		b, err := os.ReadFile(name)
		if err != nil || string(b) != f.content {
			t.Errorf("%s reads %q: %v", path, b, err)
		}
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		want := treeFileMode
		if f.mode == "0755" {
			want = treeExecMode
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s is %o, want %o", path, info.Mode().Perm(), want)
		}
	}
}

// A file a tree being laid out holds is not taken away by the eviction another layout runs, even
// past the bound: it is on its way into a tree.
func TestAFileHeldForALayoutSurvivesAnotherLayoutsEviction(t *testing.T) {
	s := newObjectStore(t)
	work := t.TempDir()
	cache := treeCacheAt(work)
	cache.bound = 64

	m, r := s.taskFor(t, nil, map[string]file{"old.txt": {string(bytes.Repeat([]byte("o"), 40)), "0644"}}, nil)
	o, err := objectsOf(m.Namespace, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	at, release, err := cache.hold(t.Context(), o, m.Namespace, []cachedObject{{objectKey: objectKey{digest: r.Tree[0].SHA256}, path: "old.txt"}}, agk.DefaultLimits())
	defer release()
	if err != nil {
		t.Fatal(err)
	}
	fresh, rf := s.taskFor(t, nil, map[string]file{"new.txt": {string(bytes.Repeat([]byte("n"), 40)), "0644"}}, nil)
	if _, err := Assemble(t.Context(), anotherStep(fresh, "archive"), rf, Assembly{WorkRoot: work}); err != nil {
		t.Fatal(err)
	}
	for _, p := range at {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("a file held for a layout went: %v", err)
		}
	}
}
