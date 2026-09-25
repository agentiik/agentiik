package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

// agk status reads one run as GET /api/v1/runs/{id} answers it.

func askStatus(t *testing.T, answer string, args ...string) (int, string, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runs/"+aRun {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"no such thing, or not yours"}`))
			return
		}
		w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	return against(t.Context(), t.TempDir(), srv.URL, append([]string{"status"}, args...)...)
}

// A run reads as its state and how long it took, each step's verdict with the digest of what it
// published, what it was given and produced, and the failures it ended with.
func TestAStatusSaysHowARunStands(t *testing.T) {
	d := runReading(agk.Failed, agk.VerdictSucceeded, aTask(agk.TaskSucceeded, new(0)))
	d.Steps = append(d.Steps, db.StepSummary{Step: "charge", Verdict: agk.VerdictFailed, Attempts: 2})
	d.Tasks = append(d.Tasks,
		db.TaskSummary{Step: "charge", State: agk.TaskFailed, Attempt: 1, Shard: &agk.Shard{Index: 2, Of: 3}, ExitCode: new(3)},
		db.TaskSummary{Step: "charge", State: agk.TaskFailed, Attempt: 2, Shard: &agk.Shard{Index: 2, Of: 3}, ExitCode: new(4), Runner: "runner-dmz-02"},
	)
	d.Inputs = map[string]any{"orders": []any{}, "cycle": "2026-01"}
	d.TriggeredBy = "operator"
	d.Trigger = agk.TriggerManual
	answer, _ := json.Marshal(d)

	code, out, errs := askStatus(t, string(answer), aRun)
	if code != exitSucceeded {
		t.Fatalf("agk status answered %d: %s", code, errs)
	}
	for _, want := range []string{
		"run " + aRun + ": finance/monthly-invoicing@a3f9c1e, failed after 1.5s",
		"manual by operator at 2026-09-25T10:00:00Z",
		"normalize  succeeded, ok 1 sha256:abababababab",
		"charge     failed, attempt 2",
		"inputs: cycle, orders",
		"step charge 2/3, attempt 2: exit code 4, " + agk.Band(4).String() + ": failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("agk status does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "exit code 3") {
		t.Errorf("agk status reports an attempt a retry moved past as a failure:\n%s", out)
	}
}

// -o json is the installation's answer as it gave it, fields this binary does not know included.
func TestAStatusInJSONIsTheAnswerAsGiven(t *testing.T) {
	code, out, errs := askStatus(t, `{"run":"`+aRun+`","state":"running","something_new":1}`, aRun, "-o", "json")
	if code != exitSucceeded {
		t.Fatalf("agk status -o json answered %d: %s", code, errs)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["something_new"] != 1.0 {
		t.Errorf("agk status -o json wrote %s", out)
	}
}

// A run that is not there, or not the caller's, is refused as one.
func TestAStatusOfARunNotThereIsRefused(t *testing.T) {
	code, _, errs := askStatus(t, "", "01M3RUNBBBBBBBBBBBBBBBBBBB")
	if code != exitRefused || !strings.Contains(errs, "no run 01M3RUNBBBBBBBBBBBBBBBBBBB, or not yours") {
		t.Errorf("a run not there answered %d: %s", code, errs)
	}
}
