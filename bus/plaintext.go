package bus

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/agentiik/agentiik/internal/tlsfloor"
)

// ErrPlaintext is a bus address that would be reached in plaintext across a network.
var ErrPlaintext = errors.New("bus: the bus is never reached in plaintext across a network")

// CheckURL refuses a bus address that would carry the connection in plaintext across a network,
// before anything is dialled.
//
// "No plaintext path anywhere, including between the control plane and the bus." Every server of
// the address is tls:// or wss://, or nats:// or ws:// to a loopback address alone, which crosses
// no network: that is how the tests reach the server they run beside. The control plane's configuration is stricter still and takes
// no loopback exception, and this is the check a runner holds the address the API hands it to, and
// the one every connection holds itself to whoever opened it.
//
// The address is not repeated, since a bus address can carry a user and a token.
func CheckURL(servers string) error {
	for _, server := range strings.Split(servers, ",") {
		u, err := url.Parse(strings.TrimSpace(server))
		switch {
		case err != nil || u.Host == "":
			return fmt.Errorf("%w: one of its servers is not a URL with a host", ErrPlaintext)
		case u.Scheme == "tls" || u.Scheme == "wss":
		case (u.Scheme == "nats" || u.Scheme == "ws") && tlsfloor.Loopback(u.Hostname()):
		case u.Scheme == "nats" || u.Scheme == "ws":
			return fmt.Errorf("%w: one of its servers is %s:// to an address that is not this machine, and only tls:// and wss:// are taken across a network", ErrPlaintext, u.Scheme)
		default:
			return fmt.Errorf("%w: one of its servers is %s://, which is not a NATS address: write tls:// or wss://", ErrPlaintext, u.Scheme)
		}
	}
	return nil
}

// secure says whether every server of an address that passed CheckURL is reached over TLS.
func secure(servers string) bool {
	for _, server := range strings.Split(servers, ",") {
		if u, err := url.Parse(strings.TrimSpace(server)); err != nil || u.Scheme != "tls" && u.Scheme != "wss" {
			return false
		}
	}
	return true
}

// plainWebsocket says whether an address that passed CheckURL names a ws:// server, which is on
// this machine and spoken to in plaintext.
func plainWebsocket(servers string) bool {
	for _, server := range strings.Split(servers, ",") {
		if u, err := url.Parse(strings.TrimSpace(server)); err == nil && u.Scheme == "ws" {
			return true
		}
	}
	return false
}
