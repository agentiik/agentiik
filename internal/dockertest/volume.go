package dockertest

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agentiik/agentiik/internal/docker"
)

// volume is one volume this daemon holds. Files is what is on it, and it is kept only while
// a running container has it mounted, as a tmpfs volume keeps what is written on it: the
// daemon unmounts it when the last container lets go, and everything on it goes with the
// mount. Every volume here is modelled that way, which is the only kind the driver makes.
type volume struct {
	spec    docker.VolumeSpec
	created time.Time
	files   map[string][]byte
	mounted int
}

// HolderCommand is what a container that fills a secrets volume runs, which this daemon
// plays itself rather than handing to the test's function: the static helper told to write
// the values on its standard input under /agk/secrets and to wait for its standard input
// to end. The driver names it in driver.HolderCommand, and a test there holds the two to
// each other.
var HolderCommand = []string{"/agk/bin/agk", "hold-secrets"}

// secretName is the one-segment file name a value is written under.
var secretName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// isHolder says whether a container is one this daemon plays as the helper filling a volume.
func isHolder(c docker.Config) bool {
	command := append(append([]string(nil), c.Entrypoint...), c.Cmd...)
	return len(command) == len(HolderCommand) && command[0] == HolderCommand[0] && command[1] == HolderCommand[1]
}

// volumeCreate records one volume, and answers with the one already there under its name
// whatever the create asked for, as a daemon does.
func (d *Daemon) volumeCreate(w http.ResponseWriter, r *http.Request) {
	var spec docker.VolumeSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if spec.Driver == "" {
		spec.Driver = "local"
	}
	d.mu.Lock()
	v, ok := d.volumes[spec.Name]
	if !ok {
		v = &volume{spec: spec, created: time.Now().UTC()}
		d.volumes[spec.Name] = v
	}
	answer := v.describe()
	d.mu.Unlock()
	writeJSON(w, http.StatusCreated, answer)
}

// describe is a volume as the daemon answers it. The lock is held.
func (v *volume) describe() docker.Volume {
	return docker.Volume{
		Name: v.spec.Name, Driver: v.spec.Driver, Options: v.spec.DriverOpts,
		Labels: v.spec.Labels, CreatedAt: v.created,
	}
}

// volumeList answers with what is there, filtered by label where the query asked.
func (d *Daemon) volumeList(w http.ResponseWriter, r *http.Request) {
	wanted := labelFilter(r)
	d.mu.Lock()
	defer d.mu.Unlock()
	list := []docker.Volume{}
	for _, name := range d.volumeNames() {
		v := d.volumes[name]
		if matchesLabels(v.spec.Labels, wanted) {
			list = append(list, v.describe())
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"Volumes": list})
}

// volumeRemove removes one volume and records that it was removed. A volume a container this
// daemon holds names, created, running or exited, is refused with the 409 a daemon answers.
func (d *Daemon) volumeRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	d.mu.Lock()
	_, ok := d.volumes[name]
	used := ok && d.volumeUsed(name)
	if ok && !used {
		delete(d.volumes, name)
		d.removed = append(d.removed, name)
	}
	d.mu.Unlock()
	switch {
	case !ok:
		writeError(w, http.StatusNotFound, "get "+name+": no such volume")
	case used:
		writeError(w, http.StatusConflict, "remove "+name+": volume is in use")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// Volumes is the names of the volumes this daemon holds, sorted.
func (d *Daemon) Volumes() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.volumeNames()
}

// Volume answers with the spec a volume was created with, and whether there is one.
func (d *Daemon) Volume(name string) (docker.VolumeSpec, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.volumes[name]
	if !ok {
		return docker.VolumeSpec{}, false
	}
	return v.spec, true
}

// volumeNames is the names of the volumes, sorted. The lock is held.
func (d *Daemon) volumeNames() []string {
	names := make([]string, 0, len(d.volumes))
	for name := range d.volumes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// volumeUsed says whether a container this daemon holds names a volume. The lock is held.
func (d *Daemon) volumeUsed(name string) bool {
	for _, l := range d.containers {
		for _, m := range l.host.Mounts {
			if m.Type == docker.MountVolume && m.Source == name {
				return true
			}
		}
	}
	return false
}

// createVolumes makes the volumes a create names that are not there yet, as a daemon does,
// with the driver and the options the mount carries. The lock is held.
func (d *Daemon) createVolumes(mounts []docker.Mount) {
	for _, m := range mounts {
		if m.Type != docker.MountVolume || m.Source == "" {
			continue
		}
		if _, ok := d.volumes[m.Source]; ok {
			continue
		}
		spec := docker.VolumeSpec{Name: m.Source, Driver: "local"}
		if o := m.VolumeOptions; o != nil {
			spec.Labels = o.Labels
			if o.DriverConfig != nil {
				if o.DriverConfig.Name != "" {
					spec.Driver = o.DriverConfig.Name
				}
				spec.DriverOpts = o.DriverConfig.Options
			}
		}
		d.volumes[m.Source] = &volume{spec: spec, created: time.Now().UTC()}
	}
}

// mountVolumes mounts a container's volumes as it starts, once, and answers with what is on
// them now, by volume and file, which is what the container reads.
func (d *Daemon) mountVolumes(l *live) map[string]map[string][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	seen := map[string]map[string][]byte{}
	for _, m := range l.host.Mounts {
		v, ok := d.volumes[m.Source]
		if m.Type != docker.MountVolume || !ok {
			continue
		}
		if !l.holding {
			v.mounted++
		}
		files := map[string][]byte{}
		for name, b := range v.files {
			files[name] = append([]byte(nil), b...)
		}
		seen[m.Source] = files
	}
	l.holding = true
	return seen
}

// unmountVolumes lets go of a container's volumes as it exits or is removed, once, and
// empties each one no running container has mounted any longer.
func (d *Daemon) unmountVolumes(l *live) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !l.holding {
		return
	}
	l.holding = false
	for _, m := range l.host.Mounts {
		v, ok := d.volumes[m.Source]
		if m.Type != docker.MountVolume || !ok {
			continue
		}
		v.mounted--
		if v.mounted <= 0 {
			v.mounted, v.files = 0, nil
		}
	}
}

// hold is the helper filling a secrets volume, played by this daemon: it reads the values off
// standard input as a tar stream, writes them on the volume mounted writable at
// /agk/secrets, says ready, and waits for its standard input to end.
func (d *Daemon) hold(l *live, c Container) (int, error) {
	if _, ok := c.Mount(HolderCommand[0]); !ok {
		fmt.Fprintf(c.Stderr, "exec %s: no such file or directory\n", HolderCommand[0])
		return 127, nil
	}
	m, ok := c.Mount("/agk/secrets")
	if !ok || m.Type != docker.MountVolume || m.ReadOnly {
		fmt.Fprintln(c.Stderr, "agk hold-secrets: /agk/secrets is not a writable volume")
		return 1, nil
	}
	r := tar.NewReader(c.Stdin)
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintf(c.Stderr, "agk hold-secrets: the values did not arrive as a tar stream: %v\n", err)
			return 1, nil
		}
		if h.Typeflag != tar.TypeReg || !secretName.MatchString(h.Name) {
			fmt.Fprintf(c.Stderr, "agk hold-secrets: %q is not a value\n", h.Name)
			return 1, nil
		}
		b, err := io.ReadAll(r)
		if err != nil {
			return 1, err
		}
		d.mu.Lock()
		if v, ok := d.volumes[m.Source]; ok && v.mounted > 0 {
			if v.files == nil {
				v.files = map[string][]byte{}
			}
			v.files[h.Name] = b
		}
		d.mu.Unlock()
	}
	fmt.Fprintln(c.Stdout, "ready")
	io.Copy(io.Discard, c.Stdin)
	return 0, nil
}

// volumeArchive answers a read of a volume a running container has mounted, as a tar of what
// is on it, and false where the path is not such a volume.
func (d *Daemon) volumeArchive(w http.ResponseWriter, l *live, wanted string) bool {
	l.mu.Lock()
	running := l.started && !l.exited
	l.mu.Unlock()
	var source string
	for _, m := range l.host.Mounts {
		if m.Type == docker.MountVolume && path.Clean(m.Target) == path.Clean(wanted) {
			source = m.Source
		}
	}
	if source == "" {
		return false
	}
	d.mu.Lock()
	files := map[string][]byte{}
	if v, ok := d.volumes[source]; ok && running {
		for name, b := range v.files {
			files[name] = append([]byte(nil), b...)
		}
	}
	d.mu.Unlock()

	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	tw := tar.NewWriter(w)
	base := path.Base(wanted)
	tw.WriteHeader(&tar.Header{Name: base + "/", Mode: 0o555, Typeflag: tar.TypeDir})
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		tw.WriteHeader(&tar.Header{Name: base + "/" + name, Mode: 0o444, Size: int64(len(files[name])), Typeflag: tar.TypeReg})
		tw.Write(files[name])
	}
	tw.Close()
	return true
}

// ReadFile reads one path of the container as it was given it: through a bind, off the host,
// and on a volume, what was on it when the container started.
func (c Container) ReadFile(target string) ([]byte, error) {
	var best docker.Mount
	for _, m := range c.HostConfig.Mounts {
		if (target == m.Target || strings.HasPrefix(target, m.Target+"/")) && len(m.Target) > len(best.Target) {
			best = m
		}
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(target, best.Target), "/")
	switch best.Type {
	case docker.MountBind:
		return readHostFile(best.Source, rel)
	case docker.MountVolume:
		if b, ok := c.volumes[best.Source][rel]; ok {
			return b, nil
		}
		return nil, fmt.Errorf("%s: no such file on the volume %s", target, best.Source)
	}
	return nil, fmt.Errorf("%s: nothing is mounted there", target)
}

// readHostFile reads a file under a directory of the host, or the file itself where rel is
// empty.
func readHostFile(source, rel string) ([]byte, error) {
	if rel == "" {
		return os.ReadFile(source)
	}
	return os.ReadFile(filepath.Join(source, filepath.FromSlash(rel)))
}

// IsHolder says whether a container is one this daemon plays as the helper filling a volume,
// which a test's own function is never handed.
func IsHolder(c Container) bool { return isHolder(c.Config) }
