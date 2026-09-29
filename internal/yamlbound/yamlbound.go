// Package yamlbound bounds what a YAML document stands for once its aliases are followed, before
// anything decodes it.
//
// An alias repeats the value its anchor names, and an anchored value may hold aliases itself, so
// each level of nesting multiplies what the level below stands for: ten lines of a few dozen bytes,
// each naming the line before nine times, stand for three billion values. The decoder expands every
// alias it meets into a value of its own, and nothing in it bounds how many, so such a document,
// in a workflow file anybody holding workflow:write pushes or in the /agk/brick.yaml of any image a
// step names, holds the API, the hook or a runner for as long as it takes to run out of memory: at
// the release this package arrived, 430 bytes took two seconds to read, and each further line nine
// times as long. The decoder shares an anchored value between its aliases; what multiplies it is
// reading the decoded value on the language's own terms, which visits every alias as a value.
//
// So the document is parsed first into its syntax tree, where an alias is a name and nothing is
// expanded, and what it stands for is counted there, each anchor's count kept at its definition
// and added again at each alias naming it, which costs a pass over what is written. A document
// standing for more than it may is refused before it is decoded. It is the defence yaml.v3 made
// against the same attack, a ceiling on what aliases add, counted rather than detected, here
// counted on the tree rather than as the decoder goes, since this decoder offers no hook to count
// with.
package yamlbound

import (
	"fmt"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// MaxValues is the most values a document may stand for once its aliases are followed: 250,000,
// about a tenth of a second of the reader's time and twenty megabytes, measured on a brick manifest
// at the release this package arrived. A workflow
// file or a brick manifest a person writes and reviews is some hundreds of values, a large one some
// thousands, and anchors that repeat a block a few times add a few times that: far below it,
// where a document built to exhaust whoever reads it is far above it after a handful of lines.
const MaxValues = 250_000

// Check refuses a document standing for more than most values once its aliases are followed. A
// document that does not parse is left for the decoder to refuse, in its own words.
func Check(doc []byte, most int) error {
	file, err := parser.ParseBytes(doc, 0)
	if err != nil {
		return nil
	}
	c := counter{anchors: map[string]int64{}, most: int64(most)}
	for _, d := range file.Docs {
		c.total = add(c.total, c.size(d), c.most)
		if c.total > c.most {
			return fmt.Errorf("the document stands for more than %d values once its aliases are followed: an alias repeats what its anchor names and nested ones multiply, so a few lines can stand for billions of values, and it is refused before it is read", most)
		}
	}
	return nil
}

type counter struct {
	// anchors are what each anchor stands for, by name, as last defined before where the walk
	// is: YAML lets a later anchor take a name again, and an alias names the latest before it.
	anchors map[string]int64
	total   int64
	most    int64
}

// size is what a node stands for, counted in values: a scalar is one, a collection one and what
// its members stand for, an alias what its anchor stands for. Every sum stops just past most, so
// that a count past it stays past it without overflowing.
func (c *counter) size(n ast.Node) int64 {
	switch n := n.(type) {
	case nil:
		return 0
	case *ast.DocumentNode:
		return c.size(n.Body)
	case *ast.MappingNode:
		total := int64(1)
		for _, v := range n.Values {
			total = add(total, c.size(v), c.most)
		}
		return total
	case *ast.MappingValueNode:
		return add(c.size(n.Key), c.size(n.Value), c.most)
	case *ast.MappingKeyNode:
		return c.size(n.Value)
	case *ast.SequenceNode:
		total := int64(1)
		for _, v := range n.Values {
			total = add(total, c.size(v), c.most)
		}
		return total
	case *ast.AnchorNode:
		size := c.size(n.Value)
		if n.Name != nil {
			c.anchors[n.Name.String()] = size
		}
		return size
	case *ast.AliasNode:
		if n.Value == nil {
			return 1
		}
		size, ok := c.anchors[n.Value.String()]
		if !ok {
			// An alias to no anchor, which the decoder refuses.
			return 1
		}
		return size
	case *ast.TagNode:
		return c.size(n.Value)
	case *ast.CommentGroupNode:
		return 0
	}
	return 1
}

// add is a + b, stopped at most + 1.
func add(a, b, most int64) int64 {
	if a > most || b > most || a+b > most {
		return most + 1
	}
	return a + b
}
