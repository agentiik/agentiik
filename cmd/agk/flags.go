package main

import (
	"errors"
	"flag"
	"fmt"
	"runtime/debug"
	"strings"
)

// The flags, through the standard library's flag package and no dependency.
//
// The surface is small, the spellings are the documentation's, and a flag library would buy
// subcommands the command table already is. What flag does not give is a repeatable
// name=value flag, which is four lines below.

// flags builds the flag set of one command.
//
// It is ContinueOnError rather than ExitOnError because the whole command line is one
// function returning an exit code: a flag set that called os.Exit would take the process out
// from under a test holding the writers. Everything it prints goes to the Err of the Env for
// the same reason, and the usage line is the command's own, since a person who mistyped a
// flag wants the flags of the command they typed.
func flags(e Env, name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.Err)
	fs.Usage = func() {
		fmt.Fprintf(e.Err, "Usage:\n\t%s\n\nFlags:\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// parse reads the arguments and says whether the command may go on, with the code to leave
// with where it may not. A flag set that refused has already said what it refused, and the
// code is flag's own 2.
//
// Asking for the flags is not one of those. -h is a control the usage text offers, "every
// command takes -h for its own flags", so a person who typed it got what they asked for and
// the process leaves with 0: the same answer agk -h gives, and the same answer the standard
// library's own ExitOnError gives a flag set it owns. Collapsing it into the refusal would
// tell somebody they had made a mistake by reading the help.
func parse(fs *flag.FlagSet, args []string) (int, bool) {
	switch err := fs.Parse(args); {
	case err == nil:
		return exitSucceeded, true
	case errors.Is(err, flag.ErrHelp):
		// The set has already printed its own usage and its flags, through the Usage
		// this package gave it.
		return exitSucceeded, false
	default:
		return exitUsage, false
	}
}

// pairs is a flag written more than once, each time as name=value: --input, --input-file,
// --secret and --secret-file are each one of these.
//
// The order is kept as it was written, because a value written twice under one name is the
// last one winning, which is what a person retyping a flag means by it.
type pairs []string

func (p *pairs) String() string { return strings.Join(*p, ",") }

func (p *pairs) Set(v string) error {
	if !strings.Contains(v, "=") {
		return fmt.Errorf("%q is not written name=value", v)
	}
	*p = append(*p, v)
	return nil
}

// version is what this binary is, out of what the toolchain recorded in it.
//
// There is no constant to keep in step: every repository of the project carries the same
// version tagged at the same moment, and the tag is what the build stamps.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "agk, built with no version recorded"
	}
	v := info.Main.Version
	if v == "" {
		v = "(devel)"
	}
	line := "agk " + v
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && setting.Value != "" {
			line += ", " + setting.Value
		}
	}
	return line + ", " + info.GoVersion
}
