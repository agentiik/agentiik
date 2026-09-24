// Package dockertest is a fake Docker daemon on a temporary unix socket, and the one
// place that decides whether a real one is present.
//
// It exists because there are exactly two things in this group that resist a test, the
// socket and the container, and this is the second of them. A Daemon speaks the Engine
// API well enough for internal/docker to talk to it for real, and its container is a Go
// function the test supplies, given the working directory as the driver prepared it and
// the environment as the driver computed it, returning an exit code. That single seam
// makes the whole driver testable without Docker.
//
// It is a fake daemon on a real socket rather than an in-process interface on purpose. An
// in-process fake would let a wire bug pass every rule test, because the rules would never
// be marshalled; here they are, and a test reading Created() reads the settings table back
// after a round trip through JSON rather than trusting the struct it was handed. It also
// means internal/docker and driver share one fake instead of keeping two in step.
//
// It takes no *testing.T, on the httptest precedent, so both packages can use it and
// neither has to be a test to do so.
//
// # What a test asserts through it
//
// That the envelope arrived at /agk/in/<port>/envelope.json and on standard input. That
// /agk/repo is read-only. That AGK_SHARD is absent without a fan-out and reads 3/8 with
// one. That a HostConfig carries CapDrop ALL, ReadonlyRootfs true and AutoRemove false.
// That /agk/out is a bind and /tmp a tmpfs. That exit 120 is never retried and 137 is
// charged to the runtime. That after_script ran after a failed script without changing the
// verdict. That a secret value printed by a container never reaches the log or the
// payload. That a script writing nothing publishes its standard output on out. And that
// the container, the network and the working directory are all destroyed, which Removed()
// makes an assertion rather than an assumption.
//
// # The behaviours it can be asked for
//
// They are the list of things that actually go wrong, so that the failure modes that
// matter are covered in CI where no daemon exists:
//
//	PullFailsHalfway     a 200 whose progress stream carries an error object in the middle
//	ExitsDuringAttach    a container that exits while the attach is being established
//	DaemonVanishes       a daemon that closes every connection mid-task
//	EventStreamDrops     an event stream that drops and must be resumed from since
//	WithoutUsernsRemap   an /info carrying no name=userns, which is the floor refusing
//	WithUsernsRemap      an /info that carries it, with a root directory ending <uid>.<gid>
//	WithoutSeccomp       an /info listing no name=seccomp, the seccomp floor refusing
//	APIVersion           a daemon answering an older version than the ceiling
//	OOMKills             a die event with an oom before it, which the wait never sees
//
// Restart is the one change made to a daemon already serving: it drops every event stream
// and answers /info as configured by the behaviours it is given, which is a daemon
// restarted with another configuration under a driver that read the first. Streams says
// whether there is a stream to drop yet.
//
// # The real daemon
//
// Socket() answers where a real daemon is and whether there is one, consulting DOCKER_HOST
// first, then the per-user path Docker Desktop uses, then /var/run/docker.sock. A test
// that needs one asks and skips when it answers false. That is what keeps the suite green
// in CI and honest on a machine with Docker, and it is one function rather than a skip
// condition copied into every file that needs it.
//
// # Layout
//
//	daemon.go     Daemon, NewDaemon, Socket, Close, Restart, Streams, Handle, and the routing
//	container.go  Container, what a created container is and what running one means
//	record.go     Created and Removed, which is what happened, as it happened
//	behaviour.go  the prepared failures, each one a thing that actually goes wrong
//	real.go       Socket, the one place that decides whether a real daemon is present
package dockertest
