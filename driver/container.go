package driver

import (
	"math"
	"strconv"
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
)

// The labels every container and every network of this driver carries. Reverse DNS is
// the Docker convention for a label namespace and agentiik.dev is the project's domain,
// which is also what the manifest's own apiVersion is written under.
//
// LabelTask is the one that matters: it carries the task identifier, which is the
// idempotency key, so a redelivered task finds the container it already started instead
// of starting a second one, a Stop reaches a container this process did not start, and a
// startup sweep can tell a container of this driver's from everything else on the host.
// The rest are there to be read by a person looking at docker ps on a runner.
const (
	LabelTask      = "dev.agentiik.task"
	LabelRun       = "dev.agentiik.run"
	LabelNamespace = "dev.agentiik.namespace"
	LabelStep      = "dev.agentiik.step"
	LabelAttempt   = "dev.agentiik.attempt"
	LabelShard     = "dev.agentiik.shard"
)

// labels is the task's identity, written where the daemon keeps it.
//
// A shard that does not exist carries no label, on the reading AGK_SHARD already takes:
// an absent value says there was no fan-out, and an empty one would say there was one
// whose index nobody wrote.
func labels(t graph.Task) map[string]string {
	l := map[string]string{
		LabelTask:      string(t.ID),
		LabelRun:       string(t.Run),
		LabelNamespace: t.Namespace,
		LabelStep:      string(t.Step),
		LabelAttempt:   strconv.Itoa(t.Attempt),
	}
	if s := t.Shard.String(); s != "" {
		l[LabelShard] = s
	}
	for name, value := range l {
		if value == "" {
			delete(l, name)
		}
	}
	return l
}

// containerConfig is the container half of the create: what the image is, what it runs,
// what it is given and what it is labelled with.
//
// The standard input flags are set together and always. A container created without
// OpenStdin has no standard input to attach to, and the envelope on standard input is
// half of what a brick is given; StdinOnce closes it after the one writer, which is what
// makes a brick reading to the end of its input see an end.
//
// WorkingDir is deliberately not set. The image declares one and a brick's entry point
// is written against it, so choosing one here would change where a brick already works
// out of; what this driver promises is the paths under /agk, which are absolute.
func containerConfig(t graph.Task, image, user string, env, entrypoint, cmd []string) docker.Config {
	return docker.Config{
		Image:      image,
		Entrypoint: entrypoint,
		Cmd:        cmd,
		Env:        env,
		// The account is the image's, required by the manifest and checked at
		// publication. It is passed rather than assumed, so that a manifest
		// declaring root is refused where manifests are read and not here.
		User:   user,
		Labels: labels(t),

		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		OpenStdin:    true,
		StdinOnce:    true,
		Tty:          false,
	}
}

// hostConfig is the settings table of the documentation, built from the documented
// fields of HostConfig and of the Resources struct it embeds.
//
// Every row of that table is here and in the same order, so the page and this function
// can be read side by side. What is not here is not set, and what this product refuses,
// Privileged and the daemon socket among them, has no spelling in the struct to be set
// with.
func hostConfig(t graph.Task, p Policy, g *given, networkMode string) (docker.HostConfig, error) {
	h := docker.HostConfig{
		// NetworkMode: none by default, and a network of the task's own for the
		// postures that have one. network.go decides which; this writes it.
		NetworkMode: networkMode,

		Mounts: g.Mounts,
		Tmpfs:  g.Tmpfs,

		// ReadonlyRootfs: true. The only writable paths are the two the mounts
		// gave it, /agk/out and /tmp.
		ReadonlyRootfs: true,

		// CapDrop: ALL. CapAdd is refused unless the runner policy allows it
		// explicitly, and nothing a workflow or a manifest can write asks for a
		// capability, so the list stays empty: the policy is a door that is shut
		// rather than one that is guarded.
		CapDrop: []string{"ALL"},

		SecurityOpt: securityOptions(p),

		// AutoRemove: false. The driver removes the container itself once the logs
		// and the exit code are collected, so that nothing is lost on a fast exit.
		AutoRemove: false,
	}

	r, err := resources(t, p)
	if err != nil {
		return docker.HostConfig{}, err
	}
	h.Resources = r
	return h, nil
}

// securityOptions is the SecurityOpt row: "no-new-privileges, the default seccomp
// profile, and an AppArmor profile or SELinux label depending on the host".
//
// no-new-privileges is unconditional. The other three are named only where the policy
// names one: a daemon applies its own default seccomp and AppArmor profiles to every
// container, and passing an empty profile name would replace a default that is already
// the right answer with nothing at all.
func securityOptions(p Policy) []string {
	opt := []string{"no-new-privileges:true"}
	if p.Seccomp != "" {
		opt = append(opt, "seccomp="+p.Seccomp)
	}
	if p.AppArmor != "" {
		opt = append(opt, "apparmor="+p.AppArmor)
	}
	if p.SELinuxLabel != "" {
		opt = append(opt, "label="+p.SELinuxLabel)
	}
	return opt
}

// resources is what the step asked for, "capped by the runner policy and the namespace
// quota". The quota is applied where quotas live and never here; this is the runner's
// half, and where the policy names a ceiling it is also the value a step that asked for
// nothing gets, because a ceiling that only applied to steps that named a number would
// not be a ceiling.
func resources(t graph.Task, p Policy) (docker.Resources, error) {
	var r docker.Resources

	memory, err := memoryBytes(t.Step, t.Resources.Memory)
	if err != nil {
		return docker.Resources{}, err
	}
	r.Memory = capped(memory, p.MemoryCap)

	cpu, err := nanoCPUs(t.Step, t.Resources.CPU)
	if err != nil {
		return docker.Resources{}, err
	}
	r.NanoCPUs = capped(cpu, nanoCPUsOf(p.CPUCap))

	// PidsLimit: "from resources.pids, default 256". It is a pointer on the wire
	// because the daemon reads zero as unlimited, and unlimited is the one answer
	// this row never gives.
	pids := int64(t.Resources.PIDs)
	if pids <= 0 {
		pids = p.PidsLimit
	}
	if pids > 0 {
		pids = capped(pids, p.PidsLimit)
		r.PidsLimit = &pids
	}

	r.Ulimits = ulimits(p)
	return r, nil
}

// capped applies a ceiling. A ceiling of zero is no ceiling, and a request of zero takes
// the ceiling, which is how a policy that names a number governs a step that names none.
func capped(want, ceiling int64) int64 {
	if ceiling <= 0 {
		return want
	}
	if want <= 0 || want > ceiling {
		return ceiling
	}
	return want
}

// ulimits is the Ulimits row, "nofile and nproc set by the runner policy". A limit the
// policy leaves at zero is not sent, so that a policy nobody filled in leaves the
// daemon's own defaults alone rather than setting every limit to nothing.
func ulimits(p Policy) []docker.Ulimit {
	var out []docker.Ulimit
	for _, u := range []struct {
		name string
		docker.Ulimit
	}{
		{name: "nofile", Ulimit: docker.Ulimit{Soft: p.Ulimits.NoFile.Soft, Hard: p.Ulimits.NoFile.Hard}},
		{name: "nproc", Ulimit: docker.Ulimit{Soft: p.Ulimits.NProc.Soft, Hard: p.Ulimits.NProc.Hard}},
	} {
		if u.Soft <= 0 && u.Hard <= 0 {
			continue
		}
		out = append(out, docker.Ulimit{Name: u.name, Soft: u.Soft, Hard: u.Hard})
	}
	return out
}

// memoryBytes reads resources.memory, which the language writes "exactly as the brick
// manifest writes them, quotation marks included".
//
// The suffix is binary and mandatory, which is the rule both the manifest and the
// workflow file are held to, "so that 512Mi cannot be read as 512 bytes". A value that
// got past both and arrives malformed here is refused rather than read as a number of
// bytes, because reading 512M as 512 bytes is how a step gets a limit a thousand times
// smaller than it asked for.
func memoryBytes(step agk.Step, v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	suffixes := []struct {
		text string
		unit int64
	}{
		{"Ki", 1 << 10},
		{"Mi", 1 << 20},
		{"Gi", 1 << 30},
		{"Ti", 1 << 40},
	}
	for _, s := range suffixes {
		digits, ok := strings.CutSuffix(v, s.text)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(digits, 10, 64)
		if err != nil || n <= 0 {
			break
		}
		if n > (1<<62)/s.unit {
			return 0, fault(step, nil, ChargeBrick, "resources.memory is %q, which is more memory than a host has", v)
		}
		return n * s.unit, nil
	}
	return 0, fault(step, nil, ChargeBrick, "resources.memory is %q: memory is a whole number above zero with a binary suffix, Ki, Mi, Gi or Ti, so that 512Mi cannot be read as 512 bytes", v)
}

// nanoCPUs reads resources.cpu, which is written as text "so that half a core reads as
// 0.5 wherever it travels", and turns it into the unit NanoCpus carries.
func nanoCPUs(step agk.Step, v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	cores, err := strconv.ParseFloat(v, 64)
	if err != nil || cores <= 0 {
		return 0, fault(step, nil, ChargeBrick, "resources.cpu is %q: cpu is written exactly as the brick manifest writes it, quotation marks included, so that half a core reads as 0.5 wherever it travels", v)
	}
	return nanoCPUsOf(cores), nil
}

// nanoCPUsOf converts a number of cores to the billionths NanoCpus is counted in.
func nanoCPUsOf(cores float64) int64 {
	if cores <= 0 {
		return 0
	}
	// Rounded rather than truncated: 0.1 is not a binary fraction, and truncating
	// the product would give a step a nanosecond less of a core than it asked for
	// every time the nearest float64 falls below the number that was written.
	return int64(math.Round(cores * 1e9))
}

// networkingConfig attaches the container to the one network it gets, at creation rather
// than afterwards. A container connected after it started has already run with the
// default bridge for as long as the two calls took.
func networkingConfig(name string) docker.NetworkingConfig {
	if name == "" || name == networkModeNone {
		return docker.NetworkingConfig{}
	}
	return docker.NetworkingConfig{
		EndpointsConfig: map[string]*docker.EndpointSettings{
			name: {},
		},
	}
}
