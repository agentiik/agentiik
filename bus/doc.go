// Package bus is the task bus, from the two sides that touch it.
//
// "NATS JetStream, with WorkQueue retention, where a message is removed as soon as it has been
// consumed, which is precisely what work distribution needs. Consumers are durable and of the
// pull kind: a runner asks for a batch of tasks when it has room, which makes distribution
// naturally proportional to each host's real capacity without the controller having to model
// load."
//
// It fills controller.Queue and nothing in the controller knows it exists, which is the same
// arrangement graph.Driver and package driver already have. That seam is not decoration: the
// deployment chapter substitutes SQS for the whole of this on one profile, and a substitution
// is only possible where the contract is narrow enough to state.
//
// # Why pull and not push
//
// The controller "does not choose a machine, and it does not start one", and a runner "opens no
// listening port and is never connected to". A pull consumer is what makes both sentences true
// at once: the platform puts work on a queue and a runner takes it when it has room, so nothing
// in the control plane models a host's load and nothing reaches into a network somebody else
// controls.
//
// # Why one stream and a subject per pool
//
// A runner declares labels and "runs_on selects the runner pool". A subject per pool is what
// lets a runner's consumer filter on the work it can take without reading the rest, which is
// what a filtered durable consumer is for. The AWS profile gives up exactly this and pays for it
// with a queue per pool, "which is why each runner pool gets its own queue".
//
// # What at-least-once costs and who pays it
//
// "JetStream guarantees at-least-once delivery. Every task is therefore built to be replayable:
// it carries an idempotency key run_id/step/attempt/shard, and the runner refuses to start a
// container for a key that has already completed." So nothing here deduplicates. A message
// delivered twice is expected, the key is what makes the second one harmless, and a bus that
// tried to promise exactly-once would be a bus promising something it cannot keep and a runner
// that had stopped checking.
package bus
