package main

import (
	"context"
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

// agk run on an installation, against a stand-in for one that answers what the API answers: the
// run it is asked for, then the run as it goes, one reading at a time.

const inputsWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  cycle:
    schema: { type: string }
    default: "2026-01"
  orders:
    schema: { type: array }
    required: true
outputs:
  invoices: { from: { step: normalize, port: ok } }
steps:
  normalize:
    image: docker.io/library/alpine@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    script: ["echo hello > /agk/out/ports/ok"]
    outputs: [ok]
`

const aRun = "01M3RUNAAAAAAAAAAAAAAAAAAA"

// standIn is an installation that starts one run and then answers the readings it was given, one
// per request, the last one for ever after.
type standIn struct {
	t        *testing.T
	mu       sync.Mutex
	started  map[string]any
	path     string
	readings []db.RunDetail
	read     int
	cancels  int
	outputs  map[string]string
	start    int
}

func (s *standIn) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer the-token" {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"no"}`))
		return
	}
	switch {
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/runs"):
		s.path = r.URL.Path
		json.NewDecoder(r.Body).Decode(&s.started)
		if s.start != 0 {
			w.WriteHeader(s.start)
			w.Write([]byte(`{"error":"the installation said no"}`))
			return
		}
		w.Header().Set("Location", "/api/v1/finance/runs/"+aRun)
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"run":"` + aRun + `","state":"queued"}`))
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/cancel"):
		s.cancels++
		w.WriteHeader(http.StatusAccepted)
	case r.Method == "GET" && strings.Contains(r.URL.Path, "/outputs/"):
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		w.Write([]byte(s.outputs[name]))
	case r.Method == "GET" && r.URL.Path == "/api/v1/runs/"+aRun:
		d := s.readings[min(s.read, len(s.readings)-1)]
		s.read++
		json.NewEncoder(w).Encode(d)
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"no such thing, or not yours"}`))
	}
}

// installationAt serves a stand-in, with following paced for a test.
func installationAt(t *testing.T, s *standIn) string {
	t.Helper()
	s.t = t
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	quickly(t)
	return srv.URL
}

// quickly paces following and asking again for a test, and puts the defaults back after it.
func quickly(t *testing.T) {
	t.Helper()
	every, most, giveUp := followEvery, followAtMost, followGiveUp
	first, atMost, after, silence := logRetryFirst, logRetryAtMost, logGiveUpAfter, logSilence
	followEvery, followAtMost, followGiveUp = time.Millisecond, 5*time.Millisecond, time.Second
	logRetryFirst, logRetryAtMost, logGiveUpAfter, logSilence = time.Millisecond, 5*time.Millisecond, 3, 5*time.Second
	t.Cleanup(func() {
		followEvery, followAtMost, followGiveUp = every, most, giveUp
		logRetryFirst, logRetryAtMost, logGiveUpAfter, logSilence = first, atMost, after, silence
	})
}

// against runs the command line in dir against the installation at url, and answers what it said.
func against(ctx context.Context, dir, url string, args ...string) (int, string, string) {
	out, errs := &strings.Builder{}, &strings.Builder{}
	e := Env{
		Out: out, Err: &serial{w: errs}, Dir: dir,
		Getenv: func(k string) string {
			switch k {
			case tokenVariable:
				return "the-token"
			case serverVariable:
				return url
			}
			return ""
		},
	}
	code := run(ctx, e, args)
	return code, out.String(), errs.String()
}

var runStart = time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)

// runReading is one answer of GET /api/v1/runs/{id}.
func runReading(state agk.RunState, verdict agk.Verdict, tasks ...db.TaskSummary) db.RunDetail {
	d := db.RunDetail{RunSummary: db.RunSummary{
		Namespace: "finance", Run: aRun, Workflow: "monthly-invoicing", Commit: "a3f9c1e0000000000000000000000000000000000", State: state,
		CreatedAt: runStart,
	}}
	if state != agk.Queued {
		d.StartedAt = runStart
	}
	step := db.StepSummary{Step: "normalize", Verdict: verdict, Attempts: 1}
	if verdict != agk.VerdictPending {
		step.StartedAt = runStart.Add(200 * time.Millisecond)
	}
	if verdict.Terminal() {
		step.FinishedAt = runStart.Add(1500 * time.Millisecond)
		d.FinishedAt = step.FinishedAt
	}
	if verdict == agk.VerdictSucceeded {
		step.Ports = map[agk.Port]db.Envelope{"ok": {Digest: "sha256:" + strings.Repeat("ab", 32), Items: 1}}
		d.Outputs = map[string]any{"invoices": map[string]any{"step": "normalize", "port": "ok", "count": 1}}
	}
	d.Steps = []db.StepSummary{step}
	d.Tasks = tasks
	return d
}

func aTask(state agk.TaskState, code *int) db.TaskSummary {
	t := db.TaskSummary{Task: agk.NewTaskID(aRun, "normalize", 1, agk.Shard{}), Step: "normalize", State: state, Attempt: 1, Runner: "runner-dmz-02", ExitCode: code}
	if state != agk.TaskPending {
		t.StartedAt = runStart.Add(200 * time.Millisecond)
	}
	if state.Terminal() {
		t.FinishedAt = runStart.Add(1400 * time.Millisecond)
	}
	return t
}

// A run on an installation is of a commit and of the inputs as schema binds them, and it is
// followed to its end in the words of a local run.
func TestARunOnAnInstallationIsStartedAndFollowedToItsEnd(t *testing.T) {
	dir := repository(t)
	write(t, dir, "agentiik.yaml", inputsWorkflow)
	commitAll(t, dir, "inputs")
	sha := gitIn(t, dir, "rev-parse", "HEAD")

	s := &standIn{readings: []db.RunDetail{
		runReading(agk.Queued, agk.VerdictPending),
		runReading(agk.Running, agk.VerdictRunning, aTask(agk.TaskRunning, nil)),
		runReading(agk.Succeeded, agk.VerdictSucceeded, aTask(agk.TaskSucceeded, new(0))),
	}}
	url := installationAt(t, s)

	code, out, errs := against(t.Context(), dir, url, "run", "--namespace", "finance", "--input", `orders=[{"ref":"A-1"}]`)
	if code != exitSucceeded {
		t.Fatalf("agk run answered %d: %s%s", code, out, errs)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path != "/api/v1/finance/workflows/monthly-invoicing/runs" {
		t.Errorf("the run was asked for at %s", s.path)
	}
	if s.started["commit"] != sha {
		t.Errorf("the run was asked for commit %v, and HEAD is %s", s.started["commit"], sha)
	}
	inputs, _ := s.started["inputs"].(map[string]any)
	if inputs["cycle"] != "2026-01" {
		t.Errorf("the declared default was not applied before the run was asked for: %v", s.started["inputs"])
	}
	if orders, _ := inputs["orders"].([]any); len(orders) != 1 {
		t.Errorf("the inputs asked for are %v", s.started["inputs"])
	}
	for _, want := range []string{
		"run " + aRun + " of finance/monthly-invoicing@" + short(sha) + " started at " + url,
		"0.2s  normalize  running",
		"1.5s  normalize  succeeded, ok 1",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("the narration does not say %q:\n%s", want, errs)
		}
	}
	for _, want := range []string{
		"monthly-invoicing succeeded in 1.5s: 1 step, 1 container",
		"invoices: 1 item",
		"run " + aRun + ": " + url + "/api/v1/finance/runs/" + aRun,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
}

// An input the declaration refuses is refused here, before any run exists, as a local run refuses
// it: the API writes a run's inputs as they arrive.
func TestAnInputTheDeclarationRefusesStartsNoRun(t *testing.T) {
	dir := repository(t)
	write(t, dir, "agentiik.yaml", inputsWorkflow)
	commitAll(t, dir, "inputs")
	s := &standIn{readings: []db.RunDetail{runReading(agk.Queued, agk.VerdictPending)}}
	url := installationAt(t, s)

	code, out, errs := against(t.Context(), dir, url, "run", "--namespace", "finance")
	if code != exitRefused {
		t.Fatalf("a run missing a required input answered %d: %s%s", code, out, errs)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started != nil {
		t.Errorf("a run was asked for with %v", s.started)
	}
	if !strings.Contains(errs, "orders") {
		t.Errorf("the refusal does not name the input: %s", errs)
	}
}

// A run that did not succeed exits 3 with the failure report of a local run, the step, the exit
// code and its band, in that order.
func TestARunThatFailedOnAnInstallationExitsThree(t *testing.T) {
	dir := repository(t)
	s := &standIn{readings: []db.RunDetail{
		runReading(agk.Failed, agk.VerdictFailed, aTask(agk.TaskFailed, new(3))),
	}}
	url := installationAt(t, s)

	code, out, errs := against(t.Context(), dir, url, "run", "--namespace", "finance")
	if code != exitNotSucceeded {
		t.Fatalf("a failed run answered %d: %s%s", code, out, errs)
	}
	for _, want := range []string{
		"monthly-invoicing failed: 1 step failed",
		"step normalize: exit code 3, " + agk.Band(3).String() + ": failed",
		"agk logs " + aRun + " shows what they wrote",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("the report does not say %q:\n%s", want, errs)
		}
	}
}

// Cancelled with nothing failed names what was stopped, as a local run does.
func TestACancelledRunNamesWhatWasStopped(t *testing.T) {
	dir := repository(t)
	s := &standIn{readings: []db.RunDetail{
		runReading(agk.Cancelled, agk.VerdictCancelled, aTask(agk.TaskCancelled, new(143))),
	}}
	url := installationAt(t, s)

	code, _, errs := against(t.Context(), dir, url, "run", "--namespace", "finance")
	if code != exitNotSucceeded {
		t.Fatalf("a cancelled run answered %d: %s", code, errs)
	}
	if !strings.Contains(errs, "monthly-invoicing cancelled: no step failed, and normalize was stopped") {
		t.Errorf("the report of a cancelled run is:\n%s", errs)
	}
}

// An interrupt stops the following, not the run: nothing is cancelled, the process says how to
// read the run again, and leaves with no outcome.
func TestAnInterruptStopsFollowingAndNotTheRun(t *testing.T) {
	dir := repository(t)
	s := &standIn{readings: []db.RunDetail{runReading(agk.Running, agk.VerdictRunning, aTask(agk.TaskRunning, nil))}}
	url := installationAt(t, s)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		for {
			s.mu.Lock()
			read := s.read
			s.mu.Unlock()
			if read >= 2 {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	code, _, errs := against(ctx, dir, url, "run", "--namespace", "finance")
	if code != exitNoOutcome {
		t.Fatalf("an interrupted following answered %d: %s", code, errs)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancels != 0 {
		t.Error("an interrupt cancelled the run on the installation")
	}
	if !strings.Contains(errs, "stopped following run "+aRun+", which goes on at "+url) {
		t.Errorf("the interrupt does not say the run goes on: %s", errs)
	}
}

// -o json writes the output envelopes on standard output as a local run writes them, and the
// report moves to standard error.
func TestOutputsAreWrittenAsALocalRunWritesThem(t *testing.T) {
	dir := repository(t)
	envelope := `{"meta":{"run_id":"` + aRun + `","step":"normalize","port":"ok","attempt":1,"count":1,"produced_at":"2026-09-25T10:00:01Z"},"items":[{"id":"a","data":{"n":1},"files":[]}]}`
	s := &standIn{
		readings: []db.RunDetail{runReading(agk.Succeeded, agk.VerdictSucceeded, aTask(agk.TaskSucceeded, new(0)))},
		outputs:  map[string]string{"invoices": envelope},
	}
	url := installationAt(t, s)

	code, out, errs := against(t.Context(), dir, url, "run", "--namespace", "finance", "-o", "json")
	if code != exitSucceeded {
		t.Fatalf("agk run -o json answered %d: %s", code, errs)
	}
	var written map[string]agk.Envelope
	if err := json.Unmarshal([]byte(out), &written); err != nil {
		t.Fatalf("standard output is not the envelopes: %v\n%s", err, out)
	}
	if e, ok := written["invoices"]; !ok || len(e.Items) != 1 || e.Items[0].ID != "a" {
		t.Errorf("the envelopes written are %s", out)
	}
	if !strings.Contains(errs, "monthly-invoicing succeeded") {
		t.Errorf("the report did not move to standard error: %s", errs)
	}
}

// A commit the installation holds no version of is refused with what to do about it.
func TestACommitNeverPushedIsRefusedSayingSo(t *testing.T) {
	dir := repository(t)
	s := &standIn{start: http.StatusNotFound}
	url := installationAt(t, s)

	code, _, errs := against(t.Context(), dir, url, "run", "--namespace", "finance")
	if code != exitRefused {
		t.Fatalf("a commit never pushed answered %d: %s", code, errs)
	}
	if !strings.Contains(errs, "push it first") {
		t.Errorf("the refusal does not say to push: %s", errs)
	}
}

// Each flag names one kind of run, and a flag of the other kind is refused rather than dropped.
func TestAFlagOfTheOtherKindOfRunIsRefused(t *testing.T) {
	dir := repository(t)
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"run", "--namespace", "finance", "--secret", "billing_api=x"}, "PUT /api/v1/{ns}/secrets/{name}"},
		{[]string{"run", "--namespace", "finance", "--logs"}, "agk logs"},
		{[]string{"run", "--namespace", "finance", "--dir", "elsewhere"}, "runner"},
		{[]string{"run", "--local", "--namespace", "finance"}, "--namespace"},
		{[]string{"run", "--local", "--commit", "HEAD"}, "--commit"},
	} {
		code, _, errs := against(t.Context(), dir, "https://agentiik.example.com", c.args...)
		if code != exitUsage {
			t.Errorf("%v answered %d: %s", c.args, code, errs)
		}
		if !strings.Contains(errs, c.says) {
			t.Errorf("%v says %q", c.args, errs)
		}
	}
}

// The credential never crosses a network in plaintext, whichever verb would send it.
func TestAnAddressThatWouldCarryTheCredentialInPlaintextIsRefused(t *testing.T) {
	dir := repository(t)
	for _, args := range [][]string{
		{"run", "--namespace", "finance"},
		{"push", "--namespace", "finance"},
		{"status", aRun},
		{"logs", aRun},
	} {
		code, _, errs := against(t.Context(), dir, "http://agentiik.example.com", args...)
		if code != exitUsage || !strings.Contains(errs, "plain http") {
			t.Errorf("%v against plain http answered %d: %s", args, code, errs)
		}
	}
	for _, where := range []string{"https://me:secret@agentiik.example.com", "agentiik.example.com", "https://agentiik.example.com/?x=1"} {
		code, _, errs := against(t.Context(), dir, where, "status", aRun)
		if code != exitUsage {
			t.Errorf("%s answered %d: %s", where, code, errs)
		}
		if strings.Contains(errs, "secret") {
			t.Errorf("the refusal repeats a password: %s", errs)
		}
	}
}
