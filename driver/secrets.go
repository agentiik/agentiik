package driver

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
)

// LabelSecrets marks the volume a task's secret values are on, and the container that holds
// it while it is filled, with the task identifier. It is not LabelTask: a container carrying
// that label is one a redelivery adopts and a Stop reaches, and the holder is neither.
const LabelSecrets = "dev.agentiik.secrets"

// secretsVolumePrefix marks a volume as this driver's, as networkPrefix marks a network.
const secretsVolumePrefix = "agk-secrets-"

// The volume a task's secret values are on is a local volume of type tmpfs, which the
// daemon mounts from memory when a container is given it and unmounts, with everything on
// it, when the last container using it lets go. It is the one tmpfs a container can be
// given already filled: a tmpfs mount of the container's own is empty when its first
// instruction runs. A value on it never touches a disk, on the runner's host or anywhere
// else, and no path of the host names it, so the host needs nothing prepared for it.
const (
	volumeDriver = "local"
	volumeTmpfs  = "tmpfs"
)

// HolderCommand is what the container that holds a task's secrets volume while it is
// filled runs: the static helper, at the path a script step is given it at, told to write
// the values it reads on standard input under /agk/secrets and to wait for its standard
// input to end.
var HolderCommand = []string{BinPath, "hold-secrets"}

// holdReady is the line the holder writes once every value is on the volume.
const holdReady = "ready"

// holdGrace is how long the holder is given to say it has written the values. It writes a
// few files on a tmpfs, so a holder that has said nothing in this long is one that is not
// going to.
const holdGrace = 30 * time.Second

// The holder's own resources. It is the helper reading a few kilobytes off standard input,
// and a Go program's threads count against a pids limit, so the limit leaves it room for
// its runtime and for nothing else.
const (
	holderMemory = 64 << 20
	holderPids   = 64
)

// secretFile is one value and the name of the file it is written in on the volume: the last
// segment of the mount point the manifest asked for it at.
type secretFile struct {
	Name  string
	Value []byte
}

// secretsVolume is the name of a task's secrets volume: derived from the task identifier and
// never minted, so that a redelivery names the volume its first delivery made, which the
// container it adopts is created with.
func secretsVolume(id agk.TaskID) string {
	name := secretsVolumePrefix + safeName(string(id))
	const max = 64
	if len(name) > max {
		sum := sha256.Sum256([]byte(id))
		name = name[:max-9] + "-" + hex.EncodeToString(sum[:4])
	}
	return name
}

// secretsLabels is what the volume and its holder carry: the task, for a sweep to find what a
// runner that died left and for a person reading docker volume ls to tell what made it.
func secretsLabels(t graph.Task) map[string]string {
	l := map[string]string{LabelSecrets: string(t.ID)}
	if t.Namespace != "" {
		l[LabelNamespace] = t.Namespace
	}
	if t.Step != "" {
		l[LabelStep] = string(t.Step)
	}
	return l
}

// volumeOptions are the local driver's options for a task's secrets volume.
//
// The flags are the ones the settings table gives a secret mount point, noexec, nosuid and
// nodev. The size is what the values take and room for the pages they are counted in, since
// tmpfs pages are host memory. The directory is 0700 while it is filled and belongs to the
// account the holder writes as, root inside the container, which a remapped daemon maps to
// the base of its range: without uid and gid the tmpfs belongs to the host's own root, whom a
// remapped container cannot write as.
func volumeOptions(files []secretFile, uid, gid int, remapped bool) map[string]string {
	total := valuesSize(files)
	// A megabyte over the values, and a page of the largest size a kernel uses for each,
	// so that a value of any size fits whatever the page size is.
	size := total + int64(len(files))*(64<<10) + (1 << 20)
	o := []string{"size=" + strconv.FormatInt(size, 10), "mode=0700", "noexec", "nosuid", "nodev"}
	if remapped {
		o = append(o, "uid="+strconv.Itoa(uid), "gid="+strconv.Itoa(gid))
	}
	return map[string]string{"type": volumeTmpfs, "device": volumeTmpfs, "o": strings.Join(o, ",")}
}

// valuesSize is what the values take together.
func valuesSize(files []secretFile) int64 {
	var total int64
	for _, f := range files {
		total += int64(len(f.Value))
	}
	return total
}

// secretsMount is the task's secrets volume at /agk/secrets, read-only.
//
// NoCopy keeps whatever the image has at /agk/secrets out of it. The driver's configuration
// travels with the mount as well as with the create, because a daemon that finds no volume
// of the name a container is created with creates one, and one created without it is a
// directory on the daemon's disk.
func secretsMount(t graph.Task, options map[string]string, readOnly bool) docker.Mount {
	return docker.Mount{
		Type:     docker.MountVolume,
		Source:   secretsVolume(t.ID),
		Target:   SecretsDir,
		ReadOnly: readOnly,
		VolumeOptions: &docker.VolumeOptions{
			NoCopy:       true,
			Labels:       secretsLabels(t),
			DriverConfig: &docker.VolumeDriver{Name: volumeDriver, Options: options},
		},
	}
}

// holder is the container that keeps a task's secrets volume mounted, and so filled, from the
// moment the values are written until the task's own container has started and holds it in
// its place.
type holder struct {
	d      *Docker
	t      graph.Task
	id     string
	stream *docker.Stream
	once   sync.Once
}

// fillSecrets makes the task's secrets volume, starts a holder on it and hands it the values,
// and answers once they are all written, with the holder that is keeping them there and the
// mount the task's container is to be given.
//
// A volume already under the task's name is taken over only where it is the volume this
// driver makes: a tmpfs of the local driver carrying this task's label. A volume that is any
// less, a directory on the daemon's disk that somebody created under the name, would put the
// values on a disk, and it is refused before anything is written.
//
// The holder runs the task's own image, which the daemon already has, as root inside it with
// nothing but the helper and the volume mounted, no network, every capability dropped and a
// root filesystem it cannot write. Nothing of the image runs: the entry point is the helper.
func (d *Docker) fillSecrets(ctx context.Context, t graph.Task, image string, files []secretFile) (*holder, docker.Mount, error) {
	uid, gid, remapped := d.currentFloor().ownership()
	options := volumeOptions(files, uid, gid, remapped)
	name := secretsVolume(t.ID)
	v, err := d.cli.VolumeCreate(ctx, docker.VolumeSpec{
		Name: name, Driver: volumeDriver, DriverOpts: options, Labels: secretsLabels(t),
	})
	if err != nil {
		return nil, docker.Mount{}, fault(t.Step, err, ChargePlatform, "the secrets volume %s could not be created, and no secret value was written", name)
	}
	if v.Driver != volumeDriver || v.Options["type"] != volumeTmpfs || v.Options["device"] != volumeTmpfs || v.Labels[LabelSecrets] != string(t.ID) {
		return nil, docker.Mount{}, fault(t.Step, nil, ChargePlatform, "a volume named %s is already on this daemon and is not this task's tmpfs, so no secret value was written on it: a value on a volume that is not a tmpfs is on a disk. The runner's next start sweeps it once no container uses it", name)
	}

	helper, err := helperMount(t, d.cfg.Policy)
	if err != nil {
		return nil, docker.Mount{}, err
	}
	config := docker.Config{
		Image:      image,
		Entrypoint: HolderCommand[:1],
		Cmd:        HolderCommand[1:],
		User:       "0:0",
		WorkingDir: "/",
		Labels:     secretsLabels(t),
		// The image's own health check would run image code in the holder, as root
		// and with the volume writable, and the daemon keeps what a check prints on
		// its disk.
		Healthcheck:  &docker.HealthConfig{Test: []string{"NONE"}},
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		OpenStdin:    true,
		StdinOnce:    true,
	}
	pids := int64(holderPids)
	host := docker.HostConfig{
		NetworkMode:    networkModeNone,
		Mounts:         []docker.Mount{secretsMount(t, options, false), helper},
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    securityOptions(d.cfg.Policy),
		// The tmpfs pages the holder writes are counted against its memory.
		Resources: docker.Resources{Memory: holderMemory + valuesSize(files), PidsLimit: &pids},
	}
	created, err := d.cli.ContainerCreate(ctx, "", config, host, docker.NetworkingConfig{})
	if err != nil {
		return nil, docker.Mount{}, fault(t.Step, err, ChargePlatform, "the container that fills the secrets volume could not be created, and no secret value was written")
	}
	h := &holder{d: d, t: t, id: created.ID}

	fail := func(err error) (*holder, docker.Mount, error) {
		h.release(ctx)
		return nil, docker.Mount{}, err
	}
	h.stream, err = d.cli.ContainerAttach(ctx, created.ID, docker.AttachOptions{Stdin: true, Stdout: true, Stderr: true, Stream: true})
	if err != nil {
		return fail(fault(t.Step, err, ChargePlatform, "attaching to the container that fills the secrets volume"))
	}
	if err := d.cli.ContainerStart(ctx, created.ID); err != nil {
		return fail(fault(t.Step, err, ChargePlatform, "the container that fills the secrets volume could not be started"))
	}
	if err := h.write(ctx, files); err != nil {
		return fail(err)
	}
	return h, secretsMount(t, options, true), nil
}

// helperMount is the static helper bound read-only at BinPath, for the holder.
func helperMount(t graph.Task, p Policy) (docker.Mount, error) {
	if !helperIsFile(p.Helper) {
		return docker.Mount{}, fault(t.Step, nil, ChargePlatform, "%s is named as the static helper and is not a file on this host: it fills a task's secrets volume, and a path that is not there would be bound as a directory", p.Helper)
	}
	return bind(p.Helper, BinPath, true), nil
}

// write hands the holder the values as a tar stream on its standard input, and waits for it
// to say they are written. Standard input stays open: its end is what lets the holder go.
func (h *holder) write(ctx context.Context, files []secretFile) error {
	fail := func(format string, a ...any) error {
		return fault(h.t.Step, nil, ChargePlatform, "the secrets volume could not be filled: "+format, a...)
	}

	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.Name, Mode: 0o400, Size: int64(len(f.Value)), Typeflag: tar.TypeReg}); err != nil {
			return fail("%v", err)
		}
		if _, err := tw.Write(f.Value); err != nil {
			return fail("%v", err)
		}
	}
	if err := tw.Close(); err != nil {
		return fail("%v", err)
	}

	said := make(chan error, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		for {
			frame, err := h.stream.Next()
			if err != nil {
				said <- fmt.Errorf("the holder ended before it said the values were written: %s", strings.TrimSpace(stderr.String()))
				return
			}
			switch frame.Stream {
			case docker.Stdout:
				stdout.Write(frame.Bytes)
				if line, _, whole := strings.Cut(stdout.String(), "\n"); whole && line == holdReady {
					said <- nil
					return
				}
			case docker.Stderr:
				stderr.Write(frame.Bytes)
			}
		}
	}()
	go h.stream.Stdin.Write(archive.Bytes())

	select {
	case err := <-said:
		if err != nil {
			return fail("%v", err)
		}
		return nil
	case <-ctx.Done():
		h.stream.Close()
		return fault(h.t.Step, ctx.Err(), ChargePlatform, "the secrets volume was being filled when the task's context ended")
	case <-time.After(holdGrace):
		h.stream.Close()
		return fail("the holder said nothing in %s", holdGrace)
	}
}

// release lets the holder go, once: its standard input is closed, which ends it, and it is
// removed. It is called when the task's container has started, and on every way out of a task
// whose container never did.
func (h *holder) release(ctx context.Context) {
	if h == nil {
		return
	}
	h.once.Do(func() {
		if h.stream != nil {
			h.stream.Close()
		}
		tidy, cancel := context.WithTimeout(context.WithoutCancel(ctx), removalGrace)
		defer cancel()
		if err := h.d.cli.ContainerRemove(tidy, h.id, true); err != nil && !docker.IsNotFound(err) {
			h.d.say(fmt.Sprintf("%s left the container that filled its secrets volume on this host, holding task %s's secret values in memory until the runner's next start sweeps it: %v", h.t.Step, h.t.ID, err))
		}
	})
}

// removeSecrets takes the task's secrets volume away, once no container uses it, and says
// what it could not take away. Said and not returned, for the reason tidy says a directory.
//
// A holder an earlier delivery left, one whose runner died before it let it go, is removed
// first: it has exited, since its standard input ended with that runner, and it still names
// the volume, which the daemon would otherwise refuse to remove. It belongs to this task,
// which this process holds.
func (d *Docker) removeSecrets(ctx context.Context, t graph.Task) {
	if len(t.Secrets) == 0 {
		return
	}
	tidy, cancel := context.WithTimeout(context.WithoutCancel(ctx), removalGrace)
	defer cancel()
	d.removeHolders(tidy, t)
	name := secretsVolume(t.ID)
	if err := d.cli.VolumeRemove(tidy, name); err != nil && !docker.IsNotFound(err) {
		d.say(fmt.Sprintf("%s left its secrets volume %s on this daemon, and the runner's next start sweeps it once no container uses it: %v", t.Step, name, err))
	}
}

// removeHolders removes every holder of the task's secrets volume, whichever delivery made it.
func (d *Docker) removeHolders(ctx context.Context, t graph.Task) {
	left, err := d.cli.ContainerList(ctx, docker.Filters{}.Add("label", LabelSecrets+"="+string(t.ID)))
	if err != nil {
		return
	}
	for _, c := range left {
		if err := d.cli.ContainerRemove(ctx, c.ID, true); err != nil && !docker.IsNotFound(err) {
			d.say(fmt.Sprintf("%s left a container that filled its secrets volume on this daemon, and it was not removed: %v", t.Step, err))
		}
	}
}

// written reads back the values on the secrets volume of a container that is running, for a
// delivery that adopted it. A volume is emptied when no container has it mounted, so a
// container that has exited, or never started, has none to read.
func (d *Docker) written(ctx context.Context, container string) map[string][]byte {
	rc, err := d.cli.ContainerArchive(ctx, container, SecretsDir)
	if err != nil {
		return nil
	}
	defer rc.Close()
	found := map[string][]byte{}
	r := tar.NewReader(io.LimitReader(rc, 16<<20))
	for {
		h, err := r.Next()
		if err != nil {
			return found
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name := path.Base(h.Name)
		if !secretMountPattern.MatchString(SecretsDir + "/" + name) {
			continue
		}
		value, err := io.ReadAll(r)
		if err != nil {
			return found
		}
		found[name] = value
	}
}

// ownSecretsVolume says whether a container has the task's own secrets volume at
// /agk/secrets, which is the one volume whose values are read back for the masker.
func ownSecretsVolume(t graph.Task, mounts []docker.MountPoint) bool {
	for _, m := range mounts {
		if m.Destination == SecretsDir {
			return m.Type == docker.MountVolume && m.Name == secretsVolume(t.ID)
		}
	}
	return false
}

// sweepSecrets removes the holders and the secrets volumes an earlier process left: a runner
// that died while a volume was being filled leaves the holder, which ends when its standard
// input does and stays as a container that has exited, and one that died before it removed a
// task's volume leaves the volume. A holder that has exited holds nothing, whatever its age,
// and is removed; a running one is another live process's filling unless it is older than
// sweepAge. A volume a container still uses is refused by the daemon and left, since a
// delivery that adopts that container will need it; one younger than sweepAge, or of a task
// this process holds, is another live process's, as sweepAge says of a network.
func (d *Docker) sweepSecrets(ctx context.Context) error {
	holders, err := d.cli.ContainerList(ctx, docker.Filters{}.Add("label", LabelSecrets))
	if err != nil {
		return fmt.Errorf("driver: the containers that filled secrets volumes an earlier process left could not be listed: %w", err)
	}
	left := d.now().Add(-sweepAge)
	for _, c := range holders {
		task := c.Labels[LabelSecrets]
		if d.lookup(agk.TaskID(task)) != nil || (c.State == "running" && time.Unix(c.Created, 0).After(left)) {
			continue
		}
		if err := d.cli.ContainerRemove(ctx, c.ID, true); err != nil && !docker.IsNotFound(err) {
			d.say(fmt.Sprintf("the container %s, left on this daemon filling the secrets volume of task %s, was not removed: %v", c.ID, task, err))
		}
	}

	volumes, err := d.cli.VolumeList(ctx, docker.Filters{}.Add("label", LabelSecrets))
	if err != nil {
		return fmt.Errorf("driver: the secrets volumes an earlier process left could not be listed: %w", err)
	}
	var removed []string
	for _, v := range volumes {
		task := v.Labels[LabelSecrets]
		if !strings.HasPrefix(v.Name, secretsVolumePrefix) || d.lookup(agk.TaskID(task)) != nil || v.CreatedAt.After(left) {
			continue
		}
		if err := d.cli.VolumeRemove(ctx, v.Name); err != nil {
			if !docker.IsNotFound(err) && !docker.IsConflict(err) {
				d.say(fmt.Sprintf("the secrets volume %s, left on this daemon by task %s, was not removed: %v", v.Name, task, err))
			}
			continue
		}
		removed = append(removed, v.Name)
	}
	if len(removed) > 0 {
		d.say("an earlier process left secrets volumes on this daemon that no container uses, and they were removed: " + strings.Join(removed, ", "))
	}
	return nil
}
