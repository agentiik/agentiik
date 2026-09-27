package api

import (
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// What a failed sign-in costs the audit log.
//
// Every assertion refused is recorded as signin.fail, and a refused assertion is anybody's to
// send: an answer to options they asked for themselves, signed with a key of their own. Unbounded,
// that is a row in the audit log for every request anybody cares to make, each append waiting its
// turn at the head of the one chain every act of the installation appends to. So what one address
// appends is bounded: failuresRecorded entries in any window of failuresWindow, and past that a
// refusal is answered as every other is, and counted, and the next entry the address appends says
// how many went unrecorded before it, so that the log never reads as if they had not happened.
//
// The bound is on the log and not on the sign-in: a refusal past it is still verified and still
// answered 401, and a sign-in that verifies is never refused for the failures beside it. Behind a
// proxy every request comes from the proxy's address, and a bound on sign-ins would let one person
// failing at the proxy shut everybody out; a bound on the log only makes them share it.
//
// Kept in memory, by each replica of the API for itself, since what it bounds is the volume and
// not a count anybody reads: an installation of three replicas appends three times the bound at
// most.

// failuresRecorded and failuresWindow are the bound: ten entries in ten minutes, more than a
// person fumbling for the right authenticator makes, and one a minute from anybody failing on
// purpose for as long as they keep at it.
const (
	failuresRecorded = 10
	failuresWindow   = 10 * time.Minute
)

// failuresTracked is how many addresses are counted apart. Past it, the addresses whose window
// has ended are forgotten, and where none has, every address not already counted shares one count,
// so that what the counts hold is bounded however many addresses a failure comes from.
const failuresTracked = 1024

// failedSignIns counts the failed sign-ins of each address, in fixed windows.
type failedSignIns struct {
	mu sync.Mutex
	by map[string]*failuresFrom
}

// failuresFrom is one address's count: its window's start, the entries recorded in it, and the
// failures refused unrecorded since its last entry.
type failuresFrom struct {
	since      time.Time
	recorded   int
	unrecorded int
}

func newFailedSignIns() *failedSignIns { return &failedSignIns{by: map[string]*failuresFrom{}} }

// admit says whether a failed sign-in from address at now is recorded, and, where it is, how many
// from the same address went unrecorded before it.
func (f *failedSignIns) admit(address string, now time.Time) (unrecorded int, recorded bool) {
	key := failureKey(address)
	f.mu.Lock()
	defer f.mu.Unlock()
	from, counted := f.by[key]
	if !counted {
		if len(f.by) >= failuresTracked {
			for k, c := range f.by {
				if !now.Before(c.since.Add(failuresWindow)) {
					delete(f.by, k)
				}
			}
		}
		if len(f.by) >= failuresTracked {
			// Every address past the ones counted shares the empty key, which no address has.
			key = ""
			from, counted = f.by[key]
		}
		if !counted {
			from = &failuresFrom{since: now}
			f.by[key] = from
		}
	}
	if !now.Before(from.since.Add(failuresWindow)) {
		from.since, from.recorded = now, 0
	}
	if from.recorded >= failuresRecorded {
		from.unrecorded++
		return 0, false
	}
	from.recorded++
	unrecorded, from.unrecorded = from.unrecorded, 0
	return unrecorded, true
}

// failureKey is what an address is counted under: the address itself for IPv4, and its /64 for
// IPv6, since a single machine is handed a /64 and chooses any address in it. An address that does
// not parse is counted as written.
func failureKey(address string) string {
	a, err := netip.ParseAddr(address)
	if err != nil {
		return address
	}
	a = a.Unmap()
	if a.Is4() {
		return a.String()
	}
	p, err := a.WithZone("").Prefix(64)
	if err != nil {
		return address
	}
	return p.String()
}

// addressOf is the address a request came from, as the connection says: behind a proxy on the same
// host, the proxy's. Never a header, which whoever sends the request writes.
func addressOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || host == "" {
		if r.RemoteAddr != "" {
			return r.RemoteAddr
		}
		// No login can be written with a space, so the actor of an entry naming none reads as
		// nobody's act.
		return "an unknown address"
	}
	return host
}
