package graph

import (
	"math"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// Retrying is deliberate rather than automatic. Four things have to agree before an
// attempt is made again, and they are asked in this order: the exit code table has to
// allow it at all, a lost task has to be a lost task of an idempotent step, the step's
// policy has to name the failure, and there has to be an attempt left. A failure that
// clears all four waits for the backoff and then runs again; anything else is the end of
// that shard.
//
// None of this is a decision about the step. A shard that has run out of attempts is a
// failed shard, and what that does to the step, and what the step does to the run, is
// settled in verdict.go and by continue_on_error.

// nextAttempt says whether a shard that has finished gets another attempt, and the
// moment that attempt may be dispatched.
//
// The four questions are the documentation's, in the order it asks them. What kind of
// failure this was comes off the exit code table and off nothing else, which is what
// keeps a step from being retried on a runner's trouble or on a code the table calls
// permanent. A lost task is the one kind that asks a second question of the step: a lost
// task may well have finished without the result coming back, so only an idempotent step
// is requeued after one.
//
// The wait runs from the moment the attempt ended, and the moment returned is zero when
// there is nothing to wait for: no backoff was asked for, or the result carried no
// finish time to measure one from. A zero moment is the attempt being due now, which is
// the honest consequence of a result that does not say when it ended rather than a wait
// invented on its behalf.
//
// A shard granted another attempt has not finished, and the caller records that by
// returning it to pending on its new attempt number. A shard left in the terminal state
// its last attempt ended in, with nothing on the clock to say another is coming, reads
// downstream as a shard that is over.
func nextAttempt(r Retry, idempotent bool, sh ShardState) (time.Time, bool) {
	failure, named := failureOf(sh.Task, sh.ExitCode)
	if !named {
		return time.Time{}, false
	}
	if failure == agk.FailureLost && !idempotent {
		return time.Time{}, false
	}
	if !retryAllows(r, failure) {
		return time.Time{}, false
	}
	if sh.Attempt >= retryAttempts(r) {
		return time.Time{}, false
	}
	// The attempt that just failed is attempt sh.Attempt, so the one being granted
	// is the sh.Attempt-th further attempt: the first of them waits base.
	wait := backoffWait(r.Backoff, sh.Attempt)
	if wait <= 0 || sh.FinishedAt.IsZero() {
		return time.Time{}, true
	}
	return sh.FinishedAt.Add(wait), true
}

// retryAllows says whether the policy names this kind of failure.
//
// A policy that names none accepts a transient failure and nothing else. That is the
// exit code table read as it is written: the transient band is "retried according to the
// step policy", while the application band is retried only where retry.on says so
// explicitly, so the kind a policy has not had to name is the one the table already put
// under it. lost and timeout are not in it either, because requeueing a step whose
// runner went quiet, or one stopped at its deadline, is a decision the author takes and
// not one taken for them.
func retryAllows(r Retry, f agk.Failure) bool {
	if len(r.On) == 0 {
		return f == agk.FailureTransient
	}
	for _, on := range r.On {
		if on == f {
			return true
		}
	}
	return false
}

// retryAttempts says how many attempts the policy allows in all, the first one included.
//
// max counts further attempts, which is why max: 2 is three attempts and why a step with
// no retry block at all gets one. A policy that names failures and no number grants no
// further attempt: the number is what grants one, and reading an absent max as some
// other number would be the engine retrying what the file did not ask it to.
func retryAttempts(r Retry) int {
	if r.Max < 0 {
		return 1
	}
	return 1 + r.Max
}

// backoffWait is how long to wait before the further attempt numbered further, counted
// from one. The wait starts at base, lengthens after each failure and never goes past
// max, which is exponential, and exponential is the only type there is: a second type
// would be read here and nowhere else.
//
// A policy with no backoff waits nothing, because the documentation gives no default and
// a policy that has not asked to wait has not asked to wait. A base of nothing is the
// same answer arrived at by arithmetic, and a max of nothing is no ceiling rather than a
// ceiling of zero, since a ceiling below the base would make the keyword undo the one
// beside it.
func backoffWait(b Backoff, further int) time.Duration {
	base, ceiling := time.Duration(b.Base), time.Duration(b.Max)
	if base <= 0 || further < 1 {
		return 0
	}
	wait := base
	for n := 1; n < further; n++ {
		if ceiling > 0 && wait >= ceiling {
			return ceiling
		}
		// Doubling is what lengthens after each failure, and the overflow check
		// comes first so that a policy with a large base and no ceiling cannot
		// wrap round into a wait that has already passed.
		if wait > math.MaxInt64/2 {
			return math.MaxInt64
		}
		wait *= 2
	}
	if ceiling > 0 && wait > ceiling {
		return ceiling
	}
	return wait
}
