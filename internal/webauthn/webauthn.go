// Package webauthn verifies the two passkey ceremonies of Web Authentication Level 3, registration
// and assertion, with the standard library alone and nothing behind it: no server, no database,
// no HTTP. The API issues the options, keeps the challenge, stores the record and opens the
// session; this package answers whether what a browser sent back is what the options asked for,
// signed by the key it claims.
//
// The documentation, under Authentication: "From v0.3.0, the API verifies registrations and
// assertions with the Go standard library alone: a CBOR decoder written for it and fuzzed, which
// refuses indefinite lengths, duplicate map keys and trailing bytes; COSE keys ES256, EdDSA and
// RS256, checked with crypto/ecdsa, crypto/ed25519 and crypto/rsa; attestation conveyance none,
// the attestation format none accepted."
//
// Section and step numbers are those of the W3C Recommendation, https://www.w3.org/TR/webauthn-3/.
//
// # Why no library
//
// The code that decides who someone is should be read whole by whoever reviews it, and the part a
// library would bring, a CBOR decoder, is small enough to write here and to fuzz. The decoder
// reads the subset WebAuthn writes and refuses the rest, which is what a general decoder cannot
// afford to do.
//
// # Why no attestation
//
// Attestation would certify the authenticator's make, but the synced passkeys most people hold
// carry none, so asking for it would refuse them or prove nothing. What the policy needs of the
// authenticator, device_bound_only, it reads from the Backup Eligibility flag of the authenticator
// data, which is the authenticator's own statement and needs no certificate.
//
// # What the caller does
//
// The steps of §7.1 and §7.2 that need state are the caller's, and are named here rather than
// half done:
//
//   - the challenge: random, 16 bytes at least (§13.4.3), kept when the options are issued and
//     accepted once, which is what stops a response being replayed;
//   - at registration, that no user holds the credential ID already (§7.1 step 26);
//   - at assertion, finding the record by the credential ID the browser returned, and that it
//     belongs to the user the response's user handle names (§7.2 steps 5 and 6);
//   - the policy: device_bound_only reads Credential.BackupEligible, and what a possible clone
//     means is decided on ErrPossibleClone;
//   - storing the record a registration returns, and the one an assertion returns in its place.
package webauthn

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
)

// Ceremony is what the Relying Party fixed before it asked the browser, and what the answer is
// checked against.
type Ceremony struct {
	// RPID is the Relying Party Identifier: the host of AGK_PUBLIC_URL, or of AGK_PROXY_URL behind
	// a proxy, in lower case as a browser writes a host. The authenticator data carries its
	// SHA-256, so the options and this ceremony name it with the same bytes.
	RPID string

	// Origin is the origin of the sign-in page, as a browser serializes it: scheme and host, and
	// the port only where it is not the scheme's own, https://agentiik.example.com or
	// https://localhost:8443. It is compared as a string.
	Origin string

	// Challenge is the challenge the options carried, 16 bytes at least.
	Challenge []byte

	// RequireUserVerification is the policy's user_verification: true where it is required, and
	// the UV flag must then be set; false where it is preferred, and the flag is not read.
	RequireUserVerification bool
}

// minChallenge is the shortest challenge a ceremony takes, §13.4.3: enough entropy that it cannot
// be guessed.
const minChallenge = 16

// check refuses a ceremony built wrong, which is the caller's mistake rather than the browser's.
func (c Ceremony) check() error {
	switch {
	case c.RPID == "":
		return fmt.Errorf("webauthn: the ceremony names no Relying Party Identifier")
	case c.Origin == "":
		return fmt.Errorf("webauthn: the ceremony names no origin")
	case len(c.Challenge) < minChallenge:
		return fmt.Errorf("webauthn: a challenge of %d bytes is too short to be one, which is %d bytes at least", len(c.Challenge), minChallenge)
	}
	return nil
}

// Registration is what a browser answers navigator.credentials.create() with, the two fields of
// AuthenticatorAttestationResponse this package reads (§5.2.1).
type Registration struct {
	ClientDataJSON    []byte
	AttestationObject []byte
}

// Assertion is what a browser answers navigator.credentials.get() with: the credential's raw ID
// and the fields of AuthenticatorAssertionResponse this package reads (§5.2.2). The user handle is
// not among them: which user a credential belongs to is the caller's to check, against its rows.
type Assertion struct {
	CredentialID      []byte
	ClientDataJSON    []byte
	AuthenticatorData []byte
	Signature         []byte
}

// Credential is the record of a passkey (§7.1 step 27): what a registration returns to store, what
// an assertion is verified against, and what it returns in its place.
type Credential struct {
	// ID is the credential ID, at most 1023 bytes.
	ID []byte

	// PublicKey is the credential public key as a COSE_Key, the bytes the authenticator wrote.
	PublicKey []byte

	// SignCount is the signature counter the authenticator last reported (§6.1.1).
	SignCount uint32

	// AAGUID is as received. Under attestation conveyance none a client passes it on unchanged
	// (§5.1.3), and nothing certifies it: it may name the authenticator's model, and proves
	// nothing about it.
	AAGUID [16]byte

	// BackupEligible is the BE flag, set at registration and never changed after (§6.1.3):
	// whether the passkey is synced.
	BackupEligible bool

	// BackupState is the BS flag, whether the passkey is backed up now, which may change between
	// assertions.
	BackupState bool
}

// Kind is whether a passkey is synced or device-bound, the distinction an administrator reasons
// about their exposure with (Authentication).
type Kind string

const (
	// Synced is a passkey whose Backup Eligibility flag is set: its trust moves to the cloud
	// account that syncs it.
	Synced Kind = "synced"

	// DeviceBound is a passkey whose Backup Eligibility flag is clear: it lives on one piece of
	// hardware, which can be lost.
	DeviceBound Kind = "device-bound"
)

// Kind derives the passkey's kind from its Backup Eligibility flag, as the documentation records
// it: synced where the flag is set, device-bound where it is not. Backup State does not enter
// into it: a passkey eligible for backup may be copied off the device whenever its provider
// chooses, backed up at this moment or not.
func (c Credential) Kind() Kind {
	if c.BackupEligible {
		return Synced
	}
	return DeviceBound
}

// ErrPossibleClone is what VerifyAssertion answers when the assertion is valid in every respect
// but its signature counter, which did not move forward from the stored one while one of the two
// is not zero (§7.2 step 22). That is a signal and not a proof, §6.1.1 says: two copies of the key
// in use, an authenticator that malfunctions, or two assertions verified out of order would all
// look the same. It is an error rather than a flag on the result so that a caller cannot accept
// it without having decided to, and the decision, to refuse the sign-in, to lock the credential
// or to let it through and warn, is policy, which is the caller's.
var ErrPossibleClone = errors.New("webauthn: the signature counter did not move forward, so another copy of this credential may be in use")

// VerifyRegistration verifies a registration, §7.1, and returns the record to store.
//
// The attestation format is none and nothing else. The options ask for attestation conveyance
// none, because the synced passkeys most people hold carry no attestation and device_bound_only
// reads the Backup Eligibility flag instead. A client asked for none replaces any statement but a
// self attestation with the none statement (§5.1.3), so what else could arrive is a self
// attestation or a client that did not do as asked, and either is refused naming its format: a
// self attestation proves only that the new key signed, which the first assertion proves anyway.
func VerifyRegistration(c Ceremony, r Registration) (Credential, error) {
	if err := c.check(); err != nil {
		return Credential{}, err
	}
	// §7.1 steps 5 to 11.
	if err := checkClientData(r.ClientDataJSON, typeCreate, c); err != nil {
		return Credential{}, err
	}

	// §7.1 step 13, and steps 21 and 22 for the one format accepted (§8.7).
	v, err := decodeCBOR(r.AttestationObject)
	if err != nil {
		return Credential{}, fmt.Errorf("webauthn: the attestation object cannot be read: %w", err)
	}
	obj, ok := v.(map[any]any)
	if !ok {
		return Credential{}, fmt.Errorf("webauthn: the attestation object is not a map")
	}
	format, ok := obj["fmt"].(string)
	if !ok {
		return Credential{}, fmt.Errorf("webauthn: the attestation object names no format")
	}
	if format != "none" {
		return Credential{}, fmt.Errorf("webauthn: the attestation format %q is not accepted: this installation asks for no attestation, and accepts the format \"none\" alone", format)
	}
	if stmt, ok := obj["attStmt"].(map[any]any); !ok || len(stmt) != 0 {
		return Credential{}, fmt.Errorf("webauthn: the statement of a none attestation is an empty map, and this one is not")
	}
	raw, ok := obj["authData"].([]byte)
	if !ok {
		return Credential{}, fmt.Errorf("webauthn: the attestation object carries no authenticator data")
	}
	ad, err := parseAuthenticatorData(raw)
	if err != nil {
		return Credential{}, err
	}
	// §6.1: the authenticator data of a registration carries the new credential.
	if ad.attested == nil {
		return Credential{}, fmt.Errorf("webauthn: the authenticator data of a registration carries no credential, its AT flag being clear")
	}
	// §7.1 steps 14 to 17. Step 20 is parseAuthenticatorData's, which refuses a key that names
	// none of Algorithms, and step 25 too, which refuses a credential ID over 1023 bytes.
	if err := checkAuthenticatorData(ad, c); err != nil {
		return Credential{}, err
	}

	// §7.1 step 27.
	return Credential{
		ID:             ad.attested.id,
		PublicKey:      ad.attested.publicKey,
		SignCount:      ad.signCount,
		AAGUID:         ad.attested.aaguid,
		BackupEligible: ad.flags&flagBE != 0,
		BackupState:    ad.flags&flagBS != 0,
	}, nil
}

// VerifyAssertion verifies an assertion made with the credential stored, §7.2, and returns the
// record to store in its place: the new signature counter and Backup State (§7.2 step 24).
//
// Where the error is ErrPossibleClone, the record is returned as well, since every other check
// passed: whether to store the counter the authenticator reported is part of what the caller
// decides.
func VerifyAssertion(c Ceremony, stored Credential, a Assertion) (Credential, error) {
	if err := c.check(); err != nil {
		return Credential{}, err
	}
	// §7.2 step 6 finds the record by the credential's raw ID, which is the caller's. That it
	// found this one is checked here, so that a lookup gone wrong is not a key verifying another
	// credential's assertion.
	if !bytes.Equal(a.CredentialID, stored.ID) {
		return Credential{}, fmt.Errorf("webauthn: the assertion is by another credential than the record it is verified against")
	}
	v, err := decodeCBOR(stored.PublicKey)
	if err != nil {
		return Credential{}, fmt.Errorf("webauthn: the stored public key cannot be read: %w", err)
	}
	key, err := parsePublicKey(v)
	if err != nil {
		return Credential{}, err
	}

	// §7.2 steps 8 to 14.
	if err := checkClientData(a.ClientDataJSON, typeGet, c); err != nil {
		return Credential{}, err
	}
	ad, err := parseAuthenticatorData(a.AuthenticatorData)
	if err != nil {
		return Credential{}, err
	}
	// §6.1: "For assertion signatures, the AT flag MUST NOT be set."
	if ad.attested != nil {
		return Credential{}, fmt.Errorf("webauthn: the authenticator data of an assertion carries attested credential data, which only a registration does")
	}
	// §7.2 steps 15 to 18.
	if err := checkAuthenticatorData(ad, c); err != nil {
		return Credential{}, err
	}
	// §7.2 step 19. Backup Eligibility is fixed at registration (§6.1.3), and device_bound_only
	// is decided on the stored one, so a credential whose flag has moved is not the one whose
	// kind the policy judged.
	if be := ad.flags&flagBE != 0; be != stored.BackupEligible {
		return Credential{}, fmt.Errorf("webauthn: the credential was registered %s and asserts as %s, and backup eligibility never changes", stored.Kind(), Credential{BackupEligible: be}.Kind())
	}

	// §7.2 steps 20 and 21: the signature is over the authenticator data and the SHA-256 of the
	// client data, the bytes as received.
	hash := sha256.Sum256(a.ClientDataJSON)
	signed := make([]byte, 0, len(a.AuthenticatorData)+len(hash))
	signed = append(append(signed, a.AuthenticatorData...), hash[:]...)
	if err := key.verify(signed, a.Signature); err != nil {
		return Credential{}, err
	}

	// §7.2 step 24.
	updated := Credential{
		ID:             bytes.Clone(stored.ID),
		PublicKey:      bytes.Clone(stored.PublicKey),
		SignCount:      ad.signCount,
		AAGUID:         stored.AAGUID,
		BackupEligible: stored.BackupEligible,
		BackupState:    ad.flags&flagBS != 0,
	}
	// §7.2 step 22. An authenticator with no counter reports zero every time, which is what the
	// synced passkeys most people hold do, and two zeros say nothing.
	if (ad.signCount != 0 || stored.SignCount != 0) && ad.signCount <= stored.SignCount {
		return updated, fmt.Errorf("%w: it was %d and is now %d", ErrPossibleClone, stored.SignCount, ad.signCount)
	}
	return updated, nil
}

// checkAuthenticatorData runs the checks of the authenticator data both ceremonies share: §7.1
// steps 14 to 17, which are §7.2 steps 15 to 18.
func checkAuthenticatorData(ad authenticatorData, c Ceremony) error {
	if ad.rpIDHash != sha256.Sum256([]byte(c.RPID)) {
		return fmt.Errorf("webauthn: the authenticator data is for another Relying Party than %q", c.RPID)
	}
	// Always, and not only outside conditional mediation as §7.1 step 15 allows: this
	// installation never creates a credential without the user's gesture.
	if ad.flags&flagUP == 0 {
		return fmt.Errorf("webauthn: the authenticator did not test that the user was present")
	}
	if c.RequireUserVerification && ad.flags&flagUV == 0 {
		return fmt.Errorf("webauthn: the authenticator did not verify the user, and the policy requires it")
	}
	// §6.1.3: backed up but not eligible for backup is a combination that is not allowed.
	if ad.flags&flagBE == 0 && ad.flags&flagBS != 0 {
		return fmt.Errorf("webauthn: the authenticator data says the credential is backed up but not eligible for backup, which cannot be")
	}
	return nil
}
