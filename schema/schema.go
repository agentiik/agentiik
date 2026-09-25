// Package schema is JSON Schema 2020-12 for the schemas a user writes, as opposed to
// the ones the engine owns, and the one boundary rule that is not JSON Schema's:
// required and default written on a workflow input itself.
//
// A workflow declares its inputs as the boundary a trigger fills, each one "validated
// against its schema before any step runs". An input's schema is written as
// { $ref: "./schemas/order.json" } and resolves against the commit it travelled with,
// so the compiler resolves a reference against the mounted repository tree and refuses
// any reference that leaves it: validating a workflow never reaches the network, and a
// schema can never be a file the run was not given.
//
// This is the only package where a third party JSON Schema implementation appears. It
// is isolated here because the graph evaluator needs the same three calls for brick
// params against the manifest schema, and the container driver for port schemas, and
// because the vocabulary package is imported by everything: a JSON Schema
// implementation linked into the console, the runner and the command line to carry four
// struct types is a cost nothing there pays for.
//
// Envelope validation is not done here. The envelope is the engine's own shape, fixed
// at compile time and on the hot path of every port and every item, and package agk
// checks it by hand.
package schema

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// treeBase is the base URI every schema document written in the workflow file is
// compiled under. A reference is resolved against it, so "./schemas/order.json" becomes
// agk://repo/schemas/order.json and the loader below reads schemas/order.json out of
// the tree.
//
// The trailing slash is load bearing twice over. It makes the base name a directory, so
// a relative reference resolves against the root of the tree rather than against a
// sibling of some file; and because no file in the tree can ever be addressed by it, an
// inline schema can never shadow a committed one.
//
// The scheme is the project's own rather than file: or https: so that every reference
// arrives at the loader below, including an absolute one an author wrote by hand.
const treeBase = "agk://repo/"

// Compiler compiles the JSON Schema documents a workflow writes, resolving every
// reference against one repository tree.
//
// A Compiler is safe for concurrent use, and each Compile builds its own evaluator, so
// that no compiled resource is shared between two documents and the duplicate-$id
// question that comes with compiling two documents against one resource set never
// arises.
//
// What it does share is the files of the tree, read and parsed once for the Compiler's
// life. A declaration is compiled one input at a time, at every start of a run on an
// installation, and twenty inputs naming one schema file would otherwise read and parse
// it twenty times: a megabyte schema named by forty inputs held 665 MiB for one request.
// A parsed file is only read by what compiles against it, and a file is the same bytes
// for as long as the tree is, which is the life of the Compiler.
//
// Parsing once is not compiling once: each document still compiles every file it
// reaches, and compiling costs more than the bytes, since the library's time grows with
// the square of a document's subschemas. So a Compiler may be given a weight it compiles
// no more than, every document and every file each document reaches counted each time,
// which is what NewCompilerWithin is for.
type Compiler struct {
	fsys fs.FS

	mu      sync.Mutex
	parsed  map[string]parsed
	most    int64
	reached int64
}

// parsed is one file of the tree as the loader first answered it, its failure included,
// so that a file that could not be read is not asked for again by the next input.
type parsed struct {
	doc  any
	size int64
	err  error
}

// NewCompiler returns a Compiler that resolves references against fsys, the repository
// tree at the commit the run pinned.
//
// A nil fsys is a compiler for inline schemas alone: every reference is refused, which
// is the reading that cannot be wrong. A run that was given no tree cannot be holding
// the file the reference names, and resolving it anywhere else is the network access
// this package exists to rule out.
func NewCompiler(fsys fs.FS) *Compiler {
	return &Compiler{fsys: fsys, parsed: map[string]parsed{}}
}

// NewCompilerWithin returns a Compiler that refuses to compile more than most bytes of
// schema in all, over every call: each document compiled, and each file a document
// reaches, counted once for every document that reaches it.
func NewCompilerWithin(fsys fs.FS, most int64) *Compiler {
	c := NewCompiler(fsys)
	c.most = most
	return c
}

// charge counts n more bytes compiled, and refuses them past the Compiler's weight.
// Called with mu held.
func (c *Compiler) charge(n int64) error {
	if c.most > 0 && c.reached+n > c.most {
		return fmt.Errorf("the schemas compiled here reach past the %d bytes they may weigh in all, counting a file once for each document that reaches it", c.most)
	}
	c.reached += n
	return nil
}

// Compile compiles one JSON Schema 2020-12 document, given as the JSON encoding of the
// value written under an input's schema key. The workflow language accepts an object of
// keywords or a boolean there, and so does this.
//
// A failure here is a broken workflow rather than a refused run: the schema itself does
// not hold together, or names a file the commit does not carry. The caller says which
// input it came from, because a compiler that guessed would be guessing.
func (c *Compiler) Compile(doc []byte) (*Schema, error) {
	return c.compile(doc, "")
}

// CompileAt compiles one schema out of a larger document, named by a JSON Pointer
// resolved against that document.
//
// A brick manifest is written that way. A definition under spec.definitions is named
// "#/spec/definitions/<name> and never #/definitions/<name>", and the schema of a port
// or of a parameter is a fragment of the manifest that may reference one. So the
// manifest is the resource and the pointer names the part of it to compile, which is
// what Compile cannot do: it adds the value it is given as the whole resource, and a
// same-document reference would then resolve against the fragment rather than against
// the manifest it was written in.
//
// The pointer is written as the manifest writes it, with the leading "#". An empty
// pointer compiles the document itself, which is what Compile does.
func (c *Compiler) CompileAt(document []byte, pointer string) (*Schema, error) {
	if pointer != "" && !strings.HasPrefix(pointer, "#") {
		return nil, fmt.Errorf("pointer %q is not written as a same-document reference: it names a place in the document it was written in, so it begins with #", pointer)
	}
	return c.compile(document, pointer)
}

// compile is the one compilation path: the document is the resource, and the fragment
// says which part of it is the schema.
func (c *Compiler) compile(doc []byte, fragment string) (*Schema, error) {
	c.mu.Lock()
	err := c.charge(int64(len(doc)))
	c.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("schema is not compiled: %w", err)
	}

	// UseNumber throughout, so that a large integer bound written in a schema is the
	// number the author wrote and not the nearest float64 to it.
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return nil, fmt.Errorf("schema is not a JSON document: %w", err)
	}

	jc := jsonschema.NewCompiler()
	// Pinned rather than left to the library's latest, because the workflow language
	// says 2020-12 and a document that changes meaning when a dependency is upgraded is
	// a version that no longer describes what ran.
	jc.DefaultDraft(jsonschema.Draft2020)
	jc.UseLoader(&treeLoader{fsys: c.fsys, c: c})
	if err := jc.AddResource(treeBase, v); err != nil {
		return nil, fmt.Errorf("schema could not be read: %w", err)
	}
	s, err := jc.Compile(treeBase + fragment)
	if err != nil {
		return nil, fmt.Errorf("schema does not compile: %w", err)
	}
	return &Schema{s: s}, nil
}

// Schema is one compiled JSON Schema 2020-12 document.
type Schema struct {
	s *jsonschema.Schema
}

// Validate refuses v when it departs from the schema. v is a decoded JSON value: nil, a
// bool, a string, a number, a []any or a map[string]any.
//
// The error names every place the value departed and how, on one line, because it is
// read in a log beside the input it refused and not in a terminal that will pretty-print
// it. It carries the text and not the library's own error value: the JSON Schema
// implementation is this package's choice to make and unmake, and a caller that reached
// through the error would be holding it to that choice.
func (s *Schema) Validate(v any) error {
	err := s.s.Validate(v)
	if err == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err
	}
	return errors.New(oneLine(ve))
}

// oneLine renders a validation failure as instance location and message, joined, so
// that "/amount: minimum: got -3, want 0; /lines/1: missing property 'sku'" is one log
// line. The library's own rendering opens with the schema's URI, which here is the
// synthetic agk://repo/ and means nothing to the person reading it.
//
// The detailed output is walked rather than the basic one because the basic output
// flattens a $ref away and prints the reference's own "validation failed" over the
// message underneath it, which is exactly the message worth keeping: an input's schema
// is written as a $ref more often than not.
func oneLine(ve *jsonschema.ValidationError) string {
	parts := leaves(*ve.DetailedOutput(), nil)
	if len(parts) == 0 {
		return ve.Error()
	}
	return strings.Join(parts, "; ")
}

// leaves collects the places the value actually departed from the schema. The branches
// above them carry no message of their own, only the keyword that got there.
func leaves(u jsonschema.OutputUnit, into []string) []string {
	if len(u.Errors) > 0 {
		for _, sub := range u.Errors {
			into = leaves(sub, into)
		}
		return into
	}
	if u.Error == nil {
		return into
	}
	if u.InstanceLocation == "" {
		return append(into, u.Error.String())
	}
	return append(into, u.InstanceLocation+": "+u.Error.String())
}

// treeLoader resolves a reference against the repository tree and refuses everything
// else. It is the whole of the rule that a schema travels with the commit: a reference
// that names another host, another scheme or a file the tree does not carry is refused
// at compile time, so no run can be started by a schema fetched from somewhere.
type treeLoader struct {
	fsys fs.FS
	c    *Compiler
}

func (l *treeLoader) Load(raw string) (any, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("reference %q is not a URL: %w", raw, err)
	}
	// A relative reference was resolved against treeBase before it arrived here, so
	// anything still carrying another scheme or another host is a reference the author
	// wrote absolute, and it is refused as written rather than resolved anywhere.
	if u.Scheme != "agk" || u.Host != "repo" {
		return nil, fmt.Errorf("reference %q leaves the repository tree", raw)
	}
	// Past this point the synthetic base has done its work and the reference is a path
	// in the tree, which is what the messages below name: the base is this package's
	// own machinery and reading it in an error would send an author looking for a
	// scheme they never wrote. URL resolution has also already removed every ".."
	// segment, so the path cannot climb out of the tree.
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" || !fs.ValidPath(name) {
		return nil, fmt.Errorf("reference resolves to %q, which is not a path in the repository tree", name)
	}
	if l.fsys == nil {
		return nil, fmt.Errorf("reference to %q cannot be resolved: the run has no repository tree", name)
	}
	// Held for the whole read, so that two inputs naming one file at once read it once.
	l.c.mu.Lock()
	defer l.c.mu.Unlock()
	p, held := l.c.parsed[name]
	if !held {
		b, err := fs.ReadFile(l.fsys, name)
		if err != nil {
			p.err = fmt.Errorf("reference to %q is not in the repository tree: %w", name, err)
		} else {
			p.size = int64(len(b))
			p.doc, p.err = jsonschema.UnmarshalJSON(bytes.NewReader(b))
		}
		l.c.parsed[name] = p
	}
	if p.err != nil {
		return nil, p.err
	}
	if err := l.c.charge(p.size); err != nil {
		return nil, fmt.Errorf("reference to %q: %w", name, err)
	}
	return p.doc, nil
}
