package driver

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
)

// PolicyPath is the file an operator writes a runner's settings in. The documentation
// names it for one setting and one reason: require_userns_remap "is a line in
// /etc/agentiik/runner.toml and not a command line flag, so the decision survives in
// something reviewable rather than in somebody's shell history".
const PolicyPath = "/etc/agentiik/runner.toml"

// UsernsFloor says whether a daemon without user namespace remapping is refused.
//
// It is an enumeration and not a bool, which is the whole point of the type.
// require_userns_remap defaults to true in the documentation and a Go bool defaults to
// false, so a Policy nobody filled in would read as a lifted floor, and a field whose
// zero value silently lifts a security floor is the one shape this must not have. The
// zero value here is the floor in place.
type UsernsFloor int

const (
	// RemapRequired refuses a daemon that does not remap user namespaces. It is the
	// zero value, so a Policy{} is the hardened tier and not the laptop.
	RemapRequired UsernsFloor = iota

	// RemapLifted takes work on a daemon without the remapping. An operator decided
	// that "this machine does not need the floor, which is a reasonable thing to
	// decide about a laptop and a serious thing to decide about a fleet".
	RemapLifted
)

// String writes the floor as the file's own value, so that the setting and the field
// read alike: require_userns_remap = true is RemapRequired and false is RemapLifted.
func (u UsernsFloor) String() string {
	if u == RemapLifted {
		return "false"
	}
	return "true"
}

// Lifted says whether the refusal has been lifted. It reads at the point of use as the
// question being asked there, which is not the same question the field name asks.
func (u UsernsFloor) Lifted() bool { return u == RemapLifted }

// ParseUsernsFloor reads the value of require_userns_remap. TOML has one spelling of a
// boolean, lowercase, and anything else is refused naming the key rather than defaulted,
// because defaulting a value somebody meant to set is how a floor gets lifted by a typo.
func ParseUsernsFloor(s string) (UsernsFloor, error) {
	switch s {
	case "true":
		return RemapRequired, nil
	case "false":
		return RemapLifted, nil
	default:
		return RemapRequired, fmt.Errorf("require_userns_remap is %q in %s: it is written true or false", s, PolicyPath)
	}
}

// Ulimit is one soft and hard pair, as the daemon's Ulimits carry them.
type Ulimit struct {
	Soft int64
	Hard int64
}

// Ulimits is the two the settings table names, "nofile and nproc set by the runner
// policy", and no others: a policy that could set any ulimit would be a second resources
// block nothing in the workflow language can see.
type Ulimits struct {
	NoFile Ulimit
	NProc  Ulimit
}

// Policy is what the machine says about every container it runs, as opposed to what a
// workflow says about one step. The settings table of the documentation is written
// partly in values a step asks for and partly in values the runner sets, and this is the
// second half.
//
// Its zero value is the floor in place and nothing else configured, which is what lets a
// caller with no /etc/agentiik/runner.toml pass Policy{} and still be refused on a daemon
// with no remapping. DefaultPolicy fills in the rest.
type Policy struct {
	// RequireUsernsRemap is the floor. It carries the file's own spelling so that
	// the setting and the field read alike.
	RequireUsernsRemap UsernsFloor

	// StopGrace is the t of the daemon's own stop, the wait between SIGTERM and
	// SIGKILL. The escalation belongs to the daemon rather than to a timer here, so
	// that it survives this process dying between the two signals.
	StopGrace time.Duration

	// PidsLimit is the default for PidsLimit where resources.pids says nothing, and
	// the ceiling where it says more. The documentation's default is 256.
	PidsLimit int64

	// Ulimits are nofile and nproc, applied to every container.
	Ulimits Ulimits

	// MemoryCap and CPUCap are the runner's half of "capped by the runner policy and
	// the namespace quota". Zero is no cap here, which is not the same as no cap at
	// all: the quota is the other half and it is not this package's.
	MemoryCap int64
	CPUCap    float64

	// TmpSize is the size of the /tmp tmpfs. The settings table says the writable
	// paths are "mounted as sized tmpfs", so there is no unsized case to configure.
	TmpSize int64

	// SecretsDir is the host directory secret values are written under before they
	// are bound at /agk/secrets/<name>. It is a tmpfs where the platform has one,
	// /dev/shm on Linux, because a value that touched a disk is a value somebody has
	// to erase. Empty means the task's working directory, which is the laptop case
	// and which the driver says out loud.
	SecretsDir string

	// LogMaxBytes and LogMaxLines cap the collected standard error. A log is a
	// diagnostic and not a payload: the cap keeps one runaway task from filling the
	// store, and the truncation marker says what was dropped. Zero is no cap.
	LogMaxBytes int64
	LogMaxLines int

	// Seccomp, AppArmor and SELinuxLabel are the profiles of the SecurityOpt row,
	// "the default seccomp profile, and an AppArmor profile or SELinux label
	// depending on the host". Each is empty by default, which leaves the daemon its
	// own defaults; naming one here replaces them.
	Seccomp      string
	AppArmor     string
	SELinuxLabel string

	// AllowCapAdd is the explicit permission the CapDrop row speaks of: "CapAdd is
	// refused unless the runner policy allows it explicitly". Nothing a workflow or a
	// manifest writes can ask for a capability today, so this is a door that stays
	// shut rather than one that is guarded.
	AllowCapAdd []string

	// Shell is what a script step runs under where the step names none. Empty is
	// DefaultShell.
	Shell []string

	// Helper is the static agk binary on this host, mounted read-only at BinPath for
	// a script step: "a static helper for scripts that want to be precise rather than
	// lucky, with agk items, agk emit and agk attach. It is a convenience, never a
	// requirement". It is a runner setting because which binary is on this host is a
	// fact about the host and not about a workflow, and empty is a runner that has
	// none to offer, which binds nothing.
	Helper string
}

// DefaultPolicy is the runner as it is installed: the floor in place, the documented
// pids default, and modest ceilings for the settings the documentation leaves to the
// runner.
func DefaultPolicy() Policy {
	return Policy{
		RequireUsernsRemap: RemapRequired,

		// The daemon's own default for POST /containers/{id}/stop. Taking a
		// different number here would make the driver's grace and the grace of a
		// docker stop typed by hand two different things on one host.
		StopGrace: 10 * time.Second,

		// "PidsLimit: from resources.pids, default 256."
		PidsLimit: 256,

		Ulimits: Ulimits{
			// A brick reads a handful of files and writes a handful more. The
			// soft limit is what a process sees by default and the hard limit
			// is what it may raise itself to, which is room for a step that
			// opens many files on purpose without room for one that leaks them.
			NoFile: Ulimit{Soft: 1024, Hard: 4096},
			// nproc matches the pid ceiling. A process the pids cgroup will not
			// let exist should not be one the ulimit would have allowed, so the
			// two say the same number rather than two numbers whose difference
			// nobody can explain.
			NProc: Ulimit{Soft: 256, Hard: 256},
		},

		// Enough scratch for a step that stages a file before emitting it, small
		// enough that a runaway write fails the task rather than the host. tmpfs
		// pages are host memory, which is why this is sized and not left open.
		TmpSize: 64 << 20,

		SecretsDir: defaultSecretsDir(),

		// A log is read by a person. Four megabytes is the same order as the
		// largest envelope a port may carry, so the largest thing one task hands
		// back is the same size whichever way it hands it back.
		LogMaxBytes: 4 << 20,
		LogMaxLines: 50000,
	}
}

// defaultSecretsDir answers where a secret value may be written without touching a disk.
//
// /dev/shm is a tmpfs on every Linux distribution that matters, and it is what a runner
// installed as a container or as a systemd unit has. Elsewhere, and macOS is the case
// that matters because agk run --local has to work there, there is no equivalent path,
// so this is empty and the value lands in the task's working directory instead. That is
// a real difference and the driver announces it rather than pretending otherwise.
func defaultSecretsDir() string {
	if runtime.GOOS == "linux" {
		return "/dev/shm"
	}
	return ""
}

// LoadPolicy reads the runner's configuration file.
//
// It reads the keys this package owns and ignores every other key and every table it
// does not know, which is the permissive reading the file demands: the same file carries
// the runner's own settings and will carry a [hooks] block, and a driver that refused
// what it did not recognise would refuse a file a later version wrote.
//
// There is no TOML dependency behind it. One boolean is the whole of what this package
// owns in that file today, and a parser for the rest belongs where the rest is read. When
// the runner takes one, this moves behind it and its behaviour does not change.
//
// A missing file is returned as the error it is, wrapping fs.ErrNotExist, so that a
// caller can tell "there is no file" from "the file says something I cannot read" and
// fall back to DefaultPolicy for the first only. Reading a floor out of a file that is
// not there would be the one mistake this whole shape exists to prevent.
func LoadPolicy(path string) (Policy, error) {
	f, err := os.Open(path)
	if err != nil {
		return DefaultPolicy(), err
	}
	defer f.Close()

	p := DefaultPolicy()
	s := bufio.NewScanner(f)
	line := 0
	// A key belongs to the table it is written under. Only the root table is read,
	// because require_userns_remap is written at the top of the file and a
	// require_userns_remap under [hooks] would be a different setting with the same
	// name.
	root := true
	for s.Scan() {
		line++
		text := strings.TrimSpace(s.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if strings.HasPrefix(text, "[") {
			root = false
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if !root || key != "require_userns_remap" {
			continue
		}
		floor, err := ParseUsernsFloor(bareValue(value))
		if err != nil {
			return DefaultPolicy(), fmt.Errorf("%s line %d: %w", path, line, err)
		}
		p.RequireUsernsRemap = floor
	}
	if err := s.Err(); err != nil {
		return DefaultPolicy(), fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// bareValue takes the value off a key line: the first word, with a trailing comment
// dropped. A boolean is one word, so this is the whole of what reading one needs, and it
// does not pretend to read a quoted string or an array.
func bareValue(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.IndexAny(v, " \t#"); i >= 0 {
		v = v[:i]
	}
	return v
}
