package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/bus/control"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/internal/otlp"
	"github.com/agentiik/agentiik/internal/stopsignal"
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

// untilSignalled runs fn with a context stopsignal makes, and answers its exit code. It is the one
// way the program is started, which a test starts it through too.
//
// The context is done at the first SIGINT or SIGTERM, which is a stop asked for: the lock is
// released on the way out rather than left for the database to notice. A second one ends a
// process whose way out is taking too long.
func untilSignalled(fn func(context.Context) int) int {
	ctx, stop := stopsignal.Context()
	defer stop()
	return fn(ctx)
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

// moduleVersion is the module's version as the build recorded it, as --version prints it.
func moduleVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
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
	// says why, rather than run on deaf: unless its file holds one renewed since, which the bus
	// connection comes back with once the bus drops the old one.
	work, expire := context.WithCancelCause(ctx)
	defer expire(nil)
	held := &credential{bus: c.Bus}
	if !c.Bus.Expires.IsZero() {
		go held.until(work, expire, log)
	}

	// What ended the program, once something has: nothing where ctx was stopped, which is a stop
	// asked for, and why otherwise. The credential's expiry is said as itself however it was met,
	// since the bus cuts the connection at the same second and what fails first on that is a race.
	ended := func(err error) error {
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(context.Cause(work), errCredentialExpired), held.expired(time.Now()):
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
		Reread:      rereadBus(c.Bus),
	})
	if err != nil {
		return ended(err)
	}
	defer b.Close()
	b.Trouble = func(subject string, err error) {
		log.Warn("a message was taken off the queue without being handled", "subject", subject, "error", err)
	}

	// The metrics are answered from before the lock is held, so that a standby says it stands by
	// and a scraper can tell a standby from a controller that is not there.
	counts := newCounted(b, log)
	if c.Metrics.Listen != "" {
		stop, err := counts.serveMetrics(work, c.Metrics, log)
		if err != nil {
			return ended(err)
		}
		defer stop()
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

	o := options(c, queue, versions)
	traces, err := tracer(c, name, log)
	if err != nil {
		return err
	}
	if traces != nil {
		// Whatever is still queued is sent on the way out, once the lock is already released,
		// for five seconds at most: what the program sends on the way out is bounded, so that
		// a supervisor's stop is not spent waiting on a collector, and what is left is dropped.
		defer func() {
			flush, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			traces.Close(flush)
		}()
		o.Tracer = traces
	}

	log.Info("standing by for the lock", "name", name)
	err = ctl.Lead(work, func(ctx context.Context, term db.Term) error {
		log.Info("leading", "name", name, "term", term.Token)
		defer counts.lead(ctl, term)()
		o := o
		o.Observer = counts
		return lead(ctx, ctl, term, queue, o, export, verifier(pool.AuditTrail(), 0, log), log)
	})
	return ended(err)
}

// tracer is where every run's trace goes, where AGK_OTLP_ENDPOINT names a collector, and nil where
// it names none: then nothing is built, queued or sent, and no core is handed anything to call.
//
// The resource names this program, its version and this instance, which is what a backend lists the
// spans under and what tells two controllers' traces apart. A batch the collector refused, and spans
// dropped for want of room, are said as warnings: the runs went on either way.
func tracer(c config.Controller, name string, log *slog.Logger) (*otlp.Exporter, error) {
	if c.OTLPEndpoint == "" {
		return nil, nil
	}
	return otlp.New(otlp.Options{
		Endpoint: c.OTLPEndpoint,
		Resource: []otlp.Attribute{
			otlp.String("service.name", program),
			otlp.String("service.version", moduleVersion()),
			otlp.String("service.instance.id", name),
		},
		Trouble: func(err error) { log.Warn("spans were not sent", "error", err) },
	})
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

// verifier is what checks the audit log's chain in the database at the start of a term, reading
// batch entries at a time, the default where batch is not positive.
//
// It warns, naming the first broken entry, and ends nothing: the chain is evidence of what was done
// to the log, and a controller that stopped leading over it would stop the runs and leave the log as
// it is. A verification that could not finish is said too, and the next term tries again from as
// far as this one reached.
func verifier(trail db.AuditTrail, batch int, log *slog.Logger) func(context.Context) {
	return func(ctx context.Context) {
		v, err := trail.Verify(ctx, batch)
		var broke *audit.Break
		var disagrees *db.AuditRecordDisagrees
		switch {
		case errors.As(err, &broke):
			log.Warn("the audit log's chain in the database is broken, and an entry was changed or removed after it was written: compare the log with its copy outside the installation", "entry", broke.Seq, "error", err)
		case errors.As(err, &disagrees):
			log.Warn("the audit log's chain holds, and does not agree with how far it was last verified: either it was written again from that entry, head and all, or the record was written by something other than a verification; compare the log with its copy outside the installation", "entry", disagrees.Seq, "through", v.Through, "error", err)
		case err != nil && ctx.Err() == nil:
			log.Warn("the audit log's chain could not be verified, and the next term carries on from where this one stopped", "through", v.Through, "error", err)
		case err == nil:
			log.Info("the audit log's chain holds", "from", v.From, "through", v.Through)
		}
	}
}

// lead is one term: watching and sweeping on one side, taking results and progress back on the
// other, until the fence refuses a write, either of them fails, or ctx is done. The audit log is
// exported beside them for as long as the term lasts, by the one controller that leads, so that two
// never race each other to the sink; a sink that fails ends nothing, and is tried again. Its chain
// in the database is verified beside them too, once at the start of the term where verify is not
// nil, and a break found holds nothing up.
//
// Each goes through the core of the term, and neither ends it for a run or a result it could not
// handle. Watch returns whatever the function it calls returns, so a notification about one run
// that cannot be decided, a version that will not build or a pass that met another one writing
// the same run, would end the term, and the program with it, over one run; a sweep reports such a
// run and moves on, and a notification is only a shortcut to what a sweep finds. So both are
// reported and left to the next sweep, and only the fence ends the term.
func lead(ctx context.Context, ctl *controller.Controller, term db.Term, queue *control.Queue, o controller.Options, export *audit.Exporter, verify func(context.Context), log *slog.Logger) error {
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

	if verify != nil {
		verified := make(chan struct{})
		go func() {
			defer close(verified)
			verify(ctx)
		}()
		// As the export's: a term never ends with its verification still reading.
		defer func() {
			cancel(nil)
			<-verified
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

// rereadBus reads the control plane's credential again from its file, which the bus connection
// does at every reconnection, so that it comes back with a credential renewed while the program
// runs.
func rereadBus(b config.Bus) func() (bus.Credentials, error) {
	return func() (bus.Credentials, error) {
		renewed, err := config.RereadBus(b)
		if err != nil {
			return bus.Credentials{}, err
		}
		return bus.Credentials{JWT: renewed.JWT, Seed: string(renewed.Seed)}, nil
	}
}

// credential is the control plane's bus credential as the program last read it, which a renewal
// in its file moves on.
type credential struct {
	mu  sync.Mutex
	bus config.Bus
}

// expires is when the credential last read expires, zero for one that never does.
func (c *credential) expires() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bus.Expires
}

// expired says whether the credential has expired at now, with none renewed in its file since. The
// file is read again only past the expiry of the one held.
func (c *credential) expired(now time.Time) bool {
	expires := c.expires()
	if expires.IsZero() || now.Before(expires) {
		return false
	}
	return !c.renewed(now)
}

// renewed reads the file again, and takes what it holds where that lasts past now.
func (c *credential) renewed(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := config.RereadBus(c.bus)
	if err != nil || !b.Expires.IsZero() && !now.Before(b.Expires) {
		return false
	}
	c.bus = b
	return true
}

// until waits for the credential to expire, and ends the program's work then with
// errCredentialExpired, unless the file holds one renewed since, which it then waits on in turn.
// It returns once ctx is done.
func (c *credential) until(ctx context.Context, expire context.CancelCauseFunc, log *slog.Logger) {
	for {
		expires := c.expires()
		if expires.IsZero() {
			return
		}
		t := time.NewTimer(time.Until(expires))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if !c.renewed(time.Now()) {
			expire(errCredentialExpired)
			return
		}
		log.Info("took the control plane's bus credential renewed in its file", "expires", formatExpiry(c.expires()))
	}
}

// formatExpiry is an expiry as the logs write it, or never.
func formatExpiry(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}
