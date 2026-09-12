package driver

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/agentiik/agentiik/internal/dockertest"
)

// What Probe has to answer is what a caller needs before there is a driver: which socket
// answered, what is being spoken, which platform a container runs on natively, and whether
// the daemon remaps. Every one of them is read off the daemon's own /info and none of them is
// a setting somebody wrote down about it.

func TestProbeAnswersWithWhatTheDaemonSaysItIs(t *testing.T) {
	daemon, err := dockertest.NewDaemon(dockertest.WithUsernsRemap(100000, 100000))
	if err != nil {
		t.Fatalf("starting a fake daemon: %s", err)
	}
	defer daemon.Close()

	d, err := Probe(t.Context(), daemon.Socket())
	if err != nil {
		t.Fatalf("probing the fake daemon: %s", err)
	}
	if d.Socket != daemon.Socket() {
		t.Errorf("the socket is %s, want the one that answered, %s", d.Socket, daemon.Socket())
	}
	if d.APIVersion == "" {
		t.Errorf("no API version: it is what is being spoken and not what was compiled in")
	}
	if d.OSType != "linux" || d.Architecture == "" {
		t.Errorf("the platform is %s/%s, and it is what decides which helper may be bound into a container", d.OSType, d.Architecture)
	}
	if !d.UsernsRemapped {
		t.Errorf("the daemon carries name=userns and Probe says it does not remap")
	}
}

func TestProbeReadsADaemonThatDoesNotRemap(t *testing.T) {
	daemon, err := dockertest.NewDaemon()
	if err != nil {
		t.Fatalf("starting a fake daemon: %s", err)
	}
	defer daemon.Close()

	d, err := Probe(t.Context(), daemon.Socket())
	if err != nil {
		t.Fatalf("probing the fake daemon: %s", err)
	}
	if d.UsernsRemapped {
		t.Errorf("the daemon carries no name=userns and Probe says it remaps")
	}
	// Probing is not the floor. The floor is the Policy's and New applies it, so a daemon
	// Probe describes happily is still a daemon New refuses.
	if _, err := New(Config{Socket: daemon.Socket()}); !errors.Is(err, ErrUsernsRemapRequired) {
		t.Errorf("opening a driver on it answered %v, want the floor refusing", err)
	}
}

func TestProbeSaysSoWhereThereIsNoDaemon(t *testing.T) {
	_, err := Probe(t.Context(), filepath.Join(t.TempDir(), "docker.sock"))
	if !errors.Is(err, ErrDaemonUnreachable) {
		t.Errorf("probing a socket that is not there answered %v, want the unreachable sentinel, which is the exit code the command line gives it", err)
	}
}
