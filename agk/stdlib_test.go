package agk_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestTheClosureIsTheStandardLibrary holds the rule the layout states about this
// package: everything imports it, so what it costs an importer is what every other
// package pays. A JSON Schema implementation or an object store client linked into the
// console, the runner and the command line to carry four struct types is a cost nothing
// here pays for.
//
// The one package inside the module it may reach for is internal/ulid, which is held to
// the same rule, so the closure stays the standard library.
func TestTheClosureIsTheStandardLibrary(t *testing.T) {
	const module = "github.com/agentiik/agentiik/"

	var read int
	for _, dir := range []string{".", "../internal/ulid"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			read++
			for _, spec := range file.Imports {
				imported, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case strings.HasPrefix(imported, module):
					// The vocabulary mints identifiers, and what an
					// identifier is made of stays inside the module.
					if dir != "." || imported != module+"internal/ulid" {
						t.Errorf("%s imports %s, which is not the standard library", path, imported)
					}
				case strings.Contains(strings.Split(imported, "/")[0], "."):
					t.Errorf("%s imports %s, which is not the standard library", path, imported)
				}
			}
		}
	}
	// A rule nobody read is a rule nobody keeps.
	if read < 6 {
		t.Fatalf("read the imports of %d files, which is fewer than the packages hold", read)
	}
}
