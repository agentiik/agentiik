package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/bus/control"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// The controller that leads exports the audit log for as long as its term lasts, and says at every
// start when it exports it nowhere.

// An installation naming no sink starts, and says the log goes nowhere.
func TestAControllerWithNoSinkSaysTheAuditLogGoesNowhere(t *testing.T) {
	var log output
	if exporter(config.AuditExport{}, db.AuditTrail{}, logger(&log)) != nil {
		t.Fatal("an exporter was made with no sink")
	}
	if !strings.Contains(log.String(), config.AuditExportURL) {
		t.Fatalf("a controller exporting nowhere said %q", log.String())
	}
}

// An act recorded while a term lasts reaches the sink, and once the term has ended nothing more is
// sent by it.
func TestATermExportsTheAuditLog(t *testing.T) {
	pool, super := dbtest.Open(t)
	seeded(t, pool, super)
	b := withInstallationBus(t)
	credential := b.controlPlane(t, "agentiik-controller")
	connected, err := bus.Open(t.Context(), bus.Options{URL: b.url, Name: "leading", Credentials: &credential})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(connected.Close)
	queue := control.New(connected)

	var mu sync.Mutex
	var received []string
	arrived := make(chan struct{}, 10)
	sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, strings.TrimSpace(string(body)))
		mu.Unlock()
		arrived <- struct{}{}
	}))
	defer sink.Close()

	ctl, err := controller.New(pool, "leading")
	if err != nil {
		t.Fatal(err)
	}
	ctl.Sweep = time.Hour
	tm, err := pool.BeginTerm(t.Context(), "leading")
	if err != nil {
		t.Fatal(err)
	}
	var log output
	export := exporter(config.AuditExport{URL: sink.URL}, pool.AuditTrail(), logger(&log))
	export.Client, export.Every = sink.Client(), 20*time.Millisecond
	c := config.Controller{Objects: t.TempDir(), MaxRequeues: graph.DefaultMaxRequeues, TaskCeiling: time.Hour}
	ctx, stop := context.WithCancel(t.Context())
	ended := make(chan error, 1)
	go func() {
		ended <- lead(ctx, ctl, tm, queue, options(c, queue, versionsOf(t, pool)), export, nil, logger(&log))
	}()

	record := func(target string) {
		t.Helper()
		if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
			return ns.Audit(ctx, audit.Record{Actor: "admin", Action: audit.RunCancel, Target: target, Result: audit.Done})
		}); err != nil {
			t.Fatal(err)
		}
	}
	record("the first run")
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatalf("an act recorded during a term never reached the sink:\n%s", log.String())
	}

	stop()
	select {
	case <-ended:
	case <-time.After(20 * time.Second):
		t.Fatal("the term did not end")
	}
	record("the second run")
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || !strings.Contains(received[0], "the first run") {
		t.Fatalf("the sink received %q, and the term that ended sends nothing more", received)
	}
}

// auditTerms is an installation whose terms are led one after the other, each verifying the audit
// log's chain as the program's do, and saying what it found in log.
type auditTerms struct {
	pool  *db.Pool
	super string
	bus   installationBus
	queue *control.Queue
	log   output
}

func withAuditTerms(t *testing.T) *auditTerms {
	t.Helper()
	pool, super := dbtest.Open(t)
	seeded(t, pool, super)
	b := withInstallationBus(t)
	credential := b.controlPlane(t, "agentiik-controller")
	connected, err := bus.Open(t.Context(), bus.Options{URL: b.url, Name: "leading", Credentials: &credential})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(connected.Close)
	return &auditTerms{pool: pool, super: super, bus: b, queue: control.New(connected)}
}

// record appends n acts to the audit log.
func (a *auditTerms) record(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		if err := a.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
			return ns.Audit(ctx, audit.Record{Actor: "admin", Action: audit.RunCancel, Target: fmt.Sprintf("run-%d", i), Result: audit.Done})
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// tamper runs stmt as a superuser with the audit log's triggers off.
func (a *auditTerms) tamper(t *testing.T, stmt string) {
	t.Helper()
	tx, err := dbtest.Superuser(t, a.super).Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(t.Context()))
	for _, s := range []string{`set local session_replication_role = replica`, stmt} {
		if _, err := tx.Exec(t.Context(), s); err != nil {
			t.Fatalf("%s: %s", s, err)
		}
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// lead begins a term held by name, as a failover does, and leads it until the verification has said
// what it found, which it answers. The term is still going then, taking results, and ends when the
// answer has been read.
func (a *auditTerms) lead(t *testing.T, name string) string {
	t.Helper()
	ctl, err := controller.New(a.pool, name)
	if err != nil {
		t.Fatal(err)
	}
	ctl.Sweep = time.Hour
	tm, err := a.pool.BeginTerm(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	a.log = output{}
	c := config.Controller{Objects: t.TempDir(), MaxRequeues: graph.DefaultMaxRequeues, TaskCeiling: time.Hour}
	ctx, stop := context.WithCancel(t.Context())
	ended := make(chan error, 1)
	go func() {
		ended <- lead(ctx, ctl, tm, a.queue, options(c, a.queue, versionsOf(t, a.pool)), nil, verifier(a.pool.AuditTrail(), 2, logger(&a.log)), logger(&a.log))
	}()
	defer func() {
		stop()
		select {
		case <-ended:
		case <-time.After(20 * time.Second):
			t.Fatal("the term did not end")
		}
	}()
	js := a.bus.streams(t)
	eventually(t, 10*time.Second, "the verification saying what it found while the term takes results", func() bool {
		if !strings.Contains(a.log.String(), "audit log's chain") {
			return false
		}
		_, err := js.Consumer(t.Context(), bus.Results, "controller")
		return err == nil
	})
	select {
	case err := <-ended:
		t.Fatalf("the term ended with %v once the chain was verified", err)
	default:
	}
	return a.log.String()
}

// A chain broken in the database is said at the start of the term, naming the first broken entry,
// and the term goes on.
func TestATermWarnsOfABrokenAuditChainAndGoesOn(t *testing.T) {
	a := withAuditTerms(t)
	a.record(t, 5)
	a.tamper(t, `update audit_log set actor = 'somebody else' where seq = 3`)
	said := a.lead(t, "leading")
	if !strings.Contains(said, "level=WARN") || !strings.Contains(said, "chain in the database is broken") || !strings.Contains(said, "entry=3") {
		t.Fatalf("a term beginning on a chain broken at entry 3 said:\n%s", said)
	}
}

// Each term carries on from how far the one before it verified the chain, whichever controller led
// it, and checks the entry recorded before carrying on from it.
func TestAFailoverVerifiesFromTheRecordedEntry(t *testing.T) {
	a := withAuditTerms(t)
	a.record(t, 5)
	if said := a.lead(t, "first"); !strings.Contains(said, "chain holds") || !strings.Contains(said, "from=0 through=5") {
		t.Fatalf("the first term said:\n%s", said)
	}
	a.record(t, 3)
	if said := a.lead(t, "second"); !strings.Contains(said, "chain holds") || !strings.Contains(said, "from=5 through=8") {
		t.Fatalf("the term after a failover said:\n%s", said)
	}
	a.tamper(t, `update audit_log set detail = '{"changed":true}' where seq = 8`)
	a.record(t, 1)
	if said := a.lead(t, "third"); !strings.Contains(said, "chain in the database is broken") || !strings.Contains(said, "entry=8") {
		t.Fatalf("a term carrying on from entry 8, changed since it was verified, said:\n%s", said)
	}
}

// A record of how far the chain was verified that does not agree with a chain that holds is said
// apart from a break, naming the entry recorded, and the term after it carries on from the head.
func TestATermSaysARecordThatDisagreesWithTheChain(t *testing.T) {
	a := withAuditTerms(t)
	a.record(t, 5)
	a.lead(t, "first")
	if _, err := dbtest.Superuser(t, a.super).Exec(t.Context(), `update audit_verified set through = 6`); err != nil {
		t.Fatal(err)
	}
	a.record(t, 2)
	if said := a.lead(t, "second"); !strings.Contains(said, "level=WARN") || !strings.Contains(said, "does not agree") || !strings.Contains(said, "entry=6") || strings.Contains(said, "is broken") {
		t.Fatalf("a term beginning on a record that disagrees with the chain said:\n%s", said)
	}
	if said := a.lead(t, "third"); !strings.Contains(said, "chain holds") || !strings.Contains(said, "from=7 through=7") {
		t.Fatalf("the term after it said:\n%s", said)
	}
}

// A verification still reading holds nothing up: the term takes results meanwhile, and waits for it
// only when it ends.
func TestATermTakesResultsWhileTheChainIsVerified(t *testing.T) {
	a := withAuditTerms(t)
	ctl, err := controller.New(a.pool, "leading")
	if err != nil {
		t.Fatal(err)
	}
	ctl.Sweep = time.Hour
	tm, err := a.pool.BeginTerm(t.Context(), "leading")
	if err != nil {
		t.Fatal(err)
	}
	reading, release := make(chan struct{}), make(chan struct{})
	var returned sync.WaitGroup
	returned.Add(1)
	verify := func(ctx context.Context) {
		defer returned.Done()
		close(reading)
		<-release
	}
	c := config.Controller{Objects: t.TempDir(), MaxRequeues: graph.DefaultMaxRequeues, TaskCeiling: time.Hour}
	ctx, stop := context.WithCancel(t.Context())
	ended := make(chan error, 1)
	go func() {
		ended <- lead(ctx, ctl, tm, a.queue, options(c, a.queue, versionsOf(t, a.pool)), nil, verify, logger(&a.log))
	}()
	<-reading
	js := a.bus.streams(t)
	eventually(t, 10*time.Second, "the term taking results while the chain is verified", func() bool {
		_, err := js.Consumer(t.Context(), bus.Results, "controller")
		return err == nil
	})
	stop()
	select {
	case err := <-ended:
		t.Fatalf("the term ended with %v while its verification was still reading", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-ended:
	case <-time.After(20 * time.Second):
		t.Fatal("the term did not end once its verification had")
	}
	returned.Wait()
}
