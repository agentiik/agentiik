package secret_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// "The API is the only component that reads one." The controller "names which secret a task may
// have and never sees its value", a runner holds "no secret-store credential", and a brick sees a
// file the runner wrote and never the store.
//
// So only the API may reach this package. A rule nothing checks is a preference, and this one is
// the difference between a design in which one component can read a secret and a design in which
// one component is supposed to.
//
// Reaching is the closure and not the import list. A package that imports one that imports the
// store links the store as surely as if it had named it, so this reads every package of the
// module, the root included, follows the imports that stay inside the module to the end, and
// names each package whose closure holds the store. It is the walk graph/boundary_test.go takes,
// started from every package rather than from one, because the question here is who can reach a
// package and not what a package can reach.
//
// Test files are not read, here as there. What a component links is what its non-test files
// import, and a test may reach for the store without any of that reaching a component.

// module is this module's path, which is also the import path of the package at its root.
const module = "github.com/agentiik/agentiik"

// theStore is what a secret value is read through, by path inside the module. A package under
// one of these is part of it too, so a provider written beside the built-in store is held to the
// same boundary without anybody having to remember it; one written anywhere else is added here.
var theStore = []string{"secret"}

// mayRead is the packages allowed to reach the store, by their path inside the module.
//
// The API, and nothing else, and the API exactly: a package under api is a package of its own,
// which anything may import, and allowing it by its prefix would let the controller reach the
// store through it. The list is what somebody has to add to, and adding to it is the moment to
// ask whether the new importer really is the one component that reads a value.
//
// Being on the list excuses a package from the check and from nothing else. The walk goes through
// it like any other, so a package that imports the API reaches whatever the API reaches. cmd/agk
// imports it today for the shape of a push and the limits on its tree, and while it does, the API
// reaching the store would put the store in the command line's closure and this test would say
// so: where a provider lives decides whether the command line links it.
var mayRead = map[string]bool{
	"api": true,
}

// TestOnlyTheAPIReadsASecret reads the module and names every package that reaches the store.
func TestOnlyTheAPIReadsASecret(t *testing.T) {
	imports, read, err := importsOf("..")
	if err != nil {
		t.Fatal(err)
	}

	// A walk that read nothing would pass this test by looking at no code at all, and one that
	// skipped a directory would pass it by not looking there, which is how the root went unread
	// in the first version of this test.
	if read < 150 {
		t.Fatalf("read the imports of %d files, and the module holds more than that", read)
	}
	if len(imports) < 20 {
		t.Fatalf("found %d packages, and the module holds more than that", len(imports))
	}
	for _, pkg := range []string{".", "api", "secret", "controller", "cmd/agk"} {
		if _, ok := imports[pkg]; !ok {
			t.Fatalf("the walk did not find %s, so it is not reading the module it is meant to", named(pkg))
		}
	}

	for _, through := range reaching(imports) {
		t.Errorf("%s: the API is the only component that reads a secret value", chain(through))
	}
}

// importsOf reads the imports of every non-test file of the module rooted at root, and answers
// them by package: each keyed by its path inside the module, with "." for the root, and holding
// the packages of the module it imports. It also answers how many files it read.
func importsOf(root string) (map[string][]string, int, error) {
	imports := map[string][]string{}
	read := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.IsDir():
			// What the go command ignores, this ignores as well.
			name := entry.Name()
			if path != root && (name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		case !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}

		dir, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		pkg := filepath.ToSlash(dir)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		read++
		if _, ok := imports[pkg]; !ok {
			imports[pkg] = nil
		}
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if inside, ok := insideTheModule(imported); ok && !slices.Contains(imports[pkg], inside) {
				imports[pkg] = append(imports[pkg], inside)
			}
		}
		return nil
	})
	for pkg := range imports {
		slices.Sort(imports[pkg])
	}
	return imports, read, err
}

// reaching answers, for every package held to the boundary whose closure holds the store, the
// shortest line of imports that takes it there: the package first and the store last.
func reaching(imports map[string][]string) [][]string {
	var held []string
	for pkg := range imports {
		if !mayRead[pkg] && !isTheStore(pkg) {
			held = append(held, pkg)
		}
	}
	slices.Sort(held)

	var found [][]string
	for _, pkg := range held {
		if through := towardsTheStore(imports, pkg); through != nil {
			found = append(found, through)
		}
	}
	return found
}

// towardsTheStore walks the closure of from breadth first, so that the line it answers is the
// shortest one, and answers nothing when the closure does not hold the store.
func towardsTheStore(imports map[string][]string, from string) []string {
	cameFrom := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		for _, next := range imports[pkg] {
			if _, seen := cameFrom[next]; seen {
				continue
			}
			cameFrom[next] = pkg
			if isTheStore(next) {
				through := []string{next}
				for at := pkg; at != ""; at = cameFrom[at] {
					through = append(through, at)
				}
				slices.Reverse(through)
				return through
			}
			queue = append(queue, next)
		}
	}
	return nil
}

func isTheStore(pkg string) bool {
	for _, store := range theStore {
		if pkg == store || strings.HasPrefix(pkg, store+"/") {
			return true
		}
	}
	return false
}

// insideTheModule answers an import path's path inside the module, with "." for the root, or
// nothing when it names a package of the standard library or of another module.
func insideTheModule(imported string) (string, bool) {
	if imported == module {
		return ".", true
	}
	return strings.CutPrefix(imported, module+"/")
}

// chain says a line of imports the way a person would: "controller imports api/store, which
// imports secret".
func chain(through []string) string {
	if len(through) == 0 {
		return "nothing"
	}
	var b strings.Builder
	b.WriteString(named(through[0]))
	for i, pkg := range through[1:] {
		if i == 0 {
			b.WriteString(" imports ")
		} else {
			b.WriteString(", which imports ")
		}
		b.WriteString(named(pkg))
	}
	return b.String()
}

// named is how a message spells a package: its path inside the module, or the root by name,
// since a lone dot in a sentence reads as the end of it.
func named(pkg string) string {
	if pkg == "." {
		return "the module root"
	}
	return pkg
}
