package driver

import (
	"errors"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/internal/docker"
)

// remapped is a daemon's /info as one with user namespace remapping on answers it: the
// option in the list, and a root directory ending in the <uid>.<gid> of the range.
func remapped() docker.Info {
	return docker.Info{
		SecurityOptions: []string{"name=seccomp,profile=builtin", "name=userns", "name=cgroupns"},
		DockerRootDir:   "/var/lib/docker/165536.165536",
	}
}

// plain is a daemon without the remapping, which is what Docker Desktop answers.
func plain() docker.Info {
	return docker.Info{
		SecurityOptions: []string{"name=seccomp,profile=unconfined", "name=cgroupns"},
		DockerRootDir:   "/var/lib/docker",
	}
}

// "The runner refuses a daemon without the remapping" and says what to write, where, to
// decide otherwise. The refusal is a sentinel so that tooling can tell this from every
// other way a daemon can be unusable.
func TestADaemonWithoutTheRemappingIsRefused(t *testing.T) {
	floor, err := readUsernsFloor(plain(), DefaultPolicy())
	if err == nil {
		t.Fatalf("a daemon with no user namespace remapping was accepted: %+v", floor)
	}
	if !errors.Is(err, ErrUsernsRemapRequired) {
		t.Fatalf("the refusal is not the one a caller can recognise: %s", err)
	}
	for _, want := range []string{"require_userns_remap", PolicyPath, "root inside a container"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q: %s", want, err)
		}
	}
}

// The zero value of a Policy refuses too, which is the property the enumeration exists
// for: a driver constructed by a caller who never thought about the floor still has it.
func TestAPolicyNobodyFilledInStillRefuses(t *testing.T) {
	if _, err := readUsernsFloor(plain(), Policy{}); !errors.Is(err, ErrUsernsRemapRequired) {
		t.Fatalf("a zero Policy accepted a daemon with no remapping: %v", err)
	}
}

// "Only an operator who sets require_userns_remap: false in the runner's configuration
// gets past that refusal", and when one has, the driver says what was given up, once, in
// one plain sentence.
func TestALiftedFloorTakesWorkAndSaysSoOnce(t *testing.T) {
	p := DefaultPolicy()
	p.RequireUsernsRemap = RemapLifted
	// Named so that this machine has somewhere to put a secret and the only sentence
	// under test is the one about the floor.
	p.SecretsDir = "/dev/shm"

	floor, err := readUsernsFloor(plain(), p)
	if err != nil {
		t.Fatalf("a lifted floor refused anyway: %s", err)
	}
	if !floor.Lifted || floor.Remapped {
		t.Fatalf("the floor came back %+v", floor)
	}
	if _, _, ok := floor.ownership(); ok {
		t.Fatalf("a daemon that does not remap named an account to give the task's files to")
	}

	var said []string
	say := func(line string) { said = append(said, line) }
	floor.announce(p, say)
	floor.announce(p, say)
	floor.announce(p, say)
	if len(said) != 1 {
		t.Fatalf("the driver said it %d times: %v", len(said), said)
	}
	// Naming what is given up is the whole of the sentence's job. The person
	// reading it on a laptop is not the person who wrote the file.
	for _, want := range []string{"require_userns_remap", PolicyPath, "owned by a real uid on the host"} {
		if !strings.Contains(said[0], want) {
			t.Fatalf("the announcement does not name %q: %s", want, said[0])
		}
	}
}

// A daemon that does remap has given nothing up, so there is nothing to announce, even
// where an operator has lifted the floor on a machine that did not need it lifted.
func TestADaemonThatRemapsSaysNothing(t *testing.T) {
	p := DefaultPolicy()
	p.RequireUsernsRemap = RemapLifted
	p.SecretsDir = "/dev/shm"

	floor, err := readUsernsFloor(remapped(), p)
	if err != nil {
		t.Fatalf("readUsernsFloor: %s", err)
	}
	var said []string
	floor.announce(p, func(line string) { said = append(said, line) })
	if len(said) != 0 {
		t.Fatalf("a hardened daemon announced %v", said)
	}
}

// A secret is meant to be mounted on tmpfs, and a platform with none writes the value to
// a disk instead. That is a difference between what the documentation promises and what
// this machine can do, so it is said, once, rather than left to be discovered.
func TestAPlatformWithNoTmpfsSaysWhereASecretLands(t *testing.T) {
	p := DefaultPolicy()
	p.SecretsDir = ""

	floor, err := readUsernsFloor(remapped(), p)
	if err != nil {
		t.Fatalf("readUsernsFloor: %s", err)
	}
	var said []string
	say := func(line string) { said = append(said, line) }
	floor.announce(p, say)
	floor.announce(p, say)
	if len(said) != 1 {
		t.Fatalf("the driver said it %d times: %v", len(said), said)
	}
	for _, want := range []string{"tmpfs", "working directory", PolicyPath} {
		if !strings.Contains(said[0], want) {
			t.Fatalf("the announcement does not name %q: %s", want, said[0])
		}
	}
}

// The range is read off the daemon's own root directory, which is "the one place the
// remapped range is readable without parsing /etc/subuid", and it is the ownership a
// task's working directory is given before its container is created.
func TestTheRemappedRangeIsTheOwnershipATaskGets(t *testing.T) {
	floor, err := readUsernsFloor(remapped(), DefaultPolicy())
	if err != nil {
		t.Fatalf("readUsernsFloor: %s", err)
	}
	if !floor.Remapped {
		t.Fatalf("name=userns was not read as the remapping being on")
	}
	uid, gid, ok := floor.ownership()
	if !ok || uid != 165536 || gid != 165536 {
		t.Fatalf("the ownership is %d:%d, %v, and the daemon's root directory says 165536.165536", uid, gid, ok)
	}
}

// "A caller that cannot read the range must refuse rather than guess at one, because a
// working directory chowned to the wrong account is a container that silently fails to
// write its outputs."
func TestARemappedDaemonWhoseRangeCannotBeReadIsRefused(t *testing.T) {
	info := remapped()
	info.DockerRootDir = "/var/lib/docker"
	_, err := readUsernsFloor(info, DefaultPolicy())
	if err == nil {
		t.Fatalf("a range nobody could read was guessed at")
	}
	if !strings.Contains(err.Error(), "<uid>.<gid>") {
		t.Fatalf("the refusal does not say what it looked for: %s", err)
	}
}

// A daemon that does not remap gives a task's working directory to nobody in particular,
// so preparing it changes nothing rather than failing.
func TestPreparingAWorkingDirectoryOnADaemonThatDoesNotRemapDoesNothing(t *testing.T) {
	p := DefaultPolicy()
	p.RequireUsernsRemap = RemapLifted
	floor, err := readUsernsFloor(plain(), p)
	if err != nil {
		t.Fatalf("readUsernsFloor: %s", err)
	}

	w, err := newWorkdir(t.TempDir(), shardedTask, "")
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	defer w.remove()
	if err := floor.ownWorkdir(w); err != nil {
		t.Fatalf("ownWorkdir: %s", err)
	}
}
