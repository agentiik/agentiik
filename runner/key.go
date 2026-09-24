package runner

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// KeyPath is where join writes the host's private key.
//
// "The key proves the machine": a rotation is signed with it, so a credential stolen without it
// cannot be renewed, and a host whose key is gone is a new runner that joins again. It is under
// /var/lib/agentiik, where the decision put it, the one tree the unit's ProtectSystem=strict leaves
// the agent to write in, and it is owned by the agent's account with mode 0600 because the agent
// is the one process that signs with it.
const KeyPath = "/var/lib/agentiik/runner.key"

// hostKey is a keypair join generated, written the two ways it leaves memory: the private half as
// the key file holds it, and the public half as the join sends it.
type hostKey struct {
	// private is PEM around PKCS #8, the encoding the standard library writes and reads an
	// Ed25519 private key in without a dependency, and one that names its algorithm.
	private []byte
	// public is PEM around a SubjectPublicKeyInfo, which is what the wire's public_key is.
	public string
}

// newHostKey generates the host's Ed25519 keypair.
//
// Ed25519 and nothing else, because the API refuses a key of any other algorithm: a rotation is
// "an Ed25519 signature over the runner identifier and the request time", and a runner that joined
// with another key could never renew.
func newHostKey() (hostKey, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return hostKey{}, fmt.Errorf("runner: the host's key could not be generated: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return hostKey{}, fmt.Errorf("runner: the host's key could not be written: %w", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		return hostKey{}, fmt.Errorf("runner: the host's public key could not be written: %w", err)
	}
	return hostKey{
		private: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
		public:  string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki})),
	}, nil
}
