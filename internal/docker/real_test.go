package docker_test

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// Everything in this file needs a daemon that is actually there. It is skipped where
// there is none, so that a machine with nothing installed still runs the rest, and it runs
// where there is one, so that the fake is held to what the real daemon does rather than to
// what this package believes about it. CI has one and sets AGENTIIK_TEST_REQUIRE_DOCKER,
// so there a test that cannot run fails rather than skips.

// real dials the daemon on this machine, or ends the test through dockertest.Unavailable.
func real(t *testing.T) *docker.Client {
	t.Helper()
	socket, ok := dockertest.Socket()
	if !ok {
		dockertest.Unavailable(t, "no Docker daemon on this machine")
	}
	c, err := docker.Dial(socket)
	if err != nil {
		dockertest.Unavailable(t, "the daemon at %s did not answer: %v", socket, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// TestARealDaemonNegotiatesAVersionWeCanSpeak is the check that made the ceiling a
// negotiation rather than a path prefix: the daemon on the machine this was written on
// answers below it and refuses every path under the version the documentation names.
func TestARealDaemonNegotiatesAVersionWeCanSpeak(t *testing.T) {
	c := real(t)

	spoken := c.APIVersion()
	t.Logf("the daemon at %s is spoken to at API version %s, with a ceiling of %s", c.Socket(), spoken, docker.Ceiling)
	if spoken == "" {
		t.Fatal("no version was negotiated")
	}

	info, err := c.Info(t.Context())
	if err != nil {
		t.Fatalf("asking the daemon what it is: %v", err)
	}
	t.Logf("userns-remap is %v, and the root directory is %s", info.UsernsRemapped(), info.DockerRootDir)
	if info.OSType == "" {
		t.Error("the daemon answered /info with no OSType")
	}
}

// TestARealDaemonRunsAContainerThroughTheWholeSequence is the sequence a task is: create,
// wait before start, attach, start, the envelope on standard input, the two streams apart,
// the exit code, the log, the removal.
//
// It runs against whatever small image is already on this machine and skips where there
// is none, because a test that pulls from a registry is a test that fails on an aeroplane.
func TestARealDaemonRunsAContainerThroughTheWholeSequence(t *testing.T) {
	c := real(t)
	image := localImage(t, c)

	created, err := c.ContainerCreate(t.Context(), "", docker.Config{
		Image:        image,
		Cmd:          []string{"/bin/sh", "-c", "cat; echo on-err 1>&2; exit 3"},
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		OpenStdin:    true,
		StdinOnce:    true,
		Labels:       map[string]string{"dev.agentiik.task": "test/real/1"},
	}, docker.HostConfig{
		NetworkMode:    "none",
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		AutoRemove:     false,
	}, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("creating a container from %s: %v", image, err)
	}
	defer c.ContainerRemove(t.Context(), created.ID, true)

	waited, err := c.ContainerWait(t.Context(), created.ID, docker.WaitNextExit)
	if err != nil {
		t.Fatalf("opening the wait: %v", err)
	}
	select {
	case w := <-waited:
		t.Fatalf("the wait answered %+v before the container was started", w)
	default:
	}

	stream, err := c.ContainerAttach(t.Context(), created.ID, docker.AttachOptions{
		Stdin: true, Stdout: true, Stderr: true, Stream: true,
	})
	if err != nil {
		t.Fatalf("attaching: %v", err)
	}
	defer stream.Close()

	if err := c.ContainerStart(t.Context(), created.ID); err != nil {
		t.Fatalf("starting: %v", err)
	}

	const envelope = `{"meta":{"port":"in"},"items":[]}`
	if _, err := io.WriteString(stream.Stdin, envelope); err != nil {
		t.Fatalf("writing the envelope on standard input: %v", err)
	}
	if err := stream.CloseWrite(); err != nil {
		t.Fatalf("half-closing standard input: %v", err)
	}

	var out, errs strings.Builder
	for {
		frame, err := stream.Next()
		if err != nil {
			if errors.Is(err, io.EOF) || strings.Contains(err.Error(), "use of closed") {
				break
			}
			t.Fatalf("reading a frame: %v", err)
		}
		switch frame.Stream {
		case docker.Stdout:
			out.Write(frame.Bytes)
		case docker.Stderr:
			errs.Write(frame.Bytes)
		}
	}
	if out.String() != envelope {
		t.Errorf("standard output carried %q, and the container echoed what it was given", out.String())
	}
	if strings.TrimSpace(errs.String()) != "on-err" {
		t.Errorf("standard error carried %q", errs.String())
	}

	select {
	case w := <-waited:
		if w.Err != nil {
			t.Fatalf("waiting: %v", w.Err)
		}
		if w.StatusCode != 3 {
			t.Errorf("the exit code is %d, want 3", w.StatusCode)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the wait never answered")
	}

	// The log is taken from the daemon after the exit, which is why AutoRemove is
	// false: the container is still there and so is everything it wrote.
	logs, err := c.ContainerLogs(t.Context(), created.ID, docker.LogOptions{Stdout: true, Stderr: true})
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	defer logs.Close()
	var logged strings.Builder
	for {
		frame, err := logs.Next()
		if err != nil {
			break
		}
		logged.Write(frame.Bytes)
	}
	if !strings.Contains(logged.String(), "on-err") {
		t.Errorf("the daemon's log reads %q, and it holds what the container wrote", logged.String())
	}

	in, err := c.ContainerInspect(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("inspecting: %v", err)
	}
	if in.State.StartedAt.IsZero() || in.State.FinishedAt.IsZero() {
		t.Errorf("the daemon's own moments are %s and %s", in.State.StartedAt, in.State.FinishedAt)
	}
	if in.State.ExitCode != 3 {
		t.Errorf("the inspect reads the exit code as %d", in.State.ExitCode)
	}

	found, err := c.ContainerList(t.Context(), docker.Filters{}.Add("label", "dev.agentiik.task=test/real/1"))
	if err != nil {
		t.Fatalf("listing by label: %v", err)
	}
	if len(found) != 1 || found[0].ID != created.ID {
		t.Errorf("adoption by label found %d containers", len(found))
	}
}

// TestARealDaemonStopsAContainerWithTheGraceItWasGiven holds the escalation where it
// belongs, on the daemon.
func TestARealDaemonStopsAContainerWithTheGraceItWasGiven(t *testing.T) {
	c := real(t)
	image := localImage(t, c)

	created, err := c.ContainerCreate(t.Context(), "", docker.Config{
		Image: image,
		// A shell that takes its term and does not go, which is the container
		// the escalation exists for.
		Cmd: []string{"/bin/sh", "-c", "trap '' TERM; sleep 300"},
	}, docker.HostConfig{NetworkMode: "none"}, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	defer c.ContainerRemove(t.Context(), created.ID, true)

	waited, err := c.ContainerWait(t.Context(), created.ID, docker.WaitNextExit)
	if err != nil {
		t.Fatalf("opening the wait: %v", err)
	}
	if err := c.ContainerStart(t.Context(), created.ID); err != nil {
		t.Fatalf("starting: %v", err)
	}

	started := time.Now()
	if err := c.ContainerStop(t.Context(), created.ID, 2*time.Second); err != nil {
		t.Fatalf("stopping: %v", err)
	}
	select {
	case w := <-waited:
		if w.StatusCode != 137 {
			t.Errorf("the exit code is %d, and a container the daemon killed leaves 137", w.StatusCode)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the container was never stopped")
	}
	if took := time.Since(started); took < time.Second {
		t.Errorf("the stop took %s, and the grace it was given was two seconds", took)
	}
}

// localImage is a small image that is already on this machine, or the end of the test.
// Pulling one would make this a test of somebody's registry, so CI pulls alpine:3.21, the
// image every fixture names, before the tests start.
func localImage(t *testing.T, c *docker.Client) string {
	t.Helper()
	for _, ref := range []string{"alpine:3.21", "alpine:latest", "busybox:latest", "alpine", "busybox", "debian:stable-slim"} {
		if _, err := c.ImageInspect(t.Context(), ref); err == nil {
			return ref
		}
	}
	dockertest.Unavailable(t, "no small image on this machine to run a container from: docker pull alpine:3.21")
	return ""
}
