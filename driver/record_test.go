package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
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

// The record holds the key, how and when it ended and what it left by reference, and never
// a payload. It sits under the work root, outside the task's directory, because it has to
// outlive a directory that is "removed with the container, so no residue of one namespace
// survives into the next task on that host", and a record that kept the envelopes would be
// that residue.
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
	// Read as words rather than digits: the record now names the envelope by a digest,
	// and a digest may spell any four digits at all.
	for _, payload := range []string{"INV-2026-0917", "invoice", "amount"} {
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

// An entry that does not read is judged by the age of its file instead, so that it is
// forgotten in its turn rather than refusing its key for as long as the host runs. One
// written within the week still refuses: it may be the entry of a key that ran, cut short
// by the crash it was written for.
func TestAnEntryThatDoesNotReadIsForgottenByItsOwnAge(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	bricks := &counting{}
	r := newRunner(t, oneImage(ref, goodManifest), bricks.run(func(string) int { return 0 }))

	old, recent := stepTask(ref, "old"), stepTask(ref, "recent")
	paths := map[agk.Step]string{}
	for _, task := range []graph.Task{old, recent} {
		path, err := r.keys.path(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"idempotency_key":"01JMZ8V1P9C4/`), 0o600); err != nil {
			t.Fatal(err)
		}
		paths[task.Step] = path
	}
	long := time.Now().Add(-KeysKept - time.Hour)
	if err := os.Chtimes(paths["old"], long, long); err != nil {
		t.Fatal(err)
	}

	// The record is pruned when an ending is written, the first one a driver writes
	// included.
	if _, err := r.Run(t.Context(), stepTask(ref, "another")); err != nil {
		t.Fatalf("running another key: %s", err)
	}

	if _, err := os.Stat(paths["old"]); !os.IsNotExist(err) {
		t.Errorf("an entry that does not read and was last written %s ago is still there", KeysKept+time.Hour)
	}
	if _, err := r.Run(t.Context(), old); err != nil {
		t.Errorf("the key whose unreadable entry was forgotten is refused: %s", err)
	}
	if _, err := r.Run(t.Context(), recent); err == nil {
		t.Error("a key whose entry does not read and was written within the week was started")
	}
	if old, recent := bricks.times("old"), bricks.times("recent"); old != 1 || recent != 0 {
		t.Errorf("the bricks ran %d and %d times", old, recent)
	}
}

// storeGone is an object store the network has gone from, which is the outage that also
// silences a heartbeat.
type storeGone struct{}

func (storeGone) Has(context.Context, string) (bool, error) {
	return false, errors.New("the object store could not be reached")
}

func (storeGone) Put(context.Context, string, io.Reader) error {
	return errors.New("the object store could not be reached")
}

func (storeGone) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("the object store could not be reached")
}

// A container that ran to its end has ended its key, whatever then became of what it left.
// An output that is not an envelope, or a store that refused the upload, is still the error
// Run answers with, and the key is still written down: the brick ran, and a second delivery
// of the key would run it again.
func TestWhatFailsAfterTheExitStillEndsTheKey(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	for _, c := range []struct {
		name    string
		left    func(dockertest.Container) error
		store   artifact.Objects
		refused string
	}{
		{
			name: "an output that is not an envelope",
			left: func(ctr dockertest.Container) error {
				return os.WriteFile(filepath.Join(ctr.Work, "ports", "out.json"), []byte("{not an envelope"), 0o644)
			},
			refused: "the envelope is not a JSON document",
		},
		{
			name: "a store that refused the upload",
			left: func(ctr dockertest.Container) error {
				body := []byte("%PDF-1.7 a charge was made")
				if err := os.WriteFile(filepath.Join(ctr.Work, "files", "receipt.pdf"), body, 0o644); err != nil {
					return err
				}
				sum := sha256.Sum256(body)
				item := agk.NewItem(map[string]any{"charged": true})
				item.Files = []agk.File{{
					Name:      "receipt.pdf",
					URI:       agk.URI{Run: agk.RunID(ctr.Labels[LabelRun]), Step: agk.Step(ctr.Labels[LabelStep]), Port: "out", Name: "receipt.pdf"},
					MediaType: "application/pdf",
					Size:      int64(len(body)),
					SHA256:    hex.EncodeToString(sum[:]),
				}}
				return wrote(ctr, "out", item)
			},
			store:   storeGone{},
			refused: "the object store could not be reached",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			bricks := &counting{}
			r := newRunner(t, oneImage(ref, goodManifest), func(ctr dockertest.Container) (int, error) {
				bricks.run(func(string) int { return 0 })(ctr)
				return 0, c.left(ctr)
			})
			first := r
			if c.store != nil {
				s, err := artifact.New(c.store, "finance", agk.DefaultLimits())
				if err != nil {
					t.Fatal(err)
				}
				first = reopen(t, r, func(cfg *Config) {
					cfg.Store = func(string) (*artifact.Store, error) { return s, nil }
				})
			}
			task := oneTask(ref)

			_, err := first.Run(t.Context(), task)
			if err == nil || !strings.Contains(err.Error(), c.refused) {
				t.Fatalf("the delivery answered %v, and what the container left could not be collected", err)
			}
			e, found, err := first.keys.read(task.ID)
			if err != nil || !found {
				t.Fatalf("the key is not in the record after its container ran to its end: %v", err)
			}
			if e.State != agk.TaskFailed {
				t.Errorf("the key is recorded %s, and a delivery that answered an error is recorded failed", e.State)
			}
			if e.ExitCode != nil || e.Outputs != nil {
				t.Errorf("the key is recorded exiting %v with %v, and none of what the container left reached a Result", e.ExitCode, e.Outputs)
			}

			again := reopen(t, r, nil)
			if _, err := again.Run(t.Context(), task); !errors.Is(err, ErrCompleted) {
				t.Errorf("the next delivery answered %v, and a key whose container ran to its end is refused", err)
			}
			if n := bricks.times("fetch"); n != 1 {
				t.Errorf("the brick ran %d times", n)
			}
		})
	}
}

// An adopted container that has already exited has ended its key too, even when this
// delivery cannot collect it. The removal on the way out takes the container and its
// working directory whether or not the collection worked, so a key left unrecorded here
// is a key whose next delivery starts the brick from the beginning.
func TestAnExitedContainerThatCannotBeCollectedStillEndsItsKey(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	bricks := &counting{}
	r := newRunner(t, oneImage(ref, goodManifest), bricks.run(func(string) int { return 0 }))
	task := taskWithASecret(ref)
	exitedFirstDelivery(t, r, task)

	// The secret cannot be redeemed on the delivery that adopts the container, and a log
	// read back without the value to mask it with is a log that carries it in the clear.
	adopting := reopen(t, r, func(cfg *Config) { cfg.Secrets = secretSource{} })
	if _, err := adopting.Run(t.Context(), task); err == nil {
		t.Fatal("the container was collected with no value to mask its log with")
	}
	if e, found, err := adopting.keys.read(task.ID); err != nil || !found || e.State != agk.TaskFailed {
		t.Fatalf("the record reads %+v, %v, %v, and the adopted container had run to its end", e, found, err)
	}

	created := len(r.daemon.Created())
	if _, err := reopen(t, r, nil).Run(t.Context(), task); !errors.Is(err, ErrCompleted) {
		t.Errorf("the next delivery answered %v, and a key whose container ran to its end is refused", err)
	}
	if n := len(r.daemon.Created()); n != created {
		t.Errorf("the next delivery created %d containers", n-created)
	}
	if n := bricks.times("fetch"); n != 1 {
		t.Errorf("the brick ran %d times", n)
	}
}

// A requeue comes back to the host that ended its key when the heartbeat declared the
// task lost while the host was only cut off, and the run waits on the requeue's answer.
// The host answers it from the record rather than by running the brick again, so the
// record keeps what the ending left, by reference and as a result names it: each port's
// envelope by the digest it is uploaded under and its count, each artifact by digest and
// size, and the log by its address and length. It is read back after a restart, whole, on
// the refusal the next delivery of the key meets.
func TestAnEndingIsAnsweredFromTheRecordAfterARestart(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	receipt := []byte("%PDF-1.7 a charge was made")
	sum := sha256.Sum256(receipt)

	bricks := &counting{}
	r := newRunner(t, oneImage(ref, goodManifest), func(ctr dockertest.Container) (int, error) {
		bricks.run(func(string) int { return 0 })(ctr)
		fmt.Fprintln(ctr.Stderr, "charging")
		if err := os.WriteFile(filepath.Join(ctr.Work, "files", "receipt.pdf"), receipt, 0o644); err != nil {
			return 1, err
		}
		item := agk.NewItem(map[string]any{"charged": true})
		item.Files = []agk.File{{
			Name:      "receipt.pdf",
			URI:       agk.URI{Run: agk.RunID(ctr.Labels[LabelRun]), Step: agk.Step(ctr.Labels[LabelStep]), Port: "out", Name: "receipt.pdf"},
			MediaType: "application/pdf",
			Size:      int64(len(receipt)),
			SHA256:    hex.EncodeToString(sum[:]),
		}}
		return 0, wrote(ctr, "out", item)
	})
	logging := reopen(t, r, func(cfg *Config) { cfg.Logs = &sinkFor{b: &strings.Builder{}} })
	task := oneTask(ref)

	result, err := logging.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running: %s", err)
	}
	var told Event
	r.observed.mu.Lock()
	for _, e := range r.observed.es {
		if e.Task == task.ID && e.State.Terminal() {
			told = e
		}
	}
	r.observed.mu.Unlock()
	if told.Log.Lines == 0 {
		t.Fatalf("the observer was told of %d lines of log, so this is not the case under test", told.Log.Lines)
	}
	digest, _, err := artifact.EnvelopeDigest(result.Outputs["out"])
	if err != nil {
		t.Fatal(err)
	}
	log, err := agk.NewLogURI(task.ID)
	if err != nil {
		t.Fatal(err)
	}

	err = reopen(t, r, nil).Hold(task.ID)
	var done *Completed
	if !errors.As(err, &done) || !errors.Is(err, ErrCompleted) {
		t.Fatalf("holding a key that ended before the restart answered %v, and it is refused with its ending", err)
	}
	if charge, decided := Charged(err); !decided || charge != ChargePlatform {
		t.Errorf("the refusal is charged to %s, and a key refused is not the brick's failure", charge)
	}
	e := done.Ending
	if e.Key != task.ID || e.State != agk.TaskSucceeded || e.ExitCode == nil || *e.ExitCode != 0 {
		t.Errorf("the ending reads %s %s exiting %v, and %s succeeded with 0", e.Key, e.State, e.ExitCode, task.ID)
	}
	if !e.StartedAt.Equal(result.StartedAt) || !e.FinishedAt.Equal(result.FinishedAt) || e.StartedAt.IsZero() {
		t.Errorf("the ending ran from %s to %s, and the container from %s to %s", e.StartedAt, e.FinishedAt, result.StartedAt, result.FinishedAt)
	}
	if want := []EndedPort{{Port: "out", Digest: "sha256:" + digest, Items: 1}}; !slices.Equal(e.Outputs, want) {
		t.Errorf("the ending names the outputs %+v, want %+v", e.Outputs, want)
	}
	if want := []EndedArtifact{{SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(receipt))}}; !slices.Equal(e.Artifacts, want) {
		t.Errorf("the ending names the artifacts %+v, want %+v", e.Artifacts, want)
	}
	if want := (EndedLog{URI: log, Lines: told.Log.Lines, Truncated: told.Log.Truncated}); e.Log == nil || *e.Log != want {
		t.Errorf("the ending names the log %+v, want %+v", e.Log, want)
	}
	if n := bricks.times("fetch"); n != 1 {
		t.Errorf("the brick ran %d times", n)
	}
}

// A failure is answered as it ended: its exit code and its span, and no ports, since a
// failed shard's ports are what its step publishes for it. A runner that keeps no log has
// none to name.
func TestAFailureIsAnsweredFromTheRecordWithItsExitCode(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	bricks := &counting{}
	r := newRunner(t, oneImage(ref, goodManifest), bricks.run(func(string) int { return 3 }))
	task := oneTask(ref)
	if _, err := r.Run(t.Context(), task); err != nil {
		t.Fatalf("running: %s", err)
	}

	_, err := r.Run(t.Context(), task)
	var done *Completed
	if !errors.As(err, &done) {
		t.Fatalf("the next delivery answered %v, and a key that has completed is refused with its ending", err)
	}
	e := done.Ending
	switch {
	case e.State != agk.TaskFailed || e.ExitCode == nil || *e.ExitCode != 3:
		t.Errorf("the ending reads %s exiting %v, and the brick failed with 3", e.State, e.ExitCode)
	case e.StartedAt.IsZero() || e.FinishedAt.IsZero():
		t.Errorf("the ending ran from %s to %s, and a failure reports its span", e.StartedAt, e.FinishedAt)
	case e.Outputs != nil || e.Artifacts != nil || e.Log != nil:
		t.Errorf("the ending names %+v, %+v and %+v", e.Outputs, e.Artifacts, e.Log)
	}
	if n := bricks.times("fetch"); n != 1 {
		t.Errorf("the brick ran %d times", n)
	}
}
