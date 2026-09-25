package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
)

// resultQueue is how many finished tasks are held between two passes of the loop. A send
// that finds it full blocks the goroutine that finished, which costs nothing: its container
// is gone, its directory is removed, and all it is holding is a value to hand over.
const resultQueue = 64

// Request is one run to evaluate: the graph, the tree the containers see, what the trigger
// supplied, and where the narration goes.
//
// The layout, the limits and the daemon are the session's, because the driver was
// configured with them when the handle was made.
type Request struct {
	// Graph is the resolved graph, which is what Resolve returns and what Build held to
	// every manifest.
	Graph *graph.Graph

	// Tree is the working tree, bound read-only at /agk/repo. It is the tree as it is,
	// unmodified, which is the whole point of a local run: what ran is what is on the
	// disk rather than what was committed.
	Tree string

	// Inputs are the workflow inputs, already held to their declared schemas by package
	// schema with required and default applied, which is what graph.Options.Inputs
	// expects. Vars are the workflow's own, merged.
	Inputs map[string]any
	Vars   map[string]any

	// Secrets are the values the command line supplied, by the name the secrets block
	// lists them under. They are mounted exactly as a server run mounts them, through the
	// driver: a file bound read-only at /agk/secrets/<name>, masked out of the log and
	// out of the payload, never an environment variable.
	Secrets map[string][]byte

	// Events is where the run narrates itself, one value per transition, delivered from
	// the goroutine that calls Next and Record so that the order on the screen is the
	// order the evaluator saw.
	Events func(Event)
}

// Outcome is what became of one run: the run itself, the state it ended in, the envelopes
// its declared outputs name, and what failed.
//
// State is always there, including for a run that failed, because the envelopes every step
// did publish are in it and an incident is read out of it. Outputs is filled only for a run
// that succeeded: an output is a view of a step port, and a step that did not end has no
// port to take a view of.
type Outcome struct {
	Run      agk.Run
	State    *graph.State
	Outputs  map[string]agk.Envelope
	Failures []Failure
}

// Run evaluates one graph against the daemon this session holds.
//
// It is the loop the evaluator deliberately does not have, and the five rules it follows
// are in doc.go: ask, record the dispatch before starting anything, start exactly what the
// plan says, issue every stop the plan names and then block on a Result or the clock, and
// end when the run is terminal and nothing is outstanding.
//
// An error means the run could not be evaluated at all: a state the evaluator refused, a
// working directory that could not be prepared. A run that failed is not an error, it is an
// Outcome whose State says so, which is the same line the driver draws between a Result and
// an error and the line cmd/agk carries to the shell.
func (s *Session) Run(ctx context.Context, r Request) (Outcome, error) {
	if r.Graph == nil {
		return Outcome{}, fmt.Errorf("local: there is no graph to run: Resolve builds one from a workflow the daemon's manifests have held")
	}
	if r.Tree == "" {
		return Outcome{}, fmt.Errorf("local: there is no working tree to bind at %s: a local run mounts the tree the workflow sits in", driver.RepoDir)
	}
	// The tree is made absolute because it is a bind mount source, and a bind mount
	// source is a path on the host: a relative one would be resolved against whatever
	// directory the daemon happens to run in.
	tree, err := filepath.Abs(r.Tree)
	if err != nil {
		return Outcome{}, fmt.Errorf("local: the working tree %s could not be resolved: %w", r.Tree, err)
	}

	wf := r.Graph.Workflow()
	run := agk.Run{
		ID:        agk.NewRunID(),
		Workflow:  wf.Metadata.Namespace + "/" + wf.Metadata.Name,
		Namespace: wf.Metadata.Namespace,

		// The commit is empty, and it is load bearing twice. The driver's
		// environment table drops a variable with nothing to carry, so AGK_COMMIT is
		// absent rather than empty; and a run with no commit is a run nobody can
		// mistake for a version of a repository, which a local run of a dirty tree
		// must not be mistakable for.
		Commit: "",

		Trigger: agk.TriggerManual,

		// The label, in the same word as the flag, travelling into /agk/run.json that
		// every container reads and into run.json on disk, which is the one place a
		// history looks.
		TriggeredBy: "local",
	}

	release, err := s.hold(&inflight{run: run, tree: tree, secrets: r.Secrets})
	if err != nil {
		return Outcome{}, err
	}
	defer release()

	if err := s.layout.prepare(run.ID); err != nil {
		return Outcome{}, err
	}

	ev, err := graph.Start(r.Graph, run, graph.Options{
		Inputs: r.Inputs,
		Vars:   r.Vars,
		Limits: s.limits,
	}, s.clock())
	if err != nil {
		return Outcome{}, err
	}
	// The record is written before the first container, because the run.json a
	// container reads and the run.json a person reads are the same value and the
	// container gets it first.
	s.keep(ev.State().Run)
	if err := s.writeRun(ev.State().Run); err != nil {
		return Outcome{}, err
	}

	failures, err := s.loop(ctx, ev, r)
	state := ev.State()

	// Everything that is known is written down even where the loop ended badly: a run
	// that could not be evaluated to the end still leaves the state it reached, which is
	// the thing somebody debugging reads.
	s.keep(state.Run)
	writeRun := s.writeRun(state.Run)
	writeState := s.writeState(state)
	if err != nil {
		return Outcome{}, err
	}
	if writeRun != nil {
		return Outcome{}, writeRun
	}
	if writeState != nil {
		return Outcome{}, writeState
	}

	out := Outcome{Run: state.Run, State: state, Failures: failures}
	if state.Run.State == agk.Succeeded {
		if out.Outputs, err = s.outputs(ev); err != nil {
			return Outcome{}, err
		}
	}
	return out, nil
}

// loop is the whole of what this package adds to the evaluator.
//
// The tasks run on a context the interrupt does not reach. That is deliberate and it is
// what makes a cancelled run report cancelled: an interrupt calls Evaluator.Cancel, the
// next Plan says stop everything, driver.Stop sends the daemon's own stop with the policy
// grace, and each Run comes back with a cancelled Result. Tasks bound to the interrupted
// context would instead come back as errors about a context, and a container stopped by the
// daemon would be reported as this side's trouble.
func (s *Session) loop(ctx context.Context, ev *graph.Evaluator, r Request) ([]Failure, error) {
	taskCtx, stopTasks := context.WithCancel(context.WithoutCancel(ctx))
	defer stopTasks()

	results := make(chan finished, resultQueue)
	// abandoned releases the goroutines of a loop that left early, which is a loop that
	// could not evaluate the run at all. Their containers are already being stopped by the
	// cancelled task context, and a Result nobody is going to read is a goroutine that
	// would otherwise sit on a channel send for the life of the process.
	abandoned := make(chan struct{})
	defer close(abandoned)

	refused := map[agk.TaskID]trouble{}
	narrator := newNarrator(r.Graph, r.Events)
	outstanding := 0

	// stopping holds every stop sent for a task this loop started, until the task comes back,
	// and each is sent again on every pass. The evaluator names a stop sent while the run goes
	// on once, in the pass that ends its task, and one sent once can miss: the driver may refuse
	// it, and it answers nil for a task whose container it has not reached yet, since the
	// goroutine that runs it has only just started. Its container would then run to its end or
	// its deadline. This is the loop's part of what the heartbeat's cancel is to a server.
	started := map[agk.TaskID]bool{}
	stopping := map[agk.TaskID]graph.Stop{}

	// The interrupt is read once. After Cancel the channel stays closed, so a loop that
	// kept reading it would spin instead of waiting for the containers it just called
	// off. A second interrupt is the command line's to act on, not this loop's.
	interrupted := ctx.Done()

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		now := s.clock()
		plan, err := ev.Next(now)
		if err != nil {
			// An error from Next is a broken state and not a failed run: the run
			// states have no word for a run whose Next returned an error, so this
			// ends the evaluation rather than the run.
			return nil, fmt.Errorf("local: run %s: %w", ev.State().Run.ID, err)
		}

		// Every stop the plan names, in the order it named them, every time it
		// appears, and then every one sent before for a task still out. Stop is
		// idempotent by design and a stop for a task that already finished is the
		// ordinary consequence of at-least-once delivery.
		stops := slices.Clone(plan.Stop)
		for _, task := range slices.Sorted(maps.Keys(stopping)) {
			if !slices.ContainsFunc(stops, func(s graph.Stop) bool { return s.Task == task }) {
				stops = append(stops, stopping[task])
			}
		}
		for _, stop := range stops {
			if started[stop.Task] {
				stopping[stop.Task] = stop
			}
			if err := s.tasks.Stop(taskCtx, stop); err != nil {
				s.say("the stop of task " + string(stop.Task) + " was refused: " + err.Error())
			}
		}

		// The dispatch is recorded before the goroutine starts, which is the one rule
		// this loop must not get wrong: a planned shard stays pending until a Result
		// moves it, the evaluator plans every pending shard on every pass, and a loop
		// that started a container without recording the dispatch would start the
		// same container again on the next pass. Recording it is also what fixes the
		// task's deadline, because the deadline runs from the moment the work became
		// somebody's.
		for _, task := range plan.Start {
			dispatched := s.clock()
			if err := ev.Record(graph.Result{
				Task:         task.ID,
				State:        agk.TaskDispatched,
				DispatchedAt: dispatched,
			}, dispatched); err != nil {
				return nil, fmt.Errorf("local: run %s: %w", ev.State().Run.ID, err)
			}
			if err := s.writeState(ev.State()); err != nil {
				return nil, err
			}
			outstanding++
			started[task.ID] = true
			go func(t graph.Task) {
				result, err := s.tasks.Run(taskCtx, t)
				select {
				case results <- finished{task: t, result: result, err: err}:
				case <-abandoned:
				}
			}(task)
		}

		narrator.narrate(ev.State(), s.clock())

		if ev.State().Run.State.Terminal() && outstanding == 0 {
			// What the driver said about the tasks that have just ended is still
			// on the queue, because a Result and the observations before it arrive
			// by two routes. Draining it here is what keeps the narration of a run
			// complete rather than ending one transition short.
			s.drain(narrator, ev.State())
			return failures(ev.State(), r.Graph, refused, s.layout), nil
		}

		var wake <-chan time.Time
		if !plan.Wake.IsZero() {
			after := plan.Wake.Sub(s.clock())
			if after < 0 {
				after = 0
			}
			timer.Reset(after)
			wake = timer.C
		}

		// Nothing to start, nothing in flight, nothing on the clock and a run that is
		// not over is a run nothing will ever move. Waiting on it would be a process
		// that hangs with no explanation, so it is refused with the state named: a
		// state that cannot be moved is the broken state Next has no word for either.
		if len(plan.Start) == 0 && outstanding == 0 && wake == nil {
			return nil, fmt.Errorf("local: run %s is %s and nothing is outstanding: the plan starts nothing, stops nothing that answers and waits on no moment, so the run cannot be moved", ev.State().Run.ID, ev.State().Run.State)
		}

		select {
		case done := <-results:
			outstanding--
			delete(started, done.task.ID)
			delete(stopping, done.task.ID)
			if err := s.record(ev, done, refused); err != nil {
				return nil, err
			}
		case event := <-s.observations:
			// The driver's own transitions are not Results and never enter the
			// evaluator: dispatched is this loop's to record, and running and
			// publishing are news about a task the evaluator already knows is in
			// flight. They leave as narration, in the order they arrived.
			narrator.observed(ev.State(), event, s.clock())
		case <-wake:
		case <-interrupted:
			ev.Cancel(s.clock())
			interrupted = nil
			if err := s.writeState(ev.State()); err != nil {
				return nil, err
			}
		}
		if wake != nil {
			timer.Stop()
		}
	}
}

// drain narrates every observation already on the queue and returns. It never waits: what
// has not arrived by now is about a task that has not finished, and there are none.
func (s *Session) drain(narrator *narrator, state *graph.State) {
	for {
		select {
		case event := <-s.observations:
			narrator.observed(state, event, s.clock())
		default:
			return
		}
	}
}

// finished is one task that came back, and whether the driver reported a Result or its own
// trouble.
type finished struct {
	task   graph.Task
	result graph.Result
	err    error
}

// trouble is what the driver said about a task that produced no Result: the sentence, and
// whose failure it was. It is kept because the evaluator takes only a Result and a Result
// has nowhere to carry a sentence, and because the report of a failure names what was
// refused.
type trouble struct {
	refused string
	charge  driver.Charge

	// exited says a container ran to its end before the refusal, so the code the
	// evaluator was given is one the report names.
	exited bool
}

// record takes one finished task back into the evaluator.
//
// A driver that returned an error rather than a Result has to be answered, because the
// evaluator accepts only a Result and a shard left dispatched hangs the run. The driver
// deliberately invents no exit code for a failure that produced no container, so this side
// chooses one, and which one is decided by driver.Charged and not by this code: a platform
// charge is exit 125, which the exit-code table charges to the runner and which agk.Band
// already makes unretryable and nameless to retry.on; a brick charge with no container is
// exit 120, invalid input, which the evaluator itself already uses for a task it could not
// build and which is never retried whatever retry says. A container that exited 0 and whose
// outputs were refused did run, and is recorded with the table's own code for it, 121.
//
// The state is failed and never lost. Lost is the heartbeat's word for a runner that
// stopped reporting, and it means the work may well have finished, which is false for a
// pull that died before a container existed.
func (s *Session) record(ev *graph.Evaluator, done finished, refused map[agk.TaskID]trouble) error {
	result := done.result
	if done.err != nil {
		// An error nobody charged is not the brick's. driver.Charged answers false
		// for an error that did not come from the driver at all, and charging an
		// undecided failure to a brick fails somebody else's step, which is the
		// whole reason the charge exists.
		charge, decided := driver.Charged(done.err)
		if !decided {
			charge = driver.ChargePlatform
		}
		code := brickWithNoContainer
		if charge == driver.ChargePlatform {
			code = platformFailure
		}
		// A container that exited 0 and left outputs the driver refused did run, and
		// the table has a code of its own for it, which is the one reported.
		exited := errors.Is(done.err, driver.ErrOutputsRefused)
		if exited {
			code = driver.ExitContractBroken
		}
		refused[done.task.ID] = trouble{refused: done.err.Error(), charge: charge, exited: exited}
		result = graph.Result{
			Task:     done.task.ID,
			State:    agk.TaskFailed,
			ExitCode: code,
		}
	}
	now := s.clock()
	if err := ev.Record(result, now); err != nil {
		return fmt.Errorf("local: run %s: %w", ev.State().Run.ID, err)
	}
	s.keep(ev.State().Run)
	return s.writeState(ev.State())
}

// The two exit codes this side reports for a task that produced none of its own. Both are
// rows of the exit-code table rather than numbers chosen here.
const (
	// platformFailure is 125, read as an infrastructure failure and charged to the
	// runner rather than to the brick.
	platformFailure = 125

	// brickWithNoContainer is 120, invalid input: a permanent failure, never retried,
	// which is what a manifest that breaks the contract is.
	brickWithNoContainer = 120
)

// outputs writes one envelope per declared workflow output and answers with them.
func (s *Session) outputs(ev *graph.Evaluator) (map[string]agk.Envelope, error) {
	out, err := ev.Outputs()
	if err != nil {
		return nil, err
	}
	run := ev.State().Run.ID
	for name, envelope := range out {
		path := filepath.Join(s.layout.Outputs(run), name+".json")
		doc, err := agk.EncodeValue(envelope)
		if err != nil {
			return nil, fmt.Errorf("local: the workflow output %s could not be written: %w", name, err)
		}
		// The envelope is written as it is encoded everywhere else, because a digest is
		// taken of those bytes elsewhere and two encodings of one envelope would be two
		// envelopes. The newline is for the terminal that cats it.
		if err := write(path, append(doc, '\n')); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// keep records what the run has become, so that the /agk/run.json a container reads says
// running while it is running rather than queued for ever.
func (s *Session) keep(run agk.Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run != nil {
		s.run.run = run
	}
}

// writeRun writes the local run record: the agk.Run every container is given, with local:
// true beside it.
//
// The flag is a member of the file and not a guess a reader has to make. "Label it local so
// a history never mistakes it for a server run" is the task, and a history reads this file.
func (s *Session) writeRun(run agk.Run) error {
	doc, err := json.MarshalIndent(struct {
		agk.Run
		Local bool `json:"local"`
	}{Run: run, Local: true}, "", "  ")
	if err != nil {
		return fmt.Errorf("local: the run record of %s could not be written: %w", run.ID, err)
	}
	return write(s.layout.RunFile(run.ID), append(doc, '\n'))
}

// writeState writes graph.State after every Record.
//
// Not only at the end: the state is a value and writing it costs nothing, so a run that
// dies at three in the morning leaves the thing a second process would resume from.
// Resuming is not claimed at v0.1.0; the file is there because it is free.
func (s *Session) writeState(state *graph.State) error {
	doc, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("local: the state of run %s could not be written: %w", state.Run.ID, err)
	}
	return write(s.layout.State(state.Run.ID), append(doc, '\n'))
}

// write puts one file down whole.
//
// Through a temporary file and a rename, because these are the files a second process reads
// while this one writes them: a state read half written is a state that says a step never
// ran, and a rename on one filesystem is the one operation that is not half done.
func write(path string, doc []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, recordMode); err != nil {
		return fmt.Errorf("local: %s could not be prepared: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("local: %s could not be written: %w", path, err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(doc); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("local: %s could not be written: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("local: %s could not be written: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("local: %s could not be written: %w", path, err)
	}
	return nil
}

// clock is the moment, or the real one. It is a field so that a test can hold time still,
// on the precedent driver.Config.Now already set.
func (s *Session) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// say passes one sentence to whoever is listening, which is the terminal for agk run
// --local and nothing at all for a test.
func (s *Session) say(sentence string) {
	if s.announce != nil {
		s.announce(sentence)
	}
}
