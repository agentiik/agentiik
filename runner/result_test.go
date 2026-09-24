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
	root    string
	store   *objectStore
}

func carrier(t *testing.T, run func(dockertest.Container) (int, error)) *carrying {
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
	policy.RequireSecretsTmpfs = driver.SecretsTmpfsLifted
	policy.SecretsDir = ""
	policy.StopGrace = 200 * time.Millisecond
	root := t.TempDir()
	endings := &Endings{}
	d, err := driver.New(driver.Config{
		Socket:   daemon.Socket(),
		WorkRoot: root,
		Policy:   policy,
		Logs:     &taskLogs{},
		Observer: endings,
		Host:     installed{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	b := &published{}
	results, err := OpenResults(root, b)
	if err != nil {
		t.Fatal(err)
	}
	return &carrying{
		daemon:  daemon,
		carrier: &Carrier{Runner: "runner-dmz-02", Driver: d, Endings: endings, Results: results, Logs: true, Log: func(s string) { t.Log(s) }},
		bus:     b,
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
	r := resultOf(m, "runner-dmz-02", ended, false)
	if r.Usage == nil || r.Usage.CPUSeconds != nil || r.Usage.MaxRSSBytes != nil || r.Usage.ImagePullMS != 3184 {
		t.Errorf("an unsampled usage is reported as %+v", r.Usage)
	}
	ended.Usage.Sampled = true
	r = resultOf(m, "runner-dmz-02", ended, false)
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
}

// inFlight is a driver on which another delivery of every key is still running.
type inFlight struct{}

func (inFlight) Run(context.Context, graph.Task) (graph.Result, error) {
	return graph.Result{}, fmt.Errorf("driver: task: %w", driver.ErrTaskInFlight)
}

func (inFlight) Stop(context.Context, graph.Stop) error { return nil }

// A delivery of a key another delivery on this host is still running reports nothing, since the
// other reports the ending, and leaves every tree of the key where it is, since the running
// container is bound to one of them.
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
	results, err := OpenResults(root, b)
	if err != nil {
		t.Fatal(err)
	}
	c := &Carrier{Runner: "runner-dmz-02", Driver: inFlight{}, Endings: &Endings{}, Results: results}
	if err := c.Carry(t.Context(), m, again); !errors.Is(err, ErrNotReported) {
		t.Errorf("a delivery of a key in flight answered %v", err)
	}
	if _, err := os.Stat(filepath.Join(running.Sources.Repo, "agentiik.yaml")); err != nil {
		t.Errorf("the tree the running container is bound to was taken away: %s", err)
	}
	if got := b.all(); len(got) != 0 {
		t.Errorf("a delivery of a key in flight reported %+v", got)
	}
}
