package driver

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrOwnershipCapabilities is the refusal of a remapped daemon by a process that cannot
// give a task's directory to the remapped range. It is a sentinel for the reason
// ErrUsernsRemapRequired is one: a runner that exits on it is saying its unit needs a line,
// not that Docker failed.
var ErrOwnershipCapabilities = errors.New("this daemon remaps user namespaces, and this process does not hold the capabilities a task's directory is owned inside the remapped range with")

// ErrSecretsTmpfsRequired is the refusal of a secrets directory that is not a tmpfs mounted
// noexec,nosuid,nodev, by a policy that holds the secrets floor.
var ErrSecretsTmpfsRequired = errors.New("a runner writes secret values on a tmpfs mounted noexec,nosuid,nodev, and its secrets directory is not one")

// agentCapability is one Linux capability, by its bit in the kernel's sets and by the name a
// systemd unit and a Compose file write it with.
type agentCapability struct {
	bit  uint
	name string
}

// ownershipCapabilities are the three the agent holds and no other, in the order the unit
// on the page writes them.
//
// CAP_CHOWN gives a task's directory to the base of the remapped range before the
// container exists, which is a chown to a uid that is not this process's own. CAP_FOWNER
// changes the mode of what it no longer owns. CAP_DAC_OVERRIDE re-enters those directories
// to collect /agk/out and removes them when the task ends, including what a brick created
// there under an account of the range, which nothing else could remove. Membership of the
// group that owns the daemon socket is already root-equivalent, so the three give a process
// that has taken the agent over nothing the socket does not.
var ownershipCapabilities = []agentCapability{
	{0, "CAP_CHOWN"},
	{3, "CAP_FOWNER"},
	{1, "CAP_DAC_OVERRIDE"},
}

// Host is what a runner's floors ask of the machine this process runs on rather than of
// the daemon: the capabilities the process holds, and what the mount a directory sits on
// is. It is an interface for one reason, that the answers are the kernel's and a test on
// a fake daemon has to be able to give others: a remapped fake is opened by a test that
// holds no capability, and a tmpfs mounted noexec,nosuid,nodev is not something a test
// can mount.
type Host interface {
	// Capabilities is this process's effective set, bit n being the capability the
	// kernel numbers n.
	Capabilities() (uint64, error)

	// Filesystem says what the mount dir sits on is.
	Filesystem(dir string) (Filesystem, error)
}

// kernel is the Host that asks the kernel, with capget and statfs.
type kernel struct{}

func (kernel) Capabilities() (uint64, error)             { return effectiveCapabilities() }
func (kernel) Filesystem(dir string) (Filesystem, error) { return filesystemOf(dir) }

// host is the Host the configuration names, or the kernel.
func (c Config) host() Host {
	if c.Host != nil {
		return c.Host
	}
	return kernel{}
}

// readOwnership refuses a remapped daemon to a process that could not own a task's
// directory inside its range.
//
// It is read when the daemon is opened rather than at the first chown, because the chown
// comes after the image pull and the redemption of a task's secrets, and a runner that
// cannot do it would refuse every task it took, on the platform's account, while looking
// healthy. A daemon that does not remap is given nothing to chown and asks nothing here.
// The refusal names the lines that put it right, in the two forms a runner is installed
// in, because the person who reads it is the one who wrote the unit.
//
// Only a runner's policy is held to it. agk validate opens a daemon to read manifests and
// owns no directory, and agk run --local and agk brick test are run by a person on a
// machine of their own, where the chown of the first task that needs one refuses that task
// in its own words; none of them has a unit to put a line in.
func readOwnership(f *usernsFloor, p Policy, h Host) error {
	if f == nil || !f.Remapped || !p.runner() {
		return nil
	}
	held, err := h.Capabilities()
	if err != nil {
		return fmt.Errorf("driver: %w: its capabilities could not be read: %v", ErrOwnershipCapabilities, err)
	}
	var missing []string
	for _, c := range ownershipCapabilities {
		if held&(1<<c.bit) == 0 {
			missing = append(missing, c.name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("driver: %w: it lacks %s. A task's directory is given to uid %d and gid %d, the base of the range, before its container is created, and is re-entered and removed when the task ends, which an account without them cannot do. A runner installed by its systemd unit holds them with AmbientCapabilities=%s and CapabilityBoundingSet=%s under [Service]; one installed as a container runs as a user that is not root, so the daemon's cap_add alone leaves it none, and holds them as file capabilities on %s (setcap %s=ep) kept in its bounding set with cap_add: [%s]", ErrOwnershipCapabilities, andList(missing), f.UID, f.GID, capabilityNames(""), capabilityNames(""), executable(), strings.ToLower(strings.ReplaceAll(capabilityNames(""), " ", ",")), strings.ReplaceAll(capabilityNames("CAP_"), " ", ", "))
}

// executable is the file this process runs from, which is the one a setcap names, or the
// name the runner is shipped under where it cannot say.
func executable() string {
	if path, err := os.Executable(); err == nil {
		return path
	}
	return "agk-runner"
}

// runner says whether a policy is a runner's. The seccomp and secrets floors are lifted by
// no line of runner.toml, only by a caller that is not a runner: agk run --local, agk brick
// test and agk validate lift both.
func (p Policy) runner() bool {
	return !p.RequireSecretsTmpfs.Lifted() || !p.RequireSeccomp.Lifted()
}

// capabilityNames writes the three as a unit line writes them, space separated, with the
// prefix taken off where a Compose file leaves it out.
func capabilityNames(strip string) string {
	names := make([]string, len(ownershipCapabilities))
	for i, c := range ownershipCapabilities {
		names[i] = strings.TrimPrefix(c.name, strip)
	}
	return strings.Join(names, " ")
}

// Filesystem is what the kernel says of the mount a directory sits on, as far as a secret
// value written there is concerned.
type Filesystem struct {
	Tmpfs  bool
	NoExec bool
	NoSUID bool
	NoDev  bool
}

// readSecretsDir holds the secrets directory to the floor, where the policy holds it.
//
// It is read when the daemon is opened, so that a runner on a host with no such tmpfs is
// refused before it takes anything, and again before a value is written, since a tmpfs
// unmounted under a running runner leaves a directory of the same name on whatever was
// beneath it, and a value written there has touched a disk.
func readSecretsDir(p Policy, h Host) error {
	if p.RequireSecretsTmpfs.Lifted() {
		return nil
	}
	if p.SecretsDir == "" {
		return fmt.Errorf("driver: %w: the policy names no secrets directory, and without one a value would be written into the task's working directory on disk. %s", ErrSecretsTmpfsRequired, mountOne(p))
	}
	fs, err := h.Filesystem(p.SecretsDir)
	if err != nil {
		return fmt.Errorf("driver: %w: %s could not be asked what it is: %v. %s", ErrSecretsTmpfsRequired, p.SecretsDir, err, mountOne(p))
	}
	return judgeSecretsFilesystem(p, fs)
}

// judgeSecretsFilesystem is the half of readSecretsDir that reads no disk, so that every
// answer the kernel can give is held to the floor in a test.
func judgeSecretsFilesystem(p Policy, fs Filesystem) error {
	if !fs.Tmpfs {
		return fmt.Errorf("driver: %w: %s is not on a tmpfs, so a value written there touches a disk somebody has to erase. %s", ErrSecretsTmpfsRequired, p.SecretsDir, mountOne(p))
	}
	var without []string
	for _, flag := range []struct {
		set  bool
		name string
	}{{fs.NoExec, "noexec"}, {fs.NoSUID, "nosuid"}, {fs.NoDev, "nodev"}} {
		if !flag.set {
			without = append(without, flag.name)
		}
	}
	if len(without) == 0 {
		return nil
	}
	return fmt.Errorf("driver: %w: %s is a tmpfs mounted without %s. A secret is bound into the container from there, and a bind keeps the flags of the mount its source sits on, so these are the flags a brick meets at /agk/secrets/<name>. %s", ErrSecretsTmpfsRequired, p.SecretsDir, andList(without), mountOne(p))
}

// mountOne says how an operator puts the floor right, naming the file the directory is
// set in.
func mountOne(p Policy) string {
	return "Mount a tmpfs of the runner's own, with a line such as \"tmpfs /run/agentiik/secrets tmpfs noexec,nosuid,nodev,mode=0700,uid=agentiik,gid=agentiik 0 0\" in /etc/fstab, and name it with secrets_dir in " + sourceOrPath(p)
}

// sourceOrPath is the file the policy was read from, or the one an operator would write
// it in where none was read.
func sourceOrPath(p Policy) string {
	if p.Source != "" {
		return p.Source
	}
	return PolicyPath
}
