package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// This program is mounted inside an image the project does not control, read-only, at a
// path a script calls by name. What it links is therefore not an implementation detail: it
// is what every container that runs a script step carries, and it is the difference between
// a helper and an Agentiik library inside somebody else's brick.
//
// The rule is package agk and the standard library. A rule nothing checks is a preference,
// so this reads the whole non-test import closure and names whatever is in it that should
// not be, on the precedent graph/boundary_test.go set.
//
// Test files are not read, and deliberately: what the shipped binary costs is what the
// non-test files import, and the tests beside them import brick and driver on purpose, to
// hold the paths and the variables this package spells for itself to the packages that own
// them.

// module is this module's path, with the trailing slash, so that a module-internal import
// is told from a third-party one by a prefix and not by a guess.
const module = "github.com/agentiik/agentiik/"

// insideTheModule is the whole of what this binary may reach for: the vocabulary, and the
// identifier mint the vocabulary itself reaches for.
var insideTheModule = map[string]bool{
	"agk":           true,
	"internal/ulid": true,
}

// refused is everything this program would have become if one of these appeared in its
// closure, each with the reason, so that a failure says what was breached rather than that
// a list was not matched.
//
// exact marks the paths refused as themselves and not as a prefix: net is a socket, and
// net/url or net/http are different arguments. net/http is named separately because it is
// the one a convenience like this grows by accident.
var refused = []struct {
	path  string
	what  string
	exact bool
}{
	{path: "net", what: "a socket, and this program reads files the container already has", exact: true},
	{path: "net/http", what: "an HTTP client, and there is nothing in a container for it to reach: the store is unreachable by design and the daemon socket is never mounted"},
	{path: "net/rpc", what: "a remote procedure call"},
	{path: "net/smtp", what: "a network client"},
	{path: "os/exec", what: "a process started outside this one, and a container may have no shell to start one with"},
	{path: "os/user", what: "a resolvable user account, which a scratch image has no /etc/passwd for"},
	{path: "os/signal", what: "a lifecycle, and this program is a command a script runs and waits for"},
	{path: "plugin", what: "a dynamically loaded object, which is what static excludes"},
	{path: "database/sql", what: "a database"},
	{path: "runtime/cgo", what: "a libc this program may not assume the image has"},
	{path: "testing", what: "a test, and a test binary is not what gets mounted"},
}

// refusedInside is what a package of this module may not be called for this binary to
// import it. Each of them is the engine, and the engine has no business inside the thing it
// is running.
var refusedInside = map[string]string{
	"graph":      "the evaluator, which would put a CEL compiler inside every container",
	"driver":     "the container driver, which would put a Docker Engine API client inside every container",
	"schema":     "the JSON Schema implementation",
	"brick":      "the contract's other side, which names the same paths and pulls a YAML parser in behind them",
	"artifact":   "the object store, which a container cannot reach and must not hold a client for",
	"controller": "the controller",
	"runner":     "the runner",
	"docker":     "a Docker client",
	"expr":       "a CEL compiler",
}

// TestTheClosureIsTheStandardLibraryAndTheVocabulary walks the closure and names whatever is
// in it that the mount does not allow.
func TestTheClosureIsTheStandardLibraryAndTheVocabulary(t *testing.T) {
	closure := map[string]bool{}
	read := map[string]int{}

	queue := []string{"."}
	seen := map[string]bool{".": true}

	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]

		dir := pkg
		if pkg != "." {
			dir = filepath.Join("..", "..", pkg)
		}
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
			read[pkg]++
			for _, spec := range file.Imports {
				imported, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				closure[imported] = true
				if why := whyRefused(imported); why != "" {
					t.Errorf("%s imports %s, which is %s: this binary is mounted inside an image the project does not control, and may link package agk and the standard library", path, imported, why)
					continue
				}
				if inside, ok := strings.CutPrefix(imported, module); ok && !seen[inside] {
					seen[inside] = true
					queue = append(queue, inside)
				}
			}
		}
	}

	// A rule nobody read is a rule nobody keeps. This program is six files and its
	// closure reaches the vocabulary and the mint behind it.
	if read["."] < 6 {
		t.Fatalf("read the imports of %d files of this program, which is fewer than it holds", read["."])
	}
	if len(read) < 3 {
		t.Fatalf("walked %d packages, and the closure reaches the vocabulary and the identifier mint behind it", len(read))
	}

	names := make([]string, 0, len(closure))
	for imported := range closure {
		names = append(names, imported)
	}
	slices.Sort(names)
	t.Logf("the closure is %s", strings.Join(names, ", "))
}

// TestTheBoundaryIsCheckedAndNotAssumed holds the check itself. A boundary test that would
// pass whatever it was given is a comment with a func keyword in front of it.
func TestTheBoundaryIsCheckedAndNotAssumed(t *testing.T) {
	for _, imported := range []string{
		"net",
		"net/http",
		"net/http/httptest",
		"os/exec",
		"os/user",
		"os/signal",
		"database/sql",
		"plugin",
		"testing",
		"github.com/goccy/go-yaml",
		"github.com/santhosh-tekuri/jsonschema/v6",
		"github.com/google/cel-go/cel",
		"github.com/docker/docker/client",
		module + "graph",
		module + "driver",
		module + "brick",
		module + "schema",
		module + "artifact",
		module + "internal/docker",
		module + "internal/expr",
		module + "cmd/agk",
		module + "cmd/agk/internal/helper",
	} {
		if whyRefused(imported) == "" {
			t.Errorf("%s passes the boundary check, and it is one of the things the boundary is about", imported)
		}
	}
	for _, imported := range []string{
		"fmt",
		"os",
		"io",
		"flag",
		"time",
		"strings",
		"strconv",
		"crypto/sha256",
		"encoding/hex",
		"encoding/json",
		"io/fs",
		"mime",
		"path/filepath",
		module + "agk",
		module + "internal/ulid",
	} {
		if why := whyRefused(imported); why != "" {
			t.Errorf("%s is refused as %s, and this program may reach for it", imported, why)
		}
	}
}

// whyRefused says what an import path would make this program, or nothing where the mount
// allows it.
//
// Everything not named is refused, and deliberately: a dependency arrives with a reason
// written in go.mod, and one arriving in here arrives inside every container that runs a
// script step, so a check that let an unnamed one through would be testing a list of known
// villains rather than a boundary.
func whyRefused(imported string) string {
	for _, r := range refused {
		if imported == r.path || (!r.exact && strings.HasPrefix(imported, r.path+"/")) {
			return r.what
		}
	}

	if inside, ok := strings.CutPrefix(imported, module); ok {
		for _, segment := range strings.Split(inside, "/") {
			if what, no := refusedInside[segment]; no {
				return what
			}
		}
		if !insideTheModule[inside] {
			return "not one of the two packages this binary may reach for"
		}
		return ""
	}

	// The standard library is everything whose first segment carries no dot, which is
	// the reading agk's own closure test takes.
	if !strings.Contains(strings.Split(imported, "/")[0], ".") {
		return ""
	}
	return "a dependency, and this binary carries none"
}
