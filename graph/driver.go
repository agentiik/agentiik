package graph

import "context"

// Driver is what runs a container. It is stated here and called by nothing here.
//
// Stating it on this side is the point. The evaluator says which task should run and
// what it carries; something else runs it, and both halves agree about the shape of the
// handover because the shape is written once, next to the values that cross it. What
// this package must never grow is a call to either method: the loop that reads a Plan,
// hands each Task to a Driver and feeds each Result back belongs to cmd/agk and to the
// controller, and putting it here would be the evaluator executing.
//
// Stop is a method rather than a cancelled context because a task in flight may be held
// by a runner in another process, where a context does not reach. Cancelling a context
// stops the caller waiting; it does not stop a container on another machine, and the
// three rules that call off running work need the container stopped and not the waiting.
//
// A test fakes the whole of it in four lines, a map from step name to Result, which is
// what keeps every rule in this package testable with nothing behind it.
type Driver interface {
	Run(ctx context.Context, t Task) (Result, error)
	Stop(ctx context.Context, s Stop) error
}
