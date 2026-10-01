package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

// agk console, where there is no screen to draw on, prints the view it would have opened once as
// plain lines: the tests here are that half, and the screen is package console's.

// consoleStandIn answers the routes agk console reads at start, and keeps what it was asked.
type consoleStandIn struct {
	me   int
	runs []db.ListedRun
	run  db.RunDetail

	mu    sync.Mutex
	asked []string
}

func (s *consoleStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.asked = append(s.asked, r.URL.RequestURI())
	s.mu.Unlock()
	switch {
	case r.URL.Path == "/api/v1/me":
		if s.me != 0 {
			w.WriteHeader(s.me)
			w.Write([]byte(`{"error":"no"}`))
			return
		}
		w.Write([]byte(`{"principal":"alice"}`))
	case r.URL.Path == "/api/v1/runs":
		json.NewEncoder(w).Encode(map[string]any{"runs": s.runs})
	case r.URL.Path == "/api/v1/runs/"+aRun:
		json.NewEncoder(w).Encode(s.run)
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"no such thing, or not yours"}`))
	}
}

func (s *consoleStandIn) wasAsked(uri string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.asked {
		if a == uri {
			return true
		}
	}
	return false
}

func listed(run, namespace, workflow string, state agk.RunState, started time.Time, took time.Duration, by string) db.ListedRun {
	r := db.ListedRun{RunSummary: db.RunSummary{
		Namespace: namespace, Run: agk.RunID(run), Workflow: workflow, State: state,
		Trigger: agk.TriggerManual, TriggeredBy: by, CreatedAt: started, StartedAt: started,
	}}
	if took > 0 {
		r.FinishedAt = started.Add(took)
	}
	return r
}

// The runs view, with no screen, is one line a run, newest first, its columns separated by tabs,
// no header, its times in RFC 3339, and the command leaves with 0.
func TestAConsoleWithNoScreenPrintsTheRunsAsPlainLines(t *testing.T) {
	s := &consoleStandIn{runs: []db.ListedRun{
		listed(aRun, "finance", "monthly-invoicing", agk.Failed, runStart, 112*time.Second, "alice"),
		listed("01M3RUNBBBBBBBBBBBBBBBBBBB", "ops", "nightly", agk.Succeeded, runStart.Add(-time.Hour), 22*time.Second, ""),
	}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	code, out, errs := against(t.Context(), t.TempDir(), srv.URL, "console")
	if code != exitSucceeded {
		t.Fatalf("agk console answered %d: %s", code, errs)
	}
	want := "failed\t" + aRun + "\tfinance/monthly-invoicing\tmanual\t2026-09-25T10:00:00Z\t1m 52s\talice\n" +
		"succeeded\t01M3RUNBBBBBBBBBBBBBBBBBBB\tops/nightly\tmanual\t2026-09-25T09:00:00Z\t22s\t\n"
	if out != want {
		t.Errorf("agk console printed\n%q\nwhere the runs view as plain lines is\n%q", out, want)
	}
	if !s.wasAsked("/api/v1/runs?limit=100") {
		t.Errorf("agk console asked %v, and not for the runs", s.asked)
	}
}

// --namespace opens on the runs of one namespace, which the listing is asked for.
func TestAConsoleOnANamespaceReadsItsRunsAlone(t *testing.T) {
	s := &consoleStandIn{}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	code, out, errs := against(t.Context(), t.TempDir(), srv.URL, "console", "--namespace", "finance")
	if code != exitSucceeded || out != "" {
		t.Fatalf("agk console on a namespace with no run answered %d, printing %q: %s", code, out, errs)
	}
	if !s.wasAsked("/api/v1/runs?limit=100&namespace=finance") {
		t.Errorf("agk console --namespace finance asked %v", s.asked)
	}
}

// A run named, with no screen, prints as agk status prints it.
func TestAConsoleOnARunWithNoScreenPrintsItAsStatusDoes(t *testing.T) {
	s := &consoleStandIn{run: runReading(agk.Succeeded, agk.VerdictSucceeded, aTask(agk.TaskSucceeded, new(0)))}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	code, out, errs := against(t.Context(), t.TempDir(), srv.URL, "console", aRun)
	if code != exitSucceeded {
		t.Fatalf("agk console on a run answered %d: %s", code, errs)
	}
	_, status, _ := against(t.Context(), t.TempDir(), srv.URL, "status", aRun)
	if out != status {
		t.Errorf("agk console on a run printed\n%s\nwhere agk status prints\n%s", out, status)
	}
}

// A credential the installation refuses is said so before anything is drawn, and an installation
// that cannot be reached leaves with no outcome, as every verb does.
func TestAConsoleThatCannotStartSaysWhy(t *testing.T) {
	s := &consoleStandIn{me: http.StatusUnauthorized}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	code, _, errs := against(t.Context(), t.TempDir(), srv.URL, "console")
	if code != exitRefused || !strings.Contains(errs, "did not accept the credential in "+tokenVariable) {
		t.Errorf("a refused credential answered %d: %s", code, errs)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	code, _, errs = against(t.Context(), t.TempDir(), gone.URL, "console")
	if code != exitNoOutcome {
		t.Errorf("an installation that cannot be reached answered %d: %s", code, errs)
	}

	code, _, errs = against(t.Context(), t.TempDir(), srv.URL, "console", aRun, "01M3RUNBBBBBBBBBBBBBBBBBBB")
	if code != exitUsage || !strings.Contains(errs, "one run at most") {
		t.Errorf("two runs named answered %d: %s", code, errs)
	}
}

// TERM=dumb is no screen, even on a terminal, and a terminal said to be one is not opened by a
// test that does not say so.
func TestAConsoleOnADumbTerminalPrintsPlainLines(t *testing.T) {
	s := &consoleStandIn{runs: []db.ListedRun{listed(aRun, "finance", "monthly-invoicing", agk.Succeeded, runStart, time.Second, "alice")}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	out, errs := &strings.Builder{}, &strings.Builder{}
	e := Env{
		Out: out, Err: errs, Dir: t.TempDir(),
		Getenv: func(k string) string {
			switch k {
			case tokenVariable:
				return "the-token"
			case serverVariable:
				return srv.URL
			case "TERM":
				return "dumb"
			}
			return ""
		},
		Terminal: func() bool { return true },
	}
	if e.terminal() {
		t.Fatal("TERM=dumb is taken for a screen to draw on")
	}
	if code := run(t.Context(), e, []string{"console"}); code != exitSucceeded || !strings.HasPrefix(out.String(), "succeeded\t"+aRun+"\t") {
		t.Errorf("agk console on a dumb terminal answered %d, printing %q: %s", code, out, errs)
	}
	if (Env{}).terminal() {
		t.Error("an Env that says nothing of a terminal is taken for one")
	}
}

// AGENTIIK_THEME is light or dark, and anything else is refused before the installation is asked,
// as a wrong flag is.
func TestAThemeThatIsNeitherLightNorDarkIsRefused(t *testing.T) {
	s := &consoleStandIn{}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	errs := &strings.Builder{}
	e := Env{
		Out: &strings.Builder{}, Err: errs, Dir: t.TempDir(),
		Getenv: func(k string) string {
			return map[string]string{tokenVariable: "the-token", serverVariable: srv.URL, themeVariable: "solarized"}[k]
		},
	}
	if code := run(t.Context(), e, []string{"console"}); code != exitUsage || !strings.Contains(errs.String(), `AGENTIIK_THEME is light or dark, and "solarized" is neither`) {
		t.Errorf("AGENTIIK_THEME=solarized answered %d: %s", code, errs)
	}
	if len(s.asked) != 0 {
		t.Errorf("a theme refused asked the installation %v", s.asked)
	}
}
