package graph

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// TestTheStateSurvivesBeingWrittenDown holds what the state is for. The controller says
// failover is a state resume and never a rebuild, so a state that did not come back out
// of a database exactly as it went in would make that sentence false: the instance that
// picks the run up would get a different Plan from the one the instance that died would
// have got.
func TestTheStateSurvivesBeingWrittenDown(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	in := &State{
		Version: StateVersion,
		Run: agk.Run{
			ID:          "01HZX",
			Workflow:    "finance/monthly-invoicing@1.4.0",
			Namespace:   "finance",
			Commit:      "a3f9c1e",
			Trigger:     agk.TriggerSchedule,
			TriggeredBy: "schedule",
			State:       agk.Running,
			StartedAt:   at,
		},
		Inputs: map[string]any{"orders": "agk://run/01HZX/trigger/in/orders.json"},
		Vars:   map[string]any{"currency": "EUR"},
		Steps: map[agk.Step]StepState{
			"normalize": {
				Verdict: agk.VerdictSucceeded,
				Since:   at,
				Ports:   map[agk.Port]agk.Envelope{"ok": outcomeBatch("01HZX", "normalize", "ok", 1, "a")},
				Shards: []ShardState{{
					Shard:        agk.Shard{Index: 1, Of: 2},
					Matrix:       map[string]any{"region": "eu"},
					Attempt:      2,
					Task:         agk.TaskSucceeded,
					Ports:        map[agk.Port]agk.Envelope{"ok": outcomeBatch("01HZX", "normalize", "ok", 2, "a")},
					DispatchedAt: at,
					StartedAt:    at.Add(time.Second),
					FinishedAt:   at.Add(2 * time.Second),
				}},
			},
			"invoice": {
				Verdict: agk.VerdictPending,
				Shards: []ShardState{{
					Shard:         agk.Shard{Index: 1, Of: 1},
					Attempt:       1,
					Task:          agk.TaskFailed,
					ExitCode:      100,
					FinishedAt:    at,
					NextAttemptAt: at.Add(2 * time.Second),
				}},
			},
			"archive": {Verdict: agk.VerdictPending},
		},
		Seq: 7,
	}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out State
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, &out) {
		t.Errorf("the state came back changed:\nwent in %+v\ncame out %+v", in, &out)
	}

	// What a person reading a row in a database sees. A state that travelled as
	// numbers would need this package to be read at all.
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	run, _ := doc["run"].(map[string]any)
	if run["state"] != "running" || run["trigger"] != "schedule" {
		t.Errorf("the run travels as %v", run)
	}
	steps, _ := doc["steps"].(map[string]any)
	normalize, _ := steps["normalize"].(map[string]any)
	if normalize["verdict"] != "succeeded" {
		t.Errorf("a step verdict travels as %v", normalize["verdict"])
	}
}

// TestAStepNobodyHasReachedIsAnEntryAndNotAnAbsence holds the reading State.Steps
// records: every step of the graph has an entry from the moment the run starts, so that
// a step nobody has reached is pending rather than missing, and the reduction to a run
// verdict can tell a run that has not finished from one that has.
func TestAStepNobodyHasReachedIsAnEntryAndNotAnAbsence(t *testing.T) {
	s := &State{
		Version: StateVersion,
		Run:     agk.Run{ID: "01HZX", State: agk.Running},
		Steps:   map[agk.Step]StepState{"normalize": {Verdict: agk.VerdictSucceeded}, "archive": {}},
	}
	if got := s.Steps["archive"].Verdict; got != agk.VerdictPending {
		t.Errorf("a step nobody has reached is %s", got)
	}
	if got := runVerdict(s, nil); got != agk.Running {
		t.Errorf("a run with a step still to come is %s", got)
	}
}
