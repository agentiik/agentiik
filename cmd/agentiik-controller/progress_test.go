package main

import (
	"errors"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/bus/control"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
)

// A term takes a task's progress off the bus as it takes results, through the core, and a progress
// message the fence refuses ends the term as a refused answer does: the term has passed, and the
// message is the new holder's.
func TestATermEndsAtTheFirstProgressTheFenceRefuses(t *testing.T) {
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
	c := config.Controller{Objects: t.TempDir(), MaxRequeues: graph.DefaultMaxRequeues, TaskCeiling: time.Hour}
	o := options(c, queue, versionsOf(t, pool))
	ended := make(chan error, 1)
	go func() { ended <- lead(t.Context(), ctl, tm, queue, o, logger(&log)) }()

	js := b.streams(t)
	eventually(t, 10*time.Second, "the term taking results", func() bool {
		_, err := js.Consumer(t.Context(), bus.Results, "controller")
		return err == nil
	})
	time.Sleep(time.Second)

	if _, err := pool.BeginTerm(t.Context(), "usurper"); err != nil {
		t.Fatal(err)
	}
	if err := connected.Progress(t.Context(), bus.TaskProgress{
		TaskID:         ulid.New(),
		IdempotencyKey: string(agk.NewTaskID(agk.NewRunID(), "normalize", 1, agk.Shard{})),
		Runner:         "runner-1",
		Progress:       agk.TaskRunning,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, db.ErrFenced) {
			t.Errorf("the term ended with %v, and the fence refused its progress", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the term was still going 20s after the fence refused its progress:\n%s", log.String())
	}
}
