// Package controller is the one decider.
//
// "Runs as several instances with one active at a time, elected by a session-level PostgreSQL
// advisory lock. Failover is a state resume, never a rebuild: the state lives in the database,
// not in the process." Everything this package holds in memory is therefore recoverable by
// reading the database, and nothing it knows is lost when it dies.
//
// # Election, and why election alone is not enough
//
// The active controller holds a session-level advisory lock, "not a lease with an expiry".
// PostgreSQL documents such a lock as held "until explicitly released or the session ends", so
// there is no timeout to invent and no clock to trust: the lock frees itself when the process
// exits or when the server notices the connection is gone. Standbys call the non-blocking
// variant on a loop and take over the instant it succeeds.
//
// That is not safe on its own. A controller partitioned from the database loses the lock the
// moment the server notices, and may not learn of it for as long as its own connection takes
// to fail. For that window two processes each believe they are the active one. So "every
// controller write carries the lock's acquisition counter as a fencing token and a write
// bearing an older counter is refused": taking the lock raises a counter, every write checks
// it, and a former holder's write is refused rather than applied late.
//
// The counter is a column rather than the lock's own, because PostgreSQL does not expose an
// acquisition counter for an advisory lock. Everything else about the election is the lock.
//
// # One place writes
//
// The fencing token is only as good as the number of writes that carry it, so this package has
// exactly one function that opens a write transaction, and a test counts them. A write that
// went around it would be a write a former holder could still make, which is the one thing the
// token exists to prevent.
//
// # Woken, and sweeping anyway
//
// "When the API creates a run it commits the row and issues a NOTIFY carrying the run
// identifier. PostgreSQL delivers such an event only once the transaction commits, and only to
// sessions currently listening. A controller that was restarting therefore misses
// notifications, so it also sweeps for actionable work on a fixed interval. The notification is
// a latency optimisation; the sweep is the correctness guarantee."
//
// Both are here, and the asymmetry is deliberate: a notification that never arrives costs
// latency, and a sweep that never runs costs a run. So the sweep runs whether or not anything
// was notified, and a controller that has just taken the term sweeps before it listens.
package controller
