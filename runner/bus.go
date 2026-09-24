package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/agentiik/agentiik/bus"
)

// Reaching the task bus.
//
// "The runner asks the API for a short-lived bus token whenever the previous one nears expiry. Bus
// credentials are therefore never at rest on a runner host." So nothing on the host names the bus:
// the API answers where it is and what to present to it, for the pool the runner credential
// belongs to, and the agent holds the answer in memory and nowhere else.

// busTokenPath is the route, as the router spells it.
const busTokenPath = "/api/v1/bus/token"

// busToken is the API's answer: a credential of the kind bus.Kind names, the consumer of the pool
// the runner joined and the stream it reads from, and when the credential stops working.
type busToken struct {
	Kind      string `json:"kind"`
	URL       string `json:"url"`
	JWT       string `json:"jwt"`
	Seed      string `json:"seed"`
	Consumer  string `json:"consumer"`
	Stream    string `json:"stream"`
	ExpiresAt string `json:"expires_at"`
}

// BusCredentials asks the API for this runner's bus credential.
//
// It is held to the runner that asks: of the kind this runner speaks, for the stream it takes work
// from and the consumer of the pool it joined, and not yet expired. A pool other than the one join
// wrote is refused rather than followed, since the API answers the pool the credential opened and
// runner.env the pool join was answered, and two that disagree are an installation to look at
// rather than one to take work from.
func (c *Client) BusCredentials(ctx context.Context, pool string) (bus.Credentials, error) {
	var t busToken
	if err := c.Do(ctx, http.MethodPost, busTokenPath, nil, &t); err != nil {
		return bus.Credentials{}, err
	}
	expires, err := time.Parse(time.RFC3339Nano, t.ExpiresAt)
	switch {
	case t.Kind != bus.Kind:
		return bus.Credentials{}, fmt.Errorf("runner: POST %s: the bus credential is of the kind %q, and this runner speaks %s", busTokenPath, t.Kind, bus.Kind)
	case t.URL == "" || t.JWT == "" || t.Seed == "":
		return bus.Credentials{}, fmt.Errorf("runner: POST %s: the bus credential names no bus, or nothing to present to it", busTokenPath)
	case t.Stream != bus.Stream:
		return bus.Credentials{}, fmt.Errorf("runner: POST %s: the bus credential is for the stream %q, and a runner takes work from %s", busTokenPath, t.Stream, bus.Stream)
	case t.Consumer != bus.Durable(pool):
		return bus.Credentials{}, fmt.Errorf("runner: POST %s: the bus credential takes from the consumer %q, and this runner joined pool %s", busTokenPath, t.Consumer, pool)
	case err != nil:
		return bus.Credentials{}, fmt.Errorf("runner: POST %s: the bus credential expires at %q, which is not an RFC 3339 instant", busTokenPath, t.ExpiresAt)
	case !expires.After(time.Now()):
		return bus.Credentials{}, fmt.Errorf("runner: POST %s: the bus credential expired at %s, before it arrived", busTokenPath, t.ExpiresAt)
	}
	return bus.Credentials{Kind: t.Kind, URL: t.URL, JWT: t.JWT, Seed: t.Seed, ExpiresAt: expires}, nil
}

// OpenBus asks for this runner's bus credential and connects with it, asking again for as long as
// the API does not answer.
//
// An API that does not answer is one that may, and an agent started before it, as a host booting
// beside the control plane is, waits for it rather than failing its start into a restart loop. An
// answer that refuses, the credential opening nothing or a bus of another kind, is the same on
// every try and ends the wait. A context that ends answers nil and no error: the agent was
// stopped, and that is not a failure to say.
func OpenBus(ctx context.Context, c *Client, cfg Config, say func(string)) (*bus.Bus, error) {
	wait := retryFirst
	for {
		credentials, err := c.BusCredentials(ctx, cfg.Pool)
		if err == nil {
			// A bus that cannot be reached now is one that may be, as an API that does
			// not answer is, and the credential is good for an hour.
			var b *bus.Bus
			if b, err = bus.OpenRunner(bus.Options{URL: credentials.URL, Name: cfg.Runner, Credentials: &credentials}); err == nil {
				return b, nil
			}
			err = fmt.Errorf("runner: %w: %w", ErrUnavailable, err)
		}
		if ctx.Err() != nil {
			return nil, nil
		}
		if !errors.Is(err, ErrUnavailable) {
			return nil, err
		}
		say(fmt.Sprintf("the task bus is not reached yet, and is tried again in %s: %s", wait, err))
		if !sleep(ctx, wait) {
			return nil, nil
		}
		wait = min(2*wait, retryMost)
	}
}
