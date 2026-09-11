// Package agk holds the vocabulary the documentation uses and every rule it states
// about it: the envelope that travels on a port, the items and files inside it, the
// names a run, a step and a port are known by, the agk:// URI an artifact is addressed
// from, the four size limits and the two outcomes they produce.
//
// It is the package the evaluator, the driver, the controller and the command line all
// import, so it holds types and pure rules and nothing that touches a store, a
// filesystem, a container or a schema compiler. Standard library only, held there by a
// test, so that importing the core types costs nothing and the choice of a JSON Schema
// implementation or an object store never reaches an importer.
//
// # Reading and writing an envelope
//
// The envelope is validated here by hand rather than through a JSON Schema evaluator.
// The shape is fixed at compile time, it sits on the hot path of every port and every
// item, and a hand written check is what lets a refusal name the item and the member it
// is about instead of printing an instance location. The claim that this agrees with
// envelope.schema.json is not made here: it is checked against the released fixture
// corpus, which pins the schema and the documentation together.
//
// # The two outcomes
//
// A size rule that is broken does one of two things, and they are not the same thing.
// Reject refuses the envelope whole and publishes nothing; Fail is an application
// failure of the emitting step, the exit code table's band 1 to 99, which enters the
// retry policy. Both travel as a Refusal and are told apart with errors.Is against
// ErrEnvelopeRejected and ErrStepFailed.
//
// # The run, and the task the run is made of
//
// The run itself is named here too: the run states and the verdict they end on, the step
// verdict a downstream when reads, the task states a runner reports, the four failures
// retry.on can name, the exit code table, the shard and the identifier a task is
// deduplicated on. None of that is evaluator logic, and it has to sit where both the
// evaluator and the driver can see it, because the driver imports this package and will
// never import the evaluator.
//
// The verdict of a finished run is a terminal RunState and not a second type. Two types
// for one fact is two ways for the halves of the engine to disagree about whether a run
// succeeded.
//
// # Where the documentation is silent
//
// Six readings are taken here and recorded beside the rule that applies them: the length
// imposed on a run identifier, in RunID.Validate; the outcome of max_items, in the
// envelope's own validation; the attempt and publication time a concatenated envelope
// carries, in Concat; the band an exit code below zero falls in, in Band; the shard
// index counted from one, in Shard; and what a task identifier is written as when there
// is no fan-out to name a shard, in NewTaskID.
package agk
