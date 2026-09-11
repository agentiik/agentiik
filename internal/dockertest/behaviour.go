package dockertest

import (
	"time"

	"github.com/agentiik/agentiik/internal/docker"
)

// Options is what a test supplies rather than a failure it asks for: what a container
// does, and what an image is.
type Options struct {
	// Run is the container. It is given the working directory as the driver
	// prepared it and the environment as the driver computed it, and it answers with
	// an exit code, which is the one seam that makes the whole driver testable with
	// no Docker present.
	//
	// A nil Run is a container that writes nothing and exits 0, which is a
	// legitimate brick and the commonest thing a test needs.
	Run func(Container) (int, error)

	// Images is what a pull resolves to, keyed by the reference the driver asks
	// for. An image absent from the map is an image the registry does not have, and
	// the pull fails as one.
	Images map[string]Image

	// behaviours is embedded rather than listed here, so that the two fields above
	// stay what a test supplies and everything below stays a way a daemon goes
	// wrong.
	behaviours
}

// Image is one image this daemon holds: the digest a reference resolves to, the bytes at
// /agk/brick.yaml, and what the image itself declares.
type Image struct {
	// Digest is what the reference resolved to, written sha256:<hex>. It is what
	// the manifest cache is keyed by, so two references resolving to one digest are
	// one manifest.
	Digest string

	// Manifest is the bytes of /agk/brick.yaml, or nothing for an image carrying
	// none, which is a base image a script step runs in.
	Manifest []byte

	Config docker.ImageConfig

	// Layers is how many progress messages a pull streams before it is done. It is
	// what PullFailsHalfway has a halfway to fail at.
	Layers int

	// Remote says the daemon does not hold this image yet: it is in the registry and
	// nowhere else, so an inspect answers "No such image" until a pull has fetched
	// it. It is the cold machine, and it is the only way a test reaches the pull at
	// all, since a driver does not pull an image the daemon already has.
	Remote bool
}

// Behaviour is one thing a daemon does, whether it is what a test supplies or a way
// daemons go wrong.
type Behaviour func(*Options)

// With is the two things a test supplies, as a behaviour, so that NewDaemon takes one
// kind of argument.
func With(o Options) Behaviour {
	return func(dst *Options) {
		if o.Run != nil {
			dst.Run = o.Run
		}
		if o.Images != nil {
			dst.Images = o.Images
		}
	}
}

// The behaviours are the list of things that actually go wrong, so that the failure
// modes that matter are covered where no daemon exists, which is CI.

// PullFailsHalfway is a 200 whose progress stream carries an error object in the middle,
// after it has already reported half the layers as complete. It is the reason a pull is
// read message by message rather than by its status.
var PullFailsHalfway Behaviour = func(o *Options) { o.pullFailsHalfway = true }

// ExitsDuringAttach is a container that runs to completion before the attach carries
// anything, so that nothing it wrote reaches the attached stream. What it wrote is still
// in the daemon's log, which is what AutoRemove being false is for.
var ExitsDuringAttach Behaviour = func(o *Options) { o.exitsDuringAttach = true }

// DaemonVanishes is a daemon that closes every connection from the moment the container
// is started, with no status and no message, which is what a daemon being restarted
// under a running task looks like.
var DaemonVanishes Behaviour = func(o *Options) { o.daemonVanishes = true }

// EventStreamDrops is an event stream that is closed after its first event and must be
// resumed from since, which is the case the resume exists for.
var EventStreamDrops Behaviour = func(o *Options) { o.eventStreamDrops = true }

// WithoutUsernsRemap is an /info carrying no name=userns, which is the floor refusing.
// It is the default, because it is what Docker Desktop answers.
var WithoutUsernsRemap Behaviour = func(o *Options) { o.userns = false }

// WithUsernsRemap is an /info that carries name=userns, with a root directory ending
// <uid>.<gid>, which is where the remapped range is read.
func WithUsernsRemap(uid, gid int) Behaviour {
	return func(o *Options) {
		o.userns = true
		o.usernsUID, o.usernsGID = uid, gid
	}
}

// OOMKills is a container killed for its memory: an oom event, then a die carrying 137,
// and a wait that never answers. It is the exit the event stream exists to catch.
var OOMKills Behaviour = func(o *Options) { o.oomKills = true }

// APIVersion is a daemon answering a version other than the ceiling, which is every
// daemon this has been run against so far.
func APIVersion(v string) Behaviour {
	return func(o *Options) { o.apiVersion = v }
}

// Delay holds every answer back, so that a deadline has something to fire against.
func Delay(d time.Duration) Behaviour {
	return func(o *Options) { o.delay = d }
}

// The behaviours as the daemon stores them. They are unexported because a test asks for
// one by name rather than by setting a field, which is what keeps the list above the list
// of things that go wrong rather than a struct anybody may add a field to.
type behaviours struct {
	pullFailsHalfway  bool
	exitsDuringAttach bool
	daemonVanishes    bool
	eventStreamDrops  bool
	oomKills          bool

	userns               bool
	usernsUID, usernsGID int

	apiVersion string
	delay      time.Duration
}
