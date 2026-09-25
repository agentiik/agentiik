// Package diff compares envelopes with the facts that belong to a run held aside.
//
// It is a package rather than a function in one command because two callers need it and
// they must not disagree. agk brick test compares what a brick produced against what a
// fixture expected; the proof of v0.1.0 compares what a workflow produced against what the
// same workflow produced on the same inputs a second time. If each had its own idea of what
// "the same envelopes" means, the milestone sentence would mean whatever the second one
// happened to decide, and that sentence is a test rather than a claim.
//
// # What is held aside, and nothing else
//
// A second run is a different run and says so. Three things differ by definition:
//
//	meta.run_id        which run this was
//	meta.produced_at   when it was produced, stamped by the runner and by agk.Concat
//	the run segment of every files[] uri, since an artifact is addressed
//	                   agk://run/<run>/<step>/<port>/<name>
//
// Those three are facts about which run this was and not about what it produced, so the
// default holds them aside. Everything else is compared exactly: the step and the attempt a
// batch was published by, item identities, item data, counts, port names, file names, media
// types, sizes and every sha256. An artifact digest is compared because a comparison that
// skipped the artifacts would pass a run that produced different bytes.
//
// Item identities are compared, which is the assertion with the most behind it. An item
// holds its own identity so that a shard, a join and a replay can speak about it after the
// batch has been split, concatenated or reordered, and a brick that mints a fresh
// identifier on every pass has thrown that away. That is why the helper has --id and why
// the proof's fixture derives every identity from its payload rather than minting one.
//
// Ignore names what a caller chose to hold aside, so that agk brick test's --ignore flag is
// one value and not a set of booleans, and so that a test can state in one place which
// facts it gave up. Its spellings are the envelope's own: meta.run_id, meta.produced_at,
// items.id, files.uri. Identifiers are never prettified, here as everywhere.
//
// # What a difference reads as
//
// One line naming the port, the item and the member, with what was wanted and what was
// got. Never a whole-document dump: a reader holding two envelopes of two hundred items
// needs the one member that moved, and a diff of two pretty-printed documents makes them
// find it themselves. The order is the port, then the item, then the member, which is the
// order somebody would look in.
//
// # What this package refuses to be
//
// No JSON diff library and no structural differ. An envelope is a shape agk already states,
// and comparing it member by member is what lets a difference name the member in the
// envelope's own vocabulary rather than as a JSON Pointer. Nothing here reads a file, a
// daemon or a store.
//
// # Layout
//
//	envelopes.go   Envelopes, comparing port by port and item by item
//	ignore.go      Ignore, its default, and the spelling the flag parses
//	difference.go  Difference and the one line it reads as
package diff
