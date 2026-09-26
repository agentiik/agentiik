package webauthn

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/big"
	"slices"
	"sync"
	"testing"
)

// A software authenticator, made of the standard library, which is what the known answers of these
// tests come from: it makes a key, writes the attestation object and the authenticator data an
// authenticator writes, and signs what an authenticator signs. Everything it writes is built from
// parts a test can change one at a time, which is how every check is shown to refuse the one thing
// it is there for.

// The Relying Party of the tests, and a challenge of the length the API issues.
const (
	testRPID   = "agentiik.example.com"
	testOrigin = "https://agentiik.example.com"
)

func testChallenge(t testing.TB) []byte {
	t.Helper()
	c := make([]byte, 32)
	if _, err := rand.Read(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func testCeremony(t testing.TB) Ceremony {
	return Ceremony{RPID: testRPID, Origin: testOrigin, Challenge: testChallenge(t), RequireUserVerification: true}
}

// --- CBOR, written ---

// pairs is a CBOR map written in the order given, duplicates and all, which a Go map cannot hold.
type pairs []pair

type pair struct{ k, v any }

// absent leaves an entry of pairs out, so a test can drop one member of an attestation object.
type absent struct{}

// enc writes v as an authenticator does: every head in its shortest form, definite lengths, the
// entries of pairs in their order and those of a Go map sorted by their encoding.
func enc(v any) []byte {
	switch v := v.(type) {
	case int:
		return encInt(int64(v))
	case int64:
		return encInt(v)
	case Algorithm:
		return encInt(int64(v))
	case bool:
		if v {
			return []byte{0xf5}
		}
		return []byte{0xf4}
	case []byte:
		return append(encHead(majorBytes, uint64(len(v))), v...)
	case string:
		return append(encHead(majorText, uint64(len(v))), v...)
	case []any:
		b := encHead(majorArray, uint64(len(v)))
		for _, e := range v {
			b = append(b, enc(e)...)
		}
		return b
	case pairs:
		var body []byte
		n := 0
		for _, p := range v {
			if _, skip := p.v.(absent); skip {
				continue
			}
			body = append(append(body, enc(p.k)...), enc(p.v)...)
			n++
		}
		return append(encHead(majorMap, uint64(n)), body...)
	case map[any]any:
		var entries [][2][]byte
		for k, e := range v {
			entries = append(entries, [2][]byte{enc(k), enc(e)})
		}
		slices.SortFunc(entries, func(a, b [2][]byte) int { return bytes.Compare(a[0], b[0]) })
		b := encHead(majorMap, uint64(len(entries)))
		for _, e := range entries {
			b = append(append(b, e[0]...), e[1]...)
		}
		return b
	}
	panic(fmt.Sprintf("enc: %T", v))
}

func encInt(n int64) []byte {
	if n < 0 {
		return encHead(majorNeg, uint64(-1-n))
	}
	return encHead(majorUint, uint64(n))
}

func encHead(major byte, n uint64) []byte {
	m := major << 5
	switch {
	case n < 24:
		return []byte{m | byte(n)}
	case n <= 0xff:
		return []byte{m | 24, byte(n)}
	case n <= 0xffff:
		return binary.BigEndian.AppendUint16([]byte{m | 25}, uint16(n))
	case n <= 0xffffffff:
		return binary.BigEndian.AppendUint32([]byte{m | 26}, uint32(n))
	}
	return binary.BigEndian.AppendUint64([]byte{m | 27}, n)
}

// --- keys ---

// A 2048-bit RSA key takes a moment to make, so the tests share one.
var rsaKey = sync.OnceValues(func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, 2048) })

// newKey makes a key for alg, and says what an authenticator writes for its public half.
func newKey(t testing.TB, alg Algorithm) (crypto.Signer, pairs) {
	t.Helper()
	switch alg {
	case ES256:
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		point, err := k.PublicKey.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		return k, pairs{{labelKty, ktyEC2}, {labelAlg, ES256}, {labelCrv, crvP256}, {labelX, point[1:33]}, {labelY, point[33:]}}
	case EdDSA:
		pub, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k, pairs{{labelKty, ktyOKP}, {labelAlg, EdDSA}, {labelCrv, crvEd25519}, {labelX, []byte(pub)}}
	case RS256:
		k, err := rsaKey()
		if err != nil {
			t.Fatal(err)
		}
		return k, pairs{{labelKty, ktyRSA}, {labelAlg, RS256}, {labelN, k.N.Bytes()}, {labelE, big.NewInt(int64(k.E)).Bytes()}}
	}
	t.Fatalf("no key for %s", alg)
	return nil, nil
}

// sign signs as an authenticator does, in the format §6.5.5 gives the algorithm.
func sign(t testing.TB, k crypto.Signer, alg Algorithm, message []byte) []byte {
	t.Helper()
	var sig []byte
	var err error
	switch alg {
	case ES256:
		digest := sha256.Sum256(message)
		sig, err = ecdsa.SignASN1(rand.Reader, k.(*ecdsa.PrivateKey), digest[:])
	case EdDSA:
		sig = ed25519.Sign(k.(ed25519.PrivateKey), message)
	case RS256:
		digest := sha256.Sum256(message)
		sig, err = rsa.SignPKCS1v15(nil, k.(*rsa.PrivateKey), crypto.SHA256, digest[:])
	}
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// --- what the browser and the authenticator send ---

// clientDataJSON is the client data as a browser serializes it, §5.8.1.1.
func clientDataJSON(typ string, challenge []byte, origin string) []byte {
	return fmt.Appendf(nil, `{"type":%q,"challenge":%q,"origin":%q,"crossOrigin":false}`, typ, base64.RawURLEncoding.EncodeToString(challenge), origin)
}

// authData is authenticator data in parts, §6.1.
type authData struct {
	rpIDHash   [32]byte
	flags      byte
	signCount  uint32
	aaguid     [16]byte
	id         []byte // written where the AT flag is set
	publicKey  []byte
	idLength   int    // written in place of len(id) where it is not zero
	extensions []byte // written where the ED flag is set
	trailing   []byte
}

func (a authData) bytes() []byte {
	b := slices.Concat(a.rpIDHash[:], []byte{a.flags})
	b = binary.BigEndian.AppendUint32(b, a.signCount)
	if a.flags&flagAT != 0 {
		n := len(a.id)
		if a.idLength != 0 {
			n = a.idLength
		}
		b = append(b, a.aaguid[:]...)
		b = binary.BigEndian.AppendUint16(b, uint16(n))
		b = slices.Concat(b, a.id, a.publicKey)
	}
	if a.flags&flagED != 0 {
		b = append(b, a.extensions...)
	}
	return append(b, a.trailing...)
}

// authenticator is one credential on a software authenticator.
type authenticator struct {
	alg    Algorithm
	key    crypto.Signer
	cose   pairs
	id     []byte
	aaguid [16]byte
	count  uint32
	flags  byte // UV, BE and BS as the authenticator sets them
}

func newAuthenticator(t testing.TB, alg Algorithm) *authenticator {
	t.Helper()
	k, cose := newKey(t, alg)
	a := &authenticator{alg: alg, key: k, cose: cose, id: make([]byte, 32), flags: flagUV | flagBE | flagBS}
	if _, err := rand.Read(a.id); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(a.aaguid[:]); err != nil {
		t.Fatal(err)
	}
	return a
}

// registration is a registration in parts, as the authenticator makes it for a ceremony.
type registration struct {
	ceremony   Ceremony
	clientData []byte
	format     any
	statement  any
	data       authData

	// object, where it is set, writes the attestation object in place of the parts above.
	object func(authData []byte) []byte
}

func (a *authenticator) register(c Ceremony) *registration {
	return &registration{
		ceremony:   c,
		clientData: clientDataJSON(typeCreate, c.Challenge, c.Origin),
		format:     "none",
		statement:  pairs{},
		data: authData{
			rpIDHash:  sha256.Sum256([]byte(c.RPID)),
			flags:     flagUP | flagAT | a.flags,
			signCount: a.count,
			aaguid:    a.aaguid,
			id:        slices.Clone(a.id),
			publicKey: enc(a.cose),
		},
	}
}

func (r *registration) attestationObject() []byte {
	ad := r.data.bytes()
	if r.object != nil {
		return r.object(ad)
	}
	return enc(pairs{{"fmt", r.format}, {"attStmt", r.statement}, {"authData", ad}})
}

func (r *registration) verify() (Credential, error) {
	return VerifyRegistration(r.ceremony, Registration{ClientDataJSON: r.clientData, AttestationObject: r.attestationObject()})
}

// assertion is an assertion in parts, signed when it is verified, so that what is refused is the
// part a test changed and never a signature over something else.
type assertion struct {
	ceremony   Ceremony
	stored     Credential
	id         []byte
	clientData []byte
	data       authData
	key        crypto.Signer
	alg        Algorithm

	// cut, where it is not zero, is the length the authenticator data is cut to before it is
	// signed and sent.
	cut int

	// signOver, where it is set, says what is signed in place of the authenticator data followed
	// by the SHA-256 of the client data.
	signOver func(authData, clientData []byte) []byte

	// tamper, where it is set, changes the signature once it is made.
	tamper func(sig []byte) []byte
}

func (a *authenticator) assert(c Ceremony, stored Credential) *assertion {
	a.count++
	return &assertion{
		ceremony:   c,
		stored:     stored,
		id:         slices.Clone(a.id),
		clientData: clientDataJSON(typeGet, c.Challenge, c.Origin),
		data: authData{
			rpIDHash:  sha256.Sum256([]byte(c.RPID)),
			flags:     flagUP | a.flags,
			signCount: a.count,
		},
		key: a.key,
		alg: a.alg,
	}
}

func (s *assertion) verify(t *testing.T) (Credential, error) {
	t.Helper()
	ad := s.data.bytes()
	if s.cut != 0 {
		ad = ad[:s.cut]
	}
	hash := sha256.Sum256(s.clientData)
	signed := slices.Concat(ad, hash[:])
	if s.signOver != nil {
		signed = s.signOver(ad, s.clientData)
	}
	sig := sign(t, s.key, s.alg, signed)
	if s.tamper != nil {
		sig = s.tamper(sig)
	}
	return VerifyAssertion(s.ceremony, s.stored, Assertion{CredentialID: s.id, ClientDataJSON: s.clientData, AuthenticatorData: ad, Signature: sig})
}

// registered is a credential registered with its authenticator, ready to assert with.
func registered(t *testing.T, alg Algorithm) (*authenticator, Credential) {
	t.Helper()
	a := newAuthenticator(t, alg)
	cred, err := a.register(testCeremony(t)).verify()
	if err != nil {
		t.Fatalf("registering a %s credential: %s", alg, err)
	}
	return a, cred
}
