package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	"github.com/agentiik/agentiik/internal/bustest"
	"github.com/agentiik/agentiik/internal/dockertest"
	natsserver "github.com/nats-io/nats-server/v2/server"
)

// operated is a NATS server on the bus identity bus.NewInstallation writes, as an installation's
// is, so that a runner hears stops under the credential the API mints it and nothing wider, and the
// control plane connected to it.
type operated struct {
	issuer  *bus.Issuer
	control *bus.Bus
}

func operatorBus(t *testing.T) *operated {
	t.Helper()
	until := time.Now().Add(time.Hour)
	in, err := bus.NewInstallation(t.TempDir(), until)
	if err != nil {
		t.Fatal(err)
	}
	// Beside the accounts file, since a server resolves an include against the directory of
	// the file that includes it.
	conf := filepath.Join(filepath.Dir(in.Accounts), "nats-server.conf")
	text := fmt.Sprintf("host: 127.0.0.1\nport: -1\njetstream {\n  store_dir: %q\n}\ninclude %q\n", bustest.StoreDir(t), bus.AccountsFile)
	if err := os.WriteFile(conf, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := natsserver.ProcessConfigFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	opts.NoLog, opts.NoSigs = true, true
	server, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go server.Start()
	if !server.ReadyForConnections(10 * time.Second) {
		t.Fatal("the bus did not come up")
	}
	t.Cleanup(server.Shutdown)

	seed, err := os.ReadFile(in.AccountSeed)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := bus.NewIssuer(strings.TrimSpace(string(seed)), server.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	minted, err := issuer.ForControlPlane("controller", until)
	if err != nil {
		t.Fatal(err)
	}
	control, err := bus.Open(t.Context(), bus.Options{URL: minted.URL, Name: "controller", Credentials: &minted})
	if err != nil {
		t.Fatalf("the control plane could not connect: %s", err)
	}
	t.Cleanup(control.Close)
	return &operated{issuer: issuer, control: control}
}

// runner opens a connection as the agent does, under the credential the API mints a runner.
func (o *operated) runner(t *testing.T) *bus.Bus {
	t.Helper()
	minted, err := o.issuer.ForRunner("runner-dmz-02", "dmz", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	b, err := bus.OpenRunner(bus.Options{URL: minted.URL, Name: "runner-dmz-02", Credentials: &minted})
	if err != nil {
		t.Fatalf("the runner could not connect: %s", err)
	}
	t.Cleanup(b.Close)
	return b
}

// stop publishes a stop as the controller does.
func (o *operated) stop(t *testing.T, key string, reason graph.StopReason) {
	t.Helper()
	if err := o.control.Stop(t.Context(), graph.Stop{Task: agk.TaskID(key), Reason: reason}); err != nil {
		t.Fatal(err)
	}
}

// signalled keeps each signal a container was sent and when it reached it. Its containers ignore
// SIGTERM, as a brick that does not handle it does, so the grace runs out on each, and one that is
// never stopped ends when it is let go.
type signalled struct {
	mu       sync.Mutex
	got      map[string][]caught
	released map[string]chan struct{}
}

type caught struct {
	signal string
	at     time.Time
}

func (s *signalled) run(c dockertest.Container) (int, error) {
	key := c.Labels[driver.LabelTask]
	for {
		select {
		case signal := <-c.Signalled():
			s.mu.Lock()
			s.got[key] = append(s.got[key], caught{signal, time.Now()})
			s.mu.Unlock()
			if signal == "SIGKILL" {
				return 137, nil
			}
		case <-s.release(key):
			return 0, nil
		}
	}
}

func (s *signalled) release(key string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released[key] == nil {
		s.released[key] = make(chan struct{})
	}
	return s.released[key]
}

// of are the signals the container of key was sent, in order.
func (s *signalled) of(key string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for _, c := range s.got[key] {
		names = append(names, c.signal)
	}
	return names
}

// graced is a carrier whose driver stops a container with the grace runner.toml sets, and whose
// containers are those of signalled.
func graced(t *testing.T, toml string) (*carrying, *signalled) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.toml")
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := driver.LoadPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &signalled{got: map[string][]caught{}, released: map[string]chan struct{}{}}
	return carrierWith(t, s.run, policy), s
}

// carried is a task in flight on a carrier: its message and what Carry answered once it returned.
type carried struct {
	m    bus.TaskMessage
	done chan error
}

// start carries one task of step until its container is running.
func (c *carrying) start(t *testing.T, step agk.Step, taskID string) carried {
	t.Helper()
	m, r := c.store.taskFor(t, nil, map[string]file{"agentiik.yaml": {"version: 1\n", "0644"}}, nil)
	m.Step, m.IdempotencyKey, m.TaskID, r.TaskID = string(step), string(storeRun)+"/"+string(step)+"/1", taskID, taskID
	a, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: c.root})
	if err != nil {
		t.Fatal(err)
	}
	task := carried{m: m, done: make(chan error, 1)}
	go func() { task.done <- c.carrier.Carry(context.Background(), m, a) }()
	eventually(t, "the container of "+m.IdempotencyKey+" starting", func() bool {
		for _, made := range c.daemon.Created() {
			if made.Labels[driver.LabelTask] == m.IdempotencyKey {
				return true
			}
		}
		return false
	})
	return task
}

// ended waits for the task's Carry to return, and answers the state it was reported with.
func (c *carrying) ended(t *testing.T, task carried) agk.TaskState {
	t.Helper()
	select {
	case err := <-task.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the container of %s is still running", task.m.IdempotencyKey)
	}
	for _, r := range c.bus.all() {
		if r.TaskID == task.m.TaskID {
			return r.State
		}
	}
	t.Fatalf("nothing was reported for %s", task.m.IdempotencyKey)
	return 0
}

// holding answers the keys of tasks, as the agent's loop does for the tasks it holds.
func holding(tasks ...carried) func() []string {
	return func() []string {
		var keys []string
		for _, task := range tasks {
			keys = append(keys, task.m.IdempotencyKey)
		}
		return keys
	}
}

// A stop the control plane publishes reaches the runner over its own connection, under the
// credential the API mints it, and stops the container of the key it names and no other: SIGTERM,
// then SIGKILL once the grace runner.toml sets has run out. The heartbeat's cancel of the same key,
// which the controller repeats while the container winds down, asks nothing more of the driver, so
// the reason the stop gave is what the task is reported with.
func TestAStopOnTheBusStopsTheContainerOfItsKeyWithTheGraceOfRunnerToml(t *testing.T) {
	o := operatorBus(t)
	c, signals := graced(t, "stop_grace = \"1s\"\n")
	stopped := c.start(t, "invoice", "01JMZ8V1PC7K3M0QY4B8ZR6TDN")
	beside := c.start(t, "render", "01JMZ8V1PC7K3M0QY4B8ZR6TDP")

	s := &Stops{Stopper: c.carrier.Driver.(*driver.Docker), Holding: holding(stopped, beside), Log: func(line string) { t.Log(line) }}
	if err := s.Hear(t.Context(), o.runner(t)); err != nil {
		t.Fatalf("the runner could not listen for stops: %s", err)
	}
	o.stop(t, stopped.m.IdempotencyKey, graph.StopDeadline)
	eventually(t, "the stopped container's SIGTERM", func() bool { return len(signals.of(stopped.m.IdempotencyKey)) > 0 })
	if err := s.Stop(t.Context(), graph.Stop{Task: agk.TaskID(stopped.m.IdempotencyKey), Reason: graph.StopCancelled}); err != nil {
		t.Fatal(err)
	}

	if got := c.ended(t, stopped); got != agk.TaskTimedOut {
		t.Errorf("the task a stop for its deadline ended was reported %s, want timed_out", got)
	}
	s.Wait()
	signals.mu.Lock()
	got := slices.Clone(signals.got[stopped.m.IdempotencyKey])
	signals.mu.Unlock()
	if len(got) != 2 || got[0].signal != "SIGTERM" || got[1].signal != "SIGKILL" {
		t.Fatalf("the stopped container was sent %v, want SIGTERM then SIGKILL, once", signals.of(stopped.m.IdempotencyKey))
	}
	// The daemon counts the grace in whole seconds, and the fake one waits it out exactly.
	if waited := got[1].at.Sub(got[0].at); waited < 900*time.Millisecond || waited > 5*time.Second {
		t.Errorf("SIGKILL came %s after SIGTERM, and runner.toml's stop_grace is 1s", waited)
	}

	if sent := signals.of(beside.m.IdempotencyKey); len(sent) != 0 {
		t.Errorf("the container beside the stopped one was sent %v", sent)
	}
	close(signals.release(beside.m.IdempotencyKey))
	if got := c.ended(t, beside); got != agk.TaskSucceeded {
		t.Errorf("the task nobody stopped was reported %s", got)
	}
}

// Every runner hears every stop, and one for a key this host does not hold is passed over, even
// where a container carrying the key's label is on its daemon: nothing is asked of the driver.
func TestAStopForAKeyNotHeldDoesNothing(t *testing.T) {
	o := operatorBus(t)
	c, signals := graced(t, "stop_grace = \"1s\"\n")
	elsewhere := c.start(t, "invoice", "01JMZ8V1PC7K3M0QY4B8ZR6TDN")
	held := c.start(t, "render", "01JMZ8V1PC7K3M0QY4B8ZR6TDP")

	s := &Stops{Stopper: c.carrier.Driver.(*driver.Docker), Holding: holding(held)}
	if err := s.Hear(t.Context(), o.runner(t)); err != nil {
		t.Fatal(err)
	}
	o.stop(t, elsewhere.m.IdempotencyKey, graph.StopCancelled)
	if err := s.Stop(t.Context(), graph.Stop{Task: agk.TaskID(elsewhere.m.IdempotencyKey), Reason: graph.StopCancelled}); err != nil {
		t.Fatal(err)
	}
	// Stops are handed over in the order they arrive, so once the one published after it has
	// been sent, the first has been passed over or sent.
	o.stop(t, held.m.IdempotencyKey, graph.StopCancelled)
	eventually(t, "the held task's SIGTERM", func() bool { return len(signals.of(held.m.IdempotencyKey)) > 0 })
	time.Sleep(300 * time.Millisecond)
	if sent := signals.of(elsewhere.m.IdempotencyKey); len(sent) != 0 {
		t.Errorf("a stop for a key this host does not hold sent its container %v", sent)
	}
	close(signals.release(elsewhere.m.IdempotencyKey))
	if got := c.ended(t, elsewhere); got != agk.TaskSucceeded {
		t.Errorf("the task nobody here held was reported %s", got)
	}
	c.ended(t, held)
}

// agentiik.stops keeps nothing, so a stop published while the runner's connection was down is never
// heard. The next heartbeat's cancel carries it and the task is stopped, and once the runner hears
// the bus again, a stop for the same key and the next heartbeat naming it in cancel, both arriving
// while the container winds down, send nothing more.
func TestAStopMissedWhileTheConnectionWasDownArrivesThroughTheHeartbeat(t *testing.T) {
	o := operatorBus(t)
	c, signals := graced(t, "stop_grace = \"1s\"\n")
	task := c.start(t, "invoice", "01JMZ8V1PC7K3M0QY4B8ZR6TDN")
	key := task.m.IdempotencyKey
	s := &Stops{Stopper: c.carrier.Driver.(*driver.Docker), Holding: holding(task)}

	down := o.runner(t)
	if err := s.Hear(t.Context(), down); err != nil {
		t.Fatal(err)
	}
	down.Close()
	o.stop(t, key, graph.StopDeadline)
	time.Sleep(500 * time.Millisecond)
	if sent := signals.of(key); len(sent) != 0 {
		t.Fatalf("a stop published while the connection was down reached the container: %v", sent)
	}

	api := newBeats(t, func(beatRequest) (int, string) {
		return http.StatusOK, beatAnswered(time.Now(), fmt.Sprintf(`"drain":false,"cancel":[%q]`, key))
	})
	h, _ := heartbeat(t, api.srv.URL, key)
	h.Stopper = s
	if err := h.Beat(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.Wait()
	eventually(t, "the cancelled container's SIGTERM", func() bool { return len(signals.of(key)) > 0 })

	if err := s.Hear(t.Context(), o.runner(t)); err != nil {
		t.Fatal(err)
	}
	o.stop(t, key, graph.StopDeadline)
	if err := h.Beat(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.Wait()
	// The heartbeat says only that the controller ended the dispatch, and the driver is told
	// it was cancelled, which the deadline heard afterwards does not rewrite.
	if got := c.ended(t, task); got != agk.TaskCancelled {
		t.Errorf("the task the heartbeat cancelled was reported %s", got)
	}
	s.Wait()
	if sent := signals.of(key); !slices.Equal(sent, []string{"SIGTERM", "SIGKILL"}) {
		t.Errorf("the container was sent %v, and a key is stopped once however many times either channel names it", sent)
	}
}

// A replaced connection is heard from before the one it replaces is closed, and the subscription
// goes on with it: a stop heard on both in the overlap is sent once, and one published once the
// first is closed is heard on its replacement.
func TestStopsAreHeardOnAReplacedConnection(t *testing.T) {
	o := operatorBus(t)
	const first, second = "01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/1", "01JMZ8V1P9C4XQ7K2N4D6F8H0A/render/1"
	told := &stops{}
	s := &Stops{Stopper: told, Holding: func() []string { return []string{first, second} }}

	old := o.runner(t)
	if err := s.Hear(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	if err := s.Hear(t.Context(), o.runner(t)); err != nil {
		t.Fatal(err)
	}
	o.stop(t, first, graph.StopSiblingFailed)
	eventually(t, "the stop reaching the driver", func() bool { return len(told.stopped()) > 0 })
	time.Sleep(300 * time.Millisecond)
	s.Wait()

	old.Close()
	o.stop(t, second, graph.StopSuperseded)
	eventually(t, "the stop published after the first connection closed reaching the driver", func() bool { return len(told.stopped()) > 1 })
	time.Sleep(300 * time.Millisecond)
	s.Wait()
	want := []graph.Stop{{Task: first, Reason: graph.StopSiblingFailed}, {Task: second, Reason: graph.StopSuperseded}}
	if got := told.stopped(); !slices.Equal(got, want) {
		t.Errorf("the driver was told %v, want %v", got, want)
	}
}

// A stop the driver could not carry out is let go of, so the next one for the key, the heartbeat's,
// is sent rather than passed over as already done.
func TestAStopThatFailedIsSentAgain(t *testing.T) {
	const key = "01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/1"
	failing := &failingStopper{fail: 1}
	s := &Stops{Stopper: failing, Holding: func() []string { return []string{key} }}
	stop := graph.Stop{Task: key, Reason: graph.StopCancelled}
	if err := s.Stop(t.Context(), stop); err == nil {
		t.Fatal("a stop the driver refused answered nothing")
	}
	for range 2 {
		if err := s.Stop(t.Context(), stop); err != nil {
			t.Fatal(err)
		}
	}
	if failing.asked != 2 {
		t.Errorf("the driver was asked %d times, want the failed stop and the one after it", failing.asked)
	}
}

type failingStopper struct {
	fail, asked int
}

func (f *failingStopper) Stop(context.Context, graph.Stop) error {
	f.asked++
	if f.asked <= f.fail {
		return driver.ErrDaemonUnreachable
	}
	return nil
}

// The agent listens for stops once its bus is open: a stop the control plane publishes for a task
// it runs stops the container, and the task is reported cancelled, with nothing in the database
// that would put the key in a heartbeat's cancel.
func TestTheAgentHearsStopsOnItsBus(t *testing.T) {
	in := anInstallationServing(t)
	c := carrier(t, func(ctr dockertest.Container) (int, error) {
		select {
		case <-ctr.Signalled():
			return 143, nil
		case <-time.After(30 * time.Second):
			return 0, nil
		}
	})
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
			Log: func(s string) { t.Log(s) },
		})
	}()
	defer func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %s", err)
		}
	}()

	m := in.dispatch(t, "invoice")
	eventually(t, "the task's container starting", func() bool {
		for _, made := range c.daemon.Created() {
			if made.Labels[driver.LabelTask] == m.IdempotencyKey {
				return true
			}
		}
		return false
	})
	if err := in.control.Stop(t.Context(), graph.Stop{Task: agk.TaskID(m.IdempotencyKey), Reason: graph.StopCancelled}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the stopped task's result", func() bool {
		stream, err := in.js.Stream(context.Background(), bus.Results)
		if err != nil {
			return false
		}
		msg, err := stream.GetLastMsgForSubject(context.Background(), bus.ResultSubject(in.runner))
		if err != nil {
			return false
		}
		var said map[string]any
		return json.Unmarshal(msg.Data, &said) == nil && said["task_id"] == m.TaskID && said["state"] == "cancelled"
	})
}

// A key let go of, its redemption refused and its message put back, is forgotten with the stop sent
// for it, since the driver forgets that stop with the key: a stop for the key once it is held here
// again is sent.
func TestAKeyHeldAgainIsStoppedAgain(t *testing.T) {
	const key = "01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/1"
	var mu sync.Mutex
	held := []string{key}
	told := &stops{}
	s := &Stops{Stopper: told, Holding: func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(held) }}
	stop := graph.Stop{Task: key, Reason: graph.StopCancelled}

	if err := s.Stop(t.Context(), stop); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	held = nil
	mu.Unlock()
	if err := s.Stop(t.Context(), stop); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	held = []string{key}
	mu.Unlock()
	if err := s.Stop(t.Context(), stop); err != nil {
		t.Fatal(err)
	}
	if got := told.stopped(); len(got) != 2 {
		t.Errorf("the driver was told %v, want the stop of the key held and the stop of the key held again", got)
	}
}
