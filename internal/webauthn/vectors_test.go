package webauthn

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// The test vectors of the specification, §16, which are known answers made by somebody other than
// this package's author: the examples of a Relying Party ceremony pair that "Relying Party
// implementers may check that they can successfully validate". testdata/vectors.json holds them as
// §16 prints them, keyed by section, for the RP ID example.org and the origin https://example.org.
//
// The registrations in packed attestation (§16.3, §16.10, §16.11) are refused for their format,
// which is this installation's choice, and their assertions are still verified against the key the
// registration carries: that is how an RS256 and an Ed25519 signature made elsewhere are checked.

type vector struct {
	RegChallenge      string `json:"regChallenge"`
	CredentialID      string `json:"credentialID"`
	AAGUID            string `json:"aaguid"`
	CreateClientData  string `json:"createClientData"`
	AttestationObject string `json:"attestationObject"`
	GetChallenge      string `json:"getChallenge"`
	AuthenticatorData string `json:"authenticatorData"`
	GetClientData     string `json:"getClientData"`
	Signature         string `json:"signature"`
}

func vectors(t testing.TB) map[string]vector {
	t.Helper()
	b, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]vector
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (v vector) ceremony(t *testing.T, challenge string) Ceremony {
	return Ceremony{RPID: "example.org", Origin: "https://example.org", Challenge: unhex(t, challenge)}
}

func (v vector) registration(t *testing.T) Registration {
	return Registration{ClientDataJSON: unhex(t, v.CreateClientData), AttestationObject: unhex(t, v.AttestationObject)}
}

func (v vector) assertion(t *testing.T) Assertion {
	return Assertion{
		CredentialID:      unhex(t, v.CredentialID),
		ClientDataJSON:    unhex(t, v.GetClientData),
		AuthenticatorData: unhex(t, v.AuthenticatorData),
		Signature:         unhex(t, v.Signature),
	}
}

// carried is the record a registration's authenticator data carries, read past its attestation
// format and its client data, for the vectors whose registration is refused for one of them.
func (v vector) carried(t *testing.T) Credential {
	t.Helper()
	obj, err := decodeCBOR(unhex(t, v.AttestationObject))
	if err != nil {
		t.Fatal(err)
	}
	ad, err := parseAuthenticatorData(obj.(map[any]any)["authData"].([]byte))
	if err != nil {
		t.Fatal(err)
	}
	return Credential{
		ID:             ad.attested.id,
		PublicKey:      ad.attested.publicKey,
		SignCount:      ad.signCount,
		BackupEligible: ad.flags&flagBE != 0,
		BackupState:    ad.flags&flagBS != 0,
	}
}

// §16.2 and §16.6: a registration with no attestation returns the credential of the vector, and
// its assertion verifies against it.
func TestTheSpecificationsRegistrationsVerify(t *testing.T) {
	all := vectors(t)
	for _, c := range []struct {
		section    string
		be, bs, uv bool
		assertedBS bool
		assertedUV bool
	}{
		// 16.2: registered with flags 0x59, UP BE BS AT; asserted with 0x19, UP BE BS.
		{"16.2", true, true, false, true, false},
		// 16.6: registered with flags 0x49, UP BE AT; asserted with 0x0d, UP UV BE.
		{"16.6", true, false, false, false, true},
	} {
		t.Run(c.section, func(t *testing.T) {
			v := all[c.section]
			cred, err := VerifyRegistration(v.ceremony(t, v.RegChallenge), v.registration(t))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(cred.ID, unhex(t, v.CredentialID)) {
				t.Errorf("the credential ID is %x, where §%s made %s", cred.ID, c.section, v.CredentialID)
			}
			if !bytes.Equal(cred.AAGUID[:], unhex(t, v.AAGUID)) {
				t.Errorf("the AAGUID is %x, where §%s sent %s", cred.AAGUID, c.section, v.AAGUID)
			}
			if cred.BackupEligible != c.be || cred.BackupState != c.bs || cred.SignCount != 0 {
				t.Errorf("the record is %+v, where §%s says BE %v and BS %v", cred, c.section, c.be, c.bs)
			}
			if !bytes.Equal(cred.PublicKey, v.carried(t).PublicKey) {
				t.Errorf("the public key is %x, where the authenticator data carries %x", cred.PublicKey, v.carried(t).PublicKey)
			}

			// Neither registration verified the user, so a ceremony that requires it refuses both.
			strict := v.ceremony(t, v.RegChallenge)
			strict.RequireUserVerification = true
			_, err = VerifyRegistration(strict, v.registration(t))
			wantRefused(t, err, "did not verify the user")

			get := v.ceremony(t, v.GetChallenge)
			get.RequireUserVerification = c.assertedUV
			next, err := VerifyAssertion(get, cred, v.assertion(t))
			if err != nil {
				t.Fatal(err)
			}
			if next.SignCount != 0 || next.BackupState != c.assertedBS {
				t.Errorf("the record returned is %+v, where §%s asserted counter 0 and BS %v", next, c.section, c.assertedBS)
			}

			// And the assertion signed once does not verify a byte away.
			a := v.assertion(t)
			a.Signature[len(a.Signature)-1] ^= 1
			_, err = VerifyAssertion(get, cred, a)
			wantRefused(t, err, "signature does not verify")
		})
	}
}

// §16.4 and §16.5: a ceremony that ran in a cross-origin frame is refused, registration and
// assertion alike, since the sign-in page is never framed. §16.5 names its top-level origin as
// well, and is refused for crossOrigin first; a topOrigin alone is refused in clientDataRefusals.
func TestTheSpecificationsFramedCeremoniesAreRefused(t *testing.T) {
	all := vectors(t)
	for _, section := range []string{"16.4", "16.5"} {
		t.Run(section, func(t *testing.T) {
			v := all[section]
			_, err := VerifyRegistration(v.ceremony(t, v.RegChallenge), v.registration(t))
			wantRefused(t, err, "cross-origin frame")
			_, err = VerifyAssertion(v.ceremony(t, v.GetChallenge), v.carried(t), v.assertion(t))
			wantRefused(t, err, "cross-origin frame")
		})
	}
}

// §16.3, §16.10 and §16.11: a packed attestation, self or full, is refused for its format, and the
// assertions of the three credentials, ES256, RS256 and Ed25519, verify.
func TestTheSpecificationsPackedCredentialsAssertAndDoNotRegister(t *testing.T) {
	all := vectors(t)
	for section, alg := range map[string]Algorithm{"16.3": ES256, "16.10": RS256, "16.11": EdDSA} {
		t.Run(section, func(t *testing.T) {
			v := all[section]
			_, err := VerifyRegistration(v.ceremony(t, v.RegChallenge), v.registration(t))
			wantRefused(t, err, `the attestation format "packed" is not accepted`)

			cred := v.carried(t)
			key, err := decodeCBOR(cred.PublicKey)
			if err != nil {
				t.Fatal(err)
			}
			if k, err := parsePublicKey(key); err != nil || k.alg != alg {
				t.Fatalf("the key of §%s reads as %v, %v, where it is %s", section, k.alg, err, alg)
			}
			get := v.ceremony(t, v.GetChallenge)
			if _, err := VerifyAssertion(get, cred, v.assertion(t)); err != nil {
				t.Fatal(err)
			}
			a := v.assertion(t)
			a.AuthenticatorData[len(a.AuthenticatorData)-1] ^= 1
			_, err = VerifyAssertion(get, cred, a)
			wantRefused(t, err, "signature does not verify")
		})
	}
}
