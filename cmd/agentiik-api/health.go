package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/internal/tlsfloor"
)

// healthTimeout bounds what health waits for an answer: long enough for an API answering a busy
// host, and shorter than the thirty seconds Docker gives a health check by default, so that the
// check says why it failed rather than being cut off.
const healthTimeout = 5 * time.Second

// healthVerb is agentiik-api health: 0 where the API serving beside it answers, 1 where it does
// not, for a health check run in the API's own container, whose image has no shell, no curl and
// no wget to run one with.
func healthVerb(ctx context.Context, lookup config.Lookup, _, stderr io.Writer) int {
	h, err := config.ReadAPIHealth(lookup)
	if err != nil {
		fmt.Fprintf(stderr, "%s health: the configuration refuses the start:\n%s\n", program, err)
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	if err := answers(ctx, h); err != nil {
		fmt.Fprintf(stderr, "%s health: %s\n", program, err)
		return exitFailed
	}
	return exitStopped
}

// answers asks the API listening where h says for its root, and says why where nothing answered.
//
// Any answer is ready, the 404 of a path no route serves included, because the API answers nothing
// before it is: it listens from the start, and serves only once the database, the bus, every pool's
// queue and every route are open, so until then a request waits, and fails at the timeout. A
// request with no credential to a path no route serves touches neither the database nor the bus,
// so a check every few seconds costs the installation nothing.
//
// The certificate is not verified. The request carries nothing and its answer is not read, so a
// certificate that did not verify would put nothing at risk, and a health check that refused one
// would fail over a question it is not asked: whether the certificate a person put there names
// the loopback address the check dials, which it need not.
func answers(ctx context.Context, h config.Health) error {
	address := dialable(h.Listen)
	scheme := "http"
	if h.TLS {
		scheme = "https"
	}
	transport := healthTransport(h.TLS)
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+address+"/", nil)
	if err != nil {
		return err
	}
	answer, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return fmt.Errorf("the API at %s did not answer: %w", address, err)
	}
	answer.Body.Close()
	return nil
}

// healthTransport is how health reaches the API: never through a proxy the environment names,
// which tlsfloor bypasses for the loopback alone, since the API asked is the one on this host at
// whatever address it listens on, and a proxy's answer would pass for the API's.
func healthTransport(overTLS bool) *http.Transport {
	transport := tlsfloor.Transport()
	transport.Proxy = nil
	if overTLS {
		transport.TLSClientConfig.InsecureSkipVerify = true
	}
	return transport
}

// dialable is the address a listener on listen is reached at from the same host: the loopback where
// it listens on every interface.
func dialable(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		// Refused by the configuration already, which read it.
		return listen
	}
	switch ip := net.ParseIP(host); {
	case host == "":
		host = "127.0.0.1"
	case ip != nil && ip.IsUnspecified() && ip.To4() != nil:
		host = "127.0.0.1"
	case ip != nil && ip.IsUnspecified():
		host = "::1"
	}
	return net.JoinHostPort(host, port)
}
