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

// BusIssuer mints a credential for one runner and one pool, and the narrower one a revoked runner
// finishes its grace with.
type BusIssuer interface {
	ForRunner(name, pool string, until time.Time) (bus.Credentials, error)
	ForRevokedRunner(name string, until time.Time) (bus.Credentials, error)
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
	// half understood, however it was sent.
	if err := readIfAny(r, nothingAsked{}, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}

	// No longer than the runner credential it was asked for with: "A credential past its
	// rotate_by is refused everywhere", and a bus credential outliving it by up to an hour would
	// be a runner that has to join again still pulling work.
	until := s.now().Add(BusLife)
	if !runner.RotateBy.IsZero() && runner.RotateBy.Before(until) {
		until = runner.RotateBy
	}

	// A revoked runner is in its grace, since it would not have been opened after it. It is
	// minted what the grace allows, "narrowed to publishing results and hearing stops, no
	// pull", and nothing past the grace's end, when its results stop being taken. Its pool
	// has nothing it may take, so no consumer is made ready for it. A draining runner is
	// minted what it always was: it stops taking work because the heartbeat told it to, and
	// a redemption would refuse it anyway.
	if runner.State == "revoked" {
		if runner.ResultsAcceptedUntil.Before(until) {
			until = runner.ResultsAcceptedUntil
		}
		credentials, err := s.issuer.ForRevokedRunner(runner.ID, until)
		if err != nil {
			fail(w, http.StatusInternalServerError, "the bus credential could not be minted")
			return
		}
		answerBus(w, runner, credentials)
		return
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

	credentials, err := s.issuer.ForRunner(runner.ID, runner.Pool, until)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the bus credential could not be minted")
		return
	}
	answerBus(w, runner, credentials)
}

// answerBus answers a bus credential, in one shape whatever it allows. A revoked runner is told its
// pool's consumer as well, which its credential no longer pulls from, so that the agent reads one
// answer and learns it is draining where every order reaches it, at the heartbeat.
func answerBus(w http.ResponseWriter, runner Runner, credentials bus.Credentials) {
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
