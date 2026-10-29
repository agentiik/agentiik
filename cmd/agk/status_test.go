package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// A run refused at creation says why, "cancelled: " and the reason, the example the page gives: its
// steps say nothing, since none ran.
func TestAStatusOfARunRefusedAtCreationSaysWhy(t *testing.T) {
	d := runReading(agk.Cancelled, agk.VerdictPending)
	d.StartedAt, d.FinishedAt = time.Time{}, runStart.Add(40*time.Millisecond)
	d.Trigger, d.TriggeredBy = agk.TriggerSchedule, "finance/agentiik"
	d.Reason = "finance/agentiik no longer holds workflow:run on finance/monthly-invoicing: grant 01JQ3M8T (operator on the namespace finance) expired at 2026-10-01T00:00:00Z"
	answer, _ := json.Marshal(d)

	code, out, errs := askStatus(t, string(answer), aRun)
	if code != exitSucceeded {
		t.Fatalf("agk status answered %d: %s", code, errs)
	}
	for _, want := range []string{
		"run " + aRun + ": finance/monthly-invoicing@a3f9c1e, cancelled before it started",
		"schedule by finance/agentiik at 2026-09-25T10:00:00Z",
		"cancelled: " + d.Reason,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("agk status does not say %q:\n%s", want, out)
		}
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

// On a terminal, a run is written as agk console draws text, its reason and who started it written
// by whoever pushed the workflow; -o json writes the answer with every C1 control escaped, the same
// document to whatever reads it; and a refusal's sentence is screened too. To a pipe, -o json is
// the answer as it gave it, byte for byte.
func TestAStatusOnATerminalIsWrittenAsTheConsoleDrawsIt(t *testing.T) {
	d := runReading(agk.Cancelled, agk.VerdictPending)
	d.StartedAt, d.FinishedAt = time.Time{}, runStart.Add(40*time.Millisecond)
	d.Reason = "refused\r\x1b]0;pwned\a at creation\u009b2J"
	d.TriggeredBy = "op\x1b[31m"
	answer, _ := json.Marshal(d)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runs/"+aRun {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"not now\u001b]52;c;SGFja2Vk\u0007"}`))
			return
		}
		w.Write(answer)
	}))
	t.Cleanup(srv.Close)

	code, out, errs := onATerminal(t.Context(), t.TempDir(), srv.URL, "status", aRun)
	if code != exitSucceeded || !strings.Contains(out, `cancelled: refused\r at creation\u009b2J`+"\n") || !strings.Contains(out, "manual by op at") || strings.ContainsAny(out, "\r\x1b\a\u009b") {
		t.Errorf("agk status answered %d on a terminal, writing %q: %s", code, out, errs)
	}
	code, out, errs = onATerminal(t.Context(), t.TempDir(), srv.URL, "status", aRun, "-o", "json")
	var got db.RunDetail
	if code != exitSucceeded || json.Unmarshal([]byte(out), &got) != nil || got.Reason != d.Reason || strings.Contains(out, "\u009b") || !strings.Contains(out, `creation\u009b2J`) {
		t.Errorf("agk status -o json answered %d on a terminal, writing %q: %s", code, out, errs)
	}
	if _, out, _ := against(t.Context(), t.TempDir(), srv.URL, "status", aRun, "-o", "json"); !strings.Contains(out, "creation\u009b2J") {
		t.Errorf("agk status -o json to a pipe wrote %q", out)
	}
	code, _, errs = onATerminal(t.Context(), t.TempDir(), srv.URL, "status", "01M3RUNBBBBBBBBBBBBBBBBBBB")
	if code != exitRefused || !strings.Contains(errs, "not now") || strings.ContainsAny(errs, "\x1b\a") {
		t.Errorf("a refusal answered %d on a terminal: %q", code, errs)
	}
	// agk logs says a refusal before any stream opens the same way.
	code, _, errs = onATerminal(t.Context(), t.TempDir(), srv.URL, "logs", "01M3RUNBBBBBBBBBBBBBBBBBBB")
	if code != exitRefused || !strings.Contains(errs, "not now") || strings.ContainsAny(errs, "\x1b\a") {
		t.Errorf("agk logs refused answered %d on a terminal: %q", code, errs)
	}
}

// A run that is not there, or not the caller's, is refused as one.
func TestAStatusOfARunNotThereIsRefused(t *testing.T) {
	code, _, errs := askStatus(t, "", "01M3RUNBBBBBBBBBBBBBBBBBBB")
	if code != exitRefused || !strings.Contains(errs, "no run 01M3RUNBBBBBBBBBBBBBBBBBBB, or not yours") {
		t.Errorf("a run not there answered %d: %s", code, errs)
	}
}

// -v lists every task, a purged envelope says so beside its digest, a run whose inputs are gone
// says it replays from its start only, and a run's standing reads as how long it has been in it.
func TestAStatusSaysWhatIsGoneAndHowLongARunHasStood(t *testing.T) {
	d := runReading(agk.Succeeded, agk.VerdictSucceeded, aTask(agk.TaskSucceeded, new(0)))
	p := d.Steps[0].Ports["ok"]
	p.PurgedAt = runStart.Add(time.Hour)
	d.Steps[0].Ports["ok"] = p
	d.ReplayFromStartOnly = true
	answer, _ := json.Marshal(d)
	code, out, errs := askStatus(t, string(answer), aRun, "-v")
	if code != exitSucceeded {
		t.Fatalf("agk status -v answered %d: %s", code, errs)
	}
	for _, want := range []string{
		"ok 1 sha256:abababababab (purged)",
		"| attempt 1 succeeded, exit code 0, on runner-dmz-02",
		"replays from its start only",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("agk status -v does not say %q:\n%s", want, out)
		}
	}

	now := runStart.Add(90 * time.Second)
	for _, c := range []struct {
		d    db.RunDetail
		want string
	}{
		{runReading(agk.Queued, agk.VerdictPending), "queued for 90.0s"},
		{runReading(agk.Running, agk.VerdictRunning), "running for 90.0s"},
		{func() db.RunDetail {
			d := runReading(agk.Cancelled, agk.VerdictPending)
			d.StartedAt = time.Time{}
			return d
		}(), "cancelled before it started"},
	} {
		if got := standing(c.d, now); got != c.want {
			t.Errorf("a run %s stands %q, want %q", c.d.State, got, c.want)
		}
	}
}
