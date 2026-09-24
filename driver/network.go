package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
)

// networkModeNone is the NetworkMode of a container with no network at all, which is the
// default of the settings table and the default of the language.
const networkModeNone = "none"

// networkDriver is what a task's own network is made with. bridge is the driver every
// daemon has, and an internal bridge is a network with no route out, which is exactly
// what network: internal asks for.
const networkDriver = "bridge"

// networkPrefix marks a network as this driver's, so that a person reading docker
// network ls on a runner can tell what made it and a sweep can remove what an earlier
// process left behind.
const networkPrefix = "agk-"

// gatewayMode is the bridge option that keeps the host out of a task's network, and
// isolated is the one value of it that does. An internal bridge has no route out, but the
// daemon still gives the bridge itself an address in the network, the gateway's, and that
// address is the runner host: a container on it reaches every service the host listens
// on, the daemon's own included where it listens on TCP. isolated assigns the bridge no
// address, so there is nothing of the host's in the network to reach.
const (
	gatewayMode     = "com.docker.network.bridge.gateway_mode_ipv4"
	gatewayIsolated = "isolated"
)

// isolatedSince is the Engine API version of Docker 28.0, the first release whose bridge
// knows gatewayMode=isolated. An older bridge ignores an option it does not know, so the
// network it made would be called isolated and reach the host all the same.
const isolatedSince = "1.48"

// ErrInternalNotIsolated is the refusal of network: internal on a daemon too old to keep
// the host out of the task's network. It is a sentinel for the reason
// ErrEgressProxyMissing is one: the operator's answer is to upgrade the daemon, and the
// workflow is not wrong.
var ErrInternalNotIsolated = errors.New("network: internal attaches a network with no route out of it and no address of the runner host in it, and a daemon older than Docker 28.0 cannot make a bridge that leaves the host's address out. network: none runs here; upgrading the daemon runs network: internal")

// ErrEgressProxyMissing is the refusal of network: egress. It is a sentinel so that a
// caller can tell this refusal from every other one with errors.Is, which is what lets
// an operator's tooling say "not yet" rather than "your workflow is wrong". Its sentence
// is the rule, as the settings table states it, and then what is missing.
var ErrEgressProxyMissing = errors.New("network: egress attaches a dedicated network whose outbound traffic passes through a runner proxy enforcing the egress.allow list, and this runner has no such proxy yet. network: none and network: internal run here; the proxy is v0.9.0 work")

// network is the network one task runs on: the NetworkMode its container is created
// with, and the network created for it where there is one.
//
// ID is empty for the none posture. There is nothing to remove afterwards, because there
// was nothing to create: no network at all is stronger than a private one, and it is
// what the settings table asks for by default.
type network struct {
	Mode string
	ID   string
}

// networkFor gives the task the network its posture asks for.
//
// Every task gets a network of its own whatever the posture, "so two containers on the
// same host never see each other". none is the absence of one, which no other container
// can be on either; internal is a bridge of this task's own with no outbound route, for a
// step that only talks to a sidecar service.
//
// egress is refused. The posture promises that "a proxy on the runner enforces the list",
// and there is no proxy on this runner yet. Opening the network and calling it filtered
// would let a workflow believe its egress.allow list is being enforced when nothing is
// enforcing it, and the one control the documentation says "actually bites" against a
// step exfiltrating a secret it was legitimately given is that list. So the step is
// refused before anything is created, and the refusal says why.
func networkFor(ctx context.Context, cli *docker.Client, t graph.Task) (network, error) {
	switch t.Network {
	case graph.NetworkNone:
		return network{Mode: networkModeNone}, nil

	case graph.NetworkEgress:
		return network{}, fault(t.Step, ErrEgressProxyMissing, ChargePlatform, "network: egress: %s", allowList(t.EgressAllow))

	case graph.NetworkInternal:
		if !cli.Speaks(isolatedSince) {
			return network{}, fault(t.Step, ErrInternalNotIsolated, ChargePlatform, "network: internal: the daemon speaks API version %s, and %s is Docker 28.0", cli.APIVersion(), isolatedSince)
		}
		name := networkName(t.ID)
		created, err := cli.NetworkCreate(ctx, docker.NetworkSpec{
			Name:   name,
			Driver: networkDriver,
			// Internal is the whole of the posture: a bridge with no route
			// out of the host, which is what "a network with no outbound
			// route, for steps that only talk to a sidecar service" is.
			Internal: true,
			// Attachable, so that a sidecar the runner starts beside the task
			// can join the same network. Nothing does yet, and a network that
			// refused it would have to be recreated when something does.
			Attachable: true,
			// No IPv6, said rather than left to the daemon's default, which a
			// daemon.json may have turned on for every network.
			EnableIPv6: false,
			Options:    map[string]string{gatewayMode: gatewayIsolated},
			Labels:     labels(t),
		})
		if err != nil {
			if docker.IsConflict(err) {
				return adoptNetwork(ctx, cli, t, name)
			}
			return network{}, fault(t.Step, err, ChargePlatform, "network: internal: the task's own network %s could not be created", name)
		}
		return network{Mode: name, ID: created.ID}, nil

	default:
		return network{}, fault(t.Step, nil, ChargeBrick, "network: %s: a network posture is none, egress or internal, and there is no posture that puts a container on the host's", t.Network)
	}
}

// adoptNetwork takes over the network a create found already there under the task's name.
//
// A redelivered task finds the network its first delivery created, since the name is
// derived from the task identifier and never minted. It is taken over only where it is
// that network: this task's label, internal, and isolated from the host. A network that
// is any less, left by a driver that predates the isolation or made by somebody else
// under the name, would put the container where the posture says it is not, so it is
// refused rather than used, and the sweep at the runner's next start removes it once no
// container is on it.
func adoptNetwork(ctx context.Context, cli *docker.Client, t graph.Task, name string) (network, error) {
	found, err := cli.NetworkList(ctx, docker.Filters{}.Add("label", LabelTask+"="+string(t.ID)))
	if err != nil {
		return network{}, fault(t.Step, err, ChargePlatform, "network: internal: the task's own network %s exists and could not be looked up", name)
	}
	for _, n := range found {
		if n.Name != name {
			continue
		}
		if !n.Internal || n.Options[gatewayMode] != gatewayIsolated {
			return network{}, fault(t.Step, ErrInternalNotIsolated, ChargePlatform, "network: internal: a network named %s is already on this daemon and has a route out or an address of the host in it, so it is not taken over", name)
		}
		return network{Mode: name, ID: n.ID}, nil
	}
	return network{}, fault(t.Step, nil, ChargePlatform, "network: internal: a network named %s is already on this daemon and does not carry this task's label, so it is not taken over", name)
}

// allowList says what the refused step asked to reach, because the operator reading the
// refusal is the person who has to decide what to do about it, and "nothing enforces
// your list" is easier to act on when the list is in front of them.
func allowList(allow []string) string {
	switch len(allow) {
	case 0:
		return "the step names no egress.allow entry, and a posture that opens the network to everything is not one this driver will start"
	case 1:
		return fmt.Sprintf("the step's egress.allow names %s, and nothing on this runner would hold the container to it", allow[0])
	default:
		return fmt.Sprintf("the step's egress.allow names %s, and nothing on this runner would hold the container to them", strings.Join(allow, ", "))
	}
}

// removeNetwork takes the task's network away, which is the other half of every task
// getting one of its own: a host that kept them would run out of address space after a
// few thousand tasks.
//
// A network that is already gone is the outcome asked for, and so is one with nothing to
// remove, which is every task on the none posture.
func removeNetwork(ctx context.Context, cli *docker.Client, n network) error {
	if n.ID == "" {
		return nil
	}
	if err := cli.NetworkRemove(ctx, n.ID); err != nil && !docker.IsNotFound(err) {
		return err
	}
	return nil
}

// Sweep removes the task networks an earlier process left on the daemon: a runner that
// died between creating a network and removing it leaves one behind for every task it had
// in flight, and a host that kept them would run out of address space as surely as one
// that never removed any. A runner calls it once, when it starts and before it takes any
// work.
//
// A network is this driver's by its task label and its name, and it is removed only where
// no container carries its task's label, in any state, and this process holds no task of
// that name: a container a later delivery will adopt still needs the network it was
// created on, and one created and not yet started holds no endpoint the daemon would
// refuse the removal over. A network the daemon still refuses to remove is said and left.
// What was removed is said too, since each one is a task that ended without its runner.
//
// It answers an error only where the daemon could not be asked what it has.
func (d *Docker) Sweep(ctx context.Context) error {
	networks, err := d.cli.NetworkList(ctx, docker.Filters{}.Add("label", LabelTask))
	if err != nil {
		return fmt.Errorf("driver: the task networks an earlier process left could not be listed: %w", err)
	}
	containers, err := d.cli.ContainerList(ctx, docker.Filters{}.Add("label", LabelTask))
	if err != nil {
		return fmt.Errorf("driver: the containers on the task networks an earlier process left could not be listed: %w", err)
	}
	used := map[string]bool{}
	for _, c := range containers {
		used[c.Labels[LabelTask]] = true
	}

	var removed []string
	for _, n := range networks {
		task := n.Labels[LabelTask]
		if !strings.HasPrefix(n.Name, networkPrefix) || used[task] || d.lookup(agk.TaskID(task)) != nil {
			continue
		}
		if err := d.cli.NetworkRemove(ctx, n.ID); err != nil && !docker.IsNotFound(err) {
			d.say(fmt.Sprintf("the network %s, left on this daemon by task %s, was not removed, so it holds its share of the daemon's address pools until somebody removes it: %v", n.Name, task, err))
			continue
		}
		removed = append(removed, n.Name)
	}
	if len(removed) > 0 {
		d.say("an earlier process left task networks on this daemon that no container is on, and they were removed: " + strings.Join(removed, ", "))
	}
	return nil
}

// networkName is what a task's own network is called: derived from the task identifier
// and never minted, so that the same task arriving twice names the same network and the
// second delivery adopts the first one's rather than creating a second.
//
// A task identifier carries the separators of a path, which a network name may not, so
// the name is the identifier with them replaced. A name that would be too long for the
// daemon is cut and given the first bytes of the identifier's digest, which keeps it
// derived rather than minted while keeping two long task identifiers apart.
func networkName(id agk.TaskID) string {
	name := networkPrefix + safeName(string(id))
	const max = 60
	if len(name) > max {
		sum := sha256.Sum256([]byte(id))
		name = name[:max-9] + "-" + hex.EncodeToString(sum[:4])
	}
	return name
}

// safeName writes an identifier the way a daemon object may be named: letters, digits
// and the three punctuation marks a name accepts, with everything else becoming an
// underscore.
func safeName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
