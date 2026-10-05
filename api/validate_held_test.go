package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/repo"
	"github.com/agentiik/agentiik/version"
)

// filesAt are files of the same text at each path.
func filesAt(text string, paths ...string) []committedFile {
	files := make([]committedFile, len(paths))
	for i, p := range paths {
		files[i] = committedFile{path: p, text: &text}
	}
	return files
}

// draftOf is what a validation writes of files laid over no tree, which reads nothing of the
// repository, and the tree of its root, the last object written.
func draftOf(t *testing.T, files []committedFile) (*objectsWritten, repo.ID) {
	t.Helper()
	written := &objectsWritten{}
	root, err := rewrite(t.Context(), nil, repo.ID{}, files, written)
	if err != nil {
		t.Fatal(err)
	}
	return written, root
}

// A validation reads its draft back object by object, and finding one must not cost more the later
// it was written: counted in allocations, which hashing an object again makes, rather than in time,
// which a busy machine makes up.
func TestAValidationFindsEachObjectItWroteByItsIdentifier(t *testing.T) {
	var files []committedFile
	for i := range 4096 {
		// Each in a directory of its own, so that the draft writes a tree for every file as well.
		files = append(files, filesAt(fmt.Sprintf("file %d\n", i), fmt.Sprintf("d%04d/f", i))...)
	}
	written, root := draftOf(t, files)
	h := held{written: written}
	open := func(id repo.ID) func() {
		return func() {
			o, err := h.OpenObject(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			o.Close()
		}
	}
	first := repo.HashObject(written.objects[0].t, written.objects[0].data)
	if a, b := testing.AllocsPerRun(10, open(first)), testing.AllocsPerRun(10, open(root)); b > a {
		t.Errorf("the root of a draft of %d objects takes %v allocations to open, and its first object %v", len(written.objects), b, a)
	}
}

// deep are n paths, each 200 directories down below a directory of its own: a draft of the same text
// at each is about two hundred objects, the trees below the first directory being one chain they all
// name, and a walk of it visits a chain for every path.
func deep(n int) []string {
	paths := make([]string, n)
	for i := range paths {
		paths[i] = fmt.Sprintf("a%04d/%sf", i, strings.Repeat("x/", 200))
	}
	return paths
}

// A validation whose caller has gone stops at the next path it reads rather than judging the rest of
// the draft for nobody, and it does once each tree of a draft whose trees repeat has been read, when
// the walk goes on through trees it holds already with no object read to stop at: counted in the
// directories the walk lists, rather than in time.
func TestAValidationStopsWhenItsCallerHasGone(t *testing.T) {
	written, root := draftOf(t, filesAt("", deep(64)...))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reads := &counted{under: held{written: written}}
	tree := &listing{TreeFS: repo.NewTreeFS(ctx, reads, root), after: 1000, cancel: cancel}
	_, err := version.Check(ctx, tree, version.Checking{Committed: true, Namespace: "finance", Repository: "monthly-invoicing"})
	if !errors.Is(err, context.Canceled) || tree.listed != tree.after || reads.opened >= tree.after {
		t.Errorf("a validation whose caller left as it listed its directory %d listed %d, having read %d of %d objects, and answered %v", tree.after, tree.listed, reads.opened, len(written.objects), err)
	}
}

// listing is a tree whose caller goes away as the walk lists its directory after.
type listing struct {
	*repo.TreeFS
	after, listed int
	cancel        context.CancelFunc
}

func (l *listing) ReadDir(name string) ([]fs.DirEntry, error) {
	if l.listed++; l.listed == l.after {
		l.cancel()
	}
	return l.TreeFS.ReadDir(name)
}

// counted is a lookup counting the objects opened through it.
type counted struct {
	under  repo.Lookup
	opened int
}

func (c *counted) OpenObject(ctx context.Context, id repo.ID) (repo.ObjectReader, error) {
	c.opened++
	return c.under.OpenObject(ctx, id)
}

// A draft whose caller has gone is not written past the directory it is at, although a directory
// the parent does not hold is written without reading anything that would notice.
func TestADraftWhoseCallerHasGoneIsNotWritten(t *testing.T) {
	parent, base := draftOf(t, filesAt("", "agentiik.yaml"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The caller goes as the parent's root is read, the one object below which the draft reads.
	reads := &cancelling{under: held{written: parent}, cancel: cancel}
	w := &objectsWritten{}
	if _, err := rewrite(ctx, reads, base, filesAt("", deep(64)...), w); !errors.Is(err, context.Canceled) || len(w.objects) != 0 {
		t.Errorf("a draft whose caller left as its parent's root was read wrote %d objects, and answered %v", len(w.objects), err)
	}
}

// cancelling is a lookup whose caller goes away as it opens its first object.
type cancelling struct {
	under  repo.Lookup
	cancel context.CancelFunc
}

func (c *cancelling) OpenObject(ctx context.Context, id repo.ID) (repo.ObjectReader, error) {
	c.cancel()
	return c.under.OpenObject(ctx, id)
}

// A draft naming more paths than a walk of a tree visits is refused before a tree of it is written,
// as the walk would refuse the tree, and the paths are counted as the tree lists them: each file
// written and each directory above one, once, a file removed adding none.
func TestADraftNamingMorePathsThanAWalkVisitsIsRefusedFirst(t *testing.T) {
	files := append(filesAt("", "a/b-c/x", "a/b/y", "a/b/z", "f"), committedFile{path: "g/h/removed"})
	if n := pathsNamed(files); n != 7 {
		t.Errorf("a/b-c/x, a/b/y, a/b/z and f, and g/h/removed removed, name %d paths, where their tree lists 7", n)
	}

	// 2,048 empty files, each a thousand directories below one of its own: four mebibytes of
	// paths, which are two million trees to write and a walk refused at its 65,537th path.
	var paths []string
	for i := range 2048 {
		paths = append(paths, fmt.Sprintf("a%04d/%sf", i, strings.Repeat("x/", 1018)))
	}
	if err := checkFiles(filesAt("", paths...)); err == nil || err.Error() != version.ErrTooManyEntries.Error() {
		t.Errorf("a draft of 2,048 files a thousand directories deep is answered %v", err)
	}
	// As many files as a version holds, each in a directory of its own, are well within it.
	paths = paths[:0]
	for i := range TreeMaxFiles {
		paths = append(paths, fmt.Sprintf("d%04d/f", i))
	}
	if err := checkFiles(filesAt("", paths...)); err != nil {
		t.Errorf("a draft of %d files, each in a directory of its own, is answered %v", TreeMaxFiles, err)
	}
}
