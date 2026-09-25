package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

// agk logs against a stand-in for the API's step log stream, which answers each connection with
// what a test scripts for it, and records the Last-Event-ID each one carried.

// streamStandIn serves one run of the steps it is given, and each step's log from the script of
// connections a test wrote for it.
type streamStandIn struct {
	mu       sync.Mutex
	steps    []agk.Step
	scripts  map[string][]func(w http.ResponseWriter, r *http.Request)
	resumed  map[string][]string
	detailed int
}

func (s *streamStandIn) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer the-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/api/v1/runs/"+aRun {
		s.mu.Lock()
		s.detailed++
		s.mu.Unlock()
		d := runReading(agk.Running, agk.VerdictRunning)
		d.Steps = nil
		for _, step := range s.steps {
			d.Steps = append(d.Steps, db.StepSummary{Step: step, Verdict: agk.VerdictRunning})
		}
		json.NewEncoder(w).Encode(d)
		return
	}
	step, ok := strings.CutPrefix(r.URL.Path, "/api/v1/runs/"+aRun+"/steps/")
	step, ok2 := strings.CutSuffix(step, "/logs")
	if !ok || !ok2 {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"no such thing, or not yours"}`))
		return
	}
	s.mu.Lock()
	n := len(s.resumed[step])
	s.resumed[step] = append(s.resumed[step], r.Header.Get("Last-Event-ID"))
	script, known := s.scripts[step]
	s.mu.Unlock()
	if !known {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"the run has no step of that name"}`))
		return
	}
	if n >= len(script) {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	script[n](w, r)
}

// streaming opens an event stream and writes the events given, then leaves, which cuts the
// connection before the step's log is said to be over unless the last event says it.
func streaming(events ...string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, e := range events {
			fmt.Fprint(w, e)
		}
		w.(http.Flusher).Flush()
	}
}

func sse(id, event string, data any) string {
	body, _ := json.Marshal(data)
	s := ""
	if id != "" {
		s = "id: " + id + "\n"
	}
	return s + "event: " + event + "\ndata: " + string(body) + "\n\n"
}

const (
	firstDispatch  = "01M3B00000000000000000000A"
	secondDispatch = "01M3B00000000000000000000B"
)

func dispatchEvent(row string, attempt int) string {
	return sse(row+"/0/0", "dispatch", map[string]any{"task_id": row, "idempotency_key": "k", "attempt": attempt, "requeue": 0})
}

func lineEvent(row string, seq, line int, text string) string {
	return sse(fmt.Sprintf("%s/%d/%d", row, seq, line), "line", map[string]any{"task_id": row, "line": line, "at": "2026-09-25T10:00:00Z", "text": text})
}

func endOf(row string, lines int, truncated bool) string {
	return sse("", "dispatch_end", map[string]any{"task_id": row, "lines": lines, "truncated": truncated, "final": true})
}

const stepOver = "event: end\ndata: {\"verdict\":\"succeeded\"}\n\n"

func followLogs(t *testing.T, s *streamStandIn, args ...string) (int, string, string) {
	t.Helper()
	if s.resumed == nil {
		s.resumed = map[string][]string{}
	}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	quickly(t)
	return against(t.Context(), t.TempDir(), srv.URL, append([]string{"logs"}, args...)...)
}

// A stream cut off is asked again from the last event it gave, and nothing is printed twice: not a
// line, and not the end of a dispatch the API sends again on such a resume.
func TestALogCutOffResumesWhereItStoodAndSaysNothingTwice(t *testing.T) {
	s := &streamStandIn{scripts: map[string][]func(http.ResponseWriter, *http.Request){
		"normalize": {
			streaming(dispatchEvent(firstDispatch, 1), lineEvent(firstDispatch, 1, 1, "one"), lineEvent(firstDispatch, 1, 2, "two")),
			// From further back than the reader stood, which a line already printed is not
			// printed again for.
			streaming(lineEvent(firstDispatch, 1, 2, "two"), lineEvent(firstDispatch, 2, 3, "three"), endOf(firstDispatch, 3, true)),
			streaming(endOf(firstDispatch, 3, true), dispatchEvent(secondDispatch, 2), lineEvent(secondDispatch, 1, 1, "again"), endOf(secondDispatch, 1, false), stepOver),
		},
	}}
	code, out, errs := followLogs(t, s, aRun, "normalize")
	if code != exitSucceeded {
		t.Fatalf("agk logs answered %d: %s%s", code, out, errs)
	}
	if want := "normalize | one\nnormalize | two\nnormalize | three\nnormalize, attempt 2 | again\n"; out != want {
		t.Errorf("agk logs wrote\n%s\nand the log is\n%s", out, want)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if got := s.resumed["normalize"]; len(got) != 3 || got[0] != "" || got[1] != firstDispatch+"/1/2" || got[2] != firstDispatch+"/2/3" {
		t.Errorf("the connections carried Last-Event-ID %q", got)
	}
	if n := strings.Count(errs, "the runner cut this log at 3 lines"); n != 1 {
		t.Errorf("the end of the first dispatch was said %d times:\n%s", n, errs)
	}
	if n := strings.Count(errs, "asking again"); n != 2 {
		t.Errorf("two cut connections were said %d times:\n%s", n, errs)
	}
}

// A connection that goes silent past the API's keep-alives is taken for dead and asked again from
// where it stood, rather than read from for ever.
func TestASilentStreamIsAskedAgain(t *testing.T) {
	s := &streamStandIn{scripts: map[string][]func(http.ResponseWriter, *http.Request){
		"normalize": {
			func(w http.ResponseWriter, r *http.Request) {
				streaming(dispatchEvent(firstDispatch, 1), lineEvent(firstDispatch, 1, 1, "one"))(w, r)
				<-r.Context().Done()
			},
			streaming(lineEvent(firstDispatch, 1, 2, "two"), endOf(firstDispatch, 2, false), stepOver),
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.resumed = map[string][]string{}
	quickly(t)
	logSilence = 50 * time.Millisecond
	code, out, errs := against(t.Context(), t.TempDir(), srv.URL, "logs", aRun, "normalize")
	if code != exitSucceeded {
		t.Fatalf("agk logs answered %d: %s%s", code, out, errs)
	}
	if out != "normalize | one\nnormalize | two\n" {
		t.Errorf("agk logs wrote\n%s", out)
	}
	if !strings.Contains(errs, "nothing was heard") {
		t.Errorf("the silence is not said: %s", errs)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if got := s.resumed["normalize"]; len(got) != 2 || got[1] != firstDispatch+"/1/1" {
		t.Errorf("the connections carried Last-Event-ID %q", got)
	}
}

// A refusal is not asked again: a step or a run that is not there stays not there.
func TestARefusedLogIsNotAskedAgain(t *testing.T) {
	s := &streamStandIn{scripts: map[string][]func(http.ResponseWriter, *http.Request){}}
	code, _, errs := followLogs(t, s, aRun, "nowhere")
	if code != exitRefused {
		t.Fatalf("a step that is not there answered %d: %s", code, errs)
	}
	if !strings.Contains(errs, "has no such step") {
		t.Errorf("the refusal says %s", errs)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.resumed["nowhere"]); n != 1 {
		t.Errorf("a refused log was asked for %d times, where once is all a refusal takes", n)
	}
}

// An installation that keeps failing is given up on, with no outcome, after the attempts allowed.
func TestALogThatCannotBeReopenedIsGivenUpOn(t *testing.T) {
	s := &streamStandIn{scripts: map[string][]func(http.ResponseWriter, *http.Request){"normalize": nil}}
	code, _, errs := followLogs(t, s, aRun, "normalize")
	if code != exitNoOutcome {
		t.Fatalf("a log that never opened answered %d: %s", code, errs)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.resumed["normalize"]); n != logGiveUpAfter+1 {
		t.Errorf("the log was asked for %d times, and %d attempts in a row are allowed", n, logGiveUpAfter+1)
	}
	if !strings.Contains(errs, "agk logs "+aRun+" normalize asks again") {
		t.Errorf("giving up does not say how to ask again: %s", errs)
	}
}

// With no step named, every step of the run is followed, each line behind its step.
func TestEveryStepOfARunIsFollowedWhenNoneIsNamed(t *testing.T) {
	s := &streamStandIn{
		steps: []agk.Step{"split", "total"},
		scripts: map[string][]func(http.ResponseWriter, *http.Request){
			"split": {streaming(dispatchEvent(firstDispatch, 1), lineEvent(firstDispatch, 1, 1, "splitting"), endOf(firstDispatch, 1, false), stepOver)},
			"total": {streaming(dispatchEvent(secondDispatch, 1), lineEvent(secondDispatch, 1, 1, "adding"), endOf(secondDispatch, 1, false), stepOver)},
		},
	}
	code, out, errs := followLogs(t, s, aRun)
	if code != exitSucceeded {
		t.Fatalf("agk logs answered %d: %s%s", code, out, errs)
	}
	for _, want := range []string{"split | splitting\n", "total | adding\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("agk logs wrote\n%s\nwithout %q", out, want)
		}
	}
}
