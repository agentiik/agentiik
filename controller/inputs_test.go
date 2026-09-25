package controller

import (
	"context"
	"io"
	"slices"
	"sync"
	"testing"

	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// writing is a store that says which keys were written to it.
type writing struct {
	artifact.Objects
	mu      sync.Mutex
	written []string
}

func (w *writing) Put(ctx context.Context, key string, r io.Reader) error {
	w.mu.Lock()
	w.written = append(w.written, key)
	w.mu.Unlock()
	return w.Objects.Put(ctx, key, r)
}

func (w *writing) wrote(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Contains(w.written, key)
}

// The input a task is handed is counted by its grant, so a sweep claiming it between the pass that
// found the store holding it and the grant counting it would delete bytes the task is about to be
// handed. The grant says so, and the dispatch writes them again.
func TestAnInputASweepHadClaimedIsWrittenAgainAtDispatch(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)
	ctx, cancel := context.WithCancel(t.Context())
	died := &diesOnPublishing{cancel: cancel}
	dead, err := NewCore(core.controller, core.term, Options{
		Queue: died, Versions: core.versions, Objects: core.objects, Now: core.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dead.Decide(ctx, decidedRun); err == nil {
		t.Fatal("a pass that died after publishing answered as if it had recorded the dispatch")
	}
	first := died.dispatched()
	if len(first) != 1 {
		t.Fatalf("the pass that died published %d tasks", len(first))
	}
	in, ok := first[0].Inputs["orders"]
	if !ok || in.Size <= 0 {
		t.Fatalf("the first dispatch was handed %+v", first[0].Inputs)
	}

	// Nothing counts it any more, long enough ago, and a sweep claims it.
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(),
		`update artifact_objects set refs = 0, collectable_at = now() - interval '2 days'`); err != nil {
		t.Fatal(err)
	}
	claimed, err := pool.Collectable(t.Context(), 0, 0)
	if err != nil || len(claimed) == 0 {
		t.Fatalf("the sweep claimed %v: %v", claimed, err)
	}

	store := &writing{Objects: core.objects}
	again, err := NewCore(core.controller, core.term, Options{
		Queue: q, Versions: core.versions, Objects: store, Now: core.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	if second := q.dispatched(); len(second) != 1 || second[0].Inputs["orders"].Digest != in.Digest {
		t.Fatalf("the next pass published %+v", second)
	}
	if !store.wrote(artifact.Key("finance", in.Digest)) {
		t.Error("the input a sweep had claimed was not written again at dispatch, and the sweep deletes it from under the task")
	}
}
