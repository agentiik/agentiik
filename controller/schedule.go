package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/cron"
	"github.com/agentiik/agentiik/trigger"
)

// Schedules that fire with nobody present.
//
// Only the controller that leads fires them, and each firing is one transaction, fenced by the
// term's token, that starts the run and moves the schedule's row on to its next occurrence: a
// controller that lost the lead is refused before either, and the one that took it over reads the
// row where the last firing left it. So a failover neither skips an occurrence nor fires one twice.

// Missed is how late the controller may come to an occurrence and still fire it: past it, the
// occurrence was missed while no controller led, and catch_up says whether it is made up.
//
// Ten minutes, well past the longest failover, the half minute a lost session takes to free the
// lock and the moment a standby takes to see it free, so that no failover counts as an outage and
// skips an occurrence, and short enough that a control plane down for longer is the outage
// catch_up: false is written for. An occurrence reached within it fires, late, whatever catch_up
// says; one reached after it is skipped, or made up where catch_up is true.
const Missed = 10 * time.Minute

// catchUpBatch bounds how many occurrences of one schedule a pass fires, so that a schedule every
// minute catching up a day does not hold the pass: the rest are fired by the passes after it, a
// second apart.
const catchUpBatch = 100

// declaredSchedule is a schedule as its trigger row declares it.
type declaredSchedule struct {
	Cron     string `json:"cron"`
	Timezone string `json:"timezone"`
	Jitter   string `json:"jitter"`
	CatchUp  bool   `json:"catch_up"`
}

// Fire fires every schedule whose occurrence is due, starting each run by the one path every run
// takes. A schedule that cannot fire, its version gone or its inputs refused, starts no run and says
// why on its row, as a skipped firing, and moves on: the trigger is not broken, and it fires again
// when it next comes round. trouble hears what could not be done that was the installation's.
func (co *Core) Fire(ctx context.Context, starter *trigger.Starter, trouble func(error)) error {
	now := co.now()
	var due []db.Due
	if err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		due, err = w.DueSchedules(ctx, now, 1000)
		return err
	}); err != nil {
		return err
	}
	for _, d := range due {
		err := co.fireSchedule(ctx, starter, d, now)
		switch {
		case err == nil:
		case errors.Is(err, db.ErrFenced), ctx.Err() != nil:
			return err
		default:
			trouble(fmt.Errorf("controller: the schedule %d of %s/%s could not fire: %w", d.Position, d.Namespace, d.Workflow, err))
		}
	}
	return nil
}

// fireSchedule fires what is due of one schedule, occurrence by occurrence, each in a fenced
// transaction of its own.
func (co *Core) fireSchedule(ctx context.Context, starter *trigger.Starter, d db.Due, now time.Time) error {
	var s declaredSchedule
	if err := json.Unmarshal(d.Declared, &s); err != nil {
		return err
	}
	schedule, err := cron.Parse(s.Cron)
	if err != nil {
		return err
	}
	zone, err := cron.Zone(s.Timezone)
	if err != nil {
		return err
	}
	jitter, err := graph.ParseDuration(s.Jitter)
	if err != nil {
		return err
	}

	for i := 0; i < catchUpBatch && !d.FireAt.After(now); i++ {
		occurrence := d.DueAt
		f := db.Firing{For: occurrence, At: now}
		var prepared trigger.Prepared
		if missed := now.Sub(d.FireAt) > Missed; missed && !s.CatchUp {
			// "An occurrence missed during an outage is not made up": skipped, and the schedule
			// moved on to the first occurrence the controller is not too late for, which fires
			// in this pass where it is due already.
			f.Skipped = fmt.Sprintf("the occurrence of %s was missed while no controller led, and catch_up is false", occurrence.UTC().Format(time.RFC3339))
			from := now.Add(-Missed)
			if from.Before(occurrence) {
				from = occurrence
			}
			f.Next = schedule.Next(from, zone)
		} else {
			var err error
			prepared, err = starter.Prepare(ctx, trigger.Request{
				Namespace: d.Namespace, Workflow: d.Workflow, Kind: agk.TriggerSchedule, Commit: d.Commit,
				Context: db.TriggerContext{Trigger: map[string]any{"scheduled_for": occurrence.UTC().Format(time.RFC3339)}},
				Detail:  map[string]any{"scheduled_for": occurrence.UTC().Format(time.RFC3339)},
			})
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				f.Skipped = "no run could be started: " + err.Error()
			}
			f.Next = schedule.Next(occurrence, zone)
		}
		if f.Next.IsZero() {
			return fmt.Errorf("%q comes round no more after %s", s.Cron, occurrence)
		}
		f.FireAt = f.Next.Add(trigger.Draw(time.Duration(jitter)))

		err := co.controller.Fenced(ctx, co.term, func(ctx context.Context, w *db.Wide) error {
			return w.Within(ctx, d.Namespace, func(ctx context.Context, ns *db.NS) error {
				if err := ns.Held(ctx, d); err != nil {
					return err
				}
				if f.Skipped == "" {
					run, err := prepared.Create(ctx, ns)
					var reached *db.RunsPerHourReached
					switch {
					case errors.As(err, &reached):
						// "A scheduled or event firing starts no run and is recorded as a skipped
						// firing, with that reason."
						f.Skipped = reached.Reason()
					case errors.Is(err, db.ErrWorkflowMoving):
						f.Skipped = "the workflow is being moved to another namespace, and starts no run until it is"
					case err != nil:
						return err
					default:
						f.Run = run
					}
				}
				return ns.Fired(ctx, d, f)
			})
		})
		if errors.Is(err, db.ErrScheduleMoved) {
			// Another pass fired it, or a push armed the schedule anew: nothing to do here.
			return nil
		}
		if err != nil {
			return err
		}
		d.DueAt, d.FireAt = f.Next, f.FireAt
	}
	return nil
}
