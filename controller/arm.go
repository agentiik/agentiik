package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/trigger"
)

// Arm arms, for every workflow whose default branch's head is not what it has armed, what the head
// declares under on, as the installation.
//
// A push, a change of default branch and a tree push arm their head in their own transaction, so
// that no head is ever without its triggers; what is left for the controller that leads are the
// heads nobody armed: those pushed before v0.5.0, which the upgrade arms nothing of, and one whose
// arming a request could not finish. Each is armed in a fenced transaction of its own, so that one
// that cannot be leaves the others armed. A head that cannot be, one its webhook's path collides
// with another workflow's or one the rules of this release refuse, is reported through trouble and
// left: the first is armed once the path is free, the second is recorded as armed with nothing, so
// that it is not tried again until a version that passes lands.
func (co *Core) Arm(ctx context.Context, trouble func(error)) error {
	var unarmed []db.Unarmed
	if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		unarmed, err = w.Unarmed(ctx)
		return err
	}); err != nil {
		return err
	}
	for _, u := range unarmed {
		var unarmable error
		err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
			return w.Within(ctx, u.Namespace, func(ctx context.Context, ns *db.NS) error {
				err := trigger.Reconcile(ctx, ns, u.Workflow, installationActor, co.now())
				if errors.Is(err, trigger.ErrUnarmable) {
					// Committed all the same: the workflow is disarmed and recorded as armed with
					// nothing, and the trouble is said once.
					unarmable = err
					return nil
				}
				return err
			})
		})
		var taken *db.HookTaken
		switch {
		case errors.Is(err, db.ErrFenced), ctx.Err() != nil:
			return err
		case errors.As(err, &taken):
			trouble(fmt.Errorf("controller: %s/%s is not armed: %w", u.Namespace, u.Workflow, err))
		case err != nil:
			trouble(fmt.Errorf("controller: %s/%s could not be armed: %w", u.Namespace, u.Workflow, err))
		case unarmable != nil:
			trouble(fmt.Errorf("controller: %s/%s arms nothing: %w", u.Namespace, u.Workflow, unarmable))
		}
	}
	return nil
}
