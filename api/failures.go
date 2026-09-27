package api

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// What a failed sign-in costs the audit log, and where a sign-in comes from.
//
// Every assertion refused is recorded as signin.fail, and so is every password sign-in refused once
// it has been read and counted (passwords.go). An assertion refused before its signature verified is
// anybody's to send: an answer to options they asked for themselves, signed with a key of their own,
// or with none; and so is any password sign-in, with any login and any password. Unbounded, that is a row in the audit log for every request anybody
// cares to make, each append waiting its turn at the head of the one chain every act of the
// installation appends to, and each kept for good and exported. So what such refusals append is
// bounded, twice: failuresRecorded entries from one address in a window of failuresWindow, and
// failuresRecordedAll from every address together, so that a sender with many addresses is held
// too. Past either, a refusal is answered as every other is, and counted, and the next entry
// appended says how many went unrecorded before it, from whatever address, so that the log never
// reads as if they had not happened.
//
// A refusal after the signature verified is recorded whatever the bound says, and so is the
// notification a counter that did not move forward sends its user: only whoever holds the
// passkey's private key can make one, and such a refusal is what the log and the notification are
// for, a copy of the key in use above all, which a bound anybody else could fill would silence.
//
// The bound is on the log and not on the sign-in: a refusal past it is still verified and still
// answered 401, and a sign-in that verifies is never refused for the failures beside it. A proxy in
// front of the API that does not say who its client is makes every request come from its own
// address, and a bound on sign-ins would then let one person failing at it shut everybody out; a
// bound on the log only makes them share it. The password sign-in's own bound, on the guesses it
// takes, is its own (passwords_limits.go).
//
// Kept in memory, by each replica of the API for itself, since what it bounds is the volume and not
// a count anybody reads: an installation of three replicas appends three times the bound at most.
// The sign-in routes share one count, SignIns, so that failing at one route and then at the other
// is bounded as failing at one.

// The bound: ten entries from one address in ten minutes, more than a person fumbling for the right
// authenticator makes, and one a minute from anybody failing on purpose for as long as they keep at
// it; and a hundred from every address together, ten minutes of an installation's own people
// failing many times over.
const (
	failuresRecorded    = 10
	failuresRecordedAll = 100
	failuresWindow      = 10 * time.Minute
)

// failuresTracked is how many addresses are counted apart. Past it, the addresses whose window has
// ended are forgotten, and where none has, every address not already counted shares one count, so
// that what the counts hold is bounded however many addresses a failure comes from. Forgetting an
// address forgets nothing unrecorded, which is counted for every address together.
const failuresTracked = 1024

// failedSignIns counts the failed sign-ins recorded, from each address and from all of them, in
// fixed windows, and those refused unrecorded since the last entry.
type failedSignIns struct {
	mu         sync.Mutex
	by         map[string]*failuresFrom
	all        failuresFrom
	unrecorded int
}

// failuresFrom is one window's count: when it started, and the entries recorded in it.
type failuresFrom struct {
	since    time.Time
	recorded int
}

// at is the count as of now, a window started afresh where the last has ended.
func (c *failuresFrom) at(now time.Time) *failuresFrom {
	if !now.Before(c.since.Add(failuresWindow)) {
		c.since, c.recorded = now, 0
	}
	return c
}

func newFailedSignIns() *failedSignIns { return &failedSignIns{by: map[string]*failuresFrom{}} }

// SignIns is what the sign-in routes share: where a request comes from, and the bound on what the
// sign-ins they refuse append to the audit log. serve makes one and hands it to each.
type SignIns struct {
	// proxied is an API served behind the proxy AGK_PROXY_URL names, whose client addresses
	// are read from X-Forwarded-For.
	proxied  bool
	failures *failedSignIns
}

// NewSignIns makes one, for an API served behind the proxy AGK_PROXY_URL names where proxied is set.
func NewSignIns(proxied bool) *SignIns {
	return &SignIns{proxied: proxied, failures: newFailedSignIns()}
}

// addressOf is the address a sign-in came from: the connection's, or the proxy's last entry of
// X-Forwarded-For where the API is served behind the proxy AGK_PROXY_URL names.
func (s *SignIns) addressOf(r *http.Request) string { return addressOf(r, s.proxied) }

// admit says whether a failed sign-in from address at now, one refused before its signature
// verified, is recorded, and, where it is, how many went unrecorded before it.
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
	if from.at(now).recorded >= failuresRecorded || f.all.at(now).recorded >= failuresRecordedAll {
		f.unrecorded++
		return 0, false
	}
	from.recorded++
	f.all.recorded++
	unrecorded, f.unrecorded = f.unrecorded, 0
	return unrecorded, true
}

// recordedAnyway answers how many went unrecorded before a failed sign-in recorded whatever the
// bound says, one refused after its signature verified, which is not counted against the bound.
func (f *failedSignIns) recordedAnyway() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	unrecorded := f.unrecorded
	f.unrecorded = 0
	return unrecorded
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

// addressOf is the address a request came from, as the connection says; or, for an API served
// behind the proxy AGK_PROXY_URL names where proxied is set, as the last entry of X-Forwarded-For
// says, the one that proxy wrote, and the connection's where the request carries none that reads as
// an address.
//
// The last entry and no other, and behind that proxy and nowhere else: the API then listens on the
// loopback, so every request comes through the proxy on this host, and the proxy writes its
// client's address after whatever the client sent; the entries before it are the client's to write.
// Anywhere else the header is anybody's, and not read.
func addressOf(r *http.Request, proxied bool) string {
	if proxied {
		if forwarded := r.Header.Values("X-Forwarded-For"); len(forwarded) > 0 {
			entries := strings.Split(forwarded[len(forwarded)-1], ",")
			if a, err := netip.ParseAddr(strings.TrimSpace(entries[len(entries)-1])); err == nil {
				return a.String()
			}
		}
	}
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
