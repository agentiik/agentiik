package controller

import (
	"context"

	"github.com/agentiik/agentiik/graph"
)

// What the controller hands work to, and where it gets a graph from. Both are stated here and
// called by nothing here, which is the idiom graph.Driver already sets: the shape of a handover
// is written once, beside the values that cross it, and the thing that fills it lives elsewhere.

// Queue is the task bus, from the one side the controller sees.
//
// "For each of them it creates one task per shard and publishes those tasks on the queue that
// matches the step's runs_on labels. It does not choose a machine, and it does not start one."
// That is the whole of the contract, and it is why Publish takes a graph.Task and nothing
// beside it: the task already carries its RunsOn, so routing is something an implementation
// reads rather than something a caller is asked to remember.
//
// Stop is a method rather than a cancelled context for the reason graph.Driver gives: "a task
// in flight may be held by a runner in another process, where a context does not reach".
//
// An implementation is the bus group's. A test fakes the whole of it in a dozen lines, which is
// what keeps every rule in this package testable with no bus behind it.
type Queue interface {
	// Publish puts one task on the queue its labels select. It is called after the
	// decision that planned it has committed, never before, because the database is the
	// record and the bus is a courier: a message published against a transaction that then
	// rolled back is work on a queue that no row accounts for and that nothing can recall.
	//
	// Publishing twice is expected rather than guarded against. The bus is at-least-once
	// anyway, the identifier is the idempotency key, and "a runner refuses to start a
	// container for a key that has already completed".
	Publish(ctx context.Context, t graph.Task) error

	// Stop asks for a task in flight to be stopped, for one of the four reasons
	// graph.StopReason names. A refusal is worth reporting and is not worth abandoning the
	// decision for: the task is already being stopped by something, or it has already
	// ended, and the next pass will say so either way.
	Stop(ctx context.Context, s graph.Stop) error
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
