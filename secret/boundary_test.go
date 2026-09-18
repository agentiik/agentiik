package secret_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// "The API is the only component that reads one." The controller "names which secret a task may
// have and never sees its value", a runner holds "no secret-store credential", and a brick sees a
// file the runner wrote and never the store.
//
// So this package has exactly one importer, and until the API exists it has none. A rule nothing
// checks is a preference, and this one is the difference between a design in which one component
// can read a secret and a design in which one component is supposed to.

// mayImport is the packages allowed to reach this one, by their path inside the module.
//
// The API, and nothing else. It is empty of the API today because the API is not written, which
// is worth leaving visible: the list is what somebody has to add to, and adding to it is the
// moment to ask whether the new importer really is the one component that reads a value.
var mayImport = map[string]bool{
	"api": true,
}

const module = "github.com/agentiik/agentiik/"

// TestOnlyTheAPIReadsASecret walks the module and names anything that reaches this package.
func TestOnlyTheAPIReadsASecret(t *testing.T) {
	const self = module + "secret"

	read := 0
	var importers []string
	err := filepath.Walk("..", func(path string, info os.FileInfo, err error) error {
		switch {
		case err != nil:
			return err
		case info.IsDir():
			if info.Name() == "testdata" || info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		case !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}

		// This package's own files are not importers of it.
		if filepath.Dir(path) == ".." || strings.HasPrefix(filepath.ToSlash(path), "../secret/") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		read++
		for _, spec := range file.Imports {
			if spec.Path.Value != `"`+self+`"` {
				continue
			}
			where := strings.TrimPrefix(filepath.ToSlash(filepath.Dir(path)), "../")
			if !mayImport[where] && !mayImport[firstSegment(where)] {
				importers = append(importers, where+" ("+path+")")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A walk that read nothing would pass this test by looking at no code at all.
	if read < 40 {
		t.Fatalf("read the imports of %d files, and the module holds more than that", read)
	}
	if len(importers) != 0 {
		t.Errorf("these reach the secret store, and the API is the only component that reads a secret value: %v", importers)
	}
}

func firstSegment(path string) string {
	if i := strings.Index(path, "/"); i >= 0 {
		return path[:i]
	}
	return path
}
