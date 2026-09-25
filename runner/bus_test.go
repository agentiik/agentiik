package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/bus"
)

// busAPI answers POST /api/v1/bus/token with what answer says, and counts what it was asked.
func busAPI(t *testing.T, answer func(n int32) (int, any)) (*Client, *atomic.Int32) {
	t.Helper()
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != busTokenPath {
			http.NotFound(w, r)
			return
		}
		status, body := answer(asked.Add(1))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c, &asked
}

// aBusToken is the API's answer for a runner of dmz, as the route writes it.
func aBusToken() map[string]any {
	return map[string]any{
		"kind": bus.Kind, "url": "nats://127.0.0.1:1", "jwt": "eyJ0eXAiOiJKV1QifQ.e30.c2ln", "seed": "SUAB",
		"consumer": bus.Durable("dmz"), "stream": bus.Stream,
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	}
}

// The credential is taken only for the pool this runner joined, of the kind it speaks and the
// stream it takes from, and only while it is good: anything else is an installation to look at,
// and refused naming what is wrong.
func TestABusCredentialIsTakenOnlyForThePoolThisRunnerJoined(t *testing.T) {
	c, _ := busAPI(t, func(int32) (int, any) { return http.StatusOK, aBusToken() })
	got, err := c.BusCredentials(t.Context(), "dmz")
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "nats://127.0.0.1:1" || got.Seed != "SUAB" || got.ExpiresAt.Before(time.Now()) {
		t.Errorf("the credential reads %+v", got)
	}

	for _, row := range []struct {
		field string
		value any
		says  string
	}{
		{"consumer", bus.Durable("lan"), "this runner joined pool dmz"},
		{"kind", "sqs-role", "this runner speaks " + bus.Kind},
		{"stream", "AGENTIIK_OTHER", "a runner takes work from " + bus.Stream},
		{"seed", "", "nothing to present"},
		{"expires_at", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), "before it arrived"},
	} {
		t.Run(row.field, func(t *testing.T) {
			answer := aBusToken()
			answer[row.field] = row.value
			c, _ := busAPI(t, func(int32) (int, any) { return http.StatusOK, answer })
			_, err := c.BusCredentials(t.Context(), "dmz")
			if err == nil || !strings.Contains(err.Error(), row.says) {
				t.Errorf("a credential with %s %v was answered %v", row.field, row.value, err)
			}
		})
	}
}

// An API that does not answer yet is asked again, as a host booting beside the control plane
// needs; one that refuses the runner's credential is not, since it answers the same every time.
func TestOpeningTheBusAsksAgainOnlyWhileTheAPIDoesNotAnswer(t *testing.T) {
	c, asked := busAPI(t, func(n int32) (int, any) {
		return http.StatusUnauthorized, map[string]string{"error": "that credential opens nothing"}
	})
	if _, _, err := OpenBus(t.Context(), c, Config{Runner: "runner-dmz-02", Pool: "dmz"}, func(string) {}); !errors.Is(err, ErrCredentialRefused) || asked.Load() != 1 {
		t.Errorf("a refused credential answered %v after %d requests, want the refusal after one", err, asked.Load())
	}

	c, asked = busAPI(t, func(n int32) (int, any) {
		return http.StatusServiceUnavailable, map[string]string{"error": "starting"}
	})
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(1500*time.Millisecond, cancel)
	b, _, err := OpenBus(ctx, c, Config{Runner: "runner-dmz-02", Pool: "dmz"}, func(string) {})
	if b != nil || err != nil {
		t.Errorf("a bus opened while the API never answered: %v, %v", b, err)
	}
	if asked.Load() < 2 {
		t.Errorf("an API that did not answer was asked %d times", asked.Load())
	}
}

// watched is a connection whose takes and closing a test reads.
type watched struct {
	busConn
	takes, reports atomic.Int32
	closed         atomic.Bool
}

func (w *watched) Take(ctx context.Context, pool string, batch int, wait time.Duration) ([]bus.Taken, error) {
	w.takes.Add(1)
	if w.busConn == nil {
		return nil, nil
	}
	return w.busConn.Take(ctx, pool, batch, wait)
}

func (w *watched) Report(ctx context.Context, r bus.TaskResult) error {
	w.reports.Add(1)
	return nil
}

func (w *watched) Close() {
	w.closed.Store(true)
	if w.busConn != nil {
		w.busConn.Close()
	}
}

// A connection replaced is kept for as long as a take made on it could still hand a message over
// and the message's acknowledgement matter, and no longer than its credential lasts. Everything
// after the replacement goes to the new one.
func TestAReplacedConnectionIsClosedOnceNothingItHandedOverCanNeedIt(t *testing.T) {
	for _, row := range []struct {
		name          string
		expires, want time.Duration
	}{
		{"its takes", time.Hour, 500 * time.Millisecond},
		{"its credential", 150 * time.Millisecond, 150 * time.Millisecond},
	} {
		t.Run(row.name, func(t *testing.T) {
			old, next := &watched{}, &watched{}
			start := time.Now()
			tb := newTaskBus(old, start.Add(row.expires), nil, func(s string) { t.Log(s) })
			tb.linger = 200 * time.Millisecond
			defer tb.Close()
			if _, err := tb.Take(t.Context(), "dmz", 1, 300*time.Millisecond); err != nil {
				t.Fatal(err)
			}
			tb.replace(next, start.Add(time.Hour))
			if err := tb.Report(t.Context(), bus.TaskResult{}); err != nil || old.reports.Load() != 0 || next.reports.Load() != 1 {
				t.Fatalf("after the replacement, a report answered %v and went %d times to the old connection and %d to the new", err, old.reports.Load(), next.reports.Load())
			}
			tb.Take(t.Context(), "dmz", 1, time.Hour)
			if old.takes.Load() != 1 || next.takes.Load() != 1 {
				t.Errorf("takes went %d to the old connection and %d to the new, want the second to the new", old.takes.Load(), next.takes.Load())
			}
			time.Sleep(row.want - 100*time.Millisecond - time.Since(start))
			if old.closed.Load() {
				t.Errorf("the old connection was closed before %s", row.want)
			}
			eventually(t, "the old connection closed", old.closed.Load)
			if since := time.Since(start); since > row.want+300*time.Millisecond {
				t.Errorf("the old connection was closed after %s, want %s", since, row.want)
			}
			if next.closed.Load() {
				t.Error("the new connection was closed")
			}
			tb.Close()
			if !next.closed.Load() {
				t.Error("closing the bus left the current connection open")
			}
		})
	}
}

// Keep renews at three quarters of a credential's life, keeps the connection it has while a
// renewal fails, and ends saying to join again where the runner credential opens nothing.
func TestTheBusIsRenewedAtThreeQuartersOfItsCredentialsLife(t *testing.T) {
	var dials atomic.Int32
	var at []time.Duration
	var mu sync.Mutex
	start := time.Now()
	fail := errors.New("the bus is down")
	tb := newTaskBus(&watched{}, start.Add(2*time.Second), func(ctx context.Context) (busConn, time.Time, error) {
		n := dials.Add(1)
		mu.Lock()
		at = append(at, time.Since(start))
		mu.Unlock()
		switch n {
		case 1:
			return nil, time.Time{}, fail
		case 2:
			return &watched{}, time.Now().Add(time.Hour), nil
		}
		return nil, time.Time{}, nil
	}, func(s string) { t.Log(s) })
	defer tb.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- tb.Keep(ctx) }()
	eventually(t, "the renewal after the failed one", func() bool { return dials.Load() == 2 })
	cancel()
	if err := <-done; err != nil {
		t.Errorf("keep ended with %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if at[0] < 1400*time.Millisecond || at[0] > 1800*time.Millisecond {
		t.Errorf("a two-second credential was renewed after %s, want a second and a half", at[0])
	}
	if gap := at[1] - at[0]; gap < 900*time.Millisecond || gap > 1500*time.Millisecond {
		t.Errorf("a failed renewal was asked again after %s, want a second", gap)
	}

	refused := newTaskBus(&watched{}, time.Now(), func(context.Context) (busConn, time.Time, error) {
		return nil, time.Time{}, fmt.Errorf("runner: POST %s: %w", busTokenPath, ErrCredentialRefused)
	}, func(s string) { t.Log(s) })
	defer refused.Close()
	if err := refused.Keep(t.Context()); !errors.Is(err, ErrCredentialRefused) || !strings.Contains(err.Error(), "join") {
		t.Errorf("a refused runner credential ended keep with %v", err)
	}
}

// Real NATS: a take made on a connection, answered after the connection was replaced, is
// acknowledged on the connection it was taken on, which is still open for it, so the queue ends
// holding nothing: nothing handed out and unacknowledged, and nothing to come round again.
func TestRenewingTheBusInTheMiddleOfATakeLosesNoAcknowledgement(t *testing.T) {
	in := anInstallationServing(t)
	client, err := NewClient(in.url, in.credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Runner: in.runner, Pool: in.poolName}
	var dialed []*watched
	var mu sync.Mutex
	dial := func(ctx context.Context) (busConn, time.Time, error) {
		b, expires, err := dialBus(ctx, client, cfg)
		if err != nil {
			return nil, time.Time{}, err
		}
		w := &watched{busConn: b}
		mu.Lock()
		dialed = append(dialed, w)
		mu.Unlock()
		return w, expires, nil
	}
	ctx := t.Context()
	conn, expires, err := dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tb := newTaskBus(conn, expires, dial, func(s string) { t.Log(s) })
	tb.linger = 500 * time.Millisecond
	defer tb.Close()

	type took struct {
		taken []bus.Taken
		err   error
	}
	got := make(chan took, 1)
	go func() {
		taken, err := tb.Take(ctx, in.poolName, 1, 3*time.Second)
		got <- took{taken, err}
	}()
	first := dialed[0]
	eventually(t, "the take on the first connection", func() bool { return first.takes.Load() == 1 })
	time.Sleep(200 * time.Millisecond)
	next, expires, err := dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tb.replace(next, expires)

	m := in.dispatch(t, "invoice")
	r := <-got
	if r.err != nil || len(r.taken) != 1 || r.taken[0].Task.IdempotencyKey != m.IdempotencyKey {
		t.Fatalf("the take in flight answered %d tasks, %v", len(r.taken), r.err)
	}
	if err := r.taken[0].Held(ctx); err != nil {
		t.Fatalf("the task taken before the replacement could not be acknowledged: %s", err)
	}
	if waiting, unacknowledged := in.queue(t); waiting+unacknowledged != 0 {
		t.Errorf("the queue holds %d waiting and %d unacknowledged", waiting, unacknowledged)
	}

	// The next take is the new connection's, and so is its acknowledgement.
	second := in.dispatch(t, "render")
	taken, err := tb.Take(ctx, in.poolName, 1, 3*time.Second)
	if err != nil || len(taken) != 1 || taken[0].Task.IdempotencyKey != second.IdempotencyKey {
		t.Fatalf("the take after the replacement answered %d tasks, %v", len(taken), err)
	}
	if n := dialed[1].takes.Load(); n != 1 {
		t.Errorf("the new connection was asked to take %d times", n)
	}
	if err := taken[0].Held(ctx); err != nil {
		t.Fatal(err)
	}
	if waiting, unacknowledged := in.queue(t); waiting+unacknowledged != 0 {
		t.Errorf("the queue holds %d waiting and %d unacknowledged", waiting, unacknowledged)
	}
	eventually(t, "the first connection closed once its take could hand nothing more over", first.closed.Load)
	if dialed[1].closed.Load() {
		t.Error("the new connection was closed")
	}
}
