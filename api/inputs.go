package api

import (
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
// They are decoded to be bound, as encoding/json decodes them, which is how agk run --local reads
// an input and how the controller reads the run back: a number is a 64-bit float in all three.
// Decoding costs values more than bytes, and the inputs were counted as they were read, so this
// costs no more than the controller already pays at every decision it takes on the run.

// bindInputs binds the inputs a request supplied against the declaration of one version, and
// answers them as the run records them. Whatever it refuses it has answered.
func (s *Server) bindInputs(w http.ResponseWriter, ctx context.Context, over Target, commit string, g *graph.Graph, sent jsontext.Value) (json.RawMessage, bool) {
	var supplied map[string]any
	if len(sent) > 0 {
		if err := json.Unmarshal(sent, &supplied); err != nil {
			// Read already, as an object of names and values none of which a float refuses, so
			// this is a document the body reader let through and should not have.
			fail(w, http.StatusBadRequest, "the inputs are not an object naming each input")
			return nil, false
		}
	}

	tree := &versionTree{ctx: ctx, pool: s.pool, objects: s.objects, namespace: over.Namespace, workflow: over.Workflow, commit: commit}
	declared, err := g.Workflow().DeclaredInputs(tree)
	switch {
	case errors.Is(tree.trouble, errNoObjectStore):
		fail(w, http.StatusServiceUnavailable, "this installation has no object store attached, and an input's schema names a file of the version's tree, which is kept there")
		return nil, false
	case tree.trouble != nil:
		s.report(fmt.Errorf("the tree of %s/%s@%s could not be read to bind a run's inputs: %w", over.Namespace, over.Workflow, commit, tree.trouble))
		fail(w, http.StatusInternalServerError, "the version's tree could not be read")
		return nil, false
	case err != nil:
		// The version's own declaration, which a push refuses since this route binds against it,
		// so only a version pushed before then can hold one. Nothing the request changes would
		// start it.
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return nil, false
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
	encoded, err := json.Marshal(bound)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the inputs could not be written")
		return nil, false
	}
	return encoded, true
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
