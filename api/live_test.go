package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/coder/websocket"
)

// liveServer is the API over someRuns, its live connections gathering for pace and asking again
// every reauthorise, with what identify says a request carries.
type liveServer struct {
	runs someRuns
	url  string
}

func servingLive(t *testing.T, auth api.Authorizer, identify func(*http.Request) (api.Identity, error), pace, ping, reauthorise time.Duration, stopping <-chan struct{}) liveServer {
	t.Helper()
	s := withSomeRuns(t)
	rt, err := api.NewRouter(auth, identify)
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.NewServer(rt, api.ServerOptions{Pool: s.pool, Versions: s.store, Objects: s.objects, URLs: s.signed, PublicURL: "https://agentiik.example.com", Stopping: stopping})
	if err != nil {
		t.Fatal(err)
	}
	api.LiveTiming(server, pace, ping, reauthorise)
	ts := httptest.NewServer(rt)
	t.Cleanup(ts.Close)
	return liveServer{runs: s, url: ts.URL}
}

// liveReader is one live connection, and every message it was sent.
type liveReader struct {
	conn *websocket.Conn

	mu       sync.Mutex
	messages []map[string]string
	closed   error
	arrived  chan struct{}
}

// dial opens the live connection with the headers given, and reads it until it ends.
func (l liveServer) dial(t *testing.T, header http.Header) *liveReader {
	t.Helper()
	conn, answer, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(l.url, "http")+"/api/v1/me/live", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		status := 0
		if answer != nil {
			status = answer.StatusCode
		}
		t.Fatalf("the live connection was answered %d: %v", status, err)
	}
	r := &liveReader{conn: conn, arrived: make(chan struct{}, 1)}
	t.Cleanup(func() { conn.CloseNow() })
	go func() {
		for {
			_, body, err := conn.Read(context.Background())
			r.mu.Lock()
			if err != nil {
				r.closed = err
				r.mu.Unlock()
				r.nudge()
				return
			}
			var m map[string]string
			json.Unmarshal(body, &m)
			r.messages = append(r.messages, m)
			r.mu.Unlock()
			r.nudge()
		}
	}()
	return r
}

func (r *liveReader) nudge() {
	select {
	case r.arrived <- struct{}{}:
	default:
	}
}

// awaits waits until done says the messages so far are enough, or the connection ends, failing
// after a while.
func (r *liveReader) awaits(t *testing.T, what string, done func(messages []map[string]string, closed error) bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		r.mu.Lock()
		ok := done(r.messages, r.closed)
		r.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-r.arrived:
		case <-deadline:
			r.mu.Lock()
			defer r.mu.Unlock()
			t.Fatalf("%s never came: %v, closed %v", what, r.messages, r.closed)
		}
	}
}

// told is the messages other than all, which a connection is sent whenever the listener starts.
func (r *liveReader) told() []map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]string
	for _, m := range r.messages {
		if m["kind"] != "all" {
			out = append(out, m)
		}
	}
	return out
}

func asBearer(who string) http.Header { return http.Header{"Authorization": {"Bearer " + who}} }

// "A WebSocket telling its caller, as it happens, what changed among what it may read ... a run
// created, or moved on, or one of whose steps did, sent where the caller holds run:read on that
// workflow; notifications for a notification told to the caller or dismissed; runners, to an
// administrator, for a runner or a pool changed; activity, to an administrator, for a run created,
// or moved on, or one of whose steps did, in any namespace, naming none ... What changed within a
// quarter of a second is sent once."
func TestTheLiveConnectionTellsEachCallerWhatChangedAmongWhatItReads(t *testing.T) {
	auth := granted{
		"alice": {{api.RunRead, api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}}},
		"bob":   {{api.RunRead, api.Target{Namespace: "team-ops"}}},
		"dana":  {{api.GrantManage, api.Target{}}},
	}
	l := servingLive(t, auth, bearer, 300*time.Millisecond, time.Hour, time.Hour, nil)

	// The first connection starts the listener, which says everything changed once it listens:
	// what is changed after that is heard.
	alice := l.dial(t, asBearer("alice"))
	alice.awaits(t, "the listener's first all", func(m []map[string]string, _ error) bool { return len(m) > 0 && m[0]["kind"] == "all" })
	bob := l.dial(t, asBearer("bob"))
	dana := l.dial(t, asBearer("dana"))

	s := l.runs
	s.sql(t, `insert into principals (id, kind) values ('alice', 'user') on conflict do nothing`)
	// Twice within the quarter of a second, said once.
	s.sql(t, `update runs set state = 'running' where id = $1`, s.finance[0])
	s.sql(t, `update runs set state = 'succeeded' where id = $1`, s.finance[0])
	s.sql(t, `update runs set state = 'running' where id = $1`, s.teamOps)
	s.sql(t, `update runs set state = 'running' where id = $1`, s.payroll)
	// A step and a task of the second run of finance/monthly-invoicing, said as the run.
	s.sql(t, `update steps set state = 'running' where run_id = $1`, s.finance[1])
	s.sql(t, `insert into notifications (id, recipient, kind, at, credential) values ('01JMZ8W4K2R7AAAAAAAAAAAAAA', 'alice', 'passkey_counter_refused', now(), 'cred')`)
	s.sql(t, `update runner_pools set created_by = created_by where name = 'default'`)

	financeRun := map[string]string{"kind": "run", "namespace": "finance", "workflow": "monthly-invoicing", "run": s.finance[0]}
	financeStep := map[string]string{"kind": "run", "namespace": "finance", "workflow": "monthly-invoicing", "run": s.finance[1]}
	alice.awaits(t, "alice's runs and notifications", func(m []map[string]string, _ error) bool {
		return len(alice.toldOf(m)) >= 3
	})
	bob.awaits(t, "bob's run", func(m []map[string]string, _ error) bool { return len(bob.toldOf(m)) >= 1 })
	dana.awaits(t, "dana's runners and activity", func(m []map[string]string, _ error) bool { return len(distinct(dana.toldOf(m))) >= 2 })
	// Whatever else would have come has had time to.
	time.Sleep(time.Second)

	if got := alice.told(); !sameMessages(got, []map[string]string{financeRun, financeStep, {"kind": "notifications"}}) {
		t.Errorf("alice, reading finance/monthly-invoicing, was told %v", got)
	}
	if got := bob.told(); !sameMessages(got, []map[string]string{{"kind": "run", "namespace": "team-ops", "workflow": "monthly-invoicing", "run": s.teamOps}}) {
		t.Errorf("bob, reading team-ops, was told %v", got)
	}
	// The runs changed over more than one quarter of a second may be told as activity more than
	// once; what matters is that dana is told it, naming nothing, and never a run.
	if got := distinct(dana.told()); !sameMessages(got, []map[string]string{{"kind": "runners"}, {"kind": "activity"}}) {
		t.Errorf("dana, an administrator reading no run, was told %v", got)
	}
}

// distinct is messages each once.
func distinct(messages []map[string]string) []map[string]string {
	seen := map[string]bool{}
	var out []map[string]string
	for _, m := range messages {
		j, _ := json.Marshal(m)
		if !seen[string(j)] {
			seen[string(j)] = true
			out = append(out, m)
		}
	}
	return out
}

// toldOf is told over messages already held under the reader's lock.
func (r *liveReader) toldOf(messages []map[string]string) []map[string]string {
	var out []map[string]string
	for _, m := range messages {
		if m["kind"] != "all" {
			out = append(out, m)
		}
	}
	return out
}

// sameMessages compares two sets of messages in any order.
func sameMessages(a, b []map[string]string) bool {
	key := func(m map[string]string) string {
		j, _ := json.Marshal(m)
		return string(j)
	}
	ka, kb := make([]string, 0, len(a)), make([]string, 0, len(b))
	for _, m := range a {
		ka = append(ka, key(m))
	}
	for _, m := range b {
		kb = append(kb, key(m))
	}
	slices.Sort(ka)
	slices.Sort(kb)
	return slices.Equal(ka, kb)
}

// sessionOrBearer identifies a bearer token as its principal, as bearer does, and a session cookie
// as alice.
func sessionOrBearer(revoked *sync.Map) func(*http.Request) (api.Identity, error) {
	return func(r *http.Request) (api.Identity, error) {
		if c, err := r.Cookie(api.SessionCookie); err == nil {
			if _, gone := revoked.Load(c.Value); gone {
				return api.Identity{}, nil
			}
			return api.Identity{Principal: "alice"}, nil
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if _, gone := revoked.Load(token); gone {
			return api.Identity{}, nil
		}
		return api.Identity{Principal: api.Principal(token)}, nil
	}
}

// "426 for a request that asks for no WebSocket, and 400 for a handshake RFC 6455 refuses ... a
// session's handshake carries the installation's Origin, 403 otherwise."
func TestTheLiveConnectionRefusesWhatIsNoHandshakeAndASessionFromAnotherOrigin(t *testing.T) {
	var revoked sync.Map
	l := servingLive(t, granted{}, sessionOrBearer(&revoked), 50*time.Millisecond, time.Hour, time.Hour, nil)
	ask := func(header http.Header) (*http.Response, map[string]string) {
		t.Helper()
		r, _ := http.NewRequestWithContext(t.Context(), "GET", l.url+"/api/v1/me/live", nil)
		for k, v := range header {
			r.Header[k] = v
		}
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var body map[string]string
		json.NewDecoder(res.Body).Decode(&body)
		return res, body
	}
	handshake := func(extra http.Header) http.Header {
		h := http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-Websocket-Version": {"13"}, "Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="}}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}
	session := &http.Cookie{Name: api.SessionCookie, Value: "s"}

	if res, body := ask(asBearer("alice")); res.StatusCode != http.StatusUpgradeRequired || res.Header.Get("Upgrade") != "websocket" || body["error"] == "" {
		t.Errorf("a GET asking for no WebSocket was answered %d, Upgrade %q: %v", res.StatusCode, res.Header.Get("Upgrade"), body)
	}
	for name, h := range map[string]http.Header{
		"another version": handshake(http.Header{"Authorization": {"Bearer alice"}, "Sec-Websocket-Version": {"8"}}),
		"a key too short": handshake(http.Header{"Authorization": {"Bearer alice"}, "Sec-Websocket-Key": {"c2hvcnQ="}}),
		"two keys":        handshake(http.Header{"Authorization": {"Bearer alice"}, "Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ==", "dGhlIHNhbXBsZSBub25jZQ=="}}),
	} {
		if res, body := ask(h); res.StatusCode != http.StatusBadRequest || body["error"] == "" {
			t.Errorf("%s was answered %d: %v", name, res.StatusCode, body)
		}
	}
	for name, origin := range map[string][]string{"no Origin": nil, "another site's": {"https://evil.example.com"}, "two": {"https://agentiik.example.com", "https://agentiik.example.com"}} {
		h := handshake(http.Header{"Cookie": {session.String()}})
		if origin != nil {
			h["Origin"] = origin
		}
		if res, body := ask(h); res.StatusCode != http.StatusForbidden || !strings.Contains(body["error"], "Origin") {
			t.Errorf("a session with %s Origin was answered %d: %v", name, res.StatusCode, body)
		}
	}
	if res, _ := ask(handshake(nil)); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a handshake with no credential was answered %d", res.StatusCode)
	}

	// From the installation's own origin, a session's handshake is upgraded.
	from := l.dial(t, http.Header{"Cookie": {session.String()}, "Origin": {"https://agentiik.example.com"}})
	from.conn.Close(websocket.StatusNormalClosure, "")
}

// "A credential that no longer identifies its caller closes the connection with 1008, as the API
// stopping closes it with 1001 ... The client sends nothing but the protocol's own frames, a
// message from it closing the connection with 1008."
func TestTheLiveConnectionEndsWithItsCredentialWithTheAPIAndOnAMessageFromTheClient(t *testing.T) {
	var revoked sync.Map
	stopping := make(chan struct{})
	l := servingLive(t, granted{}, sessionOrBearer(&revoked), 50*time.Millisecond, 50*time.Millisecond, 100*time.Millisecond, stopping)
	ended := func(r *liveReader, want websocket.StatusCode) func([]map[string]string, error) bool {
		return func(_ []map[string]string, closed error) bool {
			if closed == nil {
				return false
			}
			if got := websocket.CloseStatus(closed); got != want {
				t.Errorf("the connection ended with %d, not %d: %v", got, want, closed)
			}
			return true
		}
	}

	gone := l.dial(t, asBearer("erin"))
	revoked.Store("erin", true)
	gone.awaits(t, "the end of a revoked token's connection", ended(gone, websocket.StatusPolicyViolation))

	talking := l.dial(t, asBearer("frank"))
	if err := talking.conn.Write(t.Context(), websocket.MessageText, []byte(`{"kind":"subscribe"}`)); err != nil && !errors.Is(err, context.Canceled) {
		t.Logf("the message was not written whole: %v", err)
	}
	talking.awaits(t, "the end of a connection the client sent a message on", ended(talking, websocket.StatusPolicyViolation))

	// Pinged every 50 ms and answered, a connection whose credential holds stays open.
	staying := l.dial(t, asBearer("grace"))
	time.Sleep(400 * time.Millisecond)
	staying.mu.Lock()
	if staying.closed != nil {
		t.Errorf("a connection pinged and answered ended: %v", staying.closed)
	}
	staying.mu.Unlock()
	close(stopping)
	staying.awaits(t, "the end of a connection the API stopped", ended(staying, websocket.StatusGoingAway))
}
