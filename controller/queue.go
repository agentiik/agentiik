package controller

import (
	"context"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
)

// What the controller hands work to, and where it gets a graph from. Both are stated here and
// called by nothing here, which is the idiom graph.Driver already sets: the shape of a handover
// is written once, beside the values that cross it, and the thing that fills it lives elsewhere.

// Queue is the task bus, from the one side the controller sees.
//
// "For each of them it creates one task per shard and publishes those tasks on the queue that
// matches the step's runs_on labels. It does not choose a machine, and it does not start one."
// The queue is the pool's, and the pool is chosen here, where the pools are read: the one whose
// labels include every label of the step's runs_on, among the pools the namespace may reach. So a
// Dispatch names it, and an implementation publishes to it rather than choosing again from labels
// alone, which would be a second choice free to differ from the one whose policy was applied.
//
// Stop is a method rather than a cancelled context for the reason graph.Driver gives: "a task
// in flight may be held by a runner in another process, where a context does not reach".
//
// An implementation is the bus group's. A test fakes the whole of it in a dozen lines, which is
// what keeps every rule in this package testable with no bus behind it.
type Queue interface {
	// Publish puts one dispatch on the queue of the pool it names. It is called after the
	// decision that planned it has committed, never before, because the database is the
	// record and the bus is a courier: a message published against a transaction that then
	// rolled back is work on a queue that no row accounts for and that nothing can recall.
	//
	// Publishing twice is expected rather than guarded against. The bus is at-least-once
	// anyway, the identifier is the idempotency key, and "a runner refuses to start a
	// container for a key that has already completed".
	Publish(ctx context.Context, d Dispatch) error

	// Stop asks for a task in flight to be stopped, for one of the four reasons
	// graph.StopReason names. A refusal is worth reporting and is not worth abandoning the
	// decision for: the task is already being stopped by something, or it has already
	// ended, and the next pass will say so either way.
	Stop(ctx context.Context, s graph.Stop) error
}

// Dispatch is one task, as much of it as leaves this process.
//
// It is the task the evaluator decided plus the three things only the controller can supply: the
// row the task is known by outside the graph, the grant that turns the names in the message into
// values, and the digest of each input port's envelope. The last of those is why this type exists
// at all: graph.Task carries the whole envelope, because that is what an evaluator hands a driver
// in one process, and a task message carries "no business payload", so somebody has to have
// written those bytes down and know what they are called.
type Dispatch struct {
	Task graph.Task

	// Row is the task's own identifier, the ULID the tasks table is keyed by. It is not the
	// idempotency key: the key says which unit of work this is, the row says which record,
	// and a grant and a log are both addressed by the row.
	Row string

	// Grant is the clear value, which exists here and in the message and nowhere else.
	Grant string

	// Inputs are the envelopes the container will be given, by digest and count. The bytes
	// are in the object store, put there by the controller before this was built, and the
	// runner fetches them by redeeming the grant.
	Inputs map[agk.Port]InputRef

	// Pool is the runner pool the task goes to, chosen from the task's runs_on in the
	// transaction that issued its grant, which is where the pool's policy was applied.
	Pool string
}

// InputRef is one input port's envelope, named rather than carried.
type InputRef struct {
	// Digest is sixty-four lowercase hexadecimal characters, as an envelope's digest is
	// written everywhere else.
	Digest string
	Items  int
}

// Versions hands out the resolved graph of one workflow version.
//
// The controller does not fetch a repository tree and does not read a registry: "the API
// authenticates it, authorises it, resolves the ref to a commit, writes the run", and the
// manifests behind graph.Build are read from images. Both are somebody else's, and what the
// controller needs is the answer.
//
// A graph is immutable for the life of a version, which is what makes caching one safe and what
// makes this an interface rather than a function: an implementation that reads the resolved
// graph back out of workflow_versions and one that rebuilds it from the tree are the same thing
// to a caller.
type Versions interface {
	Graph(ctx context.Context, namespace, workflow, commit string) (*graph.Graph, error)
}
