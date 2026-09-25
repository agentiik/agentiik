// Package tlsfloor holds every connection the programs open to TLS 1.2 at least, and says which
// address is allowed to go without it.
//
// "TLS 1.3 where both ends allow it, TLS 1.2 as the floor, no plaintext path anywhere, including
// between the control plane and the bus." Go already offers 1.3 first and refuses anything under
// 1.2 when a configuration leaves MinVersion at zero, so the floor written here changes no
// handshake today. It is written anyway, on every configuration the programs build and hand to
// pgx, nats.go and net/http, so that the floor is a line of this module rather than a default of
// the toolchain's that a release or a library building its own configuration could move without
// this module saying so.
//
// The one path allowed without TLS is one that crosses no network: a loopback address, which is
// how a person runs agk against an installation on their own machine and how the tests reach the
// servers they start, and a local unix socket, which is how a runner reaches its Docker daemon and
// how PostgreSQL may be reached on the host it runs on. Neither leaves the kernel, and the socket
// is guarded by its file permission, as the Channels table says.
package tlsfloor

import (
	"crypto/tls"
	"net"
	"net/http"
	"strings"
)

// MinVersion is the oldest TLS any connection of the programs speaks.
const MinVersion = tls.VersionTLS12

// Config is a client configuration holding the floor, and nothing else: the roots are the
// system's, and the version is the highest both ends speak.
func Config() *tls.Config { return &tls.Config{MinVersion: MinVersion} }

// Floor raises a configuration somebody else built to the floor, and leaves one already above it
// as it was. A nil configuration is a connection with no TLS at all, which is not this function's
// to judge.
func Floor(c *tls.Config) {
	if c != nil && c.MinVersion < MinVersion {
		c.MinVersion = MinVersion
	}
}

// Transport is a new HTTP transport as Go's default one is, holding the floor.
//
// A new one each call, and so a pool of connections of its own: a caller keeps the one it made
// rather than making one per request, which would leave each request's idle connection behind.
func Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = Config()
	return t
}

// Loopback says whether a host is this machine, by name or by address, which is the one place a
// plaintext connection crosses no network.
//
// The unspecified address, 0.0.0.0 or ::, is this machine too when it is dialled: no packet to it
// leaves the host, and it is the address a server listening on every interface, as a NATS server
// the tests start does, gives as its own.
func Loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}
