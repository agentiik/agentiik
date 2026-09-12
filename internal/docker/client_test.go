package docker_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// dial starts a fake daemon and opens a client on it, both closed when the test ends.
func dial(t *testing.T, bs ...dockertest.Behaviour) (*docker.Client, *dockertest.Daemon) {
	t.Helper()
	d, err := dockertest.NewDaemon(bs...)
	if err != nil {
		t.Fatalf("starting the fake daemon: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	c, err := docker.Dial(d.Socket())
	if err != nil {
		t.Fatalf("dialing %s: %v", d.Socket(), err)
	}
	t.Cleanup(func() { c.Close() })
	return c, d
}

// TestTheVersionIsNegotiatedAndNotPinned holds the correction this package was written
// around: the ceiling is what the documentation names, and the version spoken is what
// the daemon offers when that is lower. A client with the ceiling compiled into its path
// prefix refuses to run on the machine this was written on.
func TestTheVersionIsNegotiatedAndNotPinned(t *testing.T) {
	for _, c := range []struct{ offers, speaks string }{
		{offers: "1.41", speaks: "1.41"},
		{offers: "1.55", speaks: "1.55"},
		{offers: docker.Ceiling, speaks: docker.Ceiling},
		{offers: "1.99", speaks: docker.Ceiling},
	} {
		t.Run(c.offers, func(t *testing.T) {
			client, _ := dial(t, dockertest.APIVersion(c.offers))
			if got := client.APIVersion(); got != c.speaks {
				t.Errorf("a daemon offering %s is spoken to at %s, and this spoke %s", c.offers, c.speaks, got)
			}
		})
	}
}

// TestADaemonBelowTheFloorIsRefusedNamingBothVersions holds the shape of the refusal.
// Naming one version leaves the reader to work out which end is wrong.
func TestADaemonBelowTheFloorIsRefusedNamingBothVersions(t *testing.T) {
	d, err := dockertest.NewDaemon(dockertest.APIVersion("1.24"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	_, err = docker.Dial(d.Socket())
	if err == nil {
		t.Fatal("a daemon below the floor was accepted")
	}
	if !strings.Contains(err.Error(), "1.24") || !strings.Contains(err.Error(), docker.Floor) {
		t.Errorf("the refusal is %q, and it names neither the version offered nor the floor", err)
	}
}

// TestADaemonThatIsNotThereIsUnreachable holds the distinction a driver needs before it
// charges a failure to anybody: a daemon that is not there failed nobody's step.
func TestADaemonThatIsNotThereIsUnreachable(t *testing.T) {
	_, err := docker.Dial(filepath.Join(t.TempDir(), "nothing.sock"))
	if err == nil {
		t.Fatal("dialing a socket that is not there succeeded")
	}
	if !docker.IsUnreachable(err) {
		t.Errorf("%v does not read as a daemon that could not be reached", err)
	}
}

// TestARefusalIsReadByItsStatusAndNotByItsProse holds what Error is for. The Engine API
// reports every refusal the same way, and a caller branches on the status.
func TestARefusalIsReadByItsStatusAndNotByItsProse(t *testing.T) {
	client, _ := dial(t)

	_, err := client.ContainerInspect(t.Context(), "0123456789ab")
	if err == nil {
		t.Fatal("inspecting a container that does not exist succeeded")
	}
	if !docker.IsNotFound(err) {
		t.Errorf("%v does not read as a 404", err)
	}
	var refused *docker.Error
	if !errors.As(err, &refused) {
		t.Fatalf("%v is not one of the daemon's refusals", err)
	}
	if refused.Status != 404 {
		t.Errorf("status: got %d, want 404", refused.Status)
	}
	if refused.Message == "" {
		t.Error("the refusal carries no message, and the daemon sent one")
	}
}

// TestADaemonThatVanishesMidTaskIsNotAFailedBrick holds the case a restart under a
// running task produces: no status, no message, nothing.
func TestADaemonThatVanishesMidTaskIsNotAFailedBrick(t *testing.T) {
	client, _ := dial(t, dockertest.DaemonVanishes, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{"brick": {Digest: digest}},
	}))

	created, err := client.ContainerCreate(t.Context(), "", docker.Config{Image: "brick"}, docker.HostConfig{}, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	if err := client.ContainerStart(t.Context(), created.ID); err == nil {
		t.Fatal("starting a container on a daemon that vanished succeeded")
	}
	if _, err := client.ContainerInspect(t.Context(), created.ID); err == nil {
		t.Fatal("inspecting on a daemon that vanished succeeded")
	}
}

// TestTheSocketIsFoundWhereADaemonActuallyIs holds the three places DefaultSocket looks,
// through the one of them a test can set.
func TestTheSocketIsFoundWhereADaemonActuallyIs(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///run/somewhere/docker.sock")
	if got := docker.DefaultSocket(); got != "unix:///run/somewhere/docker.sock" {
		t.Errorf("DOCKER_HOST is consulted first: got %q", got)
	}

	t.Setenv("DOCKER_HOST", "")
	if got := docker.DefaultSocket(); got == "" {
		t.Error("with nothing set there is still a path to try")
	}
}

// TestAnUnknownTransportIsRefusedByName holds that this package speaks one transport and
// says so, rather than failing to connect over another.
func TestAnUnknownTransportIsRefusedByName(t *testing.T) {
	_, err := docker.Dial("tcp://127.0.0.1:2375")
	if err == nil {
		t.Fatal("a TCP daemon was accepted")
	}
	if !strings.Contains(err.Error(), "unix socket") {
		t.Errorf("the refusal is %q, and it does not say which transport is spoken", err)
	}
}

// digest is the one an image resolves to throughout these tests. It is a constant so
// that a test asserting the manifest cache is keyed by digest has one to assert against.
const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// ctxOf is a context that ends with the test, for the calls that outlive a request.
func ctxOf(t *testing.T) context.Context {
	t.Helper()
	return t.Context()
}
