package agk

import (
	"fmt"
	"strconv"
	"strings"
)

// TaskState is where one task is. A task is one shard of one attempt of one step, which
// is the unit a runner holds, heartbeats for and reports on, and these are the states it
// reports.
//
// It is not the step verdict. A step made of eight shards has eight task states and one
// verdict, and an attempt that failed leaves a failed task behind a step that then
// succeeded on the next one.
type TaskState int

const (
	// TaskPending: decided but not yet handed to a driver. The zero value, so a
	// task that has only been planned reads as what it is.
	TaskPending TaskState = iota

	// TaskDispatched: handed out. Recording the dispatch is what fixes the task's
	// deadline, because the deadline runs from the moment the work started to be
	// somebody's.
	TaskDispatched

	// TaskRunning: the container is running.
	TaskRunning

	// TaskPublishing: the container is finished and its outputs are being
	// collected and uploaded. It is its own state because the work is done and the
	// result is not yet safe, and losing a runner here is not the same as losing
	// one mid-run.
	TaskPublishing

	// TaskSucceeded: the container exited 0 and its envelopes were published.
	TaskSucceeded

	// TaskFailed: the container exited non-zero. Which failure that is, and
	// whether it is worth another attempt, is read off the exit code and nowhere
	// else. See Band.
	TaskFailed

	// TaskLost: the runner holding the task stopped reporting. The work may well
	// have finished without the result coming back, which is why only an
	// idempotent step is requeued after one.
	TaskLost

	// TaskTimedOut: the attempt was stopped at its deadline.
	TaskTimedOut

	// TaskCancelled: the task was stopped before it could finish, for one of the
	// reasons a stop carries.
	TaskCancelled
)

// taskStates spells each state as it travels on the bus and in the API.
var taskStates = [...]string{
	TaskPending:    "pending",
	TaskDispatched: "dispatched",
	TaskRunning:    "running",
	TaskPublishing: "publishing",
	TaskSucceeded:  "succeeded",
	TaskFailed:     "failed",
	TaskLost:       "lost",
	TaskTimedOut:   "timed_out",
	TaskCancelled:  "cancelled",
}

// String names the task state.
func (t TaskState) String() string {
	if t < 0 || int(t) >= len(taskStates) {
		return fmt.Sprintf("task state %d", int(t))
	}
	return taskStates[t]
}

// Terminal says whether anything more is expected of this task.
//
// Lost is terminal, which is the whole point of the state: the dispatch is over as far
// as anyone can tell, and what happens next is the same attempt handed out again under a
// new task_id, where the step allows it, rather than more news about this one.
func (t TaskState) Terminal() bool {
	switch t {
	case TaskSucceeded, TaskFailed, TaskLost, TaskTimedOut, TaskCancelled:
		return true
	default:
		return false
	}
}

// MarshalText writes the state as it is spelled on the wire.
func (t TaskState) MarshalText() ([]byte, error) {
	if t < 0 || int(t) >= len(taskStates) {
		return nil, fmt.Errorf("%d is not one of the task states: %s", int(t), listOf(taskStates[:]))
	}
	return []byte(taskStates[t]), nil
}

// UnmarshalText reads a state back.
func (t *TaskState) UnmarshalText(b []byte) error {
	n, err := lookup(taskStates[:], string(b), "a task state")
	if err != nil {
		return err
	}
	*t = TaskState(n)
	return nil
}

// Failure is a kind of failure retry.on can name. There are exactly four, because the
// keyword accepts exactly four, and a fifth name here would be a name a workflow file
// has no way to write.
//
// Infrastructure failure, exit 125 and above, is deliberately not one of them. The
// documentation charges it to the runner and not to the brick, and retry.on has no word
// for it; folding it into transient would retry, on the step's policy, what the step is
// not responsible for.
type Failure int

const (
	// FailureTransient is the 100 to 119 exit range, which a brick uses to say it
	// was unlucky. It is the one kind the exit code table already puts under the
	// step policy.
	FailureTransient Failure = iota + 1

	// FailureFailed is the application range, 1 to 99, retried only where the
	// author has said explicitly that it is safe.
	FailureFailed

	// FailureLost is a task whose runner stopped reporting, requeued only for an
	// idempotent step.
	FailureLost

	// FailureTimeout is an attempt stopped at its deadline.
	FailureTimeout
)

// failures spells the four kinds exactly as retry.on writes them. timeout and not
// timed_out: the keyword's enumeration is the authority on its own spelling, and a
// policy is matched against it by name.
var failures = [...]string{
	FailureTransient: "transient",
	FailureFailed:    "failed",
	FailureLost:      "lost",
	FailureTimeout:   "timeout",
}

// String names the failure kind as retry.on names it.
func (f Failure) String() string {
	if f < 0 || int(f) >= len(failures) || failures[f] == "" {
		return fmt.Sprintf("failure kind %d", int(f))
	}
	return failures[f]
}

// ParseFailure reads one name of retry.on. It refuses anything else, including the
// names of failures that exist but that the keyword cannot name, so that a workflow
// file saying retry.on: [infrastructure] is refused where it is written rather than
// quietly matching nothing at run time.
func ParseFailure(s string) (Failure, error) {
	n, err := lookup(failures[:], s, "a failure retry.on can name")
	if err != nil {
		return 0, err
	}
	return Failure(n), nil
}

// MarshalText writes the kind as retry.on spells it.
func (f Failure) MarshalText() ([]byte, error) {
	if f < 0 || int(f) >= len(failures) || failures[f] == "" {
		return nil, fmt.Errorf("%d is not one of the failures retry.on can name: %s", int(f), listOf(failures[:]))
	}
	return []byte(failures[f]), nil
}

// UnmarshalText reads a kind back, through the same door a workflow file comes in by.
func (f *Failure) UnmarshalText(b []byte) error {
	v, err := ParseFailure(string(b))
	if err != nil {
		return err
	}
	*f = v
	return nil
}

// ExitBand is one row of the exit code table. The band a container exited in is the only
// place a verdict comes from: nothing in the manifest declares the codes a brick may
// exit with, so there is nothing for publication to refuse and nothing else to read.
type ExitBand int

const (
	// BandSuccess is 0. The output envelopes are published.
	BandSuccess ExitBand = iota + 1

	// BandApplicationFailure is 1 to 99. The step failed, and there is no retry
	// unless retry.on says so explicitly.
	BandApplicationFailure

	// BandTransientFailure is 100 to 119. It is retried according to the step
	// policy.
	BandTransientFailure

	// BandInvalidInput is 120. A permanent failure, never retried, whatever retry
	// says.
	BandInvalidInput

	// BandReservedForRunner is 121 to 124. A brick that exits with one of these is
	// treated as having failed the contract, whatever its manifest says.
	BandReservedForRunner

	// BandRuntimeFailure is 125 and above. It is read as an infrastructure
	// failure, charged to the runner and not to the brick.
	BandRuntimeFailure
)

// bands name each row in the words of the table's own Meaning column.
var bands = [...]string{
	BandSuccess:            "success",
	BandApplicationFailure: "application failure",
	BandTransientFailure:   "transient failure",
	BandInvalidInput:       "invalid input",
	BandReservedForRunner:  "reserved for the runner",
	BandRuntimeFailure:     "reserved for the runtime",
}

// Band reads the exit code table. It is total: every integer lands in a band, because a
// container that exited reported something and the engine has to have an answer for it.
//
// A code below zero is not one a container can exit with, so it is read where the codes
// that are not the brick's are read, as an infrastructure failure. Charging it to the
// brick would let a driver reporting its own trouble as a negative number fail somebody
// else's step.
func Band(code int) ExitBand {
	switch {
	case code == 0:
		return BandSuccess
	case code >= 1 && code <= 99:
		return BandApplicationFailure
	case code >= 100 && code <= 119:
		return BandTransientFailure
	case code == 120:
		return BandInvalidInput
	case code >= 121 && code <= 124:
		return BandReservedForRunner
	default:
		return BandRuntimeFailure
	}
}

// String names the band.
func (b ExitBand) String() string {
	if b < 0 || int(b) >= len(bands) || bands[b] == "" {
		return fmt.Sprintf("exit band %d", int(b))
	}
	return bands[b]
}

// Retryable says whether a further attempt is allowed at all, before any policy is
// consulted. It is the table's Handling column and not the step's opinion: a policy can
// only choose among the failures this lets through.
//
// Invalid input is never retried, whatever retry says, because the input will be the
// same next time. The runner bands are not retried either: 121 to 124 is a brick that
// failed the contract, and 125 and above is charged to the runner, so neither is a
// failure the step's policy has any say over.
func (b ExitBand) Retryable() bool {
	switch b {
	case BandApplicationFailure, BandTransientFailure:
		return true
	default:
		return false
	}
}

// Failure gives the band the name retry.on would have to use to ask for another
// attempt, and says whether there is one at all.
//
// Success has no name because it is not a failure, and the three permanent bands have
// none because no policy can reach them. That is the same fact Retryable states, from
// the other side: a band with no name cannot be matched, and a band that is not
// retryable has no name.
func (b ExitBand) Failure() (Failure, bool) {
	switch b {
	case BandApplicationFailure:
		return FailureFailed, true
	case BandTransientFailure:
		return FailureTransient, true
	default:
		return 0, false
	}
}

// Shard names one piece of a fan-out: which one it is, and how many there are. It is
// what AGK_SHARD carries, written index and cardinality, for example 3/8.
//
// The index is counted from one. The documentation prints 3/8 and nothing else, and 3/8
// reads as the third of eight; a 0/8 would name a shard before the first one.
type Shard struct {
	Index int `json:"index"`
	Of    int `json:"of"`
}

// String writes the shard as AGK_SHARD carries it.
//
// A shard that is not one prints as nothing, because AGK_SHARD is absent when there is
// no fan-out and the empty string is what an absent variable is worth. Nothing here
// invents a spelling for a shard that does not exist.
func (s Shard) String() string {
	if s.IsZero() {
		return ""
	}
	return strconv.Itoa(s.Index) + "/" + strconv.Itoa(s.Of)
}

// IsZero says there is no fan-out. It is the zero value of the struct and not a
// sentinel: a step running as one container has no shard, and the absence of one is
// what that looks like.
func (s Shard) IsZero() bool { return s.Index == 0 && s.Of == 0 }

// Validate refuses a shard that names a piece no fan-out could have produced.
func (s Shard) Validate() error {
	if s.IsZero() {
		return nil
	}
	if s.Of < 1 {
		return fmt.Errorf("a fan-out produces at least one shard, so a cardinality is 1 or more and not %d", s.Of)
	}
	if s.Index < 1 || s.Index > s.Of {
		return fmt.Errorf("shard %d of %d: an index is counted from one and never past the cardinality, as AGK_SHARD writes it", s.Index, s.Of)
	}
	return nil
}

// TaskID identifies one task: one shard of one attempt of one step of one run. It is
// the idempotency key, which is why it is derived from what makes the task that task
// and never minted: the same decision taken twice, by the same evaluator after a
// failover or by a bus delivering a message twice, produces the same identifier, and a
// runner that has already done the work recognises it.
type TaskID string

// NewTaskID composes the identifier: the run, the step, the attempt, and the shard when
// there is one.
//
// A step with no fan-out contributes no shard, on the same reading AGK_SHARD takes: the
// identity of a task is what distinguishes it from another, and a shard that does not
// exist distinguishes nothing. A shard, when there is one, is written the way it is
// written everywhere else, index and cardinality, so an identifier carries five segments
// where a fan-out produced it and three where none did.
func NewTaskID(run RunID, step Step, attempt int, sh Shard) TaskID {
	id := string(run) + "/" + string(step) + "/" + strconv.Itoa(attempt)
	if !sh.IsZero() {
		id += "/" + sh.String()
	}
	return TaskID(id)
}

// ParseTaskID takes an identifier apart again, which is what a driver reporting a result
// and an operator reading a log both need.
func ParseTaskID(s string) (RunID, Step, int, Shard, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 3 && len(parts) != 5 {
		return "", "", 0, Shard{}, fmt.Errorf("%q is not a task identifier: one is written run/step/attempt, and run/step/attempt/index/of where a fan-out produced it", s)
	}

	run, step := RunID(parts[0]), Step(parts[1])
	if err := run.Validate(); err != nil {
		return "", "", 0, Shard{}, fmt.Errorf("task %q: %w", s, err)
	}
	if err := step.Validate(); err != nil {
		return "", "", 0, Shard{}, fmt.Errorf("task %q: %w", s, err)
	}
	attempt, err := number(parts[2], "an attempt")
	if err != nil {
		return "", "", 0, Shard{}, fmt.Errorf("task %q: %w", s, err)
	}
	if attempt < 1 {
		return "", "", 0, Shard{}, fmt.Errorf("task %q: an attempt is numbered from 1 and not %d, as AGK_ATTEMPT carries it", s, attempt)
	}

	var sh Shard
	if len(parts) == 5 {
		if sh.Index, err = number(parts[3], "a shard index"); err != nil {
			return "", "", 0, Shard{}, fmt.Errorf("task %q: %w", s, err)
		}
		if sh.Of, err = number(parts[4], "a shard cardinality"); err != nil {
			return "", "", 0, Shard{}, fmt.Errorf("task %q: %w", s, err)
		}
		if err := sh.Validate(); err != nil {
			return "", "", 0, Shard{}, fmt.Errorf("task %q: %w", s, err)
		}
		if sh.IsZero() {
			return "", "", 0, Shard{}, fmt.Errorf("task %q: a task with no shard carries none, so it is written run/step/attempt", s)
		}
	}
	return run, step, attempt, sh, nil
}

// Validate refuses an identifier that could not have been composed here. It is what a
// server checks before trusting a value that arrived from outside.
func (t TaskID) Validate() error {
	run, step, attempt, sh, err := ParseTaskID(string(t))
	if err != nil {
		return err
	}
	// Composing it again is the check that matters: an identifier that does not
	// rebuild itself is one that says something the parts do not, and it is the
	// composed form that a runner deduplicates on.
	if got := NewTaskID(run, step, attempt, sh); got != t {
		return fmt.Errorf("%q is not written the way a task identifier is composed, which would give %q", string(t), string(got))
	}
	return nil
}

// number reads one decimal segment of an identifier, refusing the leading zeros and the
// signs strconv would otherwise accept, because two spellings of one number would be two
// identifiers for one task.
func number(s, what string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("%s is a number and this is empty", what)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("%q is not %s: it is written in decimal digits", s, what)
		}
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, fmt.Errorf("%q is not %s: a number is written without leading zeros, so that one number has one spelling", s, what)
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not %s: %w", s, what, err)
	}
	return n, nil
}
