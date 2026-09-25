package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/bus/control"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/version"
)

// The exit codes, which doc.go sets out.
const (
	exitStopped = 0
	exitFailed  = 1
	exitUsage   = 2
)

// program is what this binary calls itself, in what it prints and on its bus connection.
const program = "agentiik-controller"

func main() {
	os.Exit(untilSignalled(func(ctx context.Context) int {
		return run(ctx, os.Args[1:], nil, os.Stdout, os.Stderr)
	}))
}

// untilSignalled runs fn with a context signalled makes, and answers its exit code. It is the one
// way the program is started, which a test starts it through too.
func untilSignalled(fn func(context.Context) int) int {
	ctx, stop := signalled()
	defer stop()
	return fn(ctx)
}

// signalled is done at the first SIGINT or SIGTERM: a person's interrupt, and what systemd and
// docker stop send before they kill. Either is a stop asked for, and the lock is released on the
// way out rather than left for the database to notice.
//
// Only the first is taken. The signals go back to their default once it has arrived, so that a
// second one ends a process whose way out is taking too long, as a person pressing Ctrl-C twice
// means it to.
func signalled() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

// run is the whole program: the arguments, the configuration, and the exit code. lookup reads the
// environment, and is os.LookupEnv where nil.
func run(ctx context.Context, args []string, lookup config.Lookup, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 1 && (args[0] == "--version" || args[0] == "-version"):
		fmt.Fprintln(stdout, versionLine())
		return exitStopped
	case len(args) == 1 && (args[0] == "--help" || args[0] == "-help" || args[0] == "-h"):
		usage(stdout)
		return exitStopped
	case len(args) > 0:
		fmt.Fprintf(stderr, "%s: it takes no argument, and was given %q: everything it reads is in its environment\n", program, args)
		usage(stderr)
		return exitUsage
	}

	c, err := config.ReadController(lookup)
	if err != nil {
		// One line per setting, each naming its variable, which is how config words them.
		fmt.Fprintf(stderr, "%s: the configuration refuses the start:\n%s\n", program, err)
		return exitFailed
	}
	return start(ctx, c, stderr)
}

// start runs the controller on a configuration already read, and answers the exit code.
func start(ctx context.Context, c config.Controller, stderr io.Writer) int {
	if err := serve(ctx, c, logger(stderr)); err != nil {
		fmt.Fprintf(stderr, "%s: %s\n", program, err)
		return exitFailed
	}
	return exitStopped
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "usage: %s\n\nLeads by advisory lock, sweeps, consumes results and publishes tasks. It reads its settings from AGK_* environment variables, as https://agentiik.github.io/docs/#configuration lists them, and takes no argument but --version and --help.\n", program)
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

// logger writes to w as text: a controller's standard error is read by journald, docker logs or a
// person, and each of those reads text.
func logger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, nil))
}

// errCredentialExpired ends the program when the control plane's bus credential runs out.
var errCredentialExpired = errors.New("the control plane's bus credential has expired, and the bus refuses it from now on: write a new one to the file " + config.BusCredentialsFile + " names, and start the controller again")

// serve opens what the controller stands on and leads whenever it holds the lock, until ctx is
// done or something ends the program. It answers nil once ctx is done, and why otherwise.
func serve(ctx context.Context, c config.Controller, log *slog.Logger) error {
	name := instanceName(os.Getpid())

	// Past the credential's expiry nothing on the bus answers, so the program ends then, and
	// says why, rather than run on deaf.
	work := ctx
	if !c.Bus.Expires.IsZero() {
		var expire context.CancelFunc
		work, expire = context.WithDeadlineCause(ctx, c.Bus.Expires, errCredentialExpired)
		defer expire()
	}

	// What ended the program, once something has: nothing where ctx was stopped, which is a stop
	// asked for, and why otherwise. The credential's expiry is said as itself however it was met,
	// since the bus cuts the connection at the same second and what fails first on that is a race.
	ended := func(err error) error {
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(context.Cause(work), errCredentialExpired),
			!c.Bus.Expires.IsZero() && !time.Now().Before(c.Bus.Expires):
			return errCredentialExpired
		case context.Cause(work) != nil:
			return context.Cause(work)
		}
		return err
	}

	pool, err := db.Open(work, db.WithKeepalives(c.Database.ConnString()))
	if err != nil {
		return ended(err)
	}
	defer pool.Close()

	b, err := bus.Open(work, bus.Options{
		URL:         c.Bus.URL,
		Name:        program + " " + name,
		Credentials: &bus.Credentials{JWT: c.Bus.JWT, Seed: string(c.Bus.Seed)},
	})
	if err != nil {
		return ended(err)
	}
	defer b.Close()
	b.Trouble = func(subject string, err error) {
		log.Warn("a message was taken off the queue without being handled", "subject", subject, "error", err)
	}

	versions, err := version.New(pool, version.Options{})
	if err != nil {
		return err
	}
	ctl, err := controller.New(pool, name)
	if err != nil {
		return err
	}
	ctl.Trouble = func(run agk.RunID, err error) {
		if run == "" {
			log.Warn("the controller met trouble", "error", err)
			return
		}
		log.Warn("the controller met trouble with a run", "run", run, "error", err)
	}
	queue := control.New(b)
	export := exporter(c.AuditExport, pool.AuditTrail(), log)

	log.Info("standing by for the lock", "name", name)
	err = ctl.Lead(work, func(ctx context.Context, term db.Term) error {
		log.Info("leading", "name", name, "term", term.Token)
		return lead(ctx, ctl, term, queue, options(c, queue, versions), export, log)
	})
	return ended(err)
}

// instanceName is what an operator reads on the term, which is written with the holder's name so
// that two processes each believing they lead can be told apart. The host alone is not enough,
// since two instances on one host are how a standby is tested, and the process identifier alone is
// 1 in every container.
func instanceName(pid int) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s/%d", host, pid)
}

// options are what the core of every term is given, from the configuration.
//
// max_requeues and the task ceiling are the installation's, read from AGK_MAX_REQUEUES and
// AGK_TASK_CEILING, and never a workflow's. The size rules and the clock are the defaults, which
// is what a zero value is given.
func options(c config.Controller, q controller.Queue, v controller.Versions) controller.Options {
	requeues := c.MaxRequeues
	return controller.Options{
		Queue: q, Versions: v, Objects: artifact.Dir(c.Objects),
		Ceiling: c.TaskCeiling, MaxRequeues: &requeues,
	}
}

// exporter is what sends the audit log outside the installation, or nil where the configuration
// names no sink, which is said at every start: the log is kept in the database either way, and
// an installation whose own host may be unreadable after an incident has no other copy of it.
func exporter(sink config.AuditExport, trail db.AuditTrail, log *slog.Logger) *audit.Exporter {
	if sink.URL == "" {
		log.Warn("the audit log is exported nowhere, and is kept only in the installation's own database: name a sink outside it in " + config.AuditExportURL)
		return nil
	}
	return &audit.Exporter{
		Source: trail, URL: sink.URL, Token: string(sink.Token),
		Trouble: func(err error) {
			log.Warn("the audit log could not be exported, and is tried again", "error", err)
		},
	}
}

// lead is one term: watching and sweeping on one side, taking results and progress back on the
// other, until the fence refuses a write, either of them fails, or ctx is done. The audit log is
// exported beside them for as long as the term lasts, by the one controller that leads, so that two
// never race each other to the sink; a sink that fails ends nothing, and is tried again.
//
// Each goes through the core of the term, and neither ends it for a run or a result it could not
// handle. Watch returns whatever the function it calls returns, so a notification about one run
// that cannot be decided, a version that will not build or a pass that met another one writing
// the same run, would end the term, and the program with it, over one run; a sweep reports such a
// run and moves on, and a notification is only a shortcut to what a sweep finds. So both are
// reported and left to the next sweep, and only the fence ends the term.
func lead(ctx context.Context, ctl *controller.Controller, term db.Term, queue *control.Queue, o controller.Options, export *audit.Exporter, log *slog.Logger) error {
	core, err := controller.NewCore(ctl, term, o)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	done := make(chan error, 2)

	if export != nil {
		exported := make(chan struct{})
		go func() {
			defer close(exported)
			export.Run(ctx)
		}()
		// Deferred after the cancel above, so it runs first: the term's context is cancelled,
		// then the export is waited for, and a term never ends with its export still sending.
		defer func() {
			cancel(nil)
			<-exported
		}()
	}

	go func() {
		done <- ctl.Watch(ctx, func(ctx context.Context, w controller.Wake) error {
			err := core.Wake(ctx, w)
			if err == nil || errors.Is(err, db.ErrFenced) || ctx.Err() != nil {
				return err
			}
			if w.Swept {
				log.Warn("a sweep could not finish, and the next one comes round", "error", err)
			} else {
				log.Warn("a run could not be decided, and the next sweep comes round to it", "run", w.Run, "error", err)
			}
			return nil
		})
	}()

	go func() {
		done <- queue.Answers(ctx, func(ctx context.Context, a controller.Answer) error {
			err := core.Answer(ctx, a)
			switch {
			case err == nil, ctx.Err() != nil:
			case errors.Is(err, db.ErrFenced):
				// Answers leaves a result it could not record for another delivery
				// and carries on, which is right for every other error and wrong for
				// this one: the term has passed, and the result is the new holder's.
				cancel(err)
			case errors.Is(err, controller.ErrNotAResult):
				// Taken off the queue and said through the bus's Trouble.
			default:
				log.Warn("a result could not be recorded, and it is delivered again", "task", a.Result.Task, "runner", a.Runner, "error", err)
			}
			return err
		}, func(ctx context.Context, p controller.Progress) error {
			// Held to the fence as an answer is, and otherwise left to the bus: one
			// refused is said through its Trouble, and any other error brings it round
			// again, by when the task may have ended and it changes nothing.
			err := core.Progress(ctx, p)
			switch {
			case err == nil, ctx.Err() != nil:
			case errors.Is(err, db.ErrFenced):
				cancel(err)
			case errors.Is(err, controller.ErrNotAResult):
			case errors.Is(err, db.ErrNotYetDispatched):
				// Early rather than wrong: the pass that published the task has not
				// recorded it yet, and a later delivery finds it recorded.
			default:
				log.Warn("a task's progress could not be recorded, and it is delivered again", "task", p.Task, "runner", p.Runner, "state", p.State, "error", err)
			}
			return err
		})
	}()

	first := <-done
	cancel(first)
	<-done
	// The cause is the first thing that ended the term, which is the fence where an answer
	// was refused by it, rather than the cancellation that stopped the other side.
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return first
}
