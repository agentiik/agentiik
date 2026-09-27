package api

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/agentiik/agentiik/internal/password"
)

// An attempt past a count is refused with the time until the first of those counted leaves the
// window, whichever count it is past; one given back is not counted, and a sign-in clears its login's
// count and gives back its address's one attempt.
func TestAnAttemptPastACountWaitsForTheFirstToLeaveTheWindow(t *testing.T) {
	a := newAttempts()
	start := time.Unix(1_800_000_000, 0)
	for i := range attemptsPerLogin {
		if _, ok := a.take("alice", fmt.Sprintf("192.0.2.%d", i), start.Add(time.Duration(i)*time.Minute)); !ok {
			t.Fatalf("attempt %d was refused", i)
		}
	}
	now := start.Add(12 * time.Minute)
	if wait, ok := a.take("alice", "198.51.100.1", now); ok || wait != 3*time.Minute {
		t.Errorf("an attempt past the login's count answered %v, %s", ok, wait)
	}
	if _, ok := a.take("bob", "198.51.100.1", now); !ok {
		t.Error("another login's attempt was refused")
	}
	if _, ok := a.take("alice", "198.51.100.1", start.Add(attemptsWindow)); !ok {
		t.Error("the first attempt leaving the window made no room")
	}

	a = newAttempts()
	for i := range attemptsPerAddress {
		if _, ok := a.take(fmt.Sprintf("user-%d", i), "203.0.113.5", start); !ok {
			t.Fatalf("attempt %d from one address was refused", i)
		}
	}
	if wait, ok := a.take("alice", "203.0.113.5", start.Add(time.Second)); ok || wait != attemptsWindow-time.Second {
		t.Errorf("an attempt past the address's count answered %v, %s", ok, wait)
	}
	a.forgive("user-0", "203.0.113.5", start)
	if _, ok := a.take("alice", "203.0.113.5", start.Add(time.Second)); !ok {
		t.Error("an attempt given back is still counted")
	}

	a = newAttempts()
	for i := range attemptsPerLogin - 1 {
		a.take("alice", "203.0.113.5", start.Add(time.Duration(i)*time.Second))
	}
	at := start.Add(time.Minute)
	a.take("alice", "203.0.113.5", at)
	a.signedIn("alice", "203.0.113.5", at)
	if len(a.logins["alice"]) != 0 || len(a.addresses["203.0.113.5"]) != attemptsPerLogin-1 {
		t.Errorf("after a sign-in alice's count holds %d and the address's %d", len(a.logins["alice"]), len(a.addresses["203.0.113.5"]))
	}
}

// However many logins and addresses attempts name, the counts hold attemptsTracked of each at most,
// forgetting first those whose window holds nothing, then the least recently tried.
func TestTheCountsHoldABoundedNumberOfLoginsAndAddresses(t *testing.T) {
	a := newAttempts()
	start := time.Unix(1_800_000_000, 0)
	for i := range attemptsTracked + 100 {
		a.take(fmt.Sprintf("user-%d", i), fmt.Sprintf("10.%d.%d.%d", i>>16, (i>>8)&255, i&255), start.Add(time.Duration(i)*time.Millisecond))
	}
	if len(a.logins) != attemptsTracked || len(a.addresses) != attemptsTracked {
		t.Errorf("the counts hold %d logins and %d addresses", len(a.logins), len(a.addresses))
	}
	if _, kept := a.logins["user-0"]; kept {
		t.Error("the least recently tried login is still counted")
	}
	if _, kept := a.logins[fmt.Sprintf("user-%d", attemptsTracked+99)]; !kept {
		t.Error("the most recently tried login is not counted")
	}
	later := start.Add(attemptsWindow + time.Hour)
	a.take("fresh", "192.0.2.1", later)
	if len(a.logins) != 1 || len(a.addresses) != 1 {
		t.Errorf("once every window had ended, the counts hold %d logins and %d addresses", len(a.logins), len(a.addresses))
	}
}

// Hashing takes one turn for each processor, no more than a quarter of the memory limit holds at 19
// MiB a hash, and one at least.
func TestHashingTurnsAreSizedFromTheProcessorsAndTheMemory(t *testing.T) {
	was := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(was)
	procs := runtime.GOMAXPROCS(0)

	debug.SetMemoryLimit(math.MaxInt64)
	if container := containerMemory(); container == 0 {
		if got := hashingTurns(); got != procs {
			t.Errorf("with no memory limit, hashing takes %d turns on %d processors", got, procs)
		}
	}
	debug.SetMemoryLimit(hashingShare * password.MemoryBytes * 2)
	if got := hashingTurns(); got != min(procs, 2) {
		t.Errorf("with room for two hashes, hashing takes %d turns", got)
	}
	debug.SetMemoryLimit(password.MemoryBytes)
	if got := hashingTurns(); got != 1 {
		t.Errorf("with room for no hash, hashing takes %d turns", got)
	}
}

// A turn is waited for, for the wait at most, a request that ended stops waiting, and a hashing
// sized to nothing still hashes one at a time.
func TestATurnToHashIsWaitedForAndNoLonger(t *testing.T) {
	h := newHashing(1, 50*time.Millisecond)
	done, ok := h.turn(context.Background())
	if !ok {
		t.Fatal("the one turn was not given")
	}
	began := time.Now()
	if _, ok := h.turn(context.Background()); ok || time.Since(began) < 50*time.Millisecond {
		t.Errorf("a second turn answered %v after %s", ok, time.Since(began))
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := newHashing(0, time.Hour).turn(context.Background()); !ok {
		t.Error("no turn was given where none is held, a hashing of no turn having one")
	}
	busy := newHashing(1, time.Hour)
	busy.turn(context.Background())
	if _, ok := busy.turn(ended); ok {
		t.Error("a request that ended was given a turn")
	}
	done()
	if again, ok := h.turn(context.Background()); !ok {
		t.Error("a turn given back was not given again")
	} else {
		again()
	}
}
