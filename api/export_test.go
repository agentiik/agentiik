package api

// BetweenShipTransactions has f run between the two transactions of every shipment s takes, which
// is where another shipment of the same log can be taken.
func BetweenShipTransactions(s *RunnerAPI, f func()) { s.betweenShip = f }
