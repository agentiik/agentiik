package driver

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// A task's secret values reach its container on a tmpfs volume of its own, never through a
// path of the host: the Compose installation prepares nothing on the host, and a value
// still never touches a disk.

// secretsRunner is a runner whose container reads its secret back and publishes it, so that
// a task that ran is a task that found its value where the contract puts it.
func secretsRunner(t *testing.T, ref string) *runner {
	t.Helper()
	return newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		if dockertest.IsHolder(c) {
			t.Errorf("the test's container function was handed the holder")
		}
		b, err := c.ReadFile(SecretsDir + "/bearer")
		if err != nil {
			return 1, err
		}
		return 0, wrote(c, "out", agk.NewItem(map[string]any{"read": len(b)}))
	})
}

// The volume is a tmpfs of the local driver with the flags the settings table gives a secret
// mount point, named and labelled for the task; the container is given it read-only at
// /agk/secrets and nothing of the host there; and it is removed after the container.
func TestASecretReachesTheContainerOnATmpfsVolumeOfItsOwn(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := secretsRunner(t, ref)
	task := taskWithASecret(ref)

	result, err := r.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running: %s", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the task ended %s, and its container exits non-zero where it cannot read its secret", result.State)
	}

	created := createdFor(r, task.ID)
	m, ok := created.Mount(SecretsDir)
	if !ok {
		t.Fatalf("nothing is mounted at %s: %+v", SecretsDir, created.HostConfig.Mounts)
	}
	if m.Type != docker.MountVolume || m.Source != secretsVolume(task.ID) || !m.ReadOnly {
		t.Errorf("%s is %+v, and it is the task's secrets volume, read-only", SecretsDir, m)
	}
	if m.VolumeOptions == nil || !m.VolumeOptions.NoCopy || m.VolumeOptions.DriverConfig == nil || m.VolumeOptions.DriverConfig.Options["type"] != "tmpfs" {
		t.Errorf("the mount carries %+v, and a volume the daemon creates for it is a tmpfs, with nothing of the image copied in", m.VolumeOptions)
	}
	for _, other := range created.HostConfig.Mounts {
		if other.Type == docker.MountBind && strings.HasPrefix(other.Target, SecretsDir) {
			t.Errorf("%s is bound from the host at %s", other.Source, other.Target)
		}
	}

	spec, ok := volumeSpecOf(r, task.ID)
	if !ok {
		t.Fatalf("no volume was created for the task")
	}
	if spec.Driver != "local" || spec.DriverOpts["type"] != "tmpfs" || spec.DriverOpts["device"] != "tmpfs" {
		t.Errorf("the volume is %+v, and it is a tmpfs of the local driver", spec)
	}
	options := strings.Split(spec.DriverOpts["o"], ",")
	for _, want := range []string{"noexec", "nosuid", "nodev", "mode=0700"} {
		if !slices.Contains(options, want) {
			t.Errorf("the tmpfs is mounted %v, without %s", options, want)
		}
	}
	if spec.Labels[LabelSecrets] != string(task.ID) {
		t.Errorf("the volume is labelled %v, and a sweep finds it by its task", spec.Labels)
	}

	if left := r.daemon.Volumes(); len(left) != 0 {
		t.Errorf("the volumes %v survived the task", left)
	}
	removed := r.daemon.Removed()
	if container, volume := slices.Index(removed, created.ID), slices.Index(removed, secretsVolume(task.ID)); container < 0 || volume < container {
		t.Errorf("the daemon was asked to remove %v, and the volume goes after the container that used it", removed)
	}
}

// volumeSpecOf is the spec the task's secrets volume was created with, read off the holder's
// mount, since the volume itself is gone once the task has ended.
func volumeSpecOf(r *runner, id agk.TaskID) (docker.VolumeSpec, bool) {
	for _, c := range r.daemon.Created() {
		if !dockertest.IsHolder(c) || c.Labels[LabelSecrets] != string(id) {
			continue
		}
		m, ok := c.Mount(SecretsDir)
		if !ok || m.VolumeOptions == nil || m.VolumeOptions.DriverConfig == nil {
			return docker.VolumeSpec{}, false
		}
		return docker.VolumeSpec{
			Name: m.Source, Driver: m.VolumeOptions.DriverConfig.Name,
			DriverOpts: m.VolumeOptions.DriverConfig.Options, Labels: m.VolumeOptions.Labels,
		}, true
	}
	return docker.VolumeSpec{}, false
}

// The holder is the helper and nothing of the image: the helper read-only, the volume
// writable, no network, every capability dropped, a root filesystem it cannot write, and no
// label a redelivery or a stop would take it for the task's own container by. It is removed
// once the task's container has started, and not before, since the volume is emptied when
// no running container holds it.
func TestTheHolderIsTheHelperAloneAndGoesOnceTheContainerHasStarted(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := secretsRunner(t, ref)
	task := taskWithASecret(ref)

	if _, err := r.Run(t.Context(), task); err != nil {
		t.Fatalf("running: %s", err)
	}

	var holders []dockertest.Container
	for _, c := range r.daemon.Created() {
		if dockertest.IsHolder(c) {
			holders = append(holders, c)
		}
	}
	if len(holders) != 1 {
		t.Fatalf("%d holders were created, and a task given secrets has one", len(holders))
	}
	h := holders[0]
	if !slices.Equal(append(h.Config.Entrypoint, h.Config.Cmd...), HolderCommand) || h.Config.Image != ref {
		t.Errorf("the holder runs %v %v in %s", h.Config.Entrypoint, h.Config.Cmd, h.Config.Image)
	}
	if h.Config.Healthcheck == nil || !slices.Equal(h.Config.Healthcheck.Test, []string{"NONE"}) {
		t.Errorf("the holder's health check is %+v, and the image's own would run image code beside the values", h.Config.Healthcheck)
	}
	if _, ok := h.Labels[LabelTask]; ok {
		t.Errorf("the holder carries %s, and a redelivery would adopt it as the task's container", LabelTask)
	}
	if h.HostConfig.NetworkMode != networkModeNone || !h.HostConfig.ReadonlyRootfs || !slices.Equal(h.HostConfig.CapDrop, []string{"ALL"}) {
		t.Errorf("the holder is confined as %+v", h.HostConfig)
	}
	if !slices.Contains(h.HostConfig.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("the holder's security options are %v", h.HostConfig.SecurityOpt)
	}
	if bin, ok := h.Mount(BinPath); !ok || !bin.ReadOnly || bin.Source != r.cfg.Policy.Helper {
		t.Errorf("the helper is mounted %+v, and it is %s read-only", bin, r.cfg.Policy.Helper)
	}
	if v, ok := h.Mount(SecretsDir); !ok || v.ReadOnly || v.Source != secretsVolume(task.ID) {
		t.Errorf("the volume is mounted in the holder as %+v, and it is the task's, writable", v)
	}
	if len(h.HostConfig.Mounts) != 2 {
		t.Errorf("the holder is given %+v, and it is given the helper and the volume alone", h.HostConfig.Mounts)
	}

	removed := r.daemon.Removed()
	holder, container := slices.Index(removed, h.ID), slices.Index(removed, createdFor(r, task.ID).ID)
	if holder < 0 || container < 0 || holder > container {
		t.Errorf("the daemon was asked to remove %v, and the holder goes before the task's container", removed)
	}
}

// A task given no secret has no volume and no holder.
func TestATaskWithNoSecretHasNoSecretsVolume(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := newRunner(t, oneImage(ref, goodManifest), nil)
	if _, err := r.Run(t.Context(), oneTask(ref)); err != nil {
		t.Fatalf("running: %s", err)
	}
	for _, c := range r.daemon.Created() {
		if dockertest.IsHolder(c) {
			t.Errorf("a holder was created for a task given no secret")
		}
		if _, ok := c.Mount(SecretsDir); ok {
			t.Errorf("a task given no secret was given %s", SecretsDir)
		}
	}
}

// The helper is what fills the volume, so a runner with none refuses a task given a secret,
// on its own account and before anything is created, rather than run it without its values.
func TestASecretWithNoHelperIsRefusedBeforeAnythingIsCreated(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := secretsRunner(t, ref)
	r.cfg.Policy.Helper = ""

	_, err := r.Run(t.Context(), taskWithASecret(ref))
	runnersOwn(t, err, "a secret with no helper to fill its volume")
	if err != nil && !strings.Contains(err.Error(), BinPath) {
		t.Errorf("the refusal does not name the helper: %s", err)
	}
	if created := r.daemon.Created(); len(created) != 0 {
		t.Errorf("%d containers were created", len(created))
	}
	if left := r.daemon.Volumes(); len(left) != 0 {
		t.Errorf("the volumes %v were created", left)
	}
}

// A volume already under the task's name is taken over only where it is a tmpfs this task
// made. One on the daemon's disk would put the value on a disk, and it is refused before
// anything is written on it.
func TestAVolumeUnderTheTasksNameThatIsNotItsTmpfsIsRefused(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := secretsRunner(t, ref)
	task := taskWithASecret(ref)

	for _, spec := range []docker.VolumeSpec{
		{Name: secretsVolume(task.ID), Labels: map[string]string{LabelSecrets: string(task.ID)}},
		{Name: secretsVolume(task.ID), DriverOpts: map[string]string{"type": "tmpfs", "device": "tmpfs"}},
	} {
		if _, err := r.cli.VolumeCreate(t.Context(), spec); err != nil {
			t.Fatal(err)
		}
		_, err := r.Run(t.Context(), task)
		runnersOwn(t, err, "a volume that is not the task's tmpfs")
		for _, c := range r.daemon.Created() {
			if dockertest.IsHolder(c) || c.Labels[LabelTask] == string(task.ID) {
				t.Errorf("a container was created on %+v: %v %v", spec, c.Config.Entrypoint, c.Config.Cmd)
			}
		}
		if err := r.cli.VolumeRemove(t.Context(), spec.Name); err != nil && !docker.IsNotFound(err) {
			t.Fatal(err)
		}
	}
}

// A container a delivery created and never started is adopted by the next one, and its
// secrets volume, emptied when the holder that filled it went with the first delivery, is
// filled again before the container starts: the brick finds its value where the contract puts
// it.
func TestAnAdoptedContainerThatNeverStartedIsGivenItsValuesAgain(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := secretsRunner(t, ref)
	task := taskWithASecret(ref)
	container, _ := stageFirstDelivery(t, r, task)

	result, err := r.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("the redelivery: %s", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the adopted container ended %s, and it exits non-zero where its secret is not on its volume", result.State)
	}
	if got := createdFor(r, task.ID).ID; got != container {
		t.Fatalf("the redelivery ran %s, and it adopts %s", got, container)
	}
}

// A runner that died leaves a holder and a volume behind for each task it was filling or
// running, and the next one's sweep takes away what no container uses and nothing live could
// be about to use. A volume a container still names stays, since the delivery that adopts
// that container needs it, and so does one made a moment ago.
func TestTheSweepTakesAwayTheSecretsVolumesAndHoldersARunnerLeft(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := secretsRunner(t, ref)
	ctx := t.Context()

	left := taskWithASecret(ref)
	left.ID = agk.NewTaskID(left.Run, "left", 1, agk.Shard{})
	hold, _, err := r.fillSecrets(ctx, left, ref, []secretFile{{Name: "bearer", Value: []byte("s3cr3t-value")}})
	if err != nil {
		t.Fatalf("filling: %s", err)
	}
	// The runner died holding it: its end of the attach goes, the holder's standard input
	// ends and it exits, a moment ago, and nobody removes it.
	diedHolding(t, r, hold)

	adopted := taskWithASecret(ref)
	adopted.ID = agk.NewTaskID(adopted.Run, "adopted", 1, agk.Shard{})
	stageFirstDelivery(t, r, adopted)

	young := taskWithASecret(ref)
	young.ID = agk.NewTaskID(young.Run, "young", 1, agk.Shard{})
	if _, err := r.cli.VolumeCreate(ctx, docker.VolumeSpec{Name: secretsVolume(young.ID), Labels: map[string]string{LabelSecrets: string(young.ID)}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{secretsVolume(left.ID), secretsVolume(adopted.ID)} {
		if !r.daemon.Backdate(name, time.Hour) {
			t.Fatalf("no volume %s to backdate", name)
		}
	}

	if err := r.Sweep(ctx); err != nil {
		t.Fatalf("sweeping: %s", err)
	}

	volumes := r.daemon.Volumes()
	if slices.Contains(volumes, secretsVolume(left.ID)) {
		t.Errorf("the volume a dead runner was filling survived the sweep: %v", volumes)
	}
	for _, stays := range []string{secretsVolume(adopted.ID), secretsVolume(young.ID)} {
		if !slices.Contains(volumes, stays) {
			t.Errorf("the sweep took %s away: %v", stays, volumes)
		}
	}
	for _, c := range r.daemon.Created() {
		if dockertest.IsHolder(c) && c.Labels[LabelSecrets] == string(left.ID) && !slices.Contains(r.daemon.Removed(), c.ID) {
			t.Errorf("the holder a dead runner left survived the sweep")
		}
	}
}

// diedHolding is a runner that dies while its holder holds a volume: the attach goes with it,
// the holder's standard input ends, and the holder exits and stays.
func diedHolding(t *testing.T, r *runner, h *holder) {
	t.Helper()
	h.stream.Close()
	h.once.Do(func() {})
	deadline := time.Now().Add(5 * time.Second)
	for {
		in, err := r.cli.ContainerInspect(t.Context(), h.id)
		if err == nil && !in.State.Running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the holder did not exit when its standard input ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A runner that died between filling a volume and starting the task's container leaves the
// holder it filled with, exited and naming the volume. The redelivery that adopts the
// container takes both away with the task, rather than being refused the volume's removal by
// a container nothing would remove before the runner's next start.
func TestAHolderARunnerLeftGoesWithTheRedeliveredTask(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := secretsRunner(t, ref)
	task := taskWithASecret(ref)
	_, _, hold := stageHeld(t, r, task)
	diedHolding(t, r, hold)

	result, err := r.Run(t.Context(), task)
	if err != nil || result.State != agk.TaskSucceeded {
		t.Fatalf("the redelivery ended %s: %v", result.State, err)
	}
	if left := r.daemon.Volumes(); len(left) != 0 {
		t.Errorf("the volumes %v survived the task", left)
	}
	if !slices.Contains(r.daemon.Removed(), hold.id) {
		t.Errorf("the holder the first delivery left survived the task")
	}
}

// The command the holder runs is spelled in three places, the driver, the helper and the fake
// daemon that plays the helper, and they are one spelling.
func TestTheHolderCommandIsTheOneTheFakeDaemonPlays(t *testing.T) {
	if !slices.Equal(HolderCommand, dockertest.HolderCommand) {
		t.Errorf("the driver runs %v and the fake daemon plays %v", HolderCommand, dockertest.HolderCommand)
	}
	if HolderCommand[0] != BinPath {
		t.Errorf("the holder runs %s, and the helper is mounted at %s", HolderCommand[0], BinPath)
	}
}

// The same runner dying before it created the task's container at all leaves the holder and
// the volume and nothing to adopt: the redelivery creates the container afresh and still takes
// the holder away with the volume.
func TestAHolderARunnerLeftBeforeTheContainerGoesWithTheTask(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := secretsRunner(t, ref)
	task := taskWithASecret(ref)
	hold, _, err := r.fillSecrets(t.Context(), task, ref, []secretFile{{Name: "bearer", Value: []byte("s3cr3t-value")}})
	if err != nil {
		t.Fatalf("filling: %s", err)
	}
	diedHolding(t, r, hold)

	result, err := r.Run(t.Context(), task)
	if err != nil || result.State != agk.TaskSucceeded {
		t.Fatalf("the redelivery ended %s: %v", result.State, err)
	}
	if left := r.daemon.Volumes(); len(left) != 0 {
		t.Errorf("the volumes %v survived the task", left)
	}
	if !slices.Contains(r.daemon.Removed(), hold.id) {
		t.Errorf("the holder the first delivery left survived the task")
	}
}
