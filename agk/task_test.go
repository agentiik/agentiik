package agk_test

import (
	"encoding/json"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

// TestTheExitCodeTable walks the table row by row, at both edges of every band, because
// the bands are the only place a verdict comes from and an edge read one out is a step
// failed where the documentation says it was unlucky.
func TestTheExitCodeTable(t *testing.T) {
	for _, c := range []struct {
		code int
		band agk.ExitBand
	}{
		{0, agk.BandSuccess},
		{1, agk.BandApplicationFailure},
		{42, agk.BandApplicationFailure},
		{99, agk.BandApplicationFailure},
		{100, agk.BandTransientFailure},
		{119, agk.BandTransientFailure},
		{120, agk.BandInvalidInput},
		{121, agk.BandReservedForRunner},
		{124, agk.BandReservedForRunner},
		{125, agk.BandRuntimeFailure},
		{137, agk.BandRuntimeFailure},
		{255, agk.BandRuntimeFailure},
	} {
		if got := agk.Band(c.code); got != c.band {
			t.Errorf("exit %d is %s, and the table says %s", c.code, got, c.band)
		}
	}
}

// TestACodeBelowZeroIsNotTheBricks holds the reading Band records: a negative number is
// not something a container exited with, so it is charged where the codes that are not
// the brick's are charged.
func TestACodeBelowZeroIsNotTheBricks(t *testing.T) {
	if got := agk.Band(-1); got != agk.BandRuntimeFailure {
		t.Fatalf("exit -1 is %s, and a code no container can produce is charged to the runner", got)
	}
}

// TestWhatMayBeRetriedAtAll holds the Handling column. Invalid input is never retried
// whatever retry says, and the two runner bands are not the step's to retry either.
func TestWhatMayBeRetriedAtAll(t *testing.T) {
	for _, c := range []struct {
		band      agk.ExitBand
		retryable bool
	}{
		{agk.BandSuccess, false},
		{agk.BandApplicationFailure, true},
		{agk.BandTransientFailure, true},
		{agk.BandInvalidInput, false},
		{agk.BandReservedForRunner, false},
		{agk.BandRuntimeFailure, false},
	} {
		if got := c.band.Retryable(); got != c.retryable {
			t.Errorf("%s reports retryable=%v, and the table says %v", c.band, got, c.retryable)
		}
	}
}

// TestTheNameARetryPolicyWouldHaveToUse holds the other half of the same rule: a band
// that can be retried has a name retry.on can write, and one that cannot has none.
func TestTheNameARetryPolicyWouldHaveToUse(t *testing.T) {
	for _, c := range []struct {
		band  agk.ExitBand
		kind  agk.Failure
		named bool
	}{
		{agk.BandApplicationFailure, agk.FailureFailed, true},
		{agk.BandTransientFailure, agk.FailureTransient, true},
		{agk.BandSuccess, 0, false},
		{agk.BandInvalidInput, 0, false},
		{agk.BandReservedForRunner, 0, false},
		{agk.BandRuntimeFailure, 0, false},
	} {
		kind, named := c.band.Failure()
		if named != c.named || kind != c.kind {
			t.Errorf("%s names %v (%v), and the table says %v (%v)", c.band, kind, named, c.kind, c.named)
		}
		if named != c.band.Retryable() {
			t.Errorf("%s: a band with a name is a band that may be retried, and these disagree", c.band)
		}
	}
}

// TestInfrastructureFailureIsNotTransient holds the reading the package records: exit
// 125 and above is charged to the runner, retry.on has no word for it, and folding it
// into transient would retry on the step's policy what the step is not responsible for.
func TestInfrastructureFailureIsNotTransient(t *testing.T) {
	kind, named := agk.Band(125).Failure()
	if named {
		t.Fatalf("exit 125 names %s, and retry.on has no name for an infrastructure failure", kind)
	}
}

// TestRetryOnNamesExactlyFour holds the enumeration of the keyword. A fifth name here
// would be a name a workflow file has no way to write.
func TestRetryOnNamesExactlyFour(t *testing.T) {
	for name, want := range map[string]agk.Failure{
		"transient": agk.FailureTransient,
		"failed":    agk.FailureFailed,
		"lost":      agk.FailureLost,
		"timeout":   agk.FailureTimeout,
	} {
		got, err := agk.ParseFailure(name)
		if err != nil {
			t.Fatalf("retry.on: [%s] is refused: %v", name, err)
		}
		if got != want {
			t.Errorf("%s parses to %s", name, got)
		}
		if got.String() != name {
			t.Errorf("%s spells itself %q, and retry.on writes %q", name, got.String(), name)
		}
	}
	for _, name := range []string{"", "timed_out", "infrastructure", "invalid_input", "cancelled", "Transient"} {
		if _, err := agk.ParseFailure(name); err == nil {
			t.Errorf("retry.on: [%s] is accepted, and the keyword names transient, failed, lost and timeout", name)
		}
	}
}

// TestTaskStatesThatAreOver holds which states end a task. Lost is one of them: the task
// is over as far as anyone can tell, and what follows is a new attempt with its own
// identity.
func TestTaskStatesThatAreOver(t *testing.T) {
	for _, c := range []struct {
		state    agk.TaskState
		terminal bool
	}{
		{agk.TaskPending, false},
		{agk.TaskDispatched, false},
		{agk.TaskRunning, false},
		{agk.TaskPublishing, false},
		{agk.TaskSucceeded, true},
		{agk.TaskFailed, true},
		{agk.TaskLost, true},
		{agk.TaskTimedOut, true},
		{agk.TaskCancelled, true},
	} {
		if got := c.state.Terminal(); got != c.terminal {
			t.Errorf("%s reports terminal=%v", c.state, got)
		}
	}
	if agk.TaskState(0) != agk.TaskPending {
		t.Error("a task nobody has dispatched reads as something other than pending")
	}
}

// TestAShardIsWrittenTheWayAgkShardWritesIt holds the one spelling of a shard, which the
// environment variable, the task identifier and the console all use.
func TestAShardIsWrittenTheWayAgkShardWritesIt(t *testing.T) {
	if got := (agk.Shard{Index: 3, Of: 8}).String(); got != "3/8" {
		t.Errorf("a shard prints %q, and AGK_SHARD writes 3/8", got)
	}
	var none agk.Shard
	if !none.IsZero() || none.String() != "" {
		t.Errorf("a step with no fan-out carries %q, and AGK_SHARD is absent there", none.String())
	}
	if err := none.Validate(); err != nil {
		t.Errorf("a step with no fan-out is refused a shard it does not have: %v", err)
	}
	for _, sh := range []agk.Shard{{Index: 0, Of: 8}, {Index: 9, Of: 8}, {Index: 1, Of: 0}, {Index: -1, Of: 2}} {
		if err := sh.Validate(); err == nil {
			t.Errorf("shard %d/%d is accepted, and an index is counted from one and never past the cardinality", sh.Index, sh.Of)
		}
	}
}

// TestTheTaskIdentifierIsDerived holds the idempotency key: the same decision taken
// twice gives the same identifier, and it takes apart into the parts it was made of.
func TestTheTaskIdentifierIsDerived(t *testing.T) {
	for _, c := range []struct {
		run     agk.RunID
		step    agk.Step
		attempt int
		shard   agk.Shard
		id      agk.TaskID
	}{
		{"01HZX", "normalize", 1, agk.Shard{}, "01HZX/normalize/1"},
		{"01HZX", "invoice", 2, agk.Shard{Index: 3, Of: 8}, "01HZX/invoice/2/3/8"},
	} {
		got := agk.NewTaskID(c.run, c.step, c.attempt, c.shard)
		if got != c.id {
			t.Errorf("the identifier is %q and not %q", got, c.id)
		}
		if again := agk.NewTaskID(c.run, c.step, c.attempt, c.shard); again != got {
			t.Errorf("the same task is identified twice as %q then %q", got, again)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("%q does not validate: %v", got, err)
		}
		run, step, attempt, shard, err := agk.ParseTaskID(string(got))
		if err != nil {
			t.Fatalf("%q does not parse: %v", got, err)
		}
		if run != c.run || step != c.step || attempt != c.attempt || shard != c.shard {
			t.Errorf("%q takes apart into %q %q %d %v", got, run, step, attempt, shard)
		}
	}
}

// TestATaskIdentifierHasOneSpelling holds what Validate is for: two spellings of one
// number would be two identifiers for one task, and a runner deduplicating on the string
// would run the work twice.
func TestATaskIdentifierHasOneSpelling(t *testing.T) {
	for _, id := range []agk.TaskID{
		"01HZX/normalize/01",
		"01HZX/normalize/0",
		"01HZX/normalize/-1",
		"01HZX/normalize",
		"01HZX/normalize/1/3",
		"01HZX/normalize/1/0/0",
		"01HZX/normalize/1/9/8",
		"01HZX//1",
		"/normalize/1",
		"01HZX/nor malize/1",
	} {
		if err := id.Validate(); err == nil {
			t.Errorf("%q is accepted as a task identifier", id)
		}
	}
}

// TestTheVocabularyTravelsAsItIsSpelled holds the JSON round trip of the four
// enumerations a run is persisted with. A state that travels as a number is a state
// nobody can read in a log, and a database holding 4 has to be told what 4 was.
func TestTheVocabularyTravelsAsItIsSpelled(t *testing.T) {
	type doc struct {
		Run     agk.RunState    `json:"run"`
		Verdict agk.Verdict     `json:"verdict"`
		Task    agk.TaskState   `json:"task"`
		Trigger agk.TriggerKind `json:"trigger"`
		Failure agk.Failure     `json:"failure"`
	}
	in := doc{agk.TimedOut, agk.VerdictSkipped, agk.TaskPublishing, agk.TriggerSchedule, agk.FailureTimeout}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"run":"timed_out","verdict":"skipped","task":"publishing","trigger":"schedule","failure":"timeout"}`
	if string(b) != want {
		t.Errorf("it travels as %s\nand the documentation spells it %s", b, want)
	}
	var out doc
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("it comes back as %+v", out)
	}
	if err := json.Unmarshal([]byte(`{"run":"finished"}`), &out); err == nil {
		t.Error("a run state of finished is accepted, and there are seven")
	}
}
