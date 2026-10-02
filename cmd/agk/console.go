package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/agentiik/agentiik/cmd/agk/internal/console"
	"github.com/agentiik/agentiik/db"
)

// agk console: the web console's views in a terminal, full screen, over the routes the web console
// reads, as the principal agk is signed in as. It is agk speaking to /api/v1 as agk logs and agk
// status do: nothing to install, open or turn on at the installation, and no permission of its own.
//
// Where there is no screen to draw on, standard input or output not a terminal or TERM=dumb, the
// view it would have opened is printed once as plain lines and it leaves with 0, so that a script
// or a cron job never waits on a screen nobody sees: the runs as console.Lines writes them, and a
// run as agk status prints it.
//
// It leaves with 0 when quit, 2 for a wrong command line, and 4 where the installation cannot be
// reached at start, as every verb does. Once open, an installation that stops answering is said so
// in the top line and asked again, since somebody is watching.
func consoleVerb(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk console", "agk console [run] [--namespace <ns>] [--server <url>]")
	server := fs.String("server", "", "The installation. "+serverDefault)
	namespace := fs.String("namespace", "", "Opens on the runs of one namespace, rather than of every one you read.")
	named, code, ok := positional(fs, args)
	if !ok {
		return code
	}
	if len(named) > 1 {
		fmt.Fprintln(e.Err, "agk console opens on one run at most, by its identifier, or on the runs where none is named")
		return exitUsage
	}
	// AGENTIIK_THEME settles the ground for a terminal that never says what its background is,
	// and a value that is neither is refused before anything is asked, as a wrong flag is.
	theme := e.getenv(themeVariable)
	if theme != "" && theme != "light" && theme != "dark" {
		fmt.Fprintf(e.Err, "%s is light or dark, and %q is neither\n", themeVariable, theme)
		return exitUsage
	}
	at, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}
	run := ""
	if len(named) == 1 {
		run = named[0]
	}

	// Whom the installation takes the credential for, read before anything is drawn: an
	// installation that cannot be reached at start is said so and left, as every verb does, and
	// one that refuses the credential is said so before a screen hides the sentence.
	var me struct {
		Principal string `json:"principal"`
	}
	if err := at.getJSON(ctx, "/api/v1/me", &me); err != nil {
		if statusOf(err) == http.StatusUnauthorized {
			fmt.Fprintln(e.Err, at.refusedCredential())
		} else {
			fmt.Fprintln(e.Err, sentence(err))
		}
		if errors.Is(err, errUnreachable) {
			return exitNoOutcome
		}
		return exitRefused
	}

	if !e.terminal() {
		return plainly(ctx, e, at, run, *namespace)
	}
	host := at.base
	if u, err := url.Parse(at.base); err == nil && u.Host != "" {
		host = u.Host
	}
	err := console.Run(ctx, console.Options{
		Installation: host,
		Namespace:    *namespace,
		Run:          run,
		Read:         at.getJSON,
		Follow:       at.followLog,
		Send:         at.ask,
		Now:          e.now,
		Getenv:       e.getenv,
		Theme:        theme,
	})
	if err != nil {
		fmt.Fprintf(e.Err, "the console stopped: %s\n", err)
		return exitNoOutcome
	}
	return exitSucceeded
}

// ask sends what agk console asks: cancelling or replaying a run, both answered 202, asked and not
// yet done; and dismissing a notification, answered 204.
func (at remote) ask(ctx context.Context, method, path string, body, out any) error {
	want := http.StatusAccepted
	if method == http.MethodDelete {
		want = http.StatusNoContent
	}
	return at.sendJSON(ctx, method, path, body, want, out)
}

// themeVariable names the ground agk console draws on, light or dark, where the terminal does not
// answer the question of its background, as some multiplexers never do.
const themeVariable = "AGENTIIK_THEME"

// plainly prints once the view the console would have opened, for wherever there is no screen.
func plainly(ctx context.Context, e Env, at remote, run, namespace string) int {
	if run != "" {
		var d db.RunDetail
		if err := at.getJSON(ctx, "/api/v1/runs/"+url.PathEscape(run), &d); err != nil {
			fmt.Fprintln(e.Err, at.aboutRun(run, err))
			if errors.Is(err, errUnreachable) {
				return exitNoOutcome
			}
			return exitRefused
		}
		describe(e.Out, d, e.now(), false)
		return exitSucceeded
	}
	path := "/api/v1/runs?limit=100"
	if namespace != "" {
		path += "&namespace=" + url.QueryEscape(namespace)
	}
	var listed struct {
		Runs []db.ListedRun `json:"runs"`
	}
	if err := at.getJSON(ctx, path, &listed); err != nil {
		fmt.Fprintln(e.Err, sentence(err))
		if errors.Is(err, errUnreachable) {
			return exitNoOutcome
		}
		return exitRefused
	}
	if err := console.Lines(e.Out, listed.Runs, e.now()); err != nil {
		return exitNoOutcome
	}
	return exitSucceeded
}

// terminal says whether there is a screen to draw on: standard input and output both a terminal,
// and a TERM that is not dumb. An Env that says nothing has none, so that a test never opens one.
func (e Env) terminal() bool {
	if e.Terminal == nil || e.getenv("TERM") == "dumb" {
		return false
	}
	return e.Terminal()
}

// attached says whether both ends of the process are a terminal, read from the files themselves
// with the standard library: a character device is what a terminal is to the system, and a pipe,
// a file or /dev/null is not one.
func attached() bool {
	for _, f := range []*os.File{os.Stdin, os.Stdout} {
		info, err := f.Stat()
		if err != nil || info.Mode()&os.ModeCharDevice == 0 {
			return false
		}
	}
	return true
}
