package webauthn_test

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/agentiik/agentiik/internal/webauthn"
	"github.com/agentiik/agentiik/internal/webauthn/webauthntest"
)

// Package webauthntest's authenticator is what the API's tests run the ceremonies with, so what it
// writes is held here to what this package verifies: a registration and two assertions pass, with
// the flags it was told to set, and the second assertion moves the counter on from the first.
func TestTheTestAuthenticatorWritesWhatIsVerified(t *testing.T) {
	const rp, origin = "agentiik.example.com", "https://agentiik.example.com"
	a := webauthntest.New(origin)
	a.BackupEligible, a.BackedUp = true, true
	b64 := base64.RawURLEncoding

	challenge := func() []byte {
		c := make([]byte, 32)
		if _, err := rand.Read(c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := webauthn.Ceremony{RPID: rp, Origin: origin, Challenge: challenge(), RequireUserVerification: true}
	made, _, err := a.Create(fmt.Appendf(nil,
		`{"rp":{"id":%q,"name":"Agentiik"},"user":{"id":"AAEC","name":"alice","displayName":"Alice"},"challenge":%q,"pubKeyCredParams":[{"type":"public-key","alg":-7}]}`,
		rp, b64.EncodeToString(c.Challenge)))
	if err != nil {
		t.Fatal(err)
	}
	decoded := func(s string) []byte {
		t.Helper()
		b, err := b64.DecodeString(s)
		if err != nil {
			t.Fatalf("%q is not base64url: %s", s, err)
		}
		return b
	}
	stored, err := webauthn.VerifyRegistration(c, webauthn.Registration{
		ClientDataJSON: decoded(made.Response.ClientDataJSON), AttestationObject: decoded(made.Response.AttestationObject),
	})
	if err != nil {
		t.Fatalf("the test authenticator's registration does not verify: %s", err)
	}
	if b64.EncodeToString(stored.ID) != made.RawID || !stored.BackupEligible || !stored.BackupState || stored.Kind() != webauthn.Synced {
		t.Errorf("the registration verifies as %+v", stored)
	}

	for i := range 2 {
		c.Challenge = challenge()
		got, err := a.Get(fmt.Appendf(nil, `{"challenge":%q,"rpId":%q,"allowCredentials":[]}`, b64.EncodeToString(c.Challenge), rp))
		if err != nil {
			t.Fatal(err)
		}
		if string(decoded(got.Response.UserHandle)) != "\x00\x01\x02" {
			t.Errorf("the assertion hands back the user handle %q", got.Response.UserHandle)
		}
		updated, err := webauthn.VerifyAssertion(c, stored, webauthn.Assertion{
			CredentialID: decoded(got.RawID), ClientDataJSON: decoded(got.Response.ClientDataJSON),
			AuthenticatorData: decoded(got.Response.AuthenticatorData), Signature: decoded(got.Response.Signature),
		})
		if err != nil {
			t.Fatalf("assertion %d of the test authenticator does not verify: %s", i+1, err)
		}
		if updated.SignCount != uint32(i+1) {
			t.Errorf("assertion %d reports the counter %d", i+1, updated.SignCount)
		}
		stored = updated
	}
}
