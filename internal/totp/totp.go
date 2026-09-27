// Package totp computes and checks the codes of a TOTP generator, RFC 6238 over RFC 4226's HOTP, as
// the authenticator applications people hold compute them: HMAC-SHA-1, a step of 30 seconds, 6
// digits. "TOTP exists only alongside a password", as its second factor.
//
// A code is accepted at the step it names and one step either side, so that a clock a few seconds
// off, or a code typed as the step turned, still signs in, which RFC 6238 §5.2 recommends at most.
// That makes each code good for up to a minute and a half, and a code seen over a shoulder good for
// as long; so no code is accepted twice. Match answers the step a code was accepted at, and the
// caller keeps it and never accepts that step, or one before it, again, as §5.2 asks: "the verifier
// MUST NOT accept the second attempt of the OTP after the successful validation has been issued
// for the first OTP".
//
// SHA-1 here is HMAC-SHA-1, whose security rests on the key and not on SHA-1's resistance to
// collisions, and it is the one algorithm every authenticator application computes.
package totp

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"time"
)

// Step, Digits and Skew are RFC 6238's defaults, which every authenticator application reads a
// secret with when it is told nothing else: a code a 30-second step names, 6 digits long, accepted
// one step either side of the verifier's.
const (
	Step   = 30 * time.Second
	Digits = 6
	Skew   = 1
)

// modulus is 10 to the Digits: a code is the remainder of the truncated HMAC by it.
const modulus = 1_000_000

// StepAt is the time step t falls in: the whole steps since the Unix epoch.
func StepAt(t time.Time) int64 {
	seconds := t.Unix()
	step := int64(Step / time.Second)
	if seconds < 0 {
		// Rounded down, since a step begins at a multiple of 30 seconds.
		return (seconds - step + 1) / step
	}
	return seconds / step
}

// Code is the code of secret at step: HOTP (RFC 4226 §5.3) over the step as its counter, with
// HMAC-SHA-1, its dynamic truncation, and the last Digits digits, zero padded.
func Code(secret []byte, step int64) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", Digits, value%modulus)
}

// Match answers the step code is secret's code at, among the steps within Skew of now and after the
// step last accepted, the earliest of them where two match, and whether one did. after is the step a
// code was last accepted at, and a code of that step or of one before it is not accepted again; any
// step before the clock's, 0 among them, where none was.
//
// Every step within Skew is computed and compared, in constant time, whether or not one matched
// already, so that the time an answer takes says nothing about which step matched.
func Match(secret []byte, code string, now time.Time, after int64) (int64, bool) {
	at := StepAt(now)
	var found int64
	matched := false
	for step := at - Skew; step <= at+Skew; step++ {
		same := subtle.ConstantTimeCompare([]byte(Code(secret, step)), []byte(code)) == 1
		if same && step > after && !matched {
			found, matched = step, true
		}
	}
	return found, matched
}
