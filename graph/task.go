package graph

import (
	"time"

	"github.com/agentiik/agentiik/agk"
)

// The seam between deciding and executing is two values and one interface. A Plan is
// what the evaluator hands out, a Result is what it takes back, and the interface is
// stated in driver.go and called by nothing here.
//
// Nothing in a Plan has happened yet. It is a decision, and a decision that has not
// happened yet can be recomputed: calling Next twice with the same moment returns the
// same Plan, because a task it named is still pending until a driver reports the dispatch
// and a task it stopped is still in flight until one reports the stop. A controller that
// dies between deciding and dispatching therefore loses nothing but the work of deciding.
//
// What Next does move is the state, which is where the progress of a run lives: a step
// that skipped and a step that ended are settled there, once, and a second call at the
// same moment finds them settled and decides the same way.

// Plan is what should happen next: the tasks that have become ready, the tasks in flight
// that should be stopped, and the moment to ask again.
//
// Wake is zero when nothing waits on the clock. It is not zero when something does, and
// two things do: a retry backoff, which places an attempt at a moment in the future, and
// the root timeout, which places the end of the run at one. A caller that never comes
// back at Wake does not get a wrong Plan; it gets the right one late.
type Plan struct {
	Start []Task    `json:"start,omitempty"`
	Stop  []Stop    `json:"stop,omitempty"`
	Wake  time.Time `json:"wake,omitzero"`
}

// Task is one shard of one attempt of one step, carrying everything a driver needs and
// nothing it has to look up.
//
// Inputs is exactly the argument brick.WriteInputs takes and Outputs is exactly the
// declared argument brick.Collect takes, so the driver is the thin thing between two
// calls that already exist. That is why the merge strategies are resolved on this side:
// wait_all, zip, join and first are rules the workflow file states, and a rule hidden
// inside the driver is a rule the workflow file cannot show.
//
// The task message of the documentation carries digests rather than envelopes. That is
// one serialisation of this, made by a server driver that spills the envelope to the
// store and puts the digest on the bus, and it is not a second decision.
type Task struct {
	// ID is the idempotency key, run/step/attempt/shard, derived from what makes
	// this task this task and never minted. A driver that has already done the work
	// recognises it, which is what makes at-least-once delivery survivable.
	ID agk.TaskID `json:"id"`

	Run       agk.RunID `json:"run"`
	Workflow  string    `json:"workflow"`
	Namespace string    `json:"namespace"`
	Commit    string    `json:"commit"`

	Step    agk.Step  `json:"step"`
	Attempt int       `json:"attempt"`
	Shard   agk.Shard `json:"shard,omitzero"`

	// Image is what the container is, or Call is the sub-workflow this step is
	// instead. A step carries one or the other and never both.
	Image string `json:"image,omitempty"`
	Call  *Call  `json:"call,omitempty"`

	// The script keywords, where the image is a base image rather than a brick.
	// after_script runs in the same container even when script failed, so that a
	// diagnostic dump survives a failure, and its own exit code does not change the
	// step's verdict: keeping that true is the driver's, because it is the driver
	// that watches the two and reports one.
	Script       []string `json:"script,omitempty"`
	BeforeScript []string `json:"before_script,omitempty"`
	AfterScript  []string `json:"after_script,omitempty"`
	Shell        []string `json:"shell,omitempty"`

	// Params are resolved: every expression evaluated, the matrix combination
	// injected, and the whole validated against the manifest schema. The driver
	// writes them to /agk/params.json and asks nothing about them.
	Params map[string]any `json:"params,omitempty"`

	// Secrets are names and mount points and never values. The task message names
	// the secrets it needs and carries none of them; the values exist only after the
	// runner redeems its grant.
	Secrets []SecretMount `json:"secrets,omitempty"`

	// Inputs are merged and sharded already, one envelope per input port.
	Inputs map[agk.Port]agk.Envelope `json:"inputs,omitempty"`

	// Outputs are the ports declared, which is what is collected from /agk/out/ and
	// what publishes an empty envelope where the container wrote nothing.
	Outputs []agk.Port `json:"outputs,omitempty"`

	Files       []FileSelector `json:"files,omitempty"`
	Resources   Resources      `json:"resources,omitzero"`
	Network     Network        `json:"network"`
	EgressAllow []string       `json:"egress_allow,omitempty"`
	RunsOn      []string       `json:"runs_on,omitempty"`

	// Timeout bounds this shard and Deadline is the moment it lands on, which is
	// what AGK_DEADLINE carries and what the runner stops the container at.
	Timeout  Duration  `json:"timeout,omitempty"`
	Deadline time.Time `json:"deadline,omitzero"`

	// Idempotent says whether running this again on the same inputs is safe, which
	// is what decides whether a lost task may be requeued and whether the result may
	// be cached. Cache and CacheKey are memoisation: the key combines the image
	// digest, the resolved parameters and the digests of the input envelopes,
	// prefixed by the namespace, and it never crosses a namespace boundary.
	Idempotent bool   `json:"idempotent"`
	Cache      bool   `json:"cache,omitempty"`
	CacheKey   string `json:"cache_key,omitempty"`
}

// Stop is one task in flight that should be stopped, and why.
//
// It exists because three rules in the documentation call off work that is already
// running, and a decision to stop is as much a decision as a decision to start. A
// controller that only ever heard about starts would leave a shard running for an hour
// after the run it belongs to was cancelled.
type Stop struct {
	Task   agk.TaskID `json:"task"`
	Reason StopReason `json:"reason"`
}

// StopReason is why a task is being stopped. There are four, and each one is a rule the
// documentation states rather than a category invented here.
type StopReason int

const (
	// StopSuperseded: a merge: first lifted the barrier on another edge, and no
	// other consumer needs what this task would publish.
	StopSuperseded StopReason = iota

	// StopSiblingFailed: fail_fast, where the first shard to fail stops the shards
	// still running beside it.
	StopSiblingFailed

	// StopDeadline: the deadline set by the root timeout of the entry point passed,
	// and the tasks still running are stopped.
	StopDeadline

	// StopCancelled: a principal holding workflow:run asked, or a concurrency group
	// did.
	StopCancelled
)

// stopReasons spells each reason as a run detail and a log line carry it.
var stopReasons = [...]string{
	StopSuperseded:    "superseded",
	StopSiblingFailed: "sibling_failed",
	StopDeadline:      "deadline",
	StopCancelled:     "cancelled",
}

// String names the reason.
func (s StopReason) String() string {
	if s < 0 || int(s) >= len(stopReasons) {
		return "stopped"
	}
	return stopReasons[s]
}

// MarshalText writes the reason as it is spelled.
func (s StopReason) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Result is what became of one task. It is the only thing that enters the evaluator
// after a run has started.
//
// State may be non-terminal: recording the dispatch is what fixes the task's deadline,
// because the deadline runs from the moment the work became somebody's. A caller that
// records only terminal results gets a deadline computed from the moment the task was
// planned instead, which drifts forward with every plan, and that is the honest
// consequence of not telling the evaluator rather than a rule it can enforce.
//
// ExitCode is read for a task that succeeded or failed and for no other state, through
// agk.Band, which is the exit code table and the only place a verdict comes from.
//
// Outputs is one envelope per declared port, which is what brick.Collect returns, empty
// envelopes included: a port the container never wrote publishes an empty envelope, and
// that is success.
type Result struct {
	Task     agk.TaskID    `json:"task"`
	State    agk.TaskState `json:"state"`
	ExitCode int           `json:"exit_code,omitempty"`

	Outputs map[agk.Port]agk.Envelope `json:"outputs,omitempty"`

	DispatchedAt time.Time `json:"dispatched_at,omitzero"`
	StartedAt    time.Time `json:"started_at,omitzero"`
	FinishedAt   time.Time `json:"finished_at,omitzero"`
}
