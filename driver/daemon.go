package driver

import (
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/agentiik/agentiik/internal/docker"
)

// ErrUsernsRemapRequired is the refusal of a daemon that does not remap user namespaces.
// It is a sentinel so that a caller can tell the floor refusing from everything else that
// can go wrong with a daemon, which is what lets agk say "this machine does not meet the
// floor" rather than "docker failed".
var ErrUsernsRemapRequired = errors.New("this daemon does not remap user namespaces, and require_userns_remap has not been set to false")

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
	// once. Each is a property of the daemon and the policy rather than of a task,
	// so each is said when the daemon is opened; a sentence repeated per task is a
	// sentence nobody reads.
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

// announce says, once, what this machine gives up. It is called when the daemon is
// opened and not per task, because both sentences are about the machine and the policy.
//
// Each is one plain sentence naming the consequence rather than the setting, because the
// person who reads it on a laptop is not the person who wrote the file, and
// "require_userns_remap is false" tells them nothing they can act on. A machine that
// gives up neither thing says nothing at all.
func (f *usernsFloor) announce(p Policy, say func(string)) {
	if f == nil || say == nil {
		return
	}
	if f.Lifted && !f.Remapped {
		f.said.Do(func() {
			say("user namespace remapping is off on this daemon and require_userns_remap is false in " + PolicyPath + ", so the floor is lifted: a task's files are owned by a real uid on the host, root inside a container is the host's own root, and a process that escapes a container is that account rather than an unprivileged high-numbered one that maps to no real user.")
		})
	}
	// A secret is meant to be "mounted on tmpfs", and a tmpfs the daemon creates at
	// container start is empty and cannot be pre-populated, so the value is written
	// on this side and bound in. Where this platform has no tmpfs of its own, which
	// is the laptop the floor gets lifted for, it is written into the task's working
	// directory on a real filesystem instead. That is a difference worth one sentence.
	if p.SecretsDir == "" {
		f.saidSecrets.Do(func() {
			say("this platform has no tmpfs for the runner to write secret values on, so a task that is given a secret has its value written into the task's working directory on disk, where it is removed with the container rather than never having been written at all. An operator with a tmpfs names it in " + PolicyPath + ".")
		})
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
