package main

import (
	"crypto/tls"
	"net/http"
	"testing"
	"time"
)

// Every request to an installation, a stream's included, goes over TLS 1.2 at least.
func TestEveryRequestToAnInstallationHoldsTheTLSFloor(t *testing.T) {
	for _, timeout := range []time.Duration{0, answerTimeout} {
		transport, ok := client(timeout).Transport.(*http.Transport)
		if !ok || transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
			t.Errorf("a client with a timeout of %s does not hold the floor", timeout)
		}
	}
}
