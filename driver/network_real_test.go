package driver

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// The tests here ask the kernel what network: internal gives a container, on a real daemon,
// because every one of its promises is about what a packet can do and not about what a
// create body says. Each probe checks that the tools it needs are in the image first and
// exits 3 naming the one that is not, for the reason noWayOut does: a command the image does
// not have fails exactly like a connection that was refused.

// taskLogs is a log sink per task that can be read while the task is still running, which
// is how a test waits for a container to say it is ready before starting the one beside it.
type taskLogs struct {
	mu   sync.Mutex
	logs map[agk.TaskID]*strings.Builder
}

func (l *taskLogs) OpenLog(_ context.Context, id agk.TaskID) (io.WriteCloser, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.logs == nil {
		l.logs = map[agk.TaskID]*strings.Builder{}
	}
	if l.logs[id] == nil {
		l.logs[id] = &strings.Builder{}
	}
	return lockedLog{l, l.logs[id]}, nil
}

// of is what one task has written so far.
func (l *taskLogs) of(id agk.TaskID) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.logs[id]; b != nil {
		return b.String()
	}
	return ""
}

type lockedLog struct {
	l *taskLogs
	b *strings.Builder
}

func (w lockedLog) Write(p []byte) (int, error) {
	w.l.mu.Lock()
	defer w.l.mu.Unlock()
	return w.b.Write(p)
}

func (lockedLog) Close() error { return nil }

// internalTask is one task of the probe run on network: internal.
func internalTask(step agk.Step, image string, script ...string) graph.Task {
	return graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4", step, 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4",
		Namespace: "finance",
		Step:      step,
		Attempt:   1,
		Image:     image,
		Outputs:   []agk.Port{"out"},
		Network:   graph.NetworkInternal,
		Script:    script,
	}
}

// needs is the first line of every probe here: the tools it runs, each one there.
func needs(tools ...string) string {
	return `for tool in ` + strings.Join(tools, " ") + `; do command -v "$tool" >/dev/null || { echo "NO $tool IN THE IMAGE" >&2; exit 3; }; done`
}

// ownAddress sets SELF to the container's IPv4 address with its prefix, on the interface
// that is not the loopback, and exits 4 where there is none.
const ownAddress = `SELF=$(ip -o -4 addr show | awk '$2 != "lo" {print $4; exit}'); test -n "$SELF" || { echo "NO ADDRESS" >&2; exit 4; }`

// succeeded ends a test whose probe could not run, which is not a network that held.
func succeeded(t *testing.T, r graph.Result, log string) {
	t.Helper()
	if r.State != agk.TaskSucceeded {
		t.Fatalf("the probe ended %s with exit %d: %s", r.State, r.ExitCode, log)
	}
}

// "Every task gets its own network": on internal, the container is attached to the network
// made for its task and to nothing else, has an address there, and has no default route,
// which is what "no outbound route" is to a kernel.
func TestARealContainerOnTheInternalPostureIsOnItsOwnNetworkAndNoOther(t *testing.T) {
	d, image := realDriver(t)
	logs := &taskLogs{}
	d.cfg.Logs = logs

	task := internalTask("attached", image,
		needs("ip", "awk"),
		ownAddress,
		`echo "ADDRESS $SELF" >&2`,
		`ip route | sed 's/^/ROUTE /' >&2`,
		// Held open long enough for the test to ask the daemon what the container
		// is on, and ended by a stop once it has.
		`sleep 60`,
	)

	ended := make(chan graph.Result, 1)
	go func() {
		r, err := d.Run(t.Context(), task)
		if err != nil {
			t.Errorf("running: %s", err)
		}
		ended <- r
	}()

	attached := runningOn(t, d, task.ID)
	if err := d.Stop(t.Context(), graph.Stop{Task: task.ID, Reason: graph.StopSuperseded}); err != nil {
		t.Fatalf("stopping the probe: %s", err)
	}
	r := <-ended
	log := logs.of(task.ID)
	t.Logf("the container said:\n%s", log)
	if r.State != agk.TaskCancelled {
		t.Fatalf("the probe ended %s with exit %d, where the stop should have ended it: %s", r.State, r.ExitCode, log)
	}

	if len(attached) != 1 {
		t.Fatalf("the container is on %d networks: %v", len(attached), attached)
	}
	endpoint, ok := attached[networkName(task.ID)]
	if !ok {
		t.Fatalf("the container is on %v and not on its task's network %s", attached, networkName(task.ID))
	}
	if endpoint.IPAddress == "" {
		t.Fatalf("the container has no address on its task's network, which is a container on none by another name")
	}
	if !strings.Contains(log, "ADDRESS "+endpoint.IPAddress+"/") {
		t.Errorf("the kernel gives the container another address than the daemon says, %s: %s", endpoint.IPAddress, log)
	}
	if strings.Contains(log, "ROUTE default") {
		t.Errorf("a container on network: internal has a default route: %s", log)
	}
	if !strings.Contains(log, "ROUTE ") {
		t.Errorf("the container listed no route at all, so nothing here says what it had: %s", log)
	}
}

// "Every task gets its own network, so two containers on the same host never see each
// other." Two tasks on internal at once: the first listens on its own address and proves
// it can be reached there, the second tries that address and the port, and gets nothing.
func TestTwoRealTasksOnTheInternalPostureCannotReachEachOther(t *testing.T) {
	d, image := realDriver(t)
	logs := &taskLogs{}
	d.cfg.Logs = logs

	listener := internalTask("listener", image,
		needs("ip", "awk", "nc"),
		ownAddress,
		`(while true; do echo HELLO | nc -l -p 7070 >/dev/null 2>&1; done) &`,
		// The listener is proven reachable on its own address, over the interface
		// and not the loopback, before the other task is started. A listener that
		// never came up would make the other task's silence mean nothing.
		`for i in $(seq 100); do if nc -w 1 "${SELF%/*}" 7070 </dev/null 2>/dev/null | grep -q HELLO; then echo "LISTENING ON ${SELF%/*}" >&2; break; fi; sleep 0.1; done`,
		`sleep 60`,
	)

	ended := make(chan graph.Result, 1)
	go func() {
		r, err := d.Run(t.Context(), listener)
		if err != nil {
			t.Errorf("running the listener: %s", err)
		}
		ended <- r
	}()
	defer func() {
		d.Stop(context.WithoutCancel(t.Context()), graph.Stop{Task: listener.ID, Reason: graph.StopSuperseded})
		<-ended
	}()

	// Whatever network the listener is on, which the test above holds to be its own:
	// asking for it by name here would stop this test before the probe on a driver that
	// put both tasks on one network, which is the failure it is here to show.
	var address string
	for _, endpoint := range runningOn(t, d, listener.ID) {
		address = endpoint.IPAddress
	}
	if address == "" {
		t.Fatalf("the listener has no address on any network")
	}
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(logs.of(listener.ID), "LISTENING ON "+address) {
		if time.Now().After(deadline) {
			t.Fatalf("the listener never answered on its own address %s, so the other task's silence would prove nothing: %s", address, logs.of(listener.ID))
		}
		time.Sleep(100 * time.Millisecond)
	}

	prober := internalTask("prober", image,
		needs("nc", "grep"),
		fmt.Sprintf(`if nc -w 3 %s 7070 </dev/null | grep -q HELLO; then echo "REACHED THE OTHER TASK" >&2; fi`, address),
		fmt.Sprintf(`if nc -w 3 -z %s 7070; then echo "REACHED THE OTHER TASK" >&2; fi`, address),
		`exit 0`,
	)
	r, err := d.Run(t.Context(), prober)
	if err != nil {
		t.Fatalf("running the prober: %s", err)
	}
	log := logs.of(prober.ID)
	t.Logf("the prober said:\n%s", log)
	succeeded(t, r, log)
	if strings.Contains(log, "REACHED THE OTHER TASK") {
		t.Errorf("a task on network: internal reached another task's container at %s: %s", address, log)
	}
}

// An internal bridge has no route out of the host, but unless it is told otherwise the
// daemon gives the bridge an address in the network, and that address is the runner host
// itself. A container reaching it reaches every service the host listens on.
//
// The probe asks the first address of its subnet, which is where a daemon puts the
// gateway, on a port where nothing listens. A host that holds the address answers with a
// refusal, and a refusal is a reply: the host is there. One that does not hold it answers
// nothing. The probe asks its own address the same question first, which has to be
// refused, so that silence from the gateway is known to be silence rather than a tool
// that never says refused. On a bridge that takes no address the daemon gives the first
// one to the first container, and a container that holds it has proved the host does not.
func TestARealContainerOnTheInternalPostureCannotReachTheRunnerHost(t *testing.T) {
	d, image := realDriver(t)
	logs := &taskLogs{}
	d.cfg.Logs = logs

	task := internalTask("walled-in", image,
		needs("ip", "awk", "ipcalc", "wget", "grep"),
		ownAddress,
		`NET=$(ipcalc -n "$SELF" | cut -d= -f2)`,
		`GATEWAY=${NET%.*}.$((${NET##*.} + 1))`,
		`echo "SELF ${SELF%/*} GATEWAY $GATEWAY" >&2`,
		`if wget -q -T 3 -O /dev/null "http://${SELF%/*}:1/" 2>/tmp/self || grep -qi refused /tmp/self; then echo "THE PROBE HEARS A REFUSAL" >&2; fi`,
		// A container given the gateway's address itself is on a network where the
		// host holds none, and its own refusal is not the host's.
		`test "$GATEWAY" = "${SELF%/*}" && { echo "THE CONTAINER HOLDS THE FIRST ADDRESS" >&2; exit 0; }`,
		`if wget -q -T 3 -O /dev/null "http://$GATEWAY:1/" 2>/tmp/gateway || grep -qi refused /tmp/gateway; then echo "REACHED THE HOST" >&2; fi`,
		`sed 's/^/GATEWAY SAID /' /tmp/gateway >&2`,
		`exit 0`,
	)

	r, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running: %s", err)
	}
	log := logs.of(task.ID)
	t.Logf("the container said:\n%s", log)
	succeeded(t, r, log)
	if !strings.Contains(log, "THE PROBE HEARS A REFUSAL") {
		t.Fatalf("the probe's own address did not refuse it, so the gateway's silence would prove nothing: %s", log)
	}
	if strings.Contains(log, "REACHED THE HOST") {
		t.Errorf("a container on network: internal reached the runner host through its network's gateway: %s", log)
	}
}

// A container on internal resolves names through the daemon's embedded resolver, which
// runs on the host and could carry a query off it for the container: a name is a message,
// and a resolver that forwards one is a way out. The daemon forwards nothing for a
// container whose every network is internal, and the probe holds it to that: it asks for a
// name that only a resolver outside the host could answer, and gets no address for it.
//
// Two things have to hold first, or the silence proves nothing. The machine running the
// test resolves the name, so a resolver that forwarded it would have had an answer to give;
// and the embedded resolver answers the container's own name, so the probe is reading a
// resolver that is there and would say RESOLVED of an answer.
func TestANameResolvedOnTheInternalPostureDoesNotLeaveTheHost(t *testing.T) {
	d, image := realDriver(t)
	logs := &taskLogs{}
	d.cfg.Logs = logs

	if _, err := net.DefaultResolver.LookupHost(t.Context(), "example.com"); err != nil {
		dockertest.Unavailable(t, "this machine resolves no name outside it, so a container that cannot either proves nothing: %v", err)
	}

	resolved := func(name, as string) string {
		return `nslookup ` + name + ` >/tmp/answer 2>&1 || true; sed 's/^/ANSWER /' /tmp/answer >&2; if sed -n '/^Name:/,$p' /tmp/answer | grep -q '^Address'; then echo "RESOLVED ` + as + `" >&2; fi`
	}
	task := internalTask("resolver", image,
		needs("nslookup", "grep", "sed", "hostname"),
		`sed 's/^/RESOLV /' /etc/resolv.conf >&2`,
		// Script steps run under set -e, and a lookup that fails is the outcome
		// hoped for rather than the end of the probe.
		resolved(`"$(hostname)"`, "ITS OWN NAME"),
		resolved("example.com", "example.com"),
		`exit 0`,
	)

	r, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running: %s", err)
	}
	log := logs.of(task.ID)
	t.Logf("the container said:\n%s", log)
	succeeded(t, r, log)
	if !strings.Contains(log, "RESOLVED ITS OWN NAME") {
		t.Fatalf("the embedded resolver did not answer the container's own name, so its silence on another proves nothing: %s", log)
	}
	if strings.Contains(log, "RESOLVED example.com") {
		t.Errorf("a name asked on network: internal was resolved, so the query left the host: %s", log)
	}
}

// runningOn waits for a task's container to be running, and answers with the networks the
// daemon attached it to.
func runningOn(t *testing.T, d *Docker, id agk.TaskID) map[string]docker.Endpoint {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		found, err := d.containerOf(t.Context(), id)
		if err != nil {
			t.Fatalf("looking for the container of %s: %s", id, err)
		}
		if found != "" {
			in, err := d.cli.ContainerInspect(t.Context(), found)
			if err == nil && in.State.Running && in.NetworkSettings != nil {
				return in.NetworkSettings.Networks
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the container of %s never ran", id)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
