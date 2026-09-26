package webauthn

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"testing"
)

var algorithms = []Algorithm{ES256, EdDSA, RS256}

func wantRefused(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted, where it is refused with %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("refused with %q, where the refusal says %q", err, want)
	}
}

// A registration returns the record the authenticator made, byte for byte, with each algorithm.
func TestARegistrationReturnsTheCredentialTheAuthenticatorMade(t *testing.T) {
	for _, alg := range algorithms {
		t.Run(alg.String(), func(t *testing.T) {
			a := newAuthenticator(t, alg)
			a.count = 7
			r := a.register(testCeremony(t))
			cred, err := r.verify()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(cred.ID, a.id) {
				t.Errorf("the credential ID is %x, where the authenticator made %x", cred.ID, a.id)
			}
			if !bytes.Equal(cred.PublicKey, enc(a.cose)) {
				t.Errorf("the public key is %x, where the authenticator wrote %x", cred.PublicKey, enc(a.cose))
			}
			if cred.SignCount != 7 || cred.AAGUID != a.aaguid || !cred.BackupEligible || !cred.BackupState || cred.Kind() != Synced {
				t.Errorf("the record is %+v, where the authenticator said count 7, AAGUID %x, BE and BS set", cred, a.aaguid)
			}
		})
	}
}

// Kind follows Backup Eligibility alone, and both flags are recorded as the authenticator set them.
func TestThePasskeyKindFollowsBackupEligibility(t *testing.T) {
	for _, c := range []struct {
		flags  byte
		kind   Kind
		be, bs bool
	}{
		{flagBE | flagBS, Synced, true, true},
		{flagBE, Synced, true, false},
		{0, DeviceBound, false, false},
	} {
		a := newAuthenticator(t, ES256)
		a.flags = flagUV | c.flags
		cred, err := a.register(testCeremony(t)).verify()
		if err != nil {
			t.Fatal(err)
		}
		if cred.Kind() != c.kind || cred.BackupEligible != c.be || cred.BackupState != c.bs {
			t.Errorf("flags %08b gave %s with BE %v and BS %v, where they give %s with BE %v and BS %v", c.flags, cred.Kind(), cred.BackupEligible, cred.BackupState, c.kind, c.be, c.bs)
		}
	}
}

// An assertion verifies with each algorithm, and returns the counter and Backup State it reported.
func TestAnAssertionVerifiesAndReturnsTheNewCounter(t *testing.T) {
	for _, alg := range algorithms {
		t.Run(alg.String(), func(t *testing.T) {
			a, cred := registered(t, alg)
			for range 3 {
				s := a.assert(testCeremony(t), cred)
				next, err := s.verify(t)
				if err != nil {
					t.Fatal(err)
				}
				if next.SignCount != a.count {
					t.Fatalf("the counter returned is %d, where the authenticator reported %d", next.SignCount, a.count)
				}
				if !bytes.Equal(next.ID, cred.ID) || !bytes.Equal(next.PublicKey, cred.PublicKey) || next.AAGUID != cred.AAGUID || next.BackupEligible != cred.BackupEligible {
					t.Fatalf("the record returned is %+v, where only the counter and Backup State move from %+v", next, cred)
				}
				cred = next
			}
		})
	}
}

// Backup State may change between assertions, and the new one is what is returned to store.
func TestBackupStateMovesWithTheAuthenticator(t *testing.T) {
	a, cred := registered(t, ES256)
	a.flags &^= flagBS
	next, err := a.assert(testCeremony(t), cred).verify(t)
	if err != nil {
		t.Fatal(err)
	}
	if next.BackupState {
		t.Fatal("Backup State is still set after the authenticator cleared it")
	}
}

// The counter rule of §7.2 step 22: forward is valid, two zeros say nothing, and anything else is a
// possible clone, answered with the record so that the caller decides.
func TestACounterThatDoesNotMoveForwardIsAPossibleClone(t *testing.T) {
	for _, c := range []struct {
		stored, received uint32
		clone            bool
	}{
		{0, 0, false},
		{0, 1, false},
		{5, 6, false},
		{5, 5, true},
		{5, 4, true},
		{5, 0, true},
		{1<<32 - 1, 0, true},
	} {
		t.Run(fmt.Sprintf("%d then %d", c.stored, c.received), func(t *testing.T) {
			a, cred := registered(t, ES256)
			cred.SignCount = c.stored
			s := a.assert(testCeremony(t), cred)
			s.data.signCount = c.received
			next, err := s.verify(t)
			if c.clone != errors.Is(err, ErrPossibleClone) {
				t.Fatalf("the error is %v, where a possible clone is %v", err, c.clone)
			}
			if !c.clone && err != nil {
				t.Fatal(err)
			}
			if next.SignCount != c.received || !bytes.Equal(next.ID, cred.ID) {
				t.Fatalf("the record returned is %+v, where it carries the counter %d", next, c.received)
			}
		})
	}
}

// A possible clone is reported only once the signature holds: a forged assertion with a stale
// counter is a forgery, not a clone.
func TestACloneIsOnlyReportedOfAValidSignature(t *testing.T) {
	a, cred := registered(t, ES256)
	cred.SignCount = 10
	s := a.assert(testCeremony(t), cred)
	s.data.signCount = 3
	s.tamper = func(sig []byte) []byte { sig[len(sig)-1] ^= 1; return sig }
	_, err := s.verify(t)
	if errors.Is(err, ErrPossibleClone) {
		t.Fatal("a signature that does not verify was reported as a possible clone")
	}
	wantRefused(t, err, "signature does not verify")
}

// Where user verification is preferred, the UV flag is not read, at registration or at assertion.
func TestUserVerificationIsReadOnlyWhereTheCeremonyRequiresIt(t *testing.T) {
	a := newAuthenticator(t, ES256)
	a.flags &^= flagUV
	c := testCeremony(t)
	c.RequireUserVerification = false
	cred, err := a.register(c).verify()
	if err != nil {
		t.Fatal(err)
	}
	c = testCeremony(t)
	c.RequireUserVerification = false
	if _, err := a.assert(c, cred).verify(t); err != nil {
		t.Fatal(err)
	}
}

// Extension outputs an authenticator adds unasked are read past and ignored, at registration and
// at assertion, booleans included.
func TestExtensionsAreReadPastAndIgnored(t *testing.T) {
	ext := enc(pairs{{"credProtect", 2}, {"hmac-secret", true}})
	a := newAuthenticator(t, EdDSA)
	r := a.register(testCeremony(t))
	r.data.flags |= flagED
	r.data.extensions = ext
	cred, err := r.verify()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cred.PublicKey, enc(a.cose)) {
		t.Fatalf("the public key is %x, where it ends before the extensions", cred.PublicKey)
	}
	s := a.assert(testCeremony(t), cred)
	s.data.flags |= flagED
	s.data.extensions = enc(pairs{{"hmac-secret", []byte("output")}})
	if _, err := s.verify(t); err != nil {
		t.Fatal(err)
	}
}

// What the client data may carry and still be a browser's: members nobody asked for, a byte order
// mark (§7.1 step 5), crossOrigin absent, and members in another order.
func TestClientDataIsReadAsTheSpecificationAllows(t *testing.T) {
	for name, write := range map[string]func(typ string, challenge []byte, origin string) []byte{
		"with a member nobody asked for": func(typ string, ch []byte, origin string) []byte {
			return fmt.Appendf(nil, `{"type":%q,"challenge":%q,"origin":%q,"crossOrigin":false,"other_keys_can_be_added_here":"do not compare clientDataJSON against a template"}`, typ, b64(ch), origin)
		},
		"after a byte order mark": func(typ string, ch []byte, origin string) []byte {
			return append([]byte("\xef\xbb\xbf"), clientDataJSON(typ, ch, origin)...)
		},
		"with no crossOrigin": func(typ string, ch []byte, origin string) []byte {
			return fmt.Appendf(nil, `{"type":%q,"challenge":%q,"origin":%q}`, typ, b64(ch), origin)
		},
		"in another order": func(typ string, ch []byte, origin string) []byte {
			return fmt.Appendf(nil, `{ "origin": %q, "crossOrigin": false, "challenge": %q, "type": %q }`, origin, b64(ch), typ)
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := newAuthenticator(t, ES256)
			r := a.register(testCeremony(t))
			r.clientData = write(typeCreate, r.ceremony.Challenge, r.ceremony.Origin)
			cred, err := r.verify()
			if err != nil {
				t.Fatal(err)
			}
			s := a.assert(testCeremony(t), cred)
			s.clientData = write(typeGet, s.ceremony.Challenge, s.ceremony.Origin)
			if _, err := s.verify(t); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// The client data checks, §7.1 steps 7 to 11 and §7.2 steps 10 to 14, each refusing the one
// thing it is there for, in both ceremonies.
var clientDataRefusals = []struct {
	name  string
	write func(typ string, challenge []byte, origin string) []byte
	want  string
}{
	{"not JSON", func(string, []byte, string) []byte { return []byte("type=webauthn") }, "not the JSON object"},
	{"a JSON array", func(string, []byte, string) []byte { return []byte("[]") }, "not the JSON object"},
	{"of the other ceremony's type", func(typ string, ch []byte, origin string) []byte {
		other := map[string]string{typeCreate: typeGet, typeGet: typeCreate}[typ]
		return clientDataJSON(other, ch, origin)
	}, "the client data is of type"},
	{"of no type", func(_ string, ch []byte, origin string) []byte {
		return fmt.Appendf(nil, `{"challenge":%q,"origin":%q}`, b64(ch), origin)
	}, "the client data is of type"},
	{"answering another challenge", func(typ string, ch []byte, origin string) []byte {
		other := slices.Clone(ch)
		other[0] ^= 1
		return clientDataJSON(typ, other, origin)
	}, "another challenge"},
	{"with the challenge padded", func(typ string, ch []byte, origin string) []byte {
		return fmt.Appendf(nil, `{"type":%q,"challenge":%q,"origin":%q}`, typ, base64.URLEncoding.EncodeToString(ch[:31]), origin)
	}, "another challenge"},
	{"with the challenge in standard base64", func(typ string, ch []byte, origin string) []byte {
		ch = bytes.Repeat([]byte{0xfb}, 32)
		return fmt.Appendf(nil, `{"type":%q,"challenge":%q,"origin":%q}`, typ, base64.RawStdEncoding.EncodeToString(ch), origin)
	}, "another challenge"},
	{"from another origin", func(typ string, ch []byte, _ string) []byte {
		return clientDataJSON(typ, ch, "https://agentiik.example.org")
	}, "comes from the origin"},
	{"from the origin over http", func(typ string, ch []byte, _ string) []byte {
		return clientDataJSON(typ, ch, "http://agentiik.example.com")
	}, "comes from the origin"},
	{"from the origin with a port", func(typ string, ch []byte, _ string) []byte {
		return clientDataJSON(typ, ch, "https://agentiik.example.com:8443")
	}, "comes from the origin"},
	{"from a subdomain of the Relying Party", func(typ string, ch []byte, _ string) []byte {
		return clientDataJSON(typ, ch, "https://evil.agentiik.example.com")
	}, "comes from the origin"},
	{"in a cross-origin frame", func(typ string, ch []byte, origin string) []byte {
		return fmt.Appendf(nil, `{"type":%q,"challenge":%q,"origin":%q,"crossOrigin":true}`, typ, b64(ch), origin)
	}, "cross-origin frame"},
	{"with crossOrigin null", func(typ string, ch []byte, origin string) []byte {
		return fmt.Appendf(nil, `{"type":%q,"challenge":%q,"origin":%q,"crossOrigin":null}`, typ, b64(ch), origin)
	}, "where it is false or absent"},
	{"with crossOrigin a string", func(typ string, ch []byte, origin string) []byte {
		return fmt.Appendf(nil, `{"type":%q,"challenge":%q,"origin":%q,"crossOrigin":"false"}`, typ, b64(ch), origin)
	}, "where it is false or absent"},
	{"naming a top-level origin", func(typ string, ch []byte, origin string) []byte {
		return fmt.Appendf(nil, `{"type":%q,"challenge":%q,"origin":%q,"crossOrigin":false,"topOrigin":%q}`, typ, b64(ch), origin, origin)
	}, "top-level origin"},
	{"naming a null top-level origin", func(typ string, ch []byte, origin string) []byte {
		return fmt.Appendf(nil, `{"type":%q,"challenge":%q,"origin":%q,"topOrigin":null}`, typ, b64(ch), origin)
	}, "top-level origin"},
	{"with the challenge twice", func(typ string, ch []byte, origin string) []byte {
		return fmt.Appendf(nil, `{"type":%q,"challenge":"AAAA","origin":%q,"challenge":%q}`, typ, origin, b64(ch))
	}, "duplicate"},
	{"with the challenge under another case", func(typ string, ch []byte, origin string) []byte {
		return fmt.Appendf(nil, `{"type":%q,"Challenge":%q,"origin":%q}`, typ, b64(ch), origin)
	}, "another challenge"},
	{"with invalid UTF-8", func(typ string, ch []byte, origin string) []byte {
		return fmt.Appendf(nil, `{"type":%q,"challenge":%q,"origin":%q,"x":"%s"}`, typ, b64(ch), origin, "\xff")
	}, "UTF-8"},
	{"larger than anything a browser writes", func(typ string, ch []byte, origin string) []byte {
		return fmt.Appendf(nil, `{"type":%q,"challenge":%q,"origin":%q,"x":"%s"}`, typ, b64(ch), origin, strings.Repeat("a", maxInput))
	}, "more than the"},
}

func TestARegistrationRefusesClientDataABrowserWouldNotSend(t *testing.T) {
	for _, c := range clientDataRefusals {
		t.Run(c.name, func(t *testing.T) {
			r := newAuthenticator(t, ES256).register(testCeremony(t))
			r.clientData = c.write(typeCreate, r.ceremony.Challenge, r.ceremony.Origin)
			_, err := r.verify()
			wantRefused(t, err, c.want)
		})
	}
}

func TestAnAssertionRefusesClientDataABrowserWouldNotSend(t *testing.T) {
	a, cred := registered(t, ES256)
	for _, c := range clientDataRefusals {
		t.Run(c.name, func(t *testing.T) {
			s := a.assert(testCeremony(t), cred)
			s.clientData = c.write(typeGet, s.ceremony.Challenge, s.ceremony.Origin)
			_, err := s.verify(t)
			wantRefused(t, err, c.want)
		})
	}
}

// Every check of a registration beyond the client data, each refusing the one thing it is there
// for.
func TestARegistrationRefusesWhatItIsThereToRefuse(t *testing.T) {
	for _, c := range []struct {
		name   string
		alg    Algorithm
		change func(*registration)
		want   string
	}{
		{"a ceremony with no Relying Party Identifier", ES256, func(r *registration) { r.ceremony.RPID = "" }, "names no Relying Party Identifier"},
		{"a ceremony with no origin", ES256, func(r *registration) { r.ceremony.Origin = "" }, "names no origin"},
		{"a ceremony with a short challenge", ES256, func(r *registration) {
			r.ceremony.Challenge = r.ceremony.Challenge[:15]
			r.clientData = clientDataJSON(typeCreate, r.ceremony.Challenge, r.ceremony.Origin)
		}, "too short"},

		{"an attestation object that is not CBOR", ES256, func(r *registration) {
			r.object = func([]byte) []byte { return []byte{0xff} }
		}, "attestation object cannot be read"},
		{"an attestation object with a byte after it", ES256, func(r *registration) {
			r.object = func(ad []byte) []byte {
				return append(enc(pairs{{"fmt", "none"}, {"attStmt", pairs{}}, {"authData", ad}}), 0)
			}
		}, "follow the item"},
		{"an attestation object that is an array", ES256, func(r *registration) {
			r.object = func(ad []byte) []byte { return enc([]any{"none", pairs{}, ad}) }
		}, "is not a map"},
		{"an attestation object with its format twice", ES256, func(r *registration) {
			r.object = func(ad []byte) []byte {
				return enc(pairs{{"fmt", "packed"}, {"fmt", "none"}, {"attStmt", pairs{}}, {"authData", ad}})
			}
		}, "appears twice"},
		{"no format", ES256, func(r *registration) { r.format = absent{} }, "names no format"},
		{"a format that is not text", ES256, func(r *registration) { r.format = 0 }, "names no format"},
		{"the format packed", ES256, func(r *registration) {
			r.format = "packed"
			r.statement = pairs{{"alg", -7}, {"sig", []byte{1}}}
		}, `the attestation format "packed" is not accepted`},
		{"the format None", ES256, func(r *registration) { r.format = "None" }, `the attestation format "None" is not accepted`},
		{"a none statement that is not empty", ES256, func(r *registration) { r.statement = pairs{{"sig", []byte{1}}} }, "empty map"},
		{"a none statement that is an array", ES256, func(r *registration) { r.statement = []any{} }, "empty map"},
		{"no statement", ES256, func(r *registration) { r.statement = absent{} }, "empty map"},
		{"no authenticator data", ES256, func(r *registration) {
			r.object = func([]byte) []byte { return enc(pairs{{"fmt", "none"}, {"attStmt", pairs{}}}) }
		}, "carries no authenticator data"},
		{"authenticator data that is text", ES256, func(r *registration) {
			r.object = func(ad []byte) []byte {
				return enc(pairs{{"fmt", "none"}, {"attStmt", pairs{}}, {"authData", "authData"}})
			}
		}, "carries no authenticator data"},

		{"authenticator data of 36 bytes", ES256, func(r *registration) {
			r.object = func(ad []byte) []byte {
				return enc(pairs{{"fmt", "none"}, {"attStmt", pairs{}}, {"authData", ad[:36]}})
			}
		}, "shorter than the 37"},
		{"another Relying Party", ES256, func(r *registration) { r.data.rpIDHash = sha256.Sum256([]byte("example.com")) }, "another Relying Party"},
		{"the Relying Party Identifier of the origin's parent", ES256, func(r *registration) { r.ceremony.RPID = "example.com" }, "another Relying Party"},
		{"no user present", ES256, func(r *registration) { r.data.flags &^= flagUP }, "user was present"},
		{"no user verified", ES256, func(r *registration) { r.data.flags &^= flagUV }, "did not verify the user"},
		{"backed up but not eligible", ES256, func(r *registration) { r.data.flags &^= flagBE }, "backed up but not eligible"},
		{"no credential", ES256, func(r *registration) { r.data.flags &^= flagAT }, "AT flag being clear"},
		{"a credential its flag does not announce", ES256, func(r *registration) {
			r.object = func(ad []byte) []byte {
				ad[32] &^= flagAT
				return enc(pairs{{"fmt", "none"}, {"attStmt", pairs{}}, {"authData", ad}})
			}
		}, "follow the authenticator data"},
		{"an empty credential ID", ES256, func(r *registration) { r.data.id = nil }, "credential ID is empty"},
		{"a credential ID of 1024 bytes", ES256, func(r *registration) { r.data.id = make([]byte, 1024) }, "longer than the 1023"},
		{"a credential ID longer than the data", ES256, func(r *registration) {
			r.data.idLength = 1000
			r.data.publicKey = nil
		}, "ends inside its credential ID"},
		{"attested credential data cut short", ES256, func(r *registration) {
			r.object = func(ad []byte) []byte {
				return enc(pairs{{"fmt", "none"}, {"attStmt", pairs{}}, {"authData", ad[:37+17]}})
			}
		}, "ends inside its attested credential data"},
		{"no public key", ES256, func(r *registration) { r.data.publicKey = nil }, "public key cannot be read"},
		{"a byte after the public key", ES256, func(r *registration) { r.data.trailing = []byte{0} }, "1 bytes follow the authenticator data"},
		{"extensions flagged and absent", ES256, func(r *registration) { r.data.flags |= flagED }, "extensions of the authenticator data cannot be read"},
		{"extensions that are not a map", ES256, func(r *registration) {
			r.data.flags |= flagED
			r.data.extensions = enc([]any{})
		}, "are not a map"},
		{"extensions with a duplicate", ES256, func(r *registration) {
			r.data.flags |= flagED
			r.data.extensions = enc(pairs{{"credProtect", 1}, {"credProtect", 3}})
		}, "appears twice"},
		{"a byte after the extensions", ES256, func(r *registration) {
			r.data.flags |= flagED
			r.data.extensions = enc(pairs{{"credProtect", 1}})
			r.data.trailing = []byte{0}
		}, "follow the authenticator data"},

		{"a public key that is not a map", ES256, func(r *registration) { r.data.publicKey = enc([]any{2, -7}) }, "not a map"},
		{"a public key with no algorithm", ES256, func(r *registration) {
			r.data.publicKey = enc(pairs{{labelKty, ktyEC2}, {labelCrv, crvP256}})
		}, "names no algorithm"},
		{"a public key with no key type", ES256, func(r *registration) {
			r.data.publicKey = enc(pairs{{labelAlg, ES256}, {labelCrv, crvP256}})
		}, "names no key type"},
		{"ES384, which is not verified", ES256, func(r *registration) {
			r.data.publicKey = enc(pairs{{labelKty, ktyEC2}, {labelAlg, -35}, {labelCrv, 2}})
		}, "algorithm -35, which is not one this installation verifies"},
		{"PS256, which is not verified", RS256, func(r *registration) { withLabel(r, labelAlg, -37) }, "algorithm -37, which is not one"},
		{"an ES256 key of key type OKP", ES256, func(r *registration) { withLabel(r, labelKty, ktyOKP) }, "an ES256 key is of key type 1"},
		{"an ES256 key on P-384", ES256, func(r *registration) { withLabel(r, labelCrv, 2) }, "not on the curve 1 (P-256)"},
		{"an ES256 key with no curve", ES256, func(r *registration) { withLabel(r, labelCrv, absent{}) }, "not on the curve 1 (P-256)"},
		{"an ES256 key with a short x", ES256, func(r *registration) { withLabel(r, labelX, make([]byte, 31)) }, "x coordinate"},
		{"an ES256 key with its y compressed", ES256, func(r *registration) { withLabel(r, labelY, true) }, "y coordinate"},
		{"an ES256 key off the curve", ES256, func(r *registration) {
			y := slices.Clone(labelValue(r, labelY).([]byte))
			y[31] ^= 1
			withLabel(r, labelY, y)
		}, "not a point on P-256"},
		{"an EdDSA key of key type EC2", EdDSA, func(r *registration) { withLabel(r, labelKty, ktyEC2) }, "an EdDSA key is of key type 2"},
		{"an EdDSA key on X25519", EdDSA, func(r *registration) { withLabel(r, labelCrv, 4) }, "not on the curve 6 (Ed25519)"},
		{"an EdDSA key of 31 bytes", EdDSA, func(r *registration) { withLabel(r, labelX, make([]byte, 31)) }, "not a byte string of 32 bytes"},
		{"an RS256 key of key type EC2", RS256, func(r *registration) { withLabel(r, labelKty, ktyEC2) }, "an RS256 key is of key type 2"},
		{"an RS256 key of 2047 bits", RS256, func(r *registration) {
			withLabel(r, labelN, append([]byte{0x7f}, make([]byte, 255)...))
		}, "RSA key of 2047 bits is refused"},
		{"an RS256 key of 8200 bits", RS256, func(r *registration) {
			withLabel(r, labelN, append([]byte{0xff}, make([]byte, 1024)...))
		}, "RSA key of 8200 bits is refused"},
		{"an RS256 modulus with a leading zero", RS256, func(r *registration) {
			withLabel(r, labelN, append([]byte{0}, labelValue(r, labelN).([]byte)...))
		}, "modulus is written with a leading zero"},
		{"an RS256 key with no exponent", RS256, func(r *registration) { withLabel(r, labelE, absent{}) }, "exponent is not a byte string"},
		{"an RS256 exponent with a leading zero", RS256, func(r *registration) { withLabel(r, labelE, []byte{0, 1, 0, 1}) }, "exponent is written with a leading zero"},
		{"an even RS256 exponent", RS256, func(r *registration) { withLabel(r, labelE, []byte{1, 0, 0}) }, "RSA exponent 65536 is refused"},
		{"an RS256 exponent of 1", RS256, func(r *registration) { withLabel(r, labelE, []byte{1}) }, "RSA exponent 1 is refused"},
		{"an RS256 exponent over 31 bits", RS256, func(r *registration) { withLabel(r, labelE, []byte{0x80, 0, 0, 1}) }, "RSA exponent 2147483649 is refused"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newAuthenticator(t, c.alg).register(testCeremony(t))
			c.change(r)
			_, err := r.verify()
			wantRefused(t, err, c.want)
		})
	}
}

// labelValue and withLabel read and change one parameter of the COSE key a registration carries.
func labelValue(r *registration, label int) any {
	v, err := decodeCBOR(r.data.publicKey)
	if err != nil {
		panic(err)
	}
	return v.(map[any]any)[int64(label)]
}

func withLabel(r *registration, label int, value any) {
	v, err := decodeCBOR(r.data.publicKey)
	if err != nil {
		panic(err)
	}
	var key pairs
	for _, l := range []int{labelKty, labelAlg, labelCrv, labelX, labelY} {
		if e, ok := v.(map[any]any)[int64(l)]; ok {
			key = append(key, pair{l, e})
		}
	}
	for i := range key {
		if key[i].k == label {
			key[i].v = value
			r.data.publicKey = enc(key)
			return
		}
	}
	r.data.publicKey = enc(append(key, pair{label, value}))
}

// A registration takes the longest credential ID there is.
func TestACredentialIDOf1023BytesIsTaken(t *testing.T) {
	r := newAuthenticator(t, ES256).register(testCeremony(t))
	r.data.id = bytes.Repeat([]byte{7}, 1023)
	cred, err := r.verify()
	if err != nil {
		t.Fatal(err)
	}
	if len(cred.ID) != 1023 {
		t.Fatalf("the credential ID came back %d bytes long", len(cred.ID))
	}
}

// Every check of an assertion beyond the client data, each refusing the one thing it is there for.
func TestAnAssertionRefusesWhatItIsThereToRefuse(t *testing.T) {
	for _, c := range []struct {
		name   string
		alg    Algorithm
		change func(*assertion)
		want   string
	}{
		{"a ceremony with a short challenge", ES256, func(s *assertion) {
			s.ceremony.Challenge = s.ceremony.Challenge[:8]
			s.clientData = clientDataJSON(typeGet, s.ceremony.Challenge, s.ceremony.Origin)
		}, "too short"},
		{"another credential's ID", ES256, func(s *assertion) { s.id[0] ^= 1 }, "another credential"},
		{"no credential ID", ES256, func(s *assertion) { s.id = nil }, "another credential"},
		{"a stored key that is not CBOR", ES256, func(s *assertion) { s.stored.PublicKey = []byte{0x5f} }, "stored public key cannot be read"},
		{"a stored key with a byte after it", ES256, func(s *assertion) {
			s.stored.PublicKey = append(slices.Clone(s.stored.PublicKey), 0)
		}, "stored public key cannot be read"},
		{"a stored key of an algorithm not verified", ES256, func(s *assertion) {
			s.stored.PublicKey = enc(pairs{{labelKty, ktyEC2}, {labelAlg, -35}})
		}, "not one this installation verifies"},

		{"authenticator data of 36 bytes", ES256, func(s *assertion) { s.cut = 36 }, "shorter than the 37"},
		{"authenticator data larger than anything an authenticator writes", ES256, func(s *assertion) {
			s.data.trailing = make([]byte, maxInput)
		}, "more than the 16384 read"},
		{"another Relying Party", ES256, func(s *assertion) { s.data.rpIDHash = sha256.Sum256([]byte("example.com")) }, "another Relying Party"},
		{"no user present", ES256, func(s *assertion) { s.data.flags &^= flagUP }, "user was present"},
		{"no user verified", ES256, func(s *assertion) { s.data.flags &^= flagUV }, "did not verify the user"},
		{"backed up but not eligible", ES256, func(s *assertion) {
			s.stored.BackupEligible = false
			s.data.flags &^= flagBE
		}, "backed up but not eligible"},
		{"a synced credential asserting as device-bound", ES256, func(s *assertion) { s.data.flags &^= flagBE | flagBS }, "registered synced and asserts as device-bound"},
		{"a device-bound credential asserting as synced", ES256, func(s *assertion) { s.stored.BackupEligible = false }, "registered device-bound and asserts as synced"},
		{"attested credential data", ES256, func(s *assertion) {
			s.data.flags |= flagAT
			s.data.id = s.stored.ID
			s.data.publicKey = s.stored.PublicKey
		}, "carries attested credential data"},
		{"a byte after the authenticator data", ES256, func(s *assertion) { s.data.trailing = []byte{0} }, "1 bytes follow the authenticator data"},
		{"extensions flagged and absent", ES256, func(s *assertion) { s.data.flags |= flagED }, "extensions of the authenticator data cannot be read"},

		{"an ES256 signature with a bit flipped", ES256, func(s *assertion) { s.tamper = flipLast }, "signature does not verify"},
		{"an EdDSA signature with a bit flipped", EdDSA, func(s *assertion) { s.tamper = flipLast }, "signature does not verify"},
		{"an RS256 signature with a bit flipped", RS256, func(s *assertion) { s.tamper = flipLast }, "signature does not verify"},
		{"an ES256 signature as r and s side by side", ES256, func(s *assertion) {
			s.tamper = func(sig []byte) []byte { return rawECDSA(sig) }
		}, "signature does not verify"},
		{"an ES256 signature with a byte after it", ES256, func(s *assertion) {
			s.tamper = func(sig []byte) []byte { return append(sig, 0) }
		}, "signature does not verify"},
		{"an EdDSA signature cut short", EdDSA, func(s *assertion) {
			s.tamper = func(sig []byte) []byte { return sig[:63] }
		}, "signature does not verify"},
		{"no signature", RS256, func(s *assertion) { s.tamper = func([]byte) []byte { return nil } }, "signature does not verify"},
		{"a signature by another key", ES256, func(s *assertion) { s.key = otherES256 }, "signature does not verify"},
		{"a signature over the client data itself", EdDSA, func(s *assertion) {
			s.signOver = func(ad, cd []byte) []byte { return slices.Concat(ad, cd) }
		}, "signature does not verify"},
		{"a signature over the authenticator data alone", EdDSA, func(s *assertion) {
			s.signOver = func(ad, _ []byte) []byte { return ad }
		}, "signature does not verify"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, cred := registered(t, c.alg)
			s := a.assert(testCeremony(t), cred)
			c.change(s)
			_, err := s.verify(t)
			wantRefused(t, err, c.want)
		})
	}
}

func flipLast(sig []byte) []byte {
	sig[len(sig)-1] ^= 1
	return sig
}

// otherES256 is a key no credential of these tests was registered with.
var otherES256 = func() *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return k
}()

// rawECDSA rewrites a DER signature as r and s of 32 bytes each, the form COSE uses and §6.5.5
// does not: the same signature, spelled as WebAuthn does not spell it.
func rawECDSA(der []byte) []byte {
	var sig struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(der, &sig); err != nil {
		panic(err)
	}
	return append(sig.R.FillBytes(make([]byte, 32)), sig.S.FillBytes(make([]byte, 32))...)
}
