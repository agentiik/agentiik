package api

import (
	"context"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentiik/agentiik/internal/password"
)

// What the password sign-in lets anybody spend: guesses, and the memory a guess is checked with.
//
// # Guesses
//
// A password can be guessed, which a passkey cannot, so the attempts at one are counted: those for
// one login, from anywhere, and those from one address, at any login, each in a sliding window, and
// an attempt past either count is answered 429 with Retry-After, the whole seconds until one more
// fits, before anything is read or hashed. The account's count is what stops a guesser with many
// addresses at one account, and the address's what stops one address guessing across many.
//
// An attempt is counted as it starts rather than once it has failed, so that a hundred sent at once
// for one login are not a hundred guesses before the first is counted, and it is given back where it
// was no guess: refused before any password was compared, as by a policy forbidding passwords, or
// failing on the API's own account. A sign-in that succeeds gives back its own, and starts its
// account's count again, so that a person who mistyped nine times and then signed in does not begin
// the next day one guess from the limit; the address keeps what it spent on other accounts.
//
// The account's count holds against the one guessing and against the account's holder alike: whoever
// spends it shuts the account's password out for the rest of the window. That is the price of a
// count nobody can get around by changing address, and it is a small one here, since a password is
// the fallback: the account's passkeys, and a recovery code, are not counted.
//
// Kept in memory, by each replica of the API for itself, as the bound on the audit log is: an
// installation of three replicas lets three times the count through at most, and a restart forgets
// it. A table would hold it across both, at the price of a write for every attempt anybody makes.
//
// # Memory
//
// "19 MiB of memory" is what one hash holds while it runs, so a hundred sign-ins at once would hold
// nearly two GiB. How many run at once is bounded by hashing, sized from the processors the process
// may use and the memory it may; a sign-in waiting longer than hashingWait for its turn is answered
// 503, which says the installation is busy rather than that the password was wrong. A hash is
// computed for a login no user holds, and one holding no password, as for any other, against a hash
// of nothing anybody knows, so that how long an answer takes does not tell who has an account.

// The counts: ten attempts for one account in a quarter of an hour, more than a person makes
// finding the password they meant and forty an hour at most for anybody guessing it, under the
// hundred NIST SP 800-63B allows; and thirty from one address, so that one address guessing across
// accounts is held too, and a few people behind one address who mistype do not hold each other up.
const (
	attemptsPerLogin   = 10
	attemptsPerAddress = 30
	attemptsWindow     = 15 * time.Minute
)

// attemptsTracked is how many logins, and how many addresses, are counted at once. Past it, those
// whose window holds nothing any more are forgotten, and where none is, the one whose last attempt
// is oldest: what the counts hold is bounded however many logins and addresses attempts name, and
// what is forgotten is what was least recently tried.
const attemptsTracked = 16384

// attempts counts the password sign-ins started, for each login and each address, in a sliding
// window.
type attempts struct {
	mu        sync.Mutex
	logins    map[string][]time.Time
	addresses map[string][]time.Time
}

func newAttempts() *attempts {
	return &attempts{logins: map[string][]time.Time{}, addresses: map[string][]time.Time{}}
}

// take counts an attempt for login from address at now, and answers true; or, where either has
// spent its count in the window before now, counts nothing and answers how long until one more
// fits.
func (a *attempts) take(login, address string, now time.Time) (time.Duration, bool) {
	key := failureKey(address)
	a.mu.Lock()
	defer a.mu.Unlock()
	byLogin := inWindow(a.logins[login], now)
	byAddress := inWindow(a.addresses[key], now)
	var wait time.Duration
	if len(byLogin) >= attemptsPerLogin {
		wait = byLogin[len(byLogin)-attemptsPerLogin].Add(attemptsWindow).Sub(now)
	}
	if len(byAddress) >= attemptsPerAddress {
		wait = max(wait, byAddress[len(byAddress)-attemptsPerAddress].Add(attemptsWindow).Sub(now))
	}
	if wait > 0 {
		keep(a.logins, login, byLogin)
		keep(a.addresses, key, byAddress)
		return wait, false
	}
	keep(a.logins, login, append(byLogin, now))
	keep(a.addresses, key, append(byAddress, now))
	return 0, true
}

// forgive takes back the attempt taken at at for login from address, one that was no guess.
func (a *attempts) forgive(login, address string, at time.Time) {
	key := failureKey(address)
	a.mu.Lock()
	defer a.mu.Unlock()
	keep(a.logins, login, without(a.logins[login], at))
	keep(a.addresses, key, without(a.addresses[key], at))
}

// signedIn takes back the attempt taken at at from address, which signed in, and forgets every
// attempt counted for login.
func (a *attempts) signedIn(login, address string, at time.Time) {
	key := failureKey(address)
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.logins, login)
	keep(a.addresses, key, without(a.addresses[key], at))
}

// inWindow is the attempts of times inside the window ending at now, in the order they were taken.
func inWindow(times []time.Time, now time.Time) []time.Time {
	for len(times) > 0 && !times[0].After(now.Add(-attemptsWindow)) {
		times = times[1:]
	}
	return times
}

// without is times with one attempt taken at at removed, the latest where two were.
func without(times []time.Time, at time.Time) []time.Time {
	for i := len(times) - 1; i >= 0; i-- {
		if times[i].Equal(at) {
			return append(times[:i:i], times[i+1:]...)
		}
	}
	return times
}

// keep writes the attempts of one key, forgetting a key that holds none, and making room for a new
// key where attemptsTracked are counted already.
func keep(counts map[string][]time.Time, key string, times []time.Time) {
	if len(times) == 0 {
		delete(counts, key)
		return
	}
	if _, counted := counts[key]; !counted && len(counts) >= attemptsTracked {
		now := times[len(times)-1]
		oldest, last := "", time.Time{}
		for k, held := range counts {
			newest := held[len(held)-1]
			if !newest.After(now.Add(-attemptsWindow)) {
				delete(counts, k)
				continue
			}
			if oldest == "" || newest.Before(last) {
				oldest, last = k, newest
			}
		}
		if len(counts) >= attemptsTracked {
			delete(counts, oldest)
		}
	}
	counts[key] = times
}

// hashingWait is how long a sign-in waits for its turn to hash: five seconds, a hundred hashes and
// more for each turn at the baseline, which a morning's sign-ins never queue behind, and short
// enough to answer a person before they give up on the page.
const hashingWait = 5 * time.Second

// hashingShare is the part of the memory the process may use that hashing may hold: a quarter,
// leaving the rest to everything else the API serves at the same moment.
const hashingShare = 4

// hashing bounds how many passwords are hashed at once, one turn for each.
type hashing struct {
	turns chan struct{}
	wait  time.Duration
}

func newHashing(turns int, wait time.Duration) *hashing {
	return &hashing{turns: make(chan struct{}, max(turns, 1)), wait: wait}
}

// turn waits for a turn to hash, for hashingWait at most, and answers the function that gives it
// back, or false where none came in time or the request ended first.
func (h *hashing) turn(ctx context.Context) (func(), bool) {
	timer := time.NewTimer(h.wait)
	defer timer.Stop()
	select {
	case h.turns <- struct{}{}:
		return func() { <-h.turns }, true
	case <-timer.C:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

// hashingTurns is how many passwords may be hashed at once: one for each processor the process may
// use, since a hash is work for one of them and a second on the same processor only waits its turn
// there, and no more than hashingShare of the memory it may use holds, at password.MemoryBytes a
// hash. The memory is the Go runtime's limit, GOMEMLIMIT, or the container's, cgroup's memory.max,
// whichever is less, and where neither is set the processors alone say, which is 19 MiB for each of
// them.
func hashingTurns() int {
	turns := runtime.GOMAXPROCS(0)
	limit := debug.SetMemoryLimit(-1)
	if container := containerMemory(); container > 0 && container < limit {
		limit = container
	}
	if limit < math.MaxInt64 {
		turns = min(turns, int(limit/hashingShare/password.MemoryBytes))
	}
	return max(turns, 1)
}

// containerMemory is the memory the container the process runs in may use, as cgroup v2 writes it,
// and 0 where it says none or cannot be read, as outside a container on Linux and anywhere else.
func containerMemory() int64 {
	raw, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err != nil {
		return 0
	}
	limit, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || limit <= 0 {
		return 0
	}
	return limit
}
