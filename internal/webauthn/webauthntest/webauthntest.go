// Package webauthntest is a browser and a software authenticator behind it, made of the standard
// library, for the tests of whatever runs the passkey ceremonies over HTTP: it answers the options
// an API issues, as PublicKeyCredential.parseCreationOptionsFromJSON() and
// parseRequestOptionsFromJSON() read them, with what navigator.credentials answers, as
// PublicKeyCredential.toJSON() writes it.
//
// Package webauthn's own tests build every byte of a ceremony from parts they change one at a time,
// which is how each check is shown to refuse what it is there for. They are in its _test files,
// which no other package can import, so this is the same authenticator made whole for a test that
// wants a ceremony end to end: ES256 keys, the attestation format none, and discoverable
// credentials. Package webauthn's tests verify what it writes, so that the two do not drift apart.
//
// It is for tests: its keys live in memory and its choices are whatever a test sets.
package webauthntest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Authenticator is a platform authenticator in a browser on one origin. Its fields are what a
// test changes to have it answer otherwise than a well-behaved one would.
type Authenticator struct {
	// Origin is the origin the browser writes in the client data, the page's.
	Origin string

	// RPID, where it is not empty, is the Relying Party Identifier whose SHA-256 the authenticator
	// data carries, in place of the one the options name.
	RPID string

	// UserVerified is the UV flag: whether the authenticator verified the person, by a PIN or a
	// fingerprint. A test clearing it has an authenticator that cannot.
	UserVerified bool

	// BackupEligible and BackedUp are the BE and BS flags of every passkey it makes and every
	// assertion it signs: set on both, a synced passkey; clear on both, a device-bound one.
	BackupEligible, BackedUp bool

	// Counts is whether the authenticator keeps a signature counter, moving it forward at every
	// assertion. One that keeps none reports zero every time, as the synced passkeys most people
	// hold do.
	Counts bool

	passkeys []*Passkey
}

// New is a device-bound authenticator on origin that verifies the person and counts its
// signatures, which is what a security key is.
func New(origin string) *Authenticator {
	return &Authenticator{Origin: origin, UserVerified: true, Counts: true}
}

// Passkey is one credential the authenticator holds.
type Passkey struct {
	ID         []byte
	RPID       string
	UserHandle []byte
	UserName   string

	// Count is the signature counter, which a test may set back to have the authenticator act as
	// a clone of itself.
	Count uint32

	key *ecdsa.PrivateKey
}

// Passkeys are the credentials it holds, the newest last.
func (a *Authenticator) Passkeys() []*Passkey { return slices.Clone(a.passkeys) }

// Credential is PublicKeyCredential.toJSON(): a RegistrationResponseJSON or an
// AuthenticationResponseJSON, each member written as a browser writes it.
type Credential struct {
	ID                      string         `json:"id"`
	RawID                   string         `json:"rawId"`
	Type                    string         `json:"type"`
	Response                Response       `json:"response"`
	AuthenticatorAttachment string         `json:"authenticatorAttachment,omitempty"`
	ClientExtensionResults  map[string]any `json:"clientExtensionResults"`
}

// Response is the authenticator's response, the members of both ceremonies' together: a
// registration writes the attestation object, the transports and the conveniences a browser adds,
// and an assertion the signature and the user handle.
type Response struct {
	ClientDataJSON     string   `json:"clientDataJSON"`
	AuthenticatorData  string   `json:"authenticatorData,omitempty"`
	AttestationObject  string   `json:"attestationObject,omitempty"`
	Transports         []string `json:"transports,omitempty"`
	PublicKey          string   `json:"publicKey,omitempty"`
	PublicKeyAlgorithm int      `json:"publicKeyAlgorithm,omitempty"`
	Signature          string   `json:"signature,omitempty"`
	UserHandle         string   `json:"userHandle,omitempty"`
}

// b64 is how WebAuthn's JSON writes bytes: base64url with no padding.
var b64 = base64.RawURLEncoding

// es256 is the one COSE algorithm the authenticator makes keys for.
const es256 = -7

// creation is what the authenticator reads of PublicKeyCredentialCreationOptionsJSON.
type creation struct {
	RP struct {
		ID string `json:"id"`
	} `json:"rp"`
	User struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"user"`
	Challenge              string       `json:"challenge"`
	PubKeyCredParams       []param      `json:"pubKeyCredParams"`
	ExcludeCredentials     []descriptor `json:"excludeCredentials"`
	AuthenticatorSelection struct {
		ResidentKey      string `json:"residentKey"`
		UserVerification string `json:"userVerification"`
	} `json:"authenticatorSelection"`
	Attestation string `json:"attestation"`
}

// param is one entry of pubKeyCredParams.
type param struct {
	Type string `json:"type"`
	Alg  int    `json:"alg"`
}

// descriptor is a PublicKeyCredentialDescriptorJSON, a credential named by its ID.
type descriptor struct {
	ID string `json:"id"`
}

// Create answers navigator.credentials.create() given options, the JSON an API issued for a
// registration, and keeps the passkey it made. It refuses what a browser would: options that offer
// no ES256 key, and a passkey for an account it already holds one for among those excluded.
func (a *Authenticator) Create(options []byte) (Credential, *Passkey, error) {
	var o creation
	if err := json.Unmarshal(options, &o); err != nil {
		return Credential{}, nil, fmt.Errorf("webauthntest: the creation options do not read: %w", err)
	}
	if !slices.Contains(o.PubKeyCredParams, param{Type: "public-key", Alg: es256}) {
		return Credential{}, nil, errors.New("webauthntest: the options offer no ES256 key, the one this authenticator makes")
	}
	for _, excluded := range o.ExcludeCredentials {
		for _, p := range a.passkeys {
			if b64.EncodeToString(p.ID) == excluded.ID {
				return Credential{}, nil, errors.New("webauthntest: the authenticator holds a passkey the options exclude, which a browser answers InvalidStateError")
			}
		}
	}
	challenge, err := b64.DecodeString(o.Challenge)
	if err != nil {
		return Credential{}, nil, fmt.Errorf("webauthntest: the challenge is not base64url: %w", err)
	}
	handle, err := b64.DecodeString(o.User.ID)
	if err != nil {
		return Credential{}, nil, fmt.Errorf("webauthntest: the user handle is not base64url: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Credential{}, nil, err
	}
	p := &Passkey{ID: make([]byte, 32), RPID: o.RP.ID, UserHandle: handle, UserName: o.User.Name, key: key}
	if _, err := rand.Read(p.ID); err != nil {
		return Credential{}, nil, err
	}
	point, err := key.PublicKey.Bytes()
	if err != nil {
		return Credential{}, nil, err
	}
	// COSE_Key for ES256 (RFC 9053): kty EC2, alg ES256, crv P-256, x and y, keys in the order
	// canonical CBOR sorts them.
	cose := cborMap(
		cborInt(1), cborInt(2),
		cborInt(3), cborInt(es256),
		cborInt(-1), cborInt(1),
		cborInt(-2), cborBytes(point[1:33]),
		cborInt(-3), cborBytes(point[33:]),
	)
	data := a.authenticatorData(p, flagAT, 0)
	data = append(data, make([]byte, 16)...) // the AAGUID, all zeros as a none attestation leaves it
	data = binary.BigEndian.AppendUint16(data, uint16(len(p.ID)))
	data = append(append(data, p.ID...), cose...)
	object := cborMap(
		cborText("fmt"), cborText("none"),
		cborText("attStmt"), cborMap(),
		cborText("authData"), cborBytes(data),
	)
	a.passkeys = append(a.passkeys, p)
	id := b64.EncodeToString(p.ID)
	return Credential{
		ID: id, RawID: id, Type: "public-key",
		Response: Response{
			ClientDataJSON:     b64.EncodeToString(a.clientData("webauthn.create", challenge)),
			AuthenticatorData:  b64.EncodeToString(data),
			AttestationObject:  b64.EncodeToString(object),
			Transports:         []string{"internal", "hybrid"},
			PublicKeyAlgorithm: es256,
		},
		AuthenticatorAttachment: "platform",
		ClientExtensionResults:  map[string]any{},
	}, p, nil
}

// request is what the authenticator reads of PublicKeyCredentialRequestOptionsJSON.
type request struct {
	Challenge        string       `json:"challenge"`
	RPID             string       `json:"rpId"`
	AllowCredentials []descriptor `json:"allowCredentials"`
	UserVerification string       `json:"userVerification"`
}

// Get answers navigator.credentials.get() given options, the JSON an API issued for an assertion,
// signed with the newest passkey it holds for the options' Relying Party and, where the options
// list credentials, among those listed.
func (a *Authenticator) Get(options []byte) (Credential, error) {
	var o request
	if err := json.Unmarshal(options, &o); err != nil {
		return Credential{}, fmt.Errorf("webauthntest: the request options do not read: %w", err)
	}
	for _, p := range slices.Backward(a.passkeys) {
		if p.RPID != o.RPID {
			continue
		}
		if len(o.AllowCredentials) > 0 && !slices.Contains(o.AllowCredentials, descriptor{ID: b64.EncodeToString(p.ID)}) {
			continue
		}
		return a.GetWith(options, p)
	}
	return Credential{}, errors.New("webauthntest: the authenticator holds no passkey for that Relying Party, which a browser answers NotAllowedError")
}

// GetWith answers navigator.credentials.get() given options, signed with p whatever the options
// say, and moves p's counter forward where the authenticator counts.
func (a *Authenticator) GetWith(options []byte, p *Passkey) (Credential, error) {
	var o request
	if err := json.Unmarshal(options, &o); err != nil {
		return Credential{}, fmt.Errorf("webauthntest: the request options do not read: %w", err)
	}
	challenge, err := b64.DecodeString(o.Challenge)
	if err != nil {
		return Credential{}, fmt.Errorf("webauthntest: the challenge is not base64url: %w", err)
	}
	if a.Counts {
		p.Count++
	}
	data := a.authenticatorData(p, 0, p.Count)
	clientData := a.clientData("webauthn.get", challenge)
	hash := sha256.Sum256(clientData)
	digest := sha256.Sum256(slices.Concat(data, hash[:]))
	signature, err := ecdsa.SignASN1(rand.Reader, p.key, digest[:])
	if err != nil {
		return Credential{}, err
	}
	id := b64.EncodeToString(p.ID)
	return Credential{
		ID: id, RawID: id, Type: "public-key",
		Response: Response{
			ClientDataJSON:    b64.EncodeToString(clientData),
			AuthenticatorData: b64.EncodeToString(data),
			Signature:         b64.EncodeToString(signature),
			UserHandle:        b64.EncodeToString(p.UserHandle),
		},
		AuthenticatorAttachment: "platform",
		ClientExtensionResults:  map[string]any{},
	}, nil
}

// The flags of the authenticator data, §6.1.
const (
	flagUP = 0x01
	flagUV = 0x04
	flagBE = 0x08
	flagBS = 0x10
	flagAT = 0x40
)

// authenticatorData is the authenticator data up to the counter, §6.1: the Relying Party's hash,
// the flags, extra ones among them, and the counter.
func (a *Authenticator) authenticatorData(p *Passkey, extra byte, count uint32) []byte {
	rp := p.RPID
	if a.RPID != "" {
		rp = a.RPID
	}
	hash := sha256.Sum256([]byte(rp))
	flags := flagUP | extra
	if a.UserVerified {
		flags |= flagUV
	}
	if a.BackupEligible {
		flags |= flagBE
	}
	if a.BackedUp {
		flags |= flagBS
	}
	return binary.BigEndian.AppendUint32(append(hash[:], flags), count)
}

// clientData is the client data as a browser serializes it, §5.8.1.1.
func (a *Authenticator) clientData(typ string, challenge []byte) []byte {
	return fmt.Appendf(nil, `{"type":%q,"challenge":%q,"origin":%q,"crossOrigin":false}`, typ, b64.EncodeToString(challenge), a.Origin)
}

// CBOR, as an authenticator writes it: every head in its shortest form and definite lengths.

func cborHead(major byte, n uint64) []byte {
	m := major << 5
	switch {
	case n < 24:
		return []byte{m | byte(n)}
	case n <= 0xff:
		return []byte{m | 24, byte(n)}
	case n <= 0xffff:
		return binary.BigEndian.AppendUint16([]byte{m | 25}, uint16(n))
	}
	return binary.BigEndian.AppendUint32([]byte{m | 26}, uint32(n))
}

func cborInt(n int) []byte {
	if n < 0 {
		return cborHead(1, uint64(-1-n))
	}
	return cborHead(0, uint64(n))
}

func cborBytes(b []byte) []byte { return append(cborHead(2, uint64(len(b))), b...) }

func cborText(s string) []byte { return append(cborHead(3, uint64(len(s))), s...) }

// cborMap writes a map of the keys and values given in turn, in the order given.
func cborMap(entries ...[]byte) []byte {
	return append(cborHead(5, uint64(len(entries)/2)), bytes.Join(entries, nil)...)
}
