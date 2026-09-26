package main

import (
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/internal/tlstest"
)

// Given a certificate, the metrics are served over TLS: TLS 1.3 to a scraper that speaks it, and
// nothing to one that goes no higher than TLS 1.1 or speaks plain HTTP. Without one they are
// answered in plain HTTP, as before.
func TestGivenACertificateTheMetricsAreServedOverTLS(t *testing.T) {
	pair := tlstest.NewPair(t, time.Now().Add(time.Hour))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newCounted(nil, log)
	addr := freeAddress(t)
	stop, err := c.serveMetrics(t.Context(), config.Metrics{
		Listen: addr, TokenHash: scrapeHash,
		TLS: config.TLS{Certificate: string(pair.Certificate), Key: config.Secret(pair.Key)},
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// Unauthenticated, so that the answer is the handler's and reads nothing: a 401 is the
	// metrics' listener answering.
	answer, err := pair.Client(tls.VersionTLS12, tls.VersionTLS13).Get("https://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("a TLS 1.3 scraper was not answered: %v", err)
	}
	answer.Body.Close()
	if answer.StatusCode != http.StatusUnauthorized || answer.TLS == nil || answer.TLS.Version != tls.VersionTLS13 {
		t.Errorf("a TLS 1.3 scraper was answered %d over %+v", answer.StatusCode, answer.TLS)
	}

	if answer, err := pair.Client(tls.VersionTLS10, tls.VersionTLS11).Get("https://" + addr + "/metrics"); err == nil {
		answer.Body.Close()
		t.Errorf("a scraper going no higher than TLS 1.1 was answered %d", answer.StatusCode)
	} else if !strings.Contains(err.Error(), "protocol version") {
		t.Errorf("a scraper going no higher than TLS 1.1 was refused for another reason: %v", err)
	}

	if code, body := scrape(t, addr, scrapeToken); code == http.StatusOK {
		t.Errorf("a scrape in plain HTTP was answered %d: %s", code, body)
	}
}

// Without a certificate, the metrics are answered in plain HTTP and a TLS scraper is refused.
func TestWithoutACertificateTheMetricsAreAnsweredInPlainHTTP(t *testing.T) {
	pair := tlstest.NewPair(t, time.Now().Add(time.Hour))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newCounted(nil, log)
	addr := freeAddress(t)
	stop, err := c.serveMetrics(t.Context(), config.Metrics{Listen: addr, TokenHash: scrapeHash}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if code, _ := scrape(t, addr, ""); code != http.StatusUnauthorized {
		t.Errorf("a scrape in plain HTTP was answered %d", code)
	}
	if answer, err := pair.Client(tls.VersionTLS12, tls.VersionTLS13).Get("https://" + addr + "/metrics"); err == nil {
		answer.Body.Close()
		t.Errorf("a TLS scraper was answered %d by a listener given no certificate", answer.StatusCode)
	}
}
