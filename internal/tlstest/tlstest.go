// Package tlstest makes the certificate a test serves a listener with, and the clients that
// speak to it.
package tlstest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Pair is a self-signed certificate for 127.0.0.1 and localhost, and its key, both in PEM.
type Pair struct {
	Certificate, Key []byte

	// Roots trusts the certificate and nothing else.
	Roots *x509.CertPool
}

// NewPair makes a pair valid from an hour ago until notAfter.
func NewPair(t testing.TB, notAfter time.Time) Pair {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return Pair{
		Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Key:         pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw}),
		Roots:       roots,
	}
}

// Write puts the pair in dir as server.pem and server.key, readable by their owner alone as every
// secret's file is, and answers their paths.
func (p Pair) Write(t testing.TB, dir string) (certificate, key string) {
	t.Helper()
	certificate, key = filepath.Join(dir, "server.pem"), filepath.Join(dir, "server.key")
	for path, content := range map[string][]byte{certificate: p.Certificate, key: p.Key} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return certificate, key
}

// Client speaks TLS between lowest and highest to a server holding the pair, trusting it alone.
func (p Pair) Client(lowest, highest uint16) *http.Client {
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: p.Roots, MinVersion: lowest, MaxVersion: highest}},
		Timeout:   10 * time.Second,
	}
}
