// Package version judges a tree as the version it would be made of, and turns a stored version
// back into a graph.
//
// Check is the one validation: agk validate, agk run --local, agk push, the push route and the
// pre-receive hook all judge a version by it, so that the command line and the hook can never
// disagree. Build and Store fill controller.Versions, which the controller states and calls and
// does not implement, in the same arrangement graph.Driver and controller.Queue already have.
//
// # What a version is
//
// "A version is a commit. finance/monthly-invoicing@a3f9c1e names exactly one tree, permanently,
// because that is what a commit already is." A run pins one, and a branch that moves afterwards
// has to change nothing about it. So a version stores what its graph is rebuilt from: the entry
// point as it was at that commit, the files it included, what it read of each library another
// repository holds, the manifest of every image it names, and the digest each image it names by
// tag was resolved to when it was pushed. Rebuilding reaches no repository, no object store and no
// registry, which is what makes a run of a two year old commit evaluate the same way today as it
// did then, and run the same images. The version names its whole tree as well, which is what a
// container is given at /agk/repo and plays no part here.
//
// # Why it is cached
//
// A graph is immutable for the life of a version, which the evaluator says outright: "Nothing in
// it changes as a run proceeds ... one graph can serve every run of a version." So the answer to
// one commit is computed once and kept, and a controller deciding a thousand runs of one version
// parses one document.
package version

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing/fstest"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/yamlbound"
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
// The entry point, the includes and what each workflow include read of its library are the ones
// the version carries, which is the whole point: a run of a commit whose branch has since moved, or
// whose repository or a library it includes has since been deleted, evaluates exactly as it did
// when it started. So are the digests its tags were resolved to at
// the push, which the graph names in their place, so that a tag moved since changes nothing a run
// of it runs. Version.Tree is what a container is given, and plays no part here.
//
// And so are the rules it was accepted under. What a version holds is read with graph.LoadStored
// and brick.ParseStoredManifest, which leave out the rules added since a version could be stored,
// the bound a port and a workflow output are written to among them, so that a version recorded
// before them keeps rebuilding, and its runs and replays keep going, after an upgrade. A version
// about to be recorded is judged by Check.
func Build(v db.Version) (*graph.Graph, error) {
	if v.Entry == "" || len(v.Document) == 0 {
		return nil, fmt.Errorf("version: %s@%s carries no entry point", v.Workflow, v.Commit)
	}
	if v.Library {
		return nil, fmt.Errorf("version: %s@%s is a library: %w", v.Workflow, v.Commit, ErrLibrary)
	}

	tree := fstest.MapFS{v.Entry: &fstest.MapFile{Data: v.Document, Mode: 0o444}}
	for path, body := range v.Includes {
		tree[path] = &fstest.MapFile{Data: body, Mode: 0o444}
	}

	wf, err := graph.LoadStored(tree, v.Entry, keptLibraries(v))
	if err != nil {
		return nil, fmt.Errorf("version: %s@%s could not be loaded: %w", v.Workflow, v.Commit, err)
	}

	if err := pin(v, wf); err != nil {
		return nil, err
	}
	manifests := make(map[string]brick.Manifest, len(v.Manifests))
	read := make(map[string][]byte, len(v.Manifests))
	for _, image := range slices.Sorted(maps.Keys(v.Manifests)) {
		body := v.Manifests[image]
		m, err := brick.ParseStoredManifest(body)
		if err != nil {
			return nil, fmt.Errorf("version: the manifest of %s in %s@%s: %w", image, v.Workflow, v.Commit, err)
		}
		// Keyed by what the steps name now, which is the digest where the workflow wrote a
		// tag. Two tags resolved to one digest are one image, and so one manifest.
		key := image
		if pinned, held := v.Images[image]; held {
			key = pinned
		}
		if first, twice := read[key]; twice && !bytes.Equal(first, body) {
			return nil, fmt.Errorf("version: %s@%s carries two manifests for %s, one image under two tags", v.Workflow, v.Commit, key)
		}
		read[key], manifests[key] = body, m
	}
	g, err := graph.Build(wf, manifests)
	if err != nil {
		return nil, fmt.Errorf("version: %s@%s could not be built: %w", v.Workflow, v.Commit, err)
	}
	return g, nil
}

// ErrLibrary is a version that is a library's commit, which has no graph since nothing runs it.
var ErrLibrary = errors.New("its root agentiik.yaml is a fragment, which another workflow includes and nothing runs")

// keptLibraries answers each workflow include out of what the version kept of it: the commit its
// ref resolved to at the push, and the files resolution read there. Nothing is reached, so that
// the library moving its tag, being renamed or being deleted since changes nothing a run of this
// version does. An include the version kept nothing of is refused, which is a version stored
// before workflow includes were resolved and so one that names none.
func keptLibraries(v db.Version) graph.Remote {
	return kept{v: v}
}

type kept struct{ v db.Version }

func (k kept) Include(ref graph.WorkflowRef) (fs.FS, string, error) {
	l, ok := k.v.Libraries[ref.String()]
	if !ok {
		return nil, "", fmt.Errorf("%s@%s kept nothing of it: a version keeps what each workflow include read at the push", k.v.Workflow, k.v.Commit)
	}
	tree := fstest.MapFS{}
	for path, body := range l.Files {
		tree[path] = &fstest.MapFile{Data: body, Mode: 0o444}
	}
	return tree, l.Commit, nil
}

// pin puts the digest each tag was resolved to at the push in the place of the tag, so that the
// graph a run is decided from names every image by digest.
//
// "Images by digest in production: a tag is a mutable pointer, and a commit must determine what
// ran." A task carries the image its step names, and the wire's imageRef is name@sha256:<hex>, so
// a tag left here would become a message no runner may take, dispatched by a controller that
// cannot resolve one: it reaches no registry. An image the workflow names by digest is kept as
// written, one it names by tag takes the digest the version recorded for it, and one with none
// recorded is refused, which refuses the push that carried it.
//
// A recorded digest has to be of the tag's own repository as the workflow spells it, and every
// one has to be of a tag some step names. A version whose file names one image while its runs
// pull another would have its reviewers reading about something that never runs.
func pin(v db.Version, wf *graph.Workflow) error {
	named := map[string]bool{}
	for _, st := range wf.Steps {
		if st.Image != "" && !agk.ImageByDigest(st.Image) {
			named[st.Image] = true
		}
	}
	for _, ref := range slices.Sorted(maps.Keys(v.Images)) {
		pinned := v.Images[ref]
		repository, _, _ := strings.Cut(pinned, "@")
		switch {
		case !named[ref]:
			return fmt.Errorf("version: %s@%s records a digest for %s, which none of its steps names by a tag", v.Workflow, v.Commit, ref)
		case !agk.ImageByDigest(pinned) || repository != agk.ImageRepository(ref):
			return fmt.Errorf("version: %s@%s records %s as %q, and a tag is resolved to its own repository, %s, at a sha256 digest", v.Workflow, v.Commit, ref, pinned, agk.ImageRepository(ref))
		}
	}

	for _, step := range slices.Sorted(maps.Keys(wf.Steps)) {
		st := wf.Steps[step]
		switch pinned, held := v.Images[st.Image]; {
		case st.Image == "" || agk.ImageByDigest(st.Image):
			continue
		case strings.Contains(st.Image, "@"):
			return fmt.Errorf("version: step %s of %s@%s names %s, and a digest is written sha256: and sixty-four lowercase hexadecimal characters", step, v.Workflow, v.Commit, st.Image)
		case !held:
			return fmt.Errorf("version: step %s of %s@%s names %s by a tag, and the version records no digest for it: a server runs every image by the digest its registry serves, which agk push resolves each tag to", step, v.Workflow, v.Commit, st.Image)
		default:
			st.Image = pinned
			wf.Steps[step] = st
		}
	}
	return nil
}

// Capture is what a version holds of a tree, as Check reads it given the manifests of the images
// its bricks run and nothing else of the installation: the entry point and every file it reaches,
// and the manifest of every image a brick step runs. An image named by a tag keeps it, and the
// caller records the digest it was resolved to beside it, as agk push does.
//
// Reduced rather than whole, because this is what a run is decided from, read back every time a
// graph is rebuilt and required to rebuild it with nothing else in reach. Holding every file of
// the repository would make it grow with the repository rather than with the workflow, when the
// files a step sees are already named by the version's tree and held as objects.
func Capture(tree fs.FS, entry string, manifests map[string]brick.Manifest) (db.Version, error) {
	checked, err := Check(context.Background(), tree, Checking{Entry: entry, Resolvers: Resolvers{
		Manifest: func(_ context.Context, image string, _ agk.Step) ([]byte, error) {
			m, ok := manifests[image]
			if !ok {
				return nil, ErrNotHeld
			}
			return m.Document(), nil
		},
	}})
	if err != nil {
		return db.Version{}, err
	}
	return checked.Version, nil
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

// ReadFile reads a file whole through the tree's own ReadFile, and records it: fs.ReadFile would
// otherwise open it here and size its buffer by what the file says it weighs, before the tree
// below could refuse a byte of it. A file read already is answered as it was read, reading nothing
// and spending nothing of a budget below: the root file a library is told apart by is read by
// whatever resolves it next.
func (w *watcher) ReadFile(name string) ([]byte, error) {
	w.mu.Lock()
	held, ok := w.read[name]
	w.mu.Unlock()
	if ok {
		return held, nil
	}
	b, err := fs.ReadFile(w.under, name)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.read[name] = b
	w.mu.Unlock()
	return b, nil
}

// ReadMaxBytes is the most the validation reads out of a tree, every file it reads together: the
// entry point, its includes and the schemas its inputs name. 16 MiB, what one YAML document may
// weigh (yamlbound.MaxBytes), so that reading the files a version is rebuilt from never costs more
// than one document may. A tree push carries 4 MiB of files in all, so it never comes near it; a
// git push carries a commit whose files may be as large as an object may, and a single entry point
// of hundreds of mebibytes, a few kilobytes once compressed, would otherwise be read whole into the
// memory of the installation judging it.
const ReadMaxBytes = yamlbound.MaxBytes

// budgeted is a tree whose files are read up to a budget, and refused past it. Several trees may
// share one budget: a workflow include's files are read within what the entry point's tree left.
type budgeted struct {
	under fs.FS
	of    *budget
}

// budget is what is left to read, every tree sharing it together.
type budget struct {
	mu   sync.Mutex
	left int64
}

func (b *budget) remaining() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.left
}

func (b *budgeted) Open(name string) (fs.File, error) {
	f, err := b.under.Open(name)
	if err != nil {
		return nil, err
	}
	return &budgetedFile{File: f, name: name, of: b.of}, nil
}

// ReadFile reads a file whole, refusing one that says it weighs more than what is left before a
// byte of it is read, and one that turns out to as it is read.
func (b *budgeted) ReadFile(name string) ([]byte, error) {
	f, err := b.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > b.of.remaining() {
		return nil, readTooMuch(name)
	}
	return io.ReadAll(f)
}

// readTooMuch is a file past what the validation reads.
func readTooMuch(name string) error {
	return fmt.Errorf("%s takes what the validation reads out of a tree past %d bytes, every file it reads together: an entry point, its includes and its schemas are text a person writes and reviews", name, ReadMaxBytes)
}

type budgetedFile struct {
	fs.File
	name string
	of   *budget
}

// Read reads at most one byte past what is left, which is how a file past it is told from one that
// ends exactly at it.
func (f *budgetedFile) Read(p []byte) (int, error) {
	f.of.mu.Lock()
	left := f.of.left
	f.of.mu.Unlock()
	if left < 0 {
		return 0, readTooMuch(f.name)
	}
	if int64(len(p)) > left+1 {
		p = p[:left+1]
	}
	n, err := f.File.Read(p)
	f.of.mu.Lock()
	f.of.left -= int64(n)
	over := f.of.left < 0
	f.of.mu.Unlock()
	if over {
		return 0, readTooMuch(f.name)
	}
	return n, err
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
