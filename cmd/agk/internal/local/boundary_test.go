package local

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// doc.go says what this package may reach for and why: "The test for that is the import
// list: graph for what runs next, driver for containers, artifact for where bytes go, schema
// for nothing at all and brick for nothing at all, because driver is between
// brick.WriteInputs and brick.Collect and this is above driver."
//
// A constraint nothing checks is a preference, so this is the list read back off the files.
// It is the direct imports and not the closure: what this package may call is the question,
// and the closure of driver is driver's own business. Test files are not read, because a
// test may reach for a fake daemon without any of that reaching a caller.

// allowed is what a local run is made of: the vocabulary, the store it writes bytes through,
// the evaluator that decides what runs next, and the driver that runs it.
//
// brick is in the list for one name and is held to it below. A manifest travels from the
// driver, which reads it out of an image, to graph.Build, which holds each step to it, and
// this package is what carries it between the two: naming the type is unavoidable and reading
// the contract's directories is what would be wrong.
var allowed = map[string]string{
	"agk":      "the vocabulary every one of these speaks",
	"artifact": "the store, opened over a directory",
	"graph":    "the evaluator, which decides what runs next",
	"driver":   "the containers, and the only package in this module that may reach a daemon",
	"brick":    "the manifest type alone, carried from the driver to graph.Build",
}

// refused is what this package would have become if one of these appeared in its imports.
var refused = map[string]string{
	"schema":          "the input boundary, which is applied before a run exists and not inside one",
	"internal/docker": "the Engine API, which only driver may reach",
	"internal/expr":   "the expression language, which only the evaluator may reach",
}

func TestWhatALocalRunMayReachFor(t *testing.T) {
	const module = "github.com/agentiik/agentiik/"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package: %s", err)
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("reading the imports of %s: %s", name, err)
		}
		for _, imported := range f.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatalf("%s: %s is not a quoted path", name, imported.Path.Value)
			}
			inside, ok := strings.CutPrefix(path, module)
			if !ok {
				// The standard library, which this package may use freely: a
				// directory, a clock and a channel are what a loop is made of.
				if strings.Contains(strings.SplitN(path, "/", 2)[0], ".") {
					t.Errorf("%s imports %s: a local run takes no dependency of its own, and the three this module has are recorded in go.mod with their reasons", name, path)
				}
				continue
			}
			seen[inside] = true
			if why, no := refused[inside]; no {
				t.Errorf("%s imports %s: %s", name, path, why)
				continue
			}
			if _, yes := allowed[inside]; !yes {
				t.Errorf("%s imports %s, which is not one of the four a local run is made of", name, path)
			}
		}
	}

	// And each of them is actually used, because a list nobody reaches is a list that
	// stopped being true.
	for name, why := range allowed {
		if !seen[name] {
			t.Errorf("nothing here imports %s, which is %s: either the wiring moved or this list did", name, why)
		}
	}
}

// TestBrickIsNamedForItsManifestAndNothingElse holds the one allowance above to what it is
// for. A manifest is a value this package carries; the two edges of the container are the
// driver's, and brick.WriteInputs or brick.Collect appearing here would be a second
// collection above the one that already exists.
//
// It reads the syntax tree and not the text, because doc.go names brick.WriteInputs and
// brick.Collect in a sentence about where they belong, and a test that could not tell a
// sentence from a call would make that sentence unwritable.
func TestBrickIsNamedForItsManifestAndNothingElse(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package: %s", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("reading %s: %s", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			selector, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok || pkg.Name != "brick" {
				return true
			}
			if selector.Sel.Name != "Manifest" {
				t.Errorf("%s names brick.%s: a manifest is what this package carries, and the two edges of the container are the driver's", name, selector.Sel.Name)
			}
			return true
		})
	}
}
