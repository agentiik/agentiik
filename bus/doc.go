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
// # Why a subject per runner for results
//
// Every machine of a pool holds the same kind of credential, and the first ending recorded for
// an attempt is the one that stands. On one results subject a result's runner field would be a
// claim, and any machine of the pool could settle any task of the pool by writing the name of the
// runner that holds it. So each runner publishes on a subject of its own and its credential
// allows no other: the subject a result arrives on is the runner that sent it, the reader refuses
// a result naming anybody else, and the controller holds what is left to the runner the dispatch
// its task_id names was bound to when its grant was redeemed. The dispatch and not the key, since
// a requeue after loss keeps the key and is answered by whoever redeems it, which may be the
// runner that lost the dispatch before it or may not. Or by nobody's redemption at all, where it
// came back to the host that had already ended the key: that host answers it from its record, and
// the controller takes the answer from the runner that redeemed the dispatch the host ended. A
// compromised host's "reach is the tasks in its hands", and this, with an inbox of its own, is
// what keeps it there.
//
// # Why an inbox per runner
//
// JetStream hands a pulled message to the inbox the pull named, and every client's inbox is under
// _INBOX unless it asks for another. A runner allowed to listen there would hear every task handed
// to every runner of every pool, grant included, and could redeem a grant before the runner it was
// handed to: the first redemption binds the task, so the binding would go to whoever listened. So
// a runner's replies come back under Inbox, its credential listens there and nowhere else, and it
// acknowledges on its own pool's consumer alone.
//
// # What at-least-once costs and who pays it
//
// "JetStream guarantees at-least-once delivery. Every task is therefore built to be replayable:
// it carries an idempotency key run_id/step/attempt/shard, and the runner refuses to start a
// container for a key that has already completed." So nothing here deduplicates. A message
// delivered twice is expected, the key is what makes the second one harmless, and a bus that
// tried to promise exactly-once would be a bus promising something it cannot keep and a runner
// that had stopped checking.
//
// # When a runner acknowledges
//
// On take, once the task is written down on the host, and not when the work is over. The runner
// records the key under its work root, which is driver.Docker.Hold, and then acknowledges, before
// it pulls, redeems or creates anything. From the moment the server confirms the acknowledgement,
// which is when Taken.Held answers nil, the task is the host's to answer for and no longer the
// bus's to redeliver: "Liveness therefore lives in the database beside the task state, rather
// than as traffic on a work queue that exists to distribute work." A host that dies holding a
// task stops heartbeating, the task becomes lost, and an idempotent step is requeued under the
// same key. Holding is redeeming, though, since only a task a runner has redeemed can be lost: a
// host that dies between the acknowledgement and the redemption, pulling the image for instance,
// leaves a task nothing hands out again and the heartbeat never finds, which only the run's own
// timeout ends.
//
// Acknowledging at the end would have made the ack wait the longest a step may run. A runner
// that died would then hold its work for that long before anybody else could take it, and a
// runner that lived would have to keep telling the bus so for as long as its container ran, which
// is a second liveness channel beside the heartbeat and one that says less.
//
// What is left to redelivery is a task whose acknowledgement the server never confirmed, which
// comes round again once the pool consumer's AckWait has passed, and the requeue of a lost task.
// A confirmation is the only thing that tells a runner the bus will not hand the task to anybody
// else, so a runner starts nothing without one, and the first of the two never runs a key beside
// itself. Both can hand a host a key it has already run, and the host's record is what refuses
// one it has already carried to an ending, and what answers it: the runner acknowledges the
// message and reports the ending the record holds under the message's task_id, which is
// Bus.Ended. The requeue of a lost task is waiting on that answer, and without it the run would
// wait for its own timeout.
package bus
