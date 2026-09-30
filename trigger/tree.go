package trigger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"testing/fstest"

	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/schema"
)

// declarations are the compiled declarations of the versions runs were started of lately.
//
// A version is a commit and never changes, so its declaration compiles to the same thing every
// time, and compiling one can take a third of a second (graph.InputSchemasMaxBytes says why):
// kept, a thousand starts of one version compile it once, and starts that arrive together while
// it compiles wait for that one compile rather than each running their own. Bounded like the
// graphs version.Store keeps, and fewer, since a compiled declaration at its bound holds about
// 10 MiB.
type declarations struct {
	mu        sync.Mutex
	held      map[string]map[string]schema.Input
	order     []string
	compiling map[string]chan struct{}
}

// declarationsKept is how many compiled declarations stay held.
const declarationsKept = 16

// claim answers the declaration of key where one is held. Otherwise it answers done, and the
// caller compiles and then calls done with what it compiled and whether it did, which keeps it
// and lets the starts waiting on it go on. A caller whose context ends while another compiles is
// answered neither.
func (d *declarations) claim(ctx context.Context, key string) (map[string]schema.Input, bool, func(map[string]schema.Input, bool)) {
	for {
		d.mu.Lock()
		if declared, held := d.held[key]; held {
			d.mu.Unlock()
			return declared, true, nil
		}
		wait, busy := d.compiling[key]
		if !busy {
			if d.compiling == nil {
				d.compiling = map[string]chan struct{}{}
			}
			finished := make(chan struct{})
			d.compiling[key] = finished
			d.mu.Unlock()
			return nil, false, func(declared map[string]schema.Input, compiled bool) {
				d.mu.Lock()
				defer d.mu.Unlock()
				delete(d.compiling, key)
				close(finished)
				if compiled {
					d.keep(key, declared)
				}
			}
		}
		d.mu.Unlock()
		// A compile that failed keeps nothing, and the next to ask compiles again: a store that
		// did not answer may answer now.
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, false, nil
		}
	}
}

// keep holds one, dropping the oldest past declarationsKept. A compiled schema is only read once
// compiled, and schema.Bind copies a default before handing it on, so every start may share it.
// Called with mu held.
func (d *declarations) keep(key string, declared map[string]schema.Input) {
	if d.held == nil {
		d.held = map[string]map[string]schema.Input{}
	}
	d.held[key] = declared
	d.order = append(d.order, key)
	for len(d.order) > declarationsKept {
		delete(d.held, d.order[0])
		d.order = d.order[1:]
	}
}

// versionTree is the tree of one version as an fs.FS, which is what a schema's $ref resolves
// against: the files the version's containers see under /agk/repo, and no others.
//
// Read a file at a time and only when a schema names one, since most declarations name none and
// a start that read the tree for them would pay a database read and an object for nothing. What
// it could not read because of the installation rather than the version is kept in trouble, since
// the compiler that asked turns every failure into a sentence about the schema: a store that did
// not answer is not a workflow somebody has to fix.
//
// It serves one request, from one goroutine, and holds its context for that reason.
type versionTree struct {
	ctx     context.Context
	pool    *db.Pool
	objects artifact.Objects

	namespace, workflow, commit string

	files   map[string]db.TreeFile
	trouble error
}

func (t *versionTree) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if t.files == nil {
		var listed []db.TreeFile
		err := t.pool.In(t.ctx, t.namespace, func(ctx context.Context, ns *db.NS) error {
			var err error
			listed, err = ns.Tree(ctx, t.workflow, t.commit)
			return err
		})
		// A version recorded without its tree holds no file for a reference to name.
		if err != nil && !errors.Is(err, db.ErrNoTree) {
			return nil, t.failed(name, err)
		}
		t.files = make(map[string]db.TreeFile, len(listed))
		for _, f := range listed {
			t.files[f.Path] = f
		}
	}
	f, held := t.files[name]
	if !held {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	if t.objects == nil {
		return nil, t.failed(name, ErrNoObjectStore)
	}

	r, err := t.objects.Open(t.ctx, artifact.Key(t.namespace, f.SHA256))
	if err != nil {
		return nil, t.failed(name, err)
	}
	defer r.Close()
	// Held to the size and the digest the version names, as a runner holds every file of the
	// tree, and read no further than one byte past the size.
	body, err := io.ReadAll(io.LimitReader(r, f.Size+1))
	if err != nil {
		return nil, t.failed(name, err)
	}
	sum := sha256.Sum256(body)
	if int64(len(body)) != f.Size || hex.EncodeToString(sum[:]) != f.SHA256 {
		return nil, t.failed(name, fmt.Errorf("the object is not the %d bytes of digest %s the version names", f.Size, f.SHA256))
	}
	return fstest.MapFS{name: &fstest.MapFile{Data: body, Mode: 0o444}}.Open(name)
}

// failed keeps the first failure that was the installation's, and answers it as the compiler
// expects a file that could not be opened to be answered.
func (t *versionTree) failed(name string, err error) error {
	if t.trouble == nil {
		t.trouble = err
	}
	return &fs.PathError{Op: "open", Path: name, Err: err}
}
