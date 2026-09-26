package runner

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// Draining and revocation, as a runner obeys them: "take nothing new; finish what is held", and a
// revoked runner gone once what it held is answered.

// agentServing is one agent serving against an installation, and what it said.
type agentServing struct {
	served chan error
	cancel context.CancelFunc

	mu  sync.Mutex
	log strings.Builder
}

func (a *agentServing) said() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.log.String()
}

// ended answers whether Serve has returned, and what it returned.
func (a *agentServing) ended() (bool, error) {
	select {
	case err := <-a.served:
		a.served <- err
		return true, err
	default:
		return false, nil
	}
}

// anAgentServing serves one runner of in on c's driver, concurrency tasks at once, with heartbeats
// and takes short enough that an order is heard in a moment. It is stopped as the test ends.
func anAgentServing(t *testing.T, in *served, c *carrying, concurrency int) *agentServing {
	t.Helper()
	client, err := NewClient(in.url, in.credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	a := &agentServing{served: make(chan error, 1), cancel: cancel}
	go func() {
		a.served <- Serve(ctx, Agent{
			Config: Config{
				API: in.url, Runner: in.runner, Pool: in.poolName, Concurrency: concurrency,
				WorkDir: c.root, Credential: in.credential, Labels: []string{"zone=dmz"},
			},
			Driver: c.carrier.Driver.(*driver.Docker), Client: client, Endings: c.carrier.Endings,
			Log:   func(s string) { a.mu.Lock(); a.log.WriteString(s + "\n"); a.mu.Unlock() },
			every: 200 * time.Millisecond, wait: 300 * time.Millisecond,
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-a.served
		a.served <- nil
		t.Log(a.said())
	})
	return a
}

// ordered drains or revokes in's runner, as the API's routes do.
func (in *served) ordered(t *testing.T, order func(ctx context.Context, w *db.Wide) error) {
	t.Helper()
	if err := in.pool.Installation(t.Context(), db.RunnerInventory, order); err != nil {
		t.Fatal(err)
	}
}

// reported is the state the runner's last heartbeat said it was in.
func (in *served) reported(t *testing.T) string {
	t.Helper()
	var state *string
	if err := in.conn.QueryRow(context.Background(),
		`select reported_state from runners where id = $1`, in.runner).Scan(&state); err != nil {
		t.Error(err)
	}
	if state == nil {
		return ""
	}
	return *state
}

// resultOf answers whether the last result the runner published is m's, in state.
func (in *served) resultOf(t *testing.T, m bus.TaskMessage, state string) func() bool {
	return func() bool {
		said := in.lastResult(t)
		return said["task_id"] == m.TaskID && said["state"] == state
	}
}

// heldUntilReleased is a fake daemon's container that runs until the test releases its key, or the
// driver stops it.
type heldUntilReleased struct {
	mu       sync.Mutex
	released map[string]chan struct{}
}

func (h *heldUntilReleased) of(key string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released == nil {
		h.released = map[string]chan struct{}{}
	}
	if h.released[key] == nil {
		h.released[key] = make(chan struct{})
	}
	return h.released[key]
}

func (h *heldUntilReleased) run(c dockertest.Container) (int, error) {
	select {
	case <-h.of(c.Labels[driver.LabelTask]):
		return 0, nil
	case <-c.Signalled():
		return 143, nil
	}
}

func startedFor(c *carrying, key string) func() bool {
	return func() bool {
		for _, made := range c.daemon.Created() {
			if made.Labels[driver.LabelTask] == key {
				return true
			}
		}
		return false
	}
}

// A drain ordered while a task runs lets it finish and publishes its result, takes nothing
// dispatched afterwards, and leaves the runner up, idle and reporting draining: "results are
// accepted as usual; the runner takes nothing new but stays up".
func TestADrainOrderedMidTaskFinishesItAndTakesNothingMore(t *testing.T) {
	in := anInstallationServing(t)
	var held heldUntilReleased
	c := carrier(t, held.run)
	a := anAgentServing(t, in, c, 1)

	first := in.dispatch(t, "invoice")
	eventually(t, "the first task's container starting", startedFor(c, first.IdempotencyKey))
	in.ordered(t, func(ctx context.Context, w *db.Wide) error {
		_, err := w.Drain(ctx, in.runner, "admin", "pool zone=dmz is being retired", in.now())
		return err
	})
	eventually(t, "the runner reporting draining", func() bool { return in.reported(t) == "draining" })

	second := in.dispatch(t, "render")
	close(held.of(first.IdempotencyKey))
	eventually(t, "the first task's result", in.resultOf(t, first, "succeeded"))

	// A few heartbeats and takes' worth, for a runner that took the second task to have
	// redeemed it.
	time.Sleep(2 * time.Second)
	if startedFor(c, second.IdempotencyKey)() {
		t.Error("a draining runner started a container for a task dispatched after the order")
	}
	if got, want := in.state(t, second.TaskID), "dispatched -"; got != want {
		t.Errorf("the task dispatched after the order reads %q, want %q: bound to nobody", got, want)
	}
	if waiting, unacknowledged := in.queue(t); waiting != 1 || unacknowledged != 0 {
		t.Errorf("the queue holds %d waiting and %d handed out, want the second task waiting for another runner", waiting, unacknowledged)
	}
	if ended, err := a.ended(); ended {
		t.Errorf("a drained runner stopped serving once idle: %v", err)
	}
	if got := in.reported(t); got != "draining" {
		t.Errorf("an idle drained runner reports %q, want draining", got)
	}
}

// "Revoking a credential never destroys work already done": a runner revoked while a task runs
// finishes it and publishes its result inside the grace, and then, holding nothing, ends with
// ErrRevoked, which the agent exits on with the status the unit keeps systemd from restarting.
func TestARevokedRunnerPublishesWhatItHeldThenEnds(t *testing.T) {
	in := anInstallationServing(t)
	var held heldUntilReleased
	c := carrier(t, held.run)
	// Two slots, so that the loop has room to look while the task runs, and a runner that took
	// room for having nothing in hand would end with the task unanswered.
	a := anAgentServing(t, in, c, 2)

	m := in.dispatch(t, "invoice")
	eventually(t, "the task's container starting", startedFor(c, m.IdempotencyKey))
	in.ordered(t, func(ctx context.Context, w *db.Wide) error {
		_, err := w.Revoke(ctx, in.runner, "admin", "host decommissioned", in.now(), time.Hour)
		return err
	})
	eventually(t, "the revocation heard", func() bool { return strings.Contains(a.said(), "It is revoked") })
	// A few passes of the loop's pause while draining.
	time.Sleep(3 * time.Second)
	if ended, err := a.ended(); ended {
		t.Fatalf("a revoked runner holding a running task stopped serving before it ended: %v", err)
	}

	close(held.of(m.IdempotencyKey))
	eventually(t, "the task's result", in.resultOf(t, m, "succeeded"))
	var err error
	eventually(t, "the revoked runner ending once it held nothing", func() bool {
		var ended bool
		ended, err = a.ended()
		return ended
	})
	if !errors.Is(err, ErrRevoked) {
		t.Errorf("a revoked runner that answered for what it held ended with %v, want ErrRevoked", err)
	}
	if left, _ := os.ReadDir(filepath.Join(c.root, ResultsDir)); len(left) != 0 {
		t.Errorf("a revoked runner ended with results still kept: %v", left)
	}
}

// A revoked runner whose grace ends while it still holds a task is answered 401 at its next
// heartbeat, and ends saying to join again, the same status: the grace is the most it is given.
func TestARevokedRunnerWhoseGraceEndsFirstEndsSayingToJoinAgain(t *testing.T) {
	// A credential accepted for longer than the grace, so that what refuses it is the grace.
	in := anInstallationRotating(t, 24*time.Hour, make(ed25519.PublicKey, ed25519.PublicKeySize))
	var held heldUntilReleased
	c := carrier(t, held.run)
	a := anAgentServing(t, in, c, 1)

	m := in.dispatch(t, "invoice")
	eventually(t, "the task's container starting", startedFor(c, m.IdempotencyKey))
	in.ordered(t, func(ctx context.Context, w *db.Wide) error {
		_, err := w.Revoke(ctx, in.runner, "admin", "host decommissioned", in.now(), time.Hour)
		return err
	})
	eventually(t, "the revocation heard", func() bool { return strings.Contains(a.said(), "It is revoked") })
	in.ahead(time.Hour + time.Minute)

	var err error
	eventually(t, "the runner ending once its grace was over", func() bool {
		var ended bool
		ended, err = a.ended()
		return ended
	})
	if !errors.Is(err, ErrCredentialRefused) || errors.Is(err, ErrRevoked) {
		t.Errorf("a runner past its grace ended with %v, want the credential refused", err)
	}
}

// A message taken as the drain order came is put back before it is redeemed, and at once, for
// another runner of the pool: nothing redeemed is nothing bound, and a runner that takes nothing
// while the order stands cannot be handed it straight back.
func TestAMessageTakenAsADrainIsOrderedIsPutBackUnredeemed(t *testing.T) {
	api := anAPIAnswering(t, func(int, string) (int, any) {
		return http.StatusConflict, refusedWith("the task is held by another runner")
	})
	l := aLoop(t, carrier(t, nil), aPoolOnTheBus(t, 30*time.Second), api)
	// A put back held back from every runner would not be handed out again within the test.
	l.loop.Retry = time.Minute
	var draining atomic.Bool
	l.loop.Draining = draining.Load
	holder := &orderedOnHold{Holder: l.loop.Holder, order: func() { draining.Store(true) }}
	l.loop.Holder = holder

	m, _ := l.task(t, nil)
	taken, ok := l.pool.take(t, 5*time.Second)
	if !ok {
		t.Fatal("the pool handed out nothing")
	}
	l.loop.carry(t.Context(), taken)

	if n := l.api.redemptions(m.TaskID); n != 0 {
		t.Errorf("a message taken as the drain came was redeemed %d times", n)
	}
	if !holder.released(agk.TaskID(m.IdempotencyKey)) {
		t.Error("the key written down for the message was not let go of")
	}
	again, ok := l.pool.take(t, 2*time.Second)
	if !ok {
		t.Fatal("the message put back was not handed out again at once")
	}
	if again.Task.TaskID != m.TaskID {
		t.Errorf("the pool handed out %s, want the message put back", again.Task.TaskID)
	}
	again.Again()
	if len(l.loop.Held()) != 0 {
		t.Errorf("the loop still names %v", l.loop.Held())
	}
}

// A key an earlier agent on this host took may be bound here, its container still running, and the
// API answers its holder's redemption while it drains: a drain ordered as its message comes round
// again does not put it back for runners that would each be refused it.
func TestADrainDoesNotPutBackAKeyAnEarlierAgentHeld(t *testing.T) {
	api := anAPIAnswering(t, func(int, string) (int, any) {
		return http.StatusConflict, refusedWith("the task is over")
	})
	l := aLoop(t, carrier(t, nil), aPoolOnTheBus(t, 30*time.Second), api)
	l.loop.Draining = func() bool { return true }
	m, _ := l.task(t, nil)
	earlier := []agk.TaskID{agk.TaskID(m.IdempotencyKey)}
	// The agent's own wiring of what the driver's record listed.
	loop, _, _ := Agent{Driver: l.carrier.Driver.(*driver.Docker)}.parts(l.carrier.Results, earlier, nil)
	l.loop.HeldBefore = loop.HeldBefore
	if l.loop.HeldBefore == nil || !l.loop.HeldBefore(m.IdempotencyKey) || l.loop.HeldBefore(string(storeRun)+"/other/1") {
		t.Fatal("the agent's loop does not know the keys an earlier agent held")
	}

	taken, ok := l.pool.take(t, 5*time.Second)
	if !ok {
		t.Fatal("the pool handed out nothing")
	}
	l.loop.carry(t.Context(), taken)
	if n := l.api.redemptions(m.TaskID); n != 1 {
		t.Errorf("a key an earlier agent held was redeemed %d times while draining, want once, as its holder may", n)
	}
}

// orderedOnHold is the host's record, with a drain ordered the moment a key is written down, which
// is the last thing before the redemption.
type orderedOnHold struct {
	Holder
	order func()

	mu  sync.Mutex
	let []agk.TaskID
}

func (o *orderedOnHold) Hold(id agk.TaskID) error {
	err := o.Holder.Hold(id)
	o.order()
	return err
}

func (o *orderedOnHold) Release(id agk.TaskID) {
	o.mu.Lock()
	o.let = append(o.let, id)
	o.mu.Unlock()
	o.Holder.Release(id)
}

func (o *orderedOnHold) released(id agk.TaskID) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, let := range o.let {
		if let == id {
			return true
		}
	}
	return false
}

// A revoked loop holding nothing goes on while a result it kept is still to publish, and ends with
// ErrRevoked once the bus has taken it: its grace is for its results to reach the controller.
func TestARevokedLoopEndsOnlyOnceEveryResultKeptIsPublished(t *testing.T) {
	l := aLoop(t, carrier(t, nil), aPoolOnTheBus(t, 30*time.Second), nil)
	l.loop.Redeemer = anAPIAnswering(t, func(int, string) (int, any) { return http.StatusForbidden, refusedWith("draining") }).client(t)
	l.bus.mu.Lock()
	l.bus.refuse = errors.New("the bus is not there")
	l.bus.mu.Unlock()
	m, _ := l.store.taskFor(t, nil, map[string]file{"agentiik.yaml": {"version: 1\n", "0644"}}, nil)
	m.TaskID = "01JMZ8V1PC7K3M0"
	if err := l.carrier.Results.Report(t.Context(), timedOut(m, "runner-dmz-02")); err == nil {
		t.Fatal("a bus refusing everything took the result")
	}
	if len(l.carrier.Results.Keys()) != 1 {
		t.Fatal("the result the bus refused was not kept")
	}
	yes := func() bool { return true }
	l.loop.Draining, l.loop.Revoked = yes, yes

	done := make(chan error, 1)
	go func() { done <- l.loop.Run(t.Context()) }()
	select {
	case err := <-done:
		t.Fatalf("a revoked loop with a result still to publish ended with %v", err)
	case <-time.After(time.Second):
	}
	l.bus.mu.Lock()
	l.bus.refuse = nil
	l.bus.mu.Unlock()
	// The loop's flush publishes it again every flushEvery.
	select {
	case err := <-done:
		if !errors.Is(err, ErrRevoked) {
			t.Errorf("a revoked loop that published everything ended with %v, want ErrRevoked", err)
		}
	case <-time.After(flushEvery + 5*time.Second):
		t.Fatal("a revoked loop that published everything it kept did not end")
	}
	if got := l.bus.all(); len(got) != 1 || got[0].TaskID != m.TaskID {
		t.Errorf("the bus took %+v, want the kept result", got)
	}
}

// A drained loop that is not revoked, holding nothing and with nothing kept, goes on: it stays up
// and idle until the order is lifted.
func TestADrainedLoopThatIsNotRevokedGoesOn(t *testing.T) {
	l := aLoop(t, carrier(t, nil), aPoolOnTheBus(t, 30*time.Second), nil)
	l.loop.Redeemer = anAPIAnswering(t, func(int, string) (int, any) { return http.StatusForbidden, refusedWith("draining") }).client(t)
	l.loop.Draining = func() bool { return true }
	l.loop.Revoked = func() bool { return false }
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	start := time.Now()
	if err := l.loop.Run(ctx); err != nil {
		t.Errorf("a drained loop ended with %v", err)
	}
	if took := time.Since(start); took < 900*time.Millisecond {
		t.Errorf("a drained loop that is not revoked ended after %s, before its context did", took)
	}
}

// client is a runner client of a.
func (a *anAPI) client(t *testing.T) *Client {
	t.Helper()
	c, err := NewClient(a.url, credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
