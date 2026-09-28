package runner

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
)

// The trees a runner has fetched, kept by digest inside their namespace.
//
// "Trees are content-addressed and cached on the runner by commit, so a fleet fetches a commit
// once, not once per task." A tree is a list of files named by the SHA-256 of their bytes, so
// what is kept is the files, each once under its digest, and a commit a runner has run before is
// one whose every file it already holds: a second task of it fetches nothing, and a later commit
// fetches only what changed. Each task's tree is laid out from here with hard links, one per
// path, so laying one out costs a directory entry per file and no copy, and the bytes a task sees
// are the bytes that were checked against their digest when they arrived.
//
// Each namespace keeps its own, at <work root>/.trees/<namespace>/objects/<sha256>, and a task's
// tree is linked from its own namespace's and from no other. Two namespaces holding the same bytes
// hold them twice. Shared, a file one namespace fetched would be handed to another's task without
// that task's grant ever fetching it, and how long a fetch took would tell a namespace whether
// another holds a file it can guess the bytes of. Image layers are the exception, and stay the
// daemon's to share by digest, "since a digest is not a secret": they are public bytes pulled from
// a registry, where a tree is a namespace's own.
//
// A file of the cache is 0444, or 0555 for its executable twin, <sha256>.exec, made from it where
// a tree gives the file an executable bit: a hard link shares its mode with every other name of the
// file, so the two modes a tree lays out are two files. Nothing writes to one once it is in place:
// the tree it is linked into is bound read-only, and the agent only ever adds a file under a new
// name or takes a name away.
//
// Taking a name away is what bounds the cache. Past treeCacheBound, the files used least recently
// go first. A tree laid out from a file keeps it through its own link, so taking one away never
// reaches into a task's tree: it only means the next task of that commit fetches it again.

// treeCacheBound is how many bytes the trees of every namespace may keep between them before the
// least recently used go.
//
// Four gibibytes, and not a setting. A workflow repository is an entry point, its fragments and its
// scripts, which the interim push held to 4 MiB a tree, so this keeps the trees of every commit a
// busy host runs many times over; and it is a small part of the disk a runner is given for the
// task directories beside it, which is the disk the cache must never take from them. A tree larger
// than the bound is still laid out whole, and its files are the first to go once its task has.
const treeCacheBound int64 = 4 << 30

// objectsDir is the directory of a namespace's cache under the trees directory.
const objectsDir = "objects"

// execSuffix names the executable twin of a cached file.
const execSuffix = ".exec"

// cachedName is what a file of a namespace's cache is called: its digest, and the suffix of the
// executable twin.
var cachedName = regexp.MustCompile(`^[0-9a-f]{64}(\.exec)?$`)

// tempPrefix begins the name of a file being fetched into the cache, before it is checked and put
// in place. One left behind by an agent that stopped halfway is taken away at the next start.
const tempPrefix = ".fetching-"

// treeCache is the cache of one work root.
type treeCache struct {
	// dir is <work root>/.trees.
	dir   string
	bound int64

	mu     sync.Mutex
	loaded bool
	// entries are the files of the cache by their path under dir, <namespace>/objects/<name>.
	entries map[string]*cachedFile
	total   int64
	// tick orders uses: a file used later has a larger one.
	tick uint64
	// filling are the files being fetched or made, each closed once it is in place or has
	// failed, so that two tasks wanting one file at once fetch it once.
	filling map[string]chan struct{}
}

// cachedFile is one file of the cache: its size, when it was last used, and how many trees being
// laid out hold it, which keep it from being taken away until they are linked.
type cachedFile struct {
	size int64
	used uint64
	held int
}

var (
	cachesMu sync.Mutex
	caches   = map[string]*treeCache{}
)

// treeCacheAt is the cache of a work root, one per work root in a process, so that every task an
// agent assembles counts against the one bound.
func treeCacheAt(workRoot string) *treeCache {
	dir := filepath.Join(filepath.Clean(workRoot), TreesDir)
	cachesMu.Lock()
	defer cachesMu.Unlock()
	c, ok := caches[dir]
	if !ok {
		c = &treeCache{dir: dir, bound: treeCacheBound, entries: map[string]*cachedFile{}, filling: map[string]chan struct{}{}}
		caches[dir] = c
	}
	return c
}

// cachedObject is one file a tree wants from the cache: its digest, which twin, and a path of the
// tree that names it, for a refusal to name.
type cachedObject struct {
	objectKey
	path string
}

// objectKey is a file of a namespace's cache: its digest, and which twin.
type objectKey struct {
	digest string
	exec   bool
}

// name is the object's path under the trees directory.
func (o objectKey) name(namespace string) string {
	n := o.digest
	if o.exec {
		n += execSuffix
	}
	return namespace + "/" + objectsDir + "/" + n
}

// hold puts every object in the namespace's cache, fetching through objects what it does not hold
// yet, and answers where each one is. Each is held until release is called, so that nothing takes
// it away before the tree is linked; release is to be called whatever hold answered. want names
// each object once.
func (c *treeCache) hold(ctx context.Context, objects artifact.Objects, namespace string, want []cachedObject, l agk.Limits) (at map[objectKey]string, release func(), err error) {
	var held []string
	release = func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, name := range held {
			if e, ok := c.entries[name]; ok && e.held > 0 {
				e.held--
			}
		}
		held = nil
	}
	if !givenName.MatchString(namespace) {
		return nil, release, notRunnable{fmt.Errorf("runner: the namespace %q is not one a task can run in: a namespace is lowercase words joined by hyphens, and its trees are kept under its name", namespace)}
	}
	if err := c.load(); err != nil {
		return nil, release, err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu    sync.Mutex
		first error
		wg    sync.WaitGroup
	)
	at = map[objectKey]string{}
	work := make(chan cachedObject)
	for range min(treeFetchers, len(want)) {
		wg.Go(func() {
			for o := range work {
				path, err := c.ensure(ctx, objects, namespace, o, l)
				mu.Lock()
				if err == nil {
					at[o.objectKey] = path
					held = append(held, o.name(namespace))
				} else if first == nil {
					first = err
					cancel()
				}
				mu.Unlock()
			}
		})
	}
feed:
	for _, o := range want {
		select {
		case work <- o:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	if first != nil {
		return nil, release, first
	}
	if err := ctx.Err(); err != nil {
		return nil, release, err
	}
	return at, release, nil
}

// ensure puts one object in place and holds it, fetching it or making its executable twin where
// the cache does not hold it yet, and answers its path.
func (c *treeCache) ensure(ctx context.Context, objects artifact.Objects, namespace string, o cachedObject, l agk.Limits) (string, error) {
	name := o.name(namespace)
	path := filepath.Join(c.dir, filepath.FromSlash(name))
	for {
		c.mu.Lock()
		if e, ok := c.entries[name]; ok {
			// Held by the index and gone from the disk is a file somebody took away by
			// hand, and it is fetched again rather than linked from nothing.
			if _, err := os.Lstat(path); err == nil {
				e.held++
				c.tick++
				e.used = c.tick
				c.mu.Unlock()
				return path, nil
			}
			c.total -= e.size
			delete(c.entries, name)
		}
		if wait, ok := c.filling[name]; ok {
			c.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		done := make(chan struct{})
		c.filling[name] = done
		c.mu.Unlock()

		size, err := c.fill(ctx, objects, namespace, o, path, l)

		c.mu.Lock()
		delete(c.filling, name)
		close(done)
		if err == nil {
			c.tick++
			c.entries[name] = &cachedFile{size: size, used: c.tick, held: 1}
			c.total += size
		}
		c.mu.Unlock()
		if err != nil {
			return "", err
		}
		return path, nil
	}
}

// fill puts one object at path: the plain file fetched through objects and held to its digest, or
// the executable twin copied from the plain file, which is fetched first where it is not held.
func (c *treeCache) fill(ctx context.Context, objects artifact.Objects, namespace string, o cachedObject, path string, l agk.Limits) (int64, error) {
	dir := filepath.Dir(path)
	for _, d := range []string{c.dir, filepath.Dir(dir), dir} {
		if err := os.Mkdir(d, treesMode); err != nil && !errors.Is(err, fs.ErrExist) {
			return 0, fmt.Errorf("runner: the tree cache %s could not be created: %w", d, err)
		}
	}
	if err := os.Chmod(c.dir, treesMode); err != nil {
		return 0, fmt.Errorf("runner: the trees directory %s: %w", c.dir, err)
	}

	var (
		r    io.Reader
		mode = treeFileMode
	)
	if o.exec {
		plain := o
		plain.exec = false
		from, err := c.ensure(ctx, objects, namespace, plain, l)
		if err != nil {
			return 0, err
		}
		defer c.let(plain.name(namespace))
		f, err := os.Open(from)
		if err != nil {
			return 0, fmt.Errorf("runner: the tree file %s: %w", o.path, err)
		}
		defer f.Close()
		r, mode = f, treeExecMode
	} else {
		rc, err := objects.Open(ctx, artifact.Key(namespace, o.digest))
		if err != nil {
			return 0, fmt.Errorf("runner: the tree file %s could not be fetched: %w", o.path, err)
		}
		defer rc.Close()
		r = rc
	}

	tmp, err := os.CreateTemp(dir, tempPrefix)
	if err != nil {
		return 0, fmt.Errorf("runner: the tree file %s could not be cached: %w", o.path, err)
	}
	placed := false
	defer func() {
		if !placed {
			os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	limit := l.ArtifactMaxBytes
	if limit > 0 {
		r = io.LimitReader(r, limit+1)
	}
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return 0, fmt.Errorf("runner: the tree file %s could not be fetched: %w", o.path, err)
	case limit > 0 && n > limit:
		return 0, fmt.Errorf("%w: the tree file %s is longer than the %d bytes an object of the store may be", ErrNotAsNamed, o.path, limit)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != o.digest {
		return 0, fmt.Errorf("%w: the tree file %s is named sha256 %s and its bytes are sha256 %s", ErrNotAsNamed, o.path, o.digest, got)
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return 0, fmt.Errorf("runner: the tree file %s: %w", o.path, err)
	}
	// In place only once checked, so that a file of the cache is always bytes that hash to
	// its name, whatever stopped the agent on the way.
	if err := os.Rename(tmp.Name(), path); err != nil {
		return 0, fmt.Errorf("runner: the tree file %s could not be cached: %w", o.path, err)
	}
	placed = true
	return n, nil
}

// let lets one hold on a file go.
func (c *treeCache) let(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[name]; ok && e.held > 0 {
		e.held--
	}
}

// load reads what the cache holds from the disk, once per process, so that an agent that restarts
// keeps what it had fetched, and takes away what an agent that stopped halfway through a fetch
// left. The files held longest ago, by the moment they were put in place, are the first to go.
func (c *treeCache) load() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded {
		return nil
	}
	namespaces, err := os.ReadDir(c.dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("runner: the trees directory %s could not be read: %w", c.dir, err)
	}
	type found struct {
		name string
		size int64
		at   int64
	}
	var all []found
	for _, ns := range namespaces {
		// A tree laid out for a task is named after it, with dots between its parts, and
		// never on a namespace's grammar.
		if !ns.IsDir() || !givenName.MatchString(ns.Name()) {
			continue
		}
		dir := filepath.Join(c.dir, ns.Name(), objectsDir)
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if strings.HasPrefix(f.Name(), tempPrefix) {
				os.Remove(filepath.Join(dir, f.Name()))
				continue
			}
			info, err := f.Info()
			if err != nil || !info.Mode().IsRegular() || !cachedName.MatchString(f.Name()) {
				continue
			}
			all = append(all, found{ns.Name() + "/" + objectsDir + "/" + f.Name(), info.Size(), info.ModTime().UnixNano()})
		}
	}
	slices.SortFunc(all, func(a, b found) int { return cmp.Or(cmp.Compare(a.at, b.at), strings.Compare(a.name, b.name)) })
	for _, f := range all {
		c.tick++
		c.entries[f.name] = &cachedFile{size: f.size, used: c.tick}
		c.total += f.size
	}
	c.loaded = true
	return nil
}

// evict takes away the files used least recently until the cache is within its bound, leaving
// every file a tree being laid out holds.
func (c *treeCache) evict() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.total <= c.bound {
		return
	}
	var names []string
	for name, e := range c.entries {
		if e.held == 0 {
			names = append(names, name)
		}
	}
	slices.SortFunc(names, func(a, b string) int { return cmp.Compare(c.entries[a].used, c.entries[b].used) })
	for _, name := range names {
		if c.total <= c.bound {
			return
		}
		err := os.Remove(filepath.Join(c.dir, filepath.FromSlash(name)))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			// Kept in the count, since it is still on the disk, and tried again at
			// the next eviction.
			continue
		}
		c.total -= c.entries[name].size
		delete(c.entries, name)
	}
}
