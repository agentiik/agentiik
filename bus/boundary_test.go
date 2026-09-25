package bus

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

// The task bus is what a runner takes its work through and reports on, and a runner "holds no
// database credential, no secret-store credential and no standing object-store credential". The
// program on its host links none of the code that would use one either, and it links this
// package. So this package links no controller, no database, no API and no secret store, and
// the controller's half of the bus, which names the controller's types, is package bus/control.
// Before that split, linking this package linked the controller and through it the database
// driver, and nothing said so.
//
// It is the walk driver/boundary_test.go takes, with a list of its own. The evaluator is on it,
// since a stop names a task the way the evaluator does and the runner links the evaluator through
// the driver anyway. What stays refused is the state and whatever reads it: a package here that
// could open a database would be one a runner links.
//
// Test files are not read, here as there. What an importer pays for is what the package imports,
// and a test may put the API and the database beside the bus without any of that reaching a
// runner.

// busModule is this module's path with its trailing slash, so that a module internal import is
// told from a third party one by a prefix rather than by a guess.
const busModule = "github.com/agentiik/agentiik/"

// busMayImport is what this package's closure may hold from inside the module: the vocabulary,
// the evaluator a stop is written in, and what the evaluator brings with it.
var busMayImport = map[string]bool{
	"agk":           true,
	"artifact":      true,
	"brick":         true,
	"graph":         true,
	"schema":        true,
	"internal/expr": true,
	"internal/ulid": true,
	// The TLS floor every connection holds, and which address may go without TLS.
	"internal/tlsfloor": true,
}

// busMayDependOn is every third party package the closure may hold: the client, the credentials
// and the keys this package takes, and what the evaluator takes.
var busMayDependOn = []string{
	"github.com/nats-io/nats.go",
	"github.com/nats-io/jwt/v2",
	"github.com/nats-io/nkeys",
	"github.com/goccy/go-yaml",
	"cel.dev",
	"github.com/santhosh-tekuri/jsonschema",
}

// busRefuses is what a runner would link had any of these appeared in this package's closure, in
// the words the documentation uses, so that a failure says what was breached.
var busRefuses = []struct{ path, what string }{
	{"database/sql", "a database"},
	{"github.com/jackc", "a database driver"},
	{"github.com/lib/pq", "a database driver"},
	{"github.com/mattn/go-sqlite3", "a database driver"},
	{"modernc.org/sqlite", "a database driver"},
	{"go.mongodb.org", "a database driver"},
}

// busRefusesSegment is what a package inside this module may not be called for this one to import
// it. The controller, the database, the API and the secret store are what the runner is kept from.
// The driver is kept out for the other side's sake: the controller links this package through
// bus/control, and it "does not choose a machine, and it does not start one".
var busRefusesSegment = map[string]string{
	"db":         "the database",
	"controller": "the controller",
	"api":        "the HTTP API",
	"secret":     "the secret store",
	"driver":     "the container driver",
	"cmd":        "the command line",
}

// TestTheBusImportsNoControllerNoDatabaseAndNoSecretStore walks the closure and names whatever
// should not be in it.
func TestTheBusImportsNoControllerNoDatabaseAndNoSecretStore(t *testing.T) {
	closure := map[string]bool{}
	read := map[string]int{}

	queue := []string{"."}
	seen := map[string]bool{".": true}

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
				if why := whyTheBusRefuses(imported); why != "" {
					t.Errorf("%s imports %s, which is %s: a runner links the task bus, and a runner links no controller, no database and no secret store", path, imported, why)
					continue
				}
				if inside, ok := strings.CutPrefix(imported, busModule); ok && !seen[inside] {
					seen[inside] = true
					queue = append(queue, inside)
				}
			}
		}
	}

	// A rule nobody read is a rule nobody keeps.
	if read["."] < 5 {
		t.Fatalf("read the imports of %d files of the bus, which is fewer than it holds", read["."])
	}
	if len(read) < 3 {
		t.Fatalf("walked %d packages, and the closure of the bus is wider than that", len(read))
	}

	names := make([]string, 0, len(closure))
	for imported := range closure {
		names = append(names, imported)
	}
	sort.Strings(names)
	t.Logf("the closure is %s", strings.Join(names, ", "))
}

// TestTheBusBoundaryIsCheckedAndNotAssumed holds the check itself, since a boundary test that
// would pass whatever it was given is a comment with a func keyword in front of it. The first
// list is what this package imported before its controller half moved out, and what that brought
// with it.
func TestTheBusBoundaryIsCheckedAndNotAssumed(t *testing.T) {
	for _, imported := range []string{
		busModule + "controller",
		busModule + "db",
		"github.com/jackc/pgx/v5",
		"github.com/jackc/pgx/v5/pgxpool",
		"database/sql",
		busModule + "api",
		busModule + "secret",
		busModule + "secret/vault",
		busModule + "driver",
		busModule + "internal/docker",
		busModule + "cmd/agk",
		"github.com/nats-io/nats-server/v2/server",
		"k8s.io/client-go/kubernetes",
	} {
		if whyTheBusRefuses(imported) == "" {
			t.Errorf("%s passes the boundary check, and it is one of the things the boundary is about", imported)
		}
	}
	for _, imported := range []string{
		"context",
		"encoding/json",
		"net",
		"time",
		busModule + "agk",
		busModule + "graph",
		busModule + "internal/ulid",
		"github.com/nats-io/nats.go",
		"github.com/nats-io/nats.go/jetstream",
		"github.com/nats-io/jwt/v2",
		"github.com/nats-io/nkeys",
		"github.com/goccy/go-yaml",
		"cel.dev/cel-go/cel",
		"github.com/santhosh-tekuri/jsonschema/v6",
	} {
		if why := whyTheBusRefuses(imported); why != "" {
			t.Errorf("%s is refused as %s, and the bus may reach for it", imported, why)
		}
	}
}

// whyTheBusRefuses says what an import path would make this package, or nothing when the layout
// allows it. Everything the layout does not name is refused, deliberately: a dependency arrives
// with a reason written in go.mod, and a check that let an unnamed one through would be testing a
// list of known villains rather than a boundary.
func whyTheBusRefuses(imported string) string {
	for _, r := range busRefuses {
		if imported == r.path || strings.HasPrefix(imported, r.path+"/") {
			return r.what
		}
	}

	if inside, ok := strings.CutPrefix(imported, busModule); ok {
		for _, segment := range strings.Split(inside, "/") {
			if what, refused := busRefusesSegment[segment]; refused {
				return what
			}
		}
		if !busMayImport[inside] {
			return "not one of the packages the layout says the bus may reach for"
		}
		return ""
	}

	// The standard library is everything whose first segment carries no dot, which is the
	// reading the boundary tests of the evaluator and the driver take.
	if !strings.Contains(strings.Split(imported, "/")[0], ".") {
		return ""
	}

	for _, allowed := range busMayDependOn {
		if imported == allowed || strings.HasPrefix(imported, allowed+"/") {
			return ""
		}
	}
	return "a dependency this package does not take"
}
