package graph

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The evaluator is a library, importable with no controller, no task bus and no database
// behind it. That is the architectural constraint the root doc.go states, and it is what
// lets agk run --local be the same code path a server run takes instead of a second
// implementation that drifts from the first.
//
// A constraint nothing checks is a preference. This reads the imports of every file of
// the evaluator and of the expression door, follows the module-internal ones, and holds
// the whole closure to what the layout says it may be. It is the idiom
// agk/stdlib_test.go already uses for the vocabulary's own closure, applied to the other
// package in this module with a boundary worth keeping.
//
// Test files are not read. What an importer pays for is what the package imports, and a
// test may reach for the fixture corpus without any of that reaching an importer.

// boundaryModule is this module's path, with the trailing slash, so that a module
// internal import is told from a third party one by a prefix and not by a guess.
const boundaryModule = "github.com/agentiik/agentiik/"

// insideTheModule is what the layout says the evaluator may reach for: the vocabulary,
// the store, the brick contract, the schemas a user writes, the door CEL is behind, and
// the identifier mint that door and the vocabulary share.
var insideTheModule = map[string]bool{
	"agk":           true,
	"artifact":      true,
	"brick":         true,
	"schema":        true,
	"internal/expr": true,
	"internal/ulid": true,
}

// outsideTheModule is every third party package the closure may hold, each with the
// reason it is in it. Each one is recorded in go.mod as well, where the reason is
// written out; this is the same list read from the other end.
var outsideTheModule = []struct{ path, why string }{
	{"github.com/goccy/go-yaml", "the YAML 1.2 parser the entry point is read with"},
	{"github.com/google/cel-go", "the CEL implementation the expression language is"},
	{"cel.dev", "the CEL implementation the expression language is"},
	{"github.com/santhosh-tekuri/jsonschema", "the JSON Schema implementation, reached only through package schema, which isolates it"},
}

// refusedByName is what the evaluator would have become if any of these appeared in its
// closure. The list names the things the constraint is about, in the words the
// documentation uses for them, so that a failure says what was breached rather than that
// a list was not matched.
//
// exact marks the paths that are refused as themselves and not as a prefix: net is a
// socket and net/url is a URI parser, and package schema resolves a $ref with the
// second without ever opening the first.
var refusedByName = []struct {
	path  string
	what  string
	exact bool
}{
	{path: "database/sql", what: "a database"},
	{path: "net/http", what: "an HTTP client or an HTTP server"},
	{path: "net/rpc", what: "a remote procedure call"},
	{path: "os/exec", what: "a process started outside this one, which is how a container runtime is reached"},
	{path: "net", what: "a socket", exact: true},
	{path: "github.com/docker", what: "a container runtime"},
	{path: "github.com/containerd", what: "a container runtime"},
	{path: "github.com/opencontainers", what: "a container runtime"},
	{path: "k8s.io", what: "a container orchestrator"},
	{path: "github.com/google/go-containerregistry", what: "a registry client"},
	{path: "oras.land", what: "a registry client"},
	{path: "github.com/nats-io", what: "a task bus client"},
	{path: "github.com/rabbitmq", what: "a task bus client"},
	{path: "github.com/segmentio/kafka-go", what: "a task bus client"},
	{path: "github.com/redis", what: "a task bus client"},
	{path: "github.com/go-redis", what: "a task bus client"},
	{path: "github.com/jackc", what: "a database driver"},
	{path: "github.com/lib/pq", what: "a database driver"},
	{path: "github.com/mattn/go-sqlite3", what: "a database driver"},
	{path: "modernc.org/sqlite", what: "a database driver"},
	{path: "go.mongodb.org", what: "a database driver"},
}

// refusedSegment is what a package inside this module may not be called for the
// evaluator to import it. The root doc.go reserves driver for the container driver and
// cmd/agk for the command line, and the controller sits above both; the evaluator
// reaching for any of them would be the evaluator executing.
var refusedSegment = map[string]string{
	"controller": "the controller",
	"driver":     "the container driver",
	"runner":     "the runner",
	"bus":        "the task bus",
	"cmd":        "the command line",
}

// TestTheEvaluatorImportsNoControllerNoBusAndNoDatabase walks the closure and names
// whatever should not be in it.
func TestTheEvaluatorImportsNoControllerNoBusAndNoDatabase(t *testing.T) {
	closure := map[string]bool{}
	read := map[string]int{}

	// The two packages this group writes. The evaluator imports the expression
	// door, but seeding both says the door is held to the boundary whether or not
	// the evaluator happens to reach for it on the day the test runs.
	queue := []string{".", "internal/expr"}
	seen := map[string]bool{".": true, "internal/expr": true}

	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]

		dir := pkg
		if pkg != "." {
			dir = filepath.Join("..", pkg)
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
					t.Errorf("%s imports %s, which is %s: the evaluator is importable with no controller, no task bus and no database behind it", path, imported, why)
					continue
				}
				if inside, ok := strings.CutPrefix(imported, boundaryModule); ok && !seen[inside] {
					seen[inside] = true
					queue = append(queue, inside)
				}
			}
		}
	}

	// A rule nobody read is a rule nobody keeps. The evaluator is more than one
	// file, and its closure is wider than the evaluator.
	if read["."] < 3 {
		t.Fatalf("read the imports of %d files of the evaluator, which is fewer than it holds", read["."])
	}
	if len(read) < 3 {
		t.Fatalf("walked %d packages, and the closure of the evaluator is wider than that", len(read))
	}

	names := make([]string, 0, len(closure))
	for imported := range closure {
		names = append(names, imported)
	}
	sort.Strings(names)
	t.Logf("the closure is %s", strings.Join(names, ", "))
}

// TestTheBoundaryIsCheckedAndNotAssumed holds the check itself. A boundary test that
// would pass whatever it was given is a comment with a func keyword in front of it, so
// this hands it every kind of import the constraint exists to refuse, and one of each
// kind the layout allows.
func TestTheBoundaryIsCheckedAndNotAssumed(t *testing.T) {
	for _, imported := range []string{
		"database/sql",
		"database/sql/driver",
		"net/http",
		"net/http/httptest",
		"net",
		"net/rpc",
		"os/exec",
		"github.com/docker/docker/client",
		"github.com/containerd/containerd",
		"k8s.io/client-go/kubernetes",
		"github.com/google/go-containerregistry/pkg/v1/remote",
		"github.com/nats-io/nats.go",
		"github.com/jackc/pgx/v5",
		"github.com/aws/aws-sdk-go-v2/service/s3",
		boundaryModule + "controller",
		boundaryModule + "driver",
		boundaryModule + "internal/bus",
		boundaryModule + "cmd/agk",
		boundaryModule + "api",
	} {
		if whyRefused(imported) == "" {
			t.Errorf("%s passes the boundary check, and it is one of the things the boundary is about", imported)
		}
	}
	for _, imported := range []string{
		"fmt",
		"time",
		"sort",
		"encoding/json",
		"io/fs",
		"net/url",
		boundaryModule + "agk",
		boundaryModule + "brick",
		boundaryModule + "schema",
		boundaryModule + "artifact",
		boundaryModule + "internal/expr",
		boundaryModule + "internal/ulid",
		"github.com/goccy/go-yaml",
		"github.com/google/cel-go/cel",
		"github.com/santhosh-tekuri/jsonschema/v6",
	} {
		if why := whyRefused(imported); why != "" {
			t.Errorf("%s is refused as %s, and the layout says the evaluator may reach for it", imported, why)
		}
	}
}

// whyRefused says what an import path would make the evaluator, or nothing when the
// layout allows it.
//
// Everything the layout does not name is refused, and deliberately: a dependency arrives
// with a reason written in go.mod, and a check that let an unnamed one through would be
// testing a list of known villains rather than a boundary.
func whyRefused(imported string) string {
	for _, r := range refusedByName {
		if imported == r.path || (!r.exact && strings.HasPrefix(imported, r.path+"/")) {
			return r.what
		}
	}

	if inside, ok := strings.CutPrefix(imported, boundaryModule); ok {
		for _, segment := range strings.Split(inside, "/") {
			if what, refused := refusedSegment[segment]; refused {
				return what
			}
		}
		if !insideTheModule[inside] {
			return "not one of the packages the layout says the evaluator may reach for"
		}
		return ""
	}

	// The standard library is everything whose first segment carries no dot, which
	// is the reading agk's own closure test takes.
	if !strings.Contains(strings.Split(imported, "/")[0], ".") {
		return ""
	}

	for _, allowed := range outsideTheModule {
		if imported == allowed.path || strings.HasPrefix(imported, allowed.path+"/") {
			return ""
		}
	}
	return "a dependency this group does not take"
}
