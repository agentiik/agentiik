package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// The take loop against a real bus: a pool of its own on the shared NATS, whose consumer the test
// made the way the control plane makes one, with AckWait cut short so that what follows it is seen
// in seconds.

// aPool is one pool on the shared bus: the control plane's connection, which publishes, a runner's
// connection, which takes, and the pool's consumer, which says what the bus still means to hand
// out.
type aPool struct {
	name     string
	control  *bus.Bus
	runner   *bus.Bus
	consumer jetstream.Consumer
}

// aPoolOnTheBus makes a pool of its own, so that nothing another test publishes or takes reaches
// it, with its consumer's AckWait set to ackWait.
func aPoolOnTheBus(t *testing.T, ackWait time.Duration) *aPool {
	t.Helper()
	url := os.Getenv("AGENTIIK_TEST_BUS_URL")
	if url == "" {
		t.Skip("no NATS on this machine: set AGENTIIK_TEST_BUS_URL")
	}
	control, err := bus.Open(t.Context(), bus.Options{URL: url})
	if err != nil {
		t.Skipf("the bus at AGENTIIK_TEST_BUS_URL could not be reached: %s", err)
	}
	t.Cleanup(control.Close)
	name := "loop-" + strings.ToLower(ulid.New()[16:])

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := js.CreateOrUpdateConsumer(t.Context(), bus.Stream, jetstream.ConsumerConfig{
		Durable:       bus.Durable(name),
		FilterSubject: bus.Subject(name),
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ackWait,
		MaxDeliver:    -1,
		MaxAckPending: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { js.DeleteConsumer(context.Background(), bus.Stream, bus.Durable(name)) })

	runner, err := bus.OpenRunner(bus.Options{URL: url, Name: "runner-dmz-02"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runner.Close)
	return &aPool{name: name, control: control, runner: runner, consumer: consumer}
}

// publish puts a task message on the pool's queue, as the controller does.
func (p *aPool) publish(t *testing.T, m bus.TaskMessage) {
	t.Helper()
	m.RunsOn = []string{"pool=" + p.name}
	if err := p.control.Publish(t.Context(), m); err != nil {
		t.Fatal(err)
	}
}

// outstanding is what the pool's consumer still means to hand out: the messages nobody has been
// handed yet, and those handed and not acknowledged.
func (p *aPool) outstanding(t *testing.T) (waiting, unacknowledged int) {
	t.Helper()
	info, err := p.consumer.Info(context.Background())
	if err != nil {
		t.Error(err)
		return -1, -1
	}
	return int(info.NumPending), info.NumAckPending
}

// take is the runner asking for one message, as the loop asks.
func (p *aPool) take(t *testing.T, wait time.Duration) (bus.Taken, bool) {
	t.Helper()
	taken, err := p.runner.Take(t.Context(), p.name, 1, wait)
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) == 0 {
		return bus.Taken{}, false
	}
	return taken[0], true
}

// endedQueue is the runner's bus, remembering every ending it answered a message with.
type endedQueue struct {
	*bus.Bus

	mu    sync.Mutex
	ended []bus.TaskResult
}

func (q *endedQueue) Ended(ctx context.Context, t bus.Taken, r bus.TaskResult) error {
	q.mu.Lock()
	q.ended = append(q.ended, r)
	q.mu.Unlock()
	return q.Bus.Ended(ctx, t, r)
}

func (q *endedQueue) all() []bus.TaskResult {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]bus.TaskResult(nil), q.ended...)
}

// anAPI answers each redemption with what answer says for it, and counts them.
type anAPI struct {
	url    string
	answer func(n int, taskID string) (int, any)

	mu    sync.Mutex
	asked map[string]int
}

func anAPIAnswering(t *testing.T, answer func(n int, taskID string) (int, any)) *anAPI {
	t.Helper()
	a := &anAPI{answer: answer, asked: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != redeemPath {
			http.NotFound(w, r)
			return
		}
		var req redemptionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		a.mu.Lock()
		a.asked[req.TaskID]++
		n := a.asked[req.TaskID]
		a.mu.Unlock()
		status, body := a.answer(n, req.TaskID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	a.url = srv.URL
	return a
}

func (a *anAPI) redemptions(taskID string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asked[taskID]
}

// refusedWith is the body the API refuses with.
func refusedWith(why string) map[string]string { return map[string]string{"error": why} }

// progressed is a bus that takes every progress message.
type progressed struct {
	mu   sync.Mutex
	seen []bus.TaskProgress
}

func (p *progressed) Progress(_ context.Context, pr bus.TaskProgress) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, pr)
	return nil
}

func (p *progressed) all() []bus.TaskProgress {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bus.TaskProgress(nil), p.seen...)
}

// looping is a carrier's host taking from a pool of its own and redeeming at an API.
type looping struct {
	*carrying
	pool     *aPool
	queue    *endedQueue
	api      *anAPI
	loop     *Loop
	progress *progressed
}

func aLoop(t *testing.T, c *carrying, p *aPool, api *anAPI) *looping {
	t.Helper()
	q := &endedQueue{Bus: p.runner}
	pr := &progressed{}
	progress := NewProgress("runner-dmz-02", pr, func(s string) { t.Log(s) })
	c.carrier.Endings.Next = progress
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); progress.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	l := &looping{
		carrying: c, pool: p, queue: q, progress: pr,
		loop: &Loop{
			Runner: "runner-dmz-02", Pool: p.name, Concurrency: 2,
			Queue: q, Holder: c.carrier.Driver.(*driver.Docker), Carrier: c.carrier,
			Assembly: Assembly{WorkRoot: c.root},
			Progress: progress,
			Log:      func(s string) { t.Log(s) },
			Wait:     2 * time.Second,
			Retry:    50 * time.Millisecond,
		},
	}
	if api != nil {
		l.redeemAt(t, api)
	}
	return l
}

// redeemAt has the loop redeem at api.
func (l *looping) redeemAt(t *testing.T, api *anAPI) {
	t.Helper()
	client, err := NewClient(api.url, credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.api, l.loop.Redeemer = api, client
}

// task is a task of finance the API would redeem with r, published on the pool.
func (l *looping) task(t *testing.T, with func(*bus.TaskMessage)) (bus.TaskMessage, Redemption) {
	t.Helper()
	m, r := l.store.taskFor(t, nil, map[string]file{"agentiik.yaml": {"version: 1\n", "0644"}}, nil)
	m.TaskID = ulid.New()
	if with != nil {
		with(&m)
	}
	r.TaskID, r.ExpiresAt = m.TaskID, m.Deadline
	l.pool.publish(t, m)
	return m, r
}

// carryOne takes one message and answers it, as one slot of the loop does.
func (l *looping) carryOne(t *testing.T) bus.TaskMessage {
	t.Helper()
	taken, ok := l.pool.take(t, 5*time.Second)
	if !ok {
		t.Fatal("the pool handed out nothing")
	}
	l.loop.carry(t.Context(), taken)
	return taken.Task
}

// containersOf are the containers the host created for one key.
func (l *looping) containersOf(key string) int {
	n := 0
	for _, c := range l.daemon.Created() {
		if c.Labels[driver.LabelTask] == key {
			n++
		}
	}
	return n
}

// Each answer a redemption can get leads to what the page's table names, read on the pool's
// consumer: a message acknowledged is one the bus will never hand out again, and one put back is
// handed out again at once.
func TestEachRedemptionAnswerLeadsToTheBusActionTheTableNames(t *testing.T) {
	type outcome struct {
		// acknowledged says the message is off the queue, put back that it is handed out
		// again at once; neither is a message left to come round after AckWait.
		acknowledged, putBack bool
		// state is the one result reported, or TaskPending for none.
		state      agk.TaskState
		containers int
		redeemed   int
	}
	unusable := func(r Redemption) Redemption { r.Uploads = Uploads{}; return r }
	for _, row := range []struct {
		name   string
		with   func(*bus.TaskMessage)
		answer func(n int, r Redemption) (int, any)
		want   outcome
	}{
		{"200: acknowledged, run and reported", nil,
			func(int, Redemption) (int, any) { return 0, nil },
			outcome{acknowledged: true, state: agk.TaskSucceeded, containers: 1, redeemed: 1}},
		{"403: put back for another runner of the pool", nil,
			func(int, Redemption) (int, any) {
				return http.StatusForbidden, refusedWith("this runner is draining and takes nothing new")
			},
			outcome{putBack: true, redeemed: 1}},
		{"409: acknowledged, and nothing started or reported", nil,
			func(int, Redemption) (int, any) {
				return http.StatusConflict, refusedWith("the task is held by another runner")
			},
			outcome{acknowledged: true, redeemed: 1}},
		{"422: reported as reaching no container, then acknowledged", nil,
			func(int, Redemption) (int, any) {
				return http.StatusUnprocessableEntity, refusedWith("the namespace declares no secret billing")
			},
			outcome{acknowledged: true, state: agk.TaskFailed, redeemed: 1}},
		{"a 200 the task cannot be run on: reported as reaching no container, then acknowledged", nil,
			func(_ int, r Redemption) (int, any) { return http.StatusOK, unusable(r) },
			outcome{acknowledged: true, state: agk.TaskFailed, redeemed: 1}},
		{"a 200 for a message no runner can run: reported as reaching no container, then acknowledged",
			func(m *bus.TaskMessage) {
				// The deadline is near, so that assembling again until it passes would
				// report the task timed_out and not failed.
				m.Network = "bridge"
				m.Deadline = time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339Nano)
			},
			func(int, Redemption) (int, any) { return 0, nil },
			outcome{acknowledged: true, state: agk.TaskFailed, redeemed: 1}},
		{"a 503, then a 200: redeemed again, then run", nil,
			func(n int, r Redemption) (int, any) {
				if n == 1 {
					return http.StatusServiceUnavailable, refusedWith("the database is not answering")
				}
				return 0, nil
			},
			outcome{acknowledged: true, state: agk.TaskSucceeded, containers: 1, redeemed: 2}},
	} {
		t.Run(row.name, func(t *testing.T) {
			l := aLoop(t, carrier(t, nil), aPoolOnTheBus(t, 30*time.Second), nil)
			var r Redemption
			l.redeemAt(t, anAPIAnswering(t, func(n int, _ string) (int, any) {
				status, body := row.answer(n, r)
				if status == 0 {
					return http.StatusOK, r
				}
				return status, body
			}))
			var m bus.TaskMessage
			m, r = l.task(t, row.with)

			l.carryOne(t)

			waiting, unacknowledged := l.pool.outstanding(t)
			switch {
			case row.want.acknowledged && waiting+unacknowledged != 0:
				t.Errorf("the message is still on the queue: %d waiting and %d handed out and not acknowledged", waiting, unacknowledged)
			case row.want.putBack:
				again, ok := l.pool.take(t, time.Second)
				if !ok || again.Task.TaskID != m.TaskID {
					t.Errorf("the message put back was not handed out again at once: %d waiting and %d unacknowledged", waiting, unacknowledged)
				}
			}
			results := l.bus.all()
			switch {
			case row.want.state == agk.TaskPending && len(results) != 0:
				t.Errorf("%d results were reported, and this answer reports none: %+v", len(results), results)
			case row.want.state != agk.TaskPending && (len(results) != 1 || results[0].State != row.want.state || results[0].TaskID != m.TaskID):
				t.Errorf("the results reported are %+v, want one %s for %s", results, row.want.state, m.TaskID)
			case row.want.state != agk.TaskPending && row.want.containers == 0 && !results[0].StartedAt.IsZero():
				t.Errorf("a task that reached no container is reported as having started at %s", results[0].StartedAt)
			}
			if got := l.containersOf(m.IdempotencyKey); got != row.want.containers {
				t.Errorf("%d containers were created, want %d", got, row.want.containers)
			}
			if got := l.api.redemptions(m.TaskID); got != row.want.redeemed {
				t.Errorf("the grant was redeemed %d times, want %d", got, row.want.redeemed)
			}
			if held := l.loop.Held(); len(held) != 0 {
				t.Errorf("the loop still names %v once the message is answered", held)
			}
			// Whatever the answer, the host holds nothing of the key in memory once the
			// message is answered, so that a later delivery is written down and not
			// refused as one in flight.
			if err := l.loop.Holder.Hold(agk.TaskID(m.IdempotencyKey)); err != nil && row.want.containers == 0 {
				t.Errorf("the key cannot be written down again once the message is answered: %s", err)
			}
		})
	}
}

// A host with room for several tasks takes one as soon as it is on the queue, and does not hold it,
// unacknowledged and handed to nobody else, until its take would have filled the room or its long
// poll run out.
func TestAHostWithRoomForSeveralTakesATaskAsSoonAsItIsThere(t *testing.T) {
	var (
		mu       sync.Mutex
		redeemed time.Time
	)
	api := anAPIAnswering(t, func(int, string) (int, any) {
		mu.Lock()
		if redeemed.IsZero() {
			redeemed = time.Now()
		}
		mu.Unlock()
		return http.StatusConflict, refusedWith("the task is held by another runner")
	})
	l := aLoop(t, carrier(t, nil), aPoolOnTheBus(t, 30*time.Second), api)
	l.loop.Concurrency, l.loop.Wait = 4, 10*time.Second

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- l.loop.Run(ctx) }()
	time.Sleep(500 * time.Millisecond)
	published := time.Now()
	m, _ := l.task(t, nil)
	for deadline := time.Now().Add(15 * time.Second); l.api.redemptions(m.TaskID) == 0 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if redeemed.IsZero() {
		t.Fatal("the task was never redeemed")
	}
	if took := redeemed.Sub(published); took > 2*time.Second {
		t.Errorf("a host with four free slots redeemed a task %s after it was published", took)
	}
}

// A runner the API refuses every task, because it is draining, puts each back held back a moment,
// and does not take it straight back into another of its free slots: a pool with no other runner
// would otherwise spin on the message as fast as the API answers.
func TestARunnerRefusedEveryTaskDoesNotSpinOnTheQueue(t *testing.T) {
	api := anAPIAnswering(t, func(int, string) (int, any) {
		return http.StatusForbidden, refusedWith("this runner is draining and takes nothing new")
	})
	l := aLoop(t, carrier(t, nil), aPoolOnTheBus(t, 30*time.Second), api)
	l.loop.Concurrency, l.loop.Retry = 4, 500*time.Millisecond
	l.loop.Log = nil
	m, _ := l.task(t, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := l.loop.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := l.api.redemptions(m.TaskID); n < 2 || n > 6 {
		t.Errorf("in two seconds the grant was redeemed %d times, where holding each put back for half a second allows four or five, however many slots are free", n)
	}
	if n := l.containersOf(m.IdempotencyKey); n != 0 {
		t.Errorf("%d containers were created for a task every redemption refused", n)
	}
}

// "No answer at all, a 401 or any other 5xx: keeps the key, names it in its heartbeat and redeems
// again, acknowledging nothing. Once the deadline the message carries has passed, no answer can
// come, since the grant expires with it: it reports the task timed_out with no container ran, then
// acknowledges."
func TestAMessageWhoseDeadlinePassesDuringRedemptionIsReportedTimedOut(t *testing.T) {
	l := aLoop(t, carrier(t, nil), aPoolOnTheBus(t, 30*time.Second), nil)
	var (
		mu          sync.Mutex
		whileAsking []string
	)
	l.redeemAt(t, anAPIAnswering(t, func(int, string) (int, any) {
		waiting, unacknowledged := l.pool.outstanding(t)
		mu.Lock()
		whileAsking = append(whileAsking, strings.Join(l.loop.Held(), ",")+" "+strings.Repeat("u", unacknowledged)+strings.Repeat("w", waiting))
		mu.Unlock()
		return http.StatusUnauthorized, refusedWith("the grant opens nothing")
	}))
	m, _ := l.task(t, func(m *bus.TaskMessage) {
		m.Deadline = time.Now().Add(1500 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	})

	l.carryOne(t)

	mu.Lock()
	defer mu.Unlock()
	if len(whileAsking) < 2 {
		t.Fatalf("the grant was redeemed %d times before its deadline, and a 401 is asked again", len(whileAsking))
	}
	for i, seen := range whileAsking {
		if want := m.IdempotencyKey + " u"; seen != want {
			t.Errorf("at redemption %d the loop named its keys and the queue held its message as %q, want %q: the key kept and nothing acknowledged", i+1, seen, want)
		}
	}
	results := l.bus.all()
	if len(results) != 1 || results[0].State != agk.TaskTimedOut || !results[0].StartedAt.IsZero() || results[0].ExitCode != nil || results[0].TaskID != m.TaskID {
		t.Fatalf("the results reported are %+v, want one timed_out that reached no container", results)
	}
	if waiting, unacknowledged := l.pool.outstanding(t); waiting+unacknowledged != 0 {
		t.Errorf("the message is still on the queue once its timeout was reported: %d waiting and %d unacknowledged", waiting, unacknowledged)
	}
	if n := l.containersOf(m.IdempotencyKey); n != 0 {
		t.Errorf("%d containers were created for a task whose grant never redeemed", n)
	}
}

// The requeue of a task the heartbeat declared lost while its host was only cut off reaches the
// host that ran it to its end. It is answered with the ending the host recorded, under the
// requeue's own task_id, and acknowledged; nothing is redeemed and nothing runs.
func TestARequeueReachingTheHostThatHoldsItsEndingIsAnsweredWithItAndRunsNothing(t *testing.T) {
	var answers sync.Map
	api := anAPIAnswering(t, func(_ int, taskID string) (int, any) {
		r, _ := answers.Load(taskID)
		return http.StatusOK, r
	})
	l := aLoop(t, carrier(t, nil), aPoolOnTheBus(t, 30*time.Second), api)
	first, r := l.task(t, nil)
	answers.Store(first.TaskID, r)
	l.carryOne(t)
	if results := l.bus.all(); len(results) != 1 || results[0].State != agk.TaskSucceeded {
		t.Fatalf("the first dispatch was reported %+v", results)
	}

	requeue, _ := l.task(t, nil)
	if requeue.IdempotencyKey != first.IdempotencyKey || requeue.TaskID == first.TaskID {
		t.Fatal("the requeue is not a new dispatch of the same key")
	}
	l.carryOne(t)

	ended := l.queue.all()
	if len(ended) != 1 {
		t.Fatalf("the requeue was answered from the record %d times", len(ended))
	}
	want := l.bus.all()[0]
	want.TaskID = requeue.TaskID
	want.Usage = nil
	if got := ended[0]; got.TaskID != want.TaskID || got.State != want.State || !slices.Equal(got.Outputs, want.Outputs) || !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("the requeue was answered with %+v, want the recorded ending %+v", got, want)
	}
	if n := l.api.redemptions(requeue.TaskID); n != 0 {
		t.Errorf("the requeue's grant was redeemed %d times, and a key answered from the record binds nothing", n)
	}
	if n := l.containersOf(first.IdempotencyKey); n != 1 {
		t.Errorf("%d containers were created for one key", n)
	}
	if waiting, unacknowledged := l.pool.outstanding(t); waiting+unacknowledged != 0 {
		t.Errorf("the requeue is still on the queue once answered: %d waiting and %d unacknowledged", waiting, unacknowledged)
	}
}

// A message whose acknowledgement comes late, because its redemption took longer than AckWait, is
// handed out again while the first delivery still holds its key. The host refuses the second
// delivery before redeeming anything, and the first runs the one container the key ever gets.
func TestARedeliveredMessageNeverStartsASecondContainer(t *testing.T) {
	const ackWait = time.Second
	var r atomic.Pointer[Redemption]
	api := anAPIAnswering(t, func(n int, _ string) (int, any) {
		if n == 1 {
			time.Sleep(3 * ackWait)
		}
		return http.StatusOK, r.Load()
	})
	l := aLoop(t, carrier(t, nil), aPoolOnTheBus(t, ackWait), api)
	holder := &countingHolder{Holder: l.loop.Holder}
	l.loop.Holder = holder
	m, redemption := l.task(t, nil)
	r.Store(&redemption)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- l.loop.Run(ctx) }()
	for deadline := time.Now().Add(20 * time.Second); len(l.bus.all()) == 0 && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	// Long enough for a second container, had anything started one.
	time.Sleep(2 * ackWait)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if holder.inFlight.Load() == 0 {
		t.Fatal("no delivery was refused as one in flight, so nothing here tested a redelivery")
	}
	if n := l.containersOf(m.IdempotencyKey); n != 1 {
		t.Errorf("%d containers were created for a key delivered more than once", n)
	}
	if n := l.api.redemptions(m.TaskID); n != 1 {
		t.Errorf("the grant was redeemed %d times, and a delivery of a key in flight redeems nothing", n)
	}
	if results := l.bus.all(); len(results) != 1 || results[0].State != agk.TaskSucceeded {
		t.Errorf("the results reported are %+v, want the one success", results)
	}
	if waiting, unacknowledged := l.pool.outstanding(t); waiting+unacknowledged != 0 {
		t.Errorf("the message is still on the queue: %d waiting and %d unacknowledged", waiting, unacknowledged)
	}
}

// A task that runs tells its running and its publishing, under the dispatch it was taken as.
func TestATaskThatRunsSaysItIsRunningAndThenPublishing(t *testing.T) {
	var r atomic.Pointer[Redemption]
	api := anAPIAnswering(t, func(int, string) (int, any) { return http.StatusOK, r.Load() })
	l := aLoop(t, carrier(t, nil), aPoolOnTheBus(t, 30*time.Second), api)
	m, redemption := l.task(t, nil)
	r.Store(&redemption)
	l.carryOne(t)

	want := []bus.TaskProgress{
		{TaskID: m.TaskID, IdempotencyKey: m.IdempotencyKey, Runner: "runner-dmz-02", Progress: agk.TaskRunning},
		{TaskID: m.TaskID, IdempotencyKey: m.IdempotencyKey, Runner: "runner-dmz-02", Progress: agk.TaskPublishing},
	}
	var got []bus.TaskProgress
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if got = l.progress.all(); len(got) >= len(want) {
			break
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("the progress published is %+v, want %+v", got, want)
	}
}

// Progress never holds the driver up: a bus that does not answer leaves Observe returning at once,
// and a key nobody is carrying says nothing.
func TestProgressNeverBlocksTheDriverAndSaysNothingOfAKeyNobodyCarries(t *testing.T) {
	stuck := make(chan struct{})
	defer close(stuck)
	p := NewProgress("runner-dmz-02", progressFunc(func(context.Context, bus.TaskProgress) error { <-stuck; return nil }), nil)
	go p.Run(t.Context())
	m := bus.TaskMessage{TaskID: ulid.New(), IdempotencyKey: "01JMZ8V1P9C4/invoice/1"}
	p.Observe(t.Context(), driver.Event{Task: agk.TaskID(m.IdempotencyKey), State: agk.TaskRunning})
	if len(p.queue) != 0 {
		t.Error("a transition of a key nobody carries was queued")
	}
	done := p.carrying(m)
	defer done()

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for range 2 * progressQueued {
			p.Observe(t.Context(), driver.Event{Task: agk.TaskID(m.IdempotencyKey), State: agk.TaskRunning})
		}
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Observe blocked on a bus that does not answer")
	}
}

// countingHolder is the host's record, counting the deliveries it refused as in flight.
type countingHolder struct {
	Holder
	inFlight atomic.Int32
}

func (h *countingHolder) Hold(id agk.TaskID) error {
	err := h.Holder.Hold(id)
	if errors.Is(err, driver.ErrTaskInFlight) {
		h.inFlight.Add(1)
	}
	return err
}

type progressFunc func(context.Context, bus.TaskProgress) error

func (f progressFunc) Progress(ctx context.Context, p bus.TaskProgress) error { return f(ctx, p) }
