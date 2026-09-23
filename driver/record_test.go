package driver

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// The record of the keys a host has taken and carried to an ending, which is what makes
// "the runner refuses to start a container for a key that has already completed" true
// once the container is gone.

// reopen is the same host after its runner process restarted: a second driver on the same
// daemon and the same work root, holding nothing in memory the first one held. change is
// applied to the configuration first, which is how a test gives the second one a clock.
func reopen(t *testing.T, r *runner, change func(*Config)) *runner {
	t.Helper()
	cfg := r.cfg
	if change != nil {
		change(&cfg)
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("opening the driver again: %s", err)
	}
	t.Cleanup(func() { d.Close() })
	return &runner{Docker: d, daemon: r.daemon, work: r.work, observed: r.observed}
}

// counting is a brick that counts its runs by step, and exits with what code answers.
type counting struct {
	mu  sync.Mutex
	ran map[string]int
}

func (c *counting) run(code func(step string) int) func(dockertest.Container) (int, error) {
	return func(ctr dockertest.Container) (int, error) {
		step := ctr.Labels[LabelStep]
		c.mu.Lock()
		if c.ran == nil {
			c.ran = map[string]int{}
		}
		c.ran[step]++
		c.mu.Unlock()
		if n := code(step); n != 0 {
			return n, nil
		}
		return 0, wrote(ctr, "out", agk.NewItem(map[string]any{"invoice": "INV-2026-0917", "amount": 1284}))
	}
}

func (c *counting) times(step string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ran[step]
}

// stepTask is oneTask under another step, which is another key.
func stepTask(ref string, step agk.Step) graph.Task {
	task := oneTask(ref)
	task.ID, task.Step = agk.NewTaskID("01JMZ8V1P9C4", step, 1, agk.Shard{}), step
	return task
}

// A restart forgets everything in memory, and the redelivery that follows one is the case
// the record is for. Every ending counts, a failure as much as a success: the key names
// one attempt, and a second container for it is that attempt run twice, where a retry
// would be another key.
func TestACompletedKeyIsNeverStartedAgain(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	bricks := &counting{}
	r := newRunner(t, oneImage(ref, goodManifest), bricks.run(func(step string) int {
		if step == "check" {
			return 3
		}
		return 0
	}))

	fetch, check := stepTask(ref, "fetch"), stepTask(ref, "check")
	for _, task := range []graph.Task{fetch, check} {
		if _, err := r.Run(t.Context(), task); err != nil {
			t.Fatalf("the first delivery of %s: %s", task.ID, err)
		}
	}
	created := len(r.daemon.Created())

	again := reopen(t, r, nil)
	for _, c := range []struct {
		task  graph.Task
		ended string
	}{{fetch, "ended succeeded"}, {check, "ended failed"}} {
		_, err := again.Run(t.Context(), c.task)
		if !errors.Is(err, ErrCompleted) {
			t.Errorf("%s was delivered again after a restart and answered %v, and a key that has completed is refused", c.task.ID, err)
			continue
		}
		if !strings.Contains(err.Error(), c.ended) {
			t.Errorf("the refusal of %s reads %q", c.task.ID, err)
		}
	}
	if fetch, check := bricks.times("fetch"), bricks.times("check"); fetch != 1 || check != 1 {
		t.Errorf("the bricks ran %d and %d times, and each key was delivered twice and run once", fetch, check)
	}
	if n := len(r.daemon.Created()); n != created {
		t.Errorf("the restarted driver created %d containers for keys that had completed", n-created)
	}
}

// A runner writes the key down on take, before it acknowledges the message, and the key
// it holds still runs. A key that has completed is refused there already, before anything
// is redeemed or pulled.
func TestAKeyIsWrittenDownWhenItIsHeld(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	bricks := &counting{}
	r := newRunner(t, oneImage(ref, goodManifest), bricks.run(func(string) int { return 0 }))
	task := oneTask(ref)

	if err := r.Hold(task.ID); err != nil {
		t.Fatalf("holding a key nobody has run: %s", err)
	}
	e, found, err := r.keys.read(task.ID)
	if err != nil || !found {
		t.Fatalf("the held key is not in the record: %v", err)
	}
	if e.State != agk.TaskDispatched {
		t.Errorf("a held key is recorded %s, and it is handed out and not yet anything more", e.State)
	}
	if len(r.daemon.Created()) != 0 {
		t.Errorf("holding a key created %d containers", len(r.daemon.Created()))
	}

	if _, err := r.Run(t.Context(), task); err != nil {
		t.Fatalf("running a held key: %s", err)
	}
	if n := bricks.times("fetch"); n != 1 {
		t.Errorf("the brick ran %d times", n)
	}

	err = r.Hold(task.ID)
	if !errors.Is(err, ErrCompleted) {
		t.Fatalf("holding a key that has completed answered %v", err)
	}
	if e, _, _ := r.keys.read(task.ID); e.State != agk.TaskSucceeded {
		t.Errorf("the refused hold left the key recorded %s, and it ended succeeded", e.State)
	}
}

// The ending is written after everything is collected and before anything is removed.
// After the collection, because a key recorded before its outputs were read would be
// refused on the delivery that could still have produced them. Before the removal,
// because a runner dying between the two would otherwise leave a key whose container is
// gone and whose record says nothing, which is the whole of what the record is for.
func TestAnEndingIsWrittenBetweenTheCollectionAndTheRemoval(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	task := oneTask(ref)

	var r *runner
	var mu sync.Mutex
	var early []string
	recorded := func() bool {
		_, found, _ := r.keys.read(task.ID)
		return found
	}
	r = newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		if recorded() {
			mu.Lock()
			early = append(early, "while the container ran")
			mu.Unlock()
		}
		return 0, wrote(c, "out", agk.NewItem(map[string]any{"n": 1}))
	})
	r.cfg.Observer = observerFunc(func(_ context.Context, e Event) {
		if recorded() {
			mu.Lock()
			early = append(early, "when the observer was told "+e.State.String())
			mu.Unlock()
		}
	})

	// The identifier is read off the path, because a replaced route is called directly
	// rather than through the mux that would have filled in its wildcard.
	atRemoval := false
	r.daemon.Handle("DELETE", "/containers/{id}", func(w http.ResponseWriter, req *http.Request) {
		id := strings.TrimPrefix(req.URL.Path, "/containers/")
		for _, c := range r.daemon.Created() {
			if c.ID == id && c.Labels[LabelTask] == string(task.ID) {
				mu.Lock()
				atRemoval = recorded()
				mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})

	if _, err := r.Run(t.Context(), task); err != nil {
		t.Fatalf("running: %s", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, when := range early {
		t.Errorf("the key was recorded as ended %s, before its outputs were collected", when)
	}
	if !atRemoval {
		t.Error("the container was removed before its ending was written down")
	}
}

// observerFunc is an Observer made of a function.
type observerFunc func(context.Context, Event)

func (f observerFunc) Observe(ctx context.Context, e Event) { f(ctx, e) }

// A delivery the driver refused before any container ran is no ending. Nothing ran, so
// there is nothing a second delivery would run twice, and a key recorded here would be a
// task refused on every delivery for a failure that was never its own.
func TestAKeyNothingRanForIsNotRecorded(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	bricks := &counting{}
	r := newRunner(t, oneImage(ref, goodManifest), bricks.run(func(string) int { return 0 }))

	task := oneTask(ref)
	task.Network = graph.NetworkEgress
	task.EgressAllow = []string{"api.example.test:443"}
	if _, err := r.Run(t.Context(), task); err == nil {
		t.Fatal("a step asking for egress was started")
	}
	if _, found, _ := r.keys.read(task.ID); found {
		t.Error("a key the driver refused before creating anything was recorded")
	}

	task.Network, task.EgressAllow = graph.NetworkNone, nil
	if _, err := r.Run(t.Context(), task); err != nil {
		t.Fatalf("the next delivery of a key nothing ran for: %s", err)
	}
	if n := bricks.times("fetch"); n != 1 {
		t.Errorf("the brick ran %d times", n)
	}
}

// The record holds the key, the state and the moment, and never a payload. It sits under
// the work root, outside the task's directory, because it has to outlive a directory that
// is "removed with the container, so no residue of one namespace survives into the next
// task on that host", and a record that kept the envelopes would be that residue.
func TestTheRecordKeepsNoPayload(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	bricks := &counting{}
	r := newRunner(t, oneImage(ref, goodManifest), bricks.run(func(string) int { return 0 }))
	task := oneTask(ref)
	result, err := r.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running: %s", err)
	}
	if out := result.Outputs["out"]; len(out.Items) != 1 {
		t.Fatalf("the brick published %+v", out)
	}

	path := filepath.Join(r.work, KeysDir, "01JMZ8V1P9C4", "fetch", "1.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the ending is not where the record keeps it: %s", err)
	}
	for _, payload := range []string{"INV-2026-0917", "1284", "invoice"} {
		if strings.Contains(string(b), payload) {
			t.Errorf("the record carries %q out of the envelope: %s", payload, b)
		}
	}
	for _, want := range []string{`"idempotency_key":"01JMZ8V1P9C4/fetch/1"`, `"state":"succeeded"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the record reads %s, and it names the key and how it ended", b)
		}
	}
	if _, err := os.Stat(filepath.Join(r.work, "01JMZ8V1P9C4", "fetch", "1")); !os.IsNotExist(err) {
		t.Errorf("the task's working directory survived, and the record is what outlives it")
	}
}

// A key is remembered for as long as the task stream keeps a message and then forgotten,
// since no delivery of it can arrive later than that and a host runs tasks for years.
// Forgetting it takes the directories it leaves empty with it.
func TestAKeyIsForgottenOnceTheStreamWouldHaveDiscardedIt(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	var mu sync.Mutex
	now := time.Now().UTC()
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	bricks := &counting{}
	r := reopen(t, newRunner(t, oneImage(ref, goodManifest), bricks.run(func(string) int { return 0 })),
		func(c *Config) { c.Now = clock })

	old, recent := stepTask(ref, "old"), stepTask(ref, "recent")
	if _, err := r.Run(t.Context(), old); err != nil {
		t.Fatalf("running the old key: %s", err)
	}

	mu.Lock()
	now = now.Add(KeysKept + time.Hour)
	mu.Unlock()
	if _, err := r.Run(t.Context(), recent); err != nil {
		t.Fatalf("running the recent key: %s", err)
	}

	if _, found, _ := r.keys.read(old.ID); found {
		t.Errorf("%s is still recorded %s after it was last written", old.ID, KeysKept+time.Hour)
	}
	if _, err := os.Stat(filepath.Join(r.work, KeysDir, "01JMZ8V1P9C4", "old")); !os.IsNotExist(err) {
		t.Error("the directory the forgotten key left empty is still there")
	}
	if _, err := r.Run(t.Context(), recent); !errors.Is(err, ErrCompleted) {
		t.Errorf("the recent key was forgotten with the old one: %v", err)
	}
	if _, err := r.Run(t.Context(), old); err != nil {
		t.Errorf("a key forgotten after its retention is refused: %s", err)
	}
	if n := bricks.times("old"); n != 2 {
		t.Errorf("the old key's brick ran %d times", n)
	}
}

// An entry that does not read is not taken for a key that never ran. Starting it would be
// the second run the record exists to prevent, and a record that answered nothing for a
// file it could not read would be a record that quietly stopped refusing.
func TestAnEntryThatDoesNotReadRefusesItsKey(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	bricks := &counting{}
	r := newRunner(t, oneImage(ref, goodManifest), bricks.run(func(string) int { return 0 }))
	task := oneTask(ref)

	path, err := r.keys.path(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"idempotency_key":"01JMZ8V1P9C4/fe`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Run(t.Context(), task); err == nil {
		t.Fatal("a key whose entry does not read was started")
	}
	if err := r.Hold(task.ID); err == nil {
		t.Error("a key whose entry does not read was held")
	}
	if n := bricks.times("fetch"); n != 0 {
		t.Errorf("the brick ran %d times", n)
	}
}
