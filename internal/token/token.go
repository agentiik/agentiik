// Package token mints the credentials the installation hands out, and stores none of them.
//
// "tokens, join tokens, grants: 256 bits of entropy, stored hashed, shown once at creation, each
// with an expiry." Four sentences, and this package is the first three of them; the expiry
// belongs to whatever row holds the credential, because what a grant expires with is its task
// and what a join token expires with is an hour.
//
// # Why the clear value exists exactly once
//
// A credential is minted, handed to whoever asked, and never recoverable afterwards. What is
// kept is a hash, so a copy of the database is not a set of working credentials, and a listing
// shows every field of a token except the one that would let somebody use it. There is no
// function here that turns a hash back into a credential, and that absence is the design.
//
// # Why the prefix
//
// agkgrant_, agkjoin_ and agkrunner_ say what a credential is before anybody tries it. That is
// worth a few bytes for two reasons: a value that leaks into a log or a bug report can be
// recognised and revoked by whoever finds it, and a value presented to the wrong door can be
// refused for being the wrong kind rather than for failing a lookup that the wrong door would
// have had to perform.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// Kind is what a credential is for, and the prefix it is written with.
type Kind string

const (
	// Grant is the per-task bearer token, "the only thing that turns the names in a task
	// message into values". It carries the task it belongs to in its own text, so that the
	// API can refuse a redemption whose body names a different one "rather than believing
	// either alone".
	Grant Kind = "agkgrant"

	// Join is what an administrator hands to a machine that is about to become a runner.
	// Single use, short lived, bound to one pool and one label set.
	Join Kind = "agkjoin"

	// Runner is the long-lived credential a runner authenticates every later call with.
	Runner Kind = "agkrunner"
)

// Bits is how much entropy every credential carries.
const Bits = 256

// secretLength is how long the secret half is, written base64url without padding.
//
// Forty-three characters, which is the shortest base64url that can hold 256 bits: the wire
// refuses anything shorter for exactly that reason, and minting the minimum rather than more
// keeps the value short enough to paste.
const secretLength = 43

// New mints one credential and answers the clear value and what to store.
//
// id is the identifier the credential names inside its own text, and is empty for the kinds that
// name nothing: a grant carries its task, a join token and a runner credential carry nothing
// because what they are bound to is a row rather than a segment.
//
// The clear value is returned once and is not recoverable from the hash. A caller that loses it
// mints another.
func New(kind Kind, id string) (clear, hashed string, err error) {
	switch kind {
	case Grant:
		if id == "" {
			return "", "", fmt.Errorf("token: a grant names the task it belongs to, and this one names none")
		}
	case Join, Runner:
		if id != "" {
			return "", "", fmt.Errorf("token: a %s carries no identifier in its text, and this one was given %q: what it is bound to is a row", kind, id)
		}
	default:
		return "", "", fmt.Errorf("token: %q is not a kind of credential", kind)
	}
	if strings.ContainsAny(id, "_ ") {
		return "", "", fmt.Errorf("token: %q cannot name a credential: the underscore is what separates the parts of one", id)
	}

	raw := make([]byte, Bits/8)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("token: a credential could not be minted: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	if len(secret) != secretLength {
		return "", "", fmt.Errorf("token: a credential came out %d characters long and the wire expects at least %d", len(secret), secretLength)
	}

	clear = string(kind) + "_"
	if id != "" {
		clear += id + "_"
	}
	clear += secret
	return clear, Hash(clear), nil
}

// Hash is what is stored in place of a credential.
//
// SHA-256 and not a password hash, deliberately. A password is chosen by a person and is guessed
// by trying the likely ones, which is what a slow hash defends against; this is 256 bits from the
// operating system's generator, and there is nothing to guess. What a slow hash would buy here is
// latency on every request a runner makes.
func Hash(clear string) string {
	sum := sha256.Sum256([]byte(clear))
	return hex.EncodeToString(sum[:])
}

// Same says whether a credential is the one a hash was made from.
//
// Compared in constant time, because a comparison that stopped at the first differing byte would
// answer how much of a guess was right, and a credential can be found one byte at a time by
// anybody who can measure that.
func Same(clear, hashed string) bool {
	return subtle.ConstantTimeCompare([]byte(Hash(clear)), []byte(hashed)) == 1
}

// KindOf says what a credential claims to be, and whether it is written like one.
//
// It reads the prefix and nothing else: what a credential actually authorises is a row, and this
// only tells a door whether it is holding the right sort of thing. A value presented at the wrong
// door is refused here rather than after a lookup the wrong door should never have made.
func KindOf(clear string) (Kind, bool) {
	prefix, rest, ok := strings.Cut(clear, "_")
	if !ok || rest == "" {
		return "", false
	}
	kind := Kind(prefix)
	switch kind {
	case Grant:
		id, secret, ok := strings.Cut(rest, "_")
		return kind, ok && id != "" && len(secret) >= 16
	case Join, Runner:
		return kind, len(rest) >= secretLength
	}
	return "", false
}

// TaskOf is the task a grant names inside its own text.
//
// The API reads it so that it can refuse a redemption "where the two disagree rather than
// believing either alone": a request naming one task and a grant naming another is a request
// somebody has assembled out of two, and neither half is evidence about the other.
func TaskOf(grant string) (string, bool) {
	rest, ok := strings.CutPrefix(grant, string(Grant)+"_")
	if !ok {
		return "", false
	}
	id, secret, ok := strings.Cut(rest, "_")
	if !ok || id == "" || len(secret) < 16 {
		return "", false
	}
	return id, true
}
