package driver

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// What a runner asks of its own host before it takes work: the three capabilities that let
// an unprivileged agent own a task's directory inside the remapped range.

// machine is a Host of a test's own: the capabilities it answers with, and the mount every
// directory that exists reads as.
type machine struct {
	caps uint64
	fs   Filesystem
}

func (m machine) Capabilities() (uint64, error) { return m.caps, nil }

func (m machine) Filesystem(dir string) (Filesystem, error) {
	if _, err := os.Stat(dir); err != nil {
		return Filesystem{}, err
	}
	return m.fs, nil
}

// holding is a machine whose process holds set, and whose every directory is on a disk.
func holding(set uint64) machine { return machine{caps: set} }

// ownershipSet is the three and nothing else, which is what the unit on the page grants.
func ownershipSet() uint64 {
	var set uint64
	for _, c := range ownershipCapabilities {
		set |= 1 << c.bit
	}
	return set
}

// server is a runner's policy on a daemon that is not remapped, which is the fake's
// default: every floor held but the userns one.
func server() Policy {
	p := DefaultPolicy()
	p.RequireUsernsRemap = RemapLifted
	return p
}

// "Give the agent CAP_CHOWN, CAP_FOWNER and CAP_DAC_OVERRIDE and nothing else." A runner
// that finds a remapped daemon and lacks one of them is refused when the daemon is opened,
// naming the one it lacks and the lines that grant them, rather than at the first chown,
// after a pull and a redemption.
func TestARemappedDaemonIsRefusedToAProcessWithoutTheThreeCapabilities(t *testing.T) {
	daemon, err := dockertest.NewDaemon(dockertest.WithUsernsRemap(165536, 165536))
	if err != nil {
		t.Fatalf("starting a fake daemon: %s", err)
	}
	defer daemon.Close()
	p := DefaultPolicy()

	// CAP_CHOWN and CAP_DAC_OVERRIDE, and not CAP_FOWNER.
	host := holding(1<<0 | 1<<1)
	d, err := New(Config{Socket: daemon.Socket(), Host: host, Policy: p, WorkRoot: t.TempDir()})
	if err == nil {
		d.Close()
		t.Fatalf("a remapped daemon was opened by a process that cannot own a task's directory inside its range")
	}
	if !errors.Is(err, ErrOwnershipCapabilities) {
		t.Fatalf("the refusal is %v, and it is ErrOwnershipCapabilities", err)
	}
	for _, want := range []string{
		"lacks CAP_FOWNER.",
		"uid 165536",
		"AmbientCapabilities=CAP_CHOWN CAP_FOWNER CAP_DAC_OVERRIDE",
		"CapabilityBoundingSet=CAP_CHOWN CAP_FOWNER CAP_DAC_OVERRIDE",
		"setcap cap_chown,cap_fowner,cap_dac_override=ep",
		"cap_add: [CHOWN, FOWNER, DAC_OVERRIDE]",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %s", want, err)
		}
	}

	// The three are enough, and nothing else is asked for.
	host = holding(ownershipSet())
	d, err = New(Config{Socket: daemon.Socket(), Host: host, Policy: p, WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("a process holding the three was refused: %s", err)
	}
	d.Close()
}

// agk validate opens a remapped daemon to read manifests and owns no directory, and it is
// not a runner, which it says by lifting every floor: it is not asked for the three.
func TestACallerThatIsNotARunnerIsNotAskedForTheCapabilities(t *testing.T) {
	daemon, err := dockertest.NewDaemon(dockertest.WithUsernsRemap(165536, 165536))
	if err != nil {
		t.Fatalf("starting a fake daemon: %s", err)
	}
	defer daemon.Close()
	p := DefaultPolicy()
	p.RequireUsernsRemap = RemapLifted
	p.RequireSeccomp = SeccompLifted
	d, err := New(Config{Socket: daemon.Socket(), Host: holding(0), Policy: p, WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("a caller that lifted every floor was refused a remapped daemon: %s", err)
	}
	d.Close()
}

// A daemon that does not remap is given nothing to chown, so a process holding nothing
// opens it.
func TestADaemonThatDoesNotRemapAsksForNoCapability(t *testing.T) {
	daemon, err := dockertest.NewDaemon()
	if err != nil {
		t.Fatalf("starting a fake daemon: %s", err)
	}
	defer daemon.Close()
	host := holding(0)

	p := server()
	d, err := New(Config{Socket: daemon.Socket(), Host: host, Policy: p, WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("a daemon that does not remap asked for capabilities: %s", err)
	}
	d.Close()
}

// A daemon restarted into remapping, under a runner opened on it before, is held to the
// capabilities before the next container, as it is to every other floor, and nothing is
// created on it.
func TestADaemonRestartedIntoRemappingIsRefusedWithoutTheCapabilities(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		return 0, wrote(c, "out", agk.NewItem(map[string]any{"ran": true}))
	})

	restart(t, r, dockertest.WithUsernsRemap(165536, 165536))

	_, err := r.Run(t.Context(), oneTask(ref))
	if !errors.Is(err, ErrOwnershipCapabilities) {
		t.Fatalf("a task ran on a daemon restarted into remapping by a process that cannot own its directory, or was refused unrecognisably: %v", err)
	}
	if charge, _ := Charged(err); charge != ChargePlatform {
		t.Errorf("the refusal is charged to %s, and the host is the platform's", charge)
	}
	if created := r.daemon.Created(); len(created) != 0 {
		t.Fatalf("a container was created: %v", created)
	}
}

// statfs is what a runner reads the mount of its work root with, so a tmpfs has to read as
// one. /dev/shm is a tmpfs on every Linux this runs on.
func TestStatfsReadsATmpfsAsOne(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("statfs's filesystem magic is Linux's")
	}
	fs, err := filesystemOf("/dev/shm")
	if err != nil {
		t.Skipf("no /dev/shm here: %s", err)
	}
	if !fs.Tmpfs {
		t.Fatalf("/dev/shm read as %+v, and it is a tmpfs", fs)
	}
}
