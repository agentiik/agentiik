// Package secret is the built-in secret store, and the shape every other one fills.
//
// "Envelope encryption with AES-256-GCM data keys, wrapped by a master key held outside the
// database." That sentence is the whole of what the documentation fixes, and it fixes the two
// things that matter: a value is never in the database in the clear, and the key that would open
// it is never in the database at all.
//
// Everything else here is a decision this package had to make because the page does not, and
// each one is written beside the code that makes it. What wraps a data key, what a sealed value
// looks like, what is bound into the additional data, how a nonce is chosen, and what rotation
// means: none of that is on the page, and inventing it quietly would have been worse than
// inventing it out loud.
//
// The first version of this package got four of those wrong, and the tests in exploits_test.go
// are what found them: a version read out of the record it was meant to authenticate, so a row
// restored from a backup undid a rotation; a JSON binding that collided for two names differing
// in an invalid byte, so one secret's ciphertext opened as another's; a nonce handed to GCM
// without a length check, which panics rather than refusing; and a wrapping layer that used the
// master key directly, which is one long-lived key across every secret an installation holds.
// Each of them is kept as the test that would catch it coming back.
//
// # What the API alone may do
//
// "The API is the only component that reads one." The controller "names which secret a task may
// have and never sees its value", and a runner obtains it "at the last moment, by redeeming at
// the API the per-task grant". So this package is linked into the API's process and into nothing
// else, and the test that holds that boundary is the same idiom the evaluator and the driver
// already have.
//
// Linked into the process, and not imported by package api, because the command line imports
// package api as well. So api holds the two interfaces a store fills, Secrets for the redemption
// and Values for a declaration's PUT, and none of what fills them; this package fills them, Attach
// wires them into the API's options, and the server's own main package is what calls it.
//
// # Where a value is read from
//
// A namespace declares where each of its secrets lives, and Providers reads one through that
// declaration from the store it names. Two stores are read in this version: builtin, which is
// this package's own, and env, the API's environment, which is for development and read only
// where the installation opts a namespace in and under the prefix it gives it. vault is a name a
// declaration may carry and a store nothing reads yet.
//
// # Rotation is a write
//
// "No role reads a secret value through the API. Rotation is a write, never a read-then-write."
// There is therefore no function here that hands a caller a value in order to put it back: a new
// value is sealed under a data key of its own, and the old ciphertext is replaced. A store whose
// rotation went through a read would be a store with a reason to read, and the reason is what
// gets used for something else eventually.
//
// Rotating the master key is a different question and the page does not settle it. Reading the
// sentence above as covering it too would leave an installation unable to retire a key at all: a
// value written two years ago and never touched again would keep the key that sealed it in
// service for ever, and a rotation an installation can start and never finish is not a rotation.
// So Keyring holds the key everything new is sealed under and the older ones it can still open
// with, and Reseal moves one value from one to the other without the value leaving this package.
// Nothing there answers a caller with a value: what goes in is a Sealed from the database and
// what comes out is a Sealed to put back.
package secret
