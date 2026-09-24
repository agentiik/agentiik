package driver

import (
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/docker"
)

// ErrUsernsRemapRequired is the refusal of a daemon that does not remap user namespaces.
// It is a sentinel so that a caller can tell the floor refusing from everything else that
// can go wrong with a daemon, which is what lets agk say "this machine does not meet the
// floor" rather than "docker failed".
var ErrUsernsRemapRequired = errors.New("this daemon does not remap user namespaces, and require_userns_remap has not been set to false")

// ErrSeccompRequired is the refusal of a daemon that applies no seccomp profile to a
// container. It is a sentinel for the reason ErrUsernsRemapRequired is one: a runner that
// exits on it is saying the machine needs fixing, not that Docker failed.
var ErrSeccompRequired = errors.New("this daemon applies no seccomp profile to a container, and a runner requires one")

// usernsFloor is what one daemon's user namespace posture came to, read once per daemon.
//
// The floor is the hardened tier of the documentation: "the runner refuses a daemon
// without the remapping, and only an operator who sets require_userns_remap: false in the
// runner's configuration gets past that refusal". Docker's own guidance is the reason the
// tier exists: "If a process attempts to escalate privilege outside of the namespace, the
// process is running as an unprivileged high-number UID on the host, which does not even
// map to a real user."
type usernsFloor struct {
	// Remapped says whether the daemon remaps, which is read off its own /info
	// rather than off a setting somebody wrote down about it.
	Remapped bool

	// UID and GID are the base of the remapped range, which is the ownership every
	// task's working directory is given before its container is created. Remapping
	// "introduces some configuration complexity in situations where the container
	// needs access to resources on the Docker host, such as bind mounts", and every
	// brick receives bind mounts under /agk, so this is the complexity and the runner
	// is where it is paid.
	UID int
	GID int

	// Lifted says the operator decided this machine does not need the floor.
	Lifted bool

	// said and saidSecrets carry the two sentences this machine is worth saying
	// once. Each is a property of the daemon and the policy rather than of a task, so
	// neither is repeated per task; a sentence said eight times is a sentence nobody
	// reads. They differ in when they are due: the floor applies to every container
	// this daemon will create and is said when it is opened, where a secret landing on
	// a disk applies only to a task that was given one and waits for the first.
	said        sync.Once
	saidSecrets sync.Once
}

// readUsernsFloor holds one daemon to the floor, or lets it past where the policy says
// so.
//
// Three outcomes, and each of them is a decision taken before any container exists. A
// daemon that remaps is the hardened tier and the range it remaps into is read here. A
// daemon that does not, under a policy that requires it, is refused. A daemon that does
// not, under a policy that lifted the requirement, is accepted and the driver says so.
func readUsernsFloor(info docker.Info, p Policy) (*usernsFloor, error) {
	if info.UsernsRemapped() {
		uid, gid, ok := info.RemappedRange()
		if !ok {
			return nil, fmt.Errorf("driver: the daemon remaps user namespaces but its root directory %q does not end in the <uid>.<gid> of the range: a task's working directory has to be prepared with ownership inside that range before its container is created, and an ownership guessed at is a container that starts and then silently cannot write its outputs", info.DockerRootDir)
		}
		return newRemappedFloor(uid, gid)
	}

	if !p.RequireUsernsRemap.Lifted() {
		return nil, fmt.Errorf("driver: %w. The runner refuses a daemon without the remapping so that root inside a container is an unprivileged high-numbered account on the host rather than the host's own root. An operator who has decided that this machine does not need the floor writes require_userns_remap = false in %s, which is a line in a file rather than a flag so that lifting the floor is a thing somebody did on purpose and can be read back", ErrUsernsRemapRequired, PolicyPath)
	}
	return &usernsFloor{Lifted: true}, nil
}

// newRemappedFloor reads the range the daemon's root directory names.
func newRemappedFloor(uid, gid string) (*usernsFloor, error) {
	u, err := strconv.Atoi(uid)
	if err != nil {
		return nil, fmt.Errorf("driver: the daemon's root directory names the remapped range as %s.%s, and %q is not an account number: %w", uid, gid, uid, err)
	}
	g, err := strconv.Atoi(gid)
	if err != nil {
		return nil, fmt.Errorf("driver: the daemon's root directory names the remapped range as %s.%s, and %q is not an account number: %w", uid, gid, gid, err)
	}
	return &usernsFloor{Remapped: true, UID: u, GID: g}, nil
}

// announce says, once, what this machine gives up about every container it will create. It
// is called when the daemon is opened and not per task, because the sentence is about the
// machine and the policy rather than about any one task.
//
// It is one plain sentence naming the consequence rather than the setting, because the
// person who reads it on a laptop is not the person who wrote the file, and
// "require_userns_remap is false" tells them nothing they can act on. A machine that gives
// up nothing says nothing at all.
//
// It names a file only where one was read. Policy.Source is the file LoadPolicy took the
// setting from, and it is empty for the caller that prints this sentence most often: agk run
// --local builds a Policy of its own, lifts the floor by default and opens no configuration
// at all. Naming PolicyPath there would be inventing a file, and a reader told their
// configuration says false goes looking for a configuration that is not on the machine. The
// refusal above, where the floor is held, does name PolicyPath, because there it is advice
// about where an operator would write the setting rather than a claim that anybody read it.
func (f *usernsFloor) announce(p Policy, say func(string)) {
	if f == nil || say == nil {
		return
	}
	if f.Lifted && !f.Remapped {
		f.said.Do(func() {
			because := "this runner does not require it"
			if p.Source != "" {
				because = "require_userns_remap is false in " + p.Source
			}
			say("user namespace remapping is off on this daemon and " + because + ", so the floor is lifted: a task's files are owned by a real uid on the host, root inside a container is the host's own root, and a process that escapes a container is that account rather than an unprivileged high-numbered one that maps to no real user.")
		})
	}
}

// announceSecrets says, once, where a secret value lands on a platform with no tmpfs.
//
// A secret is meant to be "mounted on tmpfs", and a tmpfs the daemon creates at container
// start is empty and cannot be pre-populated, so the value is written on this side and
// bound in. Where this platform has no tmpfs of its own, which is the laptop the floor gets
// lifted for, it is written into the task's working directory on a real filesystem instead.
// That is a difference between what the documentation promises and what this machine can
// do, and it is worth one sentence.
//
// It is said at the first task that is actually given a secret, and not when the daemon is
// opened, because a warning a person meets when they have asked for nothing of the kind is
// a warning they learn to scroll past. agk validate opens a daemon to read the manifests of
// the images a workflow names and never writes a value; a run whose steps declare no secret
// never writes one either. Either of those printing this sentence would spend the only
// attention it gets on a run it does not apply to.
//
// The step that earned it is named first, because a sentence said while a run is narrating
// itself lands between two lines about some other step, and a reader is owed the one it is
// actually about.
func (f *usernsFloor) announceSecrets(p Policy, step agk.Step, say func(string)) {
	if f == nil || say == nil || p.SecretsDir != "" {
		return
	}
	f.saidSecrets.Do(func() {
		say(string(step) + " is given a secret and this platform has no tmpfs for the runner to write secret values on, so the value is written into the task's working directory on disk, where it is removed with the container rather than never having been written at all. An operator with a tmpfs names it in " + PolicyPath + ".")
	})
}

// confinement is what one daemon confines a container with besides its namespaces and its
// dropped capabilities, read once per daemon off its /info.
type confinement struct {
	// Seccomp says every container is filtered, by the daemon's default profile or by
	// the one the policy names. unfiltered says why not, where it is not.
	Seccomp    bool
	unfiltered string

	// AppArmor and SELinux say which of the two the daemon applies. "Depending on the
	// host" is either, both or neither.
	AppArmor bool
	SELinux  bool
}

// readConfinement holds one daemon to the SecurityOpt row: "no-new-privileges, the default
// seccomp profile, and an AppArmor profile or SELinux label depending on the host".
//
// The row is read as two rules of different weight, because the host decides one and not
// the other. Seccomp is a floor: Docker builds it in and the kernels that matter offer it,
// so a daemon without it is one somebody switched off, and it runs a tenant's brick with
// the whole system call table open. A runner refuses it, and a caller that has lifted the
// floor hears what it gives up. AppArmor and SELinux belong to the distribution, which
// ships one, the other or neither, and a runner cannot install either; a daemon with
// neither is taken, and the driver says what that leaves.
//
// A profile the policy names is held to the daemon as well. The daemon ignores an AppArmor
// profile on a host without AppArmor, and a label where it labels nothing, so either would
// be a setting that silently did nothing; and a seccomp profile on a daemon with no seccomp
// fails every container as it starts. Each is refused here, before any container exists.
func readConfinement(info docker.Info, p Policy) (confinement, error) {
	profile, filters := info.SeccompProfile()
	c := confinement{
		Seccomp:  filters && (profile != "unconfined" || p.Seccomp != ""),
		AppArmor: info.AppArmor(),
		SELinux:  info.SELinux(),
	}

	if p.Seccomp != "" && !filters {
		return confinement{}, fmt.Errorf("driver: seccomp_profile in %s names a profile, and this daemon lists no seccomp among its security options, so every container would fail to start for asking for one", sourceOf(p))
	}
	if p.AppArmor != "" && !c.AppArmor {
		return confinement{}, fmt.Errorf("driver: apparmor_profile in %s names %q, and this daemon does not apply AppArmor, so the profile would be ignored rather than applied: name one on a host that has AppArmor, or leave the key out", sourceOf(p), p.AppArmor)
	}
	if p.SELinuxLabel != "" && !c.SELinux {
		return confinement{}, fmt.Errorf("driver: selinux_label in %s names %q, and this daemon does not label containers, so the label would be ignored rather than applied: a daemon labels only where it was started with --selinux-enabled on a host with SELinux enabled", sourceOf(p), p.SELinuxLabel)
	}

	if c.Seccomp {
		return c, nil
	}
	c.unfiltered = "it lists no seccomp among its security options, so it was built without seccomp or runs on a kernel that has none"
	if filters {
		c.unfiltered = "it was started with --seccomp-profile=unconfined"
	}
	if !p.RequireSeccomp.Lifted() {
		remedy := "Run the runner on a daemon and a kernel that offer seccomp"
		if filters {
			remedy = "Start the daemon without --seccomp-profile=unconfined, or name a profile with seccomp_profile in " + PolicyPath + ", which every container is then created with"
		}
		return confinement{}, fmt.Errorf("driver: %w: %s. Seccomp is what keeps a brick to the system calls a container needs, and no setting lifts this refusal. %s", ErrSeccompRequired, c.unfiltered, remedy)
	}
	return c, nil
}

// sourceOf names the file a policy was read from, or says there was none.
func sourceOf(p Policy) string {
	if p.Source == "" {
		return "the runner's policy"
	}
	return p.Source
}

// announce says, once, when the daemon is opened, what of the SecurityOpt row this daemon
// does not apply. A daemon that gives up nothing says nothing.
func (c confinement) announce(say func(string)) {
	if say == nil {
		return
	}
	held := "its namespaces and its dropped capabilities"
	if !c.Seccomp {
		say("this daemon applies no seccomp profile to a container, because " + c.unfiltered + ", so a container may make any system call the kernel offers rather than the ones Docker's default profile allows. A runner refuses such a daemon, and a local run only says so.")
	} else {
		held = "its namespaces, its dropped capabilities and its seccomp profile"
	}
	if !c.AppArmor && !c.SELinux {
		say("this daemon applies neither an AppArmor profile nor an SELinux label to a container, which is the host's to offer, so a container is held by " + held + " with no mandatory access control behind them.")
	}
}

// ownership is the account a task's working directory is given, and whether there is one
// to give it to. There is none on a daemon that does not remap, where the files are the
// runner's own and the container reads them as whatever account the host gives it.
func (f *usernsFloor) ownership() (uid, gid int, ok bool) {
	if f == nil || !f.Remapped {
		return 0, 0, false
	}
	return f.UID, f.GID, true
}

// ownWorkdir gives a task's working directory to the remapped range, where there is one.
// It is the one call a task makes about the floor, so that the ordering, ownership
// before the container is created, is written once here rather than remembered at every
// call site.
func (f *usernsFloor) ownWorkdir(w *workdir) error {
	uid, gid, ok := f.ownership()
	if !ok {
		return nil
	}
	return w.own(uid, gid)
}
