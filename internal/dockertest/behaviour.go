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

	// RegistryDigest is the digest the image's registry serves its manifest under,
	// which the classic image store holds apart from Digest, the image's own. Empty
	// is Digest itself, which is how the containerd store holds the two.
	RegistryDigest string

	// Unpushed is an image built on this machine and never pushed, which its registry
	// serves nothing of. The containerd store reports a digest in RepoDigests for it
	// all the same, and ClassicImageStore reports none.
	Unpushed bool
}

// registryDigest is the digest the registry serves the image under, where id is the
// digest the daemon holds it by.
func (i Image) registryDigest(id string) string {
	if i.RegistryDigest != "" {
		return i.RegistryDigest
	}
	return id
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

// WithoutSeccomp is an /info listing no name=seccomp, which is a daemon built without it
// or on a kernel that has none, and the seccomp floor refusing.
var WithoutSeccomp Behaviour = func(o *Options) { o.noSeccomp = true }

// OOMKills is a container killed for its memory: an oom event, then a die carrying 137,
// and a wait that never answers. It is the exit the event stream exists to catch.
var OOMKills Behaviour = func(o *Options) { o.oomKills = true }

// ClassicImageStore is a daemon on the image store Docker had before containerd's, which
// holds an image built on the machine and never pushed under no registry digest at all.
// The default is the containerd store, the daemon's own since Docker 29, which reports
// one for every image it holds, pushed or not.
var ClassicImageStore Behaviour = func(o *Options) { o.classicStore = true }

// RegistryUnreachable is a daemon that cannot reach a registry it is asked about, which
// is a laptop off its network: every question put to one answers 500 with the dial error.
var RegistryUnreachable Behaviour = func(o *Options) { o.registryUnreachable = true }

// RegistryAnswers401 is a registry that answers a question about a repository it holds
// nothing of with 401, as quay.io does, where Docker Hub and ghcr.io answer 403. The
// daemon passes on whichever of the two its registry chose.
var RegistryAnswers401 Behaviour = func(o *Options) { o.registryAnswers401 = true }

// PullAnswers401 is a registry that wants credentials for every pull, passed on the way
// the daemon passes on quay.io's: a 500 whose message ends in the registry's 401
// Unauthorized.
var PullAnswers401 Behaviour = func(o *Options) { o.pullAnswers401 = true }

// SlowPull is a pull that takes d over every layer, which is a cold pull of a large image
// and what a deadline has to be able to cut short. It is abandoned when the request is, as
// the daemon abandons one whose client went away.
func SlowPull(d time.Duration) Behaviour {
	return func(o *Options) { o.slowPull = d }
}

// TagMoves is ref pointed at another image the moment it has been inspected, which is a
// docker build -t or a docker pull of the same tag finishing on the machine between two
// questions about it. The image it named before stays held under its digest, as a
// daemon keeps an image a tag has moved off.
func TagMoves(ref string, to Image) Behaviour {
	return func(o *Options) {
		if o.moves == nil {
			o.moves = map[string]Image{}
		}
		o.moves[ref] = to
	}
}

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
	noSeccomp            bool

	classicStore        bool
	registryUnreachable bool
	registryAnswers401  bool
	pullAnswers401      bool
	slowPull            time.Duration
	moves               map[string]Image

	apiVersion string
	delay      time.Duration
}
