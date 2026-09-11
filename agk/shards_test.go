package agk_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
)

func batch(t *testing.T, n int) agk.Envelope {
	t.Helper()
	items := make([]agk.Item, n)
	for i := range items {
		items[i] = agk.NewItem(map[string]any{"rank": i})
	}
	return agk.Envelope{
		Meta:  agk.Meta{RunID: agk.NewRunID(), Step: "normalize", Port: "ok", Attempt: 1, Count: n, ProducedAt: at},
		Items: items,
	}
}

// TestAnItemKeepsItsIdentityThroughAFanOutAndAMerge is the whole of what an item
// identifier is for: the batch is split one container per item, each container hands
// its shard back, the shards are concatenated port by port, and the item that comes out
// is the item that went in, under the name the port that produced it gave it.
func TestAnItemKeepsItsIdentityThroughAFanOutAndAMerge(t *testing.T) {
	before := batch(t, 5)

	shards, err := agk.Split(before, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(shards) != 5 {
		t.Fatalf("a fan-out over five items made %d shards", len(shards))
	}
	for i, s := range shards {
		if len(s.Items) != 1 || s.Meta.Count != 1 {
			t.Fatalf("shard %d holds %d items under a count of %d", i, len(s.Items), s.Meta.Count)
		}
		if s.Items[0].ID != before.Items[i].ID {
			t.Fatalf("shard %d carries %q and the item is %q", i, s.Items[0].ID, before.Items[i].ID)
		}
		if err := s.Validate(agk.DefaultLimits()); err != nil {
			t.Fatalf("shard %d is not an envelope: %v", i, err)
		}
	}

	after, err := agk.Concat(shards, agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Items) != len(before.Items) {
		t.Fatalf("five items went out and %d came back", len(after.Items))
	}
	for i := range after.Items {
		if after.Items[i].ID != before.Items[i].ID {
			t.Fatalf("item %d left as %q and came back as %q", i, before.Items[i].ID, after.Items[i].ID)
		}
	}
	if after.Meta.Count != 5 {
		t.Fatalf("the published envelope says it holds %d items", after.Meta.Count)
	}
}

func TestSplitCutsInOrderAndKeepsTheRemainder(t *testing.T) {
	e := batch(t, 7)

	shards, err := agk.Split(e, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := []int{len(shards[0].Items), len(shards[1].Items), len(shards[2].Items)}; len(shards) != 3 || got[0] != 3 || got[1] != 3 || got[2] != 1 {
		t.Fatalf("seven items in batches of three made shards of %v", got)
	}

	var seen []string
	for _, s := range shards {
		for _, item := range s.Items {
			seen = append(seen, item.ID)
		}
	}
	for i, id := range seen {
		if id != e.Items[i].ID {
			t.Fatalf("item %d is %q in the shards and %q in the batch", i, id, e.Items[i].ID)
		}
	}
}

func TestSplitLeavesTheBatchAlone(t *testing.T) {
	e := batch(t, 4)
	shards, err := agk.Split(e, 2)
	if err != nil {
		t.Fatal(err)
	}
	shards[0].Items[0].ID = "rewritten"
	if e.Items[0].ID == "rewritten" {
		t.Fatal("a shard shares its items with the batch it was cut from")
	}
}

// TestAnEmptyBatchStartsNoContainers: a fan-out over a port that carried nothing has
// nothing to give a container.
func TestAnEmptyBatchStartsNoContainers(t *testing.T) {
	shards, err := agk.Split(agk.Empty("r", "normalize", "ok", 1, at), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(shards) != 0 {
		t.Fatalf("an empty batch made %d shards", len(shards))
	}
}

func TestSplitRefusesAShardOfNothing(t *testing.T) {
	if _, err := agk.Split(batch(t, 2), 0); err == nil {
		t.Fatal("accepted a shard size of zero")
	}
}

func TestConcatRebuildsTheMetadata(t *testing.T) {
	early := at
	late := at.Add(2 * time.Second)

	run := agk.NewRunID()
	shard := func(attempt int, when time.Time, items ...agk.Item) agk.Envelope {
		return agk.Envelope{
			Meta:  agk.Meta{RunID: run, Step: "normalize", Port: "ok", Attempt: attempt, Count: len(items), ProducedAt: when},
			Items: items,
		}
	}

	out, err := agk.Concat([]agk.Envelope{
		shard(1, early, agk.NewItem(nil)),
		shard(2, late, agk.NewItem(nil), agk.NewItem(nil)),
	}, agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	if out.Meta.Count != 3 {
		t.Errorf("the published envelope says it holds %d items and holds 3", out.Meta.Count)
	}
	// A shard that was retried carries the higher attempt, and the batch is
	// attributed to the highest any shard needed. The port is published when the
	// step ends, which is when the last shard ended.
	if out.Meta.Attempt != 2 {
		t.Errorf("the published envelope is attributed to attempt %d", out.Meta.Attempt)
	}
	if !out.Meta.ProducedAt.Equal(late) {
		t.Errorf("the published envelope says it was produced at %s", out.Meta.ProducedAt)
	}
	if out.Meta.Step != "normalize" || out.Meta.Port != "ok" || out.Meta.RunID != run {
		t.Errorf("the published envelope lost where it came from: %+v", out.Meta)
	}
}

func TestConcatRefusesShardsThatAreNotOnePort(t *testing.T) {
	run := agk.NewRunID()
	shard := func(step agk.Step, port agk.Port) agk.Envelope {
		return agk.Envelope{
			Meta:  agk.Meta{RunID: run, Step: step, Port: port, Attempt: 1, Count: 0, ProducedAt: at},
			Items: []agk.Item{},
		}
	}

	cases := []struct {
		name   string
		shards []agk.Envelope
		names  string
	}{
		{"two ports", []agk.Envelope{shard("normalize", "ok"), shard("normalize", "rejected")}, "shards[1].meta.port"},
		{"two steps", []agk.Envelope{shard("normalize", "ok"), shard("invoice", "ok")}, "shards[1].meta.step"},
		{"nothing at all", nil, "at least one shard"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := agk.Concat(c.shards, agk.DefaultLimits())
			if err == nil {
				t.Fatal("accepted")
			}
			if !errors.Is(err, agk.ErrEnvelopeRejected) {
				t.Fatalf("refused without rejecting the envelope: %v", err)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Fatalf("the refusal does not say %q: %v", c.names, err)
			}
		})
	}
}

// TestConcatMeasuresTheSizeRulesOnTheResult: each shard is small enough and the batch
// they make is not, which is the case the rules exist for.
func TestConcatMeasuresTheSizeRulesOnTheResult(t *testing.T) {
	run := agk.NewRunID()
	shard := func() agk.Envelope {
		return agk.Envelope{
			Meta:  agk.Meta{RunID: run, Step: "normalize", Port: "ok", Attempt: 1, Count: 1, ProducedAt: at},
			Items: []agk.Item{agk.NewItem(nil)},
		}
	}

	_, err := agk.Concat([]agk.Envelope{shard(), shard(), shard()}, agk.Limits{MaxItems: 2})
	var r *agk.Refusal
	if !errors.As(err, &r) {
		t.Fatalf("three shards of one item made a batch of three under a limit of two: %v", err)
	}
	if r.Rule != agk.RuleMaxItems || r.Outcome != agk.Fail {
		t.Fatalf("refused by %s, %v", r.Rule, r.Outcome)
	}
}
