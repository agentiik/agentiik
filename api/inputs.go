package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sync"
	"testing/fstest"

	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/schema"
)

// A run's inputs, bound here against the declaration of the version it runs.
//
// "Already held to their declared schemas with required and default applied" is what a run's
// inputs are when they are written, and every client starts a run through this route: agk, the
// console, a Terraform action, curl. So they are bound here and nowhere else, by the same
// graph.Workflow.DeclaredInputs and schema.Bind agk run --local binds them with, and a request
// the declaration refuses is answered 422 naming the input, before any run exists. What is written
// is what was bound, defaults included, so a run reads the same inputs whoever started it.
//
// They are decoded to be bound with every number read as it was written, a json.Number, which is
// how agk run --local reads an input and how the controller reads the run back: "a number written
// without a fraction or an exponent is an int in an expression, and any other a double", in all
// three. Decoding costs values more than bytes, and the inputs were counted as they were read, so this
// costs no more than the controller already pays at every decision it takes on the run.

// bindInputs binds the inputs a request supplied against the declaration of one version, and
// answers them as the run records them. Whatever it refuses it has answered.
func (s *Server) bindInputs(w http.ResponseWriter, ctx context.Context, over Target, commit string, g *graph.Graph, sent jsontext.Value) (json.RawMessage, bool) {
	var supplied map[string]any
	if len(sent) > 0 {
		d := json.NewDecoder(bytes.NewReader(sent))
		d.UseNumber()
		if err := d.Decode(&supplied); err != nil {
			// Read already, as one object of names and values, so this is a document the body
			// reader let through and should not have.
			fail(w, http.StatusBadRequest, "the inputs are not an object naming each input")
			return nil, false
		}
	}

	key := over.Namespace + "/" + over.Workflow + "@" + commit
	declared, held, done := s.declared.claim(ctx, key)
	if !held && done == nil {
		// Asked for by a caller that went away while another start compiled the declaration.
		fail(w, http.StatusServiceUnavailable, "the version's declaration was being compiled when the request ended")
		return nil, false
	}
	if !held {
		compiled := false
		defer func() { done(declared, compiled) }()
		tree := &versionTree{ctx: ctx, pool: s.pool, objects: s.objects, namespace: over.Namespace, workflow: over.Workflow, commit: commit}
		var err error
		declared, err = g.Workflow().DeclaredInputs(tree)
		switch {
		case errors.Is(tree.trouble, errNoObjectStore):
			fail(w, http.StatusServiceUnavailable, "this installation has no object store attached, and an input's schema names a file of the version's tree, which is kept there")
			return nil, false
		case tree.trouble != nil:
			// A caller who went away is not trouble for whoever runs the installation.
			if ctx.Err() == nil {
				s.report(fmt.Errorf("the tree of %s/%s@%s could not be read to bind a run's inputs: %w", over.Namespace, over.Workflow, commit, tree.trouble))
			}
			fail(w, http.StatusInternalServerError, "the version's tree could not be read")
			return nil, false
		case err != nil:
			// The version's own declaration, which a push refuses since this route binds against
			// it, so only a version pushed before then can hold one. Nothing the request changes
			// would start it.
			fail(w, http.StatusUnprocessableEntity, err.Error())
			return nil, false
		}
		compiled = true
	}

	bound, err := schema.Bind(declared, supplied)
	var refused *schema.InputRefusal
	if errors.As(err, &refused) {
		refuseInput(w, refused)
		return nil, false
	}
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return nil, false
	}
	// Held to the bounds the inputs sent were held to, since the defaults the workflow declares
	// are read by the controller at every decision it takes on the run exactly as they are.
	if n := values(bound); n > inputsMaxValues {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the inputs, with the defaults the workflow declares, hold %d values, and a run's inputs hold at most %d, as many as one envelope may carry items: a run's data belongs in an artifact", n, inputsMaxValues))
		return nil, false
	}
	// Without HTML escaping, which would write a < sent as one byte in six.
	var encoded bytes.Buffer
	e := json.NewEncoder(&encoded)
	e.SetEscapeHTML(false)
	if err := e.Encode(bound); err != nil {
		fail(w, http.StatusInternalServerError, "the inputs could not be written")
		return nil, false
	}
	if int64(encoded.Len()) > startMaxBytes {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the inputs, with the defaults the workflow declares, are written in %d bytes, and a run's inputs weigh at most %d, one envelope: a run's data belongs in an artifact", encoded.Len(), startMaxBytes))
		return nil, false
	}
	return bytes.TrimSuffix(encoded.Bytes(), []byte("\n")), true
}

// values counts a decoded document as the body reader counts one: every object, array, string,
// number, boolean and null, at any depth, and the document itself.
func values(v any) int {
	n := 1
	switch v := v.(type) {
	case map[string]any:
		for _, e := range v {
			n += values(e)
		}
	case []any:
		for _, e := range v {
			n += values(e)
		}
	}
	return n
}

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

// refuseInput answers the input a request gets wrong: 422, with the sentence agk run --local
// prints for it, and the input and the rule apart, so that a form can point at the field without
// reading the sentence. The rule is the key a person finds in the workflow file: required,
// schema, or undeclared for a name the workflow does not declare.
func refuseInput(w http.ResponseWriter, r *schema.InputRefusal) {
	w.Header().Set("Cache-Control", "no-store")
	write(w, http.StatusUnprocessableEntity, map[string]string{
		"error": r.Error(), "input": r.Input, "rule": r.Rule,
	})
}

// errNoObjectStore is a tree to read with nowhere to read it from.
var errNoObjectStore = errors.New("no object store is attached")

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
		return nil, t.failed(name, errNoObjectStore)
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
