package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/bus/control"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/purge"
)

// The purges as the program runs them: by the controller that leads, for as long as its term
// lasts, said in one line when a pass removed something and counted in the metrics.

// expiredArtifact records an artifact of a run of finance, already past its retain, and answers
// how to ask whether it is still live.
func expiredArtifact(t *testing.T, pool *db.Pool, super string) func() bool {
	t.Helper()
	run := started(t, pool, goodCommit)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		_, err := ns.WriteArtifact(ctx, db.Reference{
			URI:    agk.URI{Run: run, Step: "archive", Port: "out", Name: "gone.bin"},
			Digest: strings.Repeat("e", 64), Size: 5, For: time.Hour,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `update artifacts set expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	return func() bool {
		var live int
		if err := conn.QueryRow(t.Context(), `select count(*) from artifacts where status = 'live'`).Scan(&live); err != nil {
			t.Fatal(err)
		}
		return live > 0
	}
}

// inTerm begins a term on pool with a bus of its own, and answers what lead is given for it.
func inTerm(t *testing.T, pool *db.Pool) (*controller.Controller, db.Term, *control.Queue) {
	t.Helper()
	b := withInstallationBus(t)
	credential := b.controlPlane(t, "agentiik-controller")
	connected, err := bus.Open(t.Context(), bus.Options{URL: b.url, Name: "leading", Credentials: &credential})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(connected.Close)
	ctl, err := controller.New(pool, "leading")
	if err != nil {
		t.Fatal(err)
	}
	ctl.Sweep = time.Hour
	tm, err := pool.BeginTerm(t.Context(), "leading")
	if err != nil {
		t.Fatal(err)
	}
	return ctl, tm, control.New(connected)
}

// A term purges from its start: an artifact past its retain is retired, the pass says so in one
// line, and the metrics count it.
func TestATermPurgesWhatHasRunOut(t *testing.T) {
	pool, super := dbtest.Open(t)
	seeded(t, pool, super)
	live := expiredArtifact(t, pool, super)
	ctl, tm, queue := inTerm(t, pool)

	var log output
	counts := newCounted(nil, logger(io.Discard))
	c := config.Controller{Objects: t.TempDir(), MaxRequeues: graph.DefaultMaxRequeues, TaskCeiling: time.Hour}
	ctx, stop := context.WithCancel(t.Context())
	ended := make(chan error, 1)
	go func() {
		ended <- lead(ctx, ctl, tm, queue, options(c, queue, versionsOf(t, pool)), nil, nil,
			purger(pool, c.Objects, ctl, tm, counts, logger(&log)), logger(&log))
	}()

	eventually(t, 20*time.Second, "the term retiring the artifact past its retain", func() bool {
		return !live() && strings.Contains(log.String(), "the purges removed what had run out")
	})
	stop()
	select {
	case <-ended:
	case <-time.After(20 * time.Second):
		t.Fatal("the term did not end")
	}
	if said := log.String(); !strings.Contains(said, "artifacts=1") {
		t.Errorf("the pass said:\n%s", said)
	}
	var out bytes.Buffer
	if err := counts.registry.WriteTo(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if !has("\n"+out.String(), "agentiik_artifacts_expired_total 1") {
		t.Errorf("the metrics say:\n%s", out.String())
	}
}

// A controller whose term another has taken purges nothing: the purger it leads with asks the fence
// before every call.
func TestAControllerThatNoLongerLeadsPurgesNothing(t *testing.T) {
	pool, super := dbtest.Open(t)
	seeded(t, pool, super)
	live := expiredArtifact(t, pool, super)
	ctl, err := controller.New(pool, "former")
	if err != nil {
		t.Fatal(err)
	}
	tm, err := pool.BeginTerm(t.Context(), "former")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.BeginTerm(t.Context(), "current"); err != nil {
		t.Fatal(err)
	}
	var log output
	purged, err := purger(pool, t.TempDir(), ctl, tm, nil, logger(&log)).Pass(t.Context())
	if !errors.Is(err, db.ErrFenced) || purged.Removed() || !live() {
		t.Errorf("a former leader's pass removed %+v and said %v", purged, err)
	}
}

// What a pass removed is counted under the controller's families, and a pass that removed nothing
// counts nothing: each is written from the start, at zero.
func TestWhatThePurgesRemovedIsCounted(t *testing.T) {
	c := newCounted(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var out bytes.Buffer
	if err := c.registry.WriteTo(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if !has("\n"+out.String(), "agentiik_objects_collected_total 0") || !has("\n"+out.String(), "agentiik_orphans_found_total 0") {
		t.Errorf("a controller that has purged nothing says:\n%s", out.String())
	}
	c.purged(purge.Purged{Artifacts: 3, Runs: 2, Logs: 4, Uploads: 7, Orphans: 2, Objects: 5, Bytes: 4096})
	c.purged(purge.Purged{})
	c.purged(purge.Purged{Artifacts: 1, Orphans: 1, Objects: 1, Bytes: 1024})
	out.Reset()
	if err := c.registry.WriteTo(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`agentiik_artifacts_expired_total 4`,
		`agentiik_runs_purged_total 2`,
		`agentiik_logs_purged_total 4`,
		`agentiik_orphans_found_total 3`,
		`agentiik_objects_collected_total 6`,
		`agentiik_objects_collected_bytes_total 5120`,
	} {
		if !has("\n"+out.String(), line) {
			t.Errorf("no line %s in\n%s", line, out.String())
		}
	}
}

// A term does not end with a pass still under way: the pass is waited for, and stops at its next
// call.
func TestATermEndsOnlyOnceItsPassHasStopped(t *testing.T) {
	pool, super := dbtest.Open(t)
	seeded(t, pool, super)
	ctl, tm, queue := inTerm(t, pool)

	entered, release := make(chan struct{}), make(chan struct{})
	var once, letGo sync.Once
	free := func() { letGo.Do(func() { close(release) }) }
	defer free()
	p := &purge.Purger{Pool: pool, Objects: artifact.Dir(t.TempDir()), Leading: func(context.Context) error {
		once.Do(func() {
			close(entered)
			<-release
		})
		return nil
	}}
	c := config.Controller{Objects: t.TempDir(), MaxRequeues: graph.DefaultMaxRequeues, TaskCeiling: time.Hour}
	ctx, stop := context.WithCancel(t.Context())
	ended := make(chan error, 1)
	go func() {
		ended <- lead(ctx, ctl, tm, queue, options(c, queue, versionsOf(t, pool)), nil, nil, p, logger(io.Discard))
	}()
	<-entered
	stop()
	select {
	case <-ended:
		t.Fatal("the term ended with a pass under way")
	case <-time.After(300 * time.Millisecond):
	}
	free()
	select {
	case <-ended:
	case <-time.After(20 * time.Second):
		t.Fatal("the term did not end once its pass had stopped")
	}
}

// A pass says what it removed in one line, and says nothing where it removed nothing retention
// decides: forgetting writes that have lapsed, which most passes do, is not worth a line.
func TestAPassSaysWhatItRemovedAndOnlyThat(t *testing.T) {
	var log output
	p := purger(nil, t.TempDir(), nil, db.Term{}, newCounted(nil, logger(io.Discard)), logger(&log))
	p.Passed(purge.Purged{})
	p.Passed(purge.Purged{Uploads: 12})
	if said := log.String(); said != "" {
		t.Errorf("passes that removed nothing retention decides said:\n%s", said)
	}
	p.Passed(purge.Purged{Objects: 2, Bytes: 10, Uploads: 1})
	if said := log.String(); strings.Count(said, "the purges removed what had run out") != 1 || !strings.Contains(said, "objects=2 bytes=10") {
		t.Errorf("a pass that collected two objects said:\n%s", said)
	}
	var recorded output
	p = purger(nil, t.TempDir(), nil, db.Term{}, newCounted(nil, logger(io.Discard)), logger(&recorded))
	p.Passed(purge.Purged{Recorded: 3})
	if said := recorded.String(); strings.Contains(said, "removed") || !strings.Contains(said, "left unrecorded") || !strings.Contains(said, "runs=3") {
		t.Errorf("a pass that recorded the files of three runs and removed nothing said:\n%s", said)
	}
}
