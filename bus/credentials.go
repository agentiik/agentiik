package bus

import (
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
)

// The credential a runner holds for as long as it is worth holding.
//
// "asks the API for a short-lived bus token whenever the previous one nears expiry. Bus
// credentials are therefore never at rest on a runner host, which is what lets a runner sit in a
// zone where a stolen disk must not yield a working queue consumer."
//
// Two properties follow, and neither is optional. It expires, so a disk read after the fact yields
// something that has stopped working. And it is narrow: a runner takes work from its own pool's
// consumer and says what happened, and cannot publish a task, cannot reach another pool's
// consumer, and cannot create or delete anything. The server enforces all of that, which is why
// the test that holds it runs a real one.

// Kind names how a credential is used, because the bus a profile chose decides that: NATS takes a
// user JWT and a seed, and the SQS driver of profile C will take something else entirely.
const Kind = "nats-user-jwt"

// Credentials are what a runner hands to its bus driver.
type Credentials struct {
	Kind string `json:"kind"`
	URL  string `json:"url"`

	// JWT says what this user may do and Seed proves it is that user. The seed is minted
	// here and travels once, which is the whole design: a runner that never generated a key
	// has nothing on disk to steal between one credential and the next.
	JWT  string `json:"jwt"`
	Seed string `json:"seed"`

	ExpiresAt time.Time `json:"expires_at"`
}

// Issuer mints them, out of the account key the installation holds.
//
// The account seed never leaves the control plane. It is not the object store's signing key and
// not the secret store's master key: one key, three purposes, is how a weakness in any one of them
// becomes a weakness in all three.
type Issuer struct {
	account nkeys.KeyPair
	url     string
}

// NewIssuer builds one from an account seed, which is the string beginning SA.
func NewIssuer(accountSeed, url string) (*Issuer, error) {
	if url == "" {
		return nil, errors.New("bus: no bus address to mint credentials for")
	}
	account, err := nkeys.FromSeed([]byte(accountSeed))
	if err != nil {
		return nil, fmt.Errorf("bus: that is not an account seed: %w", err)
	}
	if prefix, err := account.PublicKey(); err != nil || len(prefix) == 0 || prefix[0] != 'A' {
		return nil, errors.New("bus: that seed is not an account's, and a user JWT signed by anything else is one no server trusts")
	}
	return &Issuer{account: account, url: url}, nil
}

// ForRunner mints the credential one machine uses to take work from one pool.
//
// What it may do is written out rather than left to a wildcard. `$JS.API.>` would have been one
// line and would have let a runner create and delete streams, which a test found by doing it.
// `_INBOX.>` was one line too, and let a runner hear every message JetStream handed to anybody:
// each task pulled by every runner of every pool, grant included, and each result handed to the
// controller. So its replies come back under an inbox of its own. `$JS.ACK.>` let it acknowledge
// or terminate any of those messages, so it acknowledges on its own pool's consumer alone.
func (i *Issuer) ForRunner(name, pool string, until time.Time) (Credentials, error) {
	if err := validPool(pool); err != nil {
		return Credentials{}, fmt.Errorf("bus: %w", err)
	}
	if err := validRunner(name); err != nil {
		return Credentials{}, fmt.Errorf("bus: %w", err)
	}
	return i.mint(name, until, func(c *jwt.UserClaims) {
		c.Sub.Allow.Add(
			// Replies to its own requests, under its own inbox and nobody else's,
			// and the stop subject, which is the one thing the bus carries that is
			// not work distribution.
			Inbox(name)+".>",
			StopSubject,
		)
		c.Pub.Allow.Add(
			// Taking work from its own pool's consumer, and asking after it.
			"$JS.API.CONSUMER.MSG.NEXT."+Stream+"."+Durable(pool),
			"$JS.API.CONSUMER.INFO."+Stream+"."+Durable(pool),
			// Acknowledging what it took. A task nobody acks is redelivered, which
			// is what at-least-once means and what the idempotency key is for.
			// Both of the forms the server writes an acknowledgement subject in:
			// the first names the stream and the consumer, and the second puts a
			// domain and an account hash before them, which is the form a later
			// server will make its default.
			"$JS.ACK."+Stream+"."+Durable(pool)+".>",
			"$JS.ACK.*.*."+Stream+"."+Durable(pool)+".>",
			// Saying what happened, on its own subject and on nobody else's, which
			// is what makes the runner a result arrives under the one that sent it.
			ResultSubject(name),
		)
	})
}

// Inbox is the prefix a runner's replies come back under, which OpenRunner connects with.
//
// Its own rather than the _INBOX every client shares by default, because JetStream hands a pulled
// message to the inbox the pull request named. A credential allowed to subscribe to every inbox is
// one that hears what is handed to every other client, and a grant heard is a grant that can be
// redeemed before the runner it was handed to gets there: the first redemption binds the task, so
// the one that heard it would hold it. A runner hears under its own name alone, so what it hears
// is what it asked for, and "its reach is the tasks in its hands".
func Inbox(runner string) string { return "_INBOX_" + runner }

// ForControlPlane mints the credential the API and the controller use.
//
// Wide inside the account and nothing outside it: this is the side that creates the streams, plans
// the work and reads the results back.
func (i *Issuer) ForControlPlane(name string, until time.Time) (Credentials, error) {
	return i.mint(name, until, func(*jwt.UserClaims) {})
}

func (i *Issuer) mint(name string, until time.Time, scope func(*jwt.UserClaims)) (Credentials, error) {
	if name == "" {
		return Credentials{}, errors.New("bus: a bus credential for nobody")
	}
	if until.IsZero() {
		return Credentials{}, errors.New("bus: a bus credential that never expires, and one that never expires is one a stolen disk still holds")
	}

	user, err := nkeys.CreateUser()
	if err != nil {
		return Credentials{}, fmt.Errorf("bus: a user key could not be minted: %w", err)
	}
	public, err := user.PublicKey()
	if err != nil {
		return Credentials{}, fmt.Errorf("bus: a user key could not be read: %w", err)
	}
	seed, err := user.Seed()
	if err != nil {
		return Credentials{}, fmt.Errorf("bus: a user key could not be written: %w", err)
	}

	claims := jwt.NewUserClaims(public)
	claims.Name = name
	claims.Expires = until.UTC().Unix()
	scope(claims)

	signed, err := claims.Encode(i.account)
	if err != nil {
		return Credentials{}, fmt.Errorf("bus: the credential could not be signed: %w", err)
	}
	return Credentials{
		Kind: Kind, URL: i.url, JWT: signed, Seed: string(seed),
		// The instant the credential actually carries rather than the one the caller
		// asked for. A JWT expires on a whole second, and a runner that renewed against
		// a number a fraction later than the server enforces would renew a fraction
		// late every time.
		ExpiresAt: time.Unix(claims.Expires, 0).UTC(),
	}, nil
}

// Durable is the name of the one consumer a pool's runners share.
//
// One durable consumer per pool rather than one per machine: "a runner asks for a batch of tasks
// when it has room, which makes distribution naturally proportional to each host's real capacity",
// and that is what a shared pull consumer does. A consumer per machine would make the bus decide
// which host gets what, which is the modelling of load the pull design exists to avoid.
func Durable(pool string) string { return pool }
