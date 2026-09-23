package secret_test

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
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
// imports it today, for the shape of a push and the limits on its tree, and the main package of
// the API's own binary will once there is one. So nothing wires the store into the API's process
// until that main package is on this list too, whether the providers live under api or beside it,
// since it links them either way; and while cmd/agk imports the API, the API reaching the store
// puts the store in the command line as well, which this test names.
var mayRead = map[string]bool{
	"api": true,
}

// TestOnlyTheAPIReadsASecret reads the module and names every package that reaches the store.
func TestOnlyTheAPIReadsASecret(t *testing.T) {
	tr, err := readTree("..")
	if err != nil {
		t.Fatal(err)
	}

	// A walk that read nothing would pass this test by looking at no code at all, and one that
	// skipped a directory would pass it by not looking there, which is how the root went unread
	// in the first version of this test.
	if tr.files < 150 {
		t.Fatalf("read the imports of %d files, and the module holds more than that", tr.files)
	}
	if len(tr.packages) < 20 {
		t.Fatalf("found %d packages, and the module holds more than that", len(tr.packages))
	}
	for _, pkg := range []string{".", "api", "secret", "controller", "cmd/agk"} {
		if !slices.Contains(tr.packages, pkg) {
			t.Fatalf("the walk did not find %s, so it is not reading the module it is meant to", named(pkg))
		}
	}

	found, err := tr.reaching()
	if err != nil {
		t.Fatal(err)
	}
	for _, through := range found {
		t.Errorf("%s: the API is the only component that reads a secret value", chain(through))
	}
}

// TestTheSecretBoundaryIsCheckedAndNotAssumed holds the check itself, since a boundary test that
// would pass whatever it was given is a comment with a func keyword in front of it.
//
// The module it is handed holds what the first version of this test let through while the
// controller's closure held the store: a package under api importing the store, the controller
// importing that package, and a file at the root importing the store. Each of those is named now,
// as is a package reaching the root and one reaching a provider written beside the built-in store.
// So is a package reaching the store through each kind of directory ./... leaves out: a testdata
// directory, one whose name starts with a dot or an underscore, and one a symbolic link names. The
// go command builds an import of any of those like any other, so a package that links the store
// through one links it all the same.
//
// The walk reads every file of a package and goes through every package it reaches. The
// controller is three files, and the one that imports the store is read neither first nor last.
// The API is not named, and the command line importing it is, for what the API reaches.
//
// Nothing else is named: not the store's own packages, not a package whose only way to the store
// is a test file, and not one in a testdata directory that nothing imports.
func TestTheSecretBoundaryIsCheckedAndNotAssumed(t *testing.T) {
	root := t.TempDir()
	for path, imports := range map[string][]string{
		"doc.go":                       {"secret"},
		".leak/leak.go":                {"secret"},
		"_leak/leak.go":                {"secret"},
		"_linked/linked.go":            {"secret"},
		"agk/agk.go":                   {"internal/ulid"},
		"api/api.go":                   {"agk", "db", "secret"},
		"api/store/store.go":           {"secret"},
		"artifact/artifact.go":         {"agk", "artifact/linked"},
		"brick/brick.go":               {"agk", ".leak"},
		"bus/bus.go":                   {"."},
		"cmd/agk/push.go":              {"agk", "api"},
		"controller/a.go":              {"agk"},
		"controller/core.go":           {"agk", "api/store", "db"},
		"controller/z.go":              {"db"},
		"db/db.go":                     {"agk"},
		"driver/driver.go":             {"agk", "secret/vault"},
		"graph/graph.go":               {"agk"},
		"graph/graph_test.go":          {"secret"},
		"graph/testdata/leak.go":       {"secret"},
		"internal/ulid/ulid.go":        nil,
		"runner/runner.go":             {"agk", "runner/testdata/leak"},
		"runner/testdata/leak/leak.go": {"secret"},
		"schema/schema.go":             {"agk", "_leak"},
		"secret/seal.go":               {"internal/ulid"},
		"secret/vault/vault.go":        {"secret"},
	} {
		// Every file imports the standard library as well, which the walk has to step over.
		var src strings.Builder
		src.WriteString("package p\n\nimport (\n\t\"strings\"\n")
		for _, imported := range imports {
			if imported == "." {
				fmt.Fprintf(&src, "\t_ %q\n", module)
			} else {
				fmt.Fprintf(&src, "\t_ %q\n", module+"/"+imported)
			}
		}
		src.WriteString(")\n")
		path = filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join("..", "_linked"), filepath.Join(root, "artifact", "linked")); err != nil {
		t.Fatal(err)
	}

	tr, err := readTree(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{".", "api", "graph", "secret", "secret/vault"} {
		if !slices.Contains(tr.packages, pkg) {
			t.Fatalf("the walk did not find %s, so what it does not name below says nothing", named(pkg))
		}
	}

	want := map[string][]string{
		".":          {".", "secret"},
		"api/store":  {"api/store", "secret"},
		"artifact":   {"artifact", "artifact/linked", "secret"},
		"brick":      {"brick", ".leak", "secret"},
		"bus":        {"bus", ".", "secret"},
		"cmd/agk":    {"cmd/agk", "api", "secret"},
		"controller": {"controller", "api/store", "secret"},
		"driver":     {"driver", "secret/vault"},
		"runner":     {"runner", "runner/testdata/leak", "secret"},
		"schema":     {"schema", "_leak", "secret"},
	}
	found, err := tr.reaching()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, through := range found {
		got[through[0]] = through
		if _, ok := want[through[0]]; !ok {
			t.Errorf("the check says %s, and %s is the API, the store, a package whose closure holds neither, or one the check does not start from", chain(through), named(through[0]))
		}
	}
	for pkg, through := range want {
		if !slices.Equal(got[pkg], through) {
			t.Errorf("the check should say %s, and it says %s", chain(through), chain(got[pkg]))
		}
	}
}

// A tree is a module on disk, read a package at a time.
type tree struct {
	root string

	// packages is where the check starts: every package ./... matches, by its path inside the
	// module, with "." for the root.
	packages []string

	// imports is every package read so far, keyed as packages is, holding the packages of the
	// module its non-test files import.
	imports map[string][]string

	// files is how many files have been read.
	files int
}

// readTree finds the packages of the module rooted at root, and reads the imports of each.
func readTree(root string) (*tree, error) {
	tr := &tree{root: root, imports: map[string][]string{}}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.IsDir():
			// What ./... leaves out, this leaves out as well, and only here, where the check
			// starts. The go command still builds an import of a package in one of these like
			// any other, and the walk follows it there through importsOf, which also reads a
			// directory a symbolic link names where WalkDir would not go.
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
		if pkg := filepath.ToSlash(dir); !slices.Contains(tr.packages, pkg) {
			tr.packages = append(tr.packages, pkg)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(tr.packages)
	for _, pkg := range tr.packages {
		if _, err := tr.importsOf(pkg); err != nil {
			return nil, err
		}
	}
	return tr, nil
}

// importsOf answers the packages of the module that the non-test files of pkg import, reading
// its directory the first time it is asked.
func (tr *tree) importsOf(pkg string) ([]string, error) {
	if imports, ok := tr.imports[pkg]; ok {
		return imports, nil
	}

	dir := filepath.Join(tr.root, filepath.FromSlash(pkg))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var imports []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		tr.files++
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return nil, err
			}
			if inside, ok := insideTheModule(imported); ok && !slices.Contains(imports, inside) {
				imports = append(imports, inside)
			}
		}
	}
	slices.Sort(imports)
	tr.imports[pkg] = imports
	return imports, nil
}

// reaching answers, for every package held to the boundary whose closure holds the store, the
// shortest line of imports that takes it there: the package first and the store last.
func (tr *tree) reaching() ([][]string, error) {
	var found [][]string
	for _, pkg := range tr.packages {
		if mayRead[pkg] || isTheStore(pkg) {
			continue
		}
		through, err := tr.towardsTheStore(pkg)
		if err != nil {
			return nil, err
		}
		if through != nil {
			found = append(found, through)
		}
	}
	return found, nil
}

// towardsTheStore walks the closure of from breadth first, so that the line it answers is the
// shortest one, and answers nothing when the closure does not hold the store.
func (tr *tree) towardsTheStore(from string) ([]string, error) {
	cameFrom := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		imports, err := tr.importsOf(pkg)
		if err != nil {
			return nil, err
		}
		for _, next := range imports {
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
				return through, nil
			}
			queue = append(queue, next)
		}
	}
	return nil, nil
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
