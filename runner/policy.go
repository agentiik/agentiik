package runner

import (
	"fmt"
	"runtime"
	"slices"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/driver"
)

// The host's part of the runner policy, which the loop applies before it writes a key down.
//
// "A runner's policy is written in three places, each for what only it knows." The pool's part,
// accepted namespaces and ceilings, the controller applies at dispatch and the API at redemption;
// runner.toml's, capabilities and ulimits among them, the driver applies to every container it
// creates. What is left is this host's own: the namespaces AGK_RUNNER_NAMESPACES narrows it to, and
// its declared capacity. A message either leaves out is "put back on the queue for another runner or
// a later pull", before anything is written down or redeemed, so it binds nothing and reads no secret.

// Room is an amount of this host a task declares, in the units the daemon limits a container in:
// memory in bytes and CPU in billionths of a core. As a capacity, a part at zero bounds nothing.
type Room struct {
	Memory   int64
	NanoCPUs int64
}

// HostRoom is the capacity this host declares, measured as join measures it: the memory the kernel
// reports at meminfo and the processors this process may run on. It is measured again when the agent
// starts rather than kept from join, since it is the same machine and both change only with it.
func HostRoom(meminfo string) (Room, error) {
	memory, err := memTotal(meminfo)
	if err != nil {
		return Room{}, err
	}
	return Room{Memory: memory, NanoCPUs: int64(runtime.NumCPU()) * 1e9}, nil
}

// accepts says whether this host takes work of a namespace: every namespace its pool accepts where
// AGK_RUNNER_NAMESPACES names none, and only those it names where it does.
func (l *Loop) accepts(namespace string) bool {
	return len(l.Namespaces) == 0 || slices.Contains(l.Namespaces, namespace)
}

// reserve counts what a task declares against the host's capacity with what the loop already holds,
// and answers what it counted and whether it fits. A task that fits is counted until unreserve; one
// that does not is counted nowhere, and why says whether it would fit on this host once what it
// holds is done.
//
// A resources block the grammar refuses counts as nothing, so that the task is held and fails on the
// brick's account as the driver says, rather than going round the pool for ever.
func (l *Loop) reserve(m bus.TaskMessage) (need Room, fits bool, why string) {
	memory, nanos, err := driver.Declared(agk.Step(m.Step), m.Resources.Memory, m.Resources.CPU)
	if err != nil {
		return Room{}, true, ""
	}
	need = Room{Memory: memory, NanoCPUs: nanos}
	l.mu.Lock()
	defer l.mu.Unlock()
	over := func(want, using, capacity int64) bool { return capacity > 0 && want > capacity-using }
	switch {
	case over(need.Memory, 0, l.Capacity.Memory), over(need.NanoCPUs, 0, l.Capacity.NanoCPUs):
		return need, false, fmt.Sprintf("it declares %s, more than this host declares at all, %s", need, l.Capacity)
	case over(need.Memory, l.using.Memory, l.Capacity.Memory), over(need.NanoCPUs, l.using.NanoCPUs, l.Capacity.NanoCPUs):
		return need, false, fmt.Sprintf("it declares %s, and with the %s its other tasks declare this host would pass its declared %s", need, l.using, l.Capacity)
	}
	l.using.Memory += need.Memory
	l.using.NanoCPUs += need.NanoCPUs
	return need, true, ""
}

// unreserve gives back what reserve counted for a task the loop no longer holds.
func (l *Loop) unreserve(need Room) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.using.Memory -= need.Memory
	l.using.NanoCPUs -= need.NanoCPUs
}

// String writes an amount as a step writes its resources, memory in bytes and cpu in cores.
func (r Room) String() string {
	return fmt.Sprintf("memory %d bytes and cpu %g", r.Memory, float64(r.NanoCPUs)/1e9)
}
