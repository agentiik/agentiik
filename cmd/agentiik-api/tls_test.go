package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/internal/tlstest"
)

// Given a certificate, the API serves TLS itself, on the listener it is handed as main hands it one:
// TLS 1.3 to a client that speaks it, over HTTP/1.1, and nothing at all to a client that goes no
// higher than TLS 1.1 or speaks plain HTTP. Every other test serves without one, in plain HTTP, as
// the API did before it could serve anything else.
func TestGivenACertificateTheAPIServesTLSItself(t *testing.T) {
	database := freshDatabase(t)
	var out bytes.Buffer
	if err := migrate(t.Context(), database, &out); err != nil {
		t.Fatalf("migrating failed: %s\n%s", err, out.String())
	}
	dir := filepath.Join(t.TempDir(), "bus")
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"bus-init", dir}, empty, io.Discard, &stderr); code != exitStopped {
		t.Fatalf("bus-init exited %d: %s", code, stderr.String())
	}
	s := servingSettings(t, database.Application, dir, natsFrom(t, dir))
	pair := tlstest.NewPair(t, time.Now().Add(time.Hour))
	s.TLS = config.TLS{Certificate: string(pair.Certificate), Key: config.Secret(pair.Key)}

	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	served := make(chan error, 1)
	go func() { served <- serve(ctx, s, ln, slog.New(slog.NewTextHandler(&logged, nil))) }()
	base := "https://" + ln.Addr().String()

	get := func(c *http.Client, url string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+theToken)
		return c.Do(req)
	}

	// Offered HTTP/2 as well, which a runner's transport offers, and answered over HTTP/1.1.
	modern := pair.Client(tls.VersionTLS12, tls.VersionTLS13)
	modern.Transport.(*http.Transport).ForceAttemptHTTP2 = true
	var answer *http.Response
	deadline := time.Now().Add(30 * time.Second)
	for {
		answer, err = get(modern, base+"/api/v1/runners")
		if err == nil || time.Now().After(deadline) {
			break
		}
		select {
		case err := <-served:
			t.Fatalf("serve ended: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err != nil {
		t.Fatalf("a TLS 1.3 client was not answered: %v", err)
	}
	answer.Body.Close()
	if answer.StatusCode != http.StatusOK || answer.ProtoMajor != 1 {
		t.Errorf("a TLS 1.3 client was answered %d over %s", answer.StatusCode, answer.Proto)
	}
	if answer.TLS == nil || answer.TLS.Version != tls.VersionTLS13 {
		t.Errorf("a client speaking TLS 1.3 was not spoken TLS 1.3: %+v", answer.TLS)
	}

	// TLS 1.2 is still spoken to a client that goes no higher: it is the floor, not the ceiling.
	if answer, err := get(pair.Client(tls.VersionTLS12, tls.VersionTLS12), base+"/api/v1/runners"); err != nil {
		t.Errorf("a TLS 1.2 client was not answered: %v", err)
	} else {
		answer.Body.Close()
	}

	// Under the floor, the handshake fails, before a request reaches a route.
	if answer, err := get(pair.Client(tls.VersionTLS10, tls.VersionTLS11), base+"/api/v1/runners"); err == nil {
		answer.Body.Close()
		t.Errorf("a client going no higher than TLS 1.1 was answered %d", answer.StatusCode)
	} else if !strings.Contains(err.Error(), "protocol version") {
		t.Errorf("a client going no higher than TLS 1.1 was refused for another reason: %v", err)
	}

	// Plain HTTP reaches no route either: the server says it wants TLS, and nothing else.
	if answer, err := get(&http.Client{Timeout: 10 * time.Second}, "http://"+ln.Addr().String()+"/api/v1/runners"); err == nil {
		answer.Body.Close()
		if answer.StatusCode != http.StatusBadRequest {
			t.Errorf("a request in plain HTTP was answered %d", answer.StatusCode)
		}
	}

	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("stopped, serve answered %s", err)
		}
	case <-time.After(shutdownGrace + 10*time.Second):
		t.Fatal("serve did not return once stopped")
	}
	if !strings.Contains(logged.String(), "tls=true") {
		t.Errorf("serve did not say it serves TLS:\n%s", logged.String())
	}
}
