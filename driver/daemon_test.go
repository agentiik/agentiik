package driver

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"

	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// remapped is a daemon's /info as one with user namespace remapping on answers it: the
// option in the list, and a root directory ending in the <uid>.<gid> of the range.
func remapped() docker.Info {
	return docker.Info{
		SecurityOptions: []string{"name=seccomp,profile=builtin", "name=userns", "name=cgroupns"},
		DockerRootDir:   "/var/lib/docker/165536.165536",
	}
}

// plain is a daemon without the remapping, which is what Docker Desktop answers: its own
// seccomp profile, and neither AppArmor nor SELinux.
func plain() docker.Info {
	return docker.Info{
		SecurityOptions: []string{"name=seccomp,profile=builtin", "name=cgroupns"},
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
	for _, want := range []string{"this runner does not require it", "owned by a real uid on the host"} {
		if !strings.Contains(said[0], want) {
			t.Fatalf("the announcement does not name %q: %s", want, said[0])
		}
	}
	// And it names no file, because this policy was not read from one. A sentence that
	// said a configuration set this would send its reader looking for a configuration
	// that is not on the machine: nothing in this package opens PolicyPath, and the
	// caller that prints this most often is agk run --local, which opens nothing.
	if strings.Contains(said[0], PolicyPath) {
		t.Fatalf("the announcement names a file nobody read: %s", said[0])
	}

	// Where a file was read, the sentence names it, because there the claim is true.
	var fromFile []string
	p.Source = "/etc/agentiik/runner.toml"
	floor.said = sync.Once{}
	floor.announce(p, func(line string) { fromFile = append(fromFile, line) })
	if len(fromFile) != 1 || !strings.Contains(fromFile[0], "require_userns_remap is false in /etc/agentiik/runner.toml") {
		t.Fatalf("a policy read from a file was announced as %v", fromFile)
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
	floor.announceSecrets(p, "charge", say)
	floor.announceSecrets(p, "charge", say)
	if len(said) != 1 {
		t.Fatalf("the driver said it %d times: %v", len(said), said)
	}
	for _, want := range []string{"tmpfs", "working directory", PolicyPath} {
		if !strings.Contains(said[0], want) {
			t.Fatalf("the announcement does not name %q: %s", want, said[0])
		}
	}
}

// Opening a daemon is not a reason to hear about secrets. agk validate opens one to read
// the manifests of the images a workflow names, and a run whose steps declare no secret
// writes no value either; a sentence printed on both is one spent where it does not apply.
func TestOpeningADaemonSaysNothingAboutSecrets(t *testing.T) {
	p := DefaultPolicy()
	p.RequireUsernsRemap = RemapLifted
	p.SecretsDir = ""

	floor, err := readUsernsFloor(plain(), p)
	if err != nil {
		t.Fatalf("readUsernsFloor: %s", err)
	}
	var said []string
	floor.announce(p, func(line string) { said = append(said, line) })
	if len(said) != 1 {
		t.Fatalf("opening the daemon said %d things: %v", len(said), said)
	}
	if strings.Contains(said[0], "tmpfs") {
		t.Fatalf("opening the daemon named a secret's landing place: %s", said[0])
	}

	// And the sentence is still there for the task that earns it.
	floor.announceSecrets(p, "charge", func(line string) { said = append(said, line) })
	if len(said) != 2 || !strings.Contains(said[1], "tmpfs") {
		t.Fatalf("the first task with a secret was told %v", said[1:])
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

// A daemon with no seccomp, or one started with --seccomp-profile=unconfined, runs a brick
// with the whole system call table open. A runner refuses it, whatever its file says, and
// says what to change; the refusal is a sentinel so that a runner can exit on it as the
// machine's fault rather than Docker's.
func TestADaemonWithoutSeccompIsRefusedOnAServer(t *testing.T) {
	for _, c := range []struct {
		name    string
		options []string
		want    string
	}{
		{"no seccomp at all", []string{"name=apparmor", "name=userns"}, "lists no seccomp"},
		{"seccomp switched off", []string{"name=apparmor", "name=seccomp,profile=unconfined", "name=userns"}, "--seccomp-profile=unconfined"},
	} {
		p := DefaultPolicy()
		// The file cannot lift the seccomp floor, and lifting the userns one does not
		// lift it either: they are two floors.
		p.RequireUsernsRemap = RemapLifted
		_, err := readConfinement(docker.Info{SecurityOptions: c.options}, p)
		if !errors.Is(err, ErrSeccompRequired) {
			t.Fatalf("%s: a runner took the daemon, or refused it unrecognisably: %v", c.name, err)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: the refusal does not say why: %s", c.name, err)
		}
	}
}

// Locally the same daemon is taken and the machine says what it gives up, once, when the
// daemon is opened: agk run --local runs on somebody's own laptop, where a refusal would
// stop them for a fact about their machine they are owed a sentence about instead.
func TestADaemonWithoutSeccompIsAnnouncedLocally(t *testing.T) {
	p := DefaultPolicy()
	p.RequireSeccomp = SeccompLifted

	c, err := readConfinement(docker.Info{SecurityOptions: []string{"name=apparmor", "name=seccomp,profile=unconfined"}}, p)
	if err != nil {
		t.Fatalf("a lifted seccomp floor refused anyway: %s", err)
	}
	var said []string
	c.announce(func(s string) { said = append(said, s) })
	if len(said) != 1 {
		t.Fatalf("the driver said %d things: %v", len(said), said)
	}
	for _, want := range []string{"no seccomp profile", "--seccomp-profile=unconfined", "any system call", "A runner refuses such a daemon"} {
		if !strings.Contains(said[0], want) {
			t.Errorf("the announcement does not say %q: %s", want, said[0])
		}
	}
}

// A daemon started unconfined still filters where the runner names a profile of its own,
// because every container is then created with that profile and the daemon's default is
// never the one applied.
func TestARunnerProfileConfinesAnUnconfinedDaemon(t *testing.T) {
	p := DefaultPolicy()
	p.Seccomp = `{"defaultAction":"SCMP_ACT_ERRNO"}`
	c, err := readConfinement(docker.Info{SecurityOptions: []string{"name=apparmor", "name=seccomp,profile=unconfined"}}, p)
	if err != nil {
		t.Fatalf("a daemon applying the runner's own profile was refused: %s", err)
	}
	if !c.Seccomp {
		t.Fatalf("a container created with the runner's profile was read as unfiltered")
	}
}

// A profile the runner names that lets every call through filters nothing, whatever the
// daemon was started with, since it replaces the daemon's own for every container. It is
// counted as no profile: refused under the floor, said out loud where the floor is lifted.
func TestAProfileThatFiltersNothingIsNoProfile(t *testing.T) {
	for _, profile := range []string{
		`{"defaultAction":"SCMP_ACT_ALLOW"}`,
		`{"defaultAction":"SCMP_ACT_LOG","syscalls":[{"names":["reboot"],"action":"SCMP_ACT_ALLOW"}]}`,
	} {
		for _, daemon := range []string{"name=seccomp,profile=unconfined", "name=seccomp,profile=builtin"} {
			p := DefaultPolicy()
			p.Seccomp = profile
			info := docker.Info{SecurityOptions: []string{"name=apparmor", daemon}}
			_, err := readConfinement(info, p)
			if !errors.Is(err, ErrSeccompRequired) {
				t.Fatalf("%s on a daemon listing %s was taken as a filter: %v", profile, daemon, err)
			}
			if !strings.Contains(err.Error(), "lets every system call through") {
				t.Errorf("the refusal does not say why: %s", err)
			}

			p.RequireSeccomp = SeccompLifted
			c, err := readConfinement(info, p)
			if err != nil {
				t.Fatalf("a lifted seccomp floor refused anyway: %s", err)
			}
			if c.Seccomp {
				t.Fatalf("%s was read as filtering", profile)
			}
		}
	}

	// A profile that refuses something is a filter, on a daemon started unconfined too.
	p := DefaultPolicy()
	p.Seccomp = `{"defaultAction":"SCMP_ACT_ALLOW","syscalls":[{"names":["reboot"],"action":"SCMP_ACT_ERRNO"}]}`
	if c, err := readConfinement(docker.Info{SecurityOptions: []string{"name=seccomp,profile=unconfined"}}, p); err != nil || !c.Seccomp {
		t.Fatalf("a profile refusing reboot was not taken as a filter: %v", err)
	}
}

// "An AppArmor profile or SELinux label depending on the host": the host decides, and a
// runner cannot install either, so a daemon with neither is taken on a server as it is on
// a laptop, and the driver says what that leaves. A daemon with one of the two says
// nothing about it.
func TestADaemonWithNeitherAppArmorNorSELinuxIsAnnounced(t *testing.T) {
	for _, c := range []struct {
		name    string
		options []string
		said    int
	}{
		{"Docker Desktop", plain().SecurityOptions, 1},
		{"Ubuntu", []string{"name=apparmor", "name=seccomp,profile=builtin"}, 0},
		{"Fedora with --selinux-enabled", []string{"name=seccomp,profile=builtin", "name=selinux"}, 0},
	} {
		confined, err := readConfinement(docker.Info{SecurityOptions: c.options}, DefaultPolicy())
		if err != nil {
			t.Fatalf("%s: refused: %s", c.name, err)
		}
		var said []string
		confined.announce(func(s string) { said = append(said, s) })
		if len(said) != c.said {
			t.Fatalf("%s: the driver said %v", c.name, said)
		}
		if c.said == 1 && (!strings.Contains(said[0], "neither an AppArmor profile nor an SELinux label") || !strings.Contains(said[0], "seccomp profile")) {
			t.Errorf("%s: the announcement does not say what is left: %s", c.name, said[0])
		}
	}
}

// A profile the runner's file names is held to what the daemon can apply. The daemon
// ignores an AppArmor profile on a host without AppArmor and a label where it labels
// nothing, so either would be a setting that silently did nothing, and a seccomp profile
// on a daemon with no seccomp would fail every container at its start.
func TestAProfileTheDaemonCannotApplyIsRefused(t *testing.T) {
	for _, c := range []struct {
		name    string
		policy  func(*Policy)
		options []string
		want    string
	}{
		{"apparmor_profile", func(p *Policy) { p.AppArmor = "agentiik-brick" }, []string{"name=seccomp,profile=builtin", "name=selinux"}, "does not apply AppArmor"},
		{"selinux_label", func(p *Policy) { p.SELinuxLabel = "level:s0:c1" }, []string{"name=seccomp,profile=builtin", "name=apparmor"}, "does not label containers"},
		{"seccomp_profile", func(p *Policy) { p.Seccomp = `{"defaultAction":"SCMP_ACT_ERRNO"}`; p.RequireSeccomp = SeccompLifted }, []string{"name=apparmor"}, "lists no seccomp"},
	} {
		p := DefaultPolicy()
		p.Source = "/etc/agentiik/runner.toml"
		c.policy(&p)
		_, err := readConfinement(docker.Info{SecurityOptions: c.options}, p)
		if err == nil {
			t.Fatalf("%s was taken on a daemon that cannot apply it", c.name)
		}
		for _, want := range []string{c.name, c.want, p.Source} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal of %s does not say %q: %s", c.name, want, err)
			}
		}
	}
}

// New holds the seccomp floor before anything else happens, against a daemon on a socket
// rather than a value handed to a function: a runner opening a daemon without seccomp is
// refused, and a local caller opening the same daemon is told once and goes on.
func TestNewRefusesADaemonWithoutSeccompUnlessTheFloorIsLifted(t *testing.T) {
	daemon, err := dockertest.NewDaemon(dockertest.WithoutSeccomp, dockertest.WithUsernsRemap(165536, 165536))
	if err != nil {
		t.Fatalf("starting a fake daemon: %s", err)
	}
	defer daemon.Close()

	d, err := New(Config{Socket: daemon.Socket(), Policy: DefaultPolicy(), WorkRoot: t.TempDir()})
	if err == nil {
		d.Close()
		t.Fatalf("a runner opened a daemon that applies no seccomp profile")
	}
	if !errors.Is(err, ErrSeccompRequired) {
		t.Fatalf("the refusal is %v, and the seccomp floor refuses with ErrSeccompRequired", err)
	}

	p := DefaultPolicy()
	p.RequireSeccomp = SeccompLifted
	var said []string
	d, err = New(Config{Socket: daemon.Socket(), Policy: p, WorkRoot: t.TempDir(), Announce: func(s string) { said = append(said, s) }})
	if err != nil {
		t.Fatalf("a lifted seccomp floor refused anyway: %s", err)
	}
	defer d.Close()
	told := 0
	for _, s := range said {
		if strings.Contains(s, "no seccomp profile") {
			told++
		}
	}
	if told != 1 {
		t.Fatalf("opening the daemon said %v, and the missing seccomp is said once", said)
	}
}

// A cpu_cap is what every step naming no cpu is given, and the daemon refuses a container
// asking for more cores than it has, so a cap above the host's cores is refused when the
// daemon is opened, naming the count, rather than failing every such step as it is
// created. The fake daemon has two.
func TestNewRefusesACPUCapAboveTheDaemonsCores(t *testing.T) {
	daemon, err := dockertest.NewDaemon(dockertest.WithUsernsRemap(165536, 165536))
	if err != nil {
		t.Fatalf("starting a fake daemon: %s", err)
	}
	defer daemon.Close()

	p, err := LoadPolicy(writePolicyFile(t, "cpu_cap = \"4\"\n"))
	if err != nil {
		t.Fatalf("LoadPolicy: %s", err)
	}
	d, err := New(Config{Socket: daemon.Socket(), Policy: p, WorkRoot: t.TempDir()})
	if err == nil {
		d.Close()
		t.Fatalf("a runner opened a daemon with 2 CPUs under a cap of 4")
	}
	for _, want := range []string{"cpu_cap in " + p.Source, "is 4 cores", "this daemon has 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %s", want, err)
		}
	}

	for _, cores := range []string{"2", "0.5"} {
		p, err := LoadPolicy(writePolicyFile(t, "cpu_cap = \""+cores+"\"\n"))
		if err != nil {
			t.Fatalf("LoadPolicy: %s", err)
		}
		d, err := New(Config{Socket: daemon.Socket(), Policy: p, WorkRoot: t.TempDir()})
		if err != nil {
			t.Fatalf("a cap of %s on a daemon with 2 CPUs was refused: %s", cores, err)
		}
		d.Close()
	}
}

// A [hooks] table in the file runs nothing, and opening the daemon says so once, because
// an operator who wrote a pre_task is relying on it having run.
func TestNewSaysTheHooksDoNotRun(t *testing.T) {
	daemon, err := dockertest.NewDaemon(dockertest.WithUsernsRemap(165536, 165536))
	if err != nil {
		t.Fatalf("starting a fake daemon: %s", err)
	}
	defer daemon.Close()

	p, err := LoadPolicy(writePolicyFile(t, "[hooks]\npre_task = [\"/usr/local/sbin/attach-licence\"]\n"))
	if err != nil {
		t.Fatalf("LoadPolicy: %s", err)
	}
	var said []string
	d, err := New(Config{Socket: daemon.Socket(), Policy: p, WorkRoot: t.TempDir(), Announce: func(s string) { said = append(said, s) }})
	if err != nil {
		t.Fatalf("New: %s", err)
	}
	defer d.Close()
	hooks := 0
	for _, s := range said {
		if strings.Contains(s, "[hooks]") && strings.Contains(s, p.Source) && strings.Contains(s, "v0.9.0") {
			hooks++
		}
	}
	if hooks != 1 {
		t.Fatalf("opening the daemon said %v, and the skipped hooks are said once", said)
	}
}

// A runner lives longer than the configuration of the daemon under it. A daemon restarted
// with seccomp switched off, after New held it to the floor, drops the event stream, and
// the next container waits until the daemon has been read again: refused with
// ErrSeccompRequired, on the platform's account, and never created. Put right and
// restarted again, the daemon is taken back by the same driver.
func TestADaemonRestartedWithoutSeccompIsRefusedBeforeTheNextContainer(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		return 0, wrote(c, "out", agk.NewItem(map[string]any{"ran": true}))
	})

	restart(t, r, dockertest.WithoutSeccomp)

	_, err := r.Run(t.Context(), oneTask(ref))
	if !errors.Is(err, ErrSeccompRequired) {
		t.Fatalf("a task ran on a daemon restarted without seccomp, or was refused unrecognisably: %v", err)
	}
	if charge, _ := Charged(err); charge != ChargePlatform {
		t.Errorf("the refusal is charged to %s, and the daemon is the platform's", charge)
	}
	if created := r.daemon.Created(); len(created) != 0 {
		t.Fatalf("a container was created on a daemon that meets no floor: %v", created)
	}

	// The refused task started nothing, so its key is still to be run.
	r.daemon.Restart()
	result, err := r.Run(t.Context(), oneTask(ref))
	if err != nil {
		t.Fatalf("the daemon put right was not taken back: %s", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the state is %s", result.State)
	}
}

// restart restarts the runner's daemon once the driver's event stream is open, and waits
// for the driver to hear the stream drop, which is what a restart looks like from this
// side.
func restart(t *testing.T, r *runner, bs ...dockertest.Behaviour) {
	t.Helper()
	within(t, "the driver never opened its event stream", func() bool { return r.daemon.Streams() > 0 })
	dropped := r.said.count("event stream dropped")
	r.daemon.Restart(bs...)
	within(t, "the driver never heard its event stream drop", func() bool { return r.said.count("event stream dropped") > dropped })
}

// within waits for a condition, and fails saying what never happened.
func within(t *testing.T, never string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(never)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
