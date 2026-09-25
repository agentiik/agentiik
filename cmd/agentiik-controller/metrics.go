package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/internal/metrics"
)

// The metrics, which this program exports and the API does not.
//
// The controller is the one process that knows every figure the documentation asks for, or can read
// it: it decides every dispatch, retry and loss and every run's verdict, it holds the control
// plane's connection to the bus the queues are on, and it reads the runners and what they hold from
// the database their heartbeats are written to. It is also the one process of which exactly one
// leads. Every figure is reported by the one that leads and by no other, so that a sum over every
// instance scraped is the installation's figure: an API of three replicas, each reading the same
// database, would report every gauge three times. A standby reports agentiik_controller_leading 0
// and its counters as they stood, which are zero, since the term that ends ends the process too.
//
// On a listener of its own, AGK_METRICS_LISTEN, and never on the API's, which faces tenants: the
// metrics name every namespace and workflow that ran. Package internal/metrics says why a token is
// asked of a scrape all the same.

// durationBuckets are the upper bounds, in seconds, of both histograms: a second up to a day, since
// a task runs for seconds or for its hour of ceiling and a run for as long as its steps and its
// waits for a concurrency group take.
var durationBuckets = []float64{1, 5, 10, 30, 60, 120, 300, 600, 1800, 3600, 10800, 43200, 86400}

// scrapeBound is the longest a scrape's reads of the database and the bus may take, well inside the
// ten seconds a Prometheus scrape waits by default, so that a slow database fails a scrape rather
// than piling scrapes up behind it.
const scrapeBound = 5 * time.Second

// counted is the controller's metrics: what the core tells, as counters and histograms, and what
// is read at a scrape, as gauges.
type counted struct {
	registry   *metrics.Registry
	dispatched *metrics.Counter
	lost       *metrics.Counter
	retried    *metrics.Counter
	ended      *metrics.Histogram
	runs       *metrics.Histogram

	// term is the term this process leads under, and nil while it stands by. The gauges are read
	// through its fence, as every read of a controller is.
	term atomic.Pointer[leading]
}

// leading is what a scrape reads the database through while this process leads.
type leading struct {
	ctl  *controller.Controller
	term db.Term
}

var _ controller.Observer = (*counted)(nil)

// newCounted registers every family, reading the queues on b.
func newCounted(b *bus.Bus, log *slog.Logger) *counted {
	r := metrics.NewRegistry()
	r.Trouble = func(families []string, err error) {
		log.Warn("a scrape left metrics out, since they could not be read", "metrics", strings.Join(families, ", "), "error", err)
	}
	c := &counted{registry: r}
	c.dispatched = r.Counter("agentiik_tasks_dispatched_total",
		"Dispatches handed to a pool's queue: first dispatches, further attempts and requeues alike. The denominator of the loss rate.",
		"pool")
	c.lost = r.Counter("agentiik_tasks_lost_total",
		"Dispatches declared lost, by the heartbeat's sweep or by their runner, charged to the pool of the runner that held them.",
		"pool")
	c.retried = r.Counter("agentiik_task_retries_total",
		"Further attempts a step's retry policy granted after a failure. Over agentiik_task_duration_seconds_count, the retry rate.",
		"brick", "version")
	c.ended = r.Histogram("agentiik_task_duration_seconds",
		"How long a container ran, from the start to the end its runner reported, by the brick and version its manifest declares and the task's ending. A script step has no brick.",
		durationBuckets, "brick", "version", "state")
	c.runs = r.Histogram("agentiik_run_duration_seconds",
		"End-to-end latency of a run, from its creation to its verdict, waits for its concurrency group and its quota included.",
		durationBuckets, "namespace", "workflow", "state")

	r.Gauges(c.readLeading, metrics.Desc{
		Name: "agentiik_controller_leading",
		Help: "1 where this controller holds the lock and decides, 0 where it stands by. Every other figure is the leader's alone.",
	})
	r.Gauges(c.readQueues(b), metrics.Desc{
		Name:   "agentiik_queue_depth",
		Help:   "Task messages on a pool's queue that no runner has acknowledged: waiting for a runner with room, or being redeemed by one.",
		Labels: []string{"pool"},
	}, metrics.Desc{
		Name:   "agentiik_runner_pool_label",
		Help:   "1 for every label a pool carries: joined on pool, it reads a pool's figures per runner label, since a step goes to the one pool whose labels include all of its runs_on.",
		Labels: []string{"pool", "label"},
	})
	r.Gauges(c.readRunners, metrics.Desc{
		Name:   "agentiik_runner_slots",
		Help:   "The tasks a runner heard from in the last three heartbeat intervals said it runs at once.",
		Labels: []string{"pool", "runner"},
	}, metrics.Desc{
		Name:   "agentiik_runner_tasks",
		Help:   "Dispatches bound to a runner and in flight. Over agentiik_runner_slots, its occupancy.",
		Labels: []string{"pool", "runner"},
	}, metrics.Desc{
		Name:   "agentiik_runner_ready",
		Help:   "1 where a runner takes new work: neither drained nor revoked, and it last said ready.",
		Labels: []string{"pool", "runner"},
	})
	return c
}

// Dispatched counts a dispatch on its pool's counter.
func (c *counted) Dispatched(pool string) { c.dispatched.Inc(pool) }

// Lost counts a loss on the pool of the runner that held it.
func (c *counted) Lost(pool string) { c.lost.Inc(pool) }

// Retried counts a further attempt.
func (c *counted) Retried(brick, version string) { c.retried.Inc(brick, version) }

// Ended counts how long a container ran.
func (c *counted) Ended(brick, version string, state agk.TaskState, ran time.Duration) {
	c.ended.Observe(ran.Seconds(), brick, version, state.String())
}

// RunEnded counts a run's end-to-end latency.
func (c *counted) RunEnded(namespace, workflow string, state agk.RunState, took time.Duration) {
	c.runs.Observe(took.Seconds(), namespace, workflow, state.String())
}

// lead says this process leads under term, until the function it answers is called.
func (c *counted) lead(ctl *controller.Controller, term db.Term) func() {
	c.term.Store(&leading{ctl: ctl, term: term})
	return func() { c.term.Store(nil) }
}

func (c *counted) readLeading(_ context.Context, g *metrics.Gauges) error {
	v := 0.0
	if c.term.Load() != nil {
		v = 1
	}
	g.Set("agentiik_controller_leading", v)
	return nil
}

// readQueues reads every pool with its labels, and the depth of each one's queue. A queue the bus
// holds messages on under a pool that no longer exists is reported too: those are tasks nobody will
// take.
func (c *counted) readQueues(b *bus.Bus) func(context.Context, *metrics.Gauges) error {
	return func(ctx context.Context, g *metrics.Gauges) error {
		l := c.term.Load()
		if l == nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, scrapeBound)
		defer cancel()
		var pools []db.RunnerPool
		if err := l.ctl.Fenced(ctx, l.term, func(ctx context.Context, w *db.Wide) error {
			var err error
			pools, err = w.RunnerPools(ctx)
			return err
		}); err != nil {
			return err
		}
		depths, err := b.Depths(ctx)
		if err != nil {
			return err
		}
		for _, p := range pools {
			g.Set("agentiik_queue_depth", float64(depths[p.Name]), p.Name)
			delete(depths, p.Name)
			for _, label := range p.Labels {
				g.Set("agentiik_runner_pool_label", 1, p.Name, label)
			}
		}
		for pool, n := range depths {
			g.Set("agentiik_queue_depth", float64(n), pool)
		}
		return nil
	}
}

// readRunners reads every runner heard from, what it runs at once and what it holds.
func (c *counted) readRunners(ctx context.Context, g *metrics.Gauges) error {
	l := c.term.Load()
	if l == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, scrapeBound)
	defer cancel()
	var occupancy []db.Occupancy
	if err := l.ctl.Fenced(ctx, l.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		occupancy, err = w.Occupancy(ctx, time.Now().UTC())
		return err
	}); err != nil {
		return err
	}
	for _, o := range occupancy {
		ready := 0.0
		if o.Ready {
			ready = 1
		}
		g.Set("agentiik_runner_slots", float64(o.Slots), o.Pool, o.Runner)
		g.Set("agentiik_runner_tasks", float64(o.Held), o.Pool, o.Runner)
		g.Set("agentiik_runner_ready", ready, o.Pool, o.Runner)
	}
	return nil
}

// serveMetrics answers the metrics on m.Listen until ctx is done, and answers once it is listening:
// an address it cannot listen on refuses the start rather than leaving an installation's monitoring
// to find out that nothing answers.
func (c *counted) serveMetrics(ctx context.Context, m config.Metrics, log *slog.Logger) (func(), error) {
	raw, err := hex.DecodeString(m.TokenHash)
	if err != nil || len(raw) != sha256.Size {
		return nil, errors.New("the metrics token's hash is not a SHA-256 in hexadecimal")
	}
	var hash [sha256.Size]byte
	copy(hash[:], raw)

	ln, err := net.Listen("tcp", m.Listen)
	if err != nil {
		return nil, fmt.Errorf("the metrics cannot be answered on %s, which %s names: %w", m.Listen, config.MetricsListen, err)
	}
	server := &http.Server{
		Handler:           metrics.Handler(c.registry, hash),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("the metrics stopped being answered", "error", err)
		}
	}()
	log.Info("answering the metrics", "address", ln.Addr().String(), "path", metrics.Path)
	return func() {
		server.Close()
		<-done
	}, nil
}
