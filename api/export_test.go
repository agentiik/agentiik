package api

import (
	"context"
	"time"
)

// BetweenShipTransactions has f run between the two transactions of every shipment s takes, which
// is where another shipment of the same log can be taken.
func BetweenShipTransactions(s *RunnerAPI, f func()) { s.betweenShip = f }

// StreamTiming gives the log streams s serves the times given in place of the defaults, which are
// seconds and minutes a test cannot wait for.
func StreamTiming(s *Server, sweep, pace, keepAlive, reauthorise, settling time.Duration) {
	s.streaming = streamTiming{sweep: sweep, pace: pace, keepAlive: keepAlive, reauthorise: reauthorise, settling: settling}
	s.logs.sweep = sweep
}

// Hashing gives s the turns to hash and the wait for one given in place of those sized from the
// machine, and answers the function that takes one of its turns and holds it until the function it
// answers is called.
func Hashing(s *PasswordAPI, turns int, wait time.Duration) func() func() {
	s.hashing = newHashing(turns, wait)
	return func() func() {
		done, ok := s.hashing.turn(context.Background())
		if !ok {
			panic("no turn to hash came in time")
		}
		return done
	}
}

// BetweenChecksAndSignIn has f run between the checks of every password sign-in s answers, and of
// every password or TOTP generator it sets or removes, and the transaction that acts on them, which
// is where what was checked can change.
func BetweenChecksAndSignIn(s *PasswordAPI, f func()) { s.checked = f }

// BetweenVerifyAndRegister has f run between the verification of every registration s answers and
// the transaction that writes it, which is where the account and its policy can change.
func BetweenVerifyAndRegister(s *PasskeyAPI, f func()) { s.checked = f }

// BetweenIdentifyAndSetPolicy has f run between the checks of every policy s sets and the
// transaction that writes it, which is where the bootstrap can end.
func BetweenIdentifyAndSetPolicy(s *PolicyAPI, f func()) { s.checked = f }

// Questions is how many transactions p has opened to say who a principal is and what it holds.
func Questions(p *Principals) int64 { return p.questions.Load() }

// CheckTree is checkTree, the transport's own rules a pushed tree is held to before any version
// is judged, which a test reaches without a database.
var CheckTree = checkTree

// ManifestsCarried is manifestsCarried, the manifests a push carries as version.Check reaches them.
var ManifestsCarried = manifestsCarried

// Reencode is what PUT /api/v1/me/avatar stores of a photo sent: a PNG, with its size in pixels.
func Reencode(raw []byte) ([]byte, int, int, error) { return reencode(raw) }
