package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"time"

	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/internal/stopsignal"
)

// The exit codes, which doc.go sets out.
const (
	exitStopped = 0
	exitFailed  = 1
	exitUsage   = 2
)

// program is what this binary calls itself, in what it prints and on its bus connection.
const program = "agentiik-api"

func main() {
	os.Exit(untilSignalled(func(ctx context.Context) int {
		return run(ctx, os.Args[1:], nil, os.Stdout, os.Stderr)
	}))
}

// untilSignalled runs fn with a context stopsignal makes, and answers its exit code. It is the one
// way the program is started, which a test starts it through too.
//
// The context is done at the first SIGINT or SIGTERM, which is a stop asked for: the requests
// being answered are finished before the program ends. A second one ends a process whose way out
// is taking too long.
func untilSignalled(fn func(context.Context) int) int {
	ctx, stop := stopsignal.Context()
	defer stop()
	return fn(ctx)
}

// run is the whole program: the verb, its arguments, the configuration, and the exit code. lookup
// reads the environment, and is os.LookupEnv where nil.
func run(ctx context.Context, args []string, lookup config.Lookup, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "%s: no verb, and it is told which of its eight jobs to do\n", program)
		usage(stderr)
		return exitUsage
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "--version", "-version":
		if len(rest) == 0 {
			fmt.Fprintln(stdout, versionLine())
			return exitStopped
		}
	case "--help", "-help", "-h":
		if len(rest) == 0 {
			usage(stdout)
			return exitStopped
		}
	case "serve", "migrate", "init", "health":
		if len(rest) == 0 {
			return verbs[verb](ctx, lookup, stdout, stderr)
		}
		fmt.Fprintf(stderr, "%s %s: it takes no argument, and was given %q: everything it reads is in its environment\n", program, verb, rest)
		usage(stderr)
		return exitUsage
	case "bus-init", "bus-credential":
		if len(rest) == 1 && rest[0] != "" {
			return inDirectory[verb](rest[0], time.Now(), stdout, stderr)
		}
		fmt.Fprintf(stderr, "%s %s: it takes one argument, the directory holding the installation's bus identity, and was given %q\n", program, verb, rest)
		usage(stderr)
		return exitUsage
	case "namespace":
		if len(rest) == 2 && (rest[0] == "create" || rest[0] == "remove") {
			return namespaceVerb(ctx, lookup, rest[0], rest[1], stdout, stderr)
		}
		fmt.Fprintf(stderr, "%s %s: it takes create or remove, then the namespace's name, and was given %q\n", program, verb, rest)
		usage(stderr)
		return exitUsage
	case "audit-verify":
		if len(rest) == 1 && rest[0] != "" {
			return auditVerify(rest[0], stdout, stderr)
		}
		fmt.Fprintf(stderr, "%s %s: it takes one argument, the file an export of the audit log was written to, and was given %q\n", program, verb, rest)
		usage(stderr)
		return exitUsage
	default:
		fmt.Fprintf(stderr, "%s: %q is not one of its verbs\n", program, verb)
		usage(stderr)
		return exitUsage
	}
	fmt.Fprintf(stderr, "%s: %s takes nothing after it, and was given %q\n", program, verb, rest)
	usage(stderr)
	return exitUsage
}

// verbs are the verbs that read the environment and nothing else.
var verbs = map[string]func(ctx context.Context, lookup config.Lookup, stdout, stderr io.Writer) int{
	"serve":   serveVerb,
	"migrate": migrateVerb,
	"init":    initVerb,
	"health":  healthVerb,
}

// inDirectory are the verbs that take the directory holding the bus identity, and the moment the
// credential they write is minted from.
var inDirectory = map[string]func(dir string, now time.Time, stdout, stderr io.Writer) int{
	"bus-init":       busInit,
	"bus-credential": busCredential,
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `usage:
  %[1]s serve                 serve every route, the built-in object store and the secret providers
  %[1]s migrate               apply the migrations, and create the role the API and the controller connect as
  %[1]s init                  prepare an installation, or bring it back in line with its settings: certificate, keys, bus, database, runner's join token
  %[1]s health                exit 0 where the API serving beside it answers, for a health check in its container
  %[1]s bus-init DIR          create the installation's NATS operator and accounts in DIR
  %[1]s bus-credential DIR    mint the control plane a new bus credential under the account in DIR
  %[1]s audit-verify FILE     verify the chain of an audit log export, as its receiver wrote it down
  %[1]s namespace create NAME create a namespace, until v0.3.0's routes do
  %[1]s namespace remove NAME remove a namespace that holds no workflow, run or secret

serve, migrate, init, health and namespace read their settings from AGK_* environment variables, as
https://agentiik.github.io/docs/#configuration lists them. --version and --help take nothing else.
`, program)
}

// versionLine is what --version prints: the module's version, the commit and the toolchain, as
// the build recorded them, which is the line agk --version prints for itself.
func versionLine() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return program + ", built with no version recorded"
	}
	v := info.Main.Version
	if v == "" {
		v = "(devel)"
	}
	line := program + " " + v
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && setting.Value != "" {
			line += ", " + setting.Value
		}
	}
	return line + ", " + info.GoVersion
}
