package config

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	"github.com/agentiik/agentiik/internal/tlsfloor"
)

// TLS is the certificate a program serves its listener with itself, and is zero where it serves
// plain HTTP to whatever terminates TLS in front of it.
//
// Both are PEM, as openssl, certbot and every authority write them. Certificate is the chain, the
// server's own certificate first, and is no secret: anybody who connects is sent it. Key is its
// private key, which is.
type TLS struct {
	Certificate string
	Key         Secret
}

// Served says whether the program serves TLS itself.
func (t TLS) Served() bool { return t.Certificate != "" }

// Server is the configuration the listener is served with, holding the floor, or nil where the
// program serves plain HTTP.
//
// HTTP/1.1 alone, as a terminator in front speaks it to the programs today, so that serving TLS
// changes the transport and nothing the handlers see: every route and stream was built and tested
// over HTTP/1.1, and a client that offers HTTP/2 falls back to it.
func (t TLS) Server() (*tls.Config, error) {
	if !t.Served() {
		return nil, nil
	}
	pair, err := tls.X509KeyPair([]byte(t.Certificate), []byte(t.Key))
	if err != nil {
		// Unreachable for a TLS this package read, which it has paired once already.
		return nil, err
	}
	c := tlsfloor.Server(pair)
	c.NextProtos = []string{"http/1.1"}
	return c, nil
}

// served reads the certificate a program's one listener is served with, where it names one.
//
// Both files or neither: a certificate is nothing to serve without its key, and a key alone is an
// installation that meant to serve TLS and forgot what with. listener is the variable naming the
// address it is served on, for a refusal to say which listener would be served in plaintext.
//
// The key's file is a secret's and held to every rule a secret's is. The certificate's is held to
// all of them but its mode, since certbot and most authorities write a certificate anybody may read,
// and anybody who connects is sent it anyway.
func (r *reader) served(listener string) TLS {
	_, certSet := r.value(TLSCertFile)
	_, keySet := r.value(TLSKeyFile)
	switch {
	case certSet && !keySet:
		r.refuse(TLSKeyFile, "is not set and "+TLSCertFile+" is, and a certificate is served with its key: set both, for "+listener+" to be served over TLS, or neither")
		return TLS{}
	case keySet && !certSet:
		r.refuse(TLSCertFile, "is not set and "+TLSKeyFile+" is, and a key is served with its certificate: set both, for "+listener+" to be served over TLS, or neither")
		return TLS{}
	case !certSet:
		return TLS{}
	}
	chain := r.readFile(TLSCertFile, false)
	key := r.optionalFile(TLSKeyFile)
	if chain == nil || key == nil {
		return TLS{}
	}
	leaf, reason := firstCertificate(chain)
	if reason != "" {
		r.refuse(TLSCertFile, reason)
		return TLS{}
	}
	switch now := time.Now(); {
	case now.After(leaf.NotAfter):
		r.refuse(TLSCertFile, fmt.Sprintf("names a certificate that expired at %s, which every client refuses", leaf.NotAfter.UTC().Format(time.RFC3339)))
		return TLS{}
	case now.Before(leaf.NotBefore):
		r.refuse(TLSCertFile, fmt.Sprintf("names a certificate valid from %s only, which every client refuses until then: check this host's clock", leaf.NotBefore.UTC().Format(time.RFC3339)))
		return TLS{}
	}
	if _, err := tls.X509KeyPair(chain, key); err != nil {
		// What crypto/tls says of a key is a sentence of its own that quotes none of it. The
		// certificate the key is paired with is the chain's first, so a chain in another order
		// is refused here too, and the refusal says so.
		r.refuse(TLSKeyFile, "names a file holding no private key for the first certificate "+TLSCertFile+" names, which is the server's own, since a chain begins with it: "+err.Error())
		return TLS{}
	}
	return TLS{Certificate: string(chain), Key: Secret(key)}
}

// firstCertificate is the server's own certificate, the first of a PEM chain, or why there is none.
//
// A file holding a private key beside the chain, as haproxy and some ACME clients write one, is
// refused. Held as the certificate, the key would be a value the configuration prints, and the file
// a key's file excused from the mode a key's is held to.
func firstCertificate(chain []byte) (*x509.Certificate, string) {
	var leaf *x509.Certificate
	for rest := chain; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		switch {
		case block == nil && leaf == nil:
			return nil, "names a file holding no PEM certificate, which begins -----BEGIN CERTIFICATE-----"
		case block == nil:
			return leaf, ""
		case strings.Contains(block.Type, "PRIVATE KEY"):
			return nil, "names a file holding a private key, which belongs in the file " + TLSKeyFile + " names and nowhere else, since this one is held to no secret's rules"
		case block.Type != "CERTIFICATE" || leaf != nil:
			continue
		}
		var err error
		if leaf, err = x509.ParseCertificate(block.Bytes); err != nil {
			return nil, "names a file whose first certificate cannot be read: " + err.Error()
		}
	}
}
