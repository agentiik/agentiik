package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dockertest"
	"github.com/agentiik/agentiik/internal/fixtures"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// beats is an API that answers heartbeats as answer says and every other request with 503, and
// keeps, in order, the path of every request that reached it and the body of every heartbeat.
type beats struct {
	srv    *httptest.Server
	mu     sync.Mutex
	paths  []string
	sent   [][]byte
	answer func(beatRequest) (int, string)
}

func newBeats(t *testing.T, answer func(beatRequest) (int, string)) *beats {
	t.Helper()
	b := &beats{answer: answer}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.paths = append(b.paths, r.Method+" "+r.URL.Path)
		answer := b.answer
		b.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != heartbeatPath {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+credential {
			t.Errorf("a heartbeat carried Authorization %q", r.Header.Get("Authorization"))
		}
		var req beatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("a heartbeat that is not JSON: %s", body)
		}
		b.mu.Lock()
		b.sent = append(b.sent, body)
		b.mu.Unlock()
		status, said := answer(req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, said)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

// answering changes what the API answers from now on.
func (b *beats) answering(answer func(beatRequest) (int, string)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.answer = answer
}

// heard are the heartbeats that reached the API, in order.
func (b *beats) heard() []beatRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []beatRequest
	for _, body := range b.sent {
		var req beatRequest
		json.Unmarshal(body, &req)
		out = append(out, req)
	}
	return out
}

// requested are the requests that reached the API, as method and path, in order.
func (b *beats) requested() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.paths)
}

// answered answers a heartbeat as an installation with nothing to order does.
func answered(beatRequest) (int, string) {
	return http.StatusOK, beatAnswered(time.Now(), `"drain":false,"cancel":[]`)
}

// beatAnswered is an answer received at at, with the rest of its fields as written.
func beatAnswered(at time.Time, rest string) string {
	return fmt.Sprintf(`{"received_at":%q,%s}`, at.UTC().Format(time.RFC3339Nano), rest)
}

// heartbeat is a Heartbeat against the API at url, holding what holding answers.
func heartbeat(t *testing.T, url string, holding ...string) (*Heartbeat, *strings.Builder) {
	t.Helper()
	client, err := NewClient(url, credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	var mu sync.Mutex
	return &Heartbeat{
		Client: client, Runner: "runner-dmz-02", Concurrency: 8,
		Holding: func() []string { return holding },
		Log:     func(s string) { mu.Lock(); log.WriteString(s + "\n"); mu.Unlock() },
	}, &log
}

// The request is the wire's runnerHeartbeat.request, posted where the page says, and the corpus's
// answer is read back whole.
func TestAHeartbeatIsTheWiresRequestAndReadsTheWiresAnswer(t *testing.T) {
	b, err := fs.ReadFile(fixtures.FS, "fixtures/wire/valid/runner-heartbeat.json")
	if err != nil {
		t.Fatal(err)
	}
	var pair struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(b, &pair); err != nil {
		t.Fatal(err)
	}
	api := newBeats(t, func(beatRequest) (int, string) { return http.StatusOK, string(pair.Response) })
	held := []string{"01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/2/3/8", "01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize/1"}
	h, _ := heartbeat(t, api.srv.URL, held...)
	if err := h.Beat(t.Context()); err != nil {
		t.Fatalf("the corpus's answer was refused: %s", err)
	}

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
	schema, err := c.Compile("wire.schema.json#/$defs/runnerHeartbeat/properties/request")
	if err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	sent := api.sent[0]
	api.mu.Unlock()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(sent))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(v); err != nil {
		t.Errorf("the heartbeat the wire refuses: %s\n%s", err, sent)
	}
	got := api.heard()[0]
	if got.Runner != "runner-dmz-02" || got.State != "ready" || got.Concurrency != 8 || got.AgentVersion != Version() || !slices.Equal(got.Tasks, held) {
		t.Errorf("the heartbeat said %+v", got)
	}
	if want := []string{"POST " + heartbeatPath}; !slices.Equal(api.requested(), want) {
		t.Errorf("the heartbeat reached %v, want %v", api.requested(), want)
	}

	// An idle host says it holds nothing, rather than saying nothing.
	idle, _ := heartbeat(t, api.srv.URL)
	idle.Holding = nil
	if err := idle.Beat(t.Context()); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	last := string(api.sent[len(api.sent)-1])
	api.mu.Unlock()
	if !strings.Contains(last, `"tasks":[]`) {
		t.Errorf("an idle host's heartbeat is %s, and holding nothing is written []", last)
	}
}

// The runner counts in the interval the controller's sweep counts silence in, so that it is never
// declared lost while it reports on time.
func TestTheHeartbeatIntervalIsTheOneTheSweepCountsIn(t *testing.T) {
	if HeartbeatInterval != db.HeartbeatInterval {
		t.Errorf("the runner beats every %s and the sweep counts in %s", HeartbeatInterval, db.HeartbeatInterval)
	}
	if HeartbeatInterval != 10*time.Second {
		t.Errorf("the runner beats every %s, and the page says every 10 seconds", HeartbeatInterval)
	}
}

// An answer the wire refuses is refused rather than half read: a runner that took a missing cancel
// for an empty one would never hear a stop.
func TestAnAnswerThatIsNotTheWiresIsRefused(t *testing.T) {
	for name, said := range map[string]string{
		"no cancel":           beatAnswered(time.Now(), `"drain":false`),
		"a received_at":       `{"received_at":"yesterday","drain":false,"cancel":[]}`,
		"a field of its own":  beatAnswered(time.Now(), `"drain":false,"cancel":[],"interval_seconds":10`),
		"two values, not one": beatAnswered(time.Now(), `"drain":false,"cancel":[]`) + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			api := newBeats(t, func(beatRequest) (int, string) { return http.StatusOK, said })
			h, _ := heartbeat(t, api.srv.URL)
			if err := h.Beat(t.Context()); err == nil {
				t.Errorf("the answer %s was taken", said)
			}
		})
	}
}

// "A restarted runner names a recorded key before redeeming anything." The keys an earlier agent
// took and never ended, and those of the results it kept, are in the first heartbeat, before the
// bus credential is asked for and so before anything is taken or redeemed, and Ready follows that
// heartbeat's answer. The taken keys are named until the window a message could still come round
// in has passed, and not after; a kept result's for as long as the bus has not taken it, which
// here, the API never handing out a bus credential, is throughout.
func TestARestartedRunnerNamesARecordedKeyBeforeRedeemingAnything(t *testing.T) {
	root := t.TempDir()
	recorded := agk.NewTaskID(agk.NewRunID(), "invoice", 1, agk.Shard{})
	earlier := stepDriver(t, root)
	if err := earlier.Hold(recorded); err != nil {
		t.Fatal(err)
	}
	earlier.Close()
	kept := ending("01M2AAZ9G62NQXFAFCXKRPJEH5", "01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize/1")
	results, err := OpenResults(root, "runner-dmz-02", &published{refuse: errUnreachable})
	if err != nil {
		t.Fatal(err)
	}
	if err := results.Report(t.Context(), kept); !errors.Is(err, errUnreachable) {
		t.Fatalf("a result the bus refused answered %v", err)
	}

	api := newBeats(t, answered)
	var readies int
	a := agentOf(t, &readies, api.srv.URL, root)
	a.every, a.earlierFor = 50*time.Millisecond, 400*time.Millisecond
	ready := make(chan struct{})
	a.Ready = func() error { readies++; close(ready); return nil }
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, a) }()
	defer func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %s", err)
		}
	}()

	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent never said it was ready")
	}
	requested, heard := api.requested(), api.heard()
	if len(requested) == 0 || requested[0] != "POST "+heartbeatPath || len(heard) == 0 {
		t.Fatalf("the requests were %v, and a restarted agent heartbeats before it asks for anything", requested)
	}
	if got, want := heard[0].Tasks, []string{kept.IdempotencyKey, string(recorded)}; !slices.Equal(got, want) {
		t.Errorf("the first heartbeat names %v, want the kept result's key and the key the record holds as taken, %v", got, want)
	}

	eventually(t, "a heartbeat naming the kept result's key alone once the taken key's window passed", func() bool {
		heard := api.heard()
		return slices.Equal(heard[len(heard)-1].Tasks, []string{kept.IdempotencyKey})
	})
}

// stepDriver is a driver on a fake daemon of its own, keeping its record under root.
func stepDriver(t *testing.T, root string) *driver.Docker {
	t.Helper()
	daemon, err := dockertest.NewDaemon(dockertest.WithUsernsRemap(165536, 165536))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { daemon.Close() })
	d, err := driver.New(driver.Config{
		Socket: daemon.Socket(), WorkRoot: root, Policy: driver.Policy{SecretsDir: "/run/agentiik/secrets"}, Host: installed{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// Ready is said once a heartbeat is answered and not before: an API that does not answer is asked
// again, and the start waits on it.
func TestNoReadyIsSaidUntilAHeartbeatIsAnswered(t *testing.T) {
	api := newBeats(t, func(beatRequest) (int, string) { return http.StatusServiceUnavailable, "" })
	var readies int
	a := agentOf(t, &readies, api.srv.URL, t.TempDir())
	a.every = 50 * time.Millisecond
	ready := make(chan struct{})
	a.Ready = func() error { readies++; close(ready); return nil }
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, a) }()
	defer func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %s", err)
		}
	}()

	eventually(t, "three unanswered heartbeats", func() bool { return len(api.heard()) >= 3 })
	select {
	case <-ready:
		t.Fatal("the agent said it was ready with no heartbeat answered")
	default:
	}
	api.answering(answered)
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent never said it was ready once its heartbeat was answered")
	}
}

// "A 401 stops the agent": answered 401 once it is serving, it returns saying to join again, and
// asks nothing more.
func TestA401StopsTheAgent(t *testing.T) {
	api := newBeats(t, answered)
	var readies int
	a := agentOf(t, &readies, api.srv.URL, t.TempDir())
	a.every = 50 * time.Millisecond
	a.Ready = func() error {
		readies++
		api.answering(func(beatRequest) (int, string) {
			return http.StatusUnauthorized, `{"error":"that credential opens nothing"}`
		})
		return nil
	}
	served := make(chan error, 1)
	go func() { served <- Serve(t.Context(), a) }()
	var err error
	select {
	case err = <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent was answered 401 and is still serving")
	}
	if !errors.Is(err, ErrCredentialRefused) || !strings.Contains(err.Error(), "this runner's credential was refused: join it again") {
		t.Errorf("the agent stopped with %v", err)
	}
	if readies != 1 {
		t.Errorf("ready was said %d times", readies)
	}
	n := len(api.heard())
	time.Sleep(200 * time.Millisecond)
	if more := len(api.heard()) - n; more != 0 {
		t.Errorf("%d heartbeats went out after the agent stopped", more)
	}
}

// A 401 at the first heartbeat ends the start, and nothing is asked again.
func TestA401AtTheFirstHeartbeatEndsTheStart(t *testing.T) {
	api := newBeats(t, func(beatRequest) (int, string) { return http.StatusUnauthorized, "" })
	var readies int
	a := agentOf(t, &readies, api.srv.URL, t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := Serve(ctx, a)
	if ctx.Err() != nil {
		t.Fatal("the start answered 401 was still asking after ten seconds")
	}
	if !errors.Is(err, ErrCredentialRefused) || !strings.Contains(err.Error(), "join it again") {
		t.Errorf("the start ended with %v", err)
	}
	if readies != 0 {
		t.Errorf("ready was said %d times", readies)
	}
	if n := len(api.requested()); n != 1 {
		t.Errorf("%d requests reached the API, want the one heartbeat", n)
	}
}

// "A cancel in the answer stops the container." The answer names a key this host is running, and
// the container is sent its stop and ends cancelled.
func TestACancelInTheAnswerStopsTheContainer(t *testing.T) {
	c := carrier(t, func(ctr dockertest.Container) (int, error) {
		select {
		case <-ctr.Signalled():
			return 143, nil
		case <-time.After(20 * time.Second):
			return 0, nil
		}
	})
	m, r := c.store.taskFor(t, nil, map[string]file{"agentiik.yaml": {"version: 1\n", "0644"}}, nil)
	a, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: c.root})
	if err != nil {
		t.Fatal(err)
	}
	carried := make(chan error, 1)
	go func() { carried <- c.carrier.Carry(t.Context(), m, a) }()
	eventually(t, "the task's container starting", func() bool {
		for _, made := range c.daemon.Created() {
			if made.Labels[driver.LabelTask] == m.IdempotencyKey {
				return true
			}
		}
		return false
	})

	api := newBeats(t, func(beatRequest) (int, string) {
		return http.StatusOK, beatAnswered(time.Now(), fmt.Sprintf(`"drain":false,"cancel":[%q]`, m.IdempotencyKey))
	})
	h, _ := heartbeat(t, api.srv.URL, m.IdempotencyKey)
	h.Stopper = c.carrier.Driver.(*driver.Docker)
	if err := h.Beat(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.Wait()
	select {
	case err := <-carried:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the container the answer cancelled is still running")
	}
	var signals []string
	for _, made := range c.daemon.Created() {
		if made.Labels[driver.LabelTask] == m.IdempotencyKey {
			signals = made.Signals
		}
	}
	if !slices.Contains(signals, "SIGTERM") {
		t.Errorf("the container was sent %v, and a stop is SIGTERM first", signals)
	}
	if got := c.bus.all(); len(got) != 1 || got[0].State != agk.TaskCancelled {
		t.Errorf("the task was reported %+v, want cancelled", got)
	}
}

// stops is a Stopper that keeps what it was asked to stop.
type stops struct {
	mu   sync.Mutex
	told []graph.Stop
}

func (s *stops) Stop(_ context.Context, st graph.Stop) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.told = append(s.told, st)
	return nil
}

func (s *stops) stopped() []graph.Stop {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.told)
}

// "Only keys the request listed ever appear here, so a runner is never asked about work it does not
// have." One the request did not name is no order to this host, and nothing is stopped for it.
func TestOnlyAKeyTheHeartbeatNamedIsStopped(t *testing.T) {
	const named, other = "01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize/1", "01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/1"
	api := newBeats(t, func(beatRequest) (int, string) {
		return http.StatusOK, beatAnswered(time.Now(), fmt.Sprintf(`"drain":false,"cancel":[%q,%q]`, named, other))
	})
	h, _ := heartbeat(t, api.srv.URL, named)
	s := &stops{}
	h.Stopper = s
	if err := h.Beat(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.Wait()
	if got, want := s.stopped(), []graph.Stop{{Task: named, Reason: graph.StopCancelled}}; !slices.Equal(got, want) {
		t.Errorf("the answer stopped %v, want %v", got, want)
	}
}

// A drain order is said in the log with its reason and grace, once, and from then on the runner
// reports itself draining, "which is what a runner reports once a drain order or a revocation has
// reached it".
func TestADrainOrderIsSaidAndReportedAsDraining(t *testing.T) {
	api := newBeats(t, func(beatRequest) (int, string) {
		return http.StatusOK, beatAnswered(time.Now(), `"drain":true,"reason":"pool zone=dmz is being retired","results_accepted_until":"2026-09-24T12:00:00Z","cancel":[]`)
	})
	h, log := heartbeat(t, api.srv.URL)
	for range 3 {
		if err := h.Beat(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var states []string
	for _, req := range api.heard() {
		states = append(states, req.State)
	}
	if want := []string{"ready", "draining", "draining"}; !slices.Equal(states, want) {
		t.Errorf("the runner reported %v, want %v", states, want)
	}
	if n := strings.Count(log.String(), "orders this runner to drain"); n != 1 {
		t.Errorf("the drain was said %d times:\n%s", n, log)
	}
	for _, want := range []string{"pool zone=dmz is being retired", "2026-09-24T12:00:00Z"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the log does not say %q:\n%s", want, log)
		}
	}
	if d := h.Drain(); !d.Ordered || d.Reason != "pool zone=dmz is being retired" || !d.ResultsAcceptedUntil.Equal(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("the drain kept is %+v", d)
	}
}

// "A difference of more than a second between sent_at and received_at is logged", once while it
// lasts, and again when it ends.
func TestAClockMoreThanASecondOutIsSaidOnce(t *testing.T) {
	var ahead time.Duration
	var mu sync.Mutex
	api := newBeats(t, func(beatRequest) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		return http.StatusOK, beatAnswered(time.Now().Add(-ahead), `"drain":false,"cancel":[]`)
	})
	h, log := heartbeat(t, api.srv.URL)
	beat := func(by time.Duration) {
		mu.Lock()
		ahead = by
		mu.Unlock()
		if err := h.Beat(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	beat(0)
	beat(500 * time.Millisecond)
	if log.Len() != 0 {
		t.Errorf("a clock within a second was said:\n%s", log)
	}
	beat(90 * time.Second)
	beat(90 * time.Second)
	if n := strings.Count(log.String(), "ahead of the installation's"); n != 1 {
		t.Errorf("a clock a minute and a half ahead was said %d times:\n%s", n, log)
	}
	beat(0)
	if !strings.Contains(log.String(), "back within a second") {
		t.Errorf("the clock coming back was not said:\n%s", log)
	}
	beat(-5 * time.Second)
	if !strings.Contains(log.String(), "behind the installation's") {
		t.Errorf("a clock behind was not said:\n%s", log)
	}
}

// A heartbeat names at most as many keys as the API takes in one, the held ones first, since one
// naming more would be refused whole and every task with it.
func TestAHeartbeatNamesNoMoreKeysThanTheAPITakes(t *testing.T) {
	var held []string
	for i := range beatMaxTasks + 10 {
		held = append(held, fmt.Sprintf("01JMZ8V1P9C4XQ7K2N4D6F8H0A/step-%d/1", i))
	}
	api := newBeats(t, answered)
	h, log := heartbeat(t, api.srv.URL, held...)
	h.Earlier = []agk.TaskID{"01JMZ8V1P9C4XQ7K2N4D6F8H0A/earlier/1"}
	for range 2 {
		if err := h.Beat(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	got := api.heard()[0].Tasks
	if len(got) != beatMaxTasks || !slices.Equal(got, held[:beatMaxTasks]) {
		t.Errorf("the heartbeat named %d keys, want the first %d held", len(got), beatMaxTasks)
	}
	if n := strings.Count(log.String(), "as many as one heartbeat names"); n != 1 {
		t.Errorf("the cut was said %d times:\n%s", n, log)
	}
}

// Real PostgreSQL, the real API's routes and the shared bus: a task this runner redeemed stays
// alive for as long as the agent heartbeats, well past the 30 seconds a silent runner's task is
// declared lost in, and is declared lost 30 seconds after the agent stops. The API's clock is moved
// forward rather than waited on.
func TestARedeemedTaskStaysAliveWhileTheAgentRunsAndIsLostThirtySecondsAfterItStops(t *testing.T) {
	in := anInstallationServing(t)
	release := make(chan struct{})
	c := carrier(t, func(dockertest.Container) (int, error) {
		<-release
		return 0, nil
	})
	defer close(release)
	client, err := NewClient(in.url, in.credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, Agent{
			Config: Config{
				API: in.url, Runner: in.runner, Pool: in.poolName, Concurrency: 1,
				WorkDir: c.root, Credential: in.credential, Labels: []string{"zone=dmz"},
			},
			Driver: c.carrier.Driver.(*driver.Docker), Client: client, Endings: c.carrier.Endings,
			Log:   func(s string) { t.Log(s) },
			every: 100 * time.Millisecond,
		})
	}()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			cancel()
			if err := <-served; err != nil {
				t.Errorf("serve: %s", err)
			}
		}
	}
	defer stop()

	m := in.dispatch(t, "invoice")
	eventually(t, "the task bound to the runner", func() bool { return in.state(t, m.TaskID) == "dispatched "+in.runner })

	// Well past LostAfter by the API's clock, and the heartbeat is heard at that clock.
	in.ahead(db.LostAfter + 10*time.Second)
	now := in.now()
	eventually(t, "a heartbeat heard at the moved clock", func() bool { return !in.heardAt(t, m.TaskID).Before(now) })
	if lost := in.swept(t, in.now()); lost != 0 {
		t.Errorf("the sweep declared %d tasks lost while their runner heartbeat", lost)
	}
	if got := in.state(t, m.TaskID); got != "dispatched "+in.runner {
		t.Errorf("the task reads %q while its runner heartbeats", got)
	}

	// A daemon slow to remove the container keeps the agent winding down, and the task is still
	// named while it does: the heartbeat ends when the agent returns, not when it is told to stop.
	c.daemon.Handle("DELETE", "/containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
		w.WriteHeader(http.StatusNoContent)
	})
	told := in.now()
	stop()
	last := in.heardAt(t, m.TaskID)
	if !last.After(told.Add(500 * time.Millisecond)) {
		t.Errorf("the last heartbeat naming the task was at %s, and the agent told to stop at %s wound down for a second after", last, told)
	}
	if lost := in.swept(t, last.Add(db.LostAfter-time.Second)); lost != 0 {
		t.Errorf("the sweep declared %d tasks lost within 30 seconds of the last heartbeat", lost)
	}
	if lost := in.swept(t, last.Add(db.LostAfter+time.Second)); lost != 1 {
		t.Errorf("the sweep declared %d tasks lost 30 seconds after the agent stopped, want the one it held", lost)
	}
	if got := in.state(t, m.TaskID); got != "lost "+in.runner {
		t.Errorf("the task reads %q once its runner went silent", got)
	}
}

// A key off the wire's grammar is left out and said once, rather than sent: the API refuses a
// heartbeat naming one whole, and every task on the host would go lost with it.
func TestAKeyOffTheWiresGrammarIsLeftOutAndSaidOnce(t *testing.T) {
	const good, lowercase = "01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize/1", "01jmz8v1p9c4xq7k2n4d6f8h0a/normalize/1"
	api := newBeats(t, answered)
	h, log := heartbeat(t, api.srv.URL, good, lowercase, "01JMZ8V1P9C4XQ7K2N4D6F8H0A/fan/1/5/4")
	for range 2 {
		if err := h.Beat(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := api.heard()[1].Tasks; !slices.Equal(got, []string{good}) {
		t.Errorf("the heartbeat named %v, want %s alone", got, good)
	}
	if n := strings.Count(log.String(), "is not named in the heartbeat"); n != 2 {
		t.Errorf("the keys left out were said %d times, want once each:\n%s", n, log)
	}
}

// keyForm is the wire's idempotency key, character for character.
func TestTheKeysAHeartbeatNamesAreTheWiresIdempotencyKeys(t *testing.T) {
	doc, err := fixtures.Wire()
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs struct {
			IdempotencyKey struct {
				Pattern string `json:"pattern"`
			} `json:"idempotencyKey"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(doc, &schema); err != nil {
		t.Fatal(err)
	}
	if keyForm.String() != schema.Defs.IdempotencyKey.Pattern {
		t.Errorf("keyForm is %s and the wire writes %s", keyForm, schema.Defs.IdempotencyKey.Pattern)
	}
}

// The agent's loop takes nothing while the heartbeat's last answer orders a drain, and takes again
// once an answer lifts it.
func TestTheAgentsLoopDrainsOnTheHeartbeatsOrder(t *testing.T) {
	drain := true
	var mu sync.Mutex
	api := newBeats(t, func(beatRequest) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		return http.StatusOK, beatAnswered(time.Now(), fmt.Sprintf(`"drain":%t,"cancel":[]`, drain))
	})
	var readies int
	root := t.TempDir()
	a := agentOf(t, &readies, api.srv.URL, root)
	results, err := OpenResults(root, a.Config.Runner, &laterBus{})
	if err != nil {
		t.Fatal(err)
	}
	loop, beat, _ := a.parts(results, nil, nil)
	if loop.Draining() {
		t.Error("the loop drains before any heartbeat was answered")
	}
	if err := beat.Beat(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !loop.Draining() {
		t.Error("the loop takes work after the heartbeat's answer ordered a drain")
	}
	mu.Lock()
	drain = false
	mu.Unlock()
	if err := beat.Beat(t.Context()); err != nil {
		t.Fatal(err)
	}
	if loop.Draining() {
		t.Error("the loop still drains after an answer lifted the order")
	}
}

// A heartbeat that is slow, gathering its keys or waiting on the API, is not a clock out: the
// time it took is transit, and received_at still falls between sending and hearing the answer.
func TestASlowHeartbeatIsNotReadAsAClockOut(t *testing.T) {
	var slowAPI bool
	var mu sync.Mutex
	api := newBeats(t, func(beatRequest) (int, string) {
		mu.Lock()
		slow := slowAPI
		mu.Unlock()
		if slow {
			// received_at stamped as late as an API can stamp it, after the wait.
			time.Sleep(1500 * time.Millisecond)
			return http.StatusOK, beatAnswered(time.Now(), `"drain":false,"cancel":[]`)
		}
		return answered(beatRequest{})
	})
	h, log := heartbeat(t, api.srv.URL)
	h.Holding = func() []string {
		mu.Lock()
		slow := !slowAPI
		mu.Unlock()
		if slow {
			time.Sleep(1500 * time.Millisecond)
		}
		return nil
	}
	if err := h.Beat(t.Context()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	slowAPI = true
	mu.Unlock()
	if err := h.Beat(t.Context()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(log.String(), "clock") {
		t.Errorf("a heartbeat slow on a host whose clock is right said:\n%s", log)
	}
}

// A revocation that follows a drain is said as the drain was, with its reason and its grace: the
// order stood throughout, and what changed is what the host's journal is read for.
func TestARevocationAfterADrainIsSaid(t *testing.T) {
	answer := `"drain":true,"reason":"pool zone=dmz is being retired","cancel":[]`
	var mu sync.Mutex
	api := newBeats(t, func(beatRequest) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		return http.StatusOK, beatAnswered(time.Now(), answer)
	})
	h, log := heartbeat(t, api.srv.URL)
	for _, then := range []string{
		`"drain":true,"reason":"pool zone=dmz is being retired","cancel":[]`,
		`"drain":true,"reason":"the host was compromised","results_accepted_until":"2026-09-24T12:00:00Z","cancel":[]`,
		`"drain":true,"reason":"the host was compromised","results_accepted_until":"2026-09-24T12:00:00Z","cancel":[]`,
	} {
		mu.Lock()
		answer = then
		mu.Unlock()
		if err := h.Beat(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(log.String(), "orders this runner to drain"); n != 2 {
		t.Errorf("the order was said %d times, want once for the drain and once for the revocation:\n%s", n, log)
	}
	for _, want := range []string{"the host was compromised", "accepted until 2026-09-24T12:00:00Z"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the log does not say %q:\n%s", want, log)
		}
	}
}

// One heartbeat names every key of a host holding as many tasks as AGK_RUNNER_CONCURRENCY allows,
// and no more than the page's "at most 4,096", which is what the API takes.
func TestAHeartbeatNamesAsManyKeysAsTheAPITakesAndAHostHolds(t *testing.T) {
	if beatMaxTasks != 4096 {
		t.Errorf("a heartbeat names up to %d keys, and the page and the API say 4,096", beatMaxTasks)
	}
	if beatMaxTasks < MaxConcurrency {
		t.Errorf("a heartbeat names up to %d keys, and a host may hold %d", beatMaxTasks, MaxConcurrency)
	}
}

// The agent's heartbeat reports the concurrency it was configured with and stops through its
// driver, which is what reaches a container a cancel names.
func TestTheAgentsHeartbeatIsWiredToItsDriverAndConcurrency(t *testing.T) {
	api := newBeats(t, answered)
	var readies int
	root := t.TempDir()
	a := agentOf(t, &readies, api.srv.URL, root)
	a.Config.Concurrency = 7
	results, err := OpenResults(root, a.Config.Runner, &laterBus{})
	if err != nil {
		t.Fatal(err)
	}
	_, beat, stops := a.parts(results, nil, nil)
	if err := beat.Beat(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := api.heard()[0].Concurrency; got != 7 {
		t.Errorf("the heartbeat reports a concurrency of %d, and the agent holds 7 tasks at once", got)
	}
	// Through the agent's Stops, which a stop heard on the bus goes through too, so that the
	// two channels ask the driver once between them.
	if s, ok := beat.Stopper.(*Stops); !ok || s != stops {
		t.Errorf("a cancel is stopped through %T, not the agent's Stops", beat.Stopper)
	}
	if d, ok := stops.Stopper.(*driver.Docker); !ok || d != a.Driver {
		t.Errorf("the agent's Stops stop through %T, not the agent's driver", stops.Stopper)
	}
}
