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
// the API the per-task grant". So this package is imported by the API and by nothing else, and
// the test that holds that boundary is the same idiom the evaluator and the driver already have.
//
// # Rotation is a write
//
// "No role reads a secret value through the API. Rotation is a write, never a read-then-write."
// There is therefore no function here that hands a caller a value in order to put it back: a new
// value is sealed under a data key of its own, and the old ciphertext is replaced. A store whose
// rotation went through a read would be a store with a reason to read, and the reason is what
// gets used for something else eventually.
package secret
