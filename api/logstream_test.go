package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/version"
)

// A step's log as a reader follows it: every dispatch in the order they were made, what each holds
// and then what its runner ships, resumed where a reader left it, and ended when the step is.

const streamRun agk.RunID = "01M3AAAAAAAAAAAAAAAAAAAAAA"

// Three dispatches of render, in the order they were made.
const (
	firstRow  = "01M3B00000000000000000000A"
	secondRow = "01M3B00000000000000000000B"
	thirdRow  = "01M3B00000000000000000000C"
)

// switchable is an authorizer that allows alice everything until it is switched off.
type switchable struct{ off atomic.Bool }

func (s *switchable) Allow(_ context.Context, who api.Principal, _ api.Permission, _ api.Target) (bool, error) {
	return who == "alice" && !s.off.Load(), nil
}

// timing is what a test gives the streams in place of the defaults.
type timing struct{ sweep, keepAlive, reauthorise, settling time.Duration }

// quiet is a timing in which nothing happens on a clock while a test runs: a line that arrives
// arrives because its shipment said so.
var quiet = timing{sweep: time.Minute, keepAlive: time.Minute, reauthorise: time.Minute, settling: time.Minute}

type streams struct {
	handler    http.Handler
	served     *httptest.Server
	pool       *db.Pool
	super      string
	auth       *switchable
	stop       chan struct{}
	credential string
	runner     string
}

func withStreams(t *testing.T, tm timing) streams {
	t.Helper()
	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	for _, stmt := range []string{
		`insert into namespaces (name) values ('finance')`,
		`insert into workflows (namespace, name) values ('finance','monthly-invoicing')`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("seeding: %s", err)
		}
	}
	objects := artifact.Dir(t.TempDir())
	grants{pool: pool, objects: objects, super: super}.recorded(t, "a3f9c1e", theTree())
	for _, stmt := range []string{
		`insert into runs (namespace, id, workflow, commit, trigger, state)
		   values ('finance','` + string(streamRun) + `','monthly-invoicing','a3f9c1e','manual','running')`,
		`insert into steps (namespace, run_id, step, state) values ('finance','` + string(streamRun) + `','render','running')`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("seeding: %s", err)
		}
	}
	if err := pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		return w.CreateRunnerPool(ctx, db.RunnerPool{Name: "dmz", Labels: []string{"zone=dmz"}, CreatedBy: "admin"})
	}); err != nil {
		t.Fatal(err)
	}

	signed, err := artifact.NewSigned(objects, artifact.SignedOptions{
		Key: []byte("0123456789abcdef0123456789abcdef"), Base: "https://agentiik.example.com/objects",
	})
	if err != nil {
		t.Fatal(err)
	}
	auth := &switchable{}
	rt, err := api.NewRouter(auth, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewRunners(rt, api.RunnerOptions{Pool: pool, Objects: objects, URLs: signed}); err != nil {
		t.Fatal(err)
	}
	versions, err := version.New(pool, version.Options{})
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	srv, err := api.NewServer(rt, api.ServerOptions{Pool: pool, Versions: versions, Objects: objects, Stopping: stop})
	if err != nil {
		t.Fatal(err)
	}
	api.StreamTiming(srv, tm.sweep, 10*time.Millisecond, tm.keepAlive, tm.reauthorise, tm.settling)

	served := httptest.NewServer(rt)
	t.Cleanup(served.Close)
	s := streams{handler: rt, served: served, pool: pool, super: super, auth: auth, stop: stop}

	var token db.JoinToken
	if err := pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		now := time.Now().UTC()
		token, err = w.IssueJoinToken(ctx, "dmz", nil, "admin", now, now.Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w, answer := call(t, rt, "POST", "/api/v1/runners", "", aMachine(token.Clear))
	if w.Code != http.StatusCreated {
		t.Fatalf("joining answered %d: %s", w.Code, w.Body)
	}
	s.credential, _ = answer["credential"].(string)
	s.runner, _ = answer["runner"].(string)
	return s
}

// sql runs one statement as the superuser.
func (s streams) sql(t *testing.T, statement string, args ...any) {
	t.Helper()
	if _, err := dbtest.Superuser(t, s.super).Exec(t.Context(), statement, args...); err != nil {
		t.Fatalf("%s: %s", statement, err)
	}
}

// dispatched writes one dispatch of render the way the controller and a redemption leave it, bound
// to the test's runner, and answers its key and its grant.
func (s streams) dispatched(t *testing.T, row string, attempt int, shard agk.Shard, requeue int, state string) (agk.TaskID, string) {
	t.Helper()
	key := agk.NewTaskID(streamRun, "render", attempt, shard)
	var index, of *int
	if !shard.IsZero() {
		index, of = &shard.Index, &shard.Of
	}
	s.sql(t, `insert into tasks (namespace, id, run_id, step, attempt, shard_index, shard_of, requeue, state, runner)
		values ('finance', $1, $2, 'render', $3, $4, $5, $6, $7, $8)`,
		row, string(streamRun), attempt, index, of, requeue, state, s.runner)
	var granted db.Granted
	if err := s.pool.Installation(t.Context(), db.ControllerSweep, func(ctx context.Context, w *db.Wide) error {
		var err error
		granted, err = w.IssueGrant(ctx, "finance", key, row, db.GrantScope{
			Run: streamRun, Step: "render", Workflow: "monthly-invoicing", Commit: "a3f9c1e",
		}, time.Now().UTC().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return key, granted.Clear
}

// ship ships one chunk of a dispatch's log, which has to be taken.
func (s streams) ship(t *testing.T, key agk.TaskID, grant string, seq, first int, final bool, lines ...string) {
	t.Helper()
	c := api.LogShipment{IdempotencyKey: key, Seq: int64(seq), FirstLine: int64(first), Final: final, Lines: []api.LogLine{}}
	for _, text := range lines {
		c.Lines = append(c.Lines, api.LogLine{At: time.Now().UTC(), Text: text})
	}
	if w, _ := shipped(t, s.handler, s.credential, grant, c); w.Code != http.StatusOK {
		t.Fatalf("chunk %d of %s answered %d: %s", seq, key, w.Code, w.Body)
	}
}

// ended writes what the controller writes when a step reaches its verdict.
func (s streams) ended(t *testing.T, verdict string) {
	t.Helper()
	s.sql(t, `update steps set state = $1 where namespace = 'finance' and run_id = $2 and step = 'render'`, verdict, string(streamRun))
}

// event is one server-sent event, or a comment, whose event is ":".
type event struct{ id, event, data string }

// reading is one stream being read.
type reading struct {
	header http.Header
	events chan event
}

// logPath is the step's log.
func logPath(step string) string {
	return "/api/v1/runs/" + string(streamRun) + "/steps/" + step + "/logs"
}

// get asks for a step's log and answers the response.
func (s streams) get(t *testing.T, as, step, lastID string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	r, err := http.NewRequestWithContext(ctx, "GET", s.served.URL+logPath(step), nil)
	if err != nil {
		t.Fatal(err)
	}
	if as != "" {
		r.Header.Set("Authorization", "Bearer "+as)
	}
	if lastID != "" {
		r.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// open reads render's log as alice, from lastID where it is given.
func (s streams) open(t *testing.T, lastID string) *reading {
	t.Helper()
	resp := s.get(t, "alice", "render", lastID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the stream answered %d", resp.StatusCode)
	}
	rd := &reading{header: resp.Header, events: make(chan event, 1024)}
	go func() {
		defer close(rd.events)
		scanner := bufio.NewScanner(resp.Body)
		var e event
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				if e.event != "" {
					rd.events <- e
				}
				e = event{}
			case strings.HasPrefix(line, ": "):
				rd.events <- event{event: ":", data: strings.TrimPrefix(line, ": ")}
			case strings.HasPrefix(line, "id: "):
				e.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				e.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				e.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return rd
}

// next is the next event that is not a comment, which has to come within a few seconds.
func (rd *reading) next(t *testing.T) event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e, ok := <-rd.events:
			if !ok {
				t.Fatal("the stream ended where an event was expected")
			}
			if e.event != ":" {
				return e
			}
		case <-deadline:
			t.Fatal("no event came")
		}
	}
}

// expect is the next event, which has to be of that kind with that id, and answers its data.
func (rd *reading) expect(t *testing.T, kind, id string) map[string]any {
	t.Helper()
	e := rd.next(t)
	if e.event != kind || e.id != id {
		t.Fatalf("the stream sent %s %q: %s, want %s %q", e.event, e.id, e.data, kind, id)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(e.data), &data); err != nil {
		t.Fatalf("the data of %s is not JSON: %s", kind, e.data)
	}
	return data
}

// line is the next event, which has to be that line of that dispatch saying that.
func (rd *reading) line(t *testing.T, row string, seq, n int, text string) {
	t.Helper()
	data := rd.expect(t, "line", row+"/"+strconv.Itoa(seq)+"/"+strconv.Itoa(n))
	if data["text"] != text || data["line"] != float64(n) || data["task_id"] != row {
		t.Errorf("line %d of %s is %v, want %q", n, row, data, text)
	}
}

// silent is a stream that sends nothing but comments for a while.
func (rd *reading) silent(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case e, ok := <-rd.events:
			if !ok {
				t.Fatal("the stream ended where it should have waited")
			}
			if e.event != ":" {
				t.Fatalf("the stream sent %s %q: %s, where it should have waited", e.event, e.id, e.data)
			}
		case <-deadline:
			return
		}
	}
}

// closed is a stream that ends within a few seconds, with no event before it.
func (rd *reading) closed(t *testing.T) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e, ok := <-rd.events:
			if !ok {
				return
			}
			if e.event != ":" {
				t.Fatalf("the stream sent %s %q: %s, where it should have ended", e.event, e.id, e.data)
			}
		case <-deadline:
			t.Fatal("the stream did not end")
		}
	}
}

// A reader is sent what each dispatch's log holds, in the order the dispatches were made, and then
// what the one still going ships while they read, as it is shipped rather than on a clock; the
// stream ends once the step has its verdict and the last log is closed.
func TestAStepLogIsItsHistoryThenWhatIsShippedWhileItIsRead(t *testing.T) {
	s := withStreams(t, quiet)
	first, firstGrant := s.dispatched(t, firstRow, 1, agk.Shard{}, 0, "failed")
	s.ship(t, first, firstGrant, 1, 1, false, "one", "two")
	s.ship(t, first, firstGrant, 2, 3, true, "three")
	second, secondGrant := s.dispatched(t, secondRow, 2, agk.Shard{}, 0, "running")
	s.ship(t, second, secondGrant, 1, 1, false, "again")

	rd := s.open(t, "")
	for header, want := range map[string]string{
		"Content-Type": "text/event-stream", "Cache-Control": "no-store", "X-Accel-Buffering": "no",
	} {
		if got := rd.header.Get(header); got != want {
			t.Errorf("%s is %q, want %q", header, got, want)
		}
	}
	d := rd.expect(t, "dispatch", firstRow+"/0/0")
	if d["idempotency_key"] != string(first) || d["attempt"] != float64(1) || d["requeue"] != float64(0) {
		t.Errorf("the first dispatch is announced as %v", d)
	}
	rd.line(t, firstRow, 1, 1, "one")
	rd.line(t, firstRow, 1, 2, "two")
	rd.line(t, firstRow, 2, 3, "three")
	if end := rd.expect(t, "dispatch_end", ""); end["final"] != true || end["lines"] != float64(3) || end["truncated"] != false {
		t.Errorf("the first dispatch ends as %v", end)
	}
	if d := rd.expect(t, "dispatch", secondRow+"/0/0"); d["attempt"] != float64(2) {
		t.Errorf("the second dispatch is announced as %v", d)
	}
	rd.line(t, secondRow, 1, 1, "again")

	// Live, and told by the shipment: the sweep is a minute away.
	s.ship(t, second, secondGrant, 2, 2, false, "still going")
	rd.line(t, secondRow, 2, 2, "still going")

	// The step's verdict, then the chunk that closes the last log, whose shipment is what the
	// stream hears: a runner closes a log before it reports, so the task is still running when
	// its last chunk lands.
	s.ended(t, "succeeded")
	s.ship(t, second, secondGrant, 3, 3, true)
	if end := rd.expect(t, "dispatch_end", ""); end["final"] != true || end["lines"] != float64(2) {
		t.Errorf("the second dispatch ends as %v", end)
	}
	if end := rd.expect(t, "end", ""); end["verdict"] != "succeeded" {
		t.Errorf("the step's log ends as %v", end)
	}
	rd.closed(t)
}

// A reader who reconnects with the id of the last event they had is sent what follows it and
// nothing before; an id the stream never gave is refused rather than guessed at.
func TestAStreamResumesFromTheLastEventItsReaderHad(t *testing.T) {
	s := withStreams(t, quiet)
	first, firstGrant := s.dispatched(t, firstRow, 1, agk.Shard{}, 0, "failed")
	s.ship(t, first, firstGrant, 1, 1, false, "one", "two")
	s.ship(t, first, firstGrant, 2, 3, true, "three")
	second, secondGrant := s.dispatched(t, secondRow, 2, agk.Shard{}, 0, "succeeded")
	s.ship(t, second, secondGrant, 1, 1, true, "again")
	s.ended(t, "succeeded")

	rd := s.open(t, firstRow+"/1/1")
	rd.line(t, firstRow, 1, 2, "two")
	rd.line(t, firstRow, 2, 3, "three")
	rd.expect(t, "dispatch_end", "")
	rd.expect(t, "dispatch", secondRow+"/0/0")
	rd.line(t, secondRow, 1, 1, "again")
	rd.expect(t, "dispatch_end", "")
	rd.expect(t, "end", "")
	rd.closed(t)

	// From a dispatch event, the dispatch's lines and not its event again.
	rd = s.open(t, secondRow+"/0/0")
	rd.line(t, secondRow, 1, 1, "again")
	rd.expect(t, "dispatch_end", "")
	rd.expect(t, "end", "")

	for _, id := range []string{"yesterday", firstRow + "/1", firstRow + "/-1/2", thirdRow + "/1/1", "01M3B00000000000000000000A/99999999999/1"} {
		if resp := s.get(t, "alice", "render", id); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("Last-Event-ID %q answered %d", id, resp.StatusCode)
		}
	}
}

// Every dispatch of a key is sent, labelled with its attempt, its shard and how many times it was
// requeued, so that a reader tells a retry from a requeue; a lost dispatch is let go of at once
// rather than holding back the one that replaced it, and says its log was never closed.
func TestDispatchesAreLabelledSoAttemptsAndRequeuesAreToldApart(t *testing.T) {
	s := withStreams(t, quiet)
	shard := agk.Shard{Index: 2, Of: 3}
	lost, lostGrant := s.dispatched(t, firstRow, 1, shard, 0, "lost")
	s.ship(t, lost, lostGrant, 1, 1, false, "cut off")
	requeued, requeuedGrant := s.dispatched(t, secondRow, 1, shard, 1, "running")
	s.ship(t, requeued, requeuedGrant, 1, 1, false, "handed out again")

	rd := s.open(t, "")
	d := rd.expect(t, "dispatch", firstRow+"/0/0")
	if d["idempotency_key"] != string(lost) || d["requeue"] != float64(0) || d["attempt"] != float64(1) {
		t.Errorf("the lost dispatch is announced as %v", d)
	}
	if sh, _ := d["shard"].(map[string]any); sh["index"] != float64(2) || sh["of"] != float64(3) {
		t.Errorf("the lost dispatch's shard is %v", d["shard"])
	}
	rd.line(t, firstRow, 1, 1, "cut off")
	if end := rd.expect(t, "dispatch_end", ""); end["final"] != false || end["lines"] != float64(1) {
		t.Errorf("the lost dispatch ends as %v", end)
	}
	d = rd.expect(t, "dispatch", secondRow+"/0/0")
	if d["idempotency_key"] != string(lost) || d["requeue"] != float64(1) || d["attempt"] != float64(1) {
		t.Errorf("the requeue is announced as %v", d)
	}
	rd.line(t, secondRow, 1, 1, "handed out again")
	rd.silent(t, 300*time.Millisecond)
}

// A dispatch the control plane stopped is waited for while its runner stops the container and ships
// the last lines, since those are the lines that say what it was doing; past the settling time it
// is let go of, its log unclosed.
func TestAStoppedDispatchIsWaitedForUntilItsLastLinesArrive(t *testing.T) {
	s := withStreams(t, timing{sweep: 100 * time.Millisecond, keepAlive: time.Minute, reauthorise: time.Minute, settling: time.Minute})
	key, grant := s.dispatched(t, firstRow, 1, agk.Shard{}, 0, "cancelled")
	s.sql(t, `update tasks set finished_at = now() where id = $1`, firstRow)
	s.ship(t, key, grant, 1, 1, false, "working")
	s.ended(t, "cancelled")

	rd := s.open(t, "")
	rd.expect(t, "dispatch", firstRow+"/0/0")
	rd.line(t, firstRow, 1, 1, "working")
	rd.silent(t, 500*time.Millisecond)
	s.ship(t, key, grant, 2, 2, true, "terminated")
	rd.line(t, firstRow, 2, 2, "terminated")
	if end := rd.expect(t, "dispatch_end", ""); end["final"] != true {
		t.Errorf("the stopped dispatch ends as %v", end)
	}
	if end := rd.expect(t, "end", ""); end["verdict"] != "cancelled" {
		t.Errorf("the step's log ends as %v", end)
	}

	// Stopped a minute ago and never closed: let go of on the next sweep.
	s.sql(t, `update tasks set finished_at = now() - interval '61 seconds' where id = $1`, firstRow)
	s.sql(t, `delete from task_log_chunks where task_id = $1 and seq = 2`, firstRow)
	s.sql(t, `update task_logs set final_seq = null, next_seq = 2, lines = 1 where task_id = $1`, firstRow)
	rd = s.open(t, "")
	rd.expect(t, "dispatch", firstRow+"/0/0")
	rd.line(t, firstRow, 1, 1, "working")
	if end := rd.expect(t, "dispatch_end", ""); end["final"] != false {
		t.Errorf("the stopped dispatch that never closed its log ends as %v", end)
	}
	rd.expect(t, "end", "")
}

// What no shipment announces, a step that reached its verdict, is heard at the next sweep.
func TestAStreamHearsItsStepEndAtTheNextSweep(t *testing.T) {
	s := withStreams(t, timing{sweep: 100 * time.Millisecond, keepAlive: time.Minute, reauthorise: time.Minute, settling: time.Minute})
	rd := s.open(t, "")
	rd.silent(t, 300*time.Millisecond)
	s.ended(t, "skipped")
	if end := rd.expect(t, "end", ""); end["verdict"] != "skipped" {
		t.Errorf("the step's log ends as %v", end)
	}
	rd.closed(t)
}

// A stream nothing is written to is kept alive by comments, which no client dispatches, so that a
// proxy in between does not take it for a dead connection.
func TestAnIdleStreamIsKeptAlive(t *testing.T) {
	s := withStreams(t, timing{sweep: time.Minute, keepAlive: 100 * time.Millisecond, reauthorise: time.Minute, settling: time.Minute})
	rd := s.open(t, "")
	select {
	case e := <-rd.events:
		if e.event != ":" || e.data != "keep-alive" {
			t.Errorf("an idle stream sent %v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an idle stream sent nothing")
	}
}

// A reader who loses run:read while reading is cut off, without the event that says the log is
// over, and their reconnection is refused as any request would be.
func TestAReaderWhoLosesAccessIsCutOff(t *testing.T) {
	s := withStreams(t, timing{sweep: time.Minute, keepAlive: time.Minute, reauthorise: 100 * time.Millisecond, settling: time.Minute})
	key, grant := s.dispatched(t, firstRow, 1, agk.Shard{}, 0, "running")
	s.ship(t, key, grant, 1, 1, false, "working")
	rd := s.open(t, "")
	rd.expect(t, "dispatch", firstRow+"/0/0")
	rd.line(t, firstRow, 1, 1, "working")
	rd.silent(t, 300*time.Millisecond)

	s.auth.off.Store(true)
	rd.closed(t)
	if resp := s.get(t, "alice", "render", firstRow+"/1/1"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("reconnecting without access answered %d", resp.StatusCode)
	}
}

// An API asked to stop ends its streams at once, without the event that says the log is over, so
// that their readers reconnect to another and resume there.
func TestAStoppingAPIEndsItsStreamsWithoutSayingTheLogIsOver(t *testing.T) {
	s := withStreams(t, quiet)
	rd := s.open(t, "")
	rd.silent(t, 200*time.Millisecond)
	close(s.stop)
	rd.closed(t)
}

// A stream is refused what a request for it would be: no step of that name, no access, no
// credential, and a run whose logs are past their retention, which existed and are gone.
func TestAStreamIsRefusedWhatARequestWouldBe(t *testing.T) {
	s := withStreams(t, quiet)
	for _, c := range []struct {
		as, step string
		want     int
	}{
		{"alice", "invoice", http.StatusNotFound},
		{"alice", "not a step", http.StatusNotFound},
		{"mallory", "render", http.StatusNotFound},
		{"", "render", http.StatusUnauthorized},
	} {
		if resp := s.get(t, c.as, c.step, ""); resp.StatusCode != c.want {
			t.Errorf("step %q as %q answered %d, want %d", c.step, c.as, resp.StatusCode, c.want)
		}
	}
	s.sql(t, `update runs set state = 'succeeded', started_at = now() - interval '2 days',
		finished_at = now() - interval '1 day', expires_at = now() - interval '1 hour' where id = $1`, string(streamRun))
	if resp := s.get(t, "alice", "render", ""); resp.StatusCode != http.StatusGone {
		t.Errorf("a run past its retention answered %d", resp.StatusCode)
	}
}

// The route is guarded by run:read and reveals nothing more to anybody: a log is diagnostics,
// masked before it was written, and "reading them needs run:read".
func TestAStepLogIsGuardedByRunRead(t *testing.T) {
	s := withStreams(t, quiet)
	for _, r := range s.handler.(*api.Router).Routes() {
		if r.Pattern == "/api/v1/runs/{run}/steps/{step}/logs" {
			if r.Method != "GET" || r.Permission != api.RunRead || !r.OfRun || r.Reveals != "" {
				t.Errorf("the log stream is guarded as %+v", r)
			}
			return
		}
	}
	t.Fatal("the log stream is not served")
}

// A log shipped in more chunks than one read takes is sent whole and in order, the last chunk
// included, before the stream lets go of it.
func TestALogOfManyChunksIsSentWhole(t *testing.T) {
	s := withStreams(t, quiet)
	key, grant := s.dispatched(t, firstRow, 1, agk.Shard{}, 0, "succeeded")
	const chunks = 40
	for seq := 1; seq <= chunks; seq++ {
		s.ship(t, key, grant, seq, seq, seq == chunks, "line "+strconv.Itoa(seq))
	}
	s.ended(t, "succeeded")

	rd := s.open(t, "")
	rd.expect(t, "dispatch", firstRow+"/0/0")
	for seq := 1; seq <= chunks; seq++ {
		rd.line(t, firstRow, seq, seq, "line "+strconv.Itoa(seq))
	}
	if end := rd.expect(t, "dispatch_end", ""); end["lines"] != float64(chunks) || end["final"] != true {
		t.Errorf("the log ends as %v", end)
	}
	rd.expect(t, "end", "")
}
