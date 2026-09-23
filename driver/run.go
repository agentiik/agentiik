package driver

import (
	"context"
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
// caught. Refuse what cannot run at all. Resolve the image and read its manifest. Adopt
// by label or prepare and create. Open the wait with condition=next-exit before the
// start, which is what makes the exit-during-attach race unrepresentable. Attach, start,
// write the envelope on standard input from its own goroutine and half-close. Read the
// demultiplexed stream, keeping standard output for the shorthand and passing standard
// error through the masker into the log. Take the exit code from the wait that was
// already open, or from the event stream where the wait missed it, or from an inspect
// under both. Collect, upload, spill. Then remove the container, the network and the
// working directory, in defers that run on every path.
//
// A graph.Result means a container ran. An error means none did, and it names the step
// and the rule. No exit code is invented for a failure that produced none, because a
// driver reporting its own trouble as a brick failure fails somebody else's step.
func (d *Docker) Run(ctx context.Context, t graph.Task) (graph.Result, error) {
	if t.Call != nil && t.Image == "" {
		return graph.Result{}, fault(t.Step, ErrContractBroken, ChargeBrick,
			"the step is a call to another workflow, which the evaluator expands and no container runs")
	}

	store, err := d.store(t)
	if err != nil {
		return graph.Result{}, err
	}

	// Said here rather than when the daemon was opened, because this is the first
	// moment it is true of anything: a task with no secret never has a value written
	// for it, and a person who reads the sentence on a run that declares none learns
	// to scroll past it.
	if len(t.Secrets) > 0 {
		d.floor.announceSecrets(d.cfg.Policy, t.Step, d.say)
	}

	// The task is held from here, before anything is pulled or created, so that a stop
	// arriving while it is being prepared lands on something. That window is the image
	// pull and it is minutes wide on a cold registry; a stop answered nil inside it
	// would leave the container to be created afterwards and to run to its deadline.
	done, ok := d.register(t.ID, &held{})
	if !ok {
		return graph.Result{}, fault(t.Step, ErrTaskInFlight, ChargePlatform, "task %s", t.ID)
	}
	defer done()

	// Everything that can be refused without creating anything is refused first, and
	// network: egress is the one that matters: a workflow must not be able to
	// believe its egress.allow list is being enforced when nothing is enforcing it.
	n, err := networkFor(ctx, d.cli, t)
	if err != nil {
		return graph.Result{}, err
	}
	defer func() {
		tidy, cancel := context.WithTimeout(context.WithoutCancel(ctx), removalGrace)
		defer cancel()
		removeNetwork(tidy, d.cli, n)
	}()

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
			defer w.remove()
		}
		defer func() {
			tidy, cancel := context.WithTimeout(context.WithoutCancel(ctx), removalGrace)
			defer cancel()
			d.cli.ContainerRemove(tidy, adopted, true)
		}()
		return d.rejoin(ctx, t, store, adopted, image)
	}

	w, err := newWorkdir(d.cfg.WorkRoot, t.ID, d.cfg.Policy.SecretsDir)
	if err != nil {
		return graph.Result{}, err
	}
	defer w.remove()

	run, err := d.runOf(ctx, t)
	if err != nil {
		return graph.Result{}, err
	}
	repo, err := d.repo(ctx, t)
	if err != nil {
		return graph.Result{}, err
	}

	given, err := prepare(ctx, t, w, d.cfg.Policy, store, run, repo, d.cfg.Secrets)
	if err != nil {
		return graph.Result{}, err
	}
	// The ownership comes after the contents and before the create. The runner
	// "prepares each task's working directory with ownership inside the remapped
	// range before it creates the container", and a chown taken before the envelope,
	// the run context and the secrets were written would leave every one of them
	// owned by this process instead of by the range the container's processes live
	// in.
	if err := d.floor.ownWorkdir(w); err != nil {
		return graph.Result{}, err
	}

	// The dispatch is the creation and not the planning, because the deadline runs
	// from the moment the work became somebody's. It is taken immediately before the
	// create rather than immediately after, because the container has to be told the
	// same moment the watch will stop it at, and the environment is composed here.
	dispatched := d.now()

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

	return d.carry(ctx, t, store, created.ID, image, given, w.Out, dispatched, true)
}

// carry is the half of Run that a container exists for: wait, attach, start, read, exit,
// collect. It is separate so that the adoption path joins it at the same point.
//
// fresh says the container was created by this delivery and has never run, which decides
// two things at once: the envelope goes on its standard input, and a stop that landed
// before it existed means it is never started at all.
func (d *Docker) carry(ctx context.Context, t graph.Task, store *artifact.Store, container string, image resolved, given *given, out string, dispatched time.Time, fresh bool) (graph.Result, error) {
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
	waited, err := d.cli.ContainerWait(ctx, container, docker.WaitNextExit)
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

	if err := d.cli.ContainerStart(ctx, container); err != nil {
		if docker.IsUnreachable(err) {
			return graph.Result{}, fault(t.Step, ErrDaemonUnreachable, ChargePlatform, "starting the container: %v", err)
		}
		return graph.Result{}, fault(t.Step, ErrContractBroken, ChargePlatform, "the container could not be started: %v", err)
	}
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

	d.observe(ctx, Event{Task: t.ID, State: agk.TaskPublishing, Container: container})

	state := readExit(log, e.Code)
	if override := e.state(); override != state {
		// A deadline that fired and a stop that landed are what the task was,
		// whatever code the kill left behind: the code of a killed container says
		// how it was killed and not why.
		state = override
	}

	result := graph.Result{Task: t.ID, State: state, DispatchedAt: dispatched}
	if state == agk.TaskSucceeded || state == agk.TaskFailed {
		result.ExitCode = e.Code
	}
	result.StartedAt, result.FinishedAt = d.moments(ctx, container)

	var artifacts []agk.File
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
		if err != nil {
			return graph.Result{}, fault(t.Step, ErrContractBroken, ChargeBrick, "%v", err)
		}
		result.Outputs, artifacts = got.Outputs, got.Artifacts
	}

	ref, logErr := log.finish()
	if logErr != nil {
		// A sink that failed is the runner's trouble and not the container's, so
		// the task is not failed for it and somebody is told.
		d.say("driver: task " + string(t.ID) + ": the log sink failed: " + logErr.Error())
	}
	d.observe(ctx, Event{
		Task: t.ID, State: state, Container: container,
		Log: ref, Artifacts: artifacts,
		Usage: Usage{ImagePullMS: image.PullMillis},
	})
	return result, nil
}

// rejoin re-attaches to a container this driver already started, which a redelivered
// task finds by its label.
//
// It does not prepare anything: the working directory, the envelope and the secret files
// are the first delivery's and are still there. Where they are is read off the container
// itself, which is the one place that cannot disagree with what the container was
// actually given.
func (d *Docker) rejoin(ctx context.Context, t graph.Task, store *artifact.Store, container string, image resolved) (graph.Result, error) {
	in, err := d.cli.ContainerInspect(ctx, container)
	if err != nil {
		return graph.Result{}, fault(t.Step, ErrDaemonUnreachable, ChargePlatform,
			"a container carrying this task's label could not be inspected: %v", err)
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
		return graph.Result{}, fault(t.Step, ErrContractBroken, ChargePlatform,
			"the container adopted for this task has nothing bound at %s, so there is nowhere to collect its outputs from", brick.OutDir)
	}

	// The values are redeemed again rather than remembered, because the masker needs
	// them and the first delivery's copy of them left with the process that had it.
	// Nothing is written: this is the list the literal match runs against.
	values, err := d.values(ctx, t)
	if err != nil {
		return graph.Result{}, err
	}

	dispatched := in.State.StartedAt
	if dispatched.IsZero() {
		dispatched = d.now()
	}
	// The envelope was written on standard input by the delivery that started this
	// container, and its write half was closed after it. A second attach asking for
	// standard input would have nothing to write and an already closed pipe to write
	// it to.
	return d.carry(ctx, t, store, container, image, &given{Values: values}, out, dispatched, false)
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
func (d *Docker) moments(ctx context.Context, container string) (started, finished time.Time) {
	in, err := d.cli.ContainerInspect(ctx, container)
	if err != nil {
		return time.Time{}, time.Time{}
	}
	return in.State.StartedAt, in.State.FinishedAt
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

// store opens the artifact store of the task's namespace.
func (d *Docker) store(t graph.Task) (*artifact.Store, error) {
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

// repo is the path of the workflow repository tree at the task's commit.
func (d *Docker) repo(ctx context.Context, t graph.Task) (string, error) {
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

// values redeems the secrets of one task without writing any of them, which is what the
// masker needs and all it needs.
func (d *Docker) values(ctx context.Context, t graph.Task) ([][]byte, error) {
	if d.cfg.Secrets == nil {
		return nil, nil
	}
	var values [][]byte
	for _, s := range t.Secrets {
		value, err := d.cfg.Secrets.Value(ctx, s.Name)
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
