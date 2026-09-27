package totp

import (
	"math"
	"testing"
	"time"
)

// The SHA-1 rows of RFC 6238's Appendix B, whose secret is the ASCII of "12345678901234567890" and
// whose codes are eight digits long: a code of six is the last six digits of the same number.
func TestTheCodesAreRFC6238s(t *testing.T) {
	secret := []byte("12345678901234567890")
	for _, v := range []struct {
		unix  int64
		eight string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	} {
		at := time.Unix(v.unix, 0)
		if got := Code(secret, StepAt(at)); got != v.eight[2:] {
			t.Errorf("at %d the code is %s, and RFC 6238 says %s", v.unix, got, v.eight[2:])
		}
	}
}

// A step is 30 seconds from the epoch, and one before it rounds down.
func TestAStepIsThirtySecondsFromTheEpoch(t *testing.T) {
	for unix, want := range map[int64]int64{0: 0, 29: 0, 30: 1, 59: 1, 60: 2, -1: -1, -30: -1, -31: -2} {
		if got := StepAt(time.Unix(unix, 0)); got != want {
			t.Errorf("%d seconds after the epoch is step %d, not %d", unix, got, want)
		}
	}
}

// A code is accepted at its own step and one step either side of the verifier's, and at no step
// further; and never at the step last accepted or one before it, so that a code accepted once is
// not accepted again within the minute and a half it would otherwise be good for.
func TestACodeIsAcceptedOneStepEitherSideAndNeverTwice(t *testing.T) {
	secret := []byte("a secret of twenty b")
	now := time.Unix(1_800_000_015, 0)
	at := StepAt(now)
	none := int64(math.MinInt64)
	for offset, want := range map[int64]bool{-2: false, -1: true, 0: true, 1: true, 2: false} {
		step, ok := Match(secret, Code(secret, at+offset), now, none)
		if ok != want || (ok && step != at+offset) {
			t.Errorf("the code of step %+d answered step %d, %v", offset, step-at, ok)
		}
	}

	// Accepted at its step, the same code is refused however soon it comes again, and so is the
	// code of the step before, which a verifier ahead of the generator would otherwise take.
	step, ok := Match(secret, Code(secret, at), now, none)
	if !ok {
		t.Fatal("the current code was refused")
	}
	for _, later := range []time.Duration{0, 10 * time.Second, 40 * time.Second} {
		if _, ok := Match(secret, Code(secret, at), now.Add(later), step); ok {
			t.Errorf("the code accepted was accepted again %s later", later)
		}
	}
	if _, ok := Match(secret, Code(secret, at-1), now, step); ok {
		t.Error("the code of the step before the one accepted was accepted after it")
	}
	if next, ok := Match(secret, Code(secret, at+1), now.Add(Step), step); !ok || next != at+1 {
		t.Errorf("the next step's code answered %d, %v", next-at, ok)
	}

	for _, code := range []string{"", "00000", "0000000", "abcdef"} {
		if _, ok := Match(secret, code, now, none); ok {
			t.Errorf("%q was accepted", code)
		}
	}
	if _, ok := Match([]byte("another secret, twenty"), Code(secret, at), now, none); ok {
		t.Error("a code of one secret was accepted for another")
	}
}
