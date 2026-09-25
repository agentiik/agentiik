package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
)

// removalGrace is how long the tidying at the end of a task is given when the task's own
// context is already done, which is exactly when it matters most: a cancelled task still
// has a container, a network and a directory to take away.
const removalGrace = 30 * time.Second

// drainGrace is how long the attached stream is given to end after the container has
// exited. A stream that does not end in that time is closed under its reader: the
// container is over, and what it wrote is in the daemon's log whatever this side read.
const drainGrace = 5 * time.Second

// Run runs one task in one container and reports what became of it.
//
// The order is chosen so that the races cannot happen rather than so that they are
// caught. Refuse what cannot run at all, and a key this host has already carried to an
// ending. Resolve the image and read its manifest. Adopt by label or prepare and create.
// Open the wait with condition=next-exit before the start, which is what makes the
// exit-during-attach race unrepresentable. Attach, start, write the envelope on standard
// input from its own goroutine and half-close. Read the demultiplexed stream, keeping
// standard output for the shorthand and passing standard error through the masker into
// the log. Take the exit code from the wait that was already open, or from the event
// stream where the wait missed it, or from an inspect under both. Collect and spill, hold
// every envelope to the size rules, and only then upload the artifacts and write each
// port's envelope to the store. Write the ending down under the work root. Then remove the
// container, the network and the working directory, in defers that run on every path.
//
// A graph.Result means a container ran. An error means none did, and it names the step
// and the rule. No exit code is invented for a failure that produced none, because a
// driver reporting its own trouble as a brick failure fails somebody else's step. Where a
// container did run to its end and what it left could not be collected, Run answers with
// that error all the same, and the key is written down as ended: failed, and where what it
// left broke the output contract, with ExitContractBroken and the span it ran for, which is
// the ending a result reports for it.
func (d *Docker) Run(ctx context.Context, t graph.Task) (graph.Result, error) {
	if t.Call != nil && t.Image == "" {
		return graph.Result{}, fault(t.Step, ErrContractBroken, ChargeBrick,
			"the step is a call to another workflow, which the evaluator expands and no container runs")
	}

	store, err := d.store(ctx, t)
	if err != nil {
		return graph.Result{}, err
	}

	// Said here rather than when the daemon was opened, because this is the first
	// moment it is true of anything: a task with no secret never has a value written
	// for it, and a person who reads the sentence on a run that declares none learns
	// to scroll past it.
	if len(t.Secrets) > 0 {
		d.currentFloor().announceSecrets(d.cfg.Policy, t.Step, d.say)
	}

	// The task is held from here at the latest, before anything is pulled or created, so
	// that a stop arriving while it is being prepared lands on something. That window is
	// the image pull and it is minutes wide on a cold registry; a stop answered nil inside
	// it would leave the container to be created afterwards and to run to its deadline. A
	// runner holds it from earlier still, from Hold, and a stop that landed since is taken
	// over here with the task.
	done, ok := d.register(t.ID)
	if !ok {
		return graph.Result{}, fault(t.Step, ErrTaskInFlight, ChargePlatform, "task %s", t.ID)
	}
	defer done()
	// A Run that returns with no ending written leaves nothing of the key on this host: what
	// it created is removed on the way out, and nothing ran to an ending a second delivery
	// would repeat. So the entry saying the key was taken is forgotten, after those removals
	// and before the key is let go of in memory, and a restarted runner does not name for
	// AckWait a key nothing here holds, which would keep its dispatch from being declared
	// lost for that long. A Run that never returns, its process killed, leaves the entry for
	// the restart to name.
	defer d.forgetTaken(t.ID)

	// A key this host has already carried to an ending is refused next, and after the
	// registration rather than before it, so that what it tidies away is never a
	// container a delivery still holding the key is carrying. Adoption below reaches a
	// container that is still there; this reaches a key whose container is long gone.
	if err := d.refuseCompleted(ctx, t); err != nil {
		return graph.Result{}, err
	}

	// A daemon restarted into one that meets no floor is refused before anything is
	// created on it, the container that reads the manifest included, and held to the
	// floors again just before the task's own container, since the pull between the two
	// is minutes wide.
	if _, err := d.heldToFloors(ctx, t.Step); err != nil {
		return graph.Result{}, err
	}

	// Everything that can be refused without creating anything is refused first, and
	// network: egress is the one that matters: a workflow must not be able to
	// believe its egress.allow list is being enforced when nothing is enforcing it.
	// The network itself is created just before the container, in networkFor.
	if err := refuseNetwork(d.cli, t); err != nil {
		d.abandon(ctx, t)
		return graph.Result{}, err
	}

	image, err := resolveImage(ctx, d.cli, d.cache, t, "", nil)
	if err != nil {
		return graph.Result{}, err
	}

	// Adoption comes before anything is created. A redelivered task re-attaches to
	// the container it already started instead of starting a second one, which is
	// what makes at-least-once delivery survivable in the way the Task comment
	// promises.
	adopted, err := d.containerOf(ctx, t.ID)
	if err != nil {
		return graph.Result{}, err
	}
	if adopted != "" {
		// An adopted container is removed and its working directory taken away
		// exactly as one this delivery created is. The removal is the runner's
		// whoever started the container: "the runner removes the container itself
		// once logs and exit code are collected", and the directory is "removed
		// with the container, so no residue of one namespace survives into the next
		// task on that host". The delivery that started it may have died before its
		// own defers ran, which is the case adoption exists for, so leaving the
		// tidying to that one would leave a container and a directory of secrets
		// behind on every redelivery.
		//
		// The directory is named rather than inspected: the path is derived from
		// the task identifier, so the one this delivery names is the one the first
		// prepared, and a work root that named nothing is left alone rather than
		// guessed at.
		if w, err := workdirFor(d.cfg.WorkRoot, t.ID, d.cfg.Policy.SecretsDir); err == nil {
			defer d.tidy(t, w)
		}
		// The network goes after the container, whatever network it was created on:
		// one a driver that predates the isolation left is removed with it rather
		// than refused, which would leave the container running on it.
		defer d.removeNetwork(ctx, t, networkOf(t))
		defer func() {
			tidy, cancel := context.WithTimeout(context.WithoutCancel(ctx), removalGrace)
			defer cancel()
			d.cli.ContainerRemove(tidy, adopted, true)
		}()
		return d.ended(d.rejoin(ctx, t, store, adopted, image))
	}

	w, err := newWorkdir(d.cfg.WorkRoot, t.ID, d.cfg.Policy.SecretsDir)
	if err != nil {
		return graph.Result{}, err
	}
	defer d.tidy(t, w)

	run, err := d.runOf(ctx, t)
	if err != nil {
		return graph.Result{}, err
	}
	repo, err := d.repo(ctx, t)
	if err != nil {
		return graph.Result{}, err
	}

	given, err := prepare(ctx, t, w, d.cfg.Policy, d.cfg.host(), store, run, repo, d.secrets(ctx))
	if err != nil {
		return graph.Result{}, err
	}
	// The ownership comes after the contents and before the create. The runner
	// "prepares each task's working directory with ownership inside the remapped
	// range before it creates the container", and a chown taken before the envelope,
	// the run context and the secrets were written would leave every one of them
	// owned by this process instead of by the range the container's processes live
	// in. The floors are held first, because the range is the daemon's and the daemon
	// may have been restarted into another since the task began.
	floor, err := d.heldToFloors(ctx, t.Step)
	if err != nil {
		return graph.Result{}, err
	}
	if err := floor.ownWorkdir(w); err != nil {
		return graph.Result{}, err
	}

	// The dispatch is the creation and not the planning, because the deadline runs
	// from the moment the work became somebody's. It is taken immediately before the
	// create rather than immediately after, because the container has to be told the
	// same moment the watch will stop it at, and the environment is composed here.
	dispatched := d.now()

	n, err := networkFor(ctx, d.cli, t)
	if err != nil {
		return graph.Result{}, err
	}
	defer d.removeNetwork(ctx, t, n)

	entrypoint, cmd := scriptCommand(t, d.cfg.Policy.Shell)
	config := containerConfig(t, image.Ref, image.User, environment(t, deadlineOf(t, dispatched)), entrypoint, cmd)
	host, err := hostConfig(t, d.cfg.Policy, given, n.Mode)
	if err != nil {
		return graph.Result{}, err
	}

	created, err := d.cli.ContainerCreate(ctx, "", config, host, networkingConfig(n.Mode))
	if err != nil {
		if docker.IsUnreachable(err) {
			return graph.Result{}, fault(t.Step, ErrDaemonUnreachable, ChargePlatform, "creating the container: %v", err)
		}
		return graph.Result{}, fault(t.Step, ErrContractBroken, ChargePlatform, "the container could not be created: %v", err)
	}
	defer func() {
		tidy, cancel := context.WithTimeout(context.WithoutCancel(ctx), removalGrace)
		defer cancel()
		d.cli.ContainerRemove(tidy, created.ID, true)
	}()

	return d.ended(d.carry(ctx, t, store, created.ID, image, given, w.Out, dispatched, true, false))
}

// carry is the half of Run that a container exists for: wait, attach, start, read, exit,
// collect. It is separate so that the adoption path joins it at the same point.
//
// fresh says the container has never run, whichever delivery created it, which decides two
// things at once: the envelope goes on its standard input, and a stop that landed before it
// ran means it is never started at all.
//
// running says the container was adopted while it was already running, which also
// decides two things: it is not started, and its wait is opened with condition=not-running
// rather than next-exit. A container can exit between the inspect that found it running
// and the wait. next-exit would then wait for a further exit that only a start brings, and
// a start runs the brick a second time; not-running answers with the exit that happened.
func (d *Docker) carry(ctx context.Context, t graph.Task, store *artifact.Store, container string, image resolved, given *given, out string, dispatched time.Time, fresh, running bool) (graph.Result, error) {
	mask := newMasker(given.Values...)

	sink, closeSink, err := d.openLog(ctx, t)
	if err != nil {
		return graph.Result{}, err
	}
	defer closeSink()
	log := newLog(sink, mask, d.cfg.Now, d.cfg.Policy.LogMaxBytes, d.cfg.Policy.LogMaxLines)

	d.observe(ctx, Event{Task: t.ID, State: agk.TaskDispatched, Container: container})

	// The wait is opened before the start. With condition=next-exit the daemon
	// registers it and answers the header at once, so an exit cannot fall between
	// the two calls.
	condition := docker.WaitNextExit
	if running {
		condition = docker.WaitNotRunning
	}
	waited, err := d.cli.ContainerWait(ctx, container, condition)
	if err != nil {
		return graph.Result{}, fault(t.Step, ErrDaemonUnreachable, ChargePlatform, "opening the wait on the container: %v", err)
	}

	stream, err := d.cli.ContainerAttach(ctx, container, docker.AttachOptions{
		Stdin: fresh, Stdout: true, Stderr: true, Stream: true,
	})
	if err != nil {
		return graph.Result{}, fault(t.Step, ErrDaemonUnreachable, ChargePlatform, "attaching to the container: %v", err)
	}
	defer stream.Close()

	deadline := deadlineOf(t, dispatched)
	watcher := newWatch(d.cli, container, t.Step, log, deadline, d.cfg.Policy.StopGrace, d.cfg.Now)
	stoppedBefore := false
	if h := d.lookup(t.ID); h != nil {
		stoppedBefore = h.join(container, watcher)
	}

	if stoppedBefore && fresh {
		// A stop landed while this task was being prepared, and the container it
		// was prepared for has never run. It is not started: the work was called off
		// before it began, and starting it in order to signal it would run the
		// brick's first instructions for nothing. The container, the network and the
		// working directory leave in the defers of Run, as they do on every path.
		log.note("the task was stopped before its container was started, so nothing ran")
		state := watcher.finish(0, false, "a stop that arrived before the container did").state()
		ref, _ := log.finish()
		d.observe(ctx, Event{
			Task: t.ID, State: state, Container: container,
			Log: ref, Usage: Usage{ImagePullMS: image.PullMillis},
		})
		return graph.Result{Task: t.ID, State: state, DispatchedAt: dispatched}, nil
	}

	if !running {
		if err := d.cli.ContainerStart(ctx, container); err != nil {
			if docker.IsUnreachable(err) {
				return graph.Result{}, fault(t.Step, ErrDaemonUnreachable, ChargePlatform, "starting the container: %v", err)
			}
			return graph.Result{}, fault(t.Step, ErrContractBroken, ChargePlatform, "the container could not be started: %v", err)
		}
	}
	// What the container consumes is read from here, while it runs, since its cgroup
	// and everything counted in it go when it exits.
	spent := d.sample(ctx, container)
	defer spent.stop()
	if stoppedBefore {
		// The container was adopted and is running already, so the stop recorded
		// while this side had no watch to take it reaches the daemon now.
		watcher.sendStop(ctx)
	}
	d.observe(ctx, Event{Task: t.ID, State: agk.TaskRunning, Container: container})

	// The task is registered before the container is started, so that a stop can
	// reach it at all, and one that landed in that window reached a container the
	// daemon had not started: it answers such a stop 304 and signals nothing. There
	// is a process to signal now, so the stop is sent again rather than lost.
	watcher.resendStop(ctx)

	// The envelope goes on standard input from a goroutine of its own, and the write
	// half is closed after it. A brick is entitled not to read standard input, so a
	// broken pipe here is ordinary and is not reported as anything.
	go writeStdin(stream, given.Stdin)

	// The stream is read on a goroutine of its own, and not here, because the
	// deadline has to be enforced while the container is writing. A copy loop on this
	// goroutine would only start the deadline once the container had already stopped,
	// which is the one moment it is no longer needed.
	stdout := newCapture(d.limits().EnvelopeMaxBytes, mask)
	copied := make(chan int, 1)
	go func() { copied <- copyStream(stream, log, stdout) }()

	e, err := watcher.await(ctx, waited)
	if err != nil {
		return graph.Result{}, err
	}
	spent.exited()

	// The attach ends when the container does, so what is left is whatever is still
	// on the socket. It is given a moment to arrive and then the stream is closed
	// under the reader, because the daemon's own log is the backstop and a task must
	// not hang on a connection that is not going to end.
	var frames int
	select {
	case frames = <-copied:
	case <-time.After(drainGrace):
		stream.Close()
		frames = <-copied
	}

	if frames == 0 {
		// The container ran to completion before the attach carried anything,
		// which is the fast exit AutoRemove being false exists for. The daemon
		// still has everything it wrote.
		d.replay(ctx, container, log, stdout)
	}

	return d.conclude(ctx, t, store, container, image, spent, log, mask, stdout, e, out, dispatched)
}

// conclude is the end every container this driver carries comes to, whether it was watched
// to its exit or found already over: the exit read as a task state, the ports collected off
// the mount where it succeeded, the log closed and the observer told. spent is what read
// the container's statistics while it ran, and nil for one found already over.
//
// A collection that fails is an error that came after the exit, which is how ended knows
// to write the key down all the same.
func (d *Docker) conclude(ctx context.Context, t graph.Task, store *artifact.Store, container string, image resolved, spent *sampler, log *taskLog, mask *masker, stdout *capture, e exit, out string, dispatched time.Time) (graph.Result, error) {
	d.observe(ctx, Event{Task: t.ID, State: agk.TaskPublishing, Container: container})

	// A deadline that fired and a stop that landed are what the task was, whatever code
	// the kill left behind: the code of a killed container says how it was killed and
	// not why. So they are read first, and the exit code table is not asked about a code
	// it would read as the runtime's failure when it was this side that stopped it.
	state := e.state()
	if state == agk.TaskTimedOut || state == agk.TaskCancelled {
		log.note("%s", stoppedNote(e.Code, state))
	} else {
		state = readExit(log, e.Code)
	}

	result := graph.Result{Task: t.ID, State: state, DispatchedAt: dispatched}
	if state == agk.TaskSucceeded || state == agk.TaskFailed {
		result.ExitCode = e.Code
	}
	result.StartedAt, result.FinishedAt = d.moments(ctx, t, container, dispatched)

	var artifacts []agk.File
	var ports []EndedPort
	if state == agk.TaskSucceeded {
		got, err := collect(ctx, store, collection{
			Task:       t,
			Dir:        out,
			ProducedAt: d.now(),
			Limits:     d.limits(),
			Stdout:     stdout.Bytes(),
			Code:       e.Code,
			Mask:       mask,
		})
		// The ports go into the store here, before ended writes the ending down, which is
		// the runner's order: it "posts the outputs under the upload policy, and records
		// the key's ending". The record names each envelope by the digest this answers,
		// and a requeue that comes back to this host is answered from the record, so a
		// digest recorded before the store had it would be a result the controller could
		// never read back. One that cannot be written is an error after the exit, and the
		// key is written down failed, naming nothing.
		if err == nil {
			ports, err = publishPorts(ctx, store, t.Step, got.Outputs)
		}
		if err != nil {
			return graph.Result{}, d.refused(ctx, t, container, spent.usage(image), log, result, err)
		}
		result.Outputs, artifacts = got.Outputs, got.Artifacts
	}

	ref, logErr := log.finish()
	if logErr != nil {
		// A sink that failed is the runner's trouble and not the container's, so
		// the task is not failed for it and somebody is told.
		d.say("driver: task " + string(t.ID) + ": the log sink failed: " + logErr.Error())
	}
	// The exit is told whatever the state, since a container stopped at its deadline or
	// cancelled exited too, with the code the stop left, and a result reports it.
	code := e.Code
	d.observe(ctx, Event{
		Task: t.ID, State: state, Container: container,
		Log: ref, Outputs: ports, Artifacts: artifacts,
		Usage:    spent.usage(image),
		ExitCode: &code, StartedAt: result.StartedAt, FinishedAt: result.FinishedAt,
	})
	return result, nil
}

// refused ends a task whose container exited 0 and whose outputs were not published, and
// answers with the error Run returns for it.
//
// The container ran to its end, so this is an ending like any other: the log says why and
// is closed, the observer is told the task failed, and the key is written down.
//
// Where the brick broke the output contract, the error is an ErrOutputsRefused and the key
// is written down with ExitContractBroken and the span the container ran for, since a
// failed result with neither reads as a task where no container ran. The exit code table
// charges the code to the brick and never retries it.
//
// Where the store would not take what passed, or the collection was cut short because the
// runner's own context ended, the brick did what it was asked and the failure is the
// platform's. The key is written down failed with no code, as it was before any code
// existed for this: the container exited 0, and no row of the table says what a runner
// reports for outputs it could not write.
//
// The error keeps what refused. A size rule's *agk.Refusal is reachable through errors.As,
// so a caller can tell which rule it was and what the rule does to the run.
func (d *Docker) refused(ctx context.Context, t graph.Task, container string, usage Usage, log *taskLog, r graph.Result, err error) error {
	charge, decided := Charged(err)
	switch {
	case decided:
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		// A collection cut short because the runner is going away says nothing about
		// what the brick left.
		err = fault(t.Step, err, ChargePlatform, "the outputs were being collected when the runner's context ended")
		charge = ChargePlatform
	default:
		err = fault(t.Step, ErrOutputsRefused, ChargeBrick, "%w", err)
		charge = ChargeBrick
	}

	if charge == ChargeBrick {
		log.note("the outputs were refused, so the task failed with exit code %d, which the exit code table reserves for the runner: the brick broke the output contract, and the step is not retried: %v", ExitContractBroken, err)
	} else {
		log.note("the outputs could not be written to the store, so the task failed, charged to the runner and not to the brick: %v", err)
	}
	ref, logErr := log.finish()
	if logErr != nil {
		d.say("driver: task " + string(t.ID) + ": the log sink failed: " + logErr.Error())
	}
	ended := Event{
		Task: t.ID, State: agk.TaskFailed, Container: container,
		Log: ref, Usage: usage, Err: err,
	}
	// The exit is told as the key is written down: ExitContractBroken and the span where the
	// brick broke the contract, and none where the platform failed it, which the record
	// writes with neither.
	if charge == ChargeBrick {
		code := ExitContractBroken
		ended.ExitCode, ended.StartedAt, ended.FinishedAt = &code, r.StartedAt, r.FinishedAt
	}
	d.observe(ctx, ended)
	if charge != ChargeBrick {
		return exited(t.ID, err)
	}
	r.State, r.ExitCode = agk.TaskFailed, ExitContractBroken
	return exitedWith(r, err)
}

// rejoin re-attaches to the container an earlier delivery of this task left behind, which
// a redelivered task finds by its label, and never starts it a second time.
//
// It does not prepare anything: the working directory, the envelope and the secret files
// are the first delivery's and are still there. Where they are is read off the container
// itself, which is the one place that cannot disagree with what the container was
// actually given.
//
// What becomes of the container is read off the same inspect. One that is running is
// waited on. One that has exited is collected as it stands, because it is the work
// already done: a runner that dies between the exit and the tidying leaves exactly that
// behind, and a daemon answers a start on an exited container by running it a second
// time. Only one that was created and never started is started, since nothing has run in
// it yet, and it is carried as one this delivery created.
func (d *Docker) rejoin(ctx context.Context, t graph.Task, store *artifact.Store, container string, image resolved) (graph.Result, error) {
	in, err := d.cli.ContainerInspect(ctx, container)
	if err != nil {
		return graph.Result{}, fault(t.Step, ErrDaemonUnreachable, ChargePlatform,
			"a container carrying this task's label could not be inspected: %v", err)
	}
	// A container that is over has ended its key, whatever this delivery then makes of
	// it, so a failure from here on is one that came after its exit. It is removed on
	// the way out all the same, and its key has to be written down before it goes.
	fail := func(err error) (graph.Result, error) {
		if over(in.State) {
			err = exited(t.ID, err)
		}
		return graph.Result{}, err
	}
	// The resolved mount list and not the host configuration the create sent. The
	// daemon echoes a host configuration back in whichever form it arrived in, so a
	// container started with Binds rather than Mounts, which is what docker run and
	// every other tool on the host does, carries nothing under HostConfig.Mounts at
	// all. A container this driver adopts may have been started by anything, and the
	// resolved list is the one place that answers for all of them.
	out := ""
	for _, m := range in.Mounts {
		if m.Destination == brick.OutDir {
			out = m.Source
		}
	}
	if out == "" {
		return fail(fault(t.Step, ErrContractBroken, ChargePlatform,
			"the container adopted for this task has nothing bound at %s, so there is nowhere to collect its outputs from", brick.OutDir))
	}

	// The masker needs the values the container was given, and the first delivery's
	// copy of them in memory left with the process that had it. Its files are still in
	// the task's secrets directory, which is removed only once this returns, and they are
	// read back, with this delivery's own redemption beside them. Nothing is written:
	// this is the list the literal match runs against.
	values, err := d.values(ctx, t)
	if err != nil {
		return fail(err)
	}

	dispatched := in.State.StartedAt
	if dispatched.IsZero() {
		dispatched = d.now()
	}
	if over(in.State) {
		return d.settle(ctx, t, store, container, image, values, out, dispatched, in.State)
	}
	// The envelope of a running container was written on standard input by the delivery
	// that started it, and its write half was closed after it: a second attach asking
	// for standard input would have nothing to write and an already closed pipe to write
	// it to. One that never started had nothing written, because the delivery that
	// created it died before the start, and its standard input stays open until a writer
	// closes it. It is given its envelope there now, as that delivery would have given
	// it, or a brick reading standard input waits on it until the deadline.
	running := in.State.Running || in.State.Restarting
	g := &given{Values: values}
	if !running {
		// A container is confined as the daemon starts it and not as it was created,
		// so one about to be started for the first time is held to the floors as a
		// container about to be created is.
		if _, err := d.heldToFloors(ctx, t.Step); err != nil {
			return graph.Result{}, err
		}
		if g.Stdin, err = stdinBytes(t); err != nil {
			return graph.Result{}, err
		}
	}
	return d.carry(ctx, t, store, container, image, g, out, dispatched, !running, running)
}

// over says whether a container has run to its end: started once, and neither running nor
// being restarted now. One that was created and never started is not running either, and
// it is the one of the two that is still to be started.
func over(s docker.State) bool {
	return !s.StartedAt.IsZero() && !s.Running && !s.Restarting
}

// settle collects a container that was already over when this delivery reached it, as it
// stands: the exit code off the inspect, what it wrote off the daemon's log and its ports
// off the mount it was given. Nothing is waited on, attached to or started.
//
// A stop that arrived while this delivery was on its way changes nothing here. There was
// no process left to signal, and what the container did is what it did. A deadline is
// another matter, because it is a moment and not a message: the delivery that watched the
// container stopped it there and died before it reported, and the end is read as that
// watch would have read it.
func (d *Docker) settle(ctx context.Context, t graph.Task, store *artifact.Store, container string, image resolved, values [][]byte, out string, dispatched time.Time, s docker.State) (graph.Result, error) {
	mask := newMasker(values...)

	sink, closeSink, err := d.openLog(ctx, t)
	if err != nil {
		return graph.Result{}, exited(t.ID, err)
	}
	defer closeSink()
	log := newLog(sink, mask, d.cfg.Now, d.cfg.Policy.LogMaxBytes, d.cfg.Policy.LogMaxLines)

	d.observe(ctx, Event{Task: t.ID, State: agk.TaskDispatched, Container: container})

	log.note("the container had already exited when this delivery of the task reached it, so it is collected as it stands rather than started again, and what it wrote is read back from the daemon")
	stdout := newCapture(d.limits().EnvelopeMaxBytes, mask)
	d.replay(ctx, container, log, stdout)
	if s.OOMKilled {
		log.note("the container was killed for its memory")
	}

	e := exit{Code: s.ExitCode, OOM: s.OOMKilled, Source: "an inspect"}
	if stoppedAtDeadline(deadlineOf(t, dispatched), s.FinishedAt, d.cfg.Policy.StopGrace) {
		log.note("the step's timeout had passed when the container ended, inside the time the stop at the deadline takes, so it is read as stopped at its deadline")
		e.TimedOut = true
	}
	return d.conclude(ctx, t, store, container, image, nil, log, mask, stdout, e, out, dispatched)
}

// writeStdin puts the envelope on standard input and closes the write half after it.
//
// A brick that never reads standard input leaves the pipe to be broken, which is the
// contract working as written rather than a failure: the envelope is also under
// /agk/in/<port>/envelope.json, and a brick reads whichever of the two it prefers.
func writeStdin(stream *docker.Stream, b []byte) {
	if stream.Stdin == nil {
		return
	}
	if len(b) > 0 {
		stream.Stdin.Write(b)
	}
	stream.CloseWrite()
}

// copyStream reads the demultiplexed stream to its end, keeping standard output for the
// shorthand and putting both streams into the log, and answers with how many frames
// arrived.
//
// The count is what tells a fast exit from a quiet container: zero frames on a container
// that wrote something means the attach was established after it had already finished,
// and the daemon's own log is where that something still is.
func copyStream(stream *docker.Stream, log *taskLog, stdout io.Writer) int {
	frames := 0
	for {
		frame, err := stream.Next()
		if err != nil {
			return frames
		}
		frames++
		switch frame.Stream {
		case docker.Stdout:
			stdout.Write(frame.Bytes)
			log.write(Stdout, frame.Bytes)
		case docker.Stderr:
			log.write(Stderr, frame.Bytes)
		}
	}
}

// replay reads what the container wrote from the daemon, for the container that exited
// before the attach carried anything.
func (d *Docker) replay(ctx context.Context, container string, log *taskLog, stdout io.Writer) {
	logs, err := d.cli.ContainerLogs(ctx, container, docker.LogOptions{Stdout: true, Stderr: true})
	if err != nil {
		log.note("the container's log could not be read back from the daemon: %v", err)
		return
	}
	defer logs.Close()
	copyStream(logs, log, stdout)
}

// moments are the two the daemon itself recorded, rather than a clock on this side, so
// that the same task read twice reports the same pair.
//
// They are asked for whatever became of the task's context, since the container has
// exited and its span is part of its ending. Where the daemon cannot say, the span is the
// one this side can vouch for, from the dispatch to the moment the exit was read, which
// the key is written down with so that a second report says the same. A container that
// exited with no span at all would read as one that never started, and its exit code would
// go with the span: a success would read as a failure no retry names, and a transient
// failure as one that is never retried, for a question the daemon did not answer.
func (d *Docker) moments(ctx context.Context, t graph.Task, container string, dispatched time.Time) (started, finished time.Time) {
	ask, cancel := context.WithTimeout(context.WithoutCancel(ctx), removalGrace)
	defer cancel()
	in, err := d.cli.ContainerInspect(ask, container)
	if err == nil && !in.State.StartedAt.IsZero() && !in.State.FinishedAt.IsZero() {
		return in.State.StartedAt, in.State.FinishedAt
	}
	d.say("driver: task " + string(t.ID) + ": the daemon did not say when its container ran, so its span is taken from the dispatch to the moment its exit was read")
	return dispatched, d.now()
}

// openLog opens the task's log sink, and answers with a close that is safe to call
// whether or not one was opened.
func (d *Docker) openLog(ctx context.Context, t graph.Task) (io.Writer, func(), error) {
	if d.cfg.Logs == nil {
		return nil, func() {}, nil
	}
	sink, err := d.cfg.Logs.OpenLog(ctx, t.ID)
	if err != nil {
		return nil, func() {}, fault(t.Step, ErrDaemonUnreachable, ChargePlatform,
			"the task's log could not be opened: %v", err)
	}
	if sink == nil {
		return nil, func() {}, nil
	}
	return sink, func() { sink.Close() }, nil
}

// store is the artifact store of the task's namespace: the one its sources carry, or the one
// Config opens for the namespace.
func (d *Docker) store(ctx context.Context, t graph.Task) (*artifact.Store, error) {
	if s := sourcesOf(ctx).Store; s != nil {
		// Config.Store is asked for the task's namespace, so what it answers is that
		// namespace's by construction. A store handed over already opened is not, and a
		// runner that paired a task with another task's store would upload one
		// namespace's outputs under another's prefix.
		if s.Namespace() != t.Namespace {
			return nil, fault(t.Step, nil, ChargePlatform,
				"the artifact store this task was given is namespace %s's and the task is namespace %s's, and an artifact never crosses a namespace boundary", s.Namespace(), t.Namespace)
		}
		return s, nil
	}
	if d.cfg.Store == nil {
		return nil, fault(t.Step, ErrContractBroken, ChargePlatform,
			"this driver was built with no artifact store, and what a container leaves under %s is uploaded to one", brick.OutFilesDir)
	}
	s, err := d.cfg.Store(t.Namespace)
	if err != nil {
		return nil, fault(t.Step, ErrContractBroken, ChargePlatform,
			"the artifact store of namespace %s could not be opened: %v", t.Namespace, err)
	}
	return s, nil
}

// runOf is the run the task belongs to, which is what /agk/run.json carries.
func (d *Docker) runOf(ctx context.Context, t graph.Task) (agk.Run, error) {
	if d.cfg.Runs == nil {
		// A driver with no run source still writes the file, out of what the
		// task itself carries. The run is the same run either way; what is
		// missing is the trigger and who asked, which no task carries.
		return agk.Run{ID: t.Run, Workflow: t.Workflow, Namespace: t.Namespace, Commit: t.Commit}, nil
	}
	run, err := d.cfg.Runs(ctx, t.Run)
	if err != nil {
		return agk.Run{}, fault(t.Step, ErrContractBroken, ChargePlatform,
			"run %s could not be read, and %s carries it: %v", t.Run, RunPath, err)
	}
	return run, nil
}

// repo is the path of the workflow repository tree at the task's commit: the one its sources
// name, or the one Config prepares.
func (d *Docker) repo(ctx context.Context, t graph.Task) (string, error) {
	if repo := sourcesOf(ctx).Repo; repo != "" {
		return repo, nil
	}
	if d.cfg.Repo == nil {
		return "", nil
	}
	repo, err := d.cfg.Repo(ctx, t.Namespace, t.Workflow, t.Commit)
	if err != nil {
		return "", fault(t.Step, ErrContractBroken, ChargePlatform,
			"the workflow repository at commit %s could not be prepared, and it is mounted at %s: %v", t.Commit, RepoDir, err)
	}
	return repo, nil
}

// values are what the masker of an adopted container needs and all it needs: the values
// the first delivery wrote for the container, where they are still on this host, and the
// ones this delivery's own source redeems. Nothing is written.
//
// Both, because a secret rotated between the two redemptions leaves the container holding
// the first value and this delivery the second, and the container can print only the
// first: masked with the second alone, it would reach the log and the published outputs
// in the clear. The second is kept for the host that no longer has the first, a tmpfs a
// restart cleared, where it is the closest to the container's there is.
func (d *Docker) values(ctx context.Context, t graph.Task) ([][]byte, error) {
	if len(t.Secrets) == 0 {
		return nil, nil
	}
	// The directory is named from the task identifier rather than read off the
	// container, as the one Run takes away is: a container carrying the task's label
	// may have been started by anything, and what is read here is only ever this
	// runner's own.
	w, err := workdirFor(d.cfg.WorkRoot, t.ID, d.cfg.Policy.SecretsDir)
	if err != nil {
		w = nil
	}
	var values [][]byte
	missing := ""
	for _, s := range t.Secrets {
		if value, ok := w.written(s); ok {
			values = append(values, value)
		} else if missing == "" {
			missing = s.Name
		}
	}

	secrets := d.secrets(ctx)
	if secrets == nil {
		if missing == "" {
			return values, nil
		}
		// A server runner leaves Config.Secrets nil and gives each task its own, so a
		// redelivery that came without them to a host that no longer holds what the
		// first delivery wrote would otherwise mask nothing. It is refused for the
		// reason a value that cannot be redeemed is, and as writeSecrets refuses the
		// same omission on a delivery that creates its container: the runner's, with
		// no rule of the brick contract, which no image had a part in.
		return nil, fault(t.Step, nil, ChargePlatform,
			"secret %s is no longer where the first delivery wrote it and there is no secret source: masking is a literal match against the values the task was given, and a runner gives them with the task it runs", missing)
	}
	for _, s := range t.Secrets {
		value, err := secrets.Value(ctx, s.Name)
		if err != nil {
			// A value that cannot be redeemed is a value the masker cannot
			// see, and a log written without it would carry the secret in
			// the clear. Refusing is the safe direction.
			return nil, fault(t.Step, ErrContractBroken, ChargePlatform,
				"secret %s could not be redeemed, and masking is a literal match against the values the task was given: %v", s.Name, err)
		}
		values = append(values, value)
	}
	return values, nil
}

// abandon takes away what an earlier delivery of a refused task left: its container, its
// network and its working directory. A key this host carried under a build that ran the
// posture comes back to one that refuses it, on a daemon too old to keep the host out of an
// internal network, and the refusal would otherwise leave that container running with
// nothing left to stop it and its secrets on the host.
func (d *Docker) abandon(ctx context.Context, t graph.Task) {
	found, err := d.containerOf(ctx, t.ID)
	if err != nil || found == "" {
		return
	}
	tidy, cancel := context.WithTimeout(context.WithoutCancel(ctx), removalGrace)
	defer cancel()
	if err := d.cli.ContainerRemove(tidy, found, true); err != nil && !docker.IsNotFound(err) {
		d.say(fmt.Sprintf("%s was refused, and the container an earlier delivery of task %s left was not removed, so it runs on until somebody removes it: %v", t.Step, t.ID, err))
		return
	}
	d.removeNetwork(ctx, t, networkOf(t))
	if w, err := workdirFor(d.cfg.WorkRoot, t.ID, d.cfg.Policy.SecretsDir); err == nil {
		d.tidy(t, w)
	}
}

// removeNetwork takes a task's network away, the container on it being gone, and says
// what it could not take away. Said and not returned, for the reason tidy says a directory:
// the task has ended by now, and a network left behind changes nothing about how.
func (d *Docker) removeNetwork(ctx context.Context, t graph.Task, n network) {
	tidy, cancel := context.WithTimeout(context.WithoutCancel(ctx), removalGrace)
	defer cancel()
	if err := removeNetwork(tidy, d.cli, n); err != nil {
		d.say(fmt.Sprintf("%s left the network %s of task %s on this host, where it holds its share of the daemon's address pools until the runner's next start sweeps it: %v", t.Step, n.Mode, t.ID, err))
	}
}

// tidy takes a task's working directory away and says what it could not take away.
//
// It is said and not returned, because by the time a directory is removed the task has
// ended one way or the other and a removal cannot change which. It is said every time and
// not once, since each is a directory of its own left on this host, and the step is named
// first for the reason announceSecrets names it.
func (d *Docker) tidy(t graph.Task, w *workdir) {
	if err := w.remove(); err != nil {
		d.say(fmt.Sprintf("%s left files on this host that were not removed with its container, so what task %s was given and what its brick wrote survive into the tasks after it until somebody removes them: %v", t.Step, t.ID, err))
	}
}
