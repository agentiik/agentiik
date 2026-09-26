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

// A runner writes the key down on take, before it redeems the grant and acknowledges the
// message, and the key it holds still runs. A key that has completed is refused there
// already, before anything is redeemed or pulled.
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

// A key this host still has in flight is refused where it is written down again, before anything
// is redeemed for it, whether a Run is carrying it or another delivery holds it. That is the
// requeue of a task the heartbeat declared lost while its host was only cut off, reaching the host
// whose container still runs it: redeemed, it would be bound to this runner and answered by
// nobody, the container's one ending going to the dispatch that was lost. So the refusal leaves the
// key as it was, held for the delivery that has it, and once that delivery has ended the key the
// next one is answered with the ending, which the runner reports under the requeue's task_id.
func TestAKeyInFlightOnThisHostIsRefusedWhereItIsWrittenDown(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	running := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	bricks := &counting{}
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		once.Do(func() { close(running) })
		<-release
		return bricks.run(func(string) int { return 0 })(c)
	})
	task := oneTask(ref)

	if err := r.Hold(task.ID); err != nil {
		t.Fatal(err)
	}
	err := r.Hold(task.ID)
	if !errors.Is(err, ErrTaskInFlight) {
		t.Fatalf("a second delivery of a key another delivery holds answered %v, and it is refused before anything is redeemed", err)
	}
	if charge, decided := Charged(err); !decided || charge != ChargePlatform {
		t.Errorf("the refusal is charged to %s, and a key delivered twice is not the brick's failure", charge)
	}

	first := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), task)
		first <- err
	}()
	select {
	case <-running:
	case <-time.After(10 * time.Second):
		t.Fatal("the held key's container never ran")
	}
	if err := r.Hold(task.ID); !errors.Is(err, ErrTaskInFlight) {
		t.Fatalf("a delivery of a key whose container is running here answered %v, and it is refused before anything is redeemed", err)
	}
	if r.lookup(task.ID) == nil {
		t.Error("the refusals let go of the key the running delivery holds, which is what a stop reaches the task through")
	}
	if e, _, _ := r.keys.read(task.ID); e.State != agk.TaskDispatched {
		t.Errorf("the refusals left the key recorded %s", e.State)
	}

	close(release)
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("the running delivery: %s", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the running delivery never came back")
	}
	err = r.Hold(task.ID)
	var done *Completed
	if !errors.As(err, &done) || done.Ending.State != agk.TaskSucceeded {
		t.Fatalf("a delivery of the key once it had ended answered %v, and it is answered with the ending", err)
	}
	if n := bricks.times("fetch"); n != 1 {
		t.Errorf("the brick ran %d times for one key delivered three times", n)
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
// of the key would run it again. Where the brick broke the output contract it is written
// down as a container that ran, with 121 and a span, and charged to the brick; where the
// store would not take what the brick left it is the platform's, and no code is invented
// for it.
func TestWhatFailsAfterTheExitStillEndsTheKey(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	for _, c := range []struct {
		name    string
		left    func(dockertest.Container) error
		store   artifact.Objects
		refused string
		code    *int
		charge  Charge
	}{
		{
			name: "an output that is not an envelope",
			left: func(ctr dockertest.Container) error {
				return os.WriteFile(filepath.Join(ctr.Work, "ports", "out.json"), []byte("{not an envelope"), 0o644)
			},
			refused: "the envelope is not a JSON document",
			code:    new(ExitContractBroken),
			charge:  ChargeBrick,
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
			charge:  ChargePlatform,
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

			_, runErr := first.Run(t.Context(), task)
			if runErr == nil || !strings.Contains(runErr.Error(), c.refused) {
				t.Fatalf("the delivery answered %v, and what the container left could not be collected", runErr)
			}
			if charge, decided := Charged(runErr); !decided || charge != c.charge {
				t.Errorf("the delivery's error is charged to %s, want %s", charge, c.charge)
			}
			e, found, err := first.keys.read(task.ID)
			if err != nil || !found {
				t.Fatalf("the key is not in the record after its container ran to its end: %v", err)
			}
			if e.State != agk.TaskFailed {
				t.Errorf("the key is recorded %s, and a delivery that answered an error is recorded failed", e.State)
			}
			switch {
			case c.code == nil && e.ExitCode != nil:
				t.Errorf("the key is recorded exiting %d, and no code is invented for outputs the store would not take", *e.ExitCode)
			case c.code != nil && (e.ExitCode == nil || *e.ExitCode != *c.code):
				t.Errorf("the key is recorded exiting %v, want %d: a failed result that ran carries its code", e.ExitCode, *c.code)
			case c.code != nil && (e.StartedAt.IsZero() || e.FinishedAt.IsZero()):
				t.Errorf("the key is recorded with the span %s to %s, and a container that ran has one", e.StartedAt, e.FinishedAt)
			}
			if e.Outputs != nil {
				t.Errorf("the key is recorded with %v, and none of what the container left reached a Result", e.Outputs)
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
// envelope by the digest the store holds it under and its count, each artifact by digest
// and size, and the log by its address and length. It is read back after a restart, whole,
// on the refusal the next delivery of the key meets, and whether or not anybody was
// listening to the driver when the task ended.
//
// A digest in the record is one the store already holds, because the answer is read back
// by it: one recorded ahead of the write would be a result the controller could never read,
// redelivered for ever, and a key this host would refuse for a week.
func TestAnEndingIsAnsweredFromTheRecordAfterARestart(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	receipt := []byte("%PDF-1.7 a charge was made")
	sum := sha256.Sum256(receipt)

	for _, c := range []struct {
		name     string
		observed bool
	}{
		{"told to an observer", true},
		{"with nobody listening", false},
	} {
		t.Run(c.name, func(t *testing.T) {
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
			objects := artifact.Dir(t.TempDir())
			store, err := artifact.New(objects, "finance", agk.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			sink := &strings.Builder{}
			observed := &recorder{}
			logging := reopen(t, r, func(cfg *Config) {
				cfg.Logs = &sinkFor{b: sink}
				cfg.Store = func(string) (*artifact.Store, error) { return store, nil }
				cfg.Observer = nil
				if c.observed {
					cfg.Observer = observed
				}
			})
			task := oneTask(ref)

			result, err := logging.Run(t.Context(), task)
			if err != nil {
				t.Fatalf("running: %s", err)
			}
			lines := strings.Count(sink.String(), "\n")
			if lines == 0 {
				t.Fatalf("the sink holds %d lines of log, so this is not the case under test", lines)
			}
			if c.observed {
				var told Event
				observed.mu.Lock()
				for _, e := range observed.es {
					if e.Task == task.ID && e.State.Terminal() {
						told = e
					}
				}
				observed.mu.Unlock()
				if told.Log.Lines != lines || len(told.Outputs) != 1 {
					t.Errorf("the observer was told of %d lines and the ports %+v, and the sink holds %d lines of one port", told.Log.Lines, told.Outputs, lines)
				}
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
			if len(e.Outputs) != 1 || e.Outputs[0].Port != "out" || e.Outputs[0].Items != 1 {
				t.Fatalf("the ending names the outputs %+v, and the brick published one item on out", e.Outputs)
			}
			digest, found := strings.CutPrefix(e.Outputs[0].Digest, "sha256:")
			if !found {
				t.Fatalf("the ending names out as %q, which a result does not write", e.Outputs[0].Digest)
			}
			back, err := artifact.GetEnvelope(t.Context(), objects, "finance", digest, agk.DefaultLimits())
			if err != nil {
				t.Fatalf("the ending names out as %s, and the store does not hold it: %s", digest, err)
			}
			published := result.Outputs["out"]
			if back.Meta.Port != "out" || len(back.Items) != 1 || back.Items[0].ID != published.Items[0].ID {
				t.Errorf("the store holds under %s port %s with %d items, and the brick published %s on out", digest, back.Meta.Port, len(back.Items), published.Items[0].ID)
			}
			if want := []EndedArtifact{{SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(receipt))}}; !slices.Equal(e.Artifacts, want) {
				t.Errorf("the ending names the artifacts %+v, want %+v", e.Artifacts, want)
			}
			if want := (EndedLog{URI: log, Lines: lines}); e.Log == nil || *e.Log != want {
				t.Errorf("the ending names the log %+v, want %+v", e.Log, want)
			}
			if n := bricks.times("fetch"); n != 1 {
				t.Errorf("the brick ran %d times", n)
			}
		})
	}
}

// A success names every port it published, and one that published none says so with an
// empty list rather than with nothing: a result that says nothing of its ports is not a
// success the controller would take, so a requeue answered with it would be refused.
func TestASuccessThatPublishedNoPortIsAnsweredWithNone(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	r := newRunner(t, oneImage(ref, goodManifest), func(dockertest.Container) (int, error) { return 0, nil })
	task := oneTask(ref)
	task.Outputs = nil
	if _, err := r.Run(t.Context(), task); err != nil {
		t.Fatalf("running: %s", err)
	}

	err := reopen(t, r, nil).Hold(task.ID)
	var done *Completed
	if !errors.As(err, &done) {
		t.Fatalf("holding a key that ended before the restart answered %v, and it is refused with its ending", err)
	}
	if e := done.Ending; e.State != agk.TaskSucceeded || e.Outputs == nil || len(e.Outputs) != 0 {
		t.Errorf("the ending reads %s naming the ports %#v, and a success that published none names an empty list", e.State, e.Outputs)
	}
}

// An envelope the store would not take is a success nobody can report: its result would
// name a digest the store never received. It is an error after the exit like any other,
// charged to the platform since the brick did what it was asked, and the key is written
// down failed and naming nothing, which is what a requeue that comes back is answered with.
func TestAnEnvelopeTheStoreRefusedEndsTheKeyNamingNothing(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	bricks := &counting{}
	r := newRunner(t, oneImage(ref, goodManifest), func(ctr dockertest.Container) (int, error) {
		bricks.run(func(string) int { return 0 })(ctr)
		return 0, wrote(ctr, "out", agk.NewItem(map[string]any{"charged": true}))
	})
	s, err := artifact.New(storeGone{}, "finance", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	cut := reopen(t, r, func(cfg *Config) {
		cfg.Store = func(string) (*artifact.Store, error) { return s, nil }
	})
	task := oneTask(ref)

	_, err = cut.Run(t.Context(), task)
	if err == nil || !strings.Contains(err.Error(), "the object store could not be reached") {
		t.Fatalf("the delivery answered %v, and the envelope of out could not be written", err)
	}
	if charge, decided := Charged(err); !decided || charge != ChargePlatform {
		t.Errorf("the envelope the store refused is charged to %s, and the brick did what it was asked", charge)
	}

	err = reopen(t, r, nil).Hold(task.ID)
	var done *Completed
	if !errors.As(err, &done) {
		t.Fatalf("holding a key whose container ran to its end answered %v, and it is refused with its ending", err)
	}
	if e := done.Ending; e.State != agk.TaskFailed || e.ExitCode != nil || e.Outputs != nil {
		t.Errorf("the ending reads %s exiting %v naming %+v, and a port the store never received is named nowhere", e.State, e.ExitCode, e.Outputs)
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

// A runner reads a refusal through errors.Is and errors.As, and a runner's own tests fake
// the driver by writing the refusal as a literal. That literal is the refusal it holds, whole:
// it reads as ErrCompleted, is charged to the platform and names the key, rather than
// dereferencing something only this package could have filled in.
func TestACompletedWrittenAsALiteralIsTheRefusalItHolds(t *testing.T) {
	task := oneTask("ghcr.io/agentiik/http-request@" + imageDigest)
	at := time.Date(2026, 9, 23, 6, 0, 0, 0, time.UTC)
	var err error = &Completed{Ending: Ending{Key: task.ID, State: agk.TaskSucceeded, At: at}}

	if !errors.Is(err, ErrCompleted) {
		t.Errorf("%v does not read as ErrCompleted", err)
	}
	if charge, decided := Charged(err); !decided || charge != ChargePlatform {
		t.Errorf("the refusal is charged to %s, and a key refused is not the brick's failure", charge)
	}
	var done *Completed
	if !errors.As(err, &done) || done.Ending.Key != task.ID {
		t.Errorf("%v does not hand over the ending of %s", err, task.ID)
	}
	for _, want := range []string{"step fetch", string(task.ID), "succeeded", "2026-09-23T06:00:00Z", ErrCompleted.Error()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal reads %q, and it names %q", err, want)
		}
	}
}

// A restarted runner names in its heartbeat what an earlier process held when it stopped, and
// Dispatched is where it reads that from: every key taken and never ended, the most recently
// taken first. A key that ended is the record's answer to its requeue and held by nothing, and a
// key let go of was never this host's to answer for, so neither is listed.
func TestDispatchedListsTheKeysTakenAndNeverEndedNewestFirst(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	bricks := &counting{}
	r := newRunner(t, oneImage(ref, goodManifest), bricks.run(func(string) int { return 0 }))
	var clock sync.Mutex
	at := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	r = reopen(t, r, func(c *Config) {
		c.Now = func() time.Time {
			clock.Lock()
			defer clock.Unlock()
			at = at.Add(time.Second)
			return at
		}
	})

	older, newer := stepTask(ref, "older"), stepTask(ref, "newer")
	ended, letGo := stepTask(ref, "ended"), stepTask(ref, "let-go")
	for _, task := range []graph.Task{older, newer, ended, letGo} {
		if err := r.Hold(task.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Run(t.Context(), ended); err != nil {
		t.Fatal(err)
	}
	r.Release(letGo.ID)
	if taken, err := r.keys.takenPath(ended.ID); err != nil {
		t.Fatal(err)
	} else if _, err := os.Stat(taken); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the key's ending was written and it is still written down as taken: %v", err)
	}

	// What the next process on this host reads.
	got, err := reopen(t, r, nil).Dispatched()
	if err != nil {
		t.Fatal(err)
	}
	if want := []agk.TaskID{newer.ID, older.ID}; !slices.Equal(got, want) {
		t.Errorf("the record lists %v as taken and never ended, want %v", got, want)
	}
}

// A key let go of is forgotten on disk as well as in memory, and an ending is not: the ending is
// what refuses the key's next delivery.
func TestReleaseForgetsATakenKeyAndNeverAnEnding(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	bricks := &counting{}
	r := newRunner(t, oneImage(ref, goodManifest), bricks.run(func(string) int { return 0 }))
	task := oneTask(ref)
	if err := r.Hold(task.ID); err != nil {
		t.Fatal(err)
	}
	path, err := r.keys.takenPath(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Hold wrote nothing down: %s", err)
	}
	r.Release(task.ID)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a key let go of is still written down as taken: %v", err)
	}

	if _, err := r.Run(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	r.Release(task.ID)
	var completed *Completed
	if err := r.Hold(task.ID); !errors.As(err, &completed) {
		t.Errorf("a key that ended and was then released is held again: %v", err)
	}
}

// An entry that does not read, sits where another key's entry would or stands beside the key's
// ending lists nothing, rather than refusing the rest, and a work root with no record yet lists
// nothing and no error.
func TestDispatchedPassesOverWhatDoesNotRead(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	bricks := &counting{}
	r := newRunner(t, oneImage(ref, goodManifest), bricks.run(func(string) int { return 0 }))
	if got, err := r.Dispatched(); err != nil || len(got) != 0 {
		t.Fatalf("a host that took nothing lists %v, %v", got, err)
	}

	task := oneTask(ref)
	if err := r.Hold(task.ID); err != nil {
		t.Fatal(err)
	}
	path, err := r.keys.takenPath(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if err := os.WriteFile(filepath.Join(dir, "broken"+takenExt), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "elsewhere"+takenExt), b, 0o600); err != nil {
		t.Fatal(err)
	}

	// A key that ended, whose writer stopped before taking its taken entry away.
	ended := stepTask(ref, "ended")
	if _, err := r.Run(t.Context(), ended); err != nil {
		t.Fatal(err)
	}
	stale, err := r.keys.takenPath(ended.ID)
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.Replace(string(b), string(task.ID), string(ended.ID), 1))
	if err := os.WriteFile(stale, b, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := r.Dispatched()
	if err != nil {
		t.Fatal(err)
	}
	if want := []agk.TaskID{task.ID}; !slices.Equal(got, want) {
		t.Errorf("the record lists %v, want %v alone", got, want)
	}
}

// A Run that returns with no ending written forgets the key it carried, since what it created is
// gone and a restarted runner naming the key would keep its dispatch from being declared lost for
// nothing: a pull the daemon refused, and a Run whose caller gave up while the brick ran. A second
// delivery refused while the first runs forgets nothing of the first's.
func TestARunThatWritesNoEndingForgetsTheKeyItTook(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	running := make(chan struct{})
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		close(running)
		<-c.Signalled()
		return 0, nil
	})

	unpulled := stepTask("ghcr.io/agentiik/nowhere@"+imageDigest, "unpulled")
	if err := r.Hold(unpulled.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(t.Context(), unpulled); err == nil {
		t.Fatal("a task whose image the daemon does not have ran")
	}

	stopped := stepTask(ref, "stopped")
	if err := r.Hold(stopped.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	ran := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, stopped)
		ran <- err
	}()
	<-running
	if _, err := r.Run(t.Context(), stopped); !errors.Is(err, ErrTaskInFlight) {
		t.Errorf("a second delivery of a key in flight answered %v", err)
	}
	if got, err := r.Dispatched(); err != nil || !slices.Equal(got, []agk.TaskID{stopped.ID}) {
		t.Errorf("while the brick runs the record lists %v, %v, want %s", got, err, stopped.ID)
	}
	cancel()
	select {
	case err := <-ran:
		if e, found, _ := r.keys.read(stopped.ID); found && e.State.Terminal() {
			t.Fatalf("the Run whose caller gave up wrote the ending %s, answering %v, and this test is of one that writes none", e.State, err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the Run whose caller gave up never came back")
	}

	if got, err := reopen(t, r, nil).Dispatched(); err != nil || len(got) != 0 {
		t.Errorf("after two Runs that wrote no ending the record lists %v, %v, want nothing", got, err)
	}
}

// Ended answers the ending the record holds of a key, and nothing for a key never taken or taken
// and not ended, which it leaves as it found it: a key Ended was asked about is not taken by the
// asking.
func TestEndedAnswersTheRecordedEndingAndTakesNothing(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	bricks := &counting{}
	r := newRunner(t, oneImage(ref, goodManifest), bricks.run(func(string) int { return 0 }))
	never, taken, ended := stepTask(ref, "never"), stepTask(ref, "taken"), stepTask(ref, "ended")
	if err := r.Hold(taken.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(t.Context(), ended); err != nil {
		t.Fatal(err)
	}
	for _, task := range []graph.Task{never, taken} {
		if e, found, err := r.Ended(task.ID); found || err != nil {
			t.Errorf("%s: the record answered %+v, %t, %v", task.ID, e, found, err)
		}
	}
	e, found, err := r.Ended(ended.ID)
	if err != nil || !found || e.Key != ended.ID || e.State != agk.TaskSucceeded {
		t.Errorf("the ending recorded was answered as %+v, %t, %v", e, found, err)
	}
	if got, err := r.Dispatched(); err != nil || len(got) != 1 || got[0] != taken.ID {
		t.Errorf("asking the record left %v taken, %v, want %s alone", got, err, taken.ID)
	}
	if err := r.Hold(never.ID); err != nil {
		t.Errorf("a key Ended was asked about is refused: %s", err)
	}
}
