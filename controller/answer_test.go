package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// A result delivered twice is written once. The bus delivers at least once, so the second
// delivery is the ordinary case rather than a fault, and the test of it is the sequence: a
// sequence that moved is a decision written down and a run decided again.

// An attempt a retry replaced is over, though its shard is pending again. The failure that was
// granted the retry, delivered a second time, is a duplicate rather than a second failure: it
// neither spends another attempt nor sends one out early.
func TestAResultForAnAttemptAlreadyRetriedChangesNothing(t *testing.T) {
	core, q, pool, super := decidingOn(t, retryingWorkflow)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.taken()
	if len(first) != 1 || first[0].Attempt != 1 {
		t.Fatalf("the first pass published %+v", first)
	}
	core.answer(t, failed(first[0], 1, core.now()))
	if got := q.taken(); len(got) != 0 {
		t.Fatalf("the retry was published before its backoff had passed: %+v", got)
	}

	conn := dbtest.Superuser(t, super)
	before := seqOf(t, conn)
	core.answer(t, failed(first[0], 1, core.now()))
	if after := seqOf(t, conn); after != before {
		t.Errorf("the failure of attempt 1, delivered again, took the run from seq %d to %d", before, after)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("the failure of attempt 1, delivered again, published %+v", got)
	}

	// And the retry is still the one the first delivery was granted: attempt 2, once the
	// backoff has passed, and not attempt 3.
	clock.advance(time.Minute)
	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	if second := q.taken(); len(second) != 1 || second[0].Attempt != 2 {
		t.Errorf("after the backoff the sweep published %+v, want attempt 2 alone", second)
	}
}

// dying stands for a controller that writes a result down and dies before deciding the run
// again. Answer resolves the graph once to record the result, and Decide resolves it again;
// the second resolution is where this one stops.
type dying struct {
	Versions

	mu    sync.Mutex
	calls int
}

func (d *dying) Graph(ctx context.Context, namespace, workflow, commit string) (*graph.Graph, error) {
	d.mu.Lock()
	d.calls++
	calls := d.calls
	d.mu.Unlock()
	if calls > 1 {
		return nil, errors.New("the controller died before deciding the run again")
	}
	return d.Versions.Graph(ctx, namespace, workflow, commit)
}

// A result whose decision committed and whose run was never decided again comes back, because
// the bus heard an error rather than an acknowledgement. The second delivery is a duplicate and
// writes nothing, so what the first one made runnable is the sweep's to publish: the decision
// Answer writes leaves no wake time, and a run with none is one the sweep reaches.
func TestARedeliveryAfterTheDecisionCommittedIsANoOp(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	taken := q.taken()
	if len(taken) != 1 || taken[0].Step != "normalize" {
		t.Fatalf("the first pass published %+v", taken)
	}

	conn := dbtest.Superuser(t, super)
	before := seqOf(t, conn)
	dies, err := NewCore(core.controller, core.term, Options{
		Queue: q, Versions: &dying{Versions: core.versions}, Objects: core.objects, Now: core.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := succeeded(t, taken[0], core.now())
	if err := dies.Answer(t.Context(), Answer{Result: result, Runner: "runner-dmz-02"}); err == nil {
		t.Fatal("a controller that died before deciding the run again answered as if it had")
	}
	written := seqOf(t, conn)
	if written == before {
		t.Fatal("the result was not written before the controller died, so this is not the case under test")
	}
	if got := q.taken(); len(got) != 0 {
		t.Fatalf("a controller that died before deciding the run again published %+v", got)
	}

	// The redelivery, to a controller that is alive.
	if err := core.Answer(t.Context(), Answer{Result: result, Runner: "runner-dmz-02"}); err != nil {
		t.Fatalf("a result redelivered after its decision committed was refused: %s", err)
	}
	if after := seqOf(t, conn); after != written {
		t.Errorf("the redelivery took the run from seq %d to %d, and it is a duplicate rather than news", written, after)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("the redelivery published %+v, and a duplicate decides nothing", got)
	}

	if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
		t.Fatal(err)
	}
	if got := q.taken(); len(got) != 1 || got[0].Step != "archive" {
		t.Errorf("the sweep published %+v, want archive, which the result made runnable", got)
	}
}
