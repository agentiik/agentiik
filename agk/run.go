package agk

import (
	"fmt"
	"time"
)

// Run is one execution of one workflow, at one commit, started by one principal. It is
// the head of everything a run carries: the evaluator holds it inside its state, the
// driver stamps AGK_RUN_ID, AGK_WORKFLOW, AGK_NAMESPACE and AGK_COMMIT from it, and the
// controller persists it. It lives here rather than in the evaluator because the driver
// imports this package and will never import the evaluator.
type Run struct {
	ID          RunID       `json:"id"`
	Workflow    string      `json:"workflow"`
	Namespace   string      `json:"namespace"`
	Commit      string      `json:"commit"`
	Trigger     TriggerKind `json:"trigger"`
	TriggeredBy string      `json:"triggered_by"`
	State       RunState    `json:"state"`
	StartedAt   time.Time   `json:"started_at"`
	FinishedAt  time.Time   `json:"finished_at"`
}

// RunState is where a run is, in the seven states the documentation names. The verdict
// of a finished run is one of these and not a second type: succeeded, failed, cancelled
// and timed_out are run states that happen to be terminal, and inventing a Verdict type
// beside them would give the same fact two spellings and let them disagree.
type RunState int

// The run states, in the order the documentation lists them. Queued is the zero value
// because a run that exists has at least been created and is waiting on a concurrency
// lock or on quota, which is exactly what queued means; a state field nobody has set
// therefore reads as the state a run starts in rather than as a state that does not
// exist.
const (
	// Queued: created, waiting on a concurrency lock or on namespace quota.
	Queued RunState = iota

	// Running: at least one task active or ready.
	Running

	// Waiting: suspended, waiting for an external signal or for a human approval.
	Waiting

	// Succeeded: every reached step finished, none failed beyond tolerance.
	Succeeded

	// Failed: at least one step failed without continue_on_error.
	Failed

	// Cancelled: cancelled by a principal holding workflow:run, by a concurrency
	// group or by a merge: first.
	Cancelled

	// TimedOut: the deadline set by the root timeout of the entry point expired,
	// and the tasks still running were stopped.
	TimedOut
)

// runStates spells each state as the documentation writes it, which is also how it
// travels in JSON and how the API prints it.
var runStates = [...]string{
	Queued:    "queued",
	Running:   "running",
	Waiting:   "waiting",
	Succeeded: "succeeded",
	Failed:    "failed",
	Cancelled: "cancelled",
	TimedOut:  "timed_out",
}

// String names the state in the documentation's own spelling.
func (r RunState) String() string {
	if r < 0 || int(r) >= len(runStates) {
		return fmt.Sprintf("run state %d", int(r))
	}
	return runStates[r]
}

// Terminal says whether the run is over. The four terminal states are the four verdicts
// a run can end on, and nothing moves out of one of them: a replay is a new run.
func (r RunState) Terminal() bool {
	switch r {
	case Succeeded, Failed, Cancelled, TimedOut:
		return true
	default:
		return false
	}
}

// MarshalText writes the state as the documentation spells it, so that a persisted run
// and an API response say queued and not 0.
func (r RunState) MarshalText() ([]byte, error) {
	if r < 0 || int(r) >= len(runStates) {
		return nil, fmt.Errorf("%d is not one of the run states: %s", int(r), listOf(runStates[:]))
	}
	return []byte(runStates[r]), nil
}

// UnmarshalText reads a state back, refusing a spelling that is not one of the seven.
func (r *RunState) UnmarshalText(b []byte) error {
	v, err := lookup(runStates[:], string(b), "a run state")
	if err != nil {
		return err
	}
	*r = RunState(v)
	return nil
}

// TriggerKind says what started the run. The four kinds are the four trigger blocks the
// language has, with manual standing for a run started through the API or the command
// line rather than by a declared trigger.
type TriggerKind int

const (
	// TriggerManual: a principal asked for this run.
	TriggerManual TriggerKind = iota

	// TriggerWebhook: an inbound request matched the workflow's webhook trigger.
	TriggerWebhook

	// TriggerCron: the schedule of the workflow came round.
	TriggerCron

	// TriggerEvent: an event the workflow subscribes to was published.
	TriggerEvent
)

// triggerKinds spells the kinds as the trigger blocks are named in the language, with
// cron rather than schedule because that is the word a run carries when it says what
// started it.
var triggerKinds = [...]string{
	TriggerManual:  "manual",
	TriggerWebhook: "webhook",
	TriggerCron:    "cron",
	TriggerEvent:   "event",
}

// String names the trigger kind.
func (t TriggerKind) String() string {
	if t < 0 || int(t) >= len(triggerKinds) {
		return fmt.Sprintf("trigger kind %d", int(t))
	}
	return triggerKinds[t]
}

// MarshalText writes the kind as the language names it.
func (t TriggerKind) MarshalText() ([]byte, error) {
	if t < 0 || int(t) >= len(triggerKinds) {
		return nil, fmt.Errorf("%d is not one of the trigger kinds: %s", int(t), listOf(triggerKinds[:]))
	}
	return []byte(triggerKinds[t]), nil
}

// UnmarshalText reads a kind back.
func (t *TriggerKind) UnmarshalText(b []byte) error {
	v, err := lookup(triggerKinds[:], string(b), "a trigger kind")
	if err != nil {
		return err
	}
	*t = TriggerKind(v)
	return nil
}

// lookup turns a spelling back into the value that carries it. One table per enumeration
// and one reader for all of them, so that a name a table gains is a name every direction
// gains with it and the two directions cannot drift apart.
func lookup(names []string, s, what string) (int, error) {
	for i, name := range names {
		if name != "" && name == s {
			return i, nil
		}
	}
	return 0, fmt.Errorf("%q is not %s: %s", s, what, listOf(names))
}

// listOf prints the members of an enumeration the way a refusal names them.
func listOf(names []string) string {
	var out string
	for _, name := range names {
		if name == "" {
			continue
		}
		if out != "" {
			out += ", "
		}
		out += name
	}
	return out
}
