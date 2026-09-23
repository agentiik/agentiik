package dockertest

import (
	"archive/tar"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentiik/agentiik/internal/docker"
)

// Container is what a test's function is given: a container as it was created, with the
// three streams it is spoken on.
//
// Config and HostConfig are what came off the wire rather than what was handed to the
// client, which is the whole reason this daemon is on a real socket: a test asserting
// that CapDrop reads ALL is asserting about the bytes the daemon received.
type Container struct {
	ID   string
	Name string

	Config     docker.Config
	HostConfig docker.HostConfig
	Networking docker.NetworkingConfig
	Labels     map[string]string

	// Work is the host directory bound at /agk/out, which is where a container
	// writes what is collected from it. It is the working directory as the driver
	// prepared it, seen from the one end a container actually uses.
	Work string

	// The three streams. Stdin ends when the driver half-closes, which is what a
	// brick reading its envelope to the end waits for.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Signals are the signals delivered so far, in order, which is what a test
	// asserting SIGTERM then SIGKILL reads. It is empty in the copy a running
	// container is given, because nothing has been sent yet; the copy Created()
	// answers with carries them all.
	Signals []string

	signal <-chan string
}

// Signalled is what a container waits on to notice a stop. A container that ignores it
// is a container the grace runs out on, which is the case the escalation exists for.
func (c Container) Signalled() <-chan string { return c.signal }

// Env answers with one environment variable as the driver set it, and whether it was set
// at all. The difference matters: AGK_SHARD is absent without a fan-out rather than
// empty.
func (c Container) Env(name string) (string, bool) {
	for _, e := range c.Config.Env {
		if n, v, ok := strings.Cut(e, "="); ok && n == name {
			return v, true
		}
	}
	return "", false
}

// Mount answers with the mount at one target, which is how a test asks whether /agk/repo
// arrived and whether it arrived read-only.
func (c Container) Mount(target string) (docker.Mount, bool) {
	for _, m := range c.HostConfig.Mounts {
		if m.Target == target {
			return m, true
		}
	}
	return docker.Mount{}, false
}

// live is one container this daemon holds, and everything that happens to it.
type live struct {
	id     string
	name   string
	config docker.Config
	host   docker.HostConfig
	net    docker.NetworkingConfig

	rec *recorder

	signal chan string

	mu sync.Mutex

	// stdinR and stdinW are the standard input of the run under way, and a container
	// started again after it exited is given a fresh pair, as a daemon gives it.
	stdinR *io.PipeReader
	stdinW *io.PipeWriter

	// done closes when the run under way ends, or the next one where none is: it is
	// replaced as it closes, so a wait taken on a container that has exited waits for
	// the run a start would begin, which is what next-exit means to a daemon.
	done chan struct{}

	started    bool
	exited     bool
	code       int
	oom        bool
	signals    []string
	startedAt  time.Time
	finishedAt time.Time
}

// containerCreate records one container, exactly as it arrived.
func (d *Daemon) containerCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		docker.Config
		HostConfig       docker.HostConfig       `json:"HostConfig"`
		NetworkingConfig docker.NetworkingConfig `json:"NetworkingConfig"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "the create body did not read as JSON: "+err.Error())
		return
	}

	name := r.URL.Query().Get("name")
	d.mu.Lock()
	for _, other := range d.order {
		if name != "" && other.name == name {
			d.mu.Unlock()
			writeError(w, http.StatusConflict, "Conflict. The container name \"/"+name+"\" is already in use")
			return
		}
	}
	pr, pw := io.Pipe()
	l := &live{
		id:     newID(),
		name:   name,
		config: body.Config,
		host:   body.HostConfig,
		net:    body.NetworkingConfig,
		rec:    &recorder{live: !d.opts.exitsDuringAttach},
		stdinR: pr,
		stdinW: pw,
		signal: make(chan string, 8),
		done:   make(chan struct{}),
	}
	d.seq++
	d.containers[l.id] = l
	d.order = append(d.order, l)
	d.mu.Unlock()

	writeJSON(w, http.StatusCreated, docker.Created{ID: l.id})
}

// containerStart starts the container, which is to say it calls the test's function.
//
// A container that is running answers 304 and nothing happens. A container that has
// exited runs again, with the same configuration, the same mounts and a fresh standard
// input, and its log carries both runs: that is what a daemon does with POST /start on an
// exited container, and a fake that answered 304 there would let a driver starting a
// container that already did its work pass every test while running the brick twice.
func (d *Daemon) containerStart(w http.ResponseWriter, r *http.Request) {
	l := d.container(r.PathValue("id"))
	if l == nil {
		writeError(w, http.StatusNotFound, "No such container: "+r.PathValue("id"))
		return
	}

	if d.opts.daemonVanishes {
		d.mu.Lock()
		d.vanished = true
		d.mu.Unlock()
		if conn, _, err := hijack(w); err == nil {
			conn.Close()
		}
		return
	}

	l.mu.Lock()
	if l.started && !l.exited {
		l.mu.Unlock()
		writeError(w, http.StatusNotModified, "container already started")
		return
	}
	if l.exited {
		l.exited, l.code, l.oom = false, 0, false
		l.stdinR, l.stdinW = io.Pipe()
		l.rec.reopen()
	}
	l.started = true
	l.startedAt = time.Now().UTC()
	l.mu.Unlock()

	d.emit(docker.Event{
		Type: docker.EventTypeContainer, Action: docker.ActionStart,
		Actor: docker.EventActor{ID: l.id, Attributes: l.config.Labels},
		Time:  time.Now().Unix(), TimeNano: time.Now().UnixNano(),
	})

	if d.opts.exitsDuringAttach {
		// The container runs to completion before the answer to the start, and
		// nothing it wrote reaches the attached stream. What it wrote is in the
		// daemon's log, which is the reason AutoRemove is false.
		d.run(l)
		l.rec.close()
		w.WriteHeader(http.StatusNoContent)
		return
	}

	go d.run(l)
	w.WriteHeader(http.StatusNoContent)
}

// run calls the test's function and records what became of it.
func (d *Daemon) run(l *live) {
	code := 0
	var err error
	if d.opts.Run != nil {
		code, err = d.opts.Run(l.container())
	}
	if err != nil {
		// A function that failed is a container that could not run, which a
		// daemon reports as a non-zero exit rather than as an HTTP status: the
		// container did exist.
		fmt.Fprintf(l.rec.writer(docker.Stderr), "dockertest: the container function failed: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	if d.opts.oomKills {
		// 137 is 128 plus SIGKILL, which is what the kernel's out-of-memory
		// killer leaves behind and what the die event carries.
		l.exit(137, true)
	} else {
		l.exit(code, false)
	}
	// A real daemon closes the attached stream when the container exits, which is
	// what gives a reader of it an end. The log is what survives afterwards.
	l.rec.close()
	d.emitExit(l)
}

// emitExit writes the events an exit produces: an oom before the die where there was one,
// because the oom is the only place the reason is written and the wait never carries it.
func (d *Daemon) emitExit(l *live) {
	now := time.Now()
	l.mu.Lock()
	oom, code := l.oom, l.code
	l.mu.Unlock()

	if oom {
		d.emit(docker.Event{
			Type: docker.EventTypeContainer, Action: docker.ActionOOM,
			Actor: docker.EventActor{ID: l.id, Attributes: l.config.Labels},
			Time:  now.Unix(), TimeNano: now.UnixNano(),
		})
	}
	attributes := map[string]string{"exitCode": strconv.Itoa(code)}
	for k, v := range l.config.Labels {
		attributes[k] = v
	}
	d.emit(docker.Event{
		Type: docker.EventTypeContainer, Action: docker.ActionDie,
		Actor: docker.EventActor{ID: l.id, Attributes: attributes},
		Time:  now.Unix(), TimeNano: now.UnixNano() + 1,
	})
}

// containerAttach hijacks the connection and becomes the container's three streams.
func (d *Daemon) containerAttach(w http.ResponseWriter, r *http.Request) {
	l := d.container(r.PathValue("id"))
	if l == nil {
		writeError(w, http.StatusNotFound, "No such container: "+r.PathValue("id"))
		return
	}

	conn, _, err := hijack(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")

	l.rec.attach(conn)
	if r.URL.Query().Get("stdin") == "true" {
		// Standard input arrives raw on the same connection, and its end is the
		// client half-closing, which reaches this side as an ordinary EOF.
		l.mu.Lock()
		stdin := l.stdinW
		l.mu.Unlock()
		go func() {
			_, err := io.Copy(stdin, conn)
			stdin.CloseWithError(err)
		}()
	}
}

// containerWait answers the header at once and the exit later.
//
// Answering the header before the container has exited is the behaviour the whole
// ordering depends on: a caller that has read it knows the wait is registered and may
// start the container, which is what makes the exit-during-attach race unrepresentable.
//
// The condition is read as a daemon reads it. next-exit on a container that has already
// exited waits for the run a start would begin, and not-running, the default, answers at
// once on a container that is not running.
func (d *Daemon) containerWait(w http.ResponseWriter, r *http.Request) {
	l := d.container(r.PathValue("id"))
	if l == nil {
		writeError(w, http.StatusNotFound, "No such container: "+r.PathValue("id"))
		return
	}
	condition := r.URL.Query().Get("condition")

	l.mu.Lock()
	running := l.started && !l.exited
	done, code := l.done, l.code
	l.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	if !running && (condition == "" || condition == docker.WaitNotRunning) {
		json.NewEncoder(w).Encode(docker.Waited{StatusCode: code})
		return
	}

	select {
	case <-done:
	case <-r.Context().Done():
		return
	}
	if d.opts.oomKills {
		// The wait never answers on this one. The exit is on the event stream
		// and nowhere else, which is what the stream is followed for.
		<-r.Context().Done()
		return
	}

	l.mu.Lock()
	code = l.code
	l.mu.Unlock()
	json.NewEncoder(w).Encode(docker.Waited{StatusCode: code})
}

// containerLogs answers with everything the container wrote, framed as the attach frames
// it. It is what nothing being lost on a fast exit depends on.
func (d *Daemon) containerLogs(w http.ResponseWriter, r *http.Request) {
	l := d.container(r.PathValue("id"))
	if l == nil {
		writeError(w, http.StatusNotFound, "No such container: "+r.PathValue("id"))
		return
	}
	wantOut := r.URL.Query().Get("stdout") == "true"
	wantErr := r.URL.Query().Get("stderr") == "true"

	w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	w.WriteHeader(http.StatusOK)
	for _, f := range l.rec.recorded() {
		if (f.Stream == docker.Stdout && !wantOut) || (f.Stream == docker.Stderr && !wantErr) {
			continue
		}
		writeFrame(w, f.Stream, f.Bytes)
	}
}

// containerInspect is the backstop, and where the two moments a result reports are read.
func (d *Daemon) containerInspect(w http.ResponseWriter, r *http.Request) {
	l := d.container(r.PathValue("id"))
	if l == nil {
		writeError(w, http.StatusNotFound, "No such container: "+r.PathValue("id"))
		return
	}

	l.mu.Lock()
	state := docker.State{
		Status:     l.status(),
		Running:    l.started && !l.exited,
		OOMKilled:  l.oom,
		ExitCode:   l.code,
		StartedAt:  l.startedAt,
		FinishedAt: l.finishedAt,
	}
	l.mu.Unlock()

	config, host := l.config, l.host
	// The resolved mount list, which the daemon fills in whatever form the create
	// used and which is where a driver adopting a container reads what it was given.
	mounts := make([]docker.MountPoint, 0, len(host.Mounts))
	for _, m := range host.Mounts {
		mounts = append(mounts, docker.MountPoint{
			Type: m.Type, Source: m.Source, Destination: m.Target, RW: !m.ReadOnly,
		})
	}
	writeJSON(w, http.StatusOK, docker.Inspected{
		ID: l.id, Name: "/" + l.name, Image: l.config.Image,
		State: state, Config: &config, HostConfig: &host, Mounts: mounts,
	})
}

// containerList is adoption and the sweep: what is here, filtered by label.
func (d *Daemon) containerList(w http.ResponseWriter, r *http.Request) {
	wanted := labelFilter(r)

	d.mu.Lock()
	defer d.mu.Unlock()
	list := []docker.Summary{}
	for _, l := range d.order {
		if _, held := d.containers[l.id]; !held {
			continue
		}
		if !matchesLabels(l.config.Labels, wanted) {
			continue
		}
		l.mu.Lock()
		list = append(list, docker.Summary{
			ID: l.id, Names: []string{"/" + l.name}, Image: l.config.Image,
			State: l.status(), Labels: l.config.Labels,
		})
		l.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, list)
}

// containerStop is SIGTERM, then SIGKILL after t, as the daemon's own stop does it. The
// escalation is here rather than on the client's side, which is what makes it survive a
// runner dying between the two signals.
func (d *Daemon) containerStop(w http.ResponseWriter, r *http.Request) {
	l := d.container(r.PathValue("id"))
	if l == nil {
		writeError(w, http.StatusNotFound, "No such container: "+r.PathValue("id"))
		return
	}

	l.mu.Lock()
	running := l.started && !l.exited
	done := l.done
	l.mu.Unlock()
	// A container the daemon has not started has no process to signal, and a real
	// daemon answers a stop on one with 304 Not Modified and remembers nothing about
	// it: the start that follows runs it as though no stop had ever arrived. Modelled
	// here because it is the window a graph.Stop can land in, between the create and
	// the start of one task.
	if !running {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	grace := 10 * time.Second
	if t := r.URL.Query().Get("t"); t != "" {
		if seconds, err := strconv.Atoi(t); err == nil {
			grace = time.Duration(seconds) * time.Second
		}
	}

	l.deliver("SIGTERM")
	select {
	case <-done:
	case <-time.After(grace):
		l.deliver("SIGKILL")
		l.exit(137, false)
		d.emitExit(l)
	case <-r.Context().Done():
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// containerKill sends one signal, and SIGKILL ends the container where it stands.
func (d *Daemon) containerKill(w http.ResponseWriter, r *http.Request) {
	l := d.container(r.PathValue("id"))
	if l == nil {
		writeError(w, http.StatusNotFound, "No such container: "+r.PathValue("id"))
		return
	}
	signal := r.URL.Query().Get("signal")
	if signal == "" {
		signal = "SIGKILL"
	}
	l.deliver(signal)
	if signal == "SIGKILL" || signal == "KILL" || signal == "9" {
		if l.exit(137, false) {
			d.emitExit(l)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// containerRemove removes the container, and records that it was removed. Removed() is
// what makes the destruction an assertion rather than an assumption.
func (d *Daemon) containerRemove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	l := d.container(id)
	if l == nil {
		writeError(w, http.StatusNotFound, "No such container: "+id)
		return
	}

	l.mu.Lock()
	running := l.started && !l.exited
	l.mu.Unlock()
	if running && r.URL.Query().Get("force") != "true" {
		writeError(w, http.StatusConflict, "You cannot remove a running container "+id)
		return
	}

	d.mu.Lock()
	delete(d.containers, id)
	d.removed = append(d.removed, id)
	d.mu.Unlock()

	l.rec.close()
	w.WriteHeader(http.StatusNoContent)
}

// containerArchive reads one path out of the container, which is how /agk/brick.yaml is
// read out of an image through a container created from it and never started.
func (d *Daemon) containerArchive(w http.ResponseWriter, r *http.Request) {
	l := d.container(r.PathValue("id"))
	if l == nil {
		writeError(w, http.StatusNotFound, "No such container: "+r.PathValue("id"))
		return
	}
	wanted := r.URL.Query().Get("path")

	img, ok := d.opts.Images[l.config.Image]
	if !ok || len(img.Manifest) == 0 {
		writeError(w, http.StatusNotFound, "Could not find the file "+wanted+" in container "+l.id)
		return
	}

	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	tw := tar.NewWriter(w)
	tw.WriteHeader(&tar.Header{
		Name: path.Base(wanted), Mode: 0o444,
		Size: int64(len(img.Manifest)), Typeflag: tar.TypeReg,
	})
	tw.Write(img.Manifest)
	tw.Close()
}

// imageCreate is the pull: a stream of progress messages inside a 200 the daemon has
// already committed to.
func (d *Daemon) imageCreate(w http.ResponseWriter, r *http.Request) {
	ref := referenceOf(r)
	img, ok := d.opts.Images[ref]
	if !ok {
		writeError(w, http.StatusNotFound, "pull access denied for "+ref+", repository does not exist or may require 'docker login'")
		return
	}

	layers := img.Layers
	if layers <= 0 {
		layers = 2
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	send := func(p docker.Progress) {
		enc.Encode(p)
		if flusher != nil {
			flusher.Flush()
		}
	}

	for i := range layers {
		id := fmt.Sprintf("%08x", i)
		send(docker.Progress{ID: id, Status: "Pulling fs layer"})
		if d.opts.pullFailsHalfway && i == layers/2 {
			// Half the layers are already reported complete, inside a 200.
			// A caller reading the status and not the stream would take
			// this for a pull that worked.
			message := "failed to register layer: unexpected EOF"
			send(docker.Progress{Error: message, ErrorDetail: &docker.ErrorDetail{Message: message}})
			return
		}
		send(docker.Progress{ID: id, Status: "Download complete"})
	}
	send(docker.Progress{Status: "Status: Downloaded newer image for " + ref})

	// The pull finished, so the daemon holds the image now. A second resolve of the
	// same reference finds it and does not pull again, which is the daemon's own
	// behaviour and what the manifest cache is read against.
	d.mu.Lock()
	d.pulled[ref] = true
	d.mu.Unlock()
}

// hasPulled says whether a pull of this reference has completed on this daemon.
func (d *Daemon) hasPulled(ref string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pulled[ref]
}

// imageInspect answers with the image, which is where a reference becomes the digest it
// resolved to.
func (d *Daemon) imageInspect(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/images/"), "/json")
	img, ok := d.opts.Images[ref]
	if !ok {
		writeError(w, http.StatusNotFound, "No such image: "+ref)
		return
	}
	// An image the daemon does not hold yet is not there to be inspected, whatever
	// the registry behind it has. That is what makes the pull happen at all.
	if img.Remote && !d.hasPulled(ref) {
		writeError(w, http.StatusNotFound, "No such image: "+ref)
		return
	}

	digest := img.Digest
	if digest == "" {
		digest = "sha256:" + newID()
	}
	name, _, _ := strings.Cut(ref, "@")
	writeJSON(w, http.StatusOK, docker.Image{
		ID:           digest,
		RepoDigests:  []string{name + "@" + digest},
		Config:       img.Config,
		Architecture: "arm64",
		Os:           "linux",
	})
}

// referenceOf puts a pull's reference back together from the two parameters it travels
// in, where a digest travels in the tag position.
func referenceOf(r *http.Request) string {
	q := r.URL.Query()
	name, tag := q.Get("fromImage"), q.Get("tag")
	switch {
	case tag == "":
		return name
	case strings.Contains(tag, ":"):
		return name + "@" + tag
	default:
		return name + ":" + tag
	}
}

// container answers with one held container, or nothing.
func (d *Daemon) container(id string) *live {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.containers[id]
}

// container is the value the test's function is given.
func (l *live) container() Container {
	l.mu.Lock()
	signals := append([]string(nil), l.signals...)
	stdin := l.stdinR
	l.mu.Unlock()

	work := ""
	for _, m := range l.host.Mounts {
		if m.Target == "/agk/out" {
			work = m.Source
		}
	}
	return Container{
		ID: l.id, Name: l.name,
		Config: l.config, HostConfig: l.host, Networking: l.net,
		Labels: l.config.Labels, Work: work,
		Stdin:   stdin,
		Stdout:  l.rec.writer(docker.Stdout),
		Stderr:  l.rec.writer(docker.Stderr),
		Signals: signals,
		signal:  l.signal,
	}
}

// exit records the end of a run, once, and says whether this call was the one that did
// it.
func (l *live) exit(code int, oom bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.exited {
		return false
	}
	l.exited, l.code, l.oom = true, code, oom
	l.finishedAt = time.Now().UTC()

	l.stdinR.Close()
	close(l.done)
	l.done = make(chan struct{})
	return true
}

// deliver records a signal and hands it to the container.
func (l *live) deliver(signal string) {
	l.mu.Lock()
	l.signals = append(l.signals, signal)
	l.mu.Unlock()
	select {
	case l.signal <- signal:
	default:
		// A container that is not listening is a container the grace runs out
		// on, which is the case the escalation exists for.
	}
}

// status is the word the daemon uses for where a container is. It is called with the
// lock held.
func (l *live) status() string {
	switch {
	case l.exited:
		return "exited"
	case l.started:
		return "running"
	default:
		return "created"
	}
}

// recorder keeps everything a container wrote and, while one is attached, writes it to
// the hijacked connection as well.
//
// Both at once is what a real daemon does: the attach carries the stream live and the
// log carries it afterwards, and the second is why nothing is lost on a fast exit.
type recorder struct {
	live bool

	mu      sync.Mutex
	frames  []docker.Frame
	conn    net.Conn
	dropped bool
	closed  bool
}

func (r *recorder) attach(conn net.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		// The container is already over. A stream attached to it has nothing
		// to carry, and an end is what its reader needs.
		conn.Close()
		return
	}
	r.conn = conn
}

func (r *recorder) writer(s docker.StdStream) io.Writer {
	return streamWriter{rec: r, stream: s}
}

func (r *recorder) write(s docker.StdStream, b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, docker.Frame{Stream: s, Bytes: append([]byte(nil), b...)})
	if r.conn != nil && r.live && !r.dropped {
		if err := writeFrame(r.conn, s, b); err != nil {
			r.dropped = true
		}
	}
	return len(b), nil
}

func (r *recorder) recorded() []docker.Frame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]docker.Frame(nil), r.frames...)
}

func (r *recorder) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.conn != nil {
		r.conn.Close()
		r.conn = nil
	}
}

// reopen takes a container started again: an attach carries the new run, and the frames
// of the first stay, because a daemon's log of a container is every run it has had.
func (r *recorder) reopen() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed, r.dropped = false, false
}

// streamWriter is one of a container's two output streams.
type streamWriter struct {
	rec    *recorder
	stream docker.StdStream
}

func (s streamWriter) Write(b []byte) (int, error) { return s.rec.write(s.stream, b) }

// writeFrame writes the eight-byte header and the bytes after it, which is how the
// daemon keeps standard output and standard error apart on one connection.
func writeFrame(w io.Writer, s docker.StdStream, b []byte) error {
	var header [8]byte
	header[0] = byte(s)
	binary.BigEndian.PutUint32(header[4:], uint32(len(b)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}
