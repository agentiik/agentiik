package docker

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

// The Engine API shapes, with the wire names the daemon writes them under.
//
// Only the fields this product sets or reads are here. A library's HostConfig carries
// some ninety of them, of which the settings table names twelve, and a struct with
// seventy-eight zero values in it says less about what the driver does than a struct
// carrying the twelve. What is absent is absent on purpose: a field nobody sets cannot
// be set by accident, and a field this product deliberately refuses, Privileged and
// Devices among them, has no spelling here to refuse it with.

// Config is the container half of a create: what the image is, what it runs, what it is
// given and what it is labelled with.
//
// The four attach booleans and OpenStdin are set together or not at all. A container
// created without OpenStdin has no standard input to attach to, and the envelope on
// standard input is half of what a brick is given.
type Config struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Entrypoint []string          `json:"Entrypoint,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	User       string            `json:"User,omitempty"`
	WorkingDir string            `json:"WorkingDir,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`

	AttachStdin  bool `json:"AttachStdin"`
	AttachStdout bool `json:"AttachStdout"`
	AttachStderr bool `json:"AttachStderr"`
	OpenStdin    bool `json:"OpenStdin"`
	StdinOnce    bool `json:"StdinOnce"`

	// Tty is false and stays false. A pseudo-terminal merges standard output and
	// standard error onto one stream, and keeping them apart is what lets the
	// shorthand's standard output be published while standard error goes to the
	// masked log.
	Tty bool `json:"Tty"`
}

// HostConfig is the settings table of the documentation, field for field. It is written
// in HostConfig's own names so that the page and the struct read alike, with no
// translation between what the page says and what goes on the wire.
type HostConfig struct {
	Mounts      []Mount           `json:"Mounts,omitempty"`
	NetworkMode string            `json:"NetworkMode,omitempty"`
	Tmpfs       map[string]string `json:"Tmpfs,omitempty"`

	ReadonlyRootfs bool     `json:"ReadonlyRootfs"`
	CapDrop        []string `json:"CapDrop,omitempty"`
	CapAdd         []string `json:"CapAdd,omitempty"`
	SecurityOpt    []string `json:"SecurityOpt,omitempty"`

	// AutoRemove is false and the driver removes the container itself once the logs
	// and the exit code are collected, so that nothing is lost on a fast exit. It is
	// written without omitempty for the same reason ReadonlyRootfs is: a field the
	// table names is a field that goes on the wire, whatever its value, so that what
	// was sent can be read back.
	AutoRemove bool `json:"AutoRemove"`

	// Resources is embedded the way the Engine API embeds it, so its fields sit at
	// the top level of the JSON object exactly as the daemon expects them.
	Resources
}

// Resources is what HostConfig embeds: what the container may use.
type Resources struct {
	Memory   int64    `json:"Memory,omitempty"`
	NanoCPUs int64    `json:"NanoCpus,omitempty"`
	Ulimits  []Ulimit `json:"Ulimits,omitempty"`

	// PidsLimit is a pointer because zero and unset are different answers on this
	// field: the daemon reads 0 as unlimited, and the default the table gives is 256.
	PidsLimit *int64 `json:"PidsLimit,omitempty"`
}

// Ulimit is one process limit, by the name the kernel gives it: nofile and nproc are the
// two the runner policy sets.
type Ulimit struct {
	Name string `json:"Name"`
	Soft int64  `json:"Soft"`
	Hard int64  `json:"Hard"`
}

// Mount is one mount of the create, in the long form rather than the Binds string. The
// long form is what carries ReadOnly as a field instead of as a suffix nobody sees, and
// an input a step can write to is an input a retry reads differently.
type Mount struct {
	Type         string        `json:"Type"`
	Source       string        `json:"Source,omitempty"`
	Target       string        `json:"Target"`
	ReadOnly     bool          `json:"ReadOnly,omitempty"`
	TmpfsOptions *TmpfsOptions `json:"TmpfsOptions,omitempty"`
}

// The mount types this product uses. A volume is not one of them: a task's working
// directory is created fresh and removed with the container, and a volume outlives both.
const (
	MountBind  = "bind"
	MountTmpfs = "tmpfs"
)

// TmpfsOptions sizes a tmpfs mount. The flags themselves, noexec, nosuid and nodev,
// travel on HostConfig.Tmpfs, which is where the settings table names them and where
// every daemon from the floor up accepts them.
type TmpfsOptions struct {
	SizeBytes int64 `json:"SizeBytes,omitempty"`
	Mode      int   `json:"Mode,omitempty"`
}

// NetworkingConfig is which networks the container joins at creation. Every task gets
// its own, so this carries one endpoint and never two.
type NetworkingConfig struct {
	EndpointsConfig map[string]*EndpointSettings `json:"EndpointsConfig,omitempty"`
}

// EndpointSettings is one attachment to one network.
type EndpointSettings struct {
	NetworkID string   `json:"NetworkID,omitempty"`
	Aliases   []string `json:"Aliases,omitempty"`
}

// Created is what a create answered: the container, and whatever the daemon wanted to
// say about it.
type Created struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings,omitempty"`
}

// Waited is one exit, as the wait reports it.
//
// Err is not on the wire. It carries the failure of the wait itself, a daemon that
// closed the connection before the container exited, so that a caller reading one
// channel reads both outcomes in one place instead of watching two.
type Waited struct {
	StatusCode int        `json:"StatusCode"`
	Error      *WaitError `json:"Error,omitempty"`
	Err        error      `json:"-"`
}

// WaitError is what the daemon says when the wait itself went wrong on its side.
type WaitError struct {
	Message string `json:"Message"`
}

// The wait conditions. next-exit is the one this product uses for a container it starts,
// and the reason it is opened before the start: the daemon writes the response header as
// soon as the wait is registered, so a caller that has read the header knows the exit
// cannot be missed.
//
// not-running is for a container somebody else started. It answers at once on one that is
// already over, where next-exit would wait for an exit after the one that has happened:
// on a container nothing starts again, that wait never ends.
const (
	WaitNextExit   = "next-exit"
	WaitNotRunning = "not-running"
	WaitRemoved    = "removed"
)

// Image is the image as the daemon holds it, which is where the digest a reference
// resolved to is read and where the account the image declares is read.
type Image struct {
	ID           string      `json:"Id"`
	RepoDigests  []string    `json:"RepoDigests,omitempty"`
	RepoTags     []string    `json:"RepoTags,omitempty"`
	Config       ImageConfig `json:"Config"`
	Architecture string      `json:"Architecture,omitempty"`
	Os           string      `json:"Os,omitempty"`
	Created      string      `json:"Created,omitempty"`
}

// RegistryDigests are what ref can be pinned to: its repository, spelt as ref spells it,
// at each digest the daemon holds this image under in that repository, in the order the
// daemon lists them.
//
// The daemon writes RepoDigests in the short form docker pull takes, alpine@sha256:...
// for docker.io/library/alpine, so each is matched with ref by the repository both of
// them name rather than as text. A digest held under another repository, from a pull or
// a push of the same image elsewhere, names nothing ref's registry was asked to serve,
// and is left out.
//
// Which image store the daemon runs decides what an answer means. On the one Docker had
// before containerd's, an image built on the machine and never pushed is held under no
// digest, and this answers nothing. On the containerd store, the daemon's own since
// Docker 29, every image is held under one, pushed or not: the digest a registry would
// serve once the image was pushed there. Only the registry knows whether it was, and
// DistributionInspect is how it is asked.
func (i Image) RegistryDigests(ref string) []string {
	repository := repositoryOf(ref)
	want := canonical(repository)
	var out []string
	for _, held := range i.RepoDigests {
		name, digest, ok := strings.Cut(held, "@")
		if !ok || canonical(name) != want || !sha256Digest.MatchString(digest) {
			continue
		}
		out = append(out, repository+"@"+digest)
	}
	return out
}

// Distribution is what a registry serves under a reference, as the daemon asked it on
// this client's behalf.
type Distribution struct {
	Descriptor Descriptor `json:"Descriptor"`
}

// Descriptor names one manifest of a registry by the digest of its bytes, which is the
// digest an image is pinned by.
type Descriptor struct {
	MediaType string `json:"mediaType,omitempty"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size,omitempty"`
}

// ImageConfig is what the image itself declares, before any of it is overridden by a
// create.
type ImageConfig struct {
	User       string            `json:"User,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	Entrypoint []string          `json:"Entrypoint,omitempty"`
	Cmd        []string          `json:"Cmd,omitempty"`
	WorkingDir string            `json:"WorkingDir,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
}

// Summary is one container of a list, which is how a task is adopted by its label and
// how a startup sweep finds what an earlier process left behind.
type Summary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names,omitempty"`
	Image   string            `json:"Image,omitempty"`
	ImageID string            `json:"ImageID,omitempty"`
	State   string            `json:"State,omitempty"`
	Status  string            `json:"Status,omitempty"`
	Labels  map[string]string `json:"Labels,omitempty"`
	Created int64             `json:"Created,omitempty"`
}

// Inspected is one container as the daemon describes it. It is the backstop the driver
// consults when a task has been silent past its deadline, and it is where the two
// moments a result reports are read.
type Inspected struct {
	ID         string      `json:"Id"`
	Name       string      `json:"Name,omitempty"`
	Image      string      `json:"Image,omitempty"`
	State      State       `json:"State"`
	Config     *Config     `json:"Config,omitempty"`
	HostConfig *HostConfig `json:"HostConfig,omitempty"`

	// Mounts is what the container was actually given, resolved. It is not the same
	// answer as HostConfig.Mounts: the daemon echoes the host configuration back in
	// whichever form the create used, so a container created with Binds carries its
	// mounts there and HostConfig.Mounts is empty, while this list is filled in
	// whichever form was used. A driver adopting a container it did not create has to
	// read this one, because the container it adopts may have been started by
	// anything.
	Mounts []MountPoint `json:"Mounts,omitempty"`

	// NetworkSettings is what the container is on, as the daemon attached it, which is
	// the answer to whether a task is on its own network rather than what its create
	// asked for.
	NetworkSettings *NetworkSettings `json:"NetworkSettings,omitempty"`
}

// NetworkSettings is the networks a container is attached to, by network name.
type NetworkSettings struct {
	Networks map[string]Endpoint `json:"Networks,omitempty"`
}

// Endpoint is a container's place on one network: the network, and its address there.
type Endpoint struct {
	NetworkID   string `json:"NetworkID,omitempty"`
	IPAddress   string `json:"IPAddress,omitempty"`
	IPPrefixLen int    `json:"IPPrefixLen,omitempty"`
	Gateway     string `json:"Gateway,omitempty"`
}

// MountPoint is one mount of a container as the daemon resolved it. The names are the
// daemon's own, which are not the names of the create: the path inside the container is
// Destination here and Target there.
type MountPoint struct {
	Type        string `json:"Type,omitempty"`
	Source      string `json:"Source,omitempty"`
	Destination string `json:"Destination,omitempty"`
	RW          bool   `json:"RW,omitempty"`
}

// State is where a container is and what became of it.
//
// StartedAt and FinishedAt are the daemon's own moments rather than a clock on the
// driver's side, which is what lets the same task read twice report the same two
// moments. An unset one arrives as the year one and parses as the zero time.
type State struct {
	Status     string    `json:"Status"`
	Running    bool      `json:"Running"`
	Paused     bool      `json:"Paused"`
	Restarting bool      `json:"Restarting"`
	OOMKilled  bool      `json:"OOMKilled"`
	Dead       bool      `json:"Dead"`
	Pid        int       `json:"Pid"`
	ExitCode   int       `json:"ExitCode"`
	Error      string    `json:"Error,omitempty"`
	StartedAt  time.Time `json:"StartedAt"`
	FinishedAt time.Time `json:"FinishedAt"`
}

// Progress is one message of a pull. The daemon streams them as newline-delimited JSON
// inside a 200 that it has already committed to, so a failure arrives as one of these
// carrying Error rather than as a status.
type Progress struct {
	ID          string          `json:"id,omitempty"`
	Status      string          `json:"status,omitempty"`
	Progress    string          `json:"progress,omitempty"`
	Detail      *ProgressDetail `json:"progressDetail,omitempty"`
	Error       string          `json:"error,omitempty"`
	ErrorDetail *ErrorDetail    `json:"errorDetail,omitempty"`
}

// ProgressDetail is how far one layer has got.
type ProgressDetail struct {
	Current int64 `json:"current,omitempty"`
	Total   int64 `json:"total,omitempty"`
}

// ErrorDetail is what went wrong, where the daemon says so twice.
type ErrorDetail struct {
	Message string `json:"message,omitempty"`
}

// Event is one thing that happened to one object, as the daemon's stream reports it.
// The stream is a latency optimisation and never the correctness guarantee: a die that
// the wait missed is caught here, and a dropped stream makes a task slow rather than
// wrong.
type Event struct {
	Type     string     `json:"Type"`
	Action   string     `json:"Action"`
	Actor    EventActor `json:"Actor"`
	Scope    string     `json:"scope,omitempty"`
	Time     int64      `json:"time"`
	TimeNano int64      `json:"timeNano"`
}

// EventActor is what the event happened to, with the labels it carries, which is how an
// event is matched back to the task that owns it.
type EventActor struct {
	ID         string            `json:"ID"`
	Attributes map[string]string `json:"Attributes,omitempty"`
}

// The event types and actions this product reads. A die carries the exit code as an
// attribute; an oom arrives before it and carries none, which is why both are followed.
const (
	EventTypeContainer = "container"
	ActionDie          = "die"
	ActionOOM          = "oom"
	ActionStart        = "start"
	ActionDestroy      = "destroy"
)

// At is the moment the event happened, to the nanosecond where the daemon gave one.
func (e Event) At() time.Time {
	if e.TimeNano != 0 {
		return time.Unix(0, e.TimeNano)
	}
	if e.Time != 0 {
		return time.Unix(e.Time, 0)
	}
	return time.Time{}
}

// Filters is the filters query parameter, which the Engine API spells as a JSON object
// of name to values. It is a map rather than a builder because every call this package
// makes filters on one or two names.
type Filters map[string][]string

// Add records one value under one name, keeping whatever was recorded before.
func (f Filters) Add(name, value string) Filters {
	if f == nil {
		f = Filters{}
	}
	f[name] = append(f[name], value)
	return f
}

// encode writes the filters as the query parameter carries them, or nothing at all when
// there are none, so that an empty map does not become a filter that matches nothing.
func (f Filters) encode(q url.Values) {
	if len(f) == 0 {
		return
	}
	b, err := json.Marshal(map[string][]string(f))
	if err != nil {
		// A map of strings to strings marshals. There is no failure to report
		// and no caller who could act on one.
		return
	}
	q.Set("filters", string(b))
}
