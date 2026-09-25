package bus

import (
	"maps"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/ulid"
)

// A queue's depth is what it holds that no runner has acknowledged: a message taken and still being
// redeemed counts, one acknowledged does not, and a pool nobody published to is not named.
func TestTheDepthOfAQueueIsWhatNoRunnerHasAcknowledged(t *testing.T) {
	b := open(t)
	base := step(t)
	for i, pool := range []string{"dmz", "dmz", "dmz", DefaultPool} {
		s := agk.Step(string(base) + string(rune('a'+i)))
		if err := b.Publish(t.Context(), pool, messageAs(ulid.New(), s)); err != nil {
			t.Fatal(err)
		}
	}
	depths := func() map[string]int {
		t.Helper()
		d, err := b.Depths(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if got, want := depths(), map[string]int{"dmz": 3, DefaultPool: 1}; !maps.Equal(got, want) {
		t.Fatalf("the queues are %v deep, want %v", got, want)
	}

	taken, err := b.Take(t.Context(), "dmz", 1, 5*time.Second)
	if err != nil || len(taken) != 1 {
		t.Fatalf("taking: %v, %d", err, len(taken))
	}
	if got := depths()["dmz"]; got != 3 {
		t.Errorf("with one task taken and not acknowledged the dmz queue is %d deep, want 3", got)
	}
	if err := taken[0].Held(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, want := depths(), map[string]int{"dmz": 2, DefaultPool: 1}; !maps.Equal(got, want) {
		t.Errorf("with one task acknowledged the queues are %v deep, want %v", got, want)
	}
}
