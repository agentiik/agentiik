// Package yamlbound bounds what a YAML document stands for once its aliases are followed, before
// and after it is decoded.
//
// An alias repeats the value its anchor names, and an anchored value may hold aliases itself, so
// each level of nesting multiplies what the level below stands for: ten lines of a few dozen bytes,
// each naming the line before nine times, stand for three billion values, and one alias to a long
// scalar, repeated, stands for as many copies of it. The decoder shares an anchored value between
// its aliases, and what multiplies it is what reads the decoded value afterwards, on the language's
// own terms, visiting every alias as a value and writing every copy of a scalar out again. Such a
// document, in a workflow file anybody holding workflow:write pushes or in the /agk/brick.yaml of any
// image a step names, holds the API, the hook or a runner for as long as it takes to run out of
// memory: at the release this package arrived, 430 bytes took two seconds to read, and each further
// line nine times as long.
//
// So a document is held to a budget, in values and in bytes, twice. Check counts it on its syntax
// tree, before anything decodes it, where an alias is a name: each anchor's count is kept at its
// definition and added again at each alias naming it, and a merge key is an alias. That count is
// what the decoder builds only where a name means one thing throughout the document, since the
// decoder reads a merged anchor's tree again at every merge, and resolves the names inside it as
// they stand at that moment; so a document defining one anchor name twice, which nobody writing a
// workflow or a manifest has a reason to, is refused, and so is an alias naming no anchor defined
// before it. CheckValue then walks the decoded value before anything reads it, counting what a
// reader of it would visit, so that what the first count could not foresee is still stopped before
// it costs more than the budget.
//
// It is the defence yaml.v3 made against the same attack, a ceiling on what aliases add, counted
// rather than detected, here counted on the tree and on the value rather than as the decoder goes,
// since this decoder offers no hook to count with.
package yamlbound

import (
	"errors"
	"fmt"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/lexer"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"
)

// The budget a document is held to, once its aliases are followed.
const (
	// MaxValues is the most values a document may stand for: 250,000, about a tenth of a second
	// of a reader's time, measured on a brick manifest at the release this package arrived. A
	// workflow file or a brick manifest a person writes and reviews is some hundreds of values, a
	// large one some thousands, and anchors that repeat a block a few times add a few times that.
	MaxValues = 250_000

	// MaxBytes is the most its keys and scalars may weigh, each counted wherever it is repeated:
	// 16 MiB, four times the largest tree the tree push carries whole, so that no document a person
	// writes comes near it, and what reading one on the language's terms and writing it out as JSON
	// costs stays tens of megabytes.
	MaxBytes = 16 << 20
)

// budget is what a count may reach.
type budget struct{ values, bytes int64 }

// ErrTooMuch is a document standing for more than its budget once its aliases are followed.
var ErrTooMuch = errors.New("the document stands for more once its aliases are followed than it may")

// Check refuses a document larger than MaxBytes, nested deeper than MaxDepth or naming a value by a
// path longer than MaxPath, as structure says; one standing for more than MaxValues values or
// MaxBytes bytes once its aliases are followed; one defining an anchor name twice; and one with an
// alias naming no anchor defined before it. A document that does not parse is left for the decoder
// to refuse, in its own words.
func Check(doc []byte) error { return check(doc, budget{MaxValues, MaxBytes}) }

func check(doc []byte, most budget) error {
	if int64(len(doc)) > most.bytes {
		return tooMuch(most)
	}
	if err := structure(string(doc)); err != nil {
		return err
	}
	file, err := parser.ParseBytes(doc, 0)
	if err != nil {
		return nil
	}
	c := counter{anchors: map[string]count{}, most: most}
	var total count
	for _, d := range file.Docs {
		total = c.add(total, c.size(d))
		if c.err != nil {
			return c.err
		}
		if total.over(most) {
			return tooMuch(most)
		}
	}
	return nil
}

func tooMuch(most budget) error {
	return fmt.Errorf("%w: more than %d values or %d bytes, where an alias repeats what its anchor names and nested ones multiply, so that a few lines can stand for billions of values; it is refused before it is read", ErrTooMuch, most.values, most.bytes)
}

// count is what a node stands for.
type count struct{ values, bytes int64 }

func (c count) over(most budget) bool { return c.values > most.values || c.bytes > most.bytes }

type counter struct {
	// anchors are what each anchor stands for, by name.
	anchors map[string]count
	most    budget
	err     error
}

// add is a + b, stopped just past the budget, so that a count past it stays past it without
// overflowing.
func (c *counter) add(a, b count) count {
	sum := count{a.values + b.values, a.bytes + b.bytes}
	if a.over(c.most) || b.over(c.most) || sum.over(c.most) {
		return count{c.most.values + 1, c.most.bytes + 1}
	}
	return sum
}

// size is what a node stands for: a scalar is one value and its length, a collection one value and
// what its members stand for, an alias what its anchor stands for.
func (c *counter) size(n ast.Node) count {
	if c.err != nil {
		return count{}
	}
	switch n := n.(type) {
	case nil:
		return count{}
	case *ast.DocumentNode:
		return c.size(n.Body)
	case *ast.MappingNode:
		total := count{values: 1}
		for _, v := range n.Values {
			total = c.add(total, c.size(v))
		}
		return total
	case *ast.MappingValueNode:
		return c.add(c.size(n.Key), c.size(n.Value))
	case *ast.MappingKeyNode:
		return c.size(n.Value)
	case *ast.SequenceNode:
		total := count{values: 1}
		for _, v := range n.Values {
			total = c.add(total, c.size(v))
		}
		return total
	case *ast.AnchorNode:
		name := ""
		if n.Name != nil {
			name = n.Name.String()
		}
		if _, twice := c.anchors[name]; twice {
			c.err = fmt.Errorf("the anchor &%.64s is defined twice: a name one alias reads as one value and another as another is refused, since the decoder reads an anchor again wherever it is merged", name)
			return count{}
		}
		size := c.size(n.Value)
		c.anchors[name] = size
		return size
	case *ast.AliasNode:
		name := ""
		if n.Value != nil {
			name = n.Value.String()
		}
		size, ok := c.anchors[name]
		if !ok {
			c.err = fmt.Errorf("the alias *%.64s names no anchor defined before it", name)
			return count{}
		}
		return size
	case *ast.TagNode:
		return c.size(n.Value)
	case *ast.CommentGroupNode:
		return count{}
	case *ast.MergeKeyNode:
		return count{values: 1, bytes: 2}
	}
	if scalar, ok := n.(ast.ScalarNode); ok {
		return count{values: 1, bytes: int64(len(fmt.Sprint(scalar.GetValue())))}
	}
	return count{values: 1, bytes: int64(len(n.String()))}
}

// CheckValue refuses a decoded document standing for more than MaxValues values or MaxBytes bytes,
// counted as a reader of it visits it: every alias the decoder shared is visited as often as it is
// named. It stops at the budget, so that what it costs is bounded whatever the document.
func CheckValue(v any) error { return checkValue(v, budget{MaxValues, MaxBytes}) }

func checkValue(v any, most budget) error {
	w := walker{most: most}
	w.walk(v)
	if w.seen.over(most) {
		return tooMuch(most)
	}
	return nil
}

type walker struct {
	seen count
	most budget
}

func (w *walker) walk(v any) {
	if w.seen.over(w.most) {
		return
	}
	w.seen.values++
	switch value := v.(type) {
	case map[string]any:
		for k, sub := range value {
			w.seen.bytes += int64(len(k))
			w.walk(sub)
		}
	case map[any]any:
		for k, sub := range value {
			w.seen.bytes += int64(len(fmt.Sprint(k)))
			w.walk(sub)
		}
	case []any:
		for _, sub := range value {
			w.walk(sub)
		}
	case string:
		w.seen.bytes += int64(len(value))
	default:
		w.seen.bytes += 8
	}
}

// The shape a document may have before the parser reads it.
const (
	// MaxDepth is how deeply a document may nest, mappings and sequences together: 128, where a
	// workflow file nests a dozen levels and a JSON Schema in a manifest a few dozen.
	MaxDepth = 128

	// MaxPath is how long the path to a value may be, the keys above it joined, in bytes: 1 KiB,
	// four times what the language's longest names, a step, a port and a parameter under their
	// blocks, take together.
	MaxPath = 1024
)

// structure refuses a document nested deeper than MaxDepth, or naming a value by a path longer than
// MaxPath, read from its tokens alone, which the lexer makes in one pass.
//
// The parser keeps, for every node it makes, the path to it as text, the keys above it joined, built
// by adding the node's own key to its parent's: what it costs is the length of every path, summed
// over every node. A document nesting a hundred thousand levels, or holding one long key over many
// values, costs the square of its size that way, a quarter of an hour for one megabyte, before
// anything the aliases stand for is counted, and the parser offers no way to leave the paths out. So
// the path to each token is followed here as the parser will build it, a key pushed where a ':'
// follows it and popped where the block or the flow collection holding it ends, and a document
// whose path runs past the bound, or whose nesting does, is refused before it is parsed.
func structure(src string) error {
	type frame struct {
		flow   bool
		column int
		weight int
		key    bool
	}
	var stack []frame
	weight := 0
	pop := func() {
		weight -= stack[len(stack)-1].weight
		stack = stack[:len(stack)-1]
	}
	inFlow := func() bool {
		for _, f := range stack {
			if f.flow {
				return true
			}
		}
		return false
	}
	line := 0
	var last *token.Token
	for _, tk := range lexer.Tokenize(src) {
		switch tk.Type {
		case token.CommentType, token.SpaceType:
			continue
		case token.MappingKeyType:
			return errors.New("the document writes an explicit key, ? and a key, which the language has no use for: a key is written as its name and a colon")
		}
		if tk.Position != nil && tk.Position.Line != line {
			line = tk.Position.Line
			// A token beginning a line outside a flow collection ends every block at its
			// column or deeper, save a mapping's key at its own column where a sequence is
			// written under it, as YAML lets one be.
			if !inFlow() {
				for len(stack) > 0 {
					top := stack[len(stack)-1]
					if top.column < tk.Position.Column || (top.column == tk.Position.Column && top.key && tk.Type == token.SequenceEntryType) {
						break
					}
					pop()
				}
			}
		}
		switch tk.Type {
		case token.SequenceStartType, token.MappingStartType:
			stack = append(stack, frame{flow: true, weight: 3})
			weight += 3
		case token.SequenceEndType, token.MappingEndType:
			for len(stack) > 0 {
				flow := stack[len(stack)-1].flow
				pop()
				if flow {
					break
				}
			}
		case token.CollectEntryType:
			// The next entry of a flow collection is under the collection alone.
			for len(stack) > 0 && !stack[len(stack)-1].flow {
				pop()
			}
		case token.SequenceEntryType:
			if !inFlow() && tk.Position != nil {
				stack = append(stack, frame{column: tk.Position.Column, weight: 3})
				weight += 3
			}
		case token.MappingValueType:
			if last != nil {
				column := 0
				if last.Position != nil {
					column = last.Position.Column
				}
				w := len(last.Value) + 3
				stack = append(stack, frame{flow: false, column: column, weight: w, key: true})
				weight += w
				if inFlow() {
					// A flow mapping's key is under its collection until a comma or its end.
					stack[len(stack)-1].column = -1
				}
			}
		}
		if len(stack) > MaxDepth {
			return fmt.Errorf("the document nests deeper than %d levels, which the parser reads in the square of the depth: it is refused before it is read", MaxDepth)
		}
		if weight > MaxPath {
			return fmt.Errorf("the document names a value by a path longer than %d bytes, the keys above it joined, which the parser copies once for every value beneath it: it is refused before it is read", MaxPath)
		}
		last = tk
	}
	return nil
}
