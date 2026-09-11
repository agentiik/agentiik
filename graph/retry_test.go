package graph

import (
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// outcomeEnded builds the shard state of an attempt that has just finished, which is what the
// retry rules are asked about.
func outcomeEnded(attempt int, task agk.TaskState, exit int) ShardState {
	return ShardState{
		Shard:      agk.Shard{Index: 1, Of: 1},
		Attempt:    attempt,
		Task:       task,
		ExitCode:   exit,
		FinishedAt: outcomeAt,
	}
}

// TestTheTableDecidesWhatMayBeRetriedAtAll holds the Handling column against a policy
// that asks for everything. Invalid input is never retried whatever retry says, and the
// two runner bands are not the step's to retry either, so a policy naming every kind it
// can name still does not reach them.
func TestTheTableDecidesWhatMayBeRetriedAtAll(t *testing.T) {
	everything := Retry{Max: 5, On: []agk.Failure{agk.FailureTransient, agk.FailureFailed, agk.FailureLost, agk.FailureTimeout}}
	for _, c := range []struct {
		exit    int
		retried bool
		why     string
	}{
		{0, false, "a container that exited 0 succeeded"},
		{7, true, "an application failure is retried where retry.on says failed"},
		{100, true, "a transient failure is retried according to the step policy"},
		{119, true, "a transient failure is retried according to the step policy"},
		{120, false, "invalid input is a permanent failure, never retried, whatever retry says"},
		{121, false, "121 to 124 is a brick that failed the contract"},
		{124, false, "121 to 124 is a brick that failed the contract"},
		{125, false, "125 and above is charged to the runner and not to the brick"},
		{137, false, "125 and above is charged to the runner and not to the brick"},
	} {
		_, again := nextAttempt(everything, true, outcomeEnded(1, agk.TaskFailed, c.exit))
		if again != c.retried {
			t.Errorf("exit %d is retried=%v: %s", c.exit, again, c.why)
		}
	}
}

// TestRetryOnNamesTheFailure holds the keyword: a policy retries the kinds it names and
// no others, and an application failure is retried only where the author said so.
func TestRetryOnNamesTheFailure(t *testing.T) {
	transient := Retry{Max: 2, On: []agk.Failure{agk.FailureTransient}}
	if _, again := nextAttempt(transient, true, outcomeEnded(1, agk.TaskFailed, 7)); again {
		t.Error("retry.on: [transient] retried an application failure, which is retried only where retry.on says so explicitly")
	}
	if _, again := nextAttempt(transient, true, outcomeEnded(1, agk.TaskFailed, 100)); !again {
		t.Error("retry.on: [transient] did not retry a transient failure")
	}
	if _, again := nextAttempt(transient, true, outcomeEnded(1, agk.TaskTimedOut, 0)); again {
		t.Error("retry.on: [transient] retried an attempt stopped at its deadline")
	}

	failed := Retry{Max: 2, On: []agk.Failure{agk.FailureFailed}}
	if _, again := nextAttempt(failed, true, outcomeEnded(1, agk.TaskFailed, 7)); !again {
		t.Error("retry.on: [failed] did not retry an application failure")
	}
	if _, again := nextAttempt(failed, true, outcomeEnded(1, agk.TaskFailed, 100)); again {
		t.Error("retry.on: [failed] retried a transient failure it does not name")
	}

	timeout := Retry{Max: 1, On: []agk.Failure{agk.FailureTimeout}}
	if _, again := nextAttempt(timeout, true, outcomeEnded(1, agk.TaskTimedOut, 0)); !again {
		t.Error("retry.on: [timeout] did not retry an attempt stopped at its deadline")
	}
}

// TestAPolicyThatNamesNothingRetriesTheTransientBand holds the reading retryAllows
// records: the transient band is the one the exit code table already puts under the step
// policy, so it is the one a policy that has not had to name anything accepts.
func TestAPolicyThatNamesNothingRetriesTheTransientBand(t *testing.T) {
	silent := Retry{Max: 2}
	if _, again := nextAttempt(silent, true, outcomeEnded(1, agk.TaskFailed, 100)); !again {
		t.Error("retry: {max: 2} did not retry a transient failure")
	}
	for _, c := range []struct {
		task agk.TaskState
		exit int
		what string
	}{
		{agk.TaskFailed, 7, "an application failure"},
		{agk.TaskLost, 0, "a lost task"},
		{agk.TaskTimedOut, 0, "an attempt stopped at its deadline"},
	} {
		if _, again := nextAttempt(silent, true, outcomeEnded(1, c.task, c.exit)); again {
			t.Errorf("retry: {max: 2} retried %s without being asked to", c.what)
		}
	}
}

// TestMaxCountsFurtherAttempts holds what the number means: max: 2 is two further
// attempts, so three in all, and beyond it the step is failed and the graph carries on
// as when says.
func TestMaxCountsFurtherAttempts(t *testing.T) {
	policy := Retry{Max: 2, On: []agk.Failure{agk.FailureTransient}}
	for attempt, want := range map[int]bool{1: true, 2: true, 3: false, 4: false} {
		if _, again := nextAttempt(policy, true, outcomeEnded(attempt, agk.TaskFailed, 100)); again != want {
			t.Errorf("after attempt %d of max: 2, another attempt is %v", attempt, again)
		}
	}

	// A step with no retry block gets the one attempt it was given.
	if _, again := nextAttempt(Retry{}, true, outcomeEnded(1, agk.TaskFailed, 100)); again {
		t.Error("a step with no retry policy was retried")
	}
}

// TestOnlyAnIdempotentStepIsRequeuedAfterALoss holds the second question a lost task
// asks. A lost task may well have finished without the result coming back, so running it
// again is safe only where the author said it is.
func TestOnlyAnIdempotentStepIsRequeuedAfterALoss(t *testing.T) {
	policy := Retry{Max: 3, On: []agk.Failure{agk.FailureLost}}
	if _, again := nextAttempt(policy, true, outcomeEnded(1, agk.TaskLost, 0)); !again {
		t.Error("an idempotent step was not requeued after its task was lost")
	}
	if _, again := nextAttempt(policy, false, outcomeEnded(1, agk.TaskLost, 0)); again {
		t.Error("a step declared idempotent: false was requeued after its task was lost")
	}
	// The rule is about loss and not about the policy: a step that is not
	// idempotent is still retried on the failures it can be retried on.
	both := Retry{Max: 3, On: []agk.Failure{agk.FailureLost, agk.FailureTransient}}
	if _, again := nextAttempt(both, false, outcomeEnded(1, agk.TaskFailed, 100)); !again {
		t.Error("a step declared idempotent: false was not retried on a transient failure")
	}
}

// TestTheBackoffLengthensAndIsCapped holds the shape of the wait: it starts at base,
// lengthens after each failure and never goes past max.
func TestTheBackoffLengthensAndIsCapped(t *testing.T) {
	b := Backoff{Type: BackoffExponential, Base: Duration(2 * time.Second), Max: Duration(60 * time.Second)}
	for further, want := range map[int]time.Duration{
		1: 2 * time.Second,
		2: 4 * time.Second,
		3: 8 * time.Second,
		4: 16 * time.Second,
		5: 32 * time.Second,
		6: 60 * time.Second,
		9: 60 * time.Second,
	} {
		if got := backoffWait(b, further); got != want {
			t.Errorf("the wait before further attempt %d is %s and not %s", further, got, want)
		}
	}

	uncapped := Backoff{Base: Duration(time.Second)}
	if got := backoffWait(uncapped, 4); got != 8*time.Second {
		t.Errorf("with no ceiling the wait before further attempt 4 is %s and not 8s", got)
	}
	if got := backoffWait(uncapped, 200); got <= 0 {
		t.Errorf("a long run of failures produced a wait of %s, which has already passed", got)
	}
	if got := backoffWait(Backoff{}, 1); got != 0 {
		t.Errorf("a policy with no backoff waits %s, and it has not asked to wait", got)
	}
}

// TestTheWaitRunsFromTheEndOfTheAttempt holds where the next attempt is placed on the
// clock, which is what the evaluator wakes on.
func TestTheWaitRunsFromTheEndOfTheAttempt(t *testing.T) {
	policy := Retry{
		Max:     2,
		On:      []agk.Failure{agk.FailureTransient},
		Backoff: Backoff{Type: BackoffExponential, Base: Duration(2 * time.Second), Max: Duration(60 * time.Second)},
	}
	first := outcomeEnded(1, agk.TaskFailed, 100)
	at, again := nextAttempt(policy, true, first)
	if !again {
		t.Fatal("a transient failure was not retried")
	}
	if want := first.FinishedAt.Add(2 * time.Second); !at.Equal(want) {
		t.Errorf("the second attempt is due at %s and not at %s", at, want)
	}

	second := outcomeEnded(2, agk.TaskFailed, 100)
	at, again = nextAttempt(policy, true, second)
	if !again {
		t.Fatal("the second failure was not retried, and max: 2 allows a third attempt")
	}
	if want := second.FinishedAt.Add(4 * time.Second); !at.Equal(want) {
		t.Errorf("the third attempt is due at %s and not at %s, which is base lengthened once", at, want)
	}

	// A result that says nothing about when the attempt ended gets its next
	// attempt at once, which is the consequence of not saying.
	silent := ShardState{Attempt: 1, Task: agk.TaskFailed, ExitCode: 100}
	if at, again := nextAttempt(policy, true, silent); !again || !at.IsZero() {
		t.Errorf("an attempt that reported no finish time is due at %s (again=%v)", at, again)
	}
}
