package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/internal/config"
)

// health, as a Compose health check runs it: with the API's own environment, on a listener that
// is open and not serving yet, then serving, in plain HTTP behind a proxy and over TLS without one.

// lookupOf is an environment holding values and nothing else.
func lookupOf(values map[string]string) config.Lookup {
	return func(name string) (string, bool) {
		v, ok := values[name]
		return v, ok
	}
}

// checked runs health in env, bounded well under its own timeout so that a failing check fails fast.
func checked(t *testing.T, env map[string]string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"health"}, lookupOf(env), &stdout, &stderr)
	return code, stderr.String()
}

// A listener the API has opened and serves nothing on yet is what the API is while it reaches its
// database and its bus: the connection is taken into the backlog and nothing answers. health says
// not ready then, and ready once it serves, in plain HTTP behind a proxy.
func TestHealthIsReadyOnceTheAPIServesAndNotBefore(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	// Behind a proxy, as the Compose file sets it, on the port alone, which the API listens on
	// at the loopback.
	env := map[string]string{config.ProxyURL: "https://agentiik.example.com", config.Listen: ":" + port}

	if code, said := checked(t, env); code != exitFailed || !strings.Contains(said, "did not answer") {
		t.Fatalf("health on a listener that serves nothing yet exited %d:\n%s", code, said)
	}

	server := &http.Server{Handler: http.NotFoundHandler()}
	go server.Serve(ln)
	defer server.Close()
	if code, said := checked(t, env); code != exitStopped {
		t.Fatalf("health on an API answering 404 exited %d:\n%s", code, said)
	}
}

// Over TLS where the API holds a certificate and no proxy is named, on a certificate that names
// none of the addresses health dials, which it does not verify.
func TestHealthSpeaksTLSWhereTheAPIDoes(t *testing.T) {
	// Plain HTTP to a TLS listener is answered with a 400 by net/http before any handler, which
	// would pass for ready, so the handler says whether it was reached.
	reached := make(chan bool, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- r.TLS != nil
		http.NotFound(w, r)
	}))
	server.TLS = &tls.Config{}
	server.StartTLS()
	defer server.Close()
	env := map[string]string{config.Listen: server.Listener.Addr().String(), config.TLSCertFile: "/agentiik/tls/server.pem"}
	if code, said := checked(t, env); code != exitStopped {
		t.Fatalf("health on an API serving TLS exited %d:\n%s", code, said)
	}
	select {
	case overTLS := <-reached:
		if !overTLS {
			t.Error("health asked in plain HTTP")
		}
	default:
		t.Error("health's request never reached the API, whose TLS listener answered it on its own")
	}

	// A TLS listener that has not started its handshakes is not ready.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	env[config.Listen] = ln.Addr().String()
	if code, said := checked(t, env); code != exitFailed {
		t.Fatalf("health on a TLS listener that serves nothing yet exited %d:\n%s", code, said)
	}
}

// Nothing listening is not ready, and a configuration the API would refuse is refused.
func TestHealthFailsWhereNothingListens(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	ln.Close()
	if code, said := checked(t, map[string]string{config.Listen: address}); code != exitFailed || !strings.Contains(said, address) {
		t.Errorf("health with nothing listening exited %d:\n%s", code, said)
	}
	if code, said := checked(t, map[string]string{config.Listen: "8443"}); code != exitFailed || !strings.Contains(said, config.Listen) {
		t.Errorf("health on a listen address that is not one exited %d:\n%s", code, said)
	}
}

// The loopback where the API listens on every interface, and the address it names otherwise.
func TestHealthDialsWhereTheAPIListens(t *testing.T) {
	for listen, want := range map[string]string{
		":8443":          "127.0.0.1:8443",
		"0.0.0.0:8443":   "127.0.0.1:8443",
		"[::]:8443":      "[::1]:8443",
		"10.0.0.5:8443":  "10.0.0.5:8443",
		"127.0.0.1:8080": "127.0.0.1:8080",
		"localhost:8080": "localhost:8080",
	} {
		if got := dialable(listen); got != want {
			t.Errorf("listening on %s, health dials %s, want %s", listen, got, want)
		}
	}
}
