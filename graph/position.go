package graph

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml/ast"

	"github.com/agentiik/agentiik/agk"
)

// Position is where a refusal points: the file of the tree, and the line and column its node
// begins at, each counted from 1, as a YAML 1.2 parser counts them. Line and Column are zero
// where the refusal is about the file as a whole, or about something no one place in it holds,
// and File is empty where it is about no file of the tree at all, a brick manifest's among them.
//
// "The refusal is written on git's error stream as file:line:column: message (rule)", so that a
// person holding the file is sent to the line rather than to the key somewhere above it.
//
// Pointer is the same node as a JSON Pointer into the file, RFC 6901's, which a client holding the
// document as data rather than as text is sent to: "an invalid draft returns an error carrying a
// JSON Pointer and what was expected", and a model correcting a draft edits a value, not a column.
// It is set where Line is, and empty, the whole document, where the refusal is about the file.
type Position struct {
	File    string
	Line    int
	Column  int
	Pointer string
}

// String is the position as a compiler writes one: file:line:column, file alone where no line is
// known, and nothing where there is no file.
func (p Position) String() string {
	switch {
	case p.File == "":
		return ""
	case p.Line == 0:
		return p.File
	}
	return p.File + ":" + strconv.Itoa(p.Line) + ":" + strconv.Itoa(p.Column)
}

// source is one file a workflow was read from: its path in the tree and the document as parsed.
//
// The path is set once the loader knows it, which is after the document was read: Parse and
// ParseFragment are handed bytes, and only Load knows which file of the tree they came from. Every
// origin read out of the document shares the one source, so naming the file once names it
// everywhere a value of it is remembered.
type source struct {
	name string
	doc  *ast.File
}

// origin is where one value was written: the file, and the path of its node from the root of the
// document, each step a key of a mapping or an index of a list.
//
// It is what a refusal about a resolved step is placed with. A step's image may come from an
// included file, its outputs from the entry point and its timeout from a block three files away,
// and the line a refusal names has to be the one that wrote the value refused, not the step's.
type origin struct {
	src  *source
	path []any
}

// at is the origin of a node below this one.
func (o origin) at(more ...any) origin {
	path := make([]any, 0, len(o.path)+len(more))
	return origin{src: o.src, path: append(append(path, o.path...), more...)}
}

// value is where the value at this origin begins, and key where the key it is written under
// does: the name of a workflow output is a key, and so is a keyword an included file may not
// carry, which is refused for being written at all.
func (o origin) value() Position { return o.position(false) }
func (o origin) key() Position   { return o.position(true) }

func (o origin) position(key bool) Position {
	if o.src == nil {
		return Position{}
	}
	p := Position{File: o.src.name}
	if o.src.doc == nil || len(o.src.doc.Docs) == 0 {
		return p
	}
	node := o.src.doc.Docs[0].Body
	var keyNode ast.Node
	for _, step := range o.path {
		keyNode, node = child(node, step)
		if node == nil {
			return p
		}
	}
	at := node
	if key && keyNode != nil {
		at = keyNode
	}
	if t := at.GetToken(); t != nil && t.Position != nil {
		p.Line, p.Column = t.Position.Line, t.Position.Column
		p.Pointer = Pointer(o.path...)
	}
	return p
}

// Pointer is a path through a document, each step a key of a mapping or an index of a list, as a
// JSON Pointer: "/" before every step, and in a key ~ written ~0 and / written ~1, RFC 6901's
// escapes in that order. No step at all is the empty pointer, the whole document.
func Pointer(path ...any) string {
	var b strings.Builder
	for _, step := range path {
		b.WriteByte('/')
		switch step := step.(type) {
		case int:
			b.WriteString(strconv.Itoa(step))
		case string:
			b.WriteString(strings.ReplaceAll(strings.ReplaceAll(step, "~", "~0"), "/", "~1"))
		default:
			b.WriteString(fmt.Sprint(step))
		}
	}
	return b.String()
}

// child is one step down the document: the key and the value of a mapping entry, or the value at
// an index of a list. It answers nil for a step the document does not hold.
func child(node ast.Node, step any) (ast.Node, ast.Node) {
	node = unwrap(node)
	switch step := step.(type) {
	case string:
		var values []*ast.MappingValueNode
		switch n := node.(type) {
		case *ast.MappingNode:
			values = n.Values
		case *ast.MappingValueNode:
			values = []*ast.MappingValueNode{n}
		}
		for _, v := range values {
			if keyText(v.Key) == step {
				return v.Key, v.Value
			}
		}
	case int:
		if n, ok := node.(*ast.SequenceNode); ok && step >= 0 && step < len(n.Values) {
			return nil, n.Values[step]
		}
	}
	return nil, nil
}

// unwrap looks through the nodes that decorate a value without being it: a tag and an anchor.
func unwrap(node ast.Node) ast.Node {
	for {
		switch n := node.(type) {
		case *ast.TagNode:
			node = n.Value
		case *ast.AnchorNode:
			node = n.Value
		default:
			return node
		}
	}
}

// keyText is a key as the decoded document spells it, quotes and all taken off.
func keyText(key ast.MapKeyNode) string {
	if key == nil {
		return ""
	}
	if t := unwrap(key).GetToken(); t != nil {
		return t.Value
	}
	return ""
}

// Entry is the path of the entry point in the tree the workflow was loaded from, and empty for
// a workflow parsed from bytes alone.
func (w *Workflow) Entry() string {
	if w == nil || w.src == nil {
		return ""
	}
	return w.src.name
}

// Included is every include resolution applied, in the order it applied them: what a file
// includes before the file itself, a file two others include once, where it was first reached.
// A path include of another repository's files is that repository's and is not among them.
func (w *Workflow) Included() []Included { return slices.Clone(w.included) }

// MetadataAt is where the entry point writes metadata.name, metadata.namespace or
// metadata.labels, which a refusal of the name a push is made under points at.
func (w *Workflow) MetadataAt(key string) Position {
	return origin{src: w.src, path: []any{"metadata", key}}.value()
}

// SecretAt is where a secret the workflow names was first named: its entry in the secrets block
// of the entry point, or of the first file resolution read that names it.
func (w *Workflow) SecretAt(name string) Position {
	if at, ok := w.secretAt[name]; ok {
		return at.value()
	}
	return Position{File: w.Entry()}
}

// StepAt is where a keyword of a resolved step was written: in the step itself, in a block it
// extends or in defaults, in whichever file the layer that won came from. below names a node
// under the keyword, an index of a list or a key of a block, and the deepest of them the
// document holds is the one answered, so that an edge written in its short form, a step name
// alone, is pointed at as the step name.
func (w *Workflow) StepAt(step agk.Step, keyword string, below ...any) Position {
	return w.stepAt(false, step, keyword, below...)
}

// stepKeyAt is StepAt pointing at the last key named rather than at its value: a port an inputs
// block feeds is a key.
func (w *Workflow) stepKeyAt(step agk.Step, keyword string, below ...any) Position {
	return w.stepAt(true, step, keyword, below...)
}

func (w *Workflow) stepAt(key bool, step agk.Step, keyword string, below ...any) Position {
	at, ok := w.origins[step][keyword]
	// A parameter merges by name, so the layer that wrote the one named may not be the last
	// layer that wrote params.
	if name, named := firstName(below); named && keyword == "params" {
		if param, held := w.origins[step][paramKey(name)]; held {
			at, ok = param, true
		}
	}
	if !ok {
		return Position{File: w.Entry()}
	}
	for n := len(below); n >= 0; n-- {
		p := at.at(append([]any{keyword}, below[:n]...)...).position(key && n == len(below))
		if p.Line > 0 {
			return p
		}
	}
	return Position{File: at.src.name}
}

// firstName is the first node named below a keyword, where it is a key.
func firstName(below []any) (string, bool) {
	if len(below) == 0 {
		return "", false
	}
	name, ok := below[0].(string)
	return name, ok
}

// place puts a refusal where it points, and answers it.
func place(r *Refusal, at Position) *Refusal {
	r.At = at
	return r
}

// expressionAt is where an expression was written, given the path a refusal names it by:
// steps.<step>.<keyword> and what is below, or a key of the on block.
func (w *Workflow) expressionAt(step agk.Step, at string) Position {
	path := dotted(at)
	if len(path) >= 3 && path[0] == "steps" && path[1] == string(step) {
		if keyword, ok := path[2].(string); ok {
			return w.StepAt(step, keyword, path[3:]...)
		}
	}
	return origin{src: w.src, path: path}.value()
}

// dotted reads a path written a.b[2].c into its steps.
func dotted(at string) []any {
	var path []any
	for _, part := range strings.Split(at, ".") {
		name, rest, indexed := strings.Cut(part, "[")
		if name != "" {
			path = append(path, name)
		}
		for indexed {
			var index string
			index, rest, _ = strings.Cut(rest, "]")
			if n, err := strconv.Atoi(index); err == nil {
				path = append(path, n)
			}
			_, rest, indexed = strings.Cut(rest, "[")
		}
	}
	return path
}
