package api

import (
	"context"
	"net/http"
	"time"

	"github.com/agentiik/agentiik/bus"
)

// The credential a runner uses to reach the task bus.
//
// "asks the API for a short-lived bus token whenever the previous one nears expiry. Bus
// credentials are therefore never at rest on a runner host, which is what lets a runner sit in a
// zone where a stolen disk must not yield a working queue consumer."
//
// The API is where it comes from because the API is the only thing a runner is allowed to ask, and
// because the runner credential it presents here is what says which pool it belongs to. A machine
// cannot ask for another pool's: there is nothing in the request that names one.

// BusIssuer mints a credential for one runner and one pool.
type BusIssuer interface {
	ForRunner(name, pool string, until time.Time) (bus.Credentials, error)
}

// BusConsumers makes sure a pool has somewhere for its runners to pull from.
//
// Separate from BusIssuer because they are different objects: one holds the account key and the
// other holds a connection. A runner cannot create its own consumer, which is deliberate, so
// something on this side has to, and the moment a runner asks for a credential is the moment it is
// about to need one.
type BusConsumers interface {
	Consumer(ctx context.Context, pool string) error
}

// BusLife is how long a bus credential is minted for.
//
// An hour, and the agent renews well before it. Short enough that a credential read off a disk
// afterwards is worth nothing, long enough that renewal is a background chore rather than traffic.
const BusLife = time.Hour

// nothingAsked is the body of a request for a bus credential, which has no field.
type nothingAsked struct{}

func (nothingAsked) field(_ *body, name string) error { return unknown(name) }

func (s *RunnerAPI) busToken(w http.ResponseWriter, r *http.Request, runner Runner) {
	if s.issuer == nil {
		fail(w, http.StatusServiceUnavailable, "this installation mints no bus credentials")
		return
	}
	// A runner asking for a credential has nothing to say beyond who it is, so no body is
	// the ordinary request. One carrying a field nobody knows is still refused rather than
	// half understood.
	if r.ContentLength > 0 {
		if err := readAtMost(r, nothingAsked{}, smallMaxBytes); err != nil {
			fail(w, statusOf(err), err.Error())
			return
		}
	}

	// The pool comes from the runner the credential opened, never from the request. A
	// machine that could name its own pool could take another pool's work, which is the
	// same defect as a self-asserted label and reaches further.
	if s.consumers != nil {
		if err := s.consumers.Consumer(r.Context(), runner.Pool); err != nil {
			fail(w, http.StatusInternalServerError, "the queue for that pool could not be made ready")
			return
		}
	}

	credentials, err := s.issuer.ForRunner(runner.ID, runner.Pool, s.now().Add(BusLife))
	if err != nil {
		fail(w, http.StatusInternalServerError, "the bus credential could not be minted")
		return
	}
	// It exists in this answer and on the machine that asked, and nowhere else.
	w.Header().Set("Cache-Control", "no-store")
	write(w, http.StatusOK, map[string]any{
		"kind":       credentials.Kind,
		"url":        credentials.URL,
		"jwt":        credentials.JWT,
		"seed":       credentials.Seed,
		"consumer":   bus.Durable(runner.Pool),
		"stream":     bus.Stream,
		"expires_at": credentials.ExpiresAt.Format(time.RFC3339Nano),
	})
}
