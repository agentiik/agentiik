package graph

import (
	"time"

	"github.com/agentiik/agentiik/agk"
)

// The run, as a value. Everything that changes while a run goes changes here and nowhere
// else: the graph is fixed at validation, the evaluator is a handle, and this is the only
// thing a second process has to be given to carry on where a first one stopped.
//
// That is why it is plain data with a JSON round trip and no pointers into anything. The
// controller says failover is a state resume and never a rebuild: load the State, call
// Next, get the Plan the instance that died would have got. A state that could only be
// rebuilt by replaying the run would make that sentence false.

// StateVersion is the shape this package writes and the only one it reads. A state
// carries it so that a process reading a state written by another version refuses it
// rather than guessing at what a missing field used to mean.
const StateVersion = 1

// State is one run in progress: what it was started with, and what has happened to each
// of its steps.
//
// It restates nothing that sits in the agk.Run it holds. The run identifier, the
// workflow, the namespace, the commit and the state of the run itself are the run's, and
// a second copy here would be a second answer to what a run is.
type State struct {
	Version int            `json:"version"`
	Run     agk.Run        `json:"run"`
	Inputs  map[string]any `json:"inputs,omitempty"`
	Vars    map[string]any `json:"vars,omitempty"`

	// Trigger and Event are what started the run: "body, headers, query,
	// scheduled_for" and the CloudEvents document an event trigger matched. They sit
	// here rather than beside the evaluator because failover is a state resume: a
	// parameter written as ${{ trigger.body.cycle }} has to resolve to the same value
	// in the process that picks the run up, and a root the second process was never
	// given would resolve to nothing at all.
	Trigger map[string]any `json:"trigger,omitempty"`
	Event   map[string]any `json:"event,omitempty"`

	// Steps holds one entry per step of the graph, put there when the run starts,
	// so that a step nobody has reached is a pending entry rather than a missing
	// one and the reduction to a run verdict can tell the two apart.
	Steps map[agk.Step]StepState `json:"steps"`

	// Seq counts the decisions taken against this state. It is what a caller
	// persisting the state writes beside it to tell a stale copy from a current
	// one, and what makes two writers of one run detectable rather than silent.
	// A call that leaves the state as it was is no decision and does not move it,
	// so a caller has nothing to write when it has not moved.
	Seq int `json:"seq"`
}

// StepState is what has happened to one step: the verdict it has reached, the shards it
// was split into, and the envelopes it published when it ended.
type StepState struct {
	Verdict agk.Verdict  `json:"verdict"`
	Shards  []ShardState `json:"shards,omitempty"`

	// Ports is the publication: exactly one envelope per port the step declares,
	// written once, when the step ends. It is empty until then, which is what the
	// barrier below reads as a port that has not arrived.
	Ports map[agk.Port]agk.Envelope `json:"ports,omitempty"`

	// Since is when the step reached the verdict it carries. A published port is
	// stamped with it, and an empty envelope standing in for a step that published
	// nothing is stamped with it too, so that a batch of nothing still says when it
	// was not produced.
	Since time.Time `json:"since,omitzero"`

	// Reason says why, in the words of the rule that decided it, for the verdicts
	// where why is not obvious: a skip, a cancellation, a step whose when refused
	// every upstream state it saw. A run detail that says why reads better than one
	// that shows a verdict alone.
	Reason string `json:"reason,omitempty"`
}

// ShardState is what has happened to one shard of one step: which shard it is, which
// attempt it is on, and what the driver last said about the task running it.
//
// A shard that is retried keeps its place here and moves to its next attempt rather than
// being added beside itself. One shard is one piece of the work, however many attempts it
// takes, which is what lets max_parallel count what holds a runner and fail_fast name
// what to stop.
type ShardState struct {
	Shard  agk.Shard      `json:"shard,omitzero"`
	Matrix map[string]any `json:"matrix,omitempty"`

	// Attempt counts from 1, as AGK_ATTEMPT does.
	Attempt int `json:"attempt"`

	// Requeue counts the times this attempt was handed out again after its task was
	// lost, from 0 for the first time it was handed out. A requeue keeps the attempt
	// and so the idempotency key, which leaves this as the one thing that tells two
	// dispatches of one key apart: the controller keeps a row for each, and an ending is
	// news only about the dispatch the shard is on. It is also what max_requeues is
	// counted against, which is why the bound is per key.
	Requeue int `json:"requeue,omitempty"`

	// Task and ExitCode are what the driver reported, and they are read together:
	// the exit code table is the only place a verdict comes from, and the code is
	// read for a task that succeeded or failed and for no other state.
	Task     agk.TaskState `json:"task"`
	ExitCode int           `json:"exit_code,omitempty"`

	// NoExitCode is Result's: the ending reported no exit code, which ExitCode's 0 cannot say.
	NoExitCode bool `json:"no_exit_code,omitempty"`

	// Stopped says the evaluator ended this shard itself, cancelled, in the pass that
	// named its stop as superseded or sibling_failed, rather than hearing the ending from a
	// driver. The driver's report of it comes later, to a shard that is over, and all it
	// may still add is how the container exited: Record reads it for that and for nothing
	// else.
	Stopped bool `json:"stopped,omitempty"`

	// Ports is what this shard published, before the shards of the step are
	// concatenated port by port into what the step publishes.
	Ports map[agk.Port]agk.Envelope `json:"ports,omitempty"`

	// The four moments a task passes through. DispatchedAt is what fixes the
	// deadline, because the deadline runs from the moment the work became somebody's
	// and not from the moment it was decided on. NextAttemptAt is when the shard may
	// be handed out again, and is zero when there is nothing to wait for.
	DispatchedAt  time.Time `json:"dispatched_at,omitzero"`
	StartedAt     time.Time `json:"started_at,omitzero"`
	FinishedAt    time.Time `json:"finished_at,omitzero"`
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero"`
}
