package agk_test

import (
	"testing"

	"github.com/agentiik/agentiik/agk"
)

// TestTheStepVerdicts holds what a step can be and how it is spelled. The three that a
// downstream when names, succeeded, failed and skipped, are spelled exactly as the when
// enumeration spells them, because that comparison is the whole point of one type.
func TestTheStepVerdicts(t *testing.T) {
	for _, c := range []struct {
		verdict  agk.Verdict
		name     string
		terminal bool
	}{
		{agk.VerdictPending, "pending", false},
		{agk.VerdictRunning, "running", false},
		{agk.VerdictSucceeded, "succeeded", true},
		{agk.VerdictFailed, "failed", true},
		{agk.VerdictSkipped, "skipped", true},
		{agk.VerdictCancelled, "cancelled", true},
	} {
		if got := c.verdict.String(); got != c.name {
			t.Errorf("the verdict spells itself %q and not %q", got, c.name)
		}
		if got := c.verdict.Terminal(); got != c.terminal {
			t.Errorf("%s reports terminal=%v", c.name, got)
		}
	}
}

// TestAStepNobodyHasTouchedIsPending holds the zero value, which the evaluator's state
// leans on: a step with no entry of its own is a step that has not started.
func TestAStepNobodyHasTouchedIsPending(t *testing.T) {
	var v agk.Verdict
	if v != agk.VerdictPending {
		t.Fatalf("the zero verdict is %s", v)
	}
}

// TestAlwaysIsNotAVerdict holds the reading the type records. when names succeeded,
// failed, skipped and always; always is a way of naming any of them and not a state a
// step ever reaches, and pending and running are states a step reaches that when may not
// name. One type for both readings does not make them the same list.
func TestAlwaysIsNotAVerdict(t *testing.T) {
	var v agk.Verdict
	if err := v.UnmarshalText([]byte("always")); err == nil {
		t.Fatal("always reads back as a step verdict, and no step is ever in it")
	}
	for _, name := range []string{"succeeded", "failed", "skipped"} {
		if err := v.UnmarshalText([]byte(name)); err != nil {
			t.Errorf("when names %s and a verdict cannot be read from it: %v", name, err)
		}
	}
}
