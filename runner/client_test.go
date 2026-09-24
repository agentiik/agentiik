package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// api is an API that answers every request with one handler, behind the client of one runner.
func api(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL+"/", credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type answer struct {
	Interval int `json:"interval"`
}

func TestEveryCallCarriesTheCredentialAndJSONBothWays(t *testing.T) {
	var said struct {
		method, path, auth, contentType string
		body                            map[string]any
	}
	c := api(t, func(w http.ResponseWriter, r *http.Request) {
		said.method, said.path = r.Method, r.URL.Path
		said.auth, said.contentType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&said.body)
		w.Write([]byte(`{"interval": 30}`))
	})
	var out answer
	if err := c.Do(context.Background(), http.MethodPost, "/api/v1/runners/heartbeat", map[string]any{"held": []string{}}, &out); err != nil {
		t.Fatal(err)
	}
	if said.method != http.MethodPost || said.path != "/api/v1/runners/heartbeat" {
		t.Errorf("the API was asked %s %s", said.method, said.path)
	}
	if said.auth != "Bearer "+credential {
		t.Errorf("the call carried Authorization %q", said.auth)
	}
	if said.contentType != "application/json" || said.body == nil {
		t.Errorf("the body went as %q: %v", said.contentType, said.body)
	}
	if out.Interval != 30 {
		t.Errorf("the answer read as %+v", out)
	}
}

func TestAnAnswerIsReadStrictly(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"a field this runner does not know", `{"interval": 30, "drain": true}`},
		{"something after the value", `{"interval": 30} {}`},
		{"not JSON", `<html>a proxy's page</html>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			client := api(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, c.body) })
			var out answer
			if err := client.Do(context.Background(), http.MethodGet, "/api/v1/x", nil, &out); err == nil {
				t.Errorf("%s was read as %+v", c.body, out)
			}
		})
	}
}

func TestEachRefusalIsClassedByWhatTheRunnerDoesNext(t *testing.T) {
	for _, c := range []struct {
		status int
		class  error
	}{
		{http.StatusUnauthorized, ErrCredentialRefused},
		{http.StatusForbidden, ErrForbidden},
		{http.StatusConflict, ErrConflict},
		{http.StatusUnprocessableEntity, ErrUnprocessable},
		{http.StatusTooManyRequests, ErrUnavailable},
		{http.StatusInternalServerError, ErrUnavailable},
		{http.StatusServiceUnavailable, ErrUnavailable},
	} {
		client := api(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			io.WriteString(w, `{"error": "the API's sentence\nforged: a second line"}`)
		})
		err := client.Do(context.Background(), http.MethodGet, "/api/v1/x", nil, nil)
		var e *APIError
		switch {
		case !errors.Is(err, c.class):
			t.Errorf("%d answered %v, want %v", c.status, err, c.class)
		case !errors.As(err, &e) || e.Status != c.status:
			t.Errorf("%d answered %#v", c.status, err)
		case strings.Contains(err.Error(), "\n") || !strings.Contains(err.Error(), "the API's sentence"):
			t.Errorf("%d is reported as %q, which is not the API's sentence on one line", c.status, err)
		}
	}
}

func TestNoAnswerAtAllIsAskAgain(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	c, err := NewClient(srv.URL, credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Do(context.Background(), http.MethodGet, "/api/v1/x", nil, nil); !errors.Is(err, ErrUnavailable) {
		t.Errorf("an API that is not there answered %v", err)
	}
}

func TestTheCredentialIsNeverSentWhereARedirectPoints(t *testing.T) {
	var reached atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
	}))
	defer elsewhere.Close()
	client := api(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusTemporaryRedirect)
	})
	err := client.Do(context.Background(), http.MethodGet, "/api/v1/x", nil, nil)
	var e *APIError
	if !errors.As(err, &e) || e.Status != http.StatusTemporaryRedirect {
		t.Errorf("a redirect answered %v", err)
	}
	if reached.Load() != 0 {
		t.Errorf("the redirect was followed")
	}
}

func TestACancelledCallIsTheCancellationAndNotAnOutage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := api(t, func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	})
	if err := client.Do(ctx, http.MethodGet, "/api/v1/x", nil, nil); !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnavailable) {
		t.Errorf("a cancelled call answered %v", err)
	}
}

func TestAClientNeedsAnAddressACredentialAndAPath(t *testing.T) {
	if _, err := NewClient("", credential, nil); err == nil {
		t.Error("a client with no address was built")
	}
	if _, err := NewClient("https://agentiik.example.com", "", nil); err == nil {
		t.Error("a client with no credential was built")
	}
	client := api(t, func(http.ResponseWriter, *http.Request) { t.Error("a request went out") })
	if err := client.Do(context.Background(), http.MethodGet, "api/v1/x", nil, nil); err == nil {
		t.Error("a path not below the API was asked")
	}
}

func TestTheVersionIsTheReleaseWithoutItsV(t *testing.T) {
	for recorded, want := range map[string]string{
		"v0.2.0":                              "0.2.0",
		"v0.2.1-0.20260924101500-8fea230abcd": "0.2.1-0.20260924101500-8fea230abcd",
		"(devel)":                             devel,
		"":                                    devel,
		"v":                                   devel,
	} {
		if got := versionOf(recorded); got != want {
			t.Errorf("%q is version %q, want %q", recorded, got, want)
		}
	}
}
