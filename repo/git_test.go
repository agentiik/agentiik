package repo

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This package is held to the git binary rather than to a reading of the format: what git writes
// is read here, and what is written here is read by git. The binary is on every machine that
// checked this module out and on the CI runner, so a test that cannot find it fails rather than
// skips, since a skip would be a green run that proved nothing.

// git runs the git binary in dir, handing it stdin, and answers what it wrote to its standard
// output. It runs with no configuration but its own, so that nobody's settings change what it
// writes, and at a fixed time, so that the same commands make the same objects.
func git(t testing.TB, dir string, stdin []byte, args ...string) []byte {
	t.Helper()
	out, err := gitErr(dir, stdin, nil, args...)
	if err != nil {
		t.Fatalf("git %s: %s", strings.Join(args, " "), err)
	}
	return out
}

// gitErr is git, answering its failure rather than failing the test.
func gitErr(dir string, stdin []byte, env []string, args ...string) ([]byte, error) {
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
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "HOME="+dir, "LC_ALL=C", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Ada Lovelace", "GIT_AUTHOR_EMAIL=ada@example.com", "GIT_AUTHOR_DATE=1700000000 +0100",
		"GIT_COMMITTER_NAME=Grace Hopper", "GIT_COMMITTER_EMAIL=grace@example.com", "GIT_COMMITTER_DATE=1700000100 -0000",
	)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, stderr.Bytes())
	}
	return out, nil
}

func gitID(t testing.TB, dir, rev string) ID {
	t.Helper()
	id, err := ParseID(strings.TrimSpace(string(git(t, dir, nil, "rev-parse", rev))))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func write(t testing.TB, dir, name, content string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

// lines is n numbered lines of text, the kind of file whose next version git sends as a delta.
func lines(n int, word string) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "%05d %s: the quick brown fox jumps over the lazy dog\n", i, word)
	}
	return b.String()
}

// sample is a repository with what a workflow repository holds and what git's formats have room
// for: nested directories, an executable, an empty file, names whose order is git's and not a
// directory listing's, files changed from commit to commit so that a pack carries deltas, a merge,
// a symbolic link, a submodule, an annotated tag of a commit and one of a tree, a commit carrying
// an encoding header and one carrying a signature over several lines.
type sample struct {
	dir string
	// first is the first commit, which holds no symbolic link and no submodule; second the one
	// after it, which a thin pack is made against.
	first, second ID
}

func newSample(t testing.TB) *sample {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, nil, "init", "-q", "-b", "main", ".")
	write(t, dir, "agentiik.yaml", "apiVersion: agentiik.io/v1\nkind: Workflow\nmetadata:\n  name: sample\n"+lines(40, "step"), 0o644)
	write(t, dir, "scripts/run.sh", "#!/bin/sh\necho run\n", 0o755)
	write(t, dir, "schemas/order.json", `{"type": "object"}`+"\n", 0o644)
	write(t, dir, "data/big.txt", lines(1500, "row"), 0o644)
	write(t, dir, "deep/a/b/c/d.txt", "deep\n", 0o644)
	write(t, dir, "sort.txt", "a file whose name sorts before the directory sort in git's order\n", 0o644)
	write(t, dir, "sort/x", "x\n", 0o644)
	write(t, dir, "empty", "", 0o644)
	git(t, dir, nil, "add", "-A")
	git(t, dir, nil, "commit", "-q", "-m", "first")
	s := &sample{dir: dir, first: gitID(t, dir, "HEAD")}

	write(t, dir, "data/big.txt", strings.Replace(lines(1500, "row"), "00100 row", "00100 changed", 1)+lines(10, "more"), 0o644)
	write(t, dir, "agentiik.yaml", "apiVersion: agentiik.io/v1\nkind: Workflow\nmetadata:\n  name: sample\n"+lines(41, "step"), 0o644)
	if err := os.Symlink("agentiik.yaml", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, nil, "add", "-A")
	git(t, dir, nil, "commit", "-q", "-m", "second")
	s.second = gitID(t, dir, "HEAD")

	git(t, dir, nil, "checkout", "-q", "-b", "side")
	write(t, dir, "schemas/order.json", `{"type": "object", "required": ["id"]}`+"\n", 0o644)
	write(t, dir, "data/big.txt", strings.Replace(lines(1500, "row"), "00700 row", "00700 side", 1)+lines(10, "more"), 0o644)
	git(t, dir, nil, "commit", "-q", "-am", "side")
	git(t, dir, nil, "checkout", "-q", "main")
	write(t, dir, "data/big.txt", strings.Replace(lines(1500, "row"), "00100 row", "00100 main", 1)+lines(10, "more"), 0o644)
	git(t, dir, nil, "commit", "-q", "-am", "main")
	git(t, dir, nil, "merge", "-q", "-X", "ours", "-m", "merge side", "side")

	// A submodule, as a gitlink to a commit of another repository, which this one does not hold.
	git(t, dir, nil, "update-index", "--add", "--cacheinfo", "160000,0123456789abcdef0123456789abcdef01234567,vendor/sub")
	git(t, dir, nil, "-c", "i18n.commitEncoding=ISO-8859-1", "commit", "-q", "-m", "a submodule, in a commit with an encoding")

	git(t, dir, nil, "tag", "-a", "-m", "the first release", "v1", "HEAD")
	git(t, dir, nil, "tag", "-a", "-m", "a tree, tagged", "tree-tag", "HEAD^{tree}")
	git(t, dir, nil, "tag", "light", "HEAD~1")

	// A signed commit, written by hand since no key is at hand: git reads the signature as a
	// header over several lines, and so does this package.
	signed := fmt.Sprintf("tree %s\nparent %s\nauthor Ada Lovelace <ada@example.com> 1700000000 +0100\ncommitter Grace Hopper <grace@example.com> 1700000100 -0000\ngpgsig -----BEGIN PGP SIGNATURE-----\n \n iQEzBAABCAAdFiEE\n =abcd\n -----END PGP SIGNATURE-----\n\nsigned\n",
		gitID(t, dir, "HEAD^{tree}"), gitID(t, dir, "HEAD"))
	id := strings.TrimSpace(string(git(t, dir, []byte(signed), "hash-object", "-t", "commit", "-w", "--stdin")))
	git(t, dir, nil, "update-ref", "refs/heads/signed", id)
	return s
}

// objects are every object reachable from the repository's refs, with their types.
func (s *sample) objects(t testing.TB, revs ...string) map[ID]Type {
	t.Helper()
	if len(revs) == 0 {
		revs = []string{"--all"}
	}
	listed := git(t, s.dir, nil, append([]string{"rev-list", "--objects"}, revs...)...)
	var ids bytes.Buffer
	for line := range strings.Lines(string(listed)) {
		ids.WriteString(line[:40] + "\n")
	}
	checked := git(t, s.dir, ids.Bytes(), "cat-file", "--batch-check=%(objectname) %(objecttype)")
	all := map[ID]Type{}
	for line := range strings.Lines(string(checked)) {
		name, typ, _ := strings.Cut(strings.TrimSpace(line), " ")
		id, err := ParseID(name)
		if err != nil {
			t.Fatal(err)
		}
		all[id], _ = parseType(typ)
	}
	return all
}

// contents are the objects named, as git cat-file --batch answers them.
func (s *sample) contents(t testing.TB, ids []ID) map[ID][]byte {
	t.Helper()
	var in bytes.Buffer
	for _, id := range ids {
		in.WriteString(id.String() + "\n")
	}
	out := bufio.NewReader(bytes.NewReader(git(t, s.dir, in.Bytes(), "cat-file", "--batch")))
	all := map[ID][]byte{}
	for range ids {
		head, err := out.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(head)
		size, _ := strconv.Atoi(fields[2])
		data := make([]byte, size+1)
		if _, err := io.ReadFull(out, data); err != nil {
			t.Fatal(err)
		}
		id, _ := ParseID(fields[0])
		all[id] = data[:size]
	}
	return all
}

// pack is what git pack-objects writes for revs, read from its standard input.
func (s *sample) pack(t testing.TB, revs string, args ...string) []byte {
	t.Helper()
	return git(t, s.dir, []byte(revs), append([]string{"pack-objects", "--stdout", "--revs", "-q"}, args...)...)
}

// kinds counts the entries of a pack by kind, read with this package's own header reader, so that
// a test knows the pack it reads holds the deltas it is about.
func kinds(t testing.TB, pack []byte) map[byte]int {
	t.Helper()
	counted := map[byte]int{}
	c := &counter{r: bufio.NewReader(bytes.NewReader(pack[packHeaderLen:]))}
	count, _ := readPackHeader(pack)
	for range count {
		kind, size, err := readEntryHeader(c, 1<<40)
		if err != nil {
			t.Fatal(err)
		}
		counted[kind]++
		switch kind {
		case kindOfsDelta:
			readOfsDelta(c)
		case kindRefDelta:
			io.ReadFull(c, make([]byte, 20))
		}
		if _, err := inflate(c, size); err != nil {
			t.Fatal(err)
		}
	}
	return counted
}

// noBases is a repository holding nothing.
type noBases struct{}

func (noBases) OpenObject(context.Context, ID) (ObjectReader, error) { return nil, ErrMissing }

// stored is a pack and its index written by this package to a directory, as a repository keeps
// them, and opened.
type stored struct {
	pack, idx []byte
	*Pack
}

func store(t testing.TB, pack []byte, u *Unpacked) *stored {
	t.Helper()
	var idx bytes.Buffer
	if err := WriteIdx(&idx, u.Objects, u.Checksum); err != nil {
		t.Fatal(err)
	}
	x, err := ParseIdx(idx.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	p, err := OpenPack(bytes.NewReader(pack), int64(len(pack)), x)
	if err != nil {
		t.Fatal(err)
	}
	return &stored{pack: pack, idx: idx.Bytes(), Pack: p}
}

// unpack is Unpack over a pack held in memory, failing the test where it is refused.
func unpack(t testing.TB, pack []byte, opts UnpackOptions) ([]byte, *Unpacked) {
	t.Helper()
	var out bytes.Buffer
	u, err := Unpack(context.Background(), bytes.NewReader(pack), int64(len(pack)), &out, opts)
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), u
}

// bare makes a bare repository holding the packs given and the refs named, for git to read.
func bare(t testing.TB, refs map[string]ID, packs ...*stored) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, nil, "init", "-q", "--bare", ".")
	for _, p := range packs {
		name := filepath.Join(dir, "objects", "pack", fmt.Sprintf("pack-%x", p.idx[len(p.idx)-40:len(p.idx)-20]))
		if err := os.WriteFile(name+".pack", p.pack, 0o444); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name+".idx", p.idx, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	for ref, id := range refs {
		git(t, dir, nil, "update-ref", ref, id.String())
	}
	return dir
}

// refs are the sample's refs, as git for-each-ref lists them.
func (s *sample) refs(t testing.TB) map[string]ID {
	t.Helper()
	refs := map[string]ID{}
	for line := range strings.Lines(string(git(t, s.dir, nil, "for-each-ref", "--format=%(objectname) %(refname)"))) {
		name, ref, _ := strings.Cut(strings.TrimSpace(line), " ")
		id, err := ParseID(name)
		if err != nil {
			t.Fatal(err)
		}
		refs[ref] = id
	}
	return refs
}
