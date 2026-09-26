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
// whose values are asked of the task's secret source as the container is prepared, and
// Script, BeforeScript, AfterScript and Shell become the container's command. Everything a
// container needs that a Task deliberately does not carry arrives at construction
// instead: the socket, the store, the repository tree, the secret source, the log sink,
// the policy and the work root. That is why the interface needs no widening. A wider Run
// would be the evaluator learning about grants, trees and sockets, which is the thing
// graph/driver.go exists to prevent.
//
// Three of those are one task's where a server runs it, because a runner has them from
// the redemption of that task's grant: the store, opened on its presigned URLs and its
// upload policy, its secret values, and the tree laid out from the files it names. A
// runner holding two redemptions at once cannot answer through hooks asked by namespace
// or by name, so it gives each task its own as Sources, through WithSources, on the
// context that task's Run is called with. Each one given there answers in place of the
// Config hook of the same name, for that task and for the adoption of its container, and
// agk run --local, which redeems nothing, gives none. The context carries them because it
// is scoped to exactly one Run, as a redemption is to one task, and neither the Task nor
// the interface has to learn what a grant is.
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
// the floors, once per daemon, and again after its event stream drops, which is what a
// restart looks like. Refuse an image not named by digest on a server, and a key this host
// has already carried to an ending, which the record under the work root answers. Look for
// a container to adopt by label. Pull by digest, reading every message of the progress
// stream, because the daemon reports a failed pull as an error object inside a 200 that has
// already streamed half its layers. Read /agk/brick.yaml out of the image and cache what
// brick.ParseManifest returns under the image digest. Both are bounded by the task's
// deadline where there is nothing to adopt: a deadline that passes during either ends the
// task timed_out with no container, which is an ending and not an error, since nothing
// failed, and a stop that landed during them ends it cancelled. Adopt, or create. Prepare
// the working directory and its mounts. Open the wait with condition=next-exit before the
// container is started, which makes the exit-during-attach race unrepresentable rather than
// rare. Attach, start, write the envelope on standard input from its own goroutine and
// half-close, treating a broken pipe as ordinary because a brick is entitled not to read
// standard input. Read the demultiplexed stream, keeping standard output for the shorthand
// and passing standard error through the masker into the log. Take the exit code from the
// wait that was already open. Take the log from the daemon, which is why AutoRemove is
// false and why nothing is lost on a fast exit. Collect, upload, spill, and write each
// port's envelope to the store. Write the ending down under the work root, after the store
// has everything it names and before anything is removed, so that no moment passes in which
// the work is done and the record does not say so, and none in which the record names what
// the store does not hold. Then remove the container, the network, the secrets volume and
// the working directory, in a defer that runs on every path.
//
// The record is what a container is not. Adoption finds a container that is still
// there; the record answers for a key whose container was collected and removed, which
// is the key at-least-once delivery hands back after a restart or a requeue. It sits
// under the work root and outside every task's directory, it holds the key, its state,
// a moment and what the ending left by reference, each envelope and artifact by the
// digest the store holds it under and the log by its address, and never a payload, and
// it keeps a key for KeysKept, the task stream's own retention. A container that ran to
// its end ends its key even when what it left cannot be collected, an output that is
// not an envelope or a store that refused an artifact or an envelope: the brick ran,
// and the key is written down failed. Hold writes a key down on take, which is what a
// runner does before it redeems the task's grant and acknowledges its message, and refuses
// one that has ended with a *Completed holding the recorded Ending, before any grant is
// redeemed for it, which the runner reports under the task_id of the message it took: that
// is how the requeue of a task declared lost is answered by the host that had already ended
// it. It refuses one the host still has in flight with ErrTaskInFlight, also before any
// grant is redeemed, so that a requeue reaching the host still running its key binds nobody
// and is answered from the record once the key has ended, rather than bound to a runner
// that will never answer it and declared lost a second time. A key Hold wrote down is held
// in the in-flight registry from then on, as Run holds the task it runs, so that a stop sent
// once the redemption has bound the task, and before Run has begun, is kept for Run to find;
// Release lets go of a key the runner will not run.
//
// The daemon is not the only source of truth about a container. The wait is the fast
// path, the event stream filtered to the dev.agentiik.task label catches an exit this
// driver did not cause with an out-of-memory kill the case the documentation names, and
// an inspect is the backstop consulted when a task has been silent past its deadline, and
// sooner where its wait ended with nothing or an event about it was dropped.
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
// Seccomp is read off the same info, and it is a floor no file lifts. A daemon that lists
// no name=seccomp, or lists it with profile=unconfined while the policy names no profile
// of its own, is refused with ErrSeccompRequired unless Policy.RequireSeccomp is
// SeccompLifted, which only callers that are not runners set, and they are told what the
// machine gives up instead. AppArmor and SELinux are the host's to offer, so a daemon with
// neither is taken and said out loud, and a profile the policy names for a mechanism the
// daemon does not apply is refused rather than silently ignored. A profile that lets every
// call through counts as none.
//
// A runner's own host is held to one more. A remapped daemon is refused to a process that
// lacks CAP_CHOWN, CAP_FOWNER or CAP_DAC_OVERRIDE, read with capget, since a task's
// directory is given to the range and re-entered and removed afterwards; the refusal is
// ErrOwnershipCapabilities and names the unit lines that grant them. Config.Host answers in
// place of the kernel in a test.
//
// The floors are read when the daemon is opened, and read again before the first container
// after the event stream drops, since a daemon can only change its configuration by
// restarting and a restart drops the stream. A daemon that no longer meets a floor refuses
// each task with the sentinel New would have answered, and nothing is created on it.
//
// A runner's images are held to one more, Policy.RequireDigest. A task whose image is not
// name@sha256 is refused with ErrImageNotByDigest before anything is asked of the host,
// and a step that is not a script step, whose image carries no /agk/brick.yaml, is refused
// before its working directory, its network or its container exists, rather than run as
// the account the image declares.
// Only callers that are not runners set DigestLifted: agk run --local runs images built
// on the machine, which have only a tag, and reads every brick's manifest before it runs.
//
// Network egress is refused for now. network: none takes the none network mode and
// network: internal takes a per-task network with no outbound route and no address of the
// runner host in it, which a daemon older than Docker 28.0 cannot make and is refused for
// with ErrInternalNotIsolated. network: egress returns ErrEgressProxyMissing before
// anything is created, saying the proxy does not exist yet, so a workflow cannot believe
// its egress.allow list is being enforced when it is not. The proxy is v0.9.0 work. Every
// task gets a network of its own in all three cases, so two containers on one host never
// see each other whatever the posture. Nothing opens the network and calls it filtered.
// A network a runner that died left behind is removed by Sweep when the next one starts.
//
// # Why the policy is a value and the file has one reader
//
// Policy is a value whose zero value is every floor in place, and New never reads a file.
// Reading /etc/agentiik/runner.toml belongs where a runner is configured, and doing it in
// New would make this package refuse to be a library on a machine with no such file,
// which is the one property both #installing-a-runner and agk run --local depend on.
// LoadPolicy is offered for the caller that does have the file, so that there is one
// reader of that format and not two. It reads every setting an operator owns about the
// host, strictly: a key it does not read, a key in another case and a value of the wrong
// type are each refused, naming the line where it has one, and [hooks] is held to its
// three keys and runs nothing until the hooks arrive. It reads the seccomp profile the file
// names, too, because the Engine API takes the profile's JSON and never a path.
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
// An adopted container that is still running is masked with the values the first delivery
// wrote for it, read back off its secrets volume, beside the ones the adopting delivery
// redeemed. The documentation names a secret rotated between the two redemptions as a limit
// of masking, and the value the container holds is on its volume for as long as it runs;
// only a container that has exited, whose volume the daemon emptied as it let go of it, is
// still left with the limit.
//
// /agk/out is a bind mount from the task's working directory and not a tmpfs. A tmpfs is
// unmounted when the container stops, so an output written to one is gone before anything
// can collect it, and collecting before exit is not sound because a brick writes until
// its last instant. /tmp stays a sized tmpfs, since nothing is collected from it. This is
// the reading that makes AutoRemove false meaningful and that matches the working
// directory being created fresh, owned by an unprivileged account and removed with the
// container.
//
// /agk/secrets is a tmpfs volume of the task's own, of the local driver, mounted
// noexec,nosuid,nodev and read-only, and no directory of the host. A tmpfs mount the daemon
// creates at container start is empty, so a value could not be placed in one before the
// container's first instruction runs; a tmpfs volume can be, but it keeps what is written on
// it only while a container has it mounted, and a value written into a container that has
// not started is gone before it starts. So a holder, a container of the runner's own running
// the static helper on the task's image, mounts the volume first and is handed the values on
// its standard input, and it is removed once the task's container has started and holds the
// volume in its place. The volume goes with the task, and Sweep removes what a runner that
// died left. Nothing of a value is written on any disk, the laptop's included: Docker Desktop
// keeps the volume in its own memory as a Linux daemon does, so a local run is given a tmpfs
// as a server run is.
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
//	sources.go    what a runner gives one task in place of Config: its store, its secrets, its tree
//	policy.go     Policy, DefaultPolicy, LoadPolicy and runner.toml, the three floors as enums
//	host.go       what the floors ask of the host: three capabilities
//	daemon.go     hold the floors and the profiles to the daemon, announce what is given up
//	image.go      resolve and pull by digest, read and cache the manifest, refuse root
//	workdir.go    created fresh, owned inside the remapped range, removed with the container
//	mounts.go     brick.WriteInputs, /agk/repo, /agk/run.json, /agk/params.json, /agk/bin/agk, the values
//	secrets.go    the task's tmpfs volume at /agk/secrets, its holder, and the sweep of both
//	env.go        the AGK_* table and AGK_PARAM_<NAME>
//	container.go  the settings every task gets, as HostConfig writes them
//	network.go    a network per task, none and internal, egress refused
//	run.go        the create, wait, attach, start, copy, collect sequence, and adoption by label
//	record.go     the keys this host has taken and ended, refused once ended, kept for a week
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
