// Package control is the task bus from the controller's side. It fills controller.Queue over a
// *bus.Bus, writing what the controller decided as the task message the wire describes, and hands
// the controller what runners reported as the answer it takes.
//
// It is a package of its own so that package bus links no controller: a runner links package bus,
// and the documentation of package bus says why a runner links no controller and no database.
// Nothing in the controller knows either package exists, which is the arrangement graph.Driver and
// package driver already have.
package control

import (
	"context"
	"errors"
	"fmt"

	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/graph"
)

// Queue is the task bus as the controller sees it: controller.Queue, and the results runners
// reported, which Answers hands back.
type Queue struct {
	bus *bus.Bus
}

// New is a queue over a bus the control plane opened with bus.Open, which is the connection that
// makes sure both streams are there. A runner's connection, from bus.OpenRunner, may publish no
// task and takes no result back.
func New(b *bus.Bus) *Queue { return &Queue{bus: b} }

var _ controller.Queue = (*Queue)(nil)

// Publish puts one task on the queue its labels select.
//
// The message carries the task as the wire describes it, which messageOf writes, and bus.Publish
// says what the stream deduplicates it on and why.
func (q *Queue) Publish(ctx context.Context, d controller.Dispatch) error {
	m, err := messageOf(d)
	if err != nil {
		return fmt.Errorf("bus: %w", err)
	}
	return q.bus.Publish(ctx, m)
}

// Stop asks for a task in flight to be stopped, as bus.Stop does and says why.
func (q *Queue) Stop(ctx context.Context, s graph.Stop) error { return q.bus.Stop(ctx, s) }

// Answers hands every result to fn until ctx is done, as the controller takes it.
//
// It is bus.Reports with the controller's rules on top, and that says when a result is
// acknowledged and when it comes round again. A result is handed on with its outputs as digests,
// and fn returning an error that wraps controller.ErrNotAResult has it taken off the queue and said
// out loud through the bus's Trouble, since no delivery would change it, where any other error
// leaves it for a later delivery. So is one naming a runner other than the one whose subject it
// came on, as controller.ErrNotTheHolder, for the reason bus.ResultSubject gives.
func (q *Queue) Answers(ctx context.Context, fn func(context.Context, controller.Answer) error) error {
	if fn == nil {
		return errors.New("bus: consuming results with nothing to hand them to")
	}
	return q.bus.Reports(ctx, func(ctx context.Context, sender string, r bus.TaskResult) error {
		a, err := answerOf(r)
		if err != nil {
			return bus.Drop(fmt.Errorf("a result could not be read: %w", err))
		}
		// The subject is who sent it, and the result is taken as that runner's word or not
		// at all. One naming somebody else is a machine of the pool speaking for another,
		// the same on every delivery, and the controller is not shown it.
		if sender != a.Runner {
			return bus.Drop(fmt.Errorf("%w: %w: the result of %s names %s and was published by %s", controller.ErrNotAResult, controller.ErrNotTheHolder, a.Result.Task, a.Runner, sender))
		}
		if err := fn(ctx, a); err != nil {
			if errors.Is(err, controller.ErrNotAResult) {
				// Readable, and still nothing a controller could ever record: it
				// would be the same on every delivery, and the bus delivers
				// without limit.
				return bus.Drop(err)
			}
			return err
		}
		return nil
	})
}
