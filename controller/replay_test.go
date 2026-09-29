package controller

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// replayOf creates a replay of the run these tests decide, from a step or from the start.
func replayOf(t *testing.T, pool *db.Pool, id agk.RunID, from agk.Step) {
	t.Helper()
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.CreateRun(ctx, db.NewRun{
			ID: id, Workflow: "monthly-invoicing", Commit: "a3f9c1e",
			Trigger: agk.TriggerManual, TriggeredBy: "alice",
			Inputs:   json.RawMessage(`{"orders": [{"customer_id": "C-1042"}]}`),
			Steps:    []agk.Step{"normalize", "archive"},
			ReplayOf: decidedRun, ReplayFrom: from,
		})
	}); err != nil {
		t.Fatal(err)
	}
}

// "Replay from a step. Reuses the envelopes and artifacts already produced upstream." A replay from
// archive hands out archive alone, with the items normalize published in the run it replays, and
// normalize carries that run's verdict and no task; a replay from the start runs everything.
func TestAReplayFromAStepReusesWhatTheStepsAboveItPublished(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	normalize := q.taken()[0]
	made := succeeded(t, normalize, core.now())
	core.answer(t, made)
	for _, task := range q.taken() {
		core.answer(t, succeeded(t, task, core.now()))
	}

	const replay agk.RunID = "01JMZ8V1P9C7"
	replayOf(t, pool, replay, "archive")
	if err := core.Decide(t.Context(), replay); err != nil {
		t.Fatal(err)
	}
	handed := q.taken()
	if len(handed) != 1 || handed[0].Run != replay || handed[0].Step != "archive" {
		t.Fatalf("the replay handed out %+v", handed)
	}
	in := handed[0].Inputs["orders"]
	if len(in.Items) != 1 || in.Items[0].ID != made.Outputs["ok"].Items[0].ID || in.Meta.RunID != replay {
		t.Errorf("archive is handed %+v", in)
	}
	conn := dbtest.Superuser(t, super)
	var verdict string
	var tasks int
	if err := conn.QueryRow(t.Context(),
		`select s.state, (select count(*) from tasks t where t.run_id = s.run_id and t.step = s.step)
		 from steps s where s.run_id = $1 and s.step = 'normalize'`, string(replay)).Scan(&verdict, &tasks); err != nil {
		t.Fatal(err)
	}
	if verdict != "succeeded" || tasks != 0 {
		t.Errorf("the reused step is %s with %d tasks", verdict, tasks)
	}

	const fromStart agk.RunID = "01JMZ8V1P9C8"
	replayOf(t, pool, fromStart, "")
	if err := core.Decide(t.Context(), fromStart); err != nil {
		t.Fatal(err)
	}
	if handed := q.taken(); len(handed) != 1 || handed[0].Step != "normalize" {
		t.Errorf("a replay from the start handed out %+v", handed)
	}
}

// A replay from a step whose run no longer holds what the steps above it produced is refused with
// the reason when it would be let in, rather than failing on every pass.
func TestAReplayWithNothingLeftToReuseIsRefused(t *testing.T) {
	core, q, pool, super := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	core.answer(t, succeeded(t, q.taken()[0], core.now()))
	for _, task := range q.taken() {
		core.answer(t, succeeded(t, task, core.now()))
	}
	// The run it replays gone with its workflow's purge, say.
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `update runs set evaluation = null where id = $1`, string(decidedRun)); err != nil {
		t.Fatal(err)
	}

	const replay agk.RunID = "01JMZ8V1P9C7"
	replayOf(t, pool, replay, "archive")
	if err := core.Decide(t.Context(), replay); err != nil {
		t.Fatal(err)
	}
	if handed := q.taken(); len(handed) != 0 {
		t.Errorf("a replay with nothing to reuse handed out %+v", handed)
	}
	var state, reason string
	if err := conn.QueryRow(t.Context(), `select state, coalesce(reason, '') from runs where id = $1`, string(replay)).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "cancelled" || reason == "" {
		t.Errorf("the replay ended %s: %q", state, reason)
	}
}
