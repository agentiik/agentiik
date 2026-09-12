package driver

import (
	"bytes"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

// The exit code table, read row by row. The bands themselves are agk's and are tested
// there; what is held here is what the driver does with each of them, which is the state
// it reports and who the code is charged to.

func TestTheExitCodeTableDecidesTheState(t *testing.T) {
	for _, c := range []struct {
		code  int
		state agk.TaskState
		why   string
	}{
		{0, agk.TaskSucceeded, "success: the output envelopes are published"},
		{1, agk.TaskFailed, "application failure"},
		{99, agk.TaskFailed, "application failure"},
		{100, agk.TaskFailed, "transient failure"},
		{119, agk.TaskFailed, "transient failure"},
		{120, agk.TaskFailed, "invalid input"},
		{121, agk.TaskFailed, "reserved for the runner"},
		{124, agk.TaskFailed, "reserved for the runner"},
		{125, agk.TaskFailed, "reserved for the runtime"},
		{137, agk.TaskFailed, "reserved for the runtime, which a SIGKILL leaves"},
		{255, agk.TaskFailed, "reserved for the runtime"},
	} {
		if got := exitState(c.code); got != c.state {
			t.Errorf("exit %d is %s, want %s (%s)", c.code, got, c.state, c.why)
		}
	}
}

// 125 and above is charged to the runner and not to the brick. The charge is what the
// driver does with the fact and not a code it invents: the code is reported exactly as
// the container exited.
func TestOnlyTheRuntimeBandIsChargedToThePlatform(t *testing.T) {
	for _, code := range []int{0, 1, 99, 100, 119, 120, 121, 124} {
		if chargedToPlatform(code) {
			t.Errorf("exit %d was charged to the platform, and it is the brick's", code)
		}
	}
	for _, code := range []int{125, 126, 127, 137, 143, 255} {
		if !chargedToPlatform(code) {
			t.Errorf("exit %d was charged to the brick, and it is reserved for the runtime", code)
		}
	}
}

// A code below zero is not one a container can exit with, and agk reads it where the
// codes that are not the brick's are read. Charging it to the brick would let a driver
// reporting its own trouble fail somebody else's step.
func TestACodeNoContainerCouldHaveExitedWithIsNotTheBricks(t *testing.T) {
	if !chargedToPlatform(-1) {
		t.Errorf("exit -1 was charged to the brick")
	}
	if got := exitState(-1); got != agk.TaskFailed {
		t.Errorf("exit -1 is %s, want %s", got, agk.TaskFailed)
	}
}

// Never lost, whatever the code. That is the heartbeat's word for a runner that stopped
// reporting, and a container that exited 137 reported.
func TestAnExitCodeIsNeverReadAsLost(t *testing.T) {
	for code := -2; code <= 300; code++ {
		if got := exitState(code); got == agk.TaskLost {
			t.Fatalf("exit %d was read as %s", code, got)
		}
		if s := exitState(code); s != agk.TaskSucceeded && s != agk.TaskFailed {
			t.Fatalf("exit %d is %s, and a code says only whether the container succeeded or failed", code, s)
		}
	}
}

// The log line is the second half of the charge. It names the band and its handling in
// the table's own words, so that a person reading the log reads the rule.
func TestTheExitNoteNamesTheBandAndItsHandling(t *testing.T) {
	for _, c := range []struct {
		code int
		says []string
	}{
		{0, []string{"exited 0", "published"}},
		{7, []string{"application failure", "no retry unless retry.on says so explicitly"}},
		{110, []string{"transient failure", "retried according to the step policy"}},
		{120, []string{"invalid input", "never retried, whatever retry says"}},
		{123, []string{"reserved for the runner", "failed the contract"}},
		{137, []string{"reserved for the runtime", "charged to the runner and not to the brick"}},
	} {
		note := exitNote(c.code)
		for _, want := range c.says {
			if !strings.Contains(note, want) {
				t.Errorf("exit %d: the note does not say %q: %s", c.code, want, note)
			}
		}
	}
}

// The code itself is in the line, because the band is a range and an operator chasing an
// out-of-memory kill is looking for 137.
func TestTheExitNoteCarriesTheCode(t *testing.T) {
	if got := exitNote(137); !strings.Contains(got, "137") {
		t.Errorf("the note does not carry the code: %s", got)
	}
}

// Reading the code and writing the line are one call, because the second half of
// charging a code to the runtime is saying so where a person will read it.
func TestReadingAnExitCodeWritesTheLineThatChargesIt(t *testing.T) {
	var sink bytes.Buffer
	l := newLog(&sink, nil, logClock(), 0, 0)

	if got := readExit(l, 137); got != agk.TaskFailed {
		t.Errorf("exit 137 is %s, want %s", got, agk.TaskFailed)
	}
	if _, err := l.finish(); err != nil {
		t.Fatal(err)
	}

	lines := logLines(t, &sink)
	if len(lines) != 1 {
		t.Fatalf("wrote %d lines, want the one the exit code table wrote", len(lines))
	}
	if !strings.Contains(lines[0].Text, "charged to the runner and not to the brick") {
		t.Errorf("the log does not charge the code: %q", lines[0].Text)
	}
	if !strings.HasPrefix(lines[0].Text, notePrefix) {
		t.Errorf("the line is not marked as the runner's: %q", lines[0].Text)
	}
}
