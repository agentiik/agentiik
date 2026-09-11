package graph

import (
	"math"
	"reflect"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

// none gives one container the whole envelope, and it carries no shard, because there is
// no series for it to sit in.
func TestFanOutNoneRunsOneContainerOnTheWholeEnvelope(t *testing.T) {
	in := map[agk.Port]agk.Envelope{"in": schedEnvelope("normalize", "ok", schedItem("a1", nil), schedItem("a2", nil))}
	shards, err := fanOut("invoice", &Step{}, in)
	if err != nil {
		t.Fatalf("fan_out none: %v", err)
	}
	if len(shards) != 1 {
		t.Fatalf("%d shards, want one container", len(shards))
	}
	if !shards[0].Shard.IsZero() {
		t.Errorf("shard %s, want none: there is no fan-out", shards[0].Shard)
	}
	if !reflect.DeepEqual(shards[0].Inputs, in) {
		t.Error("the container was given something other than the whole envelope")
	}
}

// item gives one container per item, each receiving a single-item envelope.
func TestFanOutItemGivesOneContainerPerItem(t *testing.T) {
	in := map[agk.Port]agk.Envelope{"in": schedEnvelope("normalize", "ok",
		schedItem("a1", nil), schedItem("a2", nil), schedItem("a3", nil))}
	st := &Step{Strategy: Strategy{FanOut: FanOutItem}}
	shards, err := fanOut("invoice", st, in)
	if err != nil {
		t.Fatalf("fan_out item: %v", err)
	}
	if len(shards) != 3 {
		t.Fatalf("%d shards, want one per item", len(shards))
	}
	for i, s := range shards {
		if s.Shard != (agk.Shard{Index: i + 1, Of: 3}) {
			t.Errorf("shard %d is %s, want %d/3", i, s.Shard, i+1)
		}
		if got := s.Inputs["in"]; len(got.Items) != 1 || got.Meta.Count != 1 {
			t.Errorf("shard %s received %d items, want a single-item envelope", s.Shard, len(got.Items))
		}
		if s.Item == nil || s.Item.ID != s.Inputs["in"].Items[0].ID {
			t.Errorf("shard %s carries no current item", s.Shard)
		}
	}
}

// An envelope holding no items splits into no shards, which is a fan-out over an empty
// batch starting no containers.
func TestFanOutOverAnEmptyBatchStartsNoContainers(t *testing.T) {
	in := map[agk.Port]agk.Envelope{"in": schedEnvelope("normalize", "ok")}
	for _, st := range []*Step{
		{Strategy: Strategy{FanOut: FanOutItem}},
		{Strategy: Strategy{FanOut: FanOutBatch, Batch: 50}},
	} {
		shards, err := fanOut("invoice", st, in)
		if err != nil {
			t.Fatalf("fan-out: %v", err)
		}
		if len(shards) != 0 {
			t.Errorf("%d shards over a batch of nothing, want none", len(shards))
		}
	}
}

// A step declaring no input port has nothing to shard on and runs as one container.
func TestAStepWithNoInputPortRunsOnceWhateverTheFanOut(t *testing.T) {
	for _, st := range []*Step{
		{Strategy: Strategy{FanOut: FanOutItem}},
		{Strategy: Strategy{FanOut: FanOutBatch, Batch: 50}},
	} {
		shards, err := fanOut("fetch-ledger", st, nil)
		if err != nil {
			t.Fatalf("fan-out: %v", err)
		}
		if len(shards) != 1 || !shards[0].Shard.IsZero() {
			t.Errorf("%d shards, want the one container a step with no inputs is", len(shards))
		}
	}
}

// batch(n) gives one container per batch of n items, and the last one holds what is left.
func TestFanOutBatchGivesOneContainerPerBatch(t *testing.T) {
	items := []agk.Item{schedItem("a1", nil), schedItem("a2", nil), schedItem("a3", nil), schedItem("a4", nil), schedItem("a5", nil)}
	in := map[agk.Port]agk.Envelope{"in": schedEnvelope("fetch", "out", items...)}
	st := &Step{Strategy: Strategy{FanOut: FanOutBatch, Batch: 2}}
	shards, err := fanOut("load", st, in)
	if err != nil {
		t.Fatalf("fan_out batch: %v", err)
	}
	var sizes []int
	for _, s := range shards {
		sizes = append(sizes, len(s.Inputs["in"].Items))
	}
	if !reflect.DeepEqual(sizes, []int{2, 2, 1}) {
		t.Errorf("batches of %v, want 2, 2 and what is left", sizes)
	}
	if shards[0].Item != nil {
		t.Error("a batch carries no current item: item is available only under fan_out: item")
	}
}

func TestFanOutBatchRefusesASizeNoBatchCouldHave(t *testing.T) {
	in := map[agk.Port]agk.Envelope{"in": schedEnvelope("fetch", "out", schedItem("a1", nil))}
	if _, err := fanOut("load", &Step{Strategy: Strategy{FanOut: FanOutBatch}}, in); err == nil {
		t.Error("batch(0) was accepted, and a batch of no items is a shard that can never be filled")
	}
}

// Ports of differing length shard on the longest, and the shorter one gives the shards
// past its end a batch of nothing.
func TestPortsOfDifferingLengthShardOnTheLongest(t *testing.T) {
	in := map[agk.Port]agk.Envelope{
		"rows":   schedEnvelope("a", "out", schedItem("r1", nil), schedItem("r2", nil)),
		"header": schedEnvelope("b", "out", schedItem("h1", nil)),
	}
	st := &Step{Strategy: Strategy{FanOut: FanOutItem}}
	shards, err := fanOut("s", st, in)
	if err != nil {
		t.Fatalf("fan_out item: %v", err)
	}
	if len(shards) != 2 {
		t.Fatalf("%d shards, want one per item of the longest port", len(shards))
	}
	if got := shards[1].Inputs["header"]; len(got.Items) != 0 || got.Meta.Count != 0 {
		t.Errorf("the second shard received %d items on header, want a batch of nothing", len(got.Items))
	}
	if got := shards[1].Inputs["header"].Meta.Step; got != "b" {
		t.Errorf("the empty port says it came from %s, want the step that published it", got)
	}
}

// matrix runs the cartesian product of the variable lists, one shard per combination,
// each with its variables injected and each given the whole of every port.
func TestFanOutMatrixRunsEveryCombination(t *testing.T) {
	in := map[agk.Port]agk.Envelope{"in": schedEnvelope("a", "out", schedItem("a1", nil))}
	st := &Step{Strategy: Strategy{Matrix: map[string][]any{
		"region":  {"fr", "be", "ch"},
		"profile": {"standard", "reduced"},
	}}}
	shards, err := fanOut("rate-table", st, in)
	if err != nil {
		t.Fatalf("matrix: %v", err)
	}
	if len(shards) != 6 {
		t.Fatalf("%d shards, want the six combinations", len(shards))
	}
	var seen []string
	for i, s := range shards {
		if s.Shard != (agk.Shard{Index: i + 1, Of: 6}) {
			t.Errorf("shard %d is %s", i, s.Shard)
		}
		if !reflect.DeepEqual(s.Inputs, in) {
			t.Errorf("shard %s was given something other than the whole envelope", s.Shard)
		}
		seen = append(seen, s.Matrix["profile"].(string)+"/"+s.Matrix["region"].(string))
	}
	// Variable name order, with the last name moving fastest, so that the same matrix
	// always produces the same shard at the same index and a replay lands where the
	// run did.
	want := []string{
		"standard/fr", "standard/be", "standard/ch",
		"reduced/fr", "reduced/be", "reduced/ch",
	}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("combinations %v, want %v in that order every time", seen, want)
	}
}

// A strategy carrying a matrix is a matrix fan-out whether or not it says so, which is
// how the documentation's own hidden block writes it.
func TestAMatrixIsAFanOutWithoutSayingSo(t *testing.T) {
	st := &Step{Strategy: Strategy{Matrix: map[string][]any{"region": {"fr", "be"}}}}
	if got := fanOutOf(st); got != FanOutMatrix {
		t.Errorf("fan-out %v, want matrix", got)
	}
	shards, err := fanOut("rate-table", st, nil)
	if err != nil {
		t.Fatalf("matrix: %v", err)
	}
	if len(shards) != 2 {
		t.Errorf("%d shards, want one per combination", len(shards))
	}
}

// max_parallel is a ceiling on how many shards of this step run at the same time. A
// retry occupies a runner exactly as a first attempt does, so it counts.
func TestMaxParallelCountsEveryShardInFlight(t *testing.T) {
	ss := StepState{Shards: []ShardState{
		{Shard: agk.Shard{Index: 1, Of: 4}, Task: agk.TaskRunning},
		{Shard: agk.Shard{Index: 2, Of: 4}, Attempt: 2, Task: agk.TaskDispatched},
		{Shard: agk.Shard{Index: 3, Of: 4}, Task: agk.TaskSucceeded},
		{Shard: agk.Shard{Index: 4, Of: 4}, Task: agk.TaskPending},
	}}
	st := &Step{Strategy: Strategy{FanOut: FanOutItem, MaxParallel: 3}}
	if got := slotsFree(st, ss); got != 1 {
		t.Errorf("free %d, want the ceiling less the two shards holding a runner", got)
	}
	if got := slotsFree(&Step{Strategy: Strategy{FanOut: FanOutItem}}, ss); got != math.MaxInt {
		t.Errorf("free %d, want every shard at once where no ceiling is stated", got)
	}
}

// fail_fast stops the shards still running when one has failed for good. The shard that
// failed has already ended, and a shard nobody has handed out holds nothing.
func TestFailFastStopsTheShardsStillRunning(t *testing.T) {
	failed := agk.Shard{Index: 2, Of: 4}
	ss := StepState{Shards: []ShardState{
		{Shard: agk.Shard{Index: 1, Of: 4}, Task: agk.TaskRunning},
		{Shard: failed, Task: agk.TaskFailed},
		{Shard: agk.Shard{Index: 3, Of: 4}, Task: agk.TaskPublishing},
		{Shard: agk.Shard{Index: 4, Of: 4}, Task: agk.TaskPending},
	}}
	var stopped []int
	for _, s := range siblingsInFlight(ss, failed) {
		stopped = append(stopped, s.Shard.Index)
	}
	if !reflect.DeepEqual(stopped, []int{1, 3}) {
		t.Errorf("stopped %v, want the shards still in flight", stopped)
	}
	if !failFast(&Step{Strategy: Strategy{FailFast: true}}) || failFast(&Step{}) {
		t.Error("fail_fast is read off the strategy and defaults to letting every shard finish")
	}
}
