package main

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A runner "holds no database credential, no secret-store credential and no standing object-store
// credential", and the program on its host links none of the code that would use one either. It
// also opens no inbound port: "every connection is outbound, to the API, the bus, the object store
// and the registry". Both are sentences about the program a host runs, so both are held here, on
// the program, rather than on any one package of it.
//
// The closure is read with go list, for the platforms the agent ships on, rather than by walking
// the source as bus and driver do: a package's boundary is what its importers pay for, and this
// one's is what is linked, third-party packages and their own imports included, since a database
// driver arriving behind a dependency is linked all the same. Test files are not part of it.

// runnerModule is this module's path with its trailing slash.
const runnerModule = "github.com/agentiik/agentiik/"

// runnerRefusesInside is what a package of this module may not be for the agent to link it, with
// what the agent would have become. Each is matched as the package and everything below it.
var runnerRefusesInside = []struct{ path, what string }{
	{"db", "the database"},
	{"controller", "the controller"},
	{"api", "the API, and through it the database and the controller"},
	{"secret", "the secret store"},
	{"bus/control", "the controller's half of the bus"},
	{"internal/dbtest", "a database fixture"},
	{"internal/config", "the server programs' configuration, which names the master key and the database"},
	{"cmd/agk", "the command line, which links the API"},
}

// runnerRefuses is what a third-party or standard package may not be, with the same.
var runnerRefuses = []struct{ path, what string }{
	{"database/sql", "a database"},
	{"github.com/jackc", "a database driver"},
	{"github.com/lib/pq", "a database driver"},
	{"github.com/nats-io/nats-server", "a bus server"},
	{"net/http/httptest", "an HTTP server"},
}

// runnerMayImport is the whole of what the agent links from inside the module. A package not on
// it is refused until somebody adds it here, since a package that arrives in the agent arrives on
// every runner host, and the reason it may belongs beside it.
var runnerMayImport = map[string]bool{
	"cmd/agk-runner": true,
	"runner":         true,
	// The container driver, which is the half a runner shares with agk run --local, and what
	// it brings with it.
	"driver":          true,
	"agk":             true,
	"artifact":        true,
	"brick":           true,
	"graph":           true,
	"schema":          true,
	"internal/docker": true,
	"internal/expr":   true,
	"internal/ulid":   true,
	// The credential grammar, which says what kind a credential is without printing it.
	"internal/token": true,
	// The TLS floor every connection holds, and which address may go without TLS.
	"internal/tlsfloor": true,
	// The runner's half of the task bus, whose task message is what a runner takes and
	// assembles into the task the driver runs. bus/boundary_test.go keeps the controller's half
	// out of it.
	"bus": true,
	// The object store as one task's redemption lets it be reached: its presigned URLs and its
	// upload policy, and no standing credential.
	"artifact/granted": true,
}

// runnerMayDependOn is every third-party module the closure may hold. Each arrives through the
// driver or the task bus, whose own boundary tests give the reasons.
var runnerMayDependOn = []string{
	// The NATS client, its credentials and its keys, which arrive through package bus and whose
	// reasons go.mod gives, and the four modules the client itself takes.
	"github.com/nats-io/nats.go",
	"github.com/nats-io/jwt/v2",
	"github.com/nats-io/nkeys",
	"github.com/nats-io/nuid",
	"github.com/klauspost/compress",
	"golang.org/x/crypto",
	"golang.org/x/sys",
	"github.com/goccy/go-yaml",
	"cel.dev",
	"github.com/antlr4-go/antlr",
	"github.com/santhosh-tekuri/jsonschema",
	"github.com/pelletier/go-toml/v2",
	"google.golang.org/protobuf",
	"google.golang.org/genproto",
	"golang.org/x/exp",
	"golang.org/x/text",
	"go.yaml.in/yaml",
}

// platforms are what the agent ships for, and so what its closure is read for: a file built on
// one of them alone links what that file imports on it alone.
var platforms = []string{"linux/amd64", "linux/arm64"}

func TestTheAgentLinksNoDatabaseNoControllerNoAPIAndNoSecretStore(t *testing.T) {
	for _, platform := range platforms {
		t.Run(platform, func(t *testing.T) {
			closure := goList(t, platform)
			// A rule nobody read is a rule nobody keeps: the agent links the driver
			// and the evaluator behind it, which is well over a hundred packages.
			if len(closure) < 100 || !slices.Contains(closure, runnerModule+"driver") {
				t.Fatalf("read a closure of %d packages without the driver in it, which is not the agent's", len(closure))
			}
			for _, imported := range closure {
				if why := whyTheAgentRefuses(imported); why != "" {
					t.Errorf("the agent links %s, which is %s: a runner holds no database, secret-store or standing object-store credential, and the program on its host links none of the code that would use one", imported, why)
				}
			}
		})
	}
}

func TestTheBoundaryIsCheckedAndNotAssumed(t *testing.T) {
	for _, imported := range []string{
		runnerModule + "db",
		runnerModule + "db/migrations",
		runnerModule + "controller",
		runnerModule + "api",
		runnerModule + "secret",
		runnerModule + "secret/builtin",
		runnerModule + "bus/control",
		runnerModule + "internal/dbtest",
		runnerModule + "internal/config",
		runnerModule + "cmd/agk",
		runnerModule + "cmd/agk/internal/helper",
		runnerModule + "somethingnew",
		"database/sql",
		"github.com/jackc/pgx/v5",
		"github.com/jackc/pgx/v5/pgconn",
		"github.com/lib/pq",
		"github.com/nats-io/nats-server/v2/server",
		"github.com/somebody/new",
		"github.com/goccy/go-yaml-lookalike",
		"net/http/httptest",
	} {
		if whyTheAgentRefuses(imported) == "" {
			t.Errorf("%s passes the boundary check, and it is one of the things the boundary is about", imported)
		}
	}
	for _, imported := range []string{
		"net/http",
		"os/signal",
		runnerModule + "cmd/agk-runner",
		runnerModule + "runner",
		runnerModule + "driver",
		runnerModule + "internal/token",
		"github.com/pelletier/go-toml/v2",
		"github.com/pelletier/go-toml/v2/unstable",
	} {
		if why := whyTheAgentRefuses(imported); why != "" {
			t.Errorf("%s is refused as %s, and the agent may link it", imported, why)
		}
	}
}

// whyTheAgentRefuses says what an import path would make the agent, or nothing where it may link
// it.
func whyTheAgentRefuses(imported string) string {
	under := func(path, prefix string) bool { return path == prefix || strings.HasPrefix(path, prefix+"/") }
	if inside, ok := strings.CutPrefix(imported, runnerModule); ok {
		for _, r := range runnerRefusesInside {
			if under(inside, r.path) {
				return r.what
			}
		}
		if !runnerMayImport[inside] {
			return "a package of this module the agent is not known to link: add it to runnerMayImport, with its reason, if it belongs on every runner host"
		}
		return ""
	}
	for _, r := range runnerRefuses {
		if under(imported, r.path) {
			return r.what
		}
	}
	// The standard library is everything whose first segment carries no dot, and its vendored
	// copies of golang.org/x are the standard library's own.
	first := strings.Split(imported, "/")[0]
	if !strings.Contains(first, ".") {
		return ""
	}
	for _, m := range runnerMayDependOn {
		if under(imported, m) {
			return ""
		}
	}
	return "a dependency the agent is not known to link: add it to runnerMayDependOn if it arrives with a reason in go.mod"
}

// goList answers the closure of this program for one platform, as the linker sees it.
func goList(t *testing.T, platform string) []string {
	t.Helper()
	tool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no Go toolchain to read the closure with")
	}
	goos, goarch, _ := strings.Cut(platform, "/")
	cmd := exec.Command(tool, "list", "-deps", ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list for %s: %s", platform, err)
	}
	return strings.Fields(string(out))
}

// listening is every function whose presence in a binary means it can accept a connection. The
// linker drops what nothing reaches, so a symbol that is there is one something calls.
//
// syscall.Listen is not on it: package net holds it in a variable its tests hook, so every
// program that dials anything links it, and the functions above are the only way to it.
var listening = []string{
	"net.Listen",
	"net.ListenPacket",
	"net.ListenTCP",
	"net.ListenUDP",
	"net.ListenUnix",
	"net.ListenUnixgram",
	"net.ListenIP",
	"net.ListenMulticastUDP",
	"net.(*ListenConfig).Listen",
	"net.(*ListenConfig).ListenPacket",
	"net/http.Serve",
	"net/http.ServeTLS",
	"net/http.ListenAndServe",
	"net/http.ListenAndServeTLS",
	"net/http.(*Server).Serve",
	"net/http.(*Server).ServeTLS",
	"net/http.(*Server).ListenAndServe",
	"net/http.(*Server).ListenAndServeTLS",
}

func TestTheLinkedAgentHoldsNothingThatOpensAnInboundPort(t *testing.T) {
	path := buildRunner(t, "linux", "amd64", false)
	if found := listeners(t, path); len(found) > 0 {
		t.Errorf("the linked agent holds %s: no inbound port is ever opened on a runner host, and every connection is outbound", strings.Join(found, ", "))
	}
}

// TestTheSymbolCheckFindsAListenerWhereThereIsOne builds a program that serves HTTP and holds the
// check to finding it, since a check that read no symbol table would pass the agent too.
func TestTheSymbolCheckFindsAListenerWhereThereIsOne(t *testing.T) {
	tool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no Go toolchain to build the probe with")
	}
	dir := t.TempDir()
	for name, text := range map[string]string{
		"go.mod": "module probe\n",
		"main.go": `package main

import (
	"net"
	"net/http"
)

func main() {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	http.Serve(l, nil)
}
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "probe")
	cmd := exec.Command(tool, "build", "-trimpath", "-o", path, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the probe: %s\n%s", err, out)
	}
	found := listeners(t, path)
	for _, want := range []string{"net.Listen", "net/http.(*Server).Serve"} {
		if !slices.Contains(found, want) {
			t.Errorf("the check found %q in a program that listens, and not %s", found, want)
		}
	}
}

// listeners answers which of listening a linked binary holds, read with go tool nm.
func listeners(t *testing.T, path string) []string {
	t.Helper()
	out, err := exec.Command("go", "tool", "nm", path).Output()
	if err != nil {
		t.Fatalf("go tool nm %s: %s", path, err)
	}
	held := map[string]bool{}
	symbols := 0
	lines := bufio.NewScanner(bytes.NewReader(out))
	lines.Buffer(nil, 1<<20)
	for lines.Scan() {
		// address, kind, name; an undefined symbol has no address.
		fields := strings.Fields(lines.Text())
		if len(fields) < 2 {
			continue
		}
		symbols++
		held[fields[len(fields)-1]] = true
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	if symbols < 1000 {
		t.Fatalf("go tool nm read %d symbols from %s, which is a stripped binary or none: a check of its symbol table would pass anything", symbols, path)
	}
	var found []string
	for _, name := range listening {
		if held[name] {
			found = append(found, name)
		}
	}
	return found
}
