package runner

import (
	"context"
	"testing"

	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// agent is an Agent on a fake daemon that remaps, whose Ready counts how often it was said, and
// whose API answers every heartbeat and nothing else.
func agent(t *testing.T, readies *int) Agent {
	t.Helper()
	return agentOf(t, readies, newBeats(t, answered).srv.URL, t.TempDir())
}

// agentOf is an agent whose API is at url, serving under the work root root.
func agentOf(t *testing.T, readies *int, url, root string) Agent {
	t.Helper()
	daemon, err := dockertest.NewDaemon(dockertest.WithUsernsRemap(165536, 165536))
	if err != nil {
		t.Fatalf("starting a fake daemon: %s", err)
	}
	t.Cleanup(func() { daemon.Close() })
	// The host an installed agent finds: the three capabilities a remapped daemon asks
	// for, and a secrets directory on a tmpfs mounted noexec,nosuid,nodev.
	endings := &Endings{}
	d, err := driver.New(driver.Config{
		Socket:   daemon.Socket(),
		WorkRoot: root,
		Policy:   driver.Policy{SecretsDir: "/run/agentiik/secrets"},
		Observer: endings,
		Host:     installed{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	client, err := NewClient(url, credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	return Agent{
		Config: Config{Runner: "runner-dmz-02", Pool: "dmz", Concurrency: 2, WorkDir: root},
		Driver: d, Client: client, Endings: endings, Ready: func() error { *readies++; return nil },
	}
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
