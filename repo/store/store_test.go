package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/repo"
)

// A repository's packs in the object store, against the git binary and a real PostgreSQL: what git
// pushes is unpacked, written under the repository's keys after its row, and every object read back
// through the live packs as git holds it, a thin push's deltas resolved against the packs already
// there.

// git runs the git binary in dir with no configuration but its own and at a fixed time, and answers
// what it wrote.
func git(t *testing.T, dir string, stdin []byte, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "HOME="+dir, "LC_ALL=C",
		"GIT_AUTHOR_NAME=Ada Lovelace", "GIT_AUTHOR_EMAIL=ada@example.com", "GIT_AUTHOR_DATE=1700000000 +0100",
		"GIT_COMMITTER_NAME=Grace Hopper", "GIT_COMMITTER_EMAIL=grace@example.com", "GIT_COMMITTER_DATE=1700000100 -0000",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %s: %s", strings.Join(args, " "), err, stderr.Bytes())
	}
	return out
}

// history is a repository of two commits, the second changing one line of a long file, so that git
// sends it as a delta against the first.
func history(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, nil, "init", "-q", "-b", "main")
	var long strings.Builder
	for i := range 400 {
		fmt.Fprintf(&long, "line %d of a file long enough to be sent as a delta\n", i)
	}
	write := func(name, content string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("agentiik.yaml", "kind: Workflow\n")
	write("scripts/long.txt", long.String())
	git(t, dir, nil, "add", ".")
	git(t, dir, nil, "commit", "-q", "-m", "first")
	write("scripts/long.txt", strings.Replace(long.String(), "line 200 ", "line two hundred ", 1))
	git(t, dir, nil, "commit", "-q", "-am", "second")
	return dir
}

// objectsOf answers every object revs reach, by ID, with its content as git holds it.
func objectsOf(t *testing.T, dir string, revs ...string) map[repo.ID][]byte {
	t.Helper()
	out := map[repo.ID][]byte{}
	for _, line := range strings.Split(strings.TrimSpace(string(git(t, dir, nil, append([]string{"rev-list", "--objects"}, revs...)...))), "\n") {
		hexID, _, _ := strings.Cut(line, " ")
		id, err := repo.ParseID(hexID)
		if err != nil {
			t.Fatal(err)
		}
		kind := strings.TrimSpace(string(git(t, dir, nil, "cat-file", "-t", hexID)))
		out[id] = git(t, dir, nil, "cat-file", kind, hexID)
	}
	return out
}

// pushed is what a push leaves before it writes anything: the pack Unpack wrote, in a file.
type pushed struct {
	file *os.File
	size int64
	u    *repo.Unpacked
}

// unpack reads a pack git made as a push would, its thin deltas resolved against bases, and writes
// the pack of whole objects Put stores.
func unpack(t *testing.T, pack []byte, bases repo.Lookup) pushed {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "pack-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { out.Close() })
	u, err := repo.Unpack(t.Context(), bytes.NewReader(pack), int64(len(pack)), out, repo.UnpackOptions{Bases: bases})
	if err != nil {
		t.Fatalf("the pack git made could not be unpacked: %s", err)
	}
	size, err := out.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	return pushed{file: out, size: size, u: u}
}

// installation is a store over a directory and a database holding the namespace finance and its
// workflow nightly, empty.
type installation struct {
	dir   string
	pool  *db.Pool
	store *Store
}

func anInstallation(t *testing.T) *installation {
	t.Helper()
	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
		t.Fatal(err)
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, n *db.NS) error {
		return n.SaveWorkflow(ctx, "nightly", "main")
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s, err := New(artifact.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	return &installation{dir: dir, pool: pool, store: s}
}

// live makes a pack Put wrote live, as the push's transaction does, and answers the repository.
func (in *installation) live(t *testing.T, names ...string) db.Repository {
	t.Helper()
	var r db.Repository
	if err := in.pool.In(t.Context(), "finance", func(ctx context.Context, n *db.NS) error {
		for _, name := range names {
			if err := n.PackLive(ctx, "nightly", name); err != nil {
				return err
			}
		}
		var err error
		r, err = n.Repository(ctx, "nightly")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (in *installation) put(t *testing.T, p pushed) db.Pack {
	t.Helper()
	written, err := in.store.Put(t.Context(), in.pool, "finance", "nightly", p.file, p.size, p.u)
	if err != nil {
		t.Fatal(err)
	}
	return written
}

// holds is whether objects serves every one of want as git holds it.
func holds(t *testing.T, objects *Objects, want map[repo.ID][]byte) {
	t.Helper()
	for id, content := range want {
		_, got, err := repo.ReadObject(t.Context(), objects, id, 1<<20)
		if err != nil {
			t.Errorf("%s: %s", id, err)
			continue
		}
		if !bytes.Equal(got, content) {
			t.Errorf("%s reads as %q, and git holds %q", id, got, content)
		}
	}
}

// A first push, and a thin push after it made against what the first left: each is written under the
// repository's keys, and once both are live every object of the history is read back as git holds
// it, the second push's delta having been resolved against the first's pack in the store.
func TestAPushIsKeptAsAPackAndReadBackThroughItsIndex(t *testing.T) {
	in := anInstallation(t)
	dir := history(t)

	first := unpack(t, git(t, dir, []byte("HEAD~1\n"), "pack-objects", "--revs", "--stdout", "-q"), nil)
	one := in.put(t, first)
	if one.Name != hex.EncodeToString(first.u.Checksum[:]) || one.Size != first.size || one.Objects != len(first.u.Objects) {
		t.Errorf("the first pack was recorded as %+v", one)
	}
	r := in.live(t, one.Name)
	for _, key := range []string{PackKey("finance", r.Key, one.Name), IdxKey("finance", r.Key, one.Name)} {
		if !strings.HasPrefix(key, "finance/git/"+r.Key+"/pack-"+one.Name+".") {
			t.Errorf("a key of the pack is %s", key)
		}
		if _, err := os.Stat(filepath.Join(in.dir, filepath.FromSlash(key))); err != nil {
			t.Errorf("the pack's %s is not in the store: %s", key, err)
		}
	}

	objects, err := in.store.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	thin := git(t, dir, []byte("HEAD\n^HEAD~1\n"), "pack-objects", "--revs", "--thin", "--stdout", "-q")
	if _, err := repo.Unpack(t.Context(), bytes.NewReader(thin), int64(len(thin)), io.Discard, repo.UnpackOptions{}); err == nil {
		t.Fatalf("the second push, unpacked against nothing, answered %v, and it is a delta against the first", err)
	}
	second := unpack(t, thin, objects)
	if err := objects.Close(); err != nil {
		t.Fatal(err)
	}
	two := in.put(t, second)
	r = in.live(t, two.Name)
	if len(r.Packs) != 2 {
		t.Fatalf("the repository lists the packs %+v", r.Packs)
	}

	objects, err = in.store.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	defer objects.Close()
	holds(t, objects, objectsOf(t, dir, "HEAD"))
	head, err := repo.ParseID(strings.TrimSpace(string(git(t, dir, nil, "rev-parse", "HEAD"))))
	if err != nil {
		t.Fatal(err)
	}
	pack, err := objects.Find(t.Context(), head)
	if err != nil || !pack.Has(head) {
		t.Errorf("the pack holding HEAD answered %v", err)
	}
	var absent repo.ID
	absent[0] = 0xab
	if _, err := objects.OpenObject(t.Context(), absent); !errors.Is(err, repo.ErrMissing) {
		t.Errorf("an object no pack holds answered %v", err)
	}
	if held, err := objects.Has(t.Context(), absent); held || err != nil {
		t.Errorf("an object no pack holds is held %t, %v", held, err)
	}
	if held, err := objects.Has(t.Context(), head); !held || err != nil {
		t.Errorf("HEAD is held %t, %v", held, err)
	}

	// Nothing the orphan sweep walks names a pack: git/ is beside sha256/, and never in it.
	walk, err := artifact.Dir(in.dir).(artifact.Walkable).Walk("finance")
	if err != nil {
		t.Fatal(err)
	}
	defer walk.Close()
	if found, _, err := walk.Next(t.Context(), 100); len(found) != 0 || err != nil {
		t.Errorf("the orphan sweep's walk of finance answered %v, %v", found, err)
	}
}

// A pack is written only as the bytes Unpack answered for: bytes of another length, or whose trailer
// is not their checksum, are refused before the key holds them, and the pack is left receiving, which
// the collection takes once it has been past the grace.
func TestAPackIsWrittenOnlyAsTheBytesItsChecksumNames(t *testing.T) {
	in := anInstallation(t)
	dir := history(t)
	p := unpack(t, git(t, dir, []byte("HEAD\n"), "pack-objects", "--revs", "--stdout", "-q"), nil)
	good, err := io.ReadAll(p.file)
	if err != nil {
		t.Fatal(err)
	}
	flipped := bytes.Clone(good)
	flipped[len(flipped)/2] ^= 0xff
	// The one said to be shorter first, so that the pack is recorded receiving at a size not its own.
	for _, c := range []struct {
		what  string
		bytes []byte
		size  int64
	}{
		{"said to be shorter", good, p.size - 1},
		{"a byte changed", flipped, p.size},
		{"one byte short", good[:len(good)-1], p.size},
		{"one byte more", append(bytes.Clone(good), 0), p.size},
		{"its trailer dropped", good[:len(good)-20], p.size - 20},
	} {
		_, err := in.store.Put(t.Context(), in.pool, "finance", "nightly", bytes.NewReader(c.bytes), c.size, p.u)
		if err == nil {
			t.Errorf("a pack with %s was written", c.what)
		}
	}
	name := hex.EncodeToString(p.u.Checksum[:])
	var key string
	if err := in.pool.In(t.Context(), "finance", func(ctx context.Context, n *db.NS) error {
		r, err := n.Repository(ctx, "nightly")
		key = r.Key
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(in.dir, filepath.FromSlash(PackKey("finance", key, name)))); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused pack is in the store: %v", err)
	}
	if r := in.live(t); len(r.Packs) != 0 {
		t.Errorf("a refused pack is live: %+v", r.Packs)
	}

	// The same pack written as it is, once refused for sizes that were not its own, is read back.
	if _, err := p.file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	written := in.put(t, p)
	objects, err := in.store.Open(in.live(t, written.Name))
	if err != nil {
		t.Fatal(err)
	}
	defer objects.Close()
	holds(t, objects, objectsOf(t, dir, "HEAD"))
	if _, err := in.store.Put(t.Context(), in.pool, "finance", "absent", bytes.NewReader(good), p.size, p.u); !errors.Is(err, db.ErrNoWorkflow) {
		t.Errorf("a pack of no workflow answered %v", err)
	}
}

// An index is read once for the process: a second lookup of the same pack reads it from memory, and
// a store of its own, whose memory holds nothing, reads it again. An index that is not the index of
// the pack it is named after, or a pack of another size than recorded, is refused rather than read.
func TestAnIndexIsReadOnceAndOnlyAsItsPacksOwn(t *testing.T) {
	in := anInstallation(t)
	dir := history(t)
	p := in.put(t, unpack(t, git(t, dir, []byte("HEAD\n"), "pack-objects", "--revs", "--stdout", "-q"), nil))
	r := in.live(t, p.Name)
	want := objectsOf(t, dir, "HEAD")
	read := func(s *Store) error {
		objects, err := s.Open(r)
		if err != nil {
			return err
		}
		defer objects.Close()
		for id := range want {
			if _, _, err := repo.ReadObject(t.Context(), objects, id, 1<<20); err != nil {
				return err
			}
		}
		return nil
	}
	if err := read(in.store); err != nil {
		t.Fatal(err)
	}

	idx := filepath.Join(in.dir, filepath.FromSlash(IdxKey("finance", r.Key, p.Name)))
	kept, err := os.ReadFile(idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(idx); err != nil {
		t.Fatal(err)
	}
	if err := read(in.store); err != nil {
		t.Errorf("a pack whose index was read once is not read again without it: %s", err)
	}
	fresh, err := New(artifact.Dir(in.dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := read(fresh); err == nil || !strings.Contains(err.Error(), "not in the store") {
		t.Errorf("a store that never read the index, which is gone, answered %v", err)
	}

	// Another pack's index under this pack's name.
	other := unpack(t, git(t, dir, []byte("HEAD~1\n"), "pack-objects", "--revs", "--stdout", "-q"), nil)
	var wrong bytes.Buffer
	if err := repo.WriteIdx(&wrong, other.u.Objects, other.u.Checksum); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idx, wrong.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := read(fresh); err == nil || !strings.Contains(err.Error(), "is the index of pack") {
		t.Errorf("another pack's index answered %v", err)
	}

	// The right index, and a pack of another size than recorded.
	if err := os.Remove(idx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idx, kept, 0o444); err != nil {
		t.Fatal(err)
	}
	short := r
	short.Packs = []db.Pack{{Name: p.Name, Size: p.Size - 1, Objects: p.Objects}}
	objects, err := fresh.Open(short)
	if err != nil {
		t.Fatal(err)
	}
	defer objects.Close()
	for id := range want {
		if _, err := objects.OpenObject(t.Context(), id); err == nil || !strings.Contains(err.Error(), "was recorded as") {
			t.Errorf("a pack of another size than recorded answered %v", err)
		}
		break
	}
}

// Only a store that reads a range of an object keeps packs, and a repository is read only under keys
// its names make.
func TestAStoreIsOpenedOnlyWhereKeysCanBeMade(t *testing.T) {
	if _, err := New(onlyObjects{artifact.Dir(t.TempDir())}); err == nil {
		t.Error("a store that cannot read a range was opened")
	}
	s, err := New(artifact.Dir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	key, name := strings.Repeat("0", 32), strings.Repeat("1", 40)
	for _, r := range []db.Repository{
		{Namespace: "", Key: key},
		{Namespace: "a/b", Key: key},
		{Namespace: "finance", Key: "../../etc"},
		{Namespace: "finance", Key: key, Packs: []db.Pack{{Name: "../" + name}}},
	} {
		if len(r.Packs) == 0 {
			r.Packs = []db.Pack{{Name: name, Size: 1}}
		}
		if _, err := s.Open(r); err == nil {
			t.Errorf("a repository %+v was opened", r)
		}
	}
}

// onlyObjects is a byte layer with nothing but the three methods.
type onlyObjects struct{ o artifact.Objects }

func (s onlyObjects) Has(ctx context.Context, key string) (bool, error) { return s.o.Has(ctx, key) }
func (s onlyObjects) Put(ctx context.Context, key string, r io.Reader) error {
	return s.o.Put(ctx, key, r)
}
func (s onlyObjects) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.o.Open(ctx, key)
}

// The indexes kept are bounded by bytes, the least recently read let go first, and one larger than
// the bound is never kept.
func TestTheIndexesKeptAreBoundedAndTheOldestGoFirst(t *testing.T) {
	c := newIdxCache(100)
	idx := &repo.Idx{}
	c.add("a", idx, 40)
	c.add("b", idx, 40)
	if _, ok := c.get("a"); !ok {
		t.Fatal("an index kept is not found")
	}
	c.add("c", idx, 40)
	if _, ok := c.get("b"); ok {
		t.Error("the index read least recently was kept past the bound")
	}
	for _, key := range []string{"a", "c"} {
		if _, ok := c.get(key); !ok {
			t.Errorf("%s was let go", key)
		}
	}
	c.add("big", idx, 101)
	if _, ok := c.get("big"); ok {
		t.Error("an index larger than the bound was kept")
	}
	c.add("a", idx, 40)
	if c.used != 80 || len(c.entries) != 2 || c.recent.Len() != 2 {
		t.Errorf("the cache holds %d bytes in %d entries, %d in order", c.used, len(c.entries), c.recent.Len())
	}
}
