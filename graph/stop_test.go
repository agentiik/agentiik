package graph

import (
	"encoding/json"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

// A stop is written by the controller and read by a runner, so each reason has to come back as the
// reason it went out as. The spellings are the wire's, and the reason read back is what decides
// whether the stopped task ends timed_out or cancelled.
func TestEveryStopReasonComesBackFromItsSpelling(t *testing.T) {
	for reason, spelled := range map[StopReason]string{
		StopSuperseded:    "superseded",
		StopSiblingFailed: "sibling_failed",
		StopDeadline:      "deadline",
		StopCancelled:     "cancelled",
	} {
		text, err := reason.MarshalText()
		if err != nil {
			t.Fatalf("%s: %s", spelled, err)
		}
		if string(text) != spelled {
			t.Errorf("%s is written %q", spelled, text)
		}
		var back StopReason
		if err := back.UnmarshalText(text); err != nil {
			t.Fatalf("%s: %s", spelled, err)
		}
		if back != reason {
			t.Errorf("%s came back as %s", spelled, back)
		}

		// And inside the stop, the way the stop travels.
		body, err := json.Marshal(Stop{Task: "01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize/1", Reason: reason})
		if err != nil {
			t.Fatal(err)
		}
		var stop Stop
		if err := json.Unmarshal(body, &stop); err != nil {
			t.Fatalf("%s: %s", body, err)
		}
		if stop.Reason != reason || stop.Task != agk.TaskID("01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize/1") {
			t.Errorf("%s came back as %+v", body, stop)
		}
	}
	if len(stopReasons) != 4 {
		t.Errorf("there are %d stop reasons and this test reads four", len(stopReasons))
	}
}

// A spelling that is not one of the four is refused rather than read as some reason, since the
// reason decides what the task becomes. A task state is not a reason, nor is the word String falls
// back on.
func TestAStopReasonNobodyWroteIsRefused(t *testing.T) {
	for _, text := range []string{"", "stopped", "timed_out", "Cancelled", "cancelled ", "lost"} {
		var r StopReason
		if err := r.UnmarshalText([]byte(text)); err == nil {
			t.Errorf("%q was read as %s", text, r)
		}
	}
}

// A reason with no spelling cannot be written, where String would have written "stopped": a stop
// nobody can read is a container that runs to its deadline.
func TestAStopReasonWithNoSpellingIsNotWritten(t *testing.T) {
	for _, r := range []StopReason{-1, StopReason(len(stopReasons))} {
		if text, err := r.MarshalText(); err == nil {
			t.Errorf("reason %d was written %q", int(r), text)
		}
		if _, err := json.Marshal(Stop{Task: "01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize/1", Reason: r}); err == nil {
			t.Errorf("a stop for reason %d was written", int(r))
		}
	}
}
