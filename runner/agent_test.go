package runner

import (
	"context"
	"testing"

	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// agent is an Agent on a fake daemon that remaps, whose Ready counts how often it was said.
func agent(t *testing.T, readies *int) Agent {
	t.Helper()
	daemon, err := dockertest.NewDaemon(dockertest.WithUsernsRemap(165536, 165536))
	if err != nil {
		t.Fatalf("starting a fake daemon: %s", err)
	}
	t.Cleanup(func() { daemon.Close() })
	// The host an installed agent finds: the three capabilities a remapped daemon asks
	// for, and a secrets directory on a tmpfs mounted noexec,nosuid,nodev.
	d, err := driver.New(driver.Config{
		Socket:   daemon.Socket(),
		WorkRoot: t.TempDir(),
		Policy:   driver.Policy{SecretsDir: "/run/agentiik/secrets"},
		Host:     installed{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	client, err := NewClient("http://127.0.0.1:1", credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	return Agent{Driver: d, Client: client, Ready: func() error { *readies++; return nil }}
}

// installed answers as the host of an agent installed as the page says.
type installed struct{}

func (installed) Capabilities() (uint64, error) { return 1<<0 | 1<<1 | 1<<3, nil }

func (installed) Filesystem(string) (driver.Filesystem, error) {
	return driver.Filesystem{Tmpfs: true, NoExec: true, NoSUID: true, NoDev: true}, nil
}

func TestServeSaysReadyOnceAndServesUntilItIsStopped(t *testing.T) {
	var readies int
	a := agent(t, &readies)
	ctx, cancel := context.WithCancel(context.Background())
	a.Ready = func() error {
		readies++
		cancel()
		return nil
	}
	if err := Serve(ctx, a); err != nil {
		t.Fatal(err)
	}
	if readies != 1 {
		t.Errorf("ready was said %d times", readies)
	}
}

func TestAStopThatArrivedWhileStartingIsNotFollowedByReady(t *testing.T) {
	var readies int
	a := agent(t, &readies)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Serve(ctx, a); err != nil {
		t.Fatal(err)
	}
	if readies != 0 {
		t.Errorf("ready was said %d times after the agent was stopped", readies)
	}
}
