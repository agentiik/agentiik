package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dockertest"
	"github.com/agentiik/agentiik/internal/fixtures"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// taskResults compiles the wire's task result, which is what a runner publishes.
func taskResults(t *testing.T) *jsonschema.Schema {
	t.Helper()
	doc, err := fixtures.Wire()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("wire.schema.json", raw); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("wire.schema.json#/$defs/taskResult")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// wireSays holds one result to the wire, as the bytes it travels as.
func wireSays(t *testing.T, s *jsonschema.Schema, r bus.TaskResult) {
	t.Helper()
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(v); err != nil {
		t.Errorf("the result is not the wire's: %s\n%s", err, body)
	}
}

// published is a bus that takes every result, and what the host looked like as each went out.
type published struct {
	mu      sync.Mutex
	results []bus.TaskResult
	seen    []string

	// look is asked what the host looks like at the moment of each publication.
	look func() string
	// refuse makes the bus answer every publication with an error.
	refuse error
}

func (p *published) Report(_ context.Context, r bus.TaskResult) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refuse != nil {
		return p.refuse
	}
	p.results = append(p.results, r)
	if p.look != nil {
		p.seen = append(p.seen, p.look())
	}
	return nil
}

func (p *published) all() []bus.TaskResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bus.TaskResult(nil), p.results...)
}

// carrying is a server driver on a fake daemon, observed through a carrier's Endings, and the
// carrier reporting to a bus that takes everything. The work root is one, the driver's, the trees'
// and the results', as on a runner host.
type carrying struct {
	daemon  *dockertest.Daemon
	carrier *Carrier
	bus     *published
	logs    *fakeLogs
	root    string
	store   *objectStore
}

func carrier(t *testing.T, run func(dockertest.Container) (int, error)) *carrying {
	t.Helper()
	return carrierWith(t, run, nil)
}

// carrierWith is carrier with a driver policy changed by with.
func carrierWith(t *testing.T, run func(dockertest.Container) (int, error), with func(*driver.Policy)) *carrying {
	t.Helper()
	daemon, err := dockertest.NewDaemon(dockertest.With(dockertest.Options{
		Run:    run,
		Images: map[string]dockertest.Image{"ghcr.io/acme/agk-invoice@" + imageDigest: {Digest: imageDigest, Manifest: []byte(brickManifest)}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { daemon.Close() })
	policy := driver.DefaultPolicy()
	policy.RequireUsernsRemap = driver.RemapLifted
	policy.StopGrace = 200 * time.Millisecond
	// The helper fills a task's secrets volume, and the fake daemon plays it.
	policy.Helper = filepath.Join(t.TempDir(), "agk")
	if err := os.WriteFile(policy.Helper, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if with != nil {
		with(&policy)
	}
	root := t.TempDir()
	endings := &Endings{}
	d, err := driver.New(driver.Config{
		Socket:   daemon.Socket(),
		WorkRoot: root,
		Policy:   policy,
		Logs:     TaskLogs{},
		Observer: endings,
		Host:     installed{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	b := &published{}
	results, err := OpenResults(root, "runner-dmz-02", b)
	if err != nil {
		t.Fatal(err)
	}
	logs := &fakeLogs{}
	return &carrying{
		daemon:  daemon,
		carrier: &Carrier{Runner: "runner-dmz-02", Driver: d, Endings: endings, Results: results, Logs: logs, Log: func(s string) { t.Log(s) }},
		bus:     b,
		logs:    logs,
		root:    root,
		store:   newObjectStore(t),
	}
}

// carry assembles one task of finance, changed by with, and carries it, answering with what was
// published for it.
func (c *carrying) carry(t *testing.T, with func(*bus.TaskMessage)) (bus.TaskMessage, bus.TaskResult) {
	t.Helper()
	m, r := c.store.taskFor(t, nil, map[string]file{"agentiik.yaml": {"version: 1\n", "0644"}}, nil)
	if with != nil {
		with(&m)
		r.ExpiresAt = m.Deadline
	}
	a, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: c.root})
	if err != nil {
		t.Fatalf("assembling the task: %s", err)
	}
	if err := c.carrier.Carry(t.Context(), m, a); err != nil {
		t.Fatalf("carrying the task: %s", err)
	}
	got := c.bus.all()
	if len(got) != 1 {
		t.Fatalf("%d results were published for one task", len(got))
	}
	return m, got[0]
}

// Every result a runner assembles is the wire's task result, in each of the shapes an ending takes:
// a success, a failure, a container stopped at its deadline, a container that exited 0 and whose
// envelope broke the output contract, and a task that reached no container. Each says what the page
// says it says.
func TestEveryAssembledResultIsTheWiresTaskResult(t *testing.T) {
	schema := taskResults(t)
	log := func(m bus.TaskMessage) string {
		uri, err := agk.NewLogURI(agk.TaskID(m.IdempotencyKey))
		if err != nil {
			t.Fatal(err)
		}
		return uri.String()
	}

	t.Run("a success", func(t *testing.T) {
		c := carrier(t, nil)
		m, r := c.carry(t, nil)
		wireSays(t, schema, r)
		if r.TaskID != m.TaskID || r.IdempotencyKey != m.IdempotencyKey || r.Runner != "runner-dmz-02" || r.State != agk.TaskSucceeded {
			t.Errorf("the result names dispatch %s of %s by %s, %s", r.TaskID, r.IdempotencyKey, r.Runner, r.State)
		}
		if r.ExitCode == nil || *r.ExitCode != 0 || r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
			t.Errorf("a success reports exit %v from %s to %s", r.ExitCode, r.StartedAt, r.FinishedAt)
		}
		if len(r.Outputs) != 1 || r.Outputs[0].Port != "out" || r.Outputs[0].Items != 0 || !strings.HasPrefix(r.Outputs[0].Digest, "sha256:") {
			t.Errorf("a success that published an empty envelope on out reports %+v", r.Outputs)
		}
		if r.Artifacts == nil {
			t.Error("a success that wrote no artifact does not say so")
		}
		if r.Log == nil || r.Log.URI != log(m) {
			t.Errorf("the log is %+v, and the task's is at %s", r.Log, log(m))
		}
		if r.Usage == nil {
			t.Error("a container that ran reports no usage")
		}
	})

	t.Run("a failure", func(t *testing.T) {
		c := carrier(t, func(c dockertest.Container) (int, error) {
			c.Stderr.Write([]byte("rate limited by the upstream\n"))
			return 108, nil
		})
		m, r := c.carry(t, nil)
		wireSays(t, schema, r)
		if r.State != agk.TaskFailed || r.ExitCode == nil || *r.ExitCode != 108 || r.StartedAt.IsZero() {
			t.Errorf("a container that exited 108 is reported %s with exit %v", r.State, r.ExitCode)
		}
		// "a failed task publishes no port: its step publishes empty envelopes"
		if r.Outputs == nil || len(r.Outputs) != 0 || r.Artifacts == nil || len(r.Artifacts) != 0 {
			t.Errorf("a failure reports ports %v and artifacts %v, and it publishes neither", r.Outputs, r.Artifacts)
		}
		if r.Log == nil || r.Log.URI != log(m) || r.Log.Lines < 1 {
			t.Errorf("the log is %+v, and the task wrote a line to it", r.Log)
		}
	})

	t.Run("a container stopped at its deadline", func(t *testing.T) {
		c := carrier(t, func(c dockertest.Container) (int, error) {
			// It takes its term and does not go, so the grace runs out on it.
			<-c.Signalled()
			time.Sleep(10 * time.Second)
			return 0, nil
		})
		_, r := c.carry(t, func(m *bus.TaskMessage) {
			m.Deadline = time.Now().Add(1500 * time.Millisecond).UTC().Format(time.RFC3339Nano)
		})
		wireSays(t, schema, r)
		if r.State != agk.TaskTimedOut {
			t.Fatalf("a container stopped at its deadline is reported %s", r.State)
		}
		// "A timed_out or cancelled task carries an exit code wherever a container ran."
		if r.ExitCode == nil || *r.ExitCode != 137 || r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
			t.Errorf("a container killed at its deadline reports exit %v from %s to %s, and it left 137", r.ExitCode, r.StartedAt, r.FinishedAt)
		}
		if r.Outputs == nil || len(r.Outputs) != 0 {
			t.Errorf("a task stopped at its deadline reports ports %v", r.Outputs)
		}
	})

	t.Run("an envelope that broke the output contract", func(t *testing.T) {
		c := carrier(t, func(c dockertest.Container) (int, error) {
			if err := os.MkdirAll(filepath.Join(c.Work, "ports"), 0o755); err != nil {
				return 1, err
			}
			return 0, os.WriteFile(filepath.Join(c.Work, "ports", "out.json"), []byte("not an envelope"), 0o644)
		})
		_, r := c.carry(t, nil)
		wireSays(t, schema, r)
		// "A container that exited 0 and whose envelope broke inline_max_bytes, envelope_max_bytes
		// or max_items is reported failed with exit code 121", and the same holds of a port file
		// that is not an envelope: a container ran, and 121 says the brick broke the contract.
		if r.State != agk.TaskFailed || r.ExitCode == nil || *r.ExitCode != driver.ExitContractBroken || r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
			t.Errorf("a refused envelope is reported %s with exit %v from %s to %s", r.State, r.ExitCode, r.StartedAt, r.FinishedAt)
		}
		if r.Outputs == nil || len(r.Outputs) != 0 {
			t.Errorf("a refused envelope reports ports %v, and nothing was published", r.Outputs)
		}
	})

	t.Run("no container ran", func(t *testing.T) {
		c := carrier(t, nil)
		m, r := c.carry(t, func(m *bus.TaskMessage) {
			m.Image = "ghcr.io/acme/agk-invoice@sha256:" + strings.Repeat("9", 64)
		})
		wireSays(t, schema, r)
		want := bus.TaskResult{TaskID: m.TaskID, IdempotencyKey: m.IdempotencyKey, Runner: "runner-dmz-02", State: agk.TaskFailed}
		if b, _ := json.Marshal(r); string(b) != mustJSON(t, want) {
			t.Errorf("a task that reached no container is reported as %s, and it is a failure and nothing else", b)
		}
	})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The result is published after everything else, since it tells the controller the task is over:
// the container is removed, and the task's working directory and its tree are gone, when the bus
// is handed the result.
func TestTheResultIsPublishedOnceTheContainerAndItsDirectoriesAreGone(t *testing.T) {
	c := carrier(t, nil)
	c.bus.look = func() string {
		var left []string
		for _, made := range c.daemon.Created() {
			if made.Labels["dev.agentiik.task"] != "" && !slices.Contains(c.daemon.Removed(), made.ID) {
				left = append(left, "container "+made.ID)
			}
		}
		// The driver leaves the directories a task's working directory sat in, which hold
		// nothing, so what is looked for is a file, and a tree laid out for the key.
		filepath.WalkDir(c.root, func(path string, d os.DirEntry, err error) error {
			rel, _ := filepath.Rel(c.root, path)
			switch {
			case err != nil || rel == ".":
			case rel == driver.KeysDir || rel == ResultsDir:
				return filepath.SkipDir
			case filepath.Dir(rel) == TreesDir || !d.IsDir():
				left = append(left, rel)
			}
			return nil
		})
		return strings.Join(left, ", ")
	}
	c.carry(t, nil)
	if len(c.bus.seen) != 1 || c.bus.seen[0] != "" {
		t.Errorf("the result was published while the host still held %q", c.bus.seen)
	}
	if len(c.daemon.Created()) == 0 {
		t.Fatal("no container was created, so nothing here says what order anything went in")
	}
}

// Endings keeps a task's ending until its result is assembled, and hands every event on to the
// observer after it.
func TestEndingsKeepsTheEndingAndHandsEveryEventOn(t *testing.T) {
	var next []agk.TaskState
	e := &Endings{Next: observerFunc(func(_ context.Context, ev driver.Event) { next = append(next, ev.State) })}
	for _, s := range []agk.TaskState{agk.TaskDispatched, agk.TaskRunning, agk.TaskPublishing, agk.TaskSucceeded} {
		e.Observe(t.Context(), driver.Event{Task: "01JMZ8V1P9C4/invoice/1", State: s})
	}
	if !slices.Equal(next, []agk.TaskState{agk.TaskDispatched, agk.TaskRunning, agk.TaskPublishing, agk.TaskSucceeded}) {
		t.Errorf("the next observer was told %v", next)
	}
	ev, ok := e.take("01JMZ8V1P9C4/invoice/1")
	if !ok || ev.State != agk.TaskSucceeded {
		t.Errorf("the ending kept is %+v", ev)
	}
	if _, ok := e.take("01JMZ8V1P9C4/invoice/1"); ok {
		t.Error("an ending was kept after its result was assembled")
	}
}

type observerFunc func(context.Context, driver.Event)

func (f observerFunc) Observe(ctx context.Context, ev driver.Event) { f(ctx, ev) }

// A requeue that reaches the host which already ended its key is answered with the ending the
// record holds, under the requeue's task_id, and that is a result the wire takes.
func TestAnEndingTheRecordHoldsIsReportedAsTheWiresTaskResult(t *testing.T) {
	schema := taskResults(t)
	code := 137
	started := time.Date(2026, 9, 10, 6, 41, 9, 104e6, time.UTC)
	uri, err := agk.NewLogURI("01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/1")
	if err != nil {
		t.Fatal(err)
	}
	ending := driver.Ending{
		Key: "01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/1", State: agk.TaskTimedOut, ExitCode: &code,
		StartedAt: started, FinishedAt: started.Add(time.Minute),
		Log: &driver.EndedLog{URI: uri, Lines: 412},
		At:  started.Add(time.Minute),
	}
	m := bus.TaskMessage{TaskID: "01M2AAZ9G62NQXFAFCXKRPJEH5", IdempotencyKey: string(ending.Key)}
	r, err := EndingOf(m, "runner-dmz-02", ending)
	if err != nil {
		t.Fatal(err)
	}
	wireSays(t, schema, r)
	if r.TaskID != m.TaskID || r.ExitCode == nil || *r.ExitCode != 137 || r.Usage != nil || r.Outputs == nil {
		t.Errorf("the recorded ending is reported as %+v", r)
	}

	other := m
	other.IdempotencyKey = "01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/2"
	if _, err := EndingOf(other, "runner-dmz-02", ending); err == nil {
		t.Error("the ending of one key was reported as the answer to another")
	}
	taken := ending
	taken.State, taken.ExitCode = agk.TaskDispatched, nil
	if _, err := EndingOf(m, "runner-dmz-02", taken); err == nil {
		t.Error("a key the record holds as taken was reported as an ending")
	}
}

// A usage nobody sampled is reported without the two sampled figures, rather than as zeros the
// daemon never counted, and a sampled zero is still a figure.
func TestAUsageNobodySampledCarriesThePullAlone(t *testing.T) {
	m := bus.TaskMessage{TaskID: "01M2AAZ9G62NQXFAFCXKRPJEH5", IdempotencyKey: "01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/1"}
	code := 0
	started := time.Date(2026, 9, 10, 6, 41, 9, 0, time.UTC)
	ended := driver.Event{
		Task: "01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/1", State: agk.TaskSucceeded,
		ExitCode: &code, StartedAt: started, FinishedAt: started.Add(time.Second),
		Usage: driver.Usage{ImagePullMS: 3184},
	}
	r := resultOf(m, "runner-dmz-02", ended)
	if r.Usage == nil || r.Usage.CPUSeconds != nil || r.Usage.MaxRSSBytes != nil || r.Usage.ImagePullMS != 3184 {
		t.Errorf("an unsampled usage is reported as %+v", r.Usage)
	}
	ended.Usage.Sampled = true
	r = resultOf(m, "runner-dmz-02", ended)
	if r.Usage == nil || r.Usage.CPUSeconds == nil || *r.Usage.CPUSeconds != 0 || r.Usage.MaxRSSBytes == nil {
		t.Errorf("a sampled usage of zero is reported as %+v", r.Usage)
	}
	wireSays(t, taskResults(t), r)
}

// A runner stopping under a task that never reached a container reports nothing: the task did not
// fail, and its key is still recorded as taken, for the agent that comes back.
func TestATaskTheAgentStoppedUnderIsNotReported(t *testing.T) {
	c := carrier(t, nil)
	m, r := c.store.taskFor(t, nil, map[string]file{"agentiik.yaml": {"version: 1\n", "0644"}}, nil)
	a, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: c.root})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.carrier.Carry(ctx, m, a); !errors.Is(err, ErrNotReported) {
		t.Errorf("a task carried as the agent stopped answered %v", err)
	}
	if got := c.bus.all(); len(got) != 0 {
		t.Errorf("a task carried as the agent stopped was reported as %+v", got)
	}
	// Its result stays owed, for the agent that comes back, which owes nothing where the record
	// holds no ending of the key.
	if owed := c.carrier.Results.Owed(); len(owed) != 1 || owed[0].TaskID != m.TaskID {
		t.Errorf("a task carried as the agent stopped left %+v owed", owed)
	}
	if err := c.carrier.Recover(); err != nil {
		t.Fatal(err)
	}
	if owed, kept := c.carrier.Results.Owed(), c.carrier.Results.Keys(); len(owed) != 0 || len(kept) != 0 {
		t.Errorf("a task that reached no ending is owed %+v and kept as %v after a restart", owed, kept)
	}
}

// refusing is a driver that refuses every delivery with err, as it refuses one of a key another
// delivery on this host is running or has just ended.
type refusing struct{ err error }

func (d refusing) Run(context.Context, graph.Task) (graph.Result, error) {
	return graph.Result{}, d.err
}

func (refusing) Stop(context.Context, graph.Stop) error { return nil }

// A delivery of a key another delivery on this host is still running reports nothing, since the
// other reports the ending, and leaves every tree of the key where it is, since the running
// container is bound to one of them. Nor does it take the other delivery's ending, which the other
// is about to report under its own task_id.
func TestADeliveryOfAKeyInFlightLeavesItsTreesAndReportsNothing(t *testing.T) {
	root := t.TempDir()
	s := newObjectStore(t)
	m, r := s.taskFor(t, nil, map[string]file{"agentiik.yaml": {"version: 1\n", "0644"}}, nil)
	running, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { running.Remove() })
	again, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	b := &published{}
	results, err := OpenResults(root, "runner-dmz-02", b)
	if err != nil {
		t.Fatal(err)
	}
	endings := &Endings{}
	endings.Observe(t.Context(), driver.Event{Task: running.Task.ID, State: agk.TaskSucceeded})
	c := &Carrier{Runner: "runner-dmz-02", Driver: refusing{fmt.Errorf("driver: task: %w", driver.ErrTaskInFlight)}, Endings: endings, Results: results}
	if err := c.Carry(t.Context(), m, again); !errors.Is(err, ErrNotReported) {
		t.Errorf("a delivery of a key in flight answered %v", err)
	}
	if _, err := os.Stat(filepath.Join(running.Sources.Repo, "agentiik.yaml")); err != nil {
		t.Errorf("the tree the running container is bound to was taken away: %s", err)
	}
	if got := b.all(); len(got) != 0 {
		t.Errorf("a delivery of a key in flight reported %+v", got)
	}
	if _, kept := endings.take(running.Task.ID); !kept {
		t.Error("a delivery of a key in flight took the ending of the delivery running it")
	}
	if owed := results.Owed(); len(owed) != 0 {
		t.Errorf("a delivery of a key in flight left %+v owed, and it has nothing to report", owed)
	}

	// The same message delivered twice owes what the delivery running it owes, which is not
	// this delivery's to take away.
	if _, err := results.owe(m, "runner-dmz-02"); err != nil {
		t.Fatal(err)
	}
	if err := c.Carry(t.Context(), m, again); !errors.Is(err, ErrNotReported) {
		t.Errorf("a second delivery of the message running answered %v", err)
	}
	if owed := results.Owed(); len(owed) != 1 || owed[0].TaskID != m.TaskID {
		t.Errorf("a second delivery of the message running left %+v owed, want what the running delivery owes", owed)
	}
}

// A delivery Run refused for a key this host has already ended reports the ending the record
// holds, under its own task_id, and leaves alone an ending the driver told of the delivery that
// ended the key, which that delivery reports.
func TestADeliveryOfAKeyAlreadyEndedReportsTheRecordedEnding(t *testing.T) {
	root := t.TempDir()
	s := newObjectStore(t)
	m, r := s.taskFor(t, nil, map[string]file{"agentiik.yaml": {"version: 1\n", "0644"}}, nil)
	a, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	b := &published{}
	results, err := OpenResults(root, "runner-dmz-02", b)
	if err != nil {
		t.Fatal(err)
	}
	code := 108
	started := time.Date(2026, 9, 10, 6, 41, 9, 0, time.UTC)
	recorded := driver.Ending{Key: a.Task.ID, State: agk.TaskFailed, ExitCode: &code, StartedAt: started, FinishedAt: started.Add(time.Second), At: started.Add(time.Second)}
	endings := &Endings{}
	other := code + 1
	endings.Observe(t.Context(), driver.Event{Task: a.Task.ID, State: agk.TaskFailed, ExitCode: &other, StartedAt: started, FinishedAt: started.Add(time.Second)})
	c := &Carrier{Runner: "runner-dmz-02", Driver: refusing{&driver.Completed{Ending: recorded}}, Endings: endings, Results: results}
	if err := c.Carry(t.Context(), m, a); err != nil {
		t.Fatal(err)
	}
	got := b.all()
	if len(got) != 1 || got[0].TaskID != m.TaskID || got[0].ExitCode == nil || *got[0].ExitCode != 108 {
		t.Errorf("a delivery of a key already ended reported %+v, and the record says it exited 108", got)
	}
	if _, kept := endings.take(a.Task.ID); !kept {
		t.Error("a delivery of a key already ended took the ending the driver told of another delivery")
	}
}

// snapshot copies what a work root holds for a restarted agent to read, the record and the results,
// into a work root of its own: what the disk would hold had the agent stopped at that moment.
func snapshot(t *testing.T, root string) string {
	t.Helper()
	to := t.TempDir()
	for _, dir := range []string{driver.KeysDir, ResultsDir} {
		from := filepath.Join(root, dir)
		err := filepath.WalkDir(from, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			switch {
			case d.IsDir():
				return os.MkdirAll(filepath.Join(to, rel), info.Mode().Perm())
			case d.Type().IsRegular():
				b, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(to, rel), b, info.Mode().Perm())
			}
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	return to
}

// An agent that stops at any moment of a task's ending leaves its key named to the agent that comes
// back, and a result that agent publishes. Before the ending is written, the record names the key
// as taken and nothing is owed. Once it is written, whether the log is still counted as the driver
// counted it, is being closed, or is closed and its result not yet kept, the restarted agent keeps
// the result from the record under the dispatch's own task_id, names its key from its first
// heartbeat and publishes it: what the container left, and the log where it is, truncated since its
// closing chunk's answer went with the agent, counting no line the store may not hold. The record
// is then given the same log, so a requeue answered from it says the same.
func TestAnAgentStoppedAnywhereInATasksEndingLeavesItsKeyNamedAndItsResultToPublish(t *testing.T) {
	c := carrier(t, func(ctr dockertest.Container) (int, error) {
		fmt.Fprintln(ctr.Stderr, "reading 412 invoices")
		return 0, nil
	})
	stopped := map[string]string{}
	c.carrier.atPoint = func(point string) { stopped[point] = snapshot(t, c.root) }
	// Held first, as the loop holds every key it takes, which is what writes it down as taken.
	m, first := c.carry(t, func(m *bus.TaskMessage) {
		if err := c.carrier.Driver.(*driver.Docker).Hold(agk.TaskID(m.IdempotencyKey)); err != nil {
			t.Fatal(err)
		}
	})
	if first.State != agk.TaskSucceeded || first.Log == nil {
		t.Fatalf("the task was reported as %+v, and it succeeded with a log", first)
	}
	if owed := c.carrier.Results.Owed(); len(owed) != 0 {
		t.Errorf("a task whose result went out is still owed one: %+v", owed)
	}
	key := agk.TaskID(m.IdempotencyKey)

	for _, point := range []string{"run", "ran", "closing", "logged"} {
		t.Run("stopped at "+point, func(t *testing.T) {
			root, ok := stopped[point]
			if !ok {
				t.Fatalf("the carrier never reached %s", point)
			}
			d := stepDriver(t, root)
			b := &published{}
			results, err := OpenResults(root, "runner-dmz-02", b)
			if err != nil {
				t.Fatal(err)
			}
			again := &Carrier{Runner: "runner-dmz-02", Driver: d, Results: results, Logs: &fakeLogs{}, Log: func(s string) { t.Log(s) }}
			if err := again.Recover(); err != nil {
				t.Fatal(err)
			}
			taken, err := d.Dispatched()
			if err != nil {
				t.Fatal(err)
			}
			if owed := results.Owed(); len(owed) != 0 {
				t.Errorf("the restarted agent still owes %+v", owed)
			}

			if point == "run" {
				// Nothing ended: the record names the key as taken, and there is no result.
				if !slices.Equal(taken, []agk.TaskID{key}) || len(results.Keys()) != 0 {
					t.Errorf("a key taken and not ended is named as taken by %v and kept by %v, want taken alone", taken, results.Keys())
				}
				return
			}
			if len(taken) != 0 || !slices.Equal(results.Keys(), []string{m.IdempotencyKey}) {
				t.Fatalf("a key whose ending was written is named as taken by %v and kept by %v, want kept alone", taken, results.Keys())
			}
			if err := results.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := b.all()
			if len(got) != 1 {
				t.Fatalf("%d results were published for one task", len(got))
			}
			r := got[0]
			wireSays(t, taskResults(t), r)
			if r.TaskID != m.TaskID || r.Runner != "runner-dmz-02" || r.State != agk.TaskSucceeded || r.ExitCode == nil || *r.ExitCode != 0 {
				t.Errorf("the result recovered is %+v", r)
			}
			if mustJSON(t, r.Outputs) != mustJSON(t, first.Outputs) || mustJSON(t, r.Artifacts) != mustJSON(t, first.Artifacts) {
				t.Errorf("the result recovered names %s and %s, and the task left %s and %s",
					mustJSON(t, r.Outputs), mustJSON(t, r.Artifacts), mustJSON(t, first.Outputs), mustJSON(t, first.Artifacts))
			}
			if r.Log == nil || r.Log.URI != first.Log.URI || !r.Log.Truncated || r.Log.Lines != 0 {
				t.Errorf("the result recovered says of its log %+v, want it at %s, truncated and counting no line", r.Log, first.Log.URI)
			}
			recorded, ended, err := d.Ended(key)
			if err != nil || !ended || recorded.Log == nil {
				t.Fatalf("the record holds %+v, %t, %v", recorded, ended, err)
			}
			if recorded.Log.URI.String() != r.Log.URI || recorded.Log.Lines != r.Log.Lines || recorded.Log.Truncated != r.Log.Truncated {
				t.Errorf("the record says of the log %+v, and the result said %+v", recorded.Log, r.Log)
			}
			if left, _ := os.ReadDir(filepath.Join(root, ResultsDir)); len(left) != 0 {
				t.Errorf("the results directory still holds %v once the result went out", left)
			}
		})
	}
}

// endedAs is a record holding one ending, for Recover to read.
type endedAs struct {
	refusing
	e      driver.Ending
	logged []*driver.EndedLog
}

func (d *endedAs) Ended(id agk.TaskID) (driver.Ending, bool, error) { return d.e, d.e.Key == id, nil }

func (d *endedAs) Logged(_ agk.TaskID, l *driver.EndedLog) error {
	d.logged = append(d.logged, l)
	return nil
}

// A result a restart keeps addresses a log wherever the record names one, a pull that ended the task
// included, and wherever a container started, whose log the record was cleared of while it was being
// closed: truncated, and counting no line the store may not hold. An ending that names neither had
// no log opened, and its result addresses none and gives the record none.
func TestARecoveredResultAddressesALogOnlyWhereAContainerStarted(t *testing.T) {
	m := bus.TaskMessage{TaskID: "01M2AAZ9G62NQXFAFCXKRPJEH5", IdempotencyKey: "01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/1"}
	started := time.Date(2026, 9, 10, 6, 41, 9, 0, time.UTC)
	code := 0
	for name, tc := range map[string]struct {
		e      driver.Ending
		logged bool
	}{
		"no log opened":     {driver.Ending{Key: agk.TaskID(m.IdempotencyKey), State: agk.TaskFailed, At: started}, false},
		"timed out pulling": {driver.Ending{Key: agk.TaskID(m.IdempotencyKey), State: agk.TaskTimedOut, Log: &driver.EndedLog{Lines: 1}, At: started}, true},
		"closing":           {driver.Ending{Key: agk.TaskID(m.IdempotencyKey), State: agk.TaskSucceeded, ExitCode: &code, StartedAt: started, FinishedAt: started.Add(time.Second), Outputs: []driver.EndedPort{}, At: started}, true},
	} {
		t.Run(name, func(t *testing.T) {
			b := &published{}
			results, err := OpenResults(t.TempDir(), "runner-dmz-02", b)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := results.owe(m, "runner-dmz-02"); err != nil {
				t.Fatal(err)
			}
			d := &endedAs{e: tc.e}
			c := &Carrier{Runner: "runner-dmz-02", Driver: d, Results: results, Logs: &fakeLogs{}}
			if err := c.Recover(); err != nil {
				t.Fatal(err)
			}
			if err := results.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := b.all()
			if len(got) != 1 {
				t.Fatalf("%d results were published", len(got))
			}
			if has := got[0].Log != nil; has != tc.logged {
				t.Errorf("the result recovered says of its log %+v", got[0].Log)
			}
			if tc.logged && (!got[0].Log.Truncated || got[0].Log.Lines != 0) {
				t.Errorf("a log whose close went with the agent is reported %+v", got[0].Log)
			}
			if wrote := len(d.logged) > 0; wrote != tc.logged {
				t.Errorf("the record was given %v", d.logged)
			}
		})
	}
}
