package agk_test

import (
	"testing"

	"github.com/agentiik/agentiik/agk"
)

// TestTheSevenRunStates holds the run states table: the seven states, spelled as the
// documentation spells them, and the four that end a run.
//
// The four terminal ones are the run verdict. There is no second type for it, which is
// what this test is really holding: a verdict that could disagree with the state would
// be a run reported as succeeded by one half of the engine and failed by the other.
func TestTheSevenRunStates(t *testing.T) {
	for _, c := range []struct {
		state    agk.RunState
		name     string
		terminal bool
	}{
		{agk.Queued, "queued", false},
		{agk.Running, "running", false},
		{agk.Waiting, "waiting", false},
		{agk.Succeeded, "succeeded", true},
		{agk.Failed, "failed", true},
		{agk.Cancelled, "cancelled", true},
		{agk.TimedOut, "timed_out", true},
	} {
		if got := c.state.String(); got != c.name {
			t.Errorf("the state spells itself %q and the documentation writes %q", got, c.name)
		}
		if got := c.state.Terminal(); got != c.terminal {
			t.Errorf("%s reports terminal=%v", c.name, got)
		}
	}
}

// TestARunThatExistsIsAtLeastQueued holds the reading the zero value takes: a run is
// created before it is anything else, and a state nobody has set reads as the state a
// run starts in rather than as a state that does not exist.
func TestARunThatExistsIsAtLeastQueued(t *testing.T) {
	var r agk.Run
	if r.State != agk.Queued {
		t.Fatalf("a run nobody has started reads as %s", r.State)
	}
	if r.State.Terminal() {
		t.Fatal("a run nobody has started reads as finished")
	}
}

// TestWhatStartedTheRun holds the four trigger kinds, which are the four trigger blocks
// the language has plus the one a principal asks for by hand.
func TestWhatStartedTheRun(t *testing.T) {
	for _, c := range []struct {
		kind agk.TriggerKind
		name string
	}{
		{agk.TriggerManual, "manual"},
		{agk.TriggerWebhook, "webhook"},
		{agk.TriggerSchedule, "schedule"},
		{agk.TriggerEvent, "event"},
	} {
		if got := c.kind.String(); got != c.name {
			t.Errorf("the kind spells itself %q and not %q", got, c.name)
		}
		var back agk.TriggerKind
		if err := back.UnmarshalText([]byte(c.name)); err != nil || back != c.kind {
			t.Errorf("%q comes back as %s: %v", c.name, back, err)
		}
	}
}
