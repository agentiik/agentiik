package repo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

// counting is a lookup that counts what is asked of it.
type counting struct {
	Lookup
	asked []ID
}

func (c *counting) OpenObject(ctx context.Context, id ID) (ObjectReader, error) {
	c.asked = append(c.asked, id)
	return c.Lookup.OpenObject(ctx, id)
}

func sampleStore(t *testing.T) (*sample, *stored) {
	t.Helper()
	s := newSample(t)
	out, u := unpack(t, s.pack(t, "", "--all", "--delta-base-offset"), UnpackOptions{})
	return s, store(t, out, u)
}

func TestATreeReadsAsAFileSystem(t *testing.T) {
	s, st := sampleStore(t)
	fsys := NewTreeFS(context.Background(), st, gitID(t, s.dir, s.first.String()+"^{tree}"))
	if err := fstest.TestFS(fsys, "agentiik.yaml", "scripts/run.sh", "schemas/order.json", "data/big.txt", "deep/a/b/c/d.txt", "sort.txt", "sort/x", "empty"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agentiik.yaml", "data/big.txt", "empty", "deep/a/b/c/d.txt"} {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			t.Fatal(err)
		}
		if want := git(t, s.dir, nil, "show", s.first.String()+":"+name); string(data) != string(want) {
			t.Errorf("%s reads as %d bytes, and git shows %d", name, len(data), len(want))
		}
	}
	info, err := fs.Stat(fsys, "scripts/run.sh")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := info.Sys().(TreeEntry)
	if !ok || e.Mode != ModeExecutable || e.ID != gitID(t, s.dir, s.first.String()+":scripts/run.sh") || info.Mode() != 0o555 {
		t.Errorf("scripts/run.sh is %v, %v", info.Mode(), info.Sys())
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	// fs.ReadDir sorts by name, where git sorts the directory sort after sort.txt.
	if got := strings.Join(names, " "); got != "agentiik.yaml data deep empty schemas scripts sort sort.txt" {
		t.Errorf("the root lists as %s", got)
	}
}

func TestATreeFollowsNoLinkAndOpensNoSubmodule(t *testing.T) {
	s, st := sampleStore(t)
	fsys := NewTreeFS(context.Background(), st, gitID(t, s.dir, "main^{tree}"))
	for name, c := range map[string]struct {
		mode fs.FileMode
		is   string
	}{"link": {fs.ModeSymlink, "a symbolic link"}, "vendor/sub": {fs.ModeIrregular, "a submodule"}} {
		info, err := fs.Stat(fsys, name)
		if err != nil || info.Mode().Type() != c.mode {
			t.Errorf("%s stats as %v, %v", name, info, err)
		}
		if _, err := fsys.Open(name); err == nil || !strings.Contains(err.Error(), c.is) {
			t.Errorf("%s opens with %v, where it is refused as %s", name, err, c.is)
		}
		if _, err := fsys.Open(name + "/x"); err == nil || !strings.Contains(err.Error(), c.is) {
			t.Errorf("a path through %s opens with %v, where it is refused as crossing %s", name, err, c.is)
		}
	}
	if info, err := fs.Stat(fsys, "link"); err != nil || info.Size() != int64(len("agentiik.yaml")) {
		t.Errorf("the link's size is its target's length, and stats as %v, %v", info, err)
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "link" && e.Type() != fs.ModeSymlink {
			t.Errorf("the link lists as %v", e.Type())
		}
	}
	for _, name := range []string{"missing", "agentiik.yaml/x", "/agentiik.yaml", "a/../agentiik.yaml"} {
		if _, err := fsys.Open(name); err == nil {
			t.Errorf("%s opens", name)
		}
	}
}

func TestAFileReadWholeIsBoundedAsWhatElseIsHeld(t *testing.T) {
	s, st := sampleStore(t)
	defer func(was int64) { maxHeld = was }(maxHeld)
	maxHeld = 10 << 10
	fsys := NewTreeFS(context.Background(), st, gitID(t, s.dir, "main^{tree}"))
	if _, err := fs.ReadFile(fsys, "data/big.txt"); err == nil || !strings.Contains(err.Error(), "that is read whole") {
		t.Errorf("a file larger than what is held reads whole with %v", err)
	}
	f, err := fsys.Open("data/big.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if n, err := io.Copy(io.Discard, f); err != nil || n < maxHeld {
		t.Errorf("the same file opened reads as it streams: %d bytes, %v", n, err)
	}
	if _, err := fs.ReadFile(fsys, "agentiik.yaml"); err != nil {
		t.Errorf("a file smaller than what is held reads with %v", err)
	}
	defer func(was int64) { maxParsed = was }(maxParsed)
	maxParsed = 100
	fsys = NewTreeFS(context.Background(), st, gitID(t, s.dir, "main^{tree}"))
	if _, err := fs.ReadDir(fsys, "."); err == nil || !strings.Contains(err.Error(), "more than the 100") {
		t.Errorf("a tree larger than what is parsed lists with %v", err)
	}
}

func TestATreeIsReadAsItIsWalked(t *testing.T) {
	s, st := sampleStore(t)
	lookup := &counting{Lookup: st}
	fsys := NewTreeFS(context.Background(), lookup, gitID(t, s.dir, "main^{tree}"))
	if _, err := fs.ReadFile(fsys, "deep/a/b/c/d.txt"); err != nil {
		t.Fatal(err)
	}
	// The root, deep, a, b and c, then the file, and nothing beside them.
	if len(lookup.asked) != 6 {
		t.Errorf("reading one file five directories down read %d objects", len(lookup.asked))
	}
	lookup.asked = nil
	if _, err := fs.ReadFile(fsys, "deep/a/b/c/d.txt"); err != nil {
		t.Fatal(err)
	}
	if len(lookup.asked) != 1 {
		t.Errorf("reading it again read %d objects, where its trees were read already", len(lookup.asked))
	}
}

func TestATreeNamingAnObjectTheRepositoryLacksIsBrokenAndNotEmpty(t *testing.T) {
	s, _ := sampleStore(t)
	// A pack of the root tree alone: every name in it names an object the lookup does not hold.
	root := gitID(t, s.dir, "main^{tree}")
	tree := s.contents(t, []ID{root})[root]
	var pack bytes.Buffer
	w, err := NewPackWriter(&pack, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Add(TypeTree, tree); err != nil {
		t.Fatal(err)
	}
	checksum, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	fsys := NewTreeFS(context.Background(), store(t, pack.Bytes(), &Unpacked{Objects: w.Objects(), Checksum: checksum}), root)
	_, err = fs.ReadFile(fsys, "agentiik.yaml")
	if !errors.Is(err, ErrMissing) || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file whose blob the repository lacks reads with %v", err)
	}
	if _, err := fs.ReadDir(fsys, "deep"); !errors.Is(err, ErrMissing) {
		t.Errorf("a directory whose tree the repository lacks lists with %v", err)
	}
	if _, err := fs.ReadFile(fsys, "nothing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a name the tree does not hold reads with %v", err)
	}
}
