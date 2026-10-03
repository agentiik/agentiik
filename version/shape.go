package version

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/goccy/go-yaml"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/yamlbound"
	"github.com/agentiik/agentiik/language"
)

// A document refused for its shape: a key the language does not have, a value off its grammar, an
// enumeration it does not list. The parser reads the file closed, by hand, and says what is wrong in
// the documentation's words, naming the key; "that this reader agrees with the released schema is
// held by corpus_test.go", which runs every fixture the schema refuses through it. So where the
// parser refused a file for its shape, the released schema refuses it too, and says where, as a JSON
// Pointer, and against what: what the problem is told with is the parser's sentence, placed and
// explained by the schema.

// shaped is a refusal of a document's shape, as the problem it is told as.
type shaped struct {
	problem Problem
	err     error
}

func (s *shaped) Error() string { return s.err.Error() }
func (s *shaped) Unwrap() error { return s.err }

// ruleShape is the rule a refusal of shape is told under: "refused_by: schema", as the corpus marks
// every fixture the schema refuses, since the schema is what a reader holds the file to for it.
const ruleShape = "schema"

// placeShape is err placed by the schema where the parser refused a file for its shape: the first
// file read, in the order resolution read them, that the schema refuses, the entry point held to the
// workflow's schema and an included file to a fragment's. An error the schema finds nothing to say
// about is answered as it was.
func placeShape(err error, read []readFile, entry string) error {
	if err == nil || errors.Is(err, graph.ErrRefused) {
		return err
	}
	for _, f := range read {
		if ext := path.Ext(f.name); ext != ".yaml" && ext != ".yml" {
			continue
		}
		fragment := f.name != entry || graph.IsLibrary(f.doc)
		if p, ok := againstSchema(f.name, f.doc, fragment); ok {
			p.Detail = err.Error()
			return &shaped{problem: p, err: err}
		}
	}
	return err
}

// readFile is one file resolution read, by its path in the tree.
type readFile struct {
	name string
	doc  []byte
}

// againstSchema holds one file to the released schema, and answers where it refuses it.
func againstSchema(name string, doc []byte, fragment bool) (Problem, bool) {
	// Bounded before it is read again, as the parser bounded it: a file the parser refused for its
	// aliases is not one to expand here.
	if yamlbound.Check(doc) != nil {
		return Problem{}, false
	}
	data, err := yaml.YAMLToJSON(doc)
	if err != nil {
		return Problem{}, false
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return Problem{}, false
	}
	workflow, fragmentSchema, err := schemas()
	if err != nil {
		return Problem{}, false
	}
	held := workflow
	if fragment {
		held = fragmentSchema
	}
	var ve *jsonschema.ValidationError
	if !errors.As(held.Validate(v), &ve) {
		return Problem{}, false
	}
	leaf := deepest(ve)
	pointer := pointerOf(leaf.InstanceLocation)
	// A key the block does not take is placed at the key itself, which is what is to be taken out.
	extra, isExtra := leaf.ErrorKind.(*kind.AdditionalProperties)
	if isExtra && len(extra.Properties) == 1 {
		pointer += graph.Pointer(extra.Properties[0])
	}
	at := graph.Locate(doc, pointer, isExtra)
	if at.Line > 0 {
		pointer = at.Pointer
	}
	p := Problem{File: name, Line: at.Line, Column: at.Column, Pointer: pointer, Rule: ruleShape, Expected: expectation(leaf.ErrorKind)}
	p.Topic = language.TopicOf(pointer)
	return p, true
}

// deepest is the cause of a validation error that names the deepest node, the first of those where
// several do: the one place a person is sent to, rather than every branch of a oneOf the value did
// not take.
func deepest(e *jsonschema.ValidationError) *jsonschema.ValidationError {
	best := e
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			if len(e.InstanceLocation) > len(best.InstanceLocation) || len(best.Causes) > 0 {
				best = e
			}
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(e)
	return best
}

// pointerOf is an instance location as a JSON Pointer.
func pointerOf(tokens []string) string {
	path := make([]any, len(tokens))
	for i, t := range tokens {
		path[i] = t
	}
	return graph.Pointer(path...)
}

// expectation is what a keyword the value failed expected there, in a person's words.
func expectation(k jsonschema.ErrorKind) string {
	quoted := func(vs []string) string {
		out := make([]string, len(vs))
		for i, v := range vs {
			out[i] = fmt.Sprintf("%q", v)
		}
		return strings.Join(out, ", ")
	}
	switch k := k.(type) {
	case *kind.Required:
		return "the key " + strings.Join(k.Missing, ", ") + ", which this block requires"
	case *kind.AdditionalProperties:
		return "only the keys this block takes, and not " + strings.Join(k.Properties, ", ")
	case *kind.Enum:
		want := make([]string, len(k.Want))
		for i, v := range k.Want {
			want[i] = fmt.Sprint(v)
		}
		return "one of " + quoted(want)
	case *kind.Const:
		return fmt.Sprintf("the value %v", k.Want)
	case *kind.Type:
		return "a value of type " + strings.Join(k.Want, " or ")
	case *kind.Pattern:
		return "a value matching " + k.Want
	case *kind.MinLength:
		return fmt.Sprintf("at least %d characters", k.Want)
	case *kind.MaxLength:
		return fmt.Sprintf("at most %d characters", k.Want)
	case *kind.MinItems:
		return fmt.Sprintf("at least %d entries", k.Want)
	case *kind.MaxItems:
		return fmt.Sprintf("at most %d entries", k.Want)
	case *kind.MinProperties:
		return fmt.Sprintf("at least %d keys", k.Want)
	case *kind.MaxProperties:
		return fmt.Sprintf("at most %d keys", k.Want)
	case *kind.PropertyNames:
		return "a key on the grammar this block's keys are written on, and not " + fmt.Sprintf("%q", k.Property)
	case *kind.Format:
		return "a value in the format " + k.Want
	case *kind.FalseSchema:
		return "nothing here: the language has no such key or value at this place"
	}
	return "what the schema says of this place, which workflow.schema reads whole"
}

// The workflow's schema and a fragment's, compiled once from the parts the language reference
// carries, which are the released schema.
var (
	compiled      sync.Once
	workflowHeld  *jsonschema.Schema
	fragmentHeld  *jsonschema.Schema
	compileFailed error
)

func schemas() (*jsonschema.Schema, *jsonschema.Schema, error) {
	compiled.Do(func() {
		doc, ok := language.Schema("workflow")
		if !ok {
			compileFailed = errors.New("version: the language carries no workflow schema")
			return
		}
		v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
		if err != nil {
			compileFailed = err
			return
		}
		const url = "https://schemas.agentiik.dev/workflow.schema.json"
		c := jsonschema.NewCompiler()
		if err := c.AddResource(url, v); err != nil {
			compileFailed = err
			return
		}
		if workflowHeld, compileFailed = c.Compile(url); compileFailed != nil {
			return
		}
		fragmentHeld, compileFailed = c.Compile(url + "#/$defs/fragment")
	})
	return workflowHeld, fragmentHeld, compileFailed
}

// sorted is what the watcher read, in the order it read it.
func (w *watcher) sorted() []readFile {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]readFile, 0, len(w.order))
	for _, name := range w.order {
		out = append(out, readFile{name: name, doc: w.read[name]})
	}
	return slices.Clip(out)
}
