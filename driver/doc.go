// Package driver runs a container. It is the thin thing between brick.WriteInputs and
// brick.Collect, it fills graph.Driver, and it is the only package in this module that
// may reach a Docker daemon.
//
// The evaluator states the interface in graph/driver.go and calls neither of its
// methods. This package implements it and imports graph; graph imports nothing of this,
// and graph/boundary_test.go already refuses the path segment driver to the evaluator,
// so that direction is the one this respects and that test stays green untouched.
//
// One Docker serves many tasks at once. It holds the negotiated daemon handle, the brick
// manifest cache keyed by image digest, the registry of tasks in flight keyed by
// agk.TaskID, and one goroutine following the daemon event stream. Run is a small state
// machine over the daemon in which every step has a stated recovery, rather than a happy
// path with error returns added afterwards.
//
// # What Run takes and what it returns
//
// Run(ctx, graph.Task) (graph.Result, error). The Task is the whole of the per task
// argument and stays exactly as the evaluator hands it out: Inputs is already the
// argument brick.WriteInputs takes, Outputs is already the declared argument brick.Collect
// takes, Params goes to /agk/params.json unexamined, Secrets is names and mount points
// whose values are redeemed through Config.Secrets at the last moment, and Script,
// BeforeScript, AfterScript and Shell become the container's command. Everything a
// container needs that a Task deliberately does not carry arrives at construction
// instead: the socket, the store, the repository tree, the secret source, the log sink,
// the policy and the work root. That is why the interface needs no widening. A wider Run
// would be the evaluator learning about grants, trees and sockets, which is the thing
// graph/driver.go exists to prevent.
//
// A graph.Result means a container ran and the exit code table read its code. An error
// means no outcome could be determined at all, which is a daemon that could not be
// reached, the userns floor refusing, network: egress being refused, a manifest declaring
// a root user, a pull that died, or a working directory that could not be prepared. A key
// this host has already carried to an ending is refused the same way, ErrCompleted: its
// outcome is the one the delivery that ran it reported, and this delivery determines
// none. That distinction is the reason the signature carries both, and it is the only
// judgement this package makes. No exit code is invented for a failure that produced
// none, because a driver reporting its own trouble as a brick failure fails somebody
// else's step.
//
// State is read through agk.Band and nowhere else. Exit 0 is agk.TaskSucceeded, every
// other exit is agk.TaskFailed with ExitCode set, a deadline that fired is
// agk.TaskTimedOut and a Stop that landed is agk.TaskCancelled. Never agk.TaskLost: that
// is the heartbeat's word for a runner that stopped reporting, and a container that
// exited 137 reported. The band from 125 up is charged to the platform by being reported
// as it exited, which agk.Band already makes unretryable and nameless to retry.on, and by
// a log line naming it as the runtime's rather than the brick's.
//
// DispatchedAt is when the container was created, which is not cosmetic: recording the
// dispatch is what fixes the deadline. StartedAt and FinishedAt are taken from
// ContainerInspect's own State.StartedAt and State.FinishedAt rather than from a clock on
// this side, so the same task read twice reports the same moments.
//
// # What Stop takes
//
// Stop(ctx, graph.Stop) error. A graph.Stop carries an agk.TaskID and a reason and
// nothing else, so the container is resolved by its dev.agentiik.task label rather than
// out of an in-memory map. That is what lets Stop reach a container this process did not
// start, which is precisely the case graph/driver.go gives for Stop being a method and
// not a cancelled context. The in-flight registry is consulted first because it is
// faster, never because it is the truth.
//
// Stop issues the daemon's own stop with t set to the policy grace, so the SIGTERM then
// SIGKILL escalation belongs to the daemon and survives this process dying between the
// two signals; the timer on this side only sends SIGKILL if the daemon did not. Stop
// returns when the signal is away and not when the container is gone, because the three
// rules that call off running work need the container stopped and not the waiting. The
// Run still blocked is the one that reports, coming back agk.TaskCancelled, or
// agk.TaskTimedOut where the reason was graph.StopDeadline, which is how one stop
// produces one Result without Stop having to return one.
//
// Stopping a task this driver does not hold is not an error. At-least-once delivery means
// a stop can arrive for a task that already finished, and a driver that failed there
// would fail on a duplicate.
//
// # What a caller may ask of an image without running one
//
// Manifest(ctx, step, image) answers with the brick manifest of one image. It is asked of
// this package rather than read in the command line because reading /agk/brick.yaml means
// pulling an image and inspecting it, and this package is the only one in the module that
// may reach a daemon; a second reader of that file would be a second pull, a second inspect
// and a second answer to what a manifest is. It adds no behaviour: it is the resolve, the
// pull, the read and the root-account refusal that one task already does, stopping where the
// container would be created, and it fills the same digest-keyed cache, so the manifest agk
// validate read is the one the run that follows uses.
//
// It answers with a refusal where the image carries no manifest rather than with an absence.
// graph.Images names the image of a non-script step, a step held to the ports and the
// parameters its manifest declares, so an image with nothing at that path is a contract
// break there and not the base image a script step legitimately runs in.
//
// # What a caller may ask of the daemon before there is a driver
//
// Probe(ctx, socket) dials, asks the daemon what it is, and closes: the socket that answered,
// the API version being spoken, the platform a container runs on natively, and whether the
// daemon remaps user namespaces. It is a package-level function and not a method because the
// whole point of it is to be asked before New. Policy.Helper is part of the configuration New
// is given, which static helper may be bound depends on the architecture a container runs on,
// and an amd64 binary bound into an arm64 container gives a script an exec format error rather
// than a program. The second Info call per run is the honest price of that ordering.
//
// It is also what keeps the command line off the Engine API. internal/docker is module-wide,
// so cmd/agk could dial a daemon itself; asking for these facts here leaves this package the
// only one in the module that does.
//
// # The order of one task
//
// One task is one conversation with the daemon, and its order is chosen so that the races
// cannot happen rather than so that they are caught. Negotiate the API version and check
// the userns floor, once per daemon. Refuse a key this host has already carried to an
// ending, which the record under the work root answers. Adopt by label or create. Pull by
// digest, reading every message of the progress stream, because the daemon reports a
// failed pull as an error object inside a 200 that has already streamed half its layers.
// Read /agk/brick.yaml out of the image and cache what brick.ParseManifest returns under
// the image digest. Prepare the working directory and its mounts. Open the wait with
// condition=next-exit before the container is started, which makes the exit-during-attach
// race unrepresentable rather than rare. Attach, start, write the envelope on standard
// input from its own goroutine and half-close, treating a broken pipe as ordinary because
// a brick is entitled not to read standard input. Read the demultiplexed stream, keeping
// standard output for the shorthand and passing standard error through the masker into
// the log. Take the exit code from the wait that was already open. Take the log from the
// daemon, which is why AutoRemove is false and why nothing is lost on a fast exit.
// Collect, upload, spill. Write the ending down under the work root, before anything is
// removed, so that no moment passes in which the work is done and the record does not say
// so. Then remove the container, the network and the working directory, in a defer that
// runs on every path.
//
// The record is what a container is not. Adoption finds a container that is still there;
// the record answers for a key whose container was collected and removed, which is the
// key at-least-once delivery hands back after a restart or a requeue. It sits under the
// work root and outside every task's directory, it holds the key, its state and a moment
// and never a payload, and it keeps a key for KeysKept, the task stream's own retention.
//
// The daemon is not the only source of truth about a container. The wait is the fast
// path, the event stream filtered to the dev.agentiik.task label catches an exit this
// driver did not cause with an out-of-memory kill the case the documentation names, and
// an inspect is the backstop consulted when a task has been silent past its deadline.
// That is deliberately the shape the controller already uses for NOTIFY and its sweep:
// the stream is a latency optimisation and the inspect is the correctness guarantee, so a
// dropped event stream makes a task slow and never wrong.
//
// # Two decisions taken before the code
//
// User namespace remapping is a floor that can be lifted. The check is the daemon's own
// info, and SecurityOptions carrying name=userns is the signal. Without it a task is
// refused, and only Policy.RequireUsernsRemap set to RemapLifted gets past the refusal.
// When it is lifted the driver says so once, through Config.Announce, in one plain
// sentence naming what is given up: a task's files are then owned by a real uid on the
// host. This exists because Docker Desktop does not offer the remapping and agk run
// --local has to work on a laptop. When remapping is on, the daemon's root directory ends
// in <uid>.<gid> and that pair is the ownership the working directory is given before the
// container is created; a chown that cannot be done refuses the task naming the uid it
// tried and /etc/subuid, rather than creating a container that will silently fail to
// write its outputs.
//
// Network egress is refused for now. network: none takes the none network mode and
// network: internal takes a per-task network with no outbound route. network: egress
// returns ErrEgressProxyMissing before anything is created, saying the proxy does not
// exist yet, so a workflow cannot believe its egress.allow list is being enforced when it
// is not. The proxy is a task in the v0.2.0 runner group. Every task gets a network of
// its own in all three cases, so two containers on one host never see each other whatever
// the posture. Nothing opens the network and calls it filtered.
//
// # Why the policy is a value and the file has one reader
//
// Policy is a value whose zero value is the floor in place, and New never reads a file.
// Reading /etc/agentiik/runner.toml belongs where a runner is configured, and doing it in
// New would make this package refuse to be a library on a machine with no such file,
// which is the one property both #installing-a-runner and agk run --local depend on.
// LoadPolicy is offered for the caller that does have the file, so that when the runner
// arrives there is one reader of that format and not two.
//
// # What leaves through the observer
//
// graph.Result has nowhere to carry the log reference, the artifact list or the usage
// block that the documented result message carries, and adding them would be changing the
// evaluator's contract to suit its executor. They leave through Config.Observer instead,
// an interface this package owns, which is also how the runner gets the dispatched,
// running and publishing transitions its heartbeat needs while a task is still in flight.
// Nothing in graph grows a field.
//
// # Readings this package takes where the documentation is silent
//
// Masking covers the payload and not only the log. The rule is written about collected
// logs, and a script's captured standard output is also written to the store as the item
// on out, so a secret echoed there would be stored in the clear. The same literal match
// runs over both. Masking is line buffered and happens before the timestamp, the index
// and the cap, so nothing unmasked reaches a sink, the truncation marker included. Within
// a line a value split across two reads from the socket is still caught, because the line
// is whole before the match runs; a value split across lines is not, which is what the
// documentation already says of it.
//
// /agk/out is a bind mount from the task's working directory and not a tmpfs. A tmpfs is
// unmounted when the container stops, so an output written to one is gone before anything
// can collect it, and collecting before exit is not sound because a brick writes until
// its last instant. /tmp stays a sized tmpfs, since nothing is collected from it. This is
// the reading that makes AutoRemove false meaningful and that matches the working
// directory being created fresh, owned by an unprivileged account and removed with the
// container.
//
// /agk/secrets is a bind mount whose host side is a tmpfs where the platform has one,
// Policy.SecretsDir, /dev/shm on Linux. A tmpfs the daemon creates at container start is
// empty and cannot be pre-populated, so a value could not be placed in one before the
// container's first instruction runs. Where no host tmpfs exists, which is the laptop the
// userns floor gets lifted for, the values touch the work root instead and the driver says
// so once, at the first task that is given a secret rather than when the daemon is opened:
// a run that declares none never writes a value, and a warning met on a run it does not
// apply to is a warning that gets scrolled past on the run it does.
//
// The shell defaults to three elements, DefaultShell. Without -c there is nothing to hand
// a command string to.
//
// before_script, script and after_script are one shell invocation in one container, not
// three containers and not three execs. Three containers lose /tmp between them and three
// execs add a second lifecycle to get wrong. The step's exit code is script's first
// non-zero; after_script runs whatever happened to script and its own code is discarded,
// which is what the step keyword table asks for.
//
// The item a script publishes when it writes nothing and exits 0 carries its captured
// standard output under StdoutField, and the files it left in /agk/out/files/ beside it.
// The documentation names the item and not its shape, so the name is one constant here
// and belongs on the page.
//
// Four paths the contract names live here rather than in brick, which names Root, InDir
// and OutDir but not these: RepoDir, RunPath, ParamsPath and SecretsDir. Moving them into
// brick later is an addition to that package rather than a change to it.
//
// # Layout
//
// One file per rule, tests beside each, because every one of them is a rule about the
// same Task and a sub-package per rule would only move the seams.
//
//	driver.go     Docker, New, Run, Stop, Close, the in-flight registry
//	config.go     Config and what a Task deliberately does not carry
//	policy.go     Policy, DefaultPolicy, LoadPolicy, the userns floor as an enum
//	daemon.go     negotiate the API version, check the floor, announce a lifted one
//	image.go      resolve and pull by digest, read and cache the manifest, refuse root
//	workdir.go    created fresh, owned inside the remapped range, removed with the container
//	mounts.go     brick.WriteInputs, /agk/repo, /agk/run.json, /agk/params.json, /agk/secrets, /agk/bin/agk
//	env.go        the AGK_* table and AGK_PARAM_<NAME>
//	container.go  the settings every task gets, as HostConfig writes them
//	network.go    a network per task, none and internal, egress refused
//	run.go        the create, wait, attach, start, copy, collect sequence, and adoption by label
//	record.go     the keys this host has ended, refused once ended, kept for a week
//	deadline.go   SIGTERM then SIGKILL after grace, with the event stream as the backstop
//	script.go     script, before_script, after_script, the shell default, the verdict
//	collect.go    brick.Collect, the files upload, brick.Spill, the standard output shorthand
//	log.go        timestamped, indexed, capped
//	mask.go       literal match, before anything is written
//	exit.go       agk.Band to agk.TaskState, and 125 and above to the platform
//	fault.go      errors naming the step, the port and the rule
//	observer.go   what graph.Result has nowhere to carry
//
// The two things in this group that resist a test are the socket and the container, and
// each has a package of its own: internal/docker holds the Engine API and nothing else,
// and internal/dockertest is a fake daemon on a temporary socket whose container is a Go
// function a test supplies. Everything above is therefore tested for what it actually is,
// which is decisions about mounts, environment, settings, signals and bytes, with no
// Docker in reach. What needs a real daemon is one file, skipped when none is present.
package driver
