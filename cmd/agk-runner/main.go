package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/runner"
)

// The exit codes of the process. A unit restarts the agent whatever it exits with but
// exitJoinAgain, so these are for whoever reads systemctl status and for a script around join,
// and each says something different about what to do next.
const (
	// exitSucceeded: the verb did what it says, and serve was stopped by its service manager.
	exitSucceeded = 0
	// exitRefused: refused, and nothing was started. A setting, the floor, the daemon, the
	// account the agent runs as.
	exitRefused = 1
	// exitUsage: the command line was wrong, which is the standard library flag package's
	// own code.
	exitUsage = 2
	// exitJoinAgain: the API refused the runner's credential, revoked past its grace, rotated
	// past or never issued, or the host's key is gone, without which it can never be renewed,
	// and nothing the agent can do changes that. The unit the page gives lists it in
	// RestartPreventExitStatus=, so that Restart=always does not bring back every few seconds
	// an agent whose one heartbeat is refused, which is a retry loop run by systemd instead.
	exitJoinAgain = 3
)

// env is everything the verbs reach outside the process through.
//
// It exists so that a test runs serve against a temporary runner.env and runner.toml, a fake
// daemon and an API it counts the requests of, without touching /etc or the real environment.
type env struct {
	Out, Err io.Writer

	// Lookup is the environment.
	Lookup runner.Lookup

	// Geteuid is who the process runs as.
	Geteuid func() int

	// EnvFile and PolicyFile are /etc/agentiik/runner.env and /etc/agentiik/runner.toml.
	EnvFile, PolicyFile string

	// KeyFile and MemInfo are what join writes the host's key to and reads its memory from,
	// /var/lib/agentiik/runner.key and /proc/meminfo.
	KeyFile, MemInfo string

	// CredentialFile is where serve keeps the credential it renewed to, and join takes it away,
	// /var/lib/agentiik/credential.
	CredentialFile string

	// HelperFile is where the static helper is installed beside the agent,
	// /usr/local/lib/agentiik/agk-helper, which serve binds where runner.toml names none.
	HelperFile string

	// Account finds an account of this host by its name, which is who join gives what it
	// writes to.
	Account func(name string) (runner.Owner, error)

	// Host is what the driver asks of this machine rather than of the daemon: the
	// capabilities the agent holds and what its secrets directory is mounted as. Nil is
	// the kernel's own answers, which is what main gives.
	Host driver.Host
}

// command is one verb. The table is data so that the usage text and the dispatch cannot disagree.
type command struct {
	name, usage, summary string
	run                  func(context.Context, env, []string) int
}

// commands are the three verbs #command-line names for agk-runner, in its order.
var commands = []command{
	{
		name:    "join",
		usage:   "agk-runner join --api <url> --token <token> --labels <labels>",
		summary: "trade a join token for this runner's identity",
		run:     join,
	},
	{
		name:    "serve",
		usage:   "agk-runner serve",
		summary: "run the agent: heartbeat, pull, run, publish",
		run:     serve,
	},
	{
		name:    "version",
		usage:   "agk-runner version",
		summary: "print the version join sends",
		run:     version,
	},
}

func main() {
	// SIGTERM is how systemd and a container runtime stop a service, and an interrupt is how
	// a person at a terminal does. Either ends serve through its context.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// The first signal is taken, and the second is not: a person pressing Ctrl-C twice, or a
	// service manager that has waited long enough, means now.
	go func() {
		<-ctx.Done()
		stop()
	}()
	code := run(ctx, env{
		Out: os.Stdout, Err: os.Stderr,
		Lookup:  os.LookupEnv,
		Geteuid: os.Geteuid,
		EnvFile: runner.EnvPath, PolicyFile: driver.PolicyPath,
		KeyFile: runner.KeyPath, MemInfo: runner.MemInfoPath,
		CredentialFile: runner.CredentialPath,
		HelperFile:     runner.HelperPath,
		Account:        lookupAccount,
	}, os.Args[1:])
	stop()
	os.Exit(code)
}

// run is the whole command line, so that a test drives argv and reads the bytes.
func run(ctx context.Context, e env, args []string) int {
	if len(args) == 0 {
		usage(e.Err)
		return exitUsage
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		usage(e.Out)
		return exitSucceeded
	}
	for _, c := range commands {
		if c.name == args[0] {
			return c.run(ctx, e, args[1:])
		}
	}
	fmt.Fprintf(e.Err, "agk-runner: %s is not one of its verbs\n", args[0])
	usage(e.Err)
	return exitUsage
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "agk-runner is the Agentiik runner agent.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	width := 0
	for _, c := range commands {
		width = max(width, len(c.usage))
	}
	for _, c := range commands {
		fmt.Fprintf(w, "\t%-*s  %s\n", width, c.usage, c.summary)
	}
}

// version prints the version join sends, alone on its line, so that a script compares it with the
// control plane's as it is.
func version(_ context.Context, e env, args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(e.Err, "agk-runner version: it takes no arguments")
		return exitUsage
	}
	fmt.Fprintln(e.Out, runner.Version())
	return exitSucceeded
}
