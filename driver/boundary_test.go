package driver

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

// The container driver is a library, importable with no controller, no task bus and no
// database behind it. graph/boundary_test.go holds the evaluator to the same sentence,
// and this is its other half: the root doc.go names both packages, and only one of them
// was being checked.
//
// The two boundaries are not the same boundary. The evaluator may not reach a container
// runtime and this package is the one place in the module that may, so the list below
// allows a daemon and refuses the things a driver becoming a server would pick up. What
// stays refused is the state: a driver that could open a database would be a driver that
// could decide, and deciding is the controller's, which is what makes agk run --local the
// same code path a server run takes rather than a second implementation.
//
// Test files are not read, here as there. What an importer pays for is what the package
// imports, and a test may reach for a real daemon without any of that reaching an importer.

// driverModule is this module's path with its trailing slash, so that a module internal
// import is told from a third party one by a prefix rather than by a guess.
const driverModule = "github.com/agentiik/agentiik/"

// driverMayImport is what the layout says this package's closure may hold from inside the
// module: the vocabulary, the store, the brick contract, the evaluator whose interface it
// fills, the schemas, the daemon client it is the only user of, and the two internals the
// evaluator brings with it.
var driverMayImport = map[string]bool{
	"agk":             true,
	"artifact":        true,
	"brick":           true,
	"graph":           true,
	"schema":          true,
	"internal/docker": true,
	"internal/expr":   true,
	"internal/ulid":   true,
}

// driverMayDependOn is every third party package the closure may hold. All of them but one
// arrive through the evaluator rather than through anything this package writes. The one
// is the TOML parser LoadPolicy reads /etc/agentiik/runner.toml with, which is a file
// format and nothing that decides, holds state or listens, and whose reason is in go.mod.
var driverMayDependOn = []string{
	"github.com/goccy/go-yaml",
	"github.com/google/cel-go",
	"cel.dev",
	"github.com/santhosh-tekuri/jsonschema",
	"github.com/pelletier/go-toml/v2",
}

// driverRefuses is what this package would have become had any of these appeared in its
// closure, in the words the documentation uses, so that a failure says what was breached.
var driverRefuses = []struct{ path, what string }{
	{"database/sql", "a database"},
	{"github.com/jackc", "a database driver"},
	{"github.com/lib/pq", "a database driver"},
	{"github.com/mattn/go-sqlite3", "a database driver"},
	{"modernc.org/sqlite", "a database driver"},
	{"go.mongodb.org", "a database driver"},
	{"net/rpc", "a remote procedure call"},
	{"net/http/httptest", "an HTTP server"},
	{"k8s.io", "a container orchestrator"},
	{"github.com/nats-io", "a task bus client"},
	{"github.com/rabbitmq", "a task bus client"},
	{"github.com/segmentio/kafka-go", "a task bus client"},
	{"github.com/redis", "a task bus client"},
	{"github.com/go-redis", "a task bus client"},
}

// driverRefusesSegment is what a package inside this module may not be called for this one
// to import it. db is on the list for the reason its own doc.go states from the other end,
// and the rest are the layers that sit above a driver rather than beside it.
var driverRefusesSegment = map[string]string{
	"db":         "the database",
	"controller": "the controller",
	"runner":     "the runner",
	"bus":        "the task bus",
	"api":        "the HTTP API",
	"cmd":        "the command line",
}

// TestTheDriverImportsNoControllerNoBusAndNoDatabase walks the closure and names whatever
// should not be in it.
func TestTheDriverImportsNoControllerNoBusAndNoDatabase(t *testing.T) {
	closure := map[string]bool{}
	read := map[string]int{}

	queue := []string{".", "internal/docker"}
	seen := map[string]bool{".": true, "internal/docker": true}

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
				if why := whyTheDriverRefuses(imported); why != "" {
					t.Errorf("%s imports %s, which is %s: the container driver is importable with no controller, no task bus and no database behind it", path, imported, why)
					continue
				}
				if inside, ok := strings.CutPrefix(imported, driverModule); ok && !seen[inside] {
					seen[inside] = true
					queue = append(queue, inside)
				}
			}
		}
	}

	// A rule nobody read is a rule nobody keeps.
	if read["."] < 10 {
		t.Fatalf("read the imports of %d files of the driver, which is fewer than it holds", read["."])
	}
	if len(read) < 3 {
		t.Fatalf("walked %d packages, and the closure of the driver is wider than that", len(read))
	}

	names := make([]string, 0, len(closure))
	for imported := range closure {
		names = append(names, imported)
	}
	sort.Strings(names)
	t.Logf("the closure is %s", strings.Join(names, ", "))
}

// TestTheDriverBoundaryIsCheckedAndNotAssumed holds the check itself, since a boundary test
// that would pass whatever it was given is a comment with a func keyword in front of it.
func TestTheDriverBoundaryIsCheckedAndNotAssumed(t *testing.T) {
	for _, imported := range []string{
		"database/sql",
		"database/sql/driver",
		"github.com/jackc/pgx/v5",
		"github.com/jackc/pgx/v5/pgxpool",
		"net/rpc",
		"net/http/httptest",
		"k8s.io/client-go/kubernetes",
		"github.com/nats-io/nats.go",
		driverModule + "db",
		driverModule + "controller",
		driverModule + "api",
		driverModule + "internal/bus",
		driverModule + "cmd/agk",
	} {
		if whyTheDriverRefuses(imported) == "" {
			t.Errorf("%s passes the boundary check, and it is one of the things the boundary is about", imported)
		}
	}
	// A daemon over a socket is what this package is for, and net, net/http and net/url
	// are how internal/docker reaches one. Refusing them here would be refusing the
	// package its own subject, which is the mistake the evaluator's list does not make in
	// the other direction.
	for _, imported := range []string{
		"fmt",
		"time",
		"archive/tar",
		"os",
		"net",
		"net/http",
		"net/url",
		"syscall",
		driverModule + "agk",
		driverModule + "brick",
		driverModule + "graph",
		driverModule + "artifact",
		driverModule + "schema",
		driverModule + "internal/docker",
		"github.com/goccy/go-yaml",
		"github.com/google/cel-go/cel",
		"github.com/santhosh-tekuri/jsonschema/v6",
		"github.com/pelletier/go-toml/v2",
	} {
		if why := whyTheDriverRefuses(imported); why != "" {
			t.Errorf("%s is refused as %s, and the layout says the driver may reach for it", imported, why)
		}
	}
}

// whyTheDriverRefuses says what an import path would make this package, or nothing when the
// layout allows it. Everything the layout does not name is refused, deliberately: a
// dependency arrives with a reason written in go.mod, and a check that let an unnamed one
// through would be testing a list of known villains rather than a boundary.
func whyTheDriverRefuses(imported string) string {
	for _, r := range driverRefuses {
		if imported == r.path || strings.HasPrefix(imported, r.path+"/") {
			return r.what
		}
	}

	if inside, ok := strings.CutPrefix(imported, driverModule); ok {
		for _, segment := range strings.Split(inside, "/") {
			if what, refused := driverRefusesSegment[segment]; refused {
				return what
			}
		}
		if !driverMayImport[inside] {
			return "not one of the packages the layout says the driver may reach for"
		}
		return ""
	}

	// The standard library is everything whose first segment carries no dot, which is the
	// reading the two closure tests beside this one take.
	if !strings.Contains(strings.Split(imported, "/")[0], ".") {
		return ""
	}

	for _, allowed := range driverMayDependOn {
		if imported == allowed || strings.HasPrefix(imported, allowed+"/") {
			return ""
		}
	}
	return "a dependency this package does not take"
}
