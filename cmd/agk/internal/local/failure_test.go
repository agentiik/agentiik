package local

import (
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
)

// A step refused before any task started has no shard to report, and is a failure of its own with
// its reason, so that the run's report never says no step failed of it; a step whose task failed
// gives its reason to that task's failure, and a skipped step's reason is no failure.
func TestAStepRefusedBeforeAnyTaskIsAFailureWithItsReason(t *testing.T) {
	const refused = "graph: step normalize: port orders: inputs feeds the port a number: a port carries a batch of items, so the value is an object or a list of objects"
	state := &graph.State{
		Run: agk.Run{ID: "01M3B00000000000000000000R"},
		Steps: map[agk.Step]graph.StepState{
			"archive":   {Verdict: agk.VerdictSkipped, Reason: "its when refused the upstream state failed"},
			"charge":    {Verdict: agk.VerdictFailed, Reason: "the step's own word", Shards: []graph.ShardState{{Attempt: 1, Task: agk.TaskFailed, ExitCode: 3}}},
			"normalize": {Verdict: agk.VerdictFailed, Reason: refused},
		},
	}
	got := failures(state, nil, nil, Layout{Root: t.TempDir()})
	if len(got) != 2 {
		t.Fatalf("the failures are %+v", got)
	}
	if f := got[0]; f.Step != "charge" || !f.HasExit || f.ExitCode != 3 || f.Reason != "the step's own word" {
		t.Errorf("a failed task is reported as %+v", f)
	}
	if f := got[1]; f.Step != "normalize" || f.HasExit || f.Attempt != 0 || f.State != agk.TaskFailed || f.Reason != refused {
		t.Errorf("a step refused before any task is reported as %+v", f)
	}
}
