package driver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
)

// The default posture asks the daemon for nothing at all, which is what the nil client
// here proves: no network is stronger than a private one, and there is nothing to remove
// afterwards.
func TestNoNetworkIsTheDefaultAndCreatesNothing(t *testing.T) {
	n, err := networkFor(context.Background(), nil, graph.Task{Step: "invoice", Network: graph.NetworkNone})
	if err != nil {
		t.Fatalf("network: none: %s", err)
	}
	if n.Mode != networkModeNone {
		t.Fatalf("NetworkMode is %q, want none", n.Mode)
	}
	if n.ID != "" {
		t.Fatalf("something was created for a task with no network: %q", n.ID)
	}
	if err := removeNetwork(context.Background(), nil, n); err != nil {
		t.Fatalf("removing nothing: %s", err)
	}
}

// network: egress is refused, before anything is created, and the refusal says why: the
// proxy that would enforce egress.allow does not exist yet, and "the only control that
// actually bites is egress". Opening the network and calling it filtered would let a
// workflow believe its list is being enforced when nothing is enforcing it.
func TestEgressIsRefusedUntilTheProxyExists(t *testing.T) {
	task := graph.Task{Step: "invoice", Network: graph.NetworkEgress, EgressAllow: []string{"api.billing.example.com:443"}}

	// The nil client is the assertion that nothing was created: a refusal that had
	// talked to the daemon first would panic here rather than pass.
	n, err := networkFor(context.Background(), nil, task)
	if err == nil {
		t.Fatalf("network: egress was started, on network %q", n.Mode)
	}
	if !errors.Is(err, ErrEgressProxyMissing) {
		t.Fatalf("the refusal is not the one a caller can recognise: %s", err)
	}
	for _, want := range []string{"invoice", "egress", "api.billing.example.com:443", "none", "internal"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q: %s", want, err)
		}
	}
	if n.Mode != "" || n.ID != "" {
		t.Fatalf("a refused task came back with a network: %+v", n)
	}
	// A posture the runner cannot honour yet is this side's gap and not the
	// workflow's mistake, so it is charged where the exit code table charges what is
	// not the brick's.
	if charge, ok := Charged(err); !ok || charge != ChargePlatform {
		t.Fatalf("the refusal is charged to %s, decided %v", charge, ok)
	}
}

// A step that asks for egress and names nothing to allow is refused too. There is no
// reading of the posture under which the network is simply opened.
func TestEgressWithNoAllowListIsRefusedAsWell(t *testing.T) {
	_, err := networkFor(context.Background(), nil, graph.Task{Step: "invoice", Network: graph.NetworkEgress})
	if !errors.Is(err, ErrEgressProxyMissing) {
		t.Fatalf("network: egress with an empty allow list: %v", err)
	}
}

// The name is derived from the task identifier and never minted, so that a redelivered
// task names the network it already created rather than creating a second one.
func TestTheNetworkNameIsDerivedFromTheTask(t *testing.T) {
	id := agk.NewTaskID("01JMZ8V1P9C4", "invoice", 2, agk.Shard{Index: 3, Of: 8})
	name := networkName(id)
	if name != networkName(id) {
		t.Fatalf("two readings of one task gave two names")
	}
	if !strings.HasPrefix(name, networkPrefix) {
		t.Fatalf("%q does not say what made it", name)
	}
	if strings.ContainsAny(name, "/ ") {
		t.Fatalf("%q carries a separator a network name may not", name)
	}
	if strings.Contains(name, "invoice") == false {
		t.Fatalf("%q says nothing about the task it belongs to", name)
	}
}

// A task identifier long enough to overrun a name is cut and digested rather than
// truncated, so that two long identifiers do not collide on one network.
func TestALongTaskIdentifierStillNamesOneNetworkEach(t *testing.T) {
	long := strings.Repeat("a", 60)
	first := networkName(agk.TaskID("01JMZ8V1P9C4/" + long + "1/1"))
	second := networkName(agk.TaskID("01JMZ8V1P9C4/" + long + "2/1"))
	if len(first) > 63 {
		t.Fatalf("%q is %d characters", first, len(first))
	}
	if first == second {
		t.Fatalf("two tasks share the network %q", first)
	}
}

// A posture that is none of the three is refused in the words the language uses for it,
// rather than defaulting to one of them.
func TestAPostureThatIsNotOneOfTheThreeIsRefused(t *testing.T) {
	_, err := networkFor(context.Background(), nil, graph.Task{Step: "invoice", Network: graph.Network(9)})
	if err == nil {
		t.Fatalf("a posture that does not exist was accepted")
	}
	if !strings.Contains(err.Error(), "none, egress or internal") {
		t.Fatalf("the refusal does not name the three: %s", err)
	}
}
