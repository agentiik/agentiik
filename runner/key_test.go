package runner

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"regexp"
	"testing"
)

// publicKeyForm is the wire's public_key, copied from $defs/runnerRegistration.
var publicKeyForm = regexp.MustCompile(`^-----BEGIN PUBLIC KEY-----\n[A-Za-z0-9+/\n]+={0,2}\n-----END PUBLIC KEY-----\n?$`)

// The two halves are one Ed25519 keypair, the public one written as the wire takes it and the
// private one as the key file holds it.
func TestTheHostKeyIsOneEd25519KeypairWrittenTheWaysItLeavesMemory(t *testing.T) {
	k, err := newHostKey()
	if err != nil {
		t.Fatal(err)
	}
	if !publicKeyForm.MatchString(k.public) {
		t.Errorf("the public key is written %q, which the wire refuses", k.public)
	}
	block, _ := pem.Decode([]byte(k.public))
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	public, ok := parsed.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("the public key is a %T", parsed)
	}

	block, rest := pem.Decode(k.private)
	if block == nil || block.Type != "PRIVATE KEY" || len(rest) != 0 {
		t.Fatalf("the private key is not one PEM block of PKCS #8: %q", k.private)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	private, ok := key.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("the private key is a %T", key)
	}
	if !private.Public().(ed25519.PublicKey).Equal(public) {
		t.Error("the two halves are not one keypair")
	}

	// A signature made with the private half verifies with the public one, which is what a
	// rotation is.
	msg := []byte("runner-dmz-02 2026-10-20T06:00:00Z")
	if !ed25519.Verify(public, msg, ed25519.Sign(private, msg)) {
		t.Error("what the private half signs, the public half does not verify")
	}

	if again, _ := newHostKey(); again.public == k.public {
		t.Error("two keys came out the same")
	}
}
