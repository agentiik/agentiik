package docker_test

import (
	"context"
	"testing"
	"time"

	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// TestAnOutOfMemoryKillIsOnTheStreamAndNotOnTheWait is the case the event stream exists
// for. The oom arrives before the die and carries the reason, and the wait on this one
// never answers at all, so a driver following only the wait would hang until the
// deadline.
func TestAnOutOfMemoryKillIsOnTheStreamAndNotOnTheWait(t *testing.T) {
	client, _ := dial(t, dockertest.OOMKills, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{"brick": {Digest: digest}},
		Run:    func(dockertest.Container) (int, error) { return 0, nil },
	}))

	events, _ := client.Events(t.Context(), time.Time{}, docker.Filters{}.Add("type", "container"))

	created, err := client.ContainerCreate(t.Context(), "", docker.Config{
		Image: "brick", Labels: map[string]string{"dev.agentiik.task": "01HQ/heavy/1"},
	}, docker.HostConfig{}, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	wait, err := client.ContainerWait(t.Context(), created.ID, docker.WaitNextExit)
	if err != nil {
		t.Fatalf("opening the wait: %v", err)
	}
	if err := client.ContainerStart(t.Context(), created.ID); err != nil {
		t.Fatalf("starting: %v", err)
	}

	var oom, die bool
	var code string
	deadline := time.After(5 * time.Second)
	for !die {
		select {
		case e := <-events:
			if e.Actor.ID != created.ID {
				continue
			}
			switch e.Action {
			case docker.ActionOOM:
				oom = true
			case docker.ActionDie:
				die = true
				code = e.Actor.Attributes["exitCode"]
				if !oom {
					t.Error("the die arrived before the oom, and the oom is the only place the reason is written")
				}
			}
		case w := <-wait:
			t.Fatalf("the wait answered %+v, and this is the exit it never sees", w)
		case <-deadline:
			t.Fatal("no die event arrived")
		}
	}
	if !oom {
		t.Error("the kill was not reported as an out-of-memory one")
	}
	if code != "137" {
		t.Errorf("exitCode: got %q, want 137", code)
	}
}

// TestTheStreamResumesFromTheLastEventSeen holds why the resume carries since: a dropped
// stream replays its gap rather than losing what fell into it. A replay is the
// deliberate direction, because an event seen twice is the same exit recorded twice and
// a die never seen is a task that hangs.
func TestTheStreamResumesFromTheLastEventSeen(t *testing.T) {
	client, _ := dial(t, dockertest.EventStreamDrops, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{"brick": {Digest: digest}},
		Run:    func(dockertest.Container) (int, error) { return 0, nil },
	}))

	events, failures := client.Events(t.Context(), time.Time{}, docker.Filters{}.Add("type", "container"))

	created, err := client.ContainerCreate(t.Context(), "", docker.Config{Image: "brick"}, docker.HostConfig{}, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	wait, err := client.ContainerWait(t.Context(), created.ID, docker.WaitNextExit)
	if err != nil {
		t.Fatalf("opening the wait: %v", err)
	}
	if err := client.ContainerStart(t.Context(), created.ID); err != nil {
		t.Fatalf("starting: %v", err)
	}
	<-wait

	// The stream is closed after every single event, so both the start and the die
	// arrive only if it came back and resumed where it left off.
	var start, die bool
	deadline := time.After(10 * time.Second)
	for !die {
		select {
		case e := <-events:
			switch e.Action {
			case docker.ActionStart:
				start = true
			case docker.ActionDie:
				die = true
			}
		case <-failures:
			// A stream that dropped is reported and not fatal. It comes
			// back.
		case <-deadline:
			t.Fatalf("the stream did not resume: start %v, die %v", start, die)
		}
	}
	if !start {
		t.Error("the event before the drop was lost rather than replayed")
	}
}

// TestTheStreamIsClosedWithItsContext holds that following the daemon costs one
// goroutine that ends when the caller says so.
func TestTheStreamIsClosedWithItsContext(t *testing.T) {
	client, _ := dial(t)

	ctx, cancel := context.WithCancel(t.Context())
	events, failures := client.Events(ctx, time.Time{}, nil)
	cancel()

	deadline := time.After(5 * time.Second)
	for events != nil || failures != nil {
		select {
		case _, open := <-events:
			if !open {
				events = nil
			}
		case _, open := <-failures:
			if !open {
				failures = nil
			}
		case <-deadline:
			t.Fatal("the channels were not closed when the context was")
		}
	}
}
