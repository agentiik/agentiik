package runner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	server "github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/dockertest"
	"github.com/agentiik/agentiik/internal/fixtures"
	"github.com/jackc/pgx/v5"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// A task's log, shipped to the API while its container runs and closed before its result goes out.

// fakeLogs is an API taking shipments as the real one does, in order and once, in memory, and
// answering where each log stands.
type fakeLogs struct {
	mu      sync.Mutex
	shipped []LogShipment
	kept    map[string][]LogLine
	next    map[string]int
	closed  map[string]bool

	// refuse, where it answers an error, answers shipment n (from 1) with it instead.
	refuse func(n int, c LogShipment) error
	// cutAt is the cap on a log's lines, none where it is zero.
	cutAt int
}

func (f *fakeLogs) ShipLog(_ context.Context, grant string, c LogShipment) (LogShipped, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shipped = append(f.shipped, c)
	if f.refuse != nil {
		if err := f.refuse(len(f.shipped), c); err != nil {
			return LogShipped{}, err
		}
	}
	if f.kept == nil {
		f.kept, f.next, f.closed = map[string][]LogLine{}, map[string]int{}, map[string]bool{}
	}
	uri, err := agk.NewLogURI(agk.TaskID(c.IdempotencyKey))
	if err != nil {
		return LogShipped{}, err
	}
	key := c.IdempotencyKey
	if f.next[key] == 0 {
		f.next[key] = 1
	}
	accepted := 0
	cut := f.cutAt > 0 && len(f.kept[key]) >= f.cutAt
	if c.Seq == f.next[key] {
		for _, l := range c.Lines {
			if f.cutAt > 0 && len(f.kept[key]) >= f.cutAt {
				cut = true
				break
			}
			f.kept[key] = append(f.kept[key], l)
			accepted++
		}
		f.next[key]++
		f.closed[key] = c.Final
	}
	return LogShipped{URI: uri.String(), Accepted: accepted, NextSeq: f.next[key], Lines: len(f.kept[key]), Truncated: cut}, nil
}

// all is every shipment made, in order.
func (f *fakeLogs) all() []LogShipment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.shipped)
}

// texts is the log of key as it was kept.
func (f *fakeLogs) texts(key string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var texts []string
	for _, l := range f.kept[key] {
		texts = append(texts, l.Text)
	}
	return texts
}

// unavailable is an answer that is no answer: a 503.
func unavailable() error {
	return &APIError{Method: http.MethodPost, Path: logsPath, Status: http.StatusServiceUnavailable, class: ErrUnavailable}
}

// aKey is the task every shipment below is the log of.
const aKey = string(storeRun) + "/invoice/1"

// aShipment is the log of aKey, shipped through to, and opened as the driver opens it.
func aShipment(t *testing.T, to LogShipper, within time.Duration) *shipment {
	t.Helper()
	m := bus.TaskMessage{IdempotencyKey: aKey, Grant: "agkgrant_01JMZ8V1PC7K3M0QY4B8ZR6TDN_Zm9vYmFyYmF6cXV4MTIzNA"}
	s := newShipment(t.Context(), to, m, func(s string) { t.Log(s) }, 5*time.Millisecond, within)
	s.retry = 5 * time.Millisecond
	t.Cleanup(s.abandon)
	w, err := TaskLogs{}.OpenLog(withShipment(t.Context(), s), agk.TaskID(aKey))
	if err != nil {
		t.Fatal(err)
	}
	if w != io.WriteCloser(s) {
		t.Fatal("the driver was not given the task's shipment")
	}
	return s
}

// writes writes lines into s as the driver writes them, one JSON line a line, on stream.
func writes(t *testing.T, s io.Writer, stream driver.Stream, texts ...string) {
	t.Helper()
	for _, text := range texts {
		b, err := json.Marshal(driver.Line{At: time.Now().UTC(), Index: 1, Stream: stream, Text: text})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

// Every shipment is the wire's logShipment request, the grant in the Agentiik-Grant header and never
// in the body. A log longer than one chunk goes as several, each within the 4,096 lines and the
// mebibyte the API takes, numbered from 1 and each beginning where the one before it ended, and the
// last one closes it. Standard output is not shipped: it belongs to the result.
func TestEveryShipmentIsTheWiresWithTheGrantInItsHeader(t *testing.T) {
	doc, err := fixtures.Wire()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("wire.schema.json", raw); err != nil {
		t.Fatal(err)
	}
	request, err := c.Compile("wire.schema.json#/$defs/logShipment/properties/request")
	if err != nil {
		t.Fatal(err)
	}
	response, err := c.Compile("wire.schema.json#/$defs/logShipment/properties/response")
	if err != nil {
		t.Fatal(err)
	}

	const grant = "agkgrant_01JMZ8V1PC7K3M0QY4B8ZR6TDN_Zm9vYmFyYmF6cXV4MTIzNA"
	var (
		mu     sync.Mutex
		bodies [][]byte
		logs   fakeLogs
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		if r.URL.Path != logsPath || r.Header.Get("Agentiik-Grant") != grant || r.Header.Get("Authorization") != "Bearer "+string(credential) {
			http.Error(w, `{"error":"not the route, the grant or the credential"}`, http.StatusBadRequest)
			return
		}
		var c LogShipment
		if err := json.Unmarshal(body, &c); err != nil {
			http.Error(w, `{"error":"not a shipment"}`, http.StatusBadRequest)
			return
		}
		a, _ := logs.ShipLog(r.Context(), grant, c)
		b, _ := json.Marshal(a)
		if v, err := jsonschema.UnmarshalJSON(bytes.NewReader(b)); err != nil || response.Validate(v) != nil {
			t.Errorf("the fake API answered what the wire refuses: %s", b)
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	client, err := NewClient(srv.URL, credential, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Nothing is shipped until the close, so that every line is waiting then and the chunks are
	// as full as the bounds let them be.
	s := newShipment(t.Context(), client, bus.TaskMessage{IdempotencyKey: aKey, Grant: grant}, func(s string) { t.Log(s) }, time.Hour, 10*time.Second)
	t.Cleanup(s.abandon)
	w, err := TaskLogs{}.OpenLog(withShipment(t.Context(), s), agk.TaskID(aKey))
	if err != nil {
		t.Fatal(err)
	}
	// Many short lines, which the bound on lines cuts, then long ones, which the bound on bytes
	// does.
	long := strings.Repeat("é", 32<<10)
	var want []string
	for i := range 5025 {
		text := fmt.Sprintf("line %d", i+1)
		if i >= 5000 {
			text += " " + long
		}
		want = append(want, text)
		writes(t, w, driver.Stderr, text)
		writes(t, w, driver.Stdout, `{"items":[]}`)
	}
	w.Close()
	log := s.finish(false)

	if log == nil || log.Lines != 5025 || log.Truncated {
		t.Fatalf("the result's log is %+v", log)
	}
	if got := logs.texts(aKey); !slices.Equal(got, want) {
		t.Errorf("the API was shipped %d lines, want the %d written on standard error", len(got), len(want))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 3 {
		t.Fatalf("5,025 lines went in %d shipments", len(bodies))
	}
	next := 1
	for i, body := range bodies {
		v, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if err := request.Validate(v); err != nil {
			t.Errorf("shipment %d is not the wire's: %s", i+1, err)
		}
		if bytes.Contains(body, []byte(grant)) {
			t.Errorf("shipment %d carries the grant in its body", i+1)
		}
		if len(body) > 1<<20 {
			t.Errorf("shipment %d is %d bytes, past the mebibyte the API takes", i+1, len(body))
		}
		var c LogShipment
		json.Unmarshal(body, &c)
		if c.Seq != i+1 || c.FirstLine != next || len(c.Lines) > 4096 || c.Final != (i == len(bodies)-1) {
			t.Errorf("shipment %d is chunk %d from line %d with %d lines, final %t, and line %d was next", i+1, c.Seq, c.FirstLine, len(c.Lines), c.Final, next)
		}
		next += len(c.Lines)
	}
}

// A running container's lines are shipped while it runs, not held until it exits.
func TestLinesAreShippedWhileTheContainerRuns(t *testing.T) {
	logs := &fakeLogs{}
	s := aShipment(t, logs, time.Second)
	writes(t, s, driver.Stderr, "reading 412 invoices")
	eventually(t, "the first line reaching the API", func() bool { return len(logs.texts(aKey)) == 1 })
	for _, c := range logs.all() {
		if c.Final {
			t.Error("the log was closed while the container ran")
		}
	}
}

// A chunk that gets no answer is shipped again as it was, the same seq and the same lines, however
// many lines came after it; the lines that came after go in the chunks after it.
func TestAChunkThatGotNoAnswerIsShippedAgainAsItWas(t *testing.T) {
	var fail atomic.Int64
	fail.Store(3)
	logs := &fakeLogs{refuse: func(n int, c LogShipment) error {
		if fail.Load() > 0 && c.Seq == 1 {
			fail.Add(-1)
			return unavailable()
		}
		return nil
	}}
	s := aShipment(t, logs, 10*time.Second)
	writes(t, s, driver.Stderr, "one", "two")
	eventually(t, "the first chunk being refused", func() bool { return len(logs.all()) >= 1 })
	writes(t, s, driver.Stderr, "three")
	s.Close()
	log := s.finish(false)

	var first []LogShipment
	for _, c := range logs.all() {
		if c.Seq == 1 {
			first = append(first, c)
		}
	}
	if len(first) != 4 {
		t.Fatalf("chunk 1 was shipped %d times, and it got no answer three times", len(first))
	}
	for _, c := range first[1:] {
		if mustJSON(t, c) != mustJSON(t, first[0]) {
			t.Errorf("chunk 1 was shipped again as %s, and it was first %s", mustJSON(t, c), mustJSON(t, first[0]))
		}
	}
	if got := logs.texts(aKey); !slices.Equal(got, []string{"one", "two", "three"}) {
		t.Errorf("the API holds %q", got)
	}
	if log == nil || log.Lines != 3 || log.Truncated {
		t.Errorf("the result's log is %+v", log)
	}
}

// An API that does not answer the closing chunk is asked again, with the same chunk, until the close
// has been waited for as long as it is; the result then reports the lines the API last said it holds,
// and the log truncated, since the store holds less than the container wrote.
func TestAClosingChunkNeverAnsweredIsGivenUpAndTheLogReportedTruncated(t *testing.T) {
	down := atomic.Bool{}
	logs := &fakeLogs{refuse: func(int, LogShipment) error {
		if down.Load() {
			return unavailable()
		}
		return nil
	}}
	s := aShipment(t, logs, 1500*time.Millisecond)
	writes(t, s, driver.Stderr, "one", "two")
	eventually(t, "the first lines reaching the API", func() bool { return len(logs.texts(aKey)) == 2 })
	down.Store(true)
	writes(t, s, driver.Stderr, "three")
	s.Close()

	began := time.Now()
	log := s.finish(false)
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("the close was waited for %s, and it is waited for 1.5 s", took)
	}
	if log == nil || log.Lines != 2 || !log.Truncated || log.URI != "agk://log/"+string(storeRun)+"/"+string(storeRun)+"%2Finvoice%2F1" {
		t.Errorf("the result's log is %+v, and the API last said it holds two lines", log)
	}
	var closing []LogShipment
	for _, c := range logs.all() {
		if c.Final {
			closing = append(closing, c)
		}
	}
	if len(closing) < 2 {
		t.Fatalf("the closing chunk was shipped %d times, and it got no answer", len(closing))
	}
	for _, c := range closing {
		if c.Seq != 2 || c.FirstLine != 3 || len(c.Lines) != 1 {
			t.Errorf("the closing chunk was shipped as chunk %d from line %d with %d lines", c.Seq, c.FirstLine, len(c.Lines))
		}
	}
}

// A refusal no chunk gets past, a credential or a grant that opens nothing, ends the shipping at
// once: nothing is shipped again, and the close waits for nothing.
func TestARefusedShipmentIsNotShippedAgain(t *testing.T) {
	logs := &fakeLogs{refuse: func(int, LogShipment) error {
		return &APIError{Method: http.MethodPost, Path: logsPath, Status: http.StatusUnauthorized, class: ErrCredentialRefused}
	}}
	s := aShipment(t, logs, 10*time.Second)
	writes(t, s, driver.Stderr, "one")
	eventually(t, "the first chunk being refused", func() bool { return len(logs.all()) == 1 })
	writes(t, s, driver.Stderr, "two")
	s.Close()
	began := time.Now()
	if log := s.finish(false); log != nil {
		t.Errorf("a log the API never answered for is reported as %+v", log)
	}
	if took := time.Since(began); took > time.Second {
		t.Errorf("a refused log was waited on for %s", took)
	}
	if n := len(logs.all()); n != 1 {
		t.Errorf("a refused log was shipped %d times", n)
	}
}

// "A runner reading true stops shipping": once the API says the cap cut the log, nothing more is
// shipped but the closing chunk, with no lines, and the result reports the log truncated.
func TestALogTheAPICutIsShippedNoFurtherAndReportedTruncated(t *testing.T) {
	logs := &fakeLogs{cutAt: 2}
	s := aShipment(t, logs, 10*time.Second)
	writes(t, s, driver.Stderr, "one", "two", "three")
	eventually(t, "the API cutting the log", func() bool { return len(logs.all()) == 1 })
	writes(t, s, driver.Stderr, "four", "five")
	s.Close()
	log := s.finish(false)
	if log == nil || log.Lines != 2 || !log.Truncated {
		t.Errorf("the result's log is %+v", log)
	}
	all := logs.all()
	if len(all) != 2 || !all[1].Final || len(all[1].Lines) != 0 {
		t.Errorf("after the cut the runner shipped %s", mustJSON(t, all))
	}
}

// A log the driver cut is reported truncated, although the API held every line it was shipped: the
// runner's caps count what the container wrote on both streams, and the line saying so is shipped,
// but the API has no other way to learn it.
func TestALogTheRunnersCapCutIsReportedTruncated(t *testing.T) {
	c := carrierWith(t, func(c dockertest.Container) (int, error) {
		for i := range 5 {
			fmt.Fprintf(c.Stderr, "line %d\n", i+1)
		}
		return 0, nil
	}, func(p *driver.Policy) { p.LogMaxLines = 3 })
	_, r := c.carry(t, nil)
	if r.Log == nil || !r.Log.Truncated || r.Log.Lines != 4 {
		t.Errorf("a log cut at three lines, the line saying so a fourth, is reported %+v", r.Log)
	}
}

// A log API is the real API's runner routes on a real database, a runner joined to a pool of its
// own, and the task every carrier above runs dispatched and bound to that runner, so that its grant
// opens its log.
type logAPI struct {
	url     string
	pool    *db.Pool
	conn    *pgx.Conn
	dir     string
	objects artifact.Objects
	client  *Client

	// front, where it is set, answers each request in place of the API, given the API's own
	// handler to call.
	mu    sync.Mutex
	front func(w http.ResponseWriter, r *http.Request, api http.Handler)
}

// theRow is the dispatch of aKey the log API holds, which is the one objectStore.taskFor names.
const theRow = "01JMZ8V1PC7K3M0QY4B8ZR6TDN"

func aLogAPI(t *testing.T) *logAPI {
	t.Helper()
	pool, super := dbtest.Open(t)
	in := &logAPI{pool: pool, conn: dbtest.Superuser(t, super), dir: t.TempDir()}
	in.objects = artifact.Dir(in.dir)
	rt, err := server.NewRouter(server.DenyAll{}, func(*http.Request) (server.Principal, error) {
		return "", errors.New("this installation speaks to runners and to nobody else")
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.NewRunners(rt, server.RunnerOptions{Pool: pool, Objects: in.objects}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		in.mu.Lock()
		front := in.front
		in.mu.Unlock()
		if front != nil {
			front(w, r, rt)
			return
		}
		rt.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	in.url = srv.URL

	ctx := t.Context()
	for _, stmt := range []string{
		`insert into namespaces (name) values ('finance')`,
		`insert into workflows (namespace, name) values ('finance', 'monthly-invoicing')`,
	} {
		if _, err := in.conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("seeding: %s", err)
		}
	}
	entry := []byte("apiVersion: agentiik.dev/v1\nkind: Workflow\n")
	if err := pool.In(ctx, "finance", func(ctx context.Context, ns *db.NS) error {
		_, err := ns.SaveVersion(ctx, db.Version{
			Workflow: "monthly-invoicing", Commit: servedCommit, Entry: "agentiik.yaml", Document: entry,
			Tree:   []db.TreeFile{{Path: "agentiik.yaml", SHA256: sha(entry), Size: int64(len(entry)), Mode: "0644"}},
			Author: "alice",
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var credential Secret
	now := time.Now().UTC()
	if err := pool.Installation(ctx, db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		if err := w.CreateRunnerPool(ctx, db.RunnerPool{Name: "dmz", Labels: []string{"zone=dmz"}, AcceptedNamespaces: []string{"finance"}, CreatedBy: "admin"}); err != nil {
			return err
		}
		token, err := w.IssueJoinToken(ctx, "dmz", nil, "admin", now, now.Add(time.Hour))
		if err != nil {
			return err
		}
		joined, err := w.Join(ctx, db.Joining{
			Token: token.Clear, PublicKey: make(ed25519.PublicKey, ed25519.PublicKeySize), CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
			Architecture: "amd64", AgentVersion: "0.2.0",
		}, time.Hour, now)
		credential = Secret(joined.Credential)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if in.client, err = NewClient(in.url, credential, nil); err != nil {
		t.Fatal(err)
	}
	return in
}

// held dispatches the task m names, issues its grant into m, and binds it to the runner, as its
// redemption would.
func (in *logAPI) held(t *testing.T, m *bus.TaskMessage) {
	t.Helper()
	ctx := t.Context()
	for _, stmt := range []string{
		`insert into runs (namespace, id, workflow, commit, trigger) values ('finance', $1, 'monthly-invoicing', '` + servedCommit + `', 'manual')`,
		`insert into steps (namespace, run_id, step) values ('finance', $1, 'invoice')`,
	} {
		if _, err := in.conn.Exec(ctx, stmt, m.RunID); err != nil {
			t.Fatalf("seeding: %s", err)
		}
	}
	if _, err := in.conn.Exec(ctx, `insert into tasks (namespace, id, run_id, step, attempt, state, dispatched_at, published_at)
		values ('finance', $1, $2, 'invoice', 1, 'dispatched', now(), now())`, m.TaskID, m.RunID); err != nil {
		t.Fatalf("seeding: %s", err)
	}
	var granted db.Granted
	if err := in.pool.Installation(ctx, db.ControllerSweep, func(ctx context.Context, w *db.Wide) error {
		var err error
		granted, err = w.IssueGrant(ctx, "finance", agk.TaskID(m.IdempotencyKey), m.TaskID, db.GrantScope{
			Run: agk.RunID(m.RunID), Step: "invoice", Workflow: "monthly-invoicing", Commit: servedCommit,
		}, time.Now().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	m.Grant = granted.Clear
	if _, err := in.conn.Exec(ctx, `update tasks set runner = (select id from runners limit 1) where id = $1`, m.TaskID); err != nil {
		t.Fatal(err)
	}
}

// logged is the log of dispatch row as a reader finds it: where it stands, and every line it holds.
func (in *logAPI) logged(t *testing.T, row string) (db.TaskLog, []string) {
	t.Helper()
	var l db.TaskLog
	var chunks []db.LogChunk
	err := in.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		var err error
		l, chunks, err = ns.TaskLog(ctx, row)
		return err
	})
	if errors.Is(err, db.ErrNoLog) {
		return db.TaskLog{}, nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, c := range chunks {
		lines, err := server.ReadLogChunk(t.Context(), in.objects, c)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range lines {
			texts = append(texts, line.Text)
		}
	}
	return l, texts
}

// carrying is a carrier shipping to the log API, and the message of the task it holds there.
func (in *logAPI) carrying(t *testing.T, run func(dockertest.Container) (int, error), secrets []RedeemedSecret) (*carrying, bus.TaskMessage, *Assembled) {
	t.Helper()
	c := carrier(t, run)
	c.carrier.Logs = in.client
	c.carrier.shipEvery = 20 * time.Millisecond
	m, r := c.store.taskFor(t, nil, map[string]file{"agentiik.yaml": {"version: 1\n", "0644"}}, secrets)
	in.held(t, &m)
	a, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: c.root})
	if err != nil {
		t.Fatalf("assembling the task: %s", err)
	}
	return c, m, a
}

// A running task's lines can be read, where the API keeps them, before the task ends; once it has,
// its log is closed and the result reports it as the API last answered.
func TestARunningTasksLinesCanBeReadBeforeItEnds(t *testing.T) {
	in := aLogAPI(t)
	release := make(chan struct{})
	c, m, a := in.carrying(t, func(c dockertest.Container) (int, error) {
		fmt.Fprintln(c.Stderr, "reading 412 invoices")
		<-release
		fmt.Fprintln(c.Stderr, "charged 412 invoices")
		return 0, nil
	}, nil)
	// "A runner ships the closing chunk before it publishes the result."
	c.bus.look = func() string {
		if l, _ := in.logged(t, m.TaskID); l.FinalSeq == 0 {
			return "an open log"
		}
		return ""
	}

	carried := make(chan error, 1)
	go func() { carried <- c.carrier.Carry(t.Context(), m, a) }()
	eventually(t, "the first line being readable", func() bool {
		_, lines := in.logged(t, m.TaskID)
		return slices.Equal(lines, []string{"reading 412 invoices"})
	})
	if l, _ := in.logged(t, m.TaskID); l.FinalSeq != 0 {
		t.Errorf("the log was closed while its container ran: %+v", l)
	}
	if n := len(c.bus.all()); n != 0 {
		t.Errorf("%d results were published while the container ran", n)
	}
	close(release)
	if err := <-carried; err != nil {
		t.Fatal(err)
	}
	if len(c.bus.seen) != 1 || c.bus.seen[0] != "" {
		t.Errorf("the result was published with %q", c.bus.seen)
	}

	l, lines := in.logged(t, m.TaskID)
	// The driver's own last word follows the container's.
	if len(lines) != 3 || !slices.Equal(lines[:2], []string{"reading 412 invoices", "charged 412 invoices"}) || l.FinalSeq == 0 {
		t.Errorf("the ended task's log stands at %+v holding %q", l, lines)
	}
	r := c.bus.all()[0]
	wireSays(t, taskResults(t), r)
	uri, _ := agk.NewLogURI(agk.TaskID(m.IdempotencyKey))
	if r.Log == nil || r.Log.URI != uri.String() || r.Log.Lines != l.Lines || r.Log.Truncated {
		t.Errorf("the result's log is %+v, and the API holds %d lines at %s", r.Log, l.Lines, uri)
	}
}

// A secret the container prints is masked before its line leaves the host: neither the log a reader
// finds nor any byte the store holds carries it.
func TestASecretPrintedByTheContainerNeverReachesTheStoredLog(t *testing.T) {
	in := aLogAPI(t)
	const value = "bk_live_7Qm2rXt9vZa4"
	c, m, a := in.carrying(t, func(c dockertest.Container) (int, error) {
		fmt.Fprintf(c.Stderr, "calling billing with %s\n", value)
		fmt.Fprintf(c.Stderr, "Authorization: Bearer %s", value)
		return 0, nil
	}, []RedeemedSecret{{Name: "billing", Mount: "/agk/secrets/billing", Encoding: "utf-8", Value: value}})
	if err := c.carrier.Carry(t.Context(), m, a); err != nil {
		t.Fatal(err)
	}

	_, lines := in.logged(t, m.TaskID)
	if len(lines) < 2 || !slices.Equal(lines[:2], []string{"calling billing with [masked]", "Authorization: Bearer [masked]"}) {
		t.Errorf("the stored log holds %q", lines)
	}
	walked := 0
	if err := filepath.WalkDir(in.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		walked++
		b, err := os.ReadFile(path)
		if err == nil && bytes.Contains(b, []byte(value)) {
			t.Errorf("%s holds the secret", path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if walked == 0 {
		t.Fatal("the store holds nothing, so nothing here says the secret never reached it")
	}
}

// A chunk the API took and whose answer was lost is shipped again, answered as held, and written
// once.
func TestAChunkRetriedAfterALostAnswerIsWrittenOnce(t *testing.T) {
	in := aLogAPI(t)
	m, _ := newObjectStore(t).taskFor(t, nil, nil, nil)
	in.held(t, &m)
	var shipments atomic.Int64
	in.mu.Lock()
	in.front = func(w http.ResponseWriter, r *http.Request, api http.Handler) {
		if r.URL.Path == logsPath && shipments.Add(1) == 1 {
			// The API takes the chunk, and its answer never reaches the runner.
			api.ServeHTTP(httptest.NewRecorder(), r)
			http.Error(w, `{"error":"the upstream went away"}`, http.StatusBadGateway)
			return
		}
		api.ServeHTTP(w, r)
	}
	in.mu.Unlock()

	s := newShipment(t.Context(), in.client, m, func(s string) { t.Log(s) }, 5*time.Millisecond, 10*time.Second)
	s.retry = 5 * time.Millisecond
	t.Cleanup(s.abandon)
	w, err := TaskLogs{}.OpenLog(withShipment(t.Context(), s), agk.TaskID(m.IdempotencyKey))
	if err != nil {
		t.Fatal(err)
	}
	writes(t, w, driver.Stderr, "one", "two")
	eventually(t, "the first chunk being shipped again", func() bool { return shipments.Load() >= 2 })
	writes(t, w, driver.Stderr, "three")
	w.Close()
	log := s.finish(false)

	l, lines := in.logged(t, m.TaskID)
	if !slices.Equal(lines, []string{"one", "two", "three"}) || l.FinalSeq == 0 {
		t.Errorf("the log stands at %+v holding %q", l, lines)
	}
	if log == nil || log.Lines != 3 || log.Truncated {
		t.Errorf("the result's log is %+v", log)
	}
}

// An agent that restarted under a running container ships the rest of its log after what the
// earlier agent shipped: its first chunk is refused as one the API holds other lines for, and it
// asks where the log stands and goes on from there.
func TestARestartedAgentGoesOnWithTheLogAnEarlierOneBegan(t *testing.T) {
	in := aLogAPI(t)
	m, _ := newObjectStore(t).taskFor(t, nil, nil, nil)
	in.held(t, &m)

	ship := func() (*shipment, io.WriteCloser) {
		s := newShipment(t.Context(), in.client, m, func(s string) { t.Log(s) }, 5*time.Millisecond, 10*time.Second)
		s.retry = 5 * time.Millisecond
		t.Cleanup(s.abandon)
		w, err := TaskLogs{}.OpenLog(withShipment(t.Context(), s), agk.TaskID(m.IdempotencyKey))
		if err != nil {
			t.Fatal(err)
		}
		return s, w
	}
	earlier, w := ship()
	writes(t, w, driver.Stderr, "before the restart", "still before it")
	eventually(t, "the earlier agent's lines being kept", func() bool {
		_, lines := in.logged(t, m.TaskID)
		return len(lines) == 2
	})
	earlier.abandon()

	later, w := ship()
	writes(t, w, driver.Stderr, "after the restart")
	w.Close()
	log := later.finish(false)

	l, lines := in.logged(t, m.TaskID)
	if !slices.Equal(lines, []string{"before the restart", "still before it", "after the restart"}) || l.FinalSeq == 0 {
		t.Errorf("the log stands at %+v holding %q", l, lines)
	}
	if log == nil || log.Lines != 3 || log.Truncated {
		t.Errorf("the result's log is %+v", log)
	}
}
