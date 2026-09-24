package dockertest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agentiik/agentiik/internal/docker"
)

// Daemon is a Docker daemon that answers on a unix socket of its own.
//
// It speaks the Engine API well enough for internal/docker to talk to it for real, which
// is the point: a test reading Created() reads the settings table back after a round trip
// through JSON, rather than trusting the struct it was handed. An in-process fake would
// let a wire bug pass every rule test, because the rules would never be marshalled.
type Daemon struct {
	socket string
	dir    string
	ln     net.Listener
	srv    *http.Server
	mux    *http.ServeMux
	custom *http.ServeMux
	opts   Options

	mu         sync.Mutex
	seq        int
	containers map[string]*live
	order      []*live
	removed    []string
	pulled     map[string]bool
	moved      map[string]Image
	networks   map[string]docker.NetworkSpec
	events     []docker.Event
	watchers   map[chan docker.Event]struct{}
	vanished   bool

	// described is what /info answers, kept apart from opts because Restart changes it
	// while the daemon is serving and opts is read without the lock.
	described docker.Info
}

// NewDaemon starts a daemon on a temporary socket. Close removes both.
//
// It takes no *testing.T, on the httptest precedent, so that internal/docker and driver
// can both use it and neither has to be a test to do so.
func NewDaemon(bs ...Behaviour) (*Daemon, error) {
	var o Options
	o.apiVersion = docker.Ceiling
	for _, b := range bs {
		if b != nil {
			b(&o)
		}
	}

	dir, err := socketDir()
	if err != nil {
		return nil, err
	}
	socket := filepath.Join(dir, "docker.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("listening on %s: %w", socket, err)
	}

	d := &Daemon{
		socket:     socket,
		dir:        dir,
		ln:         ln,
		mux:        http.NewServeMux(),
		custom:     http.NewServeMux(),
		opts:       o,
		containers: map[string]*live{},
		pulled:     map[string]bool{},
		moved:      map[string]Image{},
		networks:   map[string]docker.NetworkSpec{},
		watchers:   map[chan docker.Event]struct{}{},
		described:  describe(o),
	}
	d.routes()
	d.srv = &http.Server{Handler: http.HandlerFunc(d.serve)}
	go d.srv.Serve(ln)
	return d, nil
}

// Socket is where this daemon answers.
func (d *Daemon) Socket() string { return d.socket }

// Close stops the daemon and removes its socket.
func (d *Daemon) Close() error {
	d.srv.Close()
	err := os.RemoveAll(d.dir)

	d.mu.Lock()
	defer d.mu.Unlock()
	for ch := range d.watchers {
		close(ch)
		delete(d.watchers, ch)
	}
	return err
}

// Restart is the daemon restarted under whoever is talking to it, configured with the
// behaviours given laid over the ones it was started with. Every event stream is dropped,
// as a restart drops them, and /info answers as the daemon is now configured. Only what
// /info says is taken from the behaviours: WithoutSeccomp, WithUsernsRemap and
// WithoutUsernsRemap. Containers, images and networks are kept, as a daemon restarted
// with live restore keeps them.
func (d *Daemon) Restart(bs ...Behaviour) {
	o := d.opts
	for _, b := range bs {
		if b != nil {
			b(&o)
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.described = describe(o)
	for ch := range d.watchers {
		close(ch)
		delete(d.watchers, ch)
	}
}

// Streams is how many event streams the daemon holds open. A test waits on it before a
// Restart that has to drop one, since a client opens its stream in a goroutine of its own
// and a restart that lands first drops nothing.
func (d *Daemon) Streams() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.watchers)
}

// Handle replaces one route, which is how a test asks for a failure this package does
// not keep a behaviour for. The pattern is written without the version prefix, because
// the prefix is stripped before anything is matched.
func (d *Daemon) Handle(method, pattern string, h http.HandlerFunc) {
	d.custom.Handle(method+" "+pattern, h)
}

// serve strips the negotiated version prefix, applies the delay, and dispatches. A route
// a test replaced wins over the one this package registered.
func (d *Daemon) serve(w http.ResponseWriter, r *http.Request) {
	if d.opts.delay > 0 {
		time.Sleep(d.opts.delay)
	}
	if d.gone() && !strings.HasSuffix(r.URL.Path, "/_ping") {
		// A daemon that vanished answers nothing at all: the connection is taken
		// and dropped, which is what the client sees when one restarts under it.
		if conn, _, err := hijack(w); err == nil {
			conn.Close()
		}
		return
	}

	if version, rest, ok := cutVersion(r.URL.Path); ok {
		r.URL.Path = rest
		r.Header.Set("X-Requested-Api-Version", version)
	}
	if h, pattern := d.custom.Handler(r); pattern != "" {
		h.ServeHTTP(w, r)
		return
	}
	d.mux.ServeHTTP(w, r)
}

// cutVersion takes /v1.55/containers/create apart into the version and the rest.
func cutVersion(p string) (version, rest string, ok bool) {
	if !strings.HasPrefix(p, "/v") {
		return "", p, false
	}
	rest = p[2:]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return "", p, false
	}
	return rest[:slash], rest[slash:], true
}

// routes registers what this daemon answers. Every path is written as the Engine API
// writes it, so that the list reads as the list of endpoints spoken.
func (d *Daemon) routes() {
	d.mux.HandleFunc("GET /_ping", d.ping)
	d.mux.HandleFunc("HEAD /_ping", d.ping)
	d.mux.HandleFunc("GET /info", d.info)

	d.mux.HandleFunc("POST /images/create", d.imageCreate)
	d.mux.HandleFunc("GET /images/", d.imageInspect)
	d.mux.HandleFunc("GET /distribution/", d.distributionInspect)

	d.mux.HandleFunc("POST /containers/create", d.containerCreate)
	d.mux.HandleFunc("GET /containers/json", d.containerList)
	d.mux.HandleFunc("POST /containers/{id}/start", d.containerStart)
	d.mux.HandleFunc("POST /containers/{id}/attach", d.containerAttach)
	d.mux.HandleFunc("POST /containers/{id}/wait", d.containerWait)
	d.mux.HandleFunc("GET /containers/{id}/logs", d.containerLogs)
	d.mux.HandleFunc("GET /containers/{id}/archive", d.containerArchive)
	d.mux.HandleFunc("GET /containers/{id}/json", d.containerInspect)
	d.mux.HandleFunc("POST /containers/{id}/stop", d.containerStop)
	d.mux.HandleFunc("POST /containers/{id}/kill", d.containerKill)
	d.mux.HandleFunc("DELETE /containers/{id}", d.containerRemove)

	d.mux.HandleFunc("POST /networks/create", d.networkCreate)
	d.mux.HandleFunc("GET /networks", d.networkList)
	d.mux.HandleFunc("DELETE /networks/{id}", d.networkRemove)

	d.mux.HandleFunc("GET /events", d.eventStream)
}

// ping answers the version negotiation, in the headers where a daemon puts it.
func (d *Daemon) ping(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Api-Version", d.opts.apiVersion)
	w.Header().Set("Ostype", "linux")
	w.Header().Set("Docker-Experimental", "false")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		w.Write([]byte("OK"))
	}
}

// info answers with the two fields the userns floor is read from, and the security
// options seccomp is read from.
func (d *Daemon) info(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	i := d.described
	d.mu.Unlock()
	writeJSON(w, http.StatusOK, i)
}

// describe is the /info of a daemon configured with o.
func describe(o Options) docker.Info {
	i := docker.Info{
		ID:            "DAEM:ON00:FAKE",
		Name:          "dockertest",
		ServerVersion: "0.0.0-dockertest",
		OSType:        "linux",
		Architecture:  "aarch64",
		NCPU:          2,
		MemTotal:      2 << 30,
		DockerRootDir: "/var/lib/docker",
		// What a daemon nobody configured otherwise answers.
		DefaultRuntime: "runc",
	}
	if !o.noSeccomp {
		i.SecurityOptions = append(i.SecurityOptions, "name=seccomp,profile=builtin")
	}
	if o.userns {
		i.SecurityOptions = append(i.SecurityOptions, "name=userns")
		i.DockerRootDir = fmt.Sprintf("/var/lib/docker/%d.%d", o.usernsUID, o.usernsGID)
	}
	return i
}

// networkCreate records one network.
func (d *Daemon) networkCreate(w http.ResponseWriter, r *http.Request) {
	var spec docker.NetworkSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := newID()

	d.mu.Lock()
	d.networks[id] = spec
	d.mu.Unlock()

	writeJSON(w, http.StatusCreated, docker.NetworkCreated{ID: id})
}

// networkList answers with what is there, filtered by label where the query asked.
func (d *Daemon) networkList(w http.ResponseWriter, r *http.Request) {
	wanted := labelFilter(r)

	d.mu.Lock()
	defer d.mu.Unlock()
	list := []docker.NetworkSummary{}
	for id, spec := range d.networks {
		if !matchesLabels(spec.Labels, wanted) {
			continue
		}
		list = append(list, docker.NetworkSummary{
			ID: id, Name: spec.Name, Driver: spec.Driver,
			Internal: spec.Internal, Labels: spec.Labels,
		})
	}
	writeJSON(w, http.StatusOK, list)
}

// networkRemove removes one, and records that it was removed.
func (d *Daemon) networkRemove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	d.mu.Lock()
	_, ok := d.networks[id]
	if ok {
		delete(d.networks, id)
		d.removed = append(d.removed, id)
	}
	d.mu.Unlock()

	if !ok {
		writeError(w, http.StatusNotFound, "no such network: "+id)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// eventStream writes events as newline-delimited JSON, replaying whatever happened at or
// after since so that a resumed stream sees the gap it missed.
func (d *Daemon) eventStream(w http.ResponseWriter, r *http.Request) {
	since := sinceOf(r)

	ch := make(chan docker.Event, 64)
	d.mu.Lock()
	backlog := make([]docker.Event, 0, len(d.events))
	for _, e := range d.events {
		// since is exclusive, as the daemon's own buffer reads it: a stream
		// resumed at the moment of the last event seen gets what came after it
		// and not that event again.
		if since.IsZero() || e.At().After(since) {
			backlog = append(backlog, e)
		}
	}
	d.watchers[ch] = struct{}{}
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		if _, open := d.watchers[ch]; open {
			delete(d.watchers, ch)
			close(ch)
		}
		d.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	enc := json.NewEncoder(w)
	write := func(e docker.Event) bool {
		if err := enc.Encode(e); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	for _, e := range backlog {
		if !write(e) {
			return
		}
		if d.opts.eventStreamDrops {
			// One event, then the connection goes. The client resumes from
			// since and reads the rest.
			return
		}
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case e, open := <-ch:
			if !open || !write(e) {
				return
			}
			if d.opts.eventStreamDrops {
				return
			}
		}
	}
}

// emit records an event and hands it to whoever is watching.
func (d *Daemon) emit(e docker.Event) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, e)
	for ch := range d.watchers {
		select {
		case ch <- e:
		default:
			// A watcher that is not keeping up loses the live copy and
			// reads it from the replay when it reconnects, which is the
			// same guarantee the stream itself gives.
		}
	}
}

// gone says whether the daemon has vanished.
func (d *Daemon) gone() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.vanished
}

// sinceOf reads the since parameter the events query carries, seconds and nanoseconds
// with a dot between them.
func sinceOf(r *http.Request) time.Time {
	s := r.URL.Query().Get("since")
	if s == "" {
		return time.Time{}
	}
	var sec, nsec int64
	if _, err := fmt.Sscanf(s, "%d.%d", &sec, &nsec); err != nil {
		if _, err := fmt.Sscanf(s, "%d", &sec); err != nil {
			return time.Time{}
		}
	}
	return time.Unix(sec, nsec)
}

// labelFilter reads the label values out of the filters query parameter.
func labelFilter(r *http.Request) []string {
	raw := r.URL.Query().Get("filters")
	if raw == "" {
		return nil
	}
	var f map[string][]string
	if json.Unmarshal([]byte(raw), &f) != nil {
		return nil
	}
	return f["label"]
}

// matchesLabels says whether a set of labels satisfies every label filter, which the
// Engine API writes as name or name=value.
func matchesLabels(labels map[string]string, wanted []string) bool {
	for _, want := range wanted {
		name, value, hasValue := strings.Cut(want, "=")
		got, ok := labels[name]
		if !ok || (hasValue && got != value) {
			return false
		}
	}
	return true
}

// writeJSON answers with one object.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError answers the way the Engine API reports a refusal, one object carrying one
// message.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

// hijack takes the connection out from under the HTTP server, which is what an attach
// does and what a vanishing daemon does.
func hijack(w http.ResponseWriter) (net.Conn, *strings.Reader, error) {
	h, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("this response cannot be hijacked")
	}
	conn, _, err := h.Hijack()
	return conn, nil, err
}

// newID is a container or network identifier, written the way the daemon writes one.
func newID() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// socketDir is a directory short enough to put a unix socket in. The path of a unix
// socket is bounded at about a hundred bytes by the kernel, and the temporary directory
// of a macOS user is most of that on its own, so /tmp is the fallback rather than the
// first choice.
func socketDir() (string, error) {
	dir, err := os.MkdirTemp("", "agkd")
	if err != nil {
		return "", err
	}
	if len(filepath.Join(dir, "docker.sock")) <= 100 {
		return dir, nil
	}
	os.RemoveAll(dir)
	return os.MkdirTemp("/tmp", "agkd")
}
