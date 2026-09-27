package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
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
	"github.com/agentiik/agentiik/purge"
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

// counted is the controller's metrics: what the core tells, as counters and histograms, and what
// is read at a scrape, as gauges.
type counted struct {
	registry   *metrics.Registry
	dispatched *metrics.Counter
	lost       *metrics.Counter
	retried    *metrics.Counter
	ended      *metrics.Histogram
	runs       *metrics.Histogram

	// What the purges and the collection removed, pass after pass.
	artifactsExpired *metrics.Counter
	runsPurged       *metrics.Counter
	logsPurged       *metrics.Counter
	objectsCollected *metrics.Counter
	bytesCollected   *metrics.Counter

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
	c.artifactsExpired = r.Counter("agentiik_artifacts_expired_total",
		"References to artifacts past their retain that the artifact purge retired, each lowering its object's count by one.")
	c.runsPurged = r.Counter("agentiik_runs_purged_total",
		"Finished runs past their retention whose envelopes the envelope purge let go of, published and handed alike. The run and its record stay.")
	c.logsPurged = r.Counter("agentiik_logs_purged_total",
		"Task logs past their run's retention deleted from the object store whole. The line count stays.")
	c.objectsCollected = r.Counter("agentiik_objects_collected_total",
		"Objects the collection deleted from the object store: counted by nothing for the grace period, named by no live artifact, and written by no upload under way.")
	c.bytesCollected = r.Counter("agentiik_objects_collected_bytes_total",
		"The bytes of the objects the collection deleted from the object store.")
	// Written at zero from the start, since they carry no label to be first seen with, so that a
	// rate over them is one from the first scrape.
	for _, counter := range []*metrics.Counter{c.artifactsExpired, c.runsPurged, c.logsPurged, c.objectsCollected, c.bytesCollected} {
		counter.Add(0)
	}

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
	r.Gauges(c.readQuotas, metrics.Desc{
		Name:   "agentiik_quota_used",
		Help:   "What a namespace holds against a quota, named by its identifier: tasks in flight for max_concurrent_tasks, runs created in the last 60 minutes for max_runs_per_hour, bytes of live artifacts and uploads for max_artifact_bytes. Over agentiik_quota_limit, how full it is.",
		Labels: []string{"namespace", "quota"},
		FoldBy: "namespace",
	}, metrics.Desc{
		Name:   "agentiik_quota_limit",
		Help:   "The quota a namespace sets, named by its identifier, for each of the three agentiik_quota_used counts that it bounds. A quota the namespace does not set has no series.",
		Labels: []string{"namespace", "quota"},
		FoldBy: "namespace",
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

// purged counts what one pass of the purges removed.
func (c *counted) purged(p purge.Purged) {
	c.artifactsExpired.Add(float64(p.Artifacts))
	c.runsPurged.Add(float64(p.Runs))
	c.logsPurged.Add(float64(p.Logs))
	c.objectsCollected.Add(float64(p.Objects))
	c.bytesCollected.Add(float64(p.Bytes))
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

// readQuotas reads what every namespace holds against the three quotas that count something, and
// the quotas it sets, in namespace order, so that which namespaces a full family keeps is the same
// from one scrape to the next.
func (c *counted) readQuotas(ctx context.Context, g *metrics.Gauges) error {
	l := c.term.Load()
	if l == nil {
		return nil
	}
	var held []db.Consumption
	if err := l.ctl.Fenced(ctx, l.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		held, err = w.Consumption(ctx)
		return err
	}); err != nil {
		return err
	}
	for _, n := range held {
		for _, q := range []struct {
			quota       string
			used, limit int64
		}{
			{"max_concurrent_tasks", n.Tasks, n.MaxConcurrentTasks},
			{"max_runs_per_hour", n.RunsLastHour, n.MaxRunsPerHour},
			{"max_artifact_bytes", n.ArtifactBytes, n.MaxArtifactBytes},
		} {
			g.Set("agentiik_quota_used", float64(q.used), n.Namespace, q.quota)
			if q.limit > 0 {
				g.Set("agentiik_quota_limit", float64(q.limit), n.Namespace, q.quota)
			}
		}
	}
	return nil
}

// serveMetrics answers the metrics on m.Listen until ctx is done, and answers once it is listening:
// an address it cannot listen on refuses the start rather than leaving an installation's monitoring
// to find out that nothing answers.
//
// Over TLS where m holds a certificate, and in plain HTTP, for a network only the monitoring reaches
// or a terminator in front, where it holds none.
func (c *counted) serveMetrics(ctx context.Context, m config.Metrics, log *slog.Logger) (func(), error) {
	raw, err := hex.DecodeString(m.TokenHash)
	if err != nil || len(raw) != sha256.Size {
		return nil, errors.New("the metrics token's hash is not a SHA-256 in hexadecimal")
	}
	var hash [sha256.Size]byte
	copy(hash[:], raw)

	served, err := m.TLS.Server()
	if err != nil {
		return nil, config.Refuse(config.TLSCertFile, err)
	}
	ln, err := net.Listen("tcp", m.Listen)
	if err != nil {
		return nil, fmt.Errorf("the metrics cannot be answered on %s, which %s names: %w", m.Listen, config.MetricsListen, err)
	}
	if served != nil {
		ln = tls.NewListener(ln, served)
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
	log.Info("answering the metrics", "address", ln.Addr().String(), "tls", m.TLS.Served(), "path", metrics.Path)
	return func() {
		server.Close()
		<-done
	}, nil
}
