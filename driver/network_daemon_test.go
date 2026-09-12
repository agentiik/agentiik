package driver

import (
	"context"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// dial opens a client on a fake daemon, which is a real socket speaking the Engine API,
// so that what a rule sends is read back after a round trip through JSON rather than
// trusted as the struct it was handed.
func dial(t *testing.T, bs ...dockertest.Behaviour) *docker.Client {
	t.Helper()
	daemon, err := dockertest.NewDaemon(bs...)
	if err != nil {
		t.Fatalf("starting a fake daemon: %s", err)
	}
	t.Cleanup(func() { daemon.Close() })

	cli, err := docker.Dial(daemon.Socket())
	if err != nil {
		t.Fatalf("dialing the fake daemon: %s", err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

// "internal attaches a network with no outbound route, for steps that only talk to a
// sidecar service", and "every task gets its own network, so two containers on the same
// host never see each other".
func TestTheInternalPostureGetsANetworkOfItsOwnWithNoWayOut(t *testing.T) {
	cli := dial(t)
	ctx := context.Background()
	task := graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4", "invoice", 2, agk.Shard{Index: 3, Of: 8}),
		Run:       "01JMZ8V1P9C4",
		Namespace: "finance",
		Step:      "invoice",
		Attempt:   2,
		Shard:     agk.Shard{Index: 3, Of: 8},
		Network:   graph.NetworkInternal,
	}

	n, err := networkFor(ctx, cli, task)
	if err != nil {
		t.Fatalf("network: internal: %s", err)
	}
	if n.ID == "" {
		t.Fatalf("nothing was created for a step that asked for a network")
	}
	if n.Mode != networkName(task.ID) {
		t.Fatalf("NetworkMode is %q, and the network is named %q", n.Mode, networkName(task.ID))
	}

	list, err := cli.NetworkList(ctx, nil)
	if err != nil {
		t.Fatalf("listing networks: %s", err)
	}
	if len(list) != 1 {
		t.Fatalf("one task created %d networks", len(list))
	}
	created := list[0]
	if !created.Internal {
		t.Fatalf("the network has a route out, and network: internal is the posture with none")
	}
	if created.Name != networkName(task.ID) {
		t.Fatalf("the network is called %q", created.Name)
	}
	if created.Labels[LabelTask] != string(task.ID) {
		t.Fatalf("the network carries %v, and nothing there says which task it belongs to", created.Labels)
	}

	// Removed with the container, in the defer that runs on every path, or a host
	// accumulates one network per task that ever ran on it.
	if err := removeNetwork(ctx, cli, n); err != nil {
		t.Fatalf("removing the network: %s", err)
	}
	list, err = cli.NetworkList(ctx, nil)
	if err != nil {
		t.Fatalf("listing networks: %s", err)
	}
	if len(list) != 0 {
		t.Fatalf("%d networks survived the task", len(list))
	}
	// A network that is already gone is the outcome asked for: a stop can arrive for
	// a task somebody already cleaned up.
	if err := removeNetwork(ctx, cli, n); err != nil {
		t.Fatalf("removing a network that is already gone: %s", err)
	}
}

// The floor is read off the daemon's own /info, over the wire, and not off a setting
// somebody wrote down about it.
func TestTheFloorIsReadOffTheDaemonItself(t *testing.T) {
	ctx := context.Background()

	hardened := dial(t, dockertest.WithUsernsRemap(165536, 165536))
	info, err := hardened.Info(ctx)
	if err != nil {
		t.Fatalf("info: %s", err)
	}
	floor, err := readUsernsFloor(info, DefaultPolicy())
	if err != nil {
		t.Fatalf("a daemon that remaps was refused: %s", err)
	}
	uid, gid, ok := floor.ownership()
	if !ok || uid != 165536 || gid != 165536 {
		t.Fatalf("the ownership read off the daemon is %d:%d, %v", uid, gid, ok)
	}

	laptop := dial(t, dockertest.WithoutUsernsRemap)
	info, err = laptop.Info(ctx)
	if err != nil {
		t.Fatalf("info: %s", err)
	}
	if _, err := readUsernsFloor(info, DefaultPolicy()); err == nil {
		t.Fatalf("a daemon with no remapping was accepted")
	}
}
