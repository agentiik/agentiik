// Package db is where the state lives.
//
// PostgreSQL holds the runs, the steps, the tasks, the approvals, the workflows and their
// versions, the artifact references and the runner inventory. It does not hold an envelope
// or a log: the documentation is explicit that it "keeps only their digests and URIs", and
// the bytes are in the object store, which is package artifact's business.
//
// Nothing in package graph or package driver may import this. Those two stay importable
// libraries "with no server, no bus and no database behind them", which is what makes
// agk run --local the same code path a server run takes instead of a second implementation
// that drifts. A test holds that boundary from the other side.
//
// # The namespace is carried by the database, not by discipline
//
// "Object keys are prefixed by namespace, and every query path carries the namespace so
// that a missing filter fails closed rather than returning another tenant's rows."
//
// Fails closed is a stronger promise than is filtered, and a forgotten WHERE in Go is a
// full table read rather than an empty one. So the rule is not a convention this package
// asks callers to follow: a Pool cannot issue a statement at all. It hands out three
// doors, each of which binds something before any query runs, and row level security on
// every namespaced table reads what was bound.
//
//	In            one namespace, bound for the transaction. The ordinary door.
//	Installation  no namespace, for the work that legitimately has none, with the
//	              reason named at the call site.
//	Session       one pinned connection, for the two things a transaction cannot hold.
//
// With nothing bound, current_setting answers null, the policy predicate is null, and a
// read returns no rows. Not an error, not another tenant's rows: nothing. A write stamped
// with a namespace other than the one bound is refused by the same policy's check, so a
// row cannot be planted in a namespace the caller is not in either.
//
// The policy is FORCE, so it applies to the role that owns the tables as well, and Open
// refuses a connection whose role bypasses row level security, because a policy a
// superuser walks through is a policy that protects nothing on the installation that
// matters.
//
// # Installation is a door and not a loophole
//
// Three things in the documentation have no namespace and would read an empty database
// without it: the controller "sweeps for actionable work on a fixed interval", which is
// the correctness guarantee of the whole scheduler; the three purges and the collector
// run across the installation; and a heartbeat "covers every in-flight task on that host",
// which is one host across several namespaces. Each call names its Reason, the set is
// small and closed, and a test counts the call sites, so an escape is something somebody
// added on purpose and can be read back rather than a habit.
//
// # Session is the third door
//
// Two things the documentation requires are properties of a connection and not of a
// transaction. The controller is elected by "a session-level PostgreSQL advisory lock",
// which "is held until explicitly released or the session ends", and it listens: "the API
// signals the controller with NOTIFY" and the controller holds LISTEN. A pooled connection
// handed back after a transaction would carry a LISTEN into the next caller and would drop
// the lock the moment it was recycled. Session pins one connection for the life of the
// holder and is not a door a namespaced handle can borrow.
//
// # The artifact, the envelope and the log
//
// The chapter draws one line through all three: "Envelopes and logs are not stored in the
// database: it keeps only their digests and URIs." So what is here is a digest, a URI, a
// size and a count, and the bytes are package artifact's.
//
// An artifact is a reference and an object, which the schema splits because expiry does:
// "Expiry applies to the reference, never to the object. The row in artifacts binding a run,
// a step and a port to a digest is what is dropped; the physical sha256/<digest> object is
// collected once its reference count reaches zero, and not before." A reference that has been
// dropped is kept rather than deleted, so that a later request can be answered 410 and not
// 404, and it carries its own size and media type: it has to outlive the object it names.
//
// An envelope is counted in the same place, for the reason two runs share an object in the
// first place. Two steps publishing identical bytes publish one object, so an envelope purge
// that deleted by digest alone would delete what another step still names. There is one
// counter for every kind of object rather than one per kind.
//
// A log is neither. One task wrote it, nothing else names it, and the row keeps the URI, the
// line count and whether it was capped. Purging one is a deletion and not a decrement.
//
// # What this package does not settle
//
// A wrong namespace is not a missing one. Every mechanism here checks that a namespace is
// bound and none checks that it is the right one: In(ctx, whateverTheRequestAsked) passes
// the handle, the policy and every test. The value has to come from the authorisation
// decision and never from what a request asserted about itself, which is one rule left to
// a reviewer, and one is a number a reviewer can hold.
package db
