package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
)

// The objects of the trees a runner lays out, kept between tasks.
//
// "Trees are content-addressed and cached on the runner by commit, so a fleet fetches a commit
// once, not once per task." A tree is a list of files named by the digest of their bytes, and two
// tasks of one commit, two shards of one step or two commits sharing most of their files, name
// most of the same objects. So each object is fetched once and kept under the digest it was
// checked against, and every tree that names it again is laid out from the copy here, fetching
// nothing: "a runner that already holds a digest writes it from its own cache". A narrowed step
// names fewer of them, and still finds here those another task of the commit fetched.
//
// Kept by namespace, never across one. An object is its bytes, and the bytes of one namespace's
// tree are no business of another's: a task laid out from what another namespace fetched would
// learn, from how fast its tree arrived, what that namespace's repository holds. So a namespace
// fetches what it names itself, as the store keeps its objects under its own prefix.

// ObjectsDir is where the objects sit under the work root, one directory per namespace.
//
// The dot keeps it apart from the task directories beside it, for the reason TreesDir has one, and
// it is private to the agent as the trees directory is: an object is written 0444 so that a tree
// can link it, and nothing but the agent reaches it except through a tree's bind.
const ObjectsDir = ".objects"

// objectsKept is how long an object no tree has named stays: seven days, the longest a task
// message waits on the stream, so that a task of any commit a runner may still be handed finds
// the objects the tasks before it fetched. What a runner has not been asked for in a week is a
// commit nothing runs any more, and keeping it would be a disk that fills with every commit ever
// pushed.
const objectsKept = 7 * 24 * time.Hour

// objectsPruneEvery is how often the objects past objectsKept are taken away, which is done on
// the way to laying out a tree rather than by a loop of its own: a runner that lays out none has
// nothing new to keep, and hourly is what the record of keys is pruned at.
const objectsPruneEvery = time.Hour

// cache is the objects of one namespace under one work root.
type cache struct {
	dir string
}

// cacheOf is the namespace's cache under the work root, or none where there is no work root to
// keep it under, which lays a tree out fetching every file, as a runner did before it kept one.
func cacheOf(workRoot, namespace string) (*cache, error) {
	if workRoot == "" {
		return nil, nil
	}
	// The rule the store holds a namespace to for a key's first segment, since this is one.
	if namespace == "" || namespace == "." || namespace == ".." || strings.ContainsAny(namespace, "/\\\x00") {
		return nil, fmt.Errorf("runner: the namespace %q is not one a directory can be named after", namespace)
	}
	top := filepath.Join(workRoot, ObjectsDir)
	pruneObjects(top, time.Now())
	dir := filepath.Join(top, namespace)
	for _, d := range []string{top, dir} {
		if err := os.Mkdir(d, treesMode); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("runner: the objects directory %s could not be created: %w", d, err)
		}
		if err := os.Chmod(d, treesMode); err != nil {
			return nil, fmt.Errorf("runner: the objects directory %s: %w", d, err)
		}
	}
	return &cache{dir: dir}, nil
}

// held answers the path of the object the cache holds for a digest, fetching it first where it
// holds none, and marks it used, so that the prune keeps it for another objectsKept.
//
// It is checked against the digest as it is fetched, exactly as a tree file is, and only then put
// under the digest's name, by a rename, so that what the cache holds under a name is the whole of
// the bytes that name says or nothing. Two tasks fetching one object at once each write their own
// and the second rename replaces the first with the same bytes.
func (c *cache) held(ctx context.Context, objects artifact.Objects, namespace, digest string, e TreeEntry, l agk.Limits) (string, error) {
	name := filepath.Join(c.dir, digest)
	now := time.Now()
	if err := os.Chtimes(name, now, now); err == nil {
		return name, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("runner: the kept object %s: %w", digest, err)
	}
	r, err := objects.Open(ctx, artifact.Key(namespace, digest))
	if err != nil {
		return "", fmt.Errorf("runner: the tree file %s could not be fetched: %w", e.Path, err)
	}
	defer r.Close()
	f, err := os.CreateTemp(c.dir, ".fetching-*")
	if err != nil {
		return "", fmt.Errorf("runner: the tree file %s could not be kept: %w", e.Path, err)
	}
	staged := f.Name()
	f.Close()
	defer os.Remove(staged)
	if err := os.Remove(staged); err != nil {
		return "", fmt.Errorf("runner: the tree file %s could not be kept: %w", e.Path, err)
	}
	kept := e
	kept.Mode = "0444"
	if err := writeChecked(staged, r, digest, kept, l); err != nil {
		return "", err
	}
	if err := os.Rename(staged, name); err != nil {
		return "", fmt.Errorf("runner: the tree file %s could not be kept: %w", e.Path, err)
	}
	return name, nil
}

// place puts the kept object at name, a file of a tree: linked where the tree gives it the mode
// the object is kept with, since a link costs no copy and no disk, and copied where it is
// executable, since a link's mode is the object's own and every tree linking it would become
// executable with it. A link refused, the cache and the trees being on two filesystems, is a copy.
func place(kept, name string, e TreeEntry, l agk.Limits) error {
	mode, err := treeMode(e.Mode)
	if err != nil {
		return fmt.Errorf("runner: the tree gives %s %w", e.Path, err)
	}
	if mode&0o111 == 0 {
		if err := os.Link(kept, name); err == nil {
			return nil
		} else if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("runner: the tree file %s could not be created: %w", e.Path, err)
		}
	}
	src, err := os.Open(kept)
	if err != nil {
		return fmt.Errorf("runner: the kept object for %s: %w", e.Path, err)
	}
	defer src.Close()
	return writeChecked(name, src, e.SHA256, e, l)
}

// pruned is when each objects directory was last pruned, so that one prune an hour is made
// whichever task asks.
var pruned sync.Map

// pruneObjects takes away, at most once every objectsPruneEvery, the objects no tree has named for
// objectsKept, a fetch that died before its rename, and a namespace's directory left empty.
//
// An object linked into a tree still laid out is removed from the cache and not from the tree,
// which keeps its own link to the bytes until it goes with its task. A task that finds its object
// gone between the two fetches it again.
func pruneObjects(top string, now time.Time) {
	if last, ok := pruned.Load(top); ok && now.Sub(last.(time.Time)) < objectsPruneEvery {
		return
	}
	pruned.Store(top, now)
	namespaces, err := os.ReadDir(top)
	if err != nil {
		return
	}
	for _, ns := range namespaces {
		dir := filepath.Join(top, ns.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			cutoff := now.Add(-objectsKept)
			if strings.HasPrefix(e.Name(), ".fetching-") {
				// A fetch takes minutes at the most, and one a day old is one that died.
				cutoff = now.Add(-24 * time.Hour)
			}
			if info.ModTime().Before(cutoff) {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
		// Refused while it holds anything, which is the whole of the check.
		os.Remove(dir)
	}
}
