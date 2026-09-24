package driver

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

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
	// An internal bridge still gives the host an address in the network, the
	// gateway's, unless it is told to keep out, and that address is the runner host.
	if created.Options[gatewayMode] != gatewayIsolated {
		t.Fatalf("the network was created with the options %v, which leave the runner host in it", created.Options)
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

// internalTaskOn is a task on network: internal, for the tests of its network alone.
func internalTaskOn(step agk.Step) graph.Task {
	return graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4", step, 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4",
		Namespace: "finance",
		Step:      step,
		Attempt:   1,
		Network:   graph.NetworkInternal,
	}
}

// A redelivered task finds the network its first delivery created, and the daemon answers
// its create with a conflict. The task takes that network over, the one with its label,
// rather than failing or making a second.
func TestARedeliveryTakesOverTheNetworkItsFirstDeliveryCreated(t *testing.T) {
	cli := dial(t)
	ctx := context.Background()
	task := internalTaskOn("invoice")

	first, err := networkFor(ctx, cli, task)
	if err != nil {
		t.Fatalf("the first delivery: %s", err)
	}
	again, err := networkFor(ctx, cli, task)
	if err != nil {
		t.Fatalf("the redelivery was refused its own network: %s", err)
	}
	if again != first {
		t.Fatalf("the redelivery has %+v, and the first delivery made %+v", again, first)
	}
	list, err := cli.NetworkList(ctx, nil)
	if err != nil {
		t.Fatalf("listing networks: %s", err)
	}
	if len(list) != 1 {
		t.Fatalf("two deliveries of one task left %d networks", len(list))
	}
	// The identifier taken over is the daemon's own, so the removal at the end of the
	// redelivery removes the network rather than asking for one by a name.
	if err := removeNetwork(ctx, cli, again); err != nil {
		t.Fatalf("removing the network taken over: %s", err)
	}
	if list, _ := cli.NetworkList(ctx, nil); len(list) != 0 {
		t.Fatalf("%d networks survived the redelivery", len(list))
	}
}

// A network already under the task's name that is not the one this driver makes for it is
// not taken over: one with a route out or the host in it, left by a driver that predates the
// isolation, would put the container where the posture says it is not, and one without the
// task's label is somebody else's.
func TestANetworkUnderTheTasksNameThatIsNotItsOwnIsNotTakenOver(t *testing.T) {
	task := internalTaskOn("invoice")
	for _, c := range []struct {
		name string
		spec docker.NetworkSpec
	}{
		{"with the host in it", docker.NetworkSpec{Name: networkName(task.ID), Internal: true, Labels: labels(task)}},
		{"with a route out", docker.NetworkSpec{Name: networkName(task.ID), Options: map[string]string{gatewayMode: gatewayIsolated}, Labels: labels(task)}},
		{"without the task's label", docker.NetworkSpec{Name: networkName(task.ID), Internal: true, Options: map[string]string{gatewayMode: gatewayIsolated}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cli := dial(t)
			ctx := context.Background()
			if _, err := cli.NetworkCreate(ctx, c.spec); err != nil {
				t.Fatalf("leaving a network behind: %s", err)
			}
			n, err := networkFor(ctx, cli, task)
			if err == nil {
				t.Fatalf("the task was put on %+v", n)
			}
			if !strings.Contains(err.Error(), networkName(task.ID)) {
				t.Fatalf("the refusal does not name the network: %s", err)
			}
			if charge, ok := Charged(err); !ok || charge != ChargePlatform {
				t.Fatalf("the refusal is charged to %s, decided %v", charge, ok)
			}
		})
	}
}

// A bridge older than Docker 28.0 ignores the option that keeps the host out, so an
// internal network it made would reach the runner host all the same. network: internal
// is refused on it, before anything is created, and none still runs.
func TestInternalIsRefusedOnADaemonThatCannotKeepTheHostOut(t *testing.T) {
	cli := dial(t, dockertest.APIVersion("1.47"))
	ctx := context.Background()

	n, err := networkFor(ctx, cli, internalTaskOn("invoice"))
	if !errors.Is(err, ErrInternalNotIsolated) {
		t.Fatalf("network: internal on API 1.47: %+v, %v", n, err)
	}
	for _, want := range []string{"invoice", "1.47", "Docker 28.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q: %s", want, err)
		}
	}
	if charge, ok := Charged(err); !ok || charge != ChargePlatform {
		t.Fatalf("the refusal is charged to %s, decided %v", charge, ok)
	}
	if list, _ := cli.NetworkList(ctx, nil); len(list) != 0 {
		t.Fatalf("a refused posture created %d networks", len(list))
	}
	if _, err := networkFor(ctx, cli, graph.Task{Step: "invoice", Network: graph.NetworkNone}); err != nil {
		t.Fatalf("network: none on the same daemon: %s", err)
	}
}

// A network the daemon would not remove is said, naming the step, the network and the
// task, rather than dropped: the task has ended either way, and a network left behind holds
// address space until somebody knows to remove it.
func TestANetworkThatWouldNotBeRemovedIsSaid(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := newRunner(t, oneImage(ref, goodManifest), func(dockertest.Container) (int, error) { return 0, nil })
	r.daemon.Handle(http.MethodDelete, "/networks/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"the bridge is busy"}`))
	})

	task := oneTask(ref)
	task.Network = graph.NetworkInternal
	if _, err := r.Run(t.Context(), task); err != nil {
		t.Fatalf("a network left behind failed the task: %s", err)
	}
	for _, want := range []string{"fetch left the network " + networkName(task.ID), string(task.ID), "the bridge is busy"} {
		if r.said.count(want) != 1 {
			t.Fatalf("nothing said %q: %q", want, r.said.s)
		}
	}
}

// What a runner that died left on the daemon is swept when the next one starts: a task's
// network that no container is on any more. A network a container still carries the task
// of is kept, since a redelivery will adopt that container and it needs its network, and so
// is one of a task this process holds, one another process has only just made, one that is
// not this driver's, and one the daemon refuses to remove, which is said.
func TestTheSweepRemovesTheTaskNetworksNoContainerIsOn(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	busy := make(chan struct{})
	t.Cleanup(func() { close(busy) })
	r := newRunner(t, oneImage(ref, goodManifest), func(dockertest.Container) (int, error) {
		<-busy
		return 0, nil
	})
	ctx := t.Context()

	// Every network here was left an hour ago but one, which another process on the
	// daemon has only just made and is about to create its container on.
	made := func(step agk.Step) graph.Task {
		task := internalTaskOn(step)
		if _, err := networkFor(ctx, r.cli, task); err != nil {
			t.Fatalf("making the network of %s: %s", step, err)
		}
		return task
	}
	left := func(step agk.Step) graph.Task {
		task := made(step)
		r.daemon.Backdate(networkName(task.ID), time.Hour)
		return task
	}
	orphan := left("orphan")
	adoptable := left("adoptable")
	held := left("held")
	inUse := left("in-use")
	fresh := made("fresh")
	for _, spec := range []docker.NetworkSpec{
		{Name: "agk-somebody-elses", Labels: map[string]string{"owner": "somebody"}},
		{Name: "somebody-elses", Labels: map[string]string{LabelTask: "01JMZ8V1P9C4/theirs/1"}},
	} {
		if _, err := r.cli.NetworkCreate(ctx, spec); err != nil {
			t.Fatalf("creating a network of somebody else's: %s", err)
		}
		r.daemon.Backdate(spec.Name, time.Hour)
	}

	// The container a redelivery would adopt, created and never started, so that no
	// endpoint of the daemon's own keeps its network.
	if _, err := r.cli.ContainerCreate(ctx, "", docker.Config{Image: ref, Labels: labels(adoptable)}, docker.HostConfig{NetworkMode: networkName(adoptable.ID)}, docker.NetworkingConfig{}); err != nil {
		t.Fatalf("creating the adoptable container: %s", err)
	}
	// A running container that carries no task label, on a task's network, which the
	// daemon refuses the removal over.
	running, err := r.cli.ContainerCreate(ctx, "", docker.Config{Image: ref}, docker.HostConfig{NetworkMode: networkName(inUse.ID)}, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("creating the running container: %s", err)
	}
	if err := r.cli.ContainerStart(ctx, running.ID); err != nil {
		t.Fatalf("starting it: %s", err)
	}
	if err := r.Hold(held.ID); err != nil {
		t.Fatalf("holding a task: %s", err)
	}
	defer r.Release(held.ID)

	if err := r.Sweep(ctx); err != nil {
		t.Fatalf("sweeping: %s", err)
	}

	list, err := r.cli.NetworkList(ctx, nil)
	if err != nil {
		t.Fatalf("listing networks: %s", err)
	}
	kept := map[string]bool{}
	for _, n := range list {
		kept[n.Name] = true
	}
	if kept[networkName(orphan.ID)] {
		t.Errorf("the network no container is on survived the sweep")
	}
	for _, name := range []string{networkName(adoptable.ID), networkName(held.ID), networkName(inUse.ID), networkName(fresh.ID), "agk-somebody-elses", "somebody-elses"} {
		if !kept[name] {
			t.Errorf("the sweep removed %s", name)
		}
	}
	if r.said.count("an earlier process left task networks on this daemon that no container is on, and they were removed: "+networkName(orphan.ID)) != 1 {
		t.Errorf("the sweep did not say what it removed: %q", r.said.s)
	}
	if r.said.count("the network "+networkName(inUse.ID)+", left on this daemon by task "+string(inUse.ID)+", was not removed") != 1 {
		t.Errorf("the sweep did not say what the daemon refused it: %q", r.said.s)
	}
}

// A daemon that cannot say what networks it has is a sweep that did nothing, and says so
// rather than reading as a clean host.
func TestASweepThatCannotListSaysSo(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := newRunner(t, oneImage(ref, goodManifest), func(dockertest.Container) (int, error) { return 0, nil })
	r.daemon.Handle(http.MethodGet, "/networks", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if err := r.Sweep(t.Context()); err == nil || !strings.Contains(err.Error(), "could not be listed") {
		t.Fatalf("a sweep that could not list anything answered %v", err)
	}
}

// A runner of a build that predates the isolation died with a task's container on a network
// that is internal and keeps the host in it. The redelivery on the new build adopts that
// container, collects it and removes it with its network, rather than refusing the network
// and leaving the container running on it with nothing left to stop it.
func TestARedeliveryAdoptsAContainerOnANetworkThatPredatesTheIsolation(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		return 0, wrote(c, "out", agk.NewItem(map[string]any{"from": "the first delivery"}))
	})
	task := oneTask(ref)
	task.Network = graph.NetworkInternal

	old, err := r.cli.NetworkCreate(t.Context(), docker.NetworkSpec{Name: networkName(task.ID), Driver: networkDriver, Internal: true, Labels: labels(task)})
	if err != nil {
		t.Fatalf("leaving the old network behind: %s", err)
	}
	container, _ := exitedFirstDelivery(t, r, task)

	result, err := r.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("the redelivery was refused: %s", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Errorf("the redelivery reports %s, and the container exited 0", result.State)
	}
	removed := r.daemon.Removed()
	if !slices.Contains(removed, container) {
		t.Errorf("the adopted container was left on the daemon: %v", removed)
	}
	if !slices.Contains(removed, old.ID) {
		t.Errorf("the old network was left on the daemon: %v", removed)
	}
}

// A task's network is made immediately before its container, and not before the pull, so
// that a network with no container on it is never one whose image is still arriving, which
// the sweep of another process on the daemon would take for left. A pull that fails leaves
// the daemon with nothing made and nothing to remove.
func TestTheNetworkIsMadeAfterThePull(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := newRunner(t, nil, func(dockertest.Container) (int, error) { return 0, nil })
	task := oneTask(ref)
	task.Network = graph.NetworkInternal

	if _, err := r.Run(t.Context(), task); err == nil {
		t.Fatalf("a task whose image is nowhere ran")
	}
	if removed := r.daemon.Removed(); len(removed) != 0 {
		t.Fatalf("a task refused at its pull made and removed %v", removed)
	}
	if list, _ := r.cli.NetworkList(t.Context(), nil); len(list) != 0 {
		t.Fatalf("a task refused at its pull left %d networks", len(list))
	}
}

// A key a build that ran network: internal carried on a daemon older than Docker 28.0 comes
// back to a build that refuses the posture there. The refusal takes away the container the
// earlier delivery left, its network and its directory, rather than leaving the container
// running with nothing to stop it.
func TestARefusedInternalTaskTakesAwayWhatAnEarlierDeliveryLeft(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := newRunner(t, oneImage(ref, goodManifest), func(dockertest.Container) (int, error) { return 0, nil }, dockertest.APIVersion("1.47"))
	task := oneTask(ref)
	task.Network = graph.NetworkInternal

	old, err := r.cli.NetworkCreate(t.Context(), docker.NetworkSpec{Name: networkName(task.ID), Driver: networkDriver, Internal: true, Labels: labels(task)})
	if err != nil {
		t.Fatalf("leaving the old network behind: %s", err)
	}
	container, root := stageFirstDelivery(t, r, task)

	if _, err := r.Run(t.Context(), task); !errors.Is(err, ErrInternalNotIsolated) {
		t.Fatalf("the redelivery on API 1.47: %v", err)
	}
	removed := r.daemon.Removed()
	if !slices.Contains(removed, container) || !slices.Contains(removed, old.ID) {
		t.Errorf("the refusal left the container or the network: removed %v", removed)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("the refusal left the working directory %s", root)
	}
}
