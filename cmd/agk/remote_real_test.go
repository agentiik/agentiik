package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
)

// agk run, agk status and agk logs against the real API over PostgreSQL, with the real
// controller deciding the run and a runner's part played through the routes a runner reaches: the
// one place the command line and the installation meet whole, so that neither half can agree with
// a stand-in and be wrong about the other.

// written is a buffer two goroutines may use, one writing and one reading what was written.
type written struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *written) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *written) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// as alice runs one command line against an installation's address.
func as(ctx context.Context, dir, url string, out, errs io.Writer, args ...string) int {
	return run(ctx, Env{
		Out: out, Err: errs, Dir: dir,
		Getenv: func(k string) string {
			switch k {
			case tokenVariable:
				return "alice"
			case serverVariable:
				return url
			}
			return ""
		},
	}, args)
}

func TestARunOnAnInstallationIsFollowedInspectedAndItsLogResumed(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	in := anInstallation(t)
	dir := repository(t)
	if code, said := in.pushFrom(t, dir); code != exitSucceeded {
		t.Fatalf("push answered %d: %s", code, said)
	}
	quickly(t)

	// agk run, which starts the run and follows it while the controller and a runner move it.
	out, errs := &written{}, &written{}
	done := make(chan int, 1)
	go func() { done <- as(t.Context(), dir, in.url, out, errs, "run", "--namespace", "finance") }()
	var run agk.RunID
	for deadline := time.Now().Add(10 * time.Second); run == ""; {
		if time.Now().After(deadline) {
			t.Fatalf("agk run never said which run it started: %s", errs)
		}
		if said, found := strings.CutPrefix(errs.String(), "run "); found {
			run = agk.RunID(strings.Fields(said)[0])
		}
		time.Sleep(5 * time.Millisecond)
	}

	c, err := controller.New(in.pool, "end-to-end")
	if err != nil {
		t.Fatal(err)
	}
	term, err := in.pool.BeginTerm(t.Context(), "end-to-end")
	if err != nil {
		t.Fatal(err)
	}
	q := &dispatched{}
	core, err := controller.NewCore(c, term, controller.Options{Queue: q, Versions: in.store, Objects: in.objects})
	if err != nil {
		t.Fatal(err)
	}
	if err := core.Decide(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if len(q.sent) != 1 {
		t.Fatalf("the controller dispatched %d tasks", len(q.sent))
	}
	d := q.sent[0]

	// A runner of the task's pool joins, redeems it, and ships its log in two chunks.
	var join db.JoinToken
	if err := in.pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		var err error
		now := time.Now().UTC()
		join, err = w.IssueJoinToken(ctx, d.Pool, nil, "alice", now, now.Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var joined struct {
		Credential string `json:"credential"`
		Runner     string `json:"runner"`
	}
	if code := in.ask(t, "POST", "/api/v1/runners", "", api.Join{
		Token:        join.Clear,
		PublicKey:    "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAXiy2zvWwTpj67NwwKIgCbjFcQdrNAboeffNXm+aJUcM=\n-----END PUBLIC KEY-----\n",
		Labels:       []string{},
		Capacity:     &api.Capacity{VCPU: 8, Memory: "16Gi", Disk: "256Gi"},
		Architecture: "amd64", AgentVersion: "0.2.0",
	}, &joined); code != http.StatusCreated {
		t.Fatalf("joining answered %d", code)
	}
	if code := in.ask(t, "POST", "/api/v1/tasks/redeem", joined.Credential, api.Redemption{Grant: d.Grant, TaskID: d.Row, IdempotencyKey: d.Task.ID}, nil); code != http.StatusOK {
		t.Fatalf("redeeming answered %d", code)
	}
	ship := func(seq, first int, final bool, lines ...string) {
		t.Helper()
		c := api.LogShipment{IdempotencyKey: d.Task.ID, Seq: int64(seq), FirstLine: int64(first), Final: final, Lines: []api.LogLine{}}
		for _, text := range lines {
			c.Lines = append(c.Lines, api.LogLine{At: time.Now().UTC(), Text: text})
		}
		body, _ := json.Marshal(c)
		r, _ := http.NewRequestWithContext(t.Context(), "POST", in.url+"/api/v1/tasks/logs", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+joined.Credential)
		r.Header.Set(api.GrantHeader, d.Grant)
		r.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("chunk %d answered %d", seq, res.StatusCode)
		}
	}
	ship(1, 1, false, "reading orders", "charging A-1")
	ship(2, 3, true, "the card was declined")

	// It fails with 3, and the controller ends the run on it.
	now := time.Now().UTC()
	if err := core.Answer(t.Context(), controller.Answer{
		Result: graph.Result{Task: d.Task.ID, State: agk.TaskFailed, ExitCode: 3, StartedAt: now.Add(-time.Second), FinishedAt: now},
		Row:    d.Row, Runner: joined.Runner,
	}); err != nil {
		t.Fatal(err)
	}
	if err := core.Decide(t.Context(), run); err != nil {
		t.Fatal(err)
	}

	select {
	case code := <-done:
		if code != exitNotSucceeded {
			t.Fatalf("agk run of a run that failed answered %d: %s%s", code, out, errs)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("agk run did not see the run end: %s", errs)
	}
	for _, want := range []string{
		"normalize  failed",
		"monthly-invoicing failed: 1 step failed",
		"step normalize: exit code 3, " + agk.Band(3).String() + ": failed",
	} {
		if !strings.Contains(errs.String(), want) {
			t.Errorf("agk run does not say %q:\n%s", want, errs)
		}
	}

	// agk status reads the run as the installation holds it.
	var statusOut, statusErrs strings.Builder
	if code := as(t.Context(), dir, in.url, &statusOut, &statusErrs, "status", string(run)); code != exitSucceeded {
		t.Fatalf("agk status answered %d: %s", code, statusErrs.String())
	}
	for _, want := range []string{"run " + string(run) + ": finance/monthly-invoicing@", ", failed after", "manual by alice", "normalize  failed"} {
		if !strings.Contains(statusOut.String(), want) {
			t.Errorf("agk status does not say %q:\n%s", want, statusOut.String())
		}
	}

	// agk logs, through a proxy that cuts the first stream after its first line, which is what
	// an API stopping or a network dropping leaves a reader with.
	upstream, _ := url.Parse(in.url)
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.FlushInterval = -1
	var cut atomic.Bool
	var resumedFrom atomic.Value
	cutting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/logs") {
			proxy.ServeHTTP(w, r)
			return
		}
		if cut.Load() {
			resumedFrom.Store(r.Header.Get("Last-Event-ID"))
			proxy.ServeHTTP(w, r)
			return
		}
		cut.Store(true)
		req, _ := http.NewRequestWithContext(r.Context(), "GET", in.url+r.URL.Path, nil)
		req.Header = r.Header.Clone()
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		defer res.Body.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(res.StatusCode)
		read := bufio.NewScanner(res.Body)
		lines := 0
		for read.Scan() {
			w.Write([]byte(read.Text() + "\n"))
			if strings.HasPrefix(read.Text(), "event: line") {
				lines++
			}
			if read.Text() == "" && lines == 1 {
				w.(http.Flusher).Flush()
				return
			}
		}
	}))
	t.Cleanup(cutting.Close)

	var logsOut, logsErrs strings.Builder
	if code := as(t.Context(), dir, cutting.URL, &logsOut, &logsErrs, "logs", string(run)); code != exitSucceeded {
		t.Fatalf("agk logs answered %d: %s%s", code, logsOut.String(), logsErrs.String())
	}
	if want := "normalize | reading orders\nnormalize | charging A-1\nnormalize | the card was declined\n"; logsOut.String() != want {
		t.Errorf("agk logs wrote\n%s\nand the runner shipped\n%s", logsOut.String(), want)
	}
	if from, _ := resumedFrom.Load().(string); from != d.Row+"/1/1" {
		t.Errorf("the stream was resumed from %q, after the first line of %s", from, d.Row)
	}
}
