package access

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The rule is resolved with no database, no bus and no HTTP behind it, so that the API and the
// controller import the same function: package api is an HTTP server the controller cannot import,
// and a rule each of them wrote for itself would be two rules. This reads the imports of every file
// of the package, follows the ones inside the module, and holds the closure to that, the idiom
// graph/boundary_test.go uses for the evaluator.

const boundaryModule = "github.com/agentiik/agentiik/"

// insideTheModule is what this package may reach for: the vocabulary, for the reserved words and
// the bound on a name, and the identifier mint the vocabulary imports, both held to the standard
// library by agk's own test.
var insideTheModule = map[string]bool{
	"agk":           true,
	"internal/ulid": true,
}

// refusedStd is the part of the standard library that would put a server, a socket, a database,
// another process or the machine it runs on behind the rule. exact marks a path refused as itself
// and not as a prefix: net is a socket, and net/url is a parser.
var refusedStd = []struct {
	path  string
	what  string
	exact bool
}{
	{path: "database/sql", what: "a database"},
	{path: "net/http", what: "an HTTP client or an HTTP server"},
	{path: "net/rpc", what: "a remote procedure call"},
	{path: "net", what: "a socket", exact: true},
	{path: "os", what: "the machine the rule runs on, its files, its environment or its processes"},
}

func TestTheRuleHasNoDatabaseNoBusAndNoHTTPBehindIt(t *testing.T) {
	queue := []string{"."}
	seen := map[string]bool{".": true}
	read := map[string]int{}
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
				if why := whyRefused(imported); why != "" {
					t.Errorf("%s imports %s, which is %s", path, imported, why)
					continue
				}
				if inside, ok := strings.CutPrefix(imported, boundaryModule); ok && !seen[inside] {
					seen[inside] = true
					queue = append(queue, inside)
				}
			}
		}
	}
	// A rule nobody read is a rule nobody keeps.
	if read["."] < 4 {
		t.Fatalf("read the imports of %d files of this package, which is fewer than it holds", read["."])
	}
}

// The check itself, handed every kind of import it exists to refuse and one of each it allows.
func TestTheBoundaryIsCheckedAndNotAssumed(t *testing.T) {
	for _, imported := range []string{
		"database/sql", "database/sql/driver", "net/http", "net/http/httptest", "net", "net/rpc", "os", "os/exec",
		"github.com/nats-io/nats.go", "github.com/jackc/pgx/v5",
		boundaryModule + "api", boundaryModule + "db", boundaryModule + "bus", boundaryModule + "controller",
		boundaryModule + "internal/config",
	} {
		if whyRefused(imported) == "" {
			t.Errorf("%s passes the boundary check", imported)
		}
	}
	for _, imported := range []string{"fmt", "time", "strings", "slices", "regexp", "errors", "net/url", boundaryModule + "agk"} {
		if why := whyRefused(imported); why != "" {
			t.Errorf("%s is refused as %s", imported, why)
		}
	}
}

// whyRefused says what an import would put behind the rule, or nothing when it may be imported.
// Everything outside the standard library and the packages named above is refused.
func whyRefused(imported string) string {
	for _, r := range refusedStd {
		if imported == r.path || (!r.exact && strings.HasPrefix(imported, r.path+"/")) {
			return r.what
		}
	}
	if inside, ok := strings.CutPrefix(imported, boundaryModule); ok {
		if insideTheModule[inside] {
			return ""
		}
		return "a package of this module the rule has no need of"
	}
	if strings.Contains(strings.Split(imported, "/")[0], ".") {
		return "a dependency"
	}
	return ""
}
