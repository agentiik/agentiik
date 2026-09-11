package agk

import "fmt"

// Verdict is what happened to one step. It is the value the evaluator fixes when a step
// ends and the value a downstream when reads, and it is one type for both: the states
// when names are the states a step reaches, so a second enumeration for the reading
// would let the two disagree over what succeeded means.
//
// The scheduling states come first because a step is pending before it is anything else
// and the reduction to a run verdict has to be able to say so.
type Verdict int

const (
	// VerdictPending: the step has not started. Its barrier is not lifted, or it
	// is waiting on the clock for another attempt. It is the zero value, so a step
	// nobody has touched reads as the state it is actually in.
	VerdictPending Verdict = iota

	// VerdictRunning: at least one shard of the step is in flight.
	VerdictRunning

	// VerdictSucceeded: the step finished and published its ports.
	VerdictSucceeded

	// VerdictFailed: the step failed. Whether that fails the run is what
	// continue_on_error settles, and it does not change this verdict: the step is
	// still failed and still says so, which is how a downstream when reaches an
	// error path.
	VerdictFailed

	// VerdictSkipped: the if condition was false. The step publishes empty
	// envelopes on all its ports and downstream steps decide their own fate.
	VerdictSkipped

	// VerdictCancelled: the step was stopped before it could finish, by a
	// principal, by a concurrency group, by a merge: first that no longer needs it
	// or by a sibling failing under fail_fast.
	VerdictCancelled
)

// verdicts spells each verdict as the documentation writes it. The three that when may
// name, succeeded, failed and skipped, are spelled exactly as the when enumeration
// spells them, because that is the comparison the language asks for.
var verdicts = [...]string{
	VerdictPending:   "pending",
	VerdictRunning:   "running",
	VerdictSucceeded: "succeeded",
	VerdictFailed:    "failed",
	VerdictSkipped:   "skipped",
	VerdictCancelled: "cancelled",
}

// String names the verdict.
func (v Verdict) String() string {
	if v < 0 || int(v) >= len(verdicts) {
		return fmt.Sprintf("verdict %d", int(v))
	}
	return verdicts[v]
}

// Terminal says whether the step is finished with. A terminal verdict is what a
// downstream barrier waits for and what the run verdict is reduced from.
//
// Cancelled is terminal and skipped is terminal: a skipped step has published its empty
// envelopes, which is a publication and not a promise of one, and a cancelled step will
// not publish at all.
func (v Verdict) Terminal() bool {
	switch v {
	case VerdictSucceeded, VerdictFailed, VerdictSkipped, VerdictCancelled:
		return true
	default:
		return false
	}
}

// MarshalText writes the verdict as the documentation spells it.
func (v Verdict) MarshalText() ([]byte, error) {
	if v < 0 || int(v) >= len(verdicts) {
		return nil, fmt.Errorf("%d is not one of the step verdicts: %s", int(v), listOf(verdicts[:]))
	}
	return []byte(verdicts[v]), nil
}

// UnmarshalText reads a verdict back.
//
// It accepts the six a step can be in and not always, which is a name when uses for any
// of them rather than a state a step ever reaches. The workflow language reads always
// where it reads when; nothing reads it here.
func (v *Verdict) UnmarshalText(b []byte) error {
	n, err := lookup(verdicts[:], string(b), "a step verdict")
	if err != nil {
		return err
	}
	*v = Verdict(n)
	return nil
}
