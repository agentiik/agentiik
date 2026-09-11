package graph

import (
	"fmt"
	"slices"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// What happened is read and never invented. A container exited with a code, the exit
// code table says what that code means, and the verdict of the shard is what the table
// says: nothing in the manifest declares the codes a brick may exit with, so there is
// nothing else to read it against and no room for an opinion beside it.
//
// From there it is two reductions, the second narrower than the first. The shards of a
// step reduce to the step's verdict. The steps of a run reduce to the run's verdict,
// which is where continue_on_error is read and nowhere else: a step that failed is
// failed and says so, and the keyword decides only whether the run hears about it.
//
// Beside them is what a step publishes, which is not a reduction of anything: a step
// ends by putting one envelope on each port its outputs name, whatever the verdict, and
// the workflow's own outputs then name step ports, which is what a finished run hands
// back.

// failureOf names the failure a finished task suffered, and says whether it is one a
// retry policy could ever name.
//
// The state says which of the four kinds it is and the exit code settles the one the
// state cannot: lost and timeout are things that happened to the task, while a container
// that exited said itself what kind of failure it was. Success has no name because it is
// not a failure, and neither has a failure the table calls permanent: invalid input is
// never retried whatever retry says, and the runner bands are not the step's to retry.
//
// A task reported failed whose code the table calls success is one of those. It is still
// failed, because the driver saw the failure, and it names no kind, so no policy can ask
// for it again: a failure nobody can name is a failure nobody can retry.
//
// The code read here is the one the step exited with. after_script runs in the same
// container even when script failed, so that a diagnostic dump survives a failure, and
// its own exit code does not change the step's verdict; keeping that true is the
// driver's, because it is the driver that watches the two and reports one.
func failureOf(task agk.TaskState, exit int) (agk.Failure, bool) {
	switch task {
	case agk.TaskLost:
		return agk.FailureLost, true
	case agk.TaskTimedOut:
		return agk.FailureTimeout, true
	case agk.TaskSucceeded, agk.TaskFailed:
		return agk.Band(exit).Failure()
	default:
		return 0, false
	}
}

// shardVerdict reduces one task to what became of the shard it ran.
//
// The exit code outranks the driver's word for it. A task reported succeeded whose code
// the table does not call success is a failed shard, because the table is the only place
// a verdict comes from and a runner mapping its own exit codes would otherwise be able
// to publish a failure as a success.
func shardVerdict(task agk.TaskState, exit int) agk.Verdict {
	switch task {
	case agk.TaskPending:
		return agk.VerdictPending
	case agk.TaskDispatched, agk.TaskRunning, agk.TaskPublishing:
		return agk.VerdictRunning
	case agk.TaskSucceeded:
		if agk.Band(exit) == agk.BandSuccess {
			return agk.VerdictSucceeded
		}
		return agk.VerdictFailed
	case agk.TaskCancelled:
		return agk.VerdictCancelled
	default:
		// Failed, lost and timed out are all failures of the shard. Which of
		// them it was is what failureOf answers, for the retry policy; the step
		// only needs to know that this shard did not produce.
		return agk.VerdictFailed
	}
}

// stepVerdict reduces the shards of one step to the verdict of the step.
//
// A shard with another attempt coming is not finished, however its last attempt ended: a
// step whose second attempt is due in thirty seconds is still running, and reporting it
// failed would let a downstream when: [failed] start on a failure that is about to be
// tried again.
//
// A failure outranks a cancellation. Under fail_fast a failing shard is what cancels its
// siblings, so a step that reported both failed, and saying it was cancelled would name
// the consequence rather than the cause.
//
// Nothing here reads continue_on_error. A step that failed is failed, and says so to
// every downstream when; what the keyword settles is the run verdict alone.
//
// A step with no shards has run nothing and failed at nothing. That is a fan-out over an
// empty batch, which starts no containers, and its ports publish empty envelopes like
// any other step that wrote none. The caller asks this only of a step that has started,
// because a step nobody has reached is pending and has no shards either.
func stepVerdict(shards []ShardState) agk.Verdict {
	verdict := agk.VerdictSucceeded
	for _, sh := range shards {
		if !sh.NextAttemptAt.IsZero() {
			return agk.VerdictRunning
		}
		switch v := shardVerdict(sh.Task, sh.ExitCode); v {
		case agk.VerdictPending, agk.VerdictRunning:
			return agk.VerdictRunning
		case agk.VerdictFailed:
			verdict = agk.VerdictFailed
		case agk.VerdictCancelled:
			if verdict != agk.VerdictFailed {
				verdict = agk.VerdictCancelled
			}
		}
	}
	return verdict
}

// runVerdict reduces the steps of a run to the state of the run, which for a finished
// run is its verdict: succeeded, failed, cancelled or timed_out.
//
// tolerates says whether a step carries continue_on_error. It is asked of the caller
// rather than read here because the answer is in the workflow file and this is a
// reduction over the state, and it is asked only about a step that failed.
//
// Two of the four verdicts are not reductions at all. Cancelled and timed_out are things
// that happened to the run as a whole, at a moment, and they are recorded on the run
// when they happen; a step failing because its container was stopped is the consequence
// of that moment and does not rewrite it. So a run already fixed at one of them keeps
// it, which is the precedence the documentation leaves open: whichever was recorded
// first stands, and a failure that follows never outranks either.
//
// The cancellation a merge: first causes is a cancellation of the steps it abandoned and
// not of the run. Reading it as a cancelled run would report cancelled on a run where a
// barrier lifted early and everything the author asked for happened.
func runVerdict(s *State, tolerates func(agk.Step) bool) agk.RunState {
	if s.Run.State.Terminal() {
		return s.Run.State
	}
	// A run whose steps are not there yet has not finished them. Every step of the
	// graph gets an entry when the run starts, so an empty map is a state that has
	// not been built rather than a graph with nothing in it, and answering succeeded
	// to it would report a run that never ran as a run that went well.
	if len(s.Steps) == 0 {
		return s.Run.State
	}

	var started, unfinished, failed bool
	for name, st := range s.Steps {
		if st.Verdict != agk.VerdictPending {
			started = true
		}
		if !st.Verdict.Terminal() {
			unfinished = true
			continue
		}
		// Failed beyond tolerance is the documentation's phrase for the one
		// thing that fails a run, and continue_on_error is the tolerance.
		if st.Verdict == agk.VerdictFailed && (tolerates == nil || !tolerates(name)) {
			failed = true
		}
	}

	switch {
	case unfinished:
		if s.Run.State == agk.Waiting {
			return agk.Waiting
		}
		if !started && s.Run.State == agk.Queued {
			return agk.Queued
		}
		return agk.Running
	case failed:
		return agk.Failed
	default:
		return agk.Succeeded
	}
}

// publish is what a step puts on its ports when it ends: exactly one envelope on each
// port its outputs name, published once, whatever the fan-out was and whatever the
// verdict is.
//
// A step split into shards has its shard envelopes concatenated port by port before
// publication, which is agk.Concat and not a second answer to what concatenation is: the
// items keep their order and their identifiers, the metadata is rebuilt, and the size
// rules are measured on the result because the result is what travels. The shards are
// read in index order, which is the order they were split into.
//
// A declared port no shard wrote publishes an empty envelope. That is not an error: it
// is what lets a brick write only the port it has something to say on, and the step below
// reads a batch of nothing and decides its own fate through its own condition. A port no
// output names is not published at all, because outputs is what a step publishes; a
// container that wrote one was refused by brick.Collect long before this.
//
// Which ports those are is portsOf, and it is not always outputs. A step that calls a
// sub-workflow declares none of its own, and reading that absence as a step that publishes
// nothing would drop every envelope the sub-run produced and leave every edge below it
// resolving to a batch of nothing.
//
// It is called for every step that ends and not only for one that succeeded. The barrier
// downstream is a barrier: a step reached through when: [failed] has input ports that
// have to be satisfied before it can start, and a failed step that published nothing
// would make the error path the keyword exists for unreachable.
//
// The publication is attributed to the highest attempt any shard needed, which is what
// Concat does with the shards it is given and what this does with the ports none of them
// wrote. A step that ran no attempt at all publishes as attempt 1, because an envelope
// carries an attempt and the count starts there.
func publish(name agk.Step, st *Step, shards []ShardState, run agk.RunID, at time.Time, l agk.Limits) (map[agk.Port]agk.Envelope, error) {
	ordered := slices.Clone(shards)
	slices.SortStableFunc(ordered, func(a, b ShardState) int { return a.Shard.Index - b.Shard.Index })

	attempt := 1
	for _, sh := range ordered {
		attempt = max(attempt, sh.Attempt)
	}

	ports := portsOf(st, ordered)
	out := make(map[agk.Port]agk.Envelope, len(ports))
	for _, port := range ports {
		var pieces []agk.Envelope
		for _, sh := range ordered {
			if e, ok := sh.Ports[port]; ok {
				pieces = append(pieces, e)
			}
		}
		if len(pieces) == 0 {
			out[port] = agk.Empty(run, name, port, attempt, at)
			continue
		}
		e, err := agk.Concat(pieces, l)
		if err != nil {
			return nil, fmt.Errorf("graph: step %s: port %s: %w", name, port, err)
		}
		out[port] = e
	}
	return out, nil
}

// portsOf names the ports a step publishes.
//
// For a step that runs a container it is outputs, exactly, because outputs is what a step
// publishes and the manifest subset rule has already held that list to the brick. A step
// that calls a sub-workflow declares none: "its ports are the declared outputs of the
// workflow it calls, which this commit does not carry and this package never fetches", so
// what came back is the only statement of them there is.
//
// A call that came back with nothing publishes nothing, and that is the honest answer
// rather than an omission: this commit cannot name the callee's ports, so it cannot mint
// the empty envelopes a brick step gets for the ports it declared and did not write. The
// barrier below reads a port that never arrived as a batch of nothing all the same.
//
// The ports of a call are put in name order, so that a run and its replay publish the same
// list whatever order a map was walked in.
func portsOf(st *Step, shards []ShardState) []agk.Port {
	if st.Call == nil || len(st.Outputs) > 0 {
		return st.Outputs
	}
	var ports []agk.Port
	for _, sh := range shards {
		for port := range sh.Ports {
			if !slices.Contains(ports, port) {
				ports = append(ports, port)
			}
		}
	}
	slices.Sort(ports)
	return ports
}

// outputOf picks the envelope one declared workflow output names.
//
// An output is one step port and never several: an output that concatenated two would
// hide which step actually produced what. The port is read from what the step published,
// which is why it is available for a step that was skipped or that failed as well as for
// one that succeeded, and why it is not available before the step ends.
func outputOf(s *State, from agk.Step, port agk.Port) (agk.Envelope, error) {
	st, ok := s.Steps[from]
	if !ok {
		return agk.Envelope{}, fmt.Errorf("output from step %s: the run holds no step of that name", from)
	}
	if !st.Verdict.Terminal() {
		return agk.Envelope{}, fmt.Errorf("output from step %s: the step is %s, and a port publishes once, when the step ends", from, st.Verdict)
	}
	e, ok := st.Ports[port]
	if !ok {
		return agk.Envelope{}, fmt.Errorf("output from step %s: the step published no port %s", from, port)
	}
	return e, nil
}
