package runner

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// The host's part of the runner policy, against a fake daemon and the shared NATS.

// runUntil runs a loop until done answers true or the wait passes, then stops it.
func runUntil(t *testing.T, l *Loop, wait time.Duration, done func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan error, 1)
	go func() { stopped <- l.Run(ctx) }()
	for deadline := time.Now().Add(wait); !done() && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
}

// "AGK_RUNNER_NAMESPACES narrows one host": a task of a namespace it leaves out starts no container
// there, redeems nothing there, and is taken by the pool's other runner, which runs it.
func TestATaskOfANamespaceThisHostLeavesOutIsRunByThePoolsOtherRunner(t *testing.T) {
	var r atomic.Pointer[Redemption]
	api := anAPIAnswering(t, func(int, string) (int, any) { return http.StatusOK, r.Load() })
	pool := aPoolOnTheBus(t, 30*time.Second)
	narrowed := aLoop(t, carrier(t, nil), pool, api)
	narrowed.loop.Namespaces = []string{"ops", "team-ops"}
	var putBack atomic.Int32
	narrowed.loop.Log = func(s string) {
		if strings.Contains(s, "takes no work of namespace finance") {
			putBack.Add(1)
		}
		t.Log("narrowed: " + s)
	}
	m, redemption := narrowed.task(t, nil)
	r.Store(&redemption)

	// The narrowed runner alone first, so that it is the one handed the message.
	runUntil(t, narrowed.loop, 10*time.Second, func() bool { return putBack.Load() > 0 })
	if putBack.Load() == 0 {
		t.Fatal("the runner that leaves finance out never put its task back")
	}
	if n := api.redemptions(m.TaskID); n != 0 {
		t.Fatalf("the grant was redeemed %d times by a runner that takes no work of its namespace", n)
	}

	other := aLoop(t, carrier(t, nil), pool, api)
	runUntil(t, other.loop, 20*time.Second, func() bool { return len(other.bus.all()) > 0 })

	if n := narrowed.containersOf(m.IdempotencyKey); n != 0 {
		t.Errorf("the runner that leaves finance out created %d containers for it", n)
	}
	if n := other.containersOf(m.IdempotencyKey); n != 1 {
		t.Errorf("the pool's other runner created %d containers for the task, want 1", n)
	}
	if results := other.bus.all(); len(results) != 1 || results[0].State != agk.TaskSucceeded {
		t.Errorf("the other runner reported %+v, want the one success", results)
	}
	if results := narrowed.bus.all(); len(results) != 0 {
		t.Errorf("the runner that put the task back reported %+v", results)
	}
	if err := narrowed.loop.Holder.Hold(agk.TaskID(m.IdempotencyKey)); err != nil {
		t.Errorf("the narrowed host wrote the key down: %s", err)
	}
}

// "Of the rest": a key this host has in flight is left on the queue whatever namespace its message
// names, and is not put back for another runner the moment it comes round, since the record is read
// before the policy.
func TestAKeyInFlightIsLeftOnTheQueueBeforeItsNamespaceIsRead(t *testing.T) {
	l := aLoop(t, carrier(t, nil), aPoolOnTheBus(t, 30*time.Second), anAPIAnswering(t, func(int, string) (int, any) {
		return http.StatusConflict, refusedWith("the task is held by another runner")
	}))
	l.loop.Namespaces = []string{"ops"}
	m, _ := l.task(t, nil)
	if err := l.loop.Holder.Hold(agk.TaskID(m.IdempotencyKey)); err != nil {
		t.Fatal(err)
	}

	l.carryOne(t)

	if _, ok := l.pool.take(t, time.Second); ok {
		t.Error("a message whose key this host has in flight was put back, and came straight round")
	}
	if n := l.api.redemptions(m.TaskID); n != 0 {
		t.Errorf("the grant was redeemed %d times", n)
	}
}

// "A runner puts back on the queue a task whose declared resources would exceed its own declared
// capacity, so oversubscription is a choice, not an accident": before anything is written down or
// redeemed, and for another runner of the pool.
func TestATaskLargerThanTheHostIsPutBackUnredeemed(t *testing.T) {
	api := anAPIAnswering(t, func(int, string) (int, any) {
		return http.StatusConflict, refusedWith("the task is held by another runner")
	})
	l := aLoop(t, carrier(t, nil), aPoolOnTheBus(t, 30*time.Second), api)
	l.loop.Capacity = Room{Memory: 1 << 30, NanoCPUs: 8e9}
	m, _ := l.task(t, func(m *bus.TaskMessage) { m.Resources.Memory = "2Gi" })

	l.carryOne(t)

	again, ok := l.pool.take(t, 3*time.Second)
	if !ok || again.Task.TaskID != m.TaskID {
		t.Fatal("the task was not put back for another runner of the pool")
	}
	if n := api.redemptions(m.TaskID); n != 0 {
		t.Errorf("the grant of a task this host has no room for was redeemed %d times", n)
	}
	if n := l.containersOf(m.IdempotencyKey); n != 0 {
		t.Errorf("%d containers were created for a task this host has no room for", n)
	}
	if held := l.loop.Held(); len(held) != 0 {
		t.Errorf("the loop names %v for a task it put back", held)
	}
	if err := l.loop.Holder.Hold(agk.TaskID(m.IdempotencyKey)); err != nil {
		t.Errorf("the key was written down for a task put back: %s", err)
	}
}

// "With what it already holds": two tasks that fit the host each on its own and not together never
// run at once on it, though it has the slots for both, and both run.
func TestTasksThatFitOnlyOneAtATimeRunOneAtATime(t *testing.T) {
	var running, most atomic.Int32
	c := carrier(t, func(dockertest.Container) (int, error) {
		n := running.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		time.Sleep(300 * time.Millisecond)
		running.Add(-1)
		return 0, nil
	})
	var (
		mu          sync.Mutex
		redemptions = map[string]Redemption{}
	)
	api := anAPIAnswering(t, func(_ int, taskID string) (int, any) {
		mu.Lock()
		defer mu.Unlock()
		return http.StatusOK, redemptions[taskID]
	})
	l := aLoop(t, c, aPoolOnTheBus(t, 30*time.Second), api)
	l.loop.Capacity = Room{Memory: 3 << 30}
	for i := range 2 {
		m, r := l.task(t, func(m *bus.TaskMessage) {
			m.Step = fmt.Sprintf("invoice-%d", i)
			m.IdempotencyKey = string(storeRun) + "/" + m.Step + "/1"
			m.Resources.Memory = "2Gi"
		})
		mu.Lock()
		redemptions[m.TaskID] = r
		mu.Unlock()
	}

	runUntil(t, l.loop, 20*time.Second, func() bool { return len(l.bus.all()) == 2 })

	if n := len(l.bus.all()); n != 2 {
		t.Fatalf("%d of the two tasks were reported", n)
	}
	if n := most.Load(); n != 1 {
		t.Errorf("%d containers ran at once, each declaring 2Gi on a host declaring 3Gi", n)
	}
}

// What a task declares is counted from the moment it fits until the loop no longer holds it, against
// memory and cpu alike, and a part of the capacity at zero bounds nothing.
func TestTheHostsRoomIsCountedWithWhatItHolds(t *testing.T) {
	l := &Loop{Capacity: Room{Memory: 1 << 30, NanoCPUs: 2e9}}
	task := func(memory, cpu string) bus.TaskMessage {
		return bus.TaskMessage{Step: "invoice", Resources: bus.Resources{Memory: memory, CPU: cpu}}
	}

	first, fits, _ := l.reserve(task("600Mi", "1"))
	if !fits {
		t.Fatal("a task within an empty host's capacity does not fit")
	}
	if _, fits, why := l.reserve(task("600Mi", "0.5")); fits || !strings.Contains(why, "its other tasks") {
		t.Errorf("a second task past the memory left fits (%t), or is not said to be refused for what the host holds: %q", fits, why)
	}
	if _, fits, _ := l.reserve(task("100Mi", "1.5")); fits {
		t.Error("a second task past the cpu left fits")
	}
	if _, fits, why := l.reserve(task("2Gi", "")); fits || !strings.Contains(why, "at all") {
		t.Errorf("a task larger than the whole host fits (%t), or is not said to be: %q", fits, why)
	}
	second, fits, _ := l.reserve(task("", ""))
	if !fits || second != (Room{}) {
		t.Errorf("a task declaring nothing counts %v and fits %t", second, fits)
	}
	// Refused on the brick's account by the driver once it runs, rather than put back for ever.
	if malformed, fits, _ := l.reserve(task("512M", "")); !fits || malformed != (Room{}) {
		t.Errorf("a task declaring memory the grammar refuses counts %v and fits %t", malformed, fits)
	}

	l.unreserve(first)
	if _, fits, _ := l.reserve(task("600Mi", "0.5")); !fits {
		t.Error("what a task declared was still counted once the loop no longer held it")
	}

	unbounded := &Loop{Capacity: Room{NanoCPUs: 1e9}}
	if _, fits, _ := unbounded.reserve(task("1Ti", "")); !fits {
		t.Error("a capacity that declares no memory bounded a task's memory")
	}
}

// The capacity serve counts against is the one join declares, measured the same way.
func TestTheHostsRoomIsMeasuredAsJoinMeasuresIt(t *testing.T) {
	room, err := HostRoom(filepath.Join("testdata", "meminfo"))
	if err != nil {
		t.Fatal(err)
	}
	if room.Memory != 16318196<<10 {
		t.Errorf("the memory is %d bytes, and meminfo says 16318196 kB", room.Memory)
	}
	if room.NanoCPUs < 1e9 {
		t.Errorf("the host has %d billionths of a core", room.NanoCPUs)
	}
	if _, err := HostRoom(filepath.Join(t.TempDir(), "meminfo")); err == nil {
		t.Error("a host whose memory cannot be read has a capacity")
	}
}
