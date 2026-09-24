package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// What a runner asks of its own host before it takes work: the three capabilities that let
// an unprivileged agent own a task's directory inside the remapped range, and a secrets
// directory on a tmpfs mounted noexec,nosuid,nodev.

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
	p.RequireSecretsTmpfs = SecretsTmpfsLifted

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
	p.RequireSecretsTmpfs = SecretsTmpfsLifted
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
	p.RequireSecretsTmpfs = SecretsTmpfsLifted
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

// "A server runner requires its secrets directory to be a tmpfs mounted
// noexec,nosuid,nodev." A directory on a disk is refused when the daemon is opened, and so
// is none at all, each naming what to mount and where to name it.
func TestASecretsDirectoryThatIsNotATmpfsIsRefused(t *testing.T) {
	daemon, err := dockertest.NewDaemon()
	if err != nil {
		t.Fatalf("starting a fake daemon: %s", err)
	}
	defer daemon.Close()

	disk := t.TempDir()
	for _, dir := range []string{disk, ""} {
		p := server()
		p.SecretsDir = dir
		d, err := New(Config{Socket: daemon.Socket(), Host: holding(0), Policy: p, WorkRoot: t.TempDir()})
		if err == nil {
			d.Close()
			t.Fatalf("a runner opened a daemon with its secrets directory at %q", dir)
		}
		if !errors.Is(err, ErrSecretsTmpfsRequired) {
			t.Fatalf("the refusal is %v, and it is ErrSecretsTmpfsRequired", err)
		}
		for _, want := range []string{"secrets_dir in " + PolicyPath, "noexec,nosuid,nodev", dir} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal of %q does not say %q: %s", dir, want, err)
			}
		}
	}

	// The same directory read as the tmpfs the floor asks for is taken, so what was
	// refused above is the mount and not the directory.
	p := server()
	p.SecretsDir = disk
	tmpfs := machine{fs: Filesystem{Tmpfs: true, NoExec: true, NoSUID: true, NoDev: true}}
	d, err := New(Config{Socket: daemon.Socket(), Host: tmpfs, Policy: p, WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("a secrets directory on a tmpfs mounted noexec,nosuid,nodev was refused: %s", err)
	}
	d.Close()

	// A caller that is not a runner lifts it, and a directory on a disk is taken.
	p.RequireSecretsTmpfs = SecretsTmpfsLifted
	d, err = New(Config{Socket: daemon.Socket(), Host: holding(0), Policy: p, WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("a lifted secrets floor refused anyway: %s", err)
	}
	d.Close()
}

// Every answer the kernel can give about the mount is held to the floor, and a refusal
// names each flag that is missing and none that is there.
func TestTheSecretsFloorNamesWhatTheMountLacks(t *testing.T) {
	p := server()
	p.SecretsDir = "/run/agentiik/secrets"
	// Each sentence is quoted up to its full stop, so that one naming a flag that is
	// there as well as the ones that are not reads differently and fails.
	for _, c := range []struct {
		fs   Filesystem
		says []string
	}{
		{Filesystem{Tmpfs: true, NoExec: true, NoSUID: true, NoDev: true}, nil},
		{Filesystem{Tmpfs: true, NoSUID: true, NoDev: true}, []string{"is a tmpfs mounted without noexec."}},
		{Filesystem{Tmpfs: true, NoExec: true}, []string{"is a tmpfs mounted without nosuid and nodev."}},
		{Filesystem{Tmpfs: true}, []string{"is a tmpfs mounted without noexec, nosuid and nodev."}},
		{Filesystem{NoExec: true, NoSUID: true, NoDev: true}, []string{"is not on a tmpfs"}},
		{Filesystem{Tmpfs: true, ReadOnly: true, NoExec: true, NoSUID: true, NoDev: true}, []string{"is a tmpfs this runner may not write to", "ReadWritePaths"}},
	} {
		err := judgeSecretsFilesystem(p, c.fs)
		if c.says == nil {
			if err != nil {
				t.Errorf("%+v was refused: %s", c.fs, err)
			}
			continue
		}
		if !errors.Is(err, ErrSecretsTmpfsRequired) {
			t.Errorf("%+v was not refused with ErrSecretsTmpfsRequired: %v", c.fs, err)
			continue
		}
		for _, want := range c.says {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal of %+v does not say %q: %s", c.fs, want, err)
			}
		}
	}

	p.RequireSecretsTmpfs = SecretsTmpfsLifted
	p.SecretsDir = ""
	if err := readSecretsDir(p, holding(0)); err != nil {
		t.Errorf("a lifted floor refused: %s", err)
	}
}

// statfs is what the floor reads the mount with, so a tmpfs has to read as one. /dev/shm
// is a tmpfs on every Linux this runs on; whether it carries noexec is the distribution's.
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

// The floor is held again before a value is written, since a tmpfs unmounted under a
// running runner leaves a directory of the same name on a disk. A task that is given no
// secret writes no value and is not held to it.
func TestNoSecretValueIsWrittenOnceTheTmpfsIsGone(t *testing.T) {
	store, err := artifact.New(artifact.Dir(t.TempDir()), "finance", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	disk := t.TempDir()
	p := server()
	p.SecretsDir = disk

	task := oneTask("ghcr.io/agentiik/http-request@" + imageDigest)
	w, err := newWorkdir(t.TempDir(), task.ID, disk)
	if err != nil {
		t.Fatalf("newWorkdir: %s", err)
	}
	t.Cleanup(func() { w.remove() })
	if _, err := prepare(context.Background(), task, w, p, holding(0), store, agk.Run{}, "", nil); err != nil {
		t.Fatalf("a task with no secret was held to the secrets floor: %s", err)
	}

	task = taskWithASecret(task.Image)
	_, err = prepare(context.Background(), task, w, p, holding(0), store, agk.Run{}, "", vault{"bearer": "s3cr3t-value"})
	if !errors.Is(err, ErrSecretsTmpfsRequired) {
		t.Fatalf("a value was prepared for a secrets directory on a disk, or refused unrecognisably: %v", err)
	}
	if charge, _ := Charged(err); charge != ChargePlatform {
		t.Errorf("the refusal is charged to %s, and the host is the platform's", charge)
	}
	filepath.Walk(disk, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			t.Errorf("a value was written on the disk at %s", path)
		}
		return nil
	})

	// The same directory read as the tmpfs it was is written on, so what was refused
	// above is the mount as it reads now and not the task.
	tmpfs := machine{fs: Filesystem{Tmpfs: true, NoExec: true, NoSUID: true, NoDev: true}}
	if _, err := prepare(context.Background(), task, w, p, tmpfs, store, agk.Run{}, "", vault{"bearer": "s3cr3t-value"}); err != nil {
		t.Fatalf("a value was refused a tmpfs mounted noexec,nosuid,nodev: %s", err)
	}
}
