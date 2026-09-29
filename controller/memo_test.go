package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// cachedWorkflow is theWorkflow with its first step cached.
var cachedWorkflow = strings.Replace(theWorkflow, "    outputs: [ok, rejected]\n", "    outputs: [ok, rejected]\n    cache: true\n", 1)

// runOf creates one more run of the workflow, over the same inputs as every other.
func runOf(t *testing.T, pool *db.Pool, id agk.RunID) {
	t.Helper()
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.CreateRun(ctx, db.NewRun{
			ID: id, Workflow: "monthly-invoicing", Commit: "a3f9c1e",
			Trigger: agk.TriggerManual, TriggeredBy: "alice",
			Inputs: json.RawMessage(`{"orders": [{"customer_id": "C-1042"}]}`),
			Steps:  []agk.Step{"normalize", "archive"},
		})
	}); err != nil {
		t.Fatal(err)
	}
}

// "With cache: true, a hit republishes the same envelopes without starting a container." The first
// run's cached step runs and its success is remembered; a second run over the same inputs finds it,
// republishes what it published without handing anything out, names where it came from, and goes
// on to the step below with the same items. Once the first run's envelopes are purged, "a cache
// entry pointing at an expired artifact is not a hit", and the step runs again.
func TestACachedStepIsRepublishedWithoutAContainer(t *testing.T) {
	core, q, pool, super := decidingOn(t, cachedWorkflow)
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `update workflow_versions set tree = '[{"path":"agentiik.yaml","sha256":"`+strings.Repeat("a", 64)+`","size":1,"mode":"0644"}]'`); err != nil {
		t.Fatal(err)
	}

	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.taken()
	if len(first) != 1 || first[0].Step != "normalize" || first[0].CacheKey == "" {
		t.Fatalf("the first run handed out %+v", first)
	}
	made := succeeded(t, first[0], core.now())
	core.answer(t, made)
	var entries int
	if err := conn.QueryRow(t.Context(), `select count(*) from step_cache where run_id = $1 and step = 'normalize'`, string(decidedRun)).Scan(&entries); err != nil || entries != 1 {
		t.Fatalf("the cached success left %d entries: %v", entries, err)
	}

	const second agk.RunID = "01JMZ8V1P9C5"
	runOf(t, pool, second)
	if err := core.Decide(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	handed := q.taken()
	for _, task := range handed {
		if task.Run == second && task.Step == "normalize" {
			t.Fatalf("a hit handed normalize out: %+v", task)
		}
	}
	var archive *agk.Envelope
	for _, task := range handed {
		if task.Run == second && task.Step == "archive" {
			e := task.Inputs["orders"]
			archive = &e
		}
	}
	if archive == nil {
		t.Fatalf("the step below the hit was not handed out: %+v", handed)
	}
	if len(archive.Items) != 1 || archive.Items[0].ID != made.Outputs["ok"].Items[0].ID || archive.Meta.RunID != second {
		t.Errorf("the step below the hit reads %+v", archive)
	}
	var memoised *string
	var runner *string
	var code *int
	if err := conn.QueryRow(t.Context(),
		`select memoised_from, runner, exit_code from tasks where run_id = $1 and step = 'normalize'`, string(second)).
		Scan(&memoised, &runner, &code); err != nil {
		t.Fatal(err)
	}
	if memoised == nil || *memoised != string(decidedRun) || runner != nil || code != nil {
		t.Errorf("the hit's task reads memoised from %v, runner %v, exit code %v", memoised, runner, code)
	}
	var detail db.RunDetail
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		var err error
		detail, err = ns.RunDetail(ctx, second)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, task := range detail.Tasks {
		if task.Step == "normalize" && task.MemoisedFrom != decidedRun {
			t.Errorf("the run detail names normalize as memoised from %q", task.MemoisedFrom)
		}
	}

	// The first run's envelopes purged, what its entry names is gone, and a third run runs
	// normalize again.
	if _, err := conn.Exec(t.Context(), `update runs set envelopes_purged_at = now() where id = $1`, string(decidedRun)); err != nil {
		t.Fatal(err)
	}
	const third agk.RunID = "01JMZ8V1P9C6"
	runOf(t, pool, third)
	if err := core.Decide(t.Context(), third); err != nil {
		t.Fatal(err)
	}
	ran := false
	for _, task := range q.taken() {
		ran = ran || task.Run == third && task.Step == "normalize"
	}
	if !ran {
		t.Error("a step whose only entry named a purged run's envelopes was not handed out")
	}
}

// An entry naming an envelope the store no longer holds is no hit, and is removed.
func TestAnEntryNamingWhatIsGoneIsForgotten(t *testing.T) {
	core, q, pool, super := decidingOn(t, cachedWorkflow)
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `update workflow_versions set tree = '[]'`); err != nil {
		t.Fatal(err)
	}
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	core.answer(t, succeeded(t, q.taken()[0], core.now()))
	if _, err := conn.Exec(t.Context(), `update step_cache set ports = jsonb_set(ports, '{0,digest}', to_jsonb('sha256:`+strings.Repeat("0", 64)+`'::text))`); err != nil {
		t.Fatal(err)
	}

	const second agk.RunID = "01JMZ8V1P9C5"
	runOf(t, pool, second)
	if err := core.Decide(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	ran := false
	for _, task := range q.taken() {
		ran = ran || task.Run == second && task.Step == "normalize"
	}
	if !ran {
		t.Error("an entry naming an envelope that is gone was taken for a hit")
	}
	var left int
	if err := conn.QueryRow(t.Context(), `select count(*) from step_cache`).Scan(&left); err != nil || left != 0 {
		t.Errorf("the entry is still there: %d, %v", left, err)
	}
}
