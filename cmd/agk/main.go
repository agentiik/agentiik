package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The exit codes of the process. Five, and each is a different thing for whoever typed the
// command to do next. The distinction between the last two is the one the driver already
// draws between a Result and an error, carried to the shell.
const (
	// exitSucceeded: the command did what it says, and a run reached succeeded.
	exitSucceeded = 0
	// exitRefused: refused, and nothing ran. The workflow was refused, an input was
	// refused, a secret a step mounts was not supplied, a brick test case did not
	// match.
	exitRefused = 1
	// exitUsage: the command line was wrong. This is the standard library flag
	// package's own code and nothing translates it.
	exitUsage = 2
	// exitNotSucceeded: the run reached a terminal state other than succeeded. It ran,
	// and it did not succeed.
	exitNotSucceeded = 3
	// exitNoOutcome: no outcome could be determined. The daemon could not be reached,
	// the userns floor refused, network: egress was refused, a pull died, a working
	// directory could not be prepared.
	exitNoOutcome = 4
)

// Env is everything the command line reaches outside itself.
//
// It is a value rather than a set of package-level functions so that a test drives argv and
// reads bytes: issue #83 runs the whole command line twice and compares what came out, and
// no per-command entry point gives that.
type Env struct {
	Out, Err   io.Writer
	Dir        string
	Now        func() time.Time
	Getenv     func(string) string
	Executable func() (string, error)
}

// command is one verb of the documented table.
//
// The table is data in this package rather than prose, so that the usage text and the
// dispatch cannot disagree: both read this.
type command struct {
	name    string
	summary string
	run     func(context.Context, Env, []string) int
}

// commands is the table of #command-line, in the order the documentation writes it, with
// each effect in the documentation's own words.
//
// One of these reaches an installation for a sign-in it does not serve yet, and brick init for
// templates released elsewhere, and each refuses naming what is missing, because a verb the
// documentation lists and the binary does not know is a binary that looks broken.
//
// The row for agk run is added by run.go, the file that implements it beside
// cmd/agk/internal/local, and it goes between graph and push, which is where the documentation
// writes it.
var commands = []command{
	{"login", "Signs in against an installation and stores an API token in the local profile.", absent("login", "an installation has no sign-in route yet, and is administered with its bootstrap token until its first administrator has enrolled", withSignIn)},
	{"token create", "Mints an API token for you, or for a service account of a namespace you own, and prints it this once.", tokenCreate},
	{"token list", "Lists the API tokens you may revoke, with their expiry, last use, scope and device label.", tokenList},
	{"token revoke", "Revokes one API token, from its next request.", tokenRevoke},
	{"whoami", "Prints the current principal, its groups and its effective permissions, everywhere or on a given namespace or workflow.", whoami},
	{"user create", "Creates a user, --admin for an administrator, and prints the enrolment link.", userCreate},
	{"user recover", "Issues a recovery code for a user who lost what signs them in, and prints the link that carries it.", userRecover},
	{"user list", "Lists the users of an installation.", userList},
	{"user show", "Shows one user: display name, whether an administrator or suspended, when created and last signed in.", userShow},
	{"user delete", "Removes a user with what they hold, and their personal namespace where it is empty.", userDelete},
	{"group create", "Creates a group, empty or with its first members.", groupCreate},
	{"group list", "Lists the groups of an installation and their members.", groupList},
	{"group show", "Shows one group and its members.", groupShow},
	{"group delete", "Removes a group with its memberships and grants.", groupDelete},
	{"group add", "Puts a user in a group, touching no grant.", groupAdd},
	{"group remove", "Takes a user out of a group, touching no grant.", groupRemove},
	{"service-account create", "Creates a service account, NS/NAME, in a namespace you own.", serviceAccountCreate},
	{"service-account list", "Lists the service accounts of the namespaces you own, or of the one named.", serviceAccountList},
	{"service-account delete", "Removes a service account with its tokens and grants.", serviceAccountDelete},
	{"namespace create", "Creates a namespace with its owner and its quotas. Administrator only.", namespaceCreate},
	{"namespace list", "Lists the namespaces the caller holds a grant in, and every one for an administrator.", namespaceList},
	{"namespace show", "Shows one namespace: its kind, its owner and its quotas.", namespaceShow},
	{"namespace delete", "Removes a namespace that holds nothing but its built-in identity. Administrator only.", namespaceDelete},
	{"namespace quotas", "Shows a namespace's quotas, and with quotas given, sets them as the whole set. Administrator only to set.", namespaceQuotas},
	{"validate", "Validates the YAML, resolves includes and inheritance, detects cycles, checks ports against the manifests of the referenced images.", validate},
	{"graph", "Writes the resolved graph as DOT or Mermaid, for review inside a merge request.", drawing},
	{"push", "Registers the workflow in a namespace on a server.", push},
	{"share", "Grants or revokes access to a namespace or a workflow.", shareVerb},
	{"grants", "Shows who can do what on a namespace or a workflow, and which scope each permission comes from.", grantsVerb},
	{"logs", "Follows the logs of a run.", logs},
	{"status", "Shows how a run on an installation stands: its state, each step's, the envelope digests and what failed.", status},
	{"brick init", "Scaffolds a brick in a chosen language, with its manifest and test harness.", absent("brick init", "there are no brick templates here", "They are released from agentiik/bricks")},
	{"brick test", "Runs the brick against a set of sample envelopes and compares against expected outputs.", brickTest},
}

// main is the four lines around run.
func main() {
	// One interrupt reaches the command through the context, which is what a run turns
	// into a cancellation of its own.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	dir, _ := os.Getwd()
	code := run(ctx, Env{
		Out: os.Stdout, Err: os.Stderr, Dir: dir,
		Now: time.Now, Getenv: os.Getenv, Executable: os.Executable,
	}, os.Args[1:])
	stop()
	os.Exit(code)
}

// run is the whole command line: the flags, the dispatch and the exit code.
//
// Everything printed goes through the two writers of the Env and nothing reaches os.Stdout
// from anywhere else, which is what makes the command line testable as a command line.
func run(ctx context.Context, e Env, args []string) int {
	// --version is a flag rather than a command, so that the documented table stays
	// exactly the table.
	for _, arg := range args {
		switch arg {
		case "--version", "-version":
			fmt.Fprintln(e.Out, version())
			return exitSucceeded
		case "-h", "-help", "--help":
			usage(e.Out)
			return exitSucceeded
		}
		// Only the leading arguments are read this way. A flag of the same spelling
		// after the verb belongs to the verb.
		if !strings.HasPrefix(arg, "-") {
			break
		}
	}

	if len(args) == 0 {
		usage(e.Err)
		return exitUsage
	}

	c, rest := verb(args)
	if c == nil {
		fmt.Fprintf(e.Err, "%s: there is no such command. The commands are %s\n", typed(args), spelling())
		return exitUsage
	}
	return c.run(ctx, e, rest)
}

// verb reads the command out of the arguments, the two-word verbs first.
//
// brick test and brick init are two words because the documentation writes them as two, and
// a binary that answered to brick-test would be a binary whose help and whose documentation
// spell one thing differently. The namespace verbs are two words for the same reason agk
// auth policy and agk user create are: a noun, then what is done to it.
func verb(args []string) (*command, []string) {
	if len(args) >= 2 {
		two := args[0] + " " + args[1]
		for i, c := range commands {
			if c.name == two {
				return &commands[i], args[2:]
			}
		}
	}
	for i, c := range commands {
		if c.name == args[0] {
			return &commands[i], args[1:]
		}
	}
	return nil, nil
}

// typed is what to call the command nobody knows, which is the verb and not the rest of the
// line: brick frobnicate is a second word of a verb that has one, and anything else is its
// first word alone.
func typed(args []string) string {
	if len(args) >= 2 && slices.ContainsFunc(commands, func(c command) bool { return strings.HasPrefix(c.name, args[0]+" ") }) {
		return args[0] + " " + args[1]
	}
	return args[0]
}

// usage is the table, and nothing that is not in the table.
func usage(w io.Writer) {
	fmt.Fprintln(w, "agk is the Agentiik command line.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "\tagk <command> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	width := 0
	for _, c := range commands {
		width = max(width, len(c.name))
	}
	for _, c := range commands {
		fmt.Fprintf(w, "\t%-*s  %s\n", width, c.name, c.summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "\t--version  Prints the version of this binary.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Every command takes -h for its own flags.")
}

// spelling lists the verbs, for the line that says there is no such command.
func spelling() string {
	names := make([]string, 0, len(commands))
	for _, c := range commands {
		names = append(names, c.name)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// now is the clock, or the real one.
func (e Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// getenv reads one variable of the environment, or nothing where the Env carries no lookup.
func (e Env) getenv(name string) string {
	if e.Getenv == nil {
		return ""
	}
	return e.Getenv(name)
}

// path resolves one path the way the person typing it means it: against the directory the
// command was run in.
func (e Env) path(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	if e.Dir == "" {
		return p
	}
	return filepath.Join(e.Dir, p)
}
