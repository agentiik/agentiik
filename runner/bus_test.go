package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
		{"url", "nats://nats.example.com:4222", "never reached in plaintext across a network"},
		{"url", "ws://10.0.0.7:8080", "never reached in plaintext across a network"},
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
	if _, err := OpenBus(t.Context(), c, Config{Runner: "runner-dmz-02", Pool: "dmz"}, func(string) {}); !errors.Is(err, ErrCredentialRefused) || asked.Load() != 1 {
		t.Errorf("a refused credential answered %v after %d requests, want the refusal after one", err, asked.Load())
	}

	c, asked = busAPI(t, func(n int32) (int, any) {
		return http.StatusServiceUnavailable, map[string]string{"error": "starting"}
	})
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(1500*time.Millisecond, cancel)
	b, err := OpenBus(ctx, c, Config{Runner: "runner-dmz-02", Pool: "dmz"}, func(string) {})
	if b != nil || err != nil {
		t.Errorf("a bus opened while the API never answered: %v, %v", b, err)
	}
	if asked.Load() < 2 {
		t.Errorf("an API that did not answer was asked %d times", asked.Load())
	}
}
