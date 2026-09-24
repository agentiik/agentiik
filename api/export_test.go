package api

import "time"

// BetweenShipTransactions has f run between the two transactions of every shipment s takes, which
// is where another shipment of the same log can be taken.
func BetweenShipTransactions(s *RunnerAPI, f func()) { s.betweenShip = f }

// StreamTiming gives the log streams s serves the times given in place of the defaults, which are
// seconds and minutes a test cannot wait for.
func StreamTiming(s *Server, sweep, pace, keepAlive, reauthorise, settling time.Duration) {
	s.streaming = streamTiming{sweep: sweep, pace: pace, keepAlive: keepAlive, reauthorise: reauthorise, settling: settling}
	s.logs.sweep = sweep
}
