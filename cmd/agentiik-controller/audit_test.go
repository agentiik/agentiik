package main

import (
	"context"
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
		ended <- lead(ctx, ctl, tm, queue, options(c, queue, versionsOf(t, pool)), export, logger(&log))
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
