package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/cron"
	"github.com/agentiik/agentiik/version"
)

// Draw is the jitter a schedule's occurrence is delayed by, at most its jitter: "each occurrence
// starts up to 5m late", so that schedules written on the hour do not arrive as one burst. Drawn
// once, when the occurrence becomes the next, and kept on the trigger's row, so that a failover
// neither draws again nor fires it twice. A variable so that a test can hold it still.
var Draw = func(jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return 0
	}
	return rand.N(jitter + 1)
}

// Entries are the triggers a workflow declares under on, as they are armed in namespace at now: each
// with what it declares, the values in force written in, a schedule with its next occurrence after
// now, and an event trigger with the namespace it hears, namespace where the file names none.
func Entries(wf *graph.Workflow, namespace string, now time.Time) ([]db.Entry, error) {
	var out []db.Entry
	for i, s := range wf.On.Schedule {
		schedule, err := cron.Parse(s.Cron)
		if err != nil {
			return nil, fmt.Errorf("the schedule %d: %w", i, err)
		}
		zone, err := cron.Zone(s.Timezone)
		if err != nil {
			return nil, fmt.Errorf("the schedule %d: %w", i, err)
		}
		declared, err := json.Marshal(map[string]any{
			"cron": s.Cron, "timezone": zone.String(), "jitter": graph.Duration(s.Jitter).String(), "catch_up": s.CatchUp,
		})
		if err != nil {
			return nil, err
		}
		due := schedule.Next(now, zone)
		if due.IsZero() {
			return nil, fmt.Errorf("the schedule %d, %q, comes round no more", i, s.Cron)
		}
		out = append(out, db.Entry{
			Kind: agk.TriggerSchedule, Position: i, Declared: declared,
			DueAt: due, FireAt: due.Add(Draw(time.Duration(s.Jitter))),
		})
	}
	for i, w := range wf.On.Webhook {
		entry := map[string]any{"path": w.Path, "method": w.Method, "auth": w.Auth, "response": w.Response}
		if w.Output != "" {
			entry["output"] = w.Output
		}
		if len(w.Map) > 0 {
			entry["map"] = w.Map
		}
		declared, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		out = append(out, db.Entry{Kind: agk.TriggerWebhook, Position: i, Declared: declared, Path: w.Path, Method: w.Method})
	}
	for i, e := range wf.On.Event {
		hears := e.Namespace
		if hears == "" {
			hears = namespace
		}
		entry := map[string]any{"namespace": hears}
		for k, v := range map[string]string{"type": e.Type, "source": e.Source, "filter": e.Filter} {
			if v != "" {
				entry[k] = v
			}
		}
		if len(e.Map) > 0 {
			entry["map"] = e.Map
		}
		declared, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		out = append(out, db.Entry{Kind: agk.TriggerEvent, Position: i, Declared: declared, Type: e.Type, Source: e.Source, Hears: hears})
	}
	return out, nil
}

// ErrUnarmable is a head whose version cannot be armed: one pushed before the rules it is now read
// by, which it no longer passes. Its triggers are disarmed, and nothing of it fires until a push
// lands a version that passes.
var ErrUnarmable = errors.New("the version on the default branch cannot be armed")

// Reconcile arms what the default branch's head of workflow declares, in the transaction ns
// carries, where it is not what the workflow has armed: after a push moved the branch, after the
// branch was renamed to another, after a tree push recorded the version an unborn branch runs, and
// at the start of a controller's term for a head nobody armed. by is who moved it, and the entries
// are armed as at now. A head with nothing to run, or a library's, disarms everything.
//
// It answers a *db.HookTaken where the head arms a webhook another workflow of the namespace holds,
// and ErrUnarmable, with every trigger of the workflow disarmed and the head recorded as armed with
// nothing, where the head is a version the rules it is now read by refuse: a caller that commits
// what it did anyway leaves the workflow firing nothing until a version that passes lands.
func Reconcile(ctx context.Context, ns *db.NS, workflow, by string, now time.Time) error {
	head, err := ns.DefaultCommit(ctx, workflow)
	if errors.Is(err, db.ErrNoVersion) {
		head, err = "", nil
	}
	if err != nil {
		return err
	}
	armed, err := ns.ArmedCommit(ctx, workflow)
	if err != nil {
		return err
	}
	if armed == head {
		return nil
	}
	if head == "" {
		return ns.Arm(ctx, workflow, "", nil, by, now)
	}
	v, err := ns.Version(ctx, workflow, head)
	if err != nil {
		return err
	}
	g, err := version.Build(v)
	if errors.Is(err, version.ErrLibrary) {
		return ns.Arm(ctx, workflow, "", nil, by, now)
	}
	var entries []db.Entry
	if err == nil {
		err = g.Workflow().Armable()
	}
	if err == nil {
		entries, err = Entries(g.Workflow(), ns.Namespace(), now)
	}
	if err != nil {
		// Recorded as armed with nothing, so that it is not tried again at every pass: what it
		// declares is armed once a version that passes lands.
		if derr := ns.Arm(ctx, workflow, head, nil, by, now); derr != nil {
			return derr
		}
		return fmt.Errorf("%w: %s@%s: %v", ErrUnarmable, workflow, head, err)
	}
	return ns.Arm(ctx, workflow, head, entries, by, now)
}
