// Package version turns a stored workflow version back into a graph.
//
// It fills controller.Versions, which the controller states and calls and does not implement, in
// the same arrangement graph.Driver and controller.Queue already have.
//
// # What a version is
//
// "A version is a commit. finance/monthly-invoicing@a3f9c1e names exactly one tree, permanently,
// because that is what a commit already is." A run pins one, and a branch that moves afterwards
// has to change nothing about it. So a version stores what its graph is rebuilt from: the entry
// point as it was at that commit, the files it included, and the manifest of every image it
// names. Rebuilding reaches no repository, no object store and no registry, which is what makes a
// run of a two year old commit evaluate the same way today as it did then. The version names its
// whole tree as well, which is what a container is given at /agk/repo and plays no part here.
//
// # Why it is cached
//
// A graph is immutable for the life of a version, which the evaluator says outright: "Nothing in
// it changes as a run proceeds ... one graph can serve every run of a version." So the answer to
// one commit is computed once and kept, and a controller deciding a thousand runs of one version
// parses one document.
package version

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"testing/fstest"

	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
)

// Store answers what a version's graph is.
type Store struct {
	pool *db.Pool

	mu   sync.Mutex
	held map[string]*graph.Graph
	// keep bounds what is remembered. A controller serving an installation with ten
	// thousand versions would otherwise hold ten thousand graphs, and the ones worth
	// holding are the ones with runs going.
	keep  int
	order []string
}

// Options are what a Store is given.
type Options struct {
	// Keep is how many versions stay parsed. The zero value is 256, which is more
	// versions than an installation has runs going and few enough to be a bounded amount
	// of memory.
	Keep int
}

// New builds one.
func New(pool *db.Pool, o Options) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("version: no database, and a version is a row")
	}
	if o.Keep <= 0 {
		o.Keep = 256
	}
	return &Store{pool: pool, held: map[string]*graph.Graph{}, keep: o.Keep}, nil
}

// Graph answers the resolved graph of one version.
//
// It is read through the installation door, because the controller is elected once and serves
// every namespace, and a version is read in order to decide a run rather than on behalf of
// whoever asked for one.
func (s *Store) Graph(ctx context.Context, namespace, workflow, commit string) (*graph.Graph, error) {
	key := namespace + "/" + workflow + "@" + commit
	s.mu.Lock()
	if g, held := s.held[key]; held {
		s.mu.Unlock()
		return g, nil
	}
	s.mu.Unlock()

	var v db.Version
	err := s.pool.Installation(ctx, db.ControllerSweep, func(ctx context.Context, w *db.Wide) error {
		var err error
		v, err = w.Version(ctx, namespace, workflow, commit)
		return err
	})
	if err != nil {
		return nil, err
	}

	g, err := Build(v)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, held := s.held[key]; !held {
		s.held[key] = g
		s.order = append(s.order, key)
		for len(s.order) > s.keep {
			delete(s.held, s.order[0])
			s.order = s.order[1:]
		}
	}
	return g, nil
}

// Build turns a stored version back into a graph, reaching nothing.
//
// The entry point and the includes are the ones the version carries, which is the whole point: a
// run of a commit whose branch has since moved, or whose repository has since been deleted,
// evaluates exactly as it did when it started. Version.Tree is what a container is given, and
// plays no part here.
func Build(v db.Version) (*graph.Graph, error) {
	if v.Entry == "" || len(v.Document) == 0 {
		return nil, fmt.Errorf("version: %s@%s carries no entry point", v.Workflow, v.Commit)
	}

	tree := fstest.MapFS{v.Entry: &fstest.MapFile{Data: v.Document, Mode: 0o444}}
	for path, body := range v.Includes {
		tree[path] = &fstest.MapFile{Data: body, Mode: 0o444}
	}

	wf, err := graph.Load(tree, v.Entry, nil)
	if err != nil {
		return nil, fmt.Errorf("version: %s@%s could not be loaded: %w", v.Workflow, v.Commit, err)
	}

	manifests := make(map[string]brick.Manifest, len(v.Manifests))
	for image, body := range v.Manifests {
		m, err := brick.ParseManifest(body)
		if err != nil {
			return nil, fmt.Errorf("version: the manifest of %s in %s@%s: %w", image, v.Workflow, v.Commit, err)
		}
		manifests[image] = m
	}
	g, err := graph.Build(wf, manifests)
	if err != nil {
		return nil, fmt.Errorf("version: %s@%s could not be built: %w", v.Workflow, v.Commit, err)
	}
	return g, nil
}

// Capture is the half of a push that the graph is rebuilt from: the tree as it is now, reduced to
// what the workflow actually reaches, plus the manifest of every image it names. The whole tree
// travels beside it in the push, as what a container is given, and is not this function's.
//
// Reduced rather than whole, because this is what a run is decided from, read back every time a
// graph is rebuilt and required to rebuild it with nothing else in reach. Holding every file of
// the repository would make it grow with the repository rather than with the workflow, when the
// files a step sees are already named by the version's tree and held as objects.
//
// What it keeps is what graph.Load read, recorded as it read it. That is exact by construction
// and it is the only way to be exact: an include may itself include, a fragment is opaque from
// out here, and working out the closure by parsing the include blocks again would be this package
// reimplementing resolution in order to agree with resolution.
func Capture(tree fs.FS, entry string, manifests map[string]brick.Manifest) (db.Version, error) {
	watched := &watcher{under: tree, read: map[string][]byte{}}
	wf, err := graph.Load(watched, entry, nil)
	if err != nil {
		return db.Version{}, err
	}
	if err := graph.Check(wf); err != nil {
		return db.Version{}, err
	}

	document, held := watched.read[entry]
	if !held {
		return db.Version{}, fmt.Errorf("version: the entry point %s was not read", entry)
	}

	v := db.Version{
		Entry: entry, Document: document,
		Includes: map[string][]byte{}, Manifests: map[string][]byte{},
	}
	for path, body := range watched.read {
		if path != entry {
			v.Includes[path] = body
		}
	}
	for image, m := range manifests {
		v.Manifests[image] = m.Document()
	}
	return v, nil
}

// watcher is a tree that remembers what was read out of it.
//
// It records the bytes rather than the paths, so that what a version holds is what the loader
// saw rather than what the tree holds now: a capture and a second read of the same tree cannot
// disagree, even if something changed in between.
type watcher struct {
	under fs.FS

	mu   sync.Mutex
	read map[string][]byte
}

func (w *watcher) Open(name string) (fs.File, error) {
	f, err := w.under.Open(name)
	if err != nil {
		return nil, err
	}
	return &watched{File: f, name: name, of: w}, nil
}

// watched records a file's bytes once it has been read to the end, which is what fs.ReadFile does
// and what graph.Load uses.
type watched struct {
	fs.File
	name string
	of   *watcher
	seen []byte
}

func (f *watched) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	if n > 0 {
		f.seen = append(f.seen, p[:n]...)
	}
	if err == io.EOF {
		f.of.mu.Lock()
		f.of.read[f.name] = f.seen
		f.of.mu.Unlock()
	}
	return n, err
}

func (f *watched) Close() error {
	// A reader that stopped at the exact end never saw EOF, so the bytes are recorded here
	// too. Recording twice is the same bytes twice.
	if len(f.seen) > 0 {
		f.of.mu.Lock()
		if _, held := f.of.read[f.name]; !held {
			f.of.read[f.name] = f.seen
		}
		f.of.mu.Unlock()
	}
	return f.File.Close()
}
