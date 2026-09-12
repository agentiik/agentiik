package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// env is everything the three verbs reach the world through.
//
// It exists so that a test runs a verb against a temporary tree laid out as /agk/in and
// /agk/out, with no container and no daemon anywhere near it: all three verbs are file
// reading and file writing, and a program whose only input is a directory and an
// environment is a program that can be tested as one.
type env struct {
	// Root is /agk in a container. Nothing outside it is read or written.
	Root string

	In       io.Reader
	Out, Err io.Writer

	Getenv func(string) string
	Now    func() time.Time
}

// command is one verb. The table is data so that the usage text and the dispatch cannot
// disagree about what this program offers.
type command struct {
	name    string
	usage   string
	summary string
	run     func(env, []string) error
}

// commands are the three verbs the documentation names, spelled exactly so. There is no
// fourth: what is not in the documented list is not offered, and a verb invented here
// would be a contract nobody agreed to, mounted inside every container that runs a
// script step.
var commands = []command{
	{
		name:    "items",
		usage:   "agk items [--port <port>]",
		summary: "write the items of the input envelope, one JSON object per line",
		run:     items,
	},
	{
		name:    "emit",
		usage:   "agk emit <port> [--from <file>|-] [--filter <expr>] [--id <field>]",
		summary: "publish items on an output port",
		run:     emit,
	},
	{
		name:    "attach",
		usage:   "agk attach <file> --port <port> [--item <id>|--all] [--name <name>] [--media-type <type>]",
		summary: "attach a file to an item of an output port",
		run:     attach,
	},
}

func main() {
	os.Exit(run(env{
		Root:   Root,
		In:     os.Stdin,
		Out:    os.Stdout,
		Err:    os.Stderr,
		Getenv: os.Getenv,
		Now:    time.Now,
	}, os.Args[1:]))
}

// run is the whole command line, so that a test drives argv and reads the bytes.
//
// Two codes, and the exit-code table is why. 0 is what a verb that did its work exits
// with. Everything else is 1, which is the band the table calls an application failure,
// retried only where the step says so: a helper that could not do what the script asked
// is the script's own failure. Nothing here exits in the transient band, because a
// refusal by this program is not something a second attempt would answer differently;
// nothing exits 120, because invalid input is the step's judgment of the batch it was
// given and not a tool's complaint about its own arguments; and nothing exits 121 to 124,
// which are reserved for the runner.
func run(e env, args []string) int {
	if len(args) == 0 {
		usage(e.Err)
		return 1
	}

	switch args[0] {
	case "help", "-h", "--help":
		usage(e.Out)
		return 0
	}

	for _, c := range commands {
		if c.name != args[0] {
			continue
		}
		err := c.run(e, args[1:])
		switch {
		case err == nil:
			return 0
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintln(e.Out, "usage: "+c.usage)
			return 0
		default:
			// The verb, then what was refused. A container's log carries the
			// output of a whole script, so a line out of this program says
			// which of its verbs wrote it.
			fmt.Fprintf(e.Err, "agk %s: %s\n", c.name, err)
			return 1
		}
	}

	fmt.Fprintf(e.Err, "agk: %s is not one of this helper's verbs\n", args[0])
	usage(e.Err)
	return 1
}

// usage writes what this program offers, which is the command table read out.
func usage(w io.Writer) {
	fmt.Fprintln(w, "agk is the helper mounted at "+BinPath+", for scripts that want to be precise rather than lucky.")
	fmt.Fprintln(w, "It is a convenience and never a requirement: a step that parses the envelope itself needs none of it.")
	fmt.Fprintln(w)
	for _, c := range commands {
		fmt.Fprintf(w, "  %s\n      %s\n", c.usage, c.summary)
	}
}

// flags builds the flag set of one verb. The standard library's own, because the surface
// is three verbs and nine flags and a dependency for that would be carried inside every
// container that runs a script step.
//
// Nothing is printed by the set itself. A parse failure comes back as an error like every
// other refusal, so that one place decides what a refusal looks like.
func flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("agk "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// leading takes the one argument a verb names before its flags, which is the port for emit
// and the file for attach.
//
// The standard flag package stops at the first argument that is not a flag, and the
// documented example writes agk emit out --from /tmp/result.json, so the argument is taken
// off the front before the set is handed the rest.
func leading(args []string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", args
	}
	return args[0], args[1:]
}

// argument settles the one argument a verb takes, from whichever side of the flags it was
// written on, and refuses a second.
//
// Either side, because a script that wrote agk attach --port out /tmp/report.csv meant the
// same thing as the documented order and being told so is no use to it. A second argument is
// refused rather than ignored: a word silently dropped is a script nobody can debug.
func argument(leading string, set *flag.FlagSet, usage string) (string, error) {
	rest := set.Args()
	if leading != "" {
		if len(rest) > 0 {
			return "", fmt.Errorf("%q is one argument too many: %s", rest[0], usage)
		}
		return leading, nil
	}
	switch len(rest) {
	case 0:
		return "", nil
	case 1:
		return rest[0], nil
	default:
		return "", fmt.Errorf("%q is one argument too many: %s", rest[1], usage)
	}
}

// noArguments refuses what a verb that takes none was given, rather than ignoring it.
func noArguments(set *flag.FlagSet, usage string) error {
	if set.NArg() == 0 {
		return nil
	}
	return fmt.Errorf("%q is one argument too many: %s", set.Arg(0), usage)
}
