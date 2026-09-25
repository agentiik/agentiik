package agk_test

import (
	"regexp"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

// The identifiers are written down here as sha256sum prints them, rather than computed again the
// way the code computes them, so that a change to what is hashed, or to which bytes are kept, is a
// change this test sees: every program derives the same trace from the same run only as long as
// the derivation never moves.
//
//	$ printf %s 01M2Z8V1P9C4XQ7K2N4D6F8H0C | sha256sum
//	65adc84f140f8df02002e0395b782fe81bcead9eed447f5ed51f4521e2d1a847  -
//	$ printf %s 01M2Z8V1P9C4XQ7K2N4D6F8H0C/normalize/1 | sha256sum
//	1e176d343d62934f341c54acaedc285e49550ec1d278647ac8db7ef51e142ec5  -
const (
	tracedRun  agk.RunID = "01M2Z8V1P9C4XQ7K2N4D6F8H0C"
	tracedTask           = "01M2Z8V1P9C4XQ7K2N4D6F8H0C/normalize/1"
)

func TestARunNamesItsTraceAndItsOwnSpanFromOneHashOfItsIdentifier(t *testing.T) {
	trace, span := tracedRun.Trace()
	if got, want := trace.String(), "65adc84f140f8df02002e0395b782fe8"; got != want {
		t.Errorf("the trace of run %s is %s, want %s: the first sixteen bytes of the SHA-256 of its identifier", tracedRun, got, want)
	}
	if got, want := span.String(), "1bcead9eed447f5e"; got != want {
		t.Errorf("the span of run %s is %s, want %s: the next eight bytes of the same hash", tracedRun, got, want)
	}
}

func TestATaskNamesItsSpanFromTheHashOfItsIdentifier(t *testing.T) {
	if got, want := agk.TaskSpan(tracedTask).String(), "1e176d343d62934f"; got != want {
		t.Errorf("the span of %s is %s, want %s: the first eight bytes of the SHA-256 of its identifier", tracedTask, got, want)
	}
}

func TestTheTraceParentIsTraceContextVersionZeroAndSampled(t *testing.T) {
	got := agk.TraceParent(tracedRun, tracedTask)
	if want := "00-65adc84f140f8df02002e0395b782fe8-1e176d343d62934f-01"; got != want {
		t.Fatalf("the traceparent is %q, want %q", got, want)
	}
	// The grammar of version 00, as W3C Trace Context writes it.
	if !regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`).MatchString(got) {
		t.Errorf("%q is not a version 00 traceparent", got)
	}
}

func TestTwoDispatchesOfOneRunShareTheTraceAndNotTheSpan(t *testing.T) {
	first := agk.TraceParent(tracedRun, "01M2Z8V1P9C4XQ7K2N4D6F8H0D")
	second := agk.TraceParent(tracedRun, "01M2Z8V1P9C4XQ7K2N4D6F8H0E")
	if first[3:35] != second[3:35] {
		t.Errorf("two dispatches of one run are in traces %s and %s", first[3:35], second[3:35])
	}
	if first[36:52] == second[36:52] {
		t.Errorf("two dispatches of one run share the span %s", first[36:52])
	}
}

func TestNothingIsTracedUnderAnIdentifierThatIsNotThere(t *testing.T) {
	for _, c := range []struct {
		run  agk.RunID
		task string
	}{{"", tracedTask}, {tracedRun, ""}} {
		if got := agk.TraceParent(c.run, c.task); got != "" {
			t.Errorf("run %q and task %q have the traceparent %q, want none", c.run, c.task, got)
		}
	}
}
