package webauthn

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"fmt"
	"math/big"
	"slices"
)

// Algorithm is a COSE algorithm identifier (§5.8.5), the number a credential public key names its
// signature scheme by.
type Algorithm int64

// The three algorithms this package verifies. Between them they cover the passkeys people hold:
// ES256 is what nearly every authenticator makes, EdDSA what some security keys offer, and RS256
// what Windows Hello has made on many machines.
const (
	ES256 Algorithm = -7
	EdDSA Algorithm = -8
	RS256 Algorithm = -257
)

// Algorithms are the algorithms a registration's options offer as pubKeyCredParams, in order of
// preference. They are exactly the ones this package verifies, which is what makes the check of
// §7.1 step 20, that the new key names one of the offered algorithms, the check that it names one
// of these.
func Algorithms() []Algorithm { return []Algorithm{ES256, EdDSA, RS256} }

func (a Algorithm) String() string {
	switch a {
	case ES256:
		return "ES256"
	case EdDSA:
		return "EdDSA"
	case RS256:
		return "RS256"
	}
	return fmt.Sprintf("algorithm %d", int64(a))
}

// COSE_Key labels and values: RFC 9052 §7.1 for the common ones, RFC 9053 §7.1 and §7.2 for EC2
// and OKP keys, RFC 8230 §4 for RSA.
const (
	labelKty = 1
	labelAlg = 3
	labelCrv = -1
	labelX   = -2
	labelY   = -3
	labelN   = -1
	labelE   = -2

	ktyOKP = 1
	ktyEC2 = 2
	ktyRSA = 3

	crvP256    = 1
	crvEd25519 = 6
)

// The sizes of RSA modulus accepted: 2048 bits at least, the smallest still held to be secure, and
// 8192 at most, which bounds what verifying an assertion costs the server with a key its holder
// chose, at twice the largest modulus in common use.
const (
	minRSABits = 2048
	maxRSABits = 8192
)

// publicKey is a credential public key, read and checked, ready to verify with.
type publicKey struct {
	alg     Algorithm
	ecdsa   *ecdsa.PublicKey
	ed25519 ed25519.PublicKey
	rsa     *rsa.PublicKey
}

// parsePublicKey reads a COSE_Key (§6.5.1), which names its algorithm and carries the parameters
// that algorithm's key type requires (§5.8.5). A label the key does not need is not read: §6.5.1
// says an authenticator writes none, and one it writes changes nothing that is verified.
func parsePublicKey(v any) (publicKey, error) {
	m, ok := v.(map[any]any)
	if !ok {
		return publicKey{}, fmt.Errorf("webauthn: the credential public key is not a map")
	}
	alg, ok := m[int64(labelAlg)].(int64)
	if !ok {
		return publicKey{}, fmt.Errorf("webauthn: the credential public key names no algorithm")
	}
	kty, ok := m[int64(labelKty)].(int64)
	if !ok {
		return publicKey{}, fmt.Errorf("webauthn: the credential public key names no key type")
	}
	k := publicKey{alg: Algorithm(alg)}
	switch k.alg {
	case ES256:
		if kty != ktyEC2 {
			return publicKey{}, fmt.Errorf("webauthn: an ES256 key is of key type %d, where it is %d (EC2)", kty, ktyEC2)
		}
		if crv, ok := m[int64(labelCrv)].(int64); !ok || crv != crvP256 {
			return publicKey{}, fmt.Errorf("webauthn: an ES256 key is not on the curve %d (P-256)", crvP256)
		}
		// Each coordinate is a byte string of the curve's size. A y written as a boolean is the
		// compressed form, which §5.8.5 forbids for ES256.
		point := []byte{4}
		for _, c := range []struct {
			name  string
			label int64
		}{{"x", labelX}, {"y", labelY}} {
			b, ok := m[c.label].([]byte)
			if !ok || len(b) != 32 {
				return publicKey{}, fmt.Errorf("webauthn: the %s coordinate of an ES256 key is not a byte string of 32 bytes", c.name)
			}
			point = append(point, b...)
		}
		// The point is checked to be on the curve here, the check §5.8.5 singles out as the one
		// that falls between a cryptographic library and the code around it.
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
		if err != nil {
			return publicKey{}, fmt.Errorf("webauthn: the ES256 key is not a point on P-256: %w", err)
		}
		k.ecdsa = pub
	case EdDSA:
		if kty != ktyOKP {
			return publicKey{}, fmt.Errorf("webauthn: an EdDSA key is of key type %d, where it is %d (OKP)", kty, ktyOKP)
		}
		if crv, ok := m[int64(labelCrv)].(int64); !ok || crv != crvEd25519 {
			return publicKey{}, fmt.Errorf("webauthn: an EdDSA key is not on the curve %d (Ed25519)", crvEd25519)
		}
		x, ok := m[int64(labelX)].([]byte)
		if !ok || len(x) != ed25519.PublicKeySize {
			return publicKey{}, fmt.Errorf("webauthn: an EdDSA key is not a byte string of %d bytes", ed25519.PublicKeySize)
		}
		if err := checkEd25519(x); err != nil {
			return publicKey{}, err
		}
		k.ed25519 = ed25519.PublicKey(x)
	case RS256:
		if kty != ktyRSA {
			return publicKey{}, fmt.Errorf("webauthn: an RS256 key is of key type %d, where it is %d (RSA)", kty, ktyRSA)
		}
		n, err := unsigned(m, labelN, "modulus")
		if err != nil {
			return publicKey{}, err
		}
		if bits := n.BitLen(); bits < minRSABits || bits > maxRSABits {
			return publicKey{}, fmt.Errorf("webauthn: an RSA key of %d bits is refused: one is %d bits at least and %d at most", bits, minRSABits, maxRSABits)
		}
		// A modulus is the product of two odd primes. crypto/rsa refuses an even one when it
		// verifies, and refusing it here keeps it from being stored.
		if n.Bit(0) == 0 {
			return publicKey{}, fmt.Errorf("webauthn: the RSA modulus is even, which no RSA key's is")
		}
		e, err := unsigned(m, labelE, "exponent")
		if err != nil {
			return publicKey{}, err
		}
		// The exponent crypto/rsa verifies with: odd, at least 3, and within 31 bits. Refusing
		// another here keeps a registration from storing a key no assertion could ever pass.
		if !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 || e.Bit(0) == 0 {
			return publicKey{}, fmt.Errorf("webauthn: the RSA exponent %s is refused: it is odd, 3 at least, and within 31 bits", e)
		}
		k.rsa = &rsa.PublicKey{N: n, E: int(e.Int64())}
	default:
		return publicKey{}, fmt.Errorf("webauthn: the credential public key is for %s, which is not one this installation verifies (ES256, EdDSA or RS256)", k.alg)
	}
	return k, nil
}

// Ed25519's field prime 2^255 - 19, its constant d = -121665/121666 (RFC 8032 §5.1), and the y
// of the points of order 8.
var (
	ed25519P  = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	ed25519D  = new(big.Int).Mod(new(big.Int).Mul(big.NewInt(-121665), new(big.Int).ModInverse(big.NewInt(121666), ed25519P)), ed25519P)
	ed25519Y8 = func() *big.Int {
		y, _ := new(big.Int).SetString("2707385501144840649318225287225658788936804267575313519463743609750303402022", 10)
		return y
	}()
)

// checkEd25519 refuses an Ed25519 public key that is not a point of the curve, or is one of the
// eight points of small order.
//
// crypto/ed25519 checks neither before it verifies. A key that is no point fails every signature,
// so a registration would store a credential nobody could ever use. A key of small order is worse:
// a signature made with no private key at all verifies against it, for any message, so once such a
// key is stored anybody who knows the credential ID signs in with it. Only whoever answered the
// registration chooses the key, but that is an authenticator, a client or an extension, and none
// of them is the user's to vouch for.
//
// The key is decoded as RFC 8032 §5.1.3 does: y little-endian with the sign of x in the top bit,
// refused where it is not below p, and x recovered from x² = (y² - 1) / (d·y² + 1), which must be
// a square. The eight points of small order have y 1, p - 1, 0, y8 or p - y8; x is zero only for
// y 1 and p - 1, which covers the step that refuses x zero with its sign bit set.
func checkEd25519(key []byte) error {
	le := slices.Clone(key)
	le[31] &= 0x7f
	slices.Reverse(le)
	y := new(big.Int).SetBytes(le)
	p := ed25519P
	if y.Cmp(p) >= 0 {
		return fmt.Errorf("webauthn: the EdDSA key is not written in its canonical form")
	}
	for _, small := range []*big.Int{big.NewInt(0), big.NewInt(1), new(big.Int).Sub(p, big.NewInt(1)), ed25519Y8, new(big.Int).Sub(p, ed25519Y8)} {
		if y.Cmp(small) == 0 {
			return fmt.Errorf("webauthn: the EdDSA key is a point of small order, against which a signature verifies with no private key")
		}
	}
	y2 := new(big.Int).Mul(y, y)
	u := new(big.Int).Sub(y2, big.NewInt(1))
	v := new(big.Int).Add(new(big.Int).Mul(ed25519D, y2), big.NewInt(1))
	v.Mod(v, p)
	// d·y² + 1 is never zero: -1/d is not a square modulo p.
	x2 := u.Mul(u, v.ModInverse(v, p))
	x2.Mod(x2, p)
	if big.Jacobi(x2, p) != 1 {
		return fmt.Errorf("webauthn: the EdDSA key is not a point on Ed25519")
	}
	return nil
}

// unsigned reads an RSA parameter, a byte string holding an unsigned big-endian integer in the
// fewest bytes that hold it (RFC 8230 §4). A leading zero byte is a second spelling of the same
// key.
func unsigned(m map[any]any, label int64, name string) (*big.Int, error) {
	b, ok := m[label].([]byte)
	if !ok || len(b) == 0 {
		return nil, fmt.Errorf("webauthn: the RSA %s is not a byte string", name)
	}
	if b[0] == 0 {
		return nil, fmt.Errorf("webauthn: the RSA %s is written with a leading zero byte", name)
	}
	return new(big.Int).SetBytes(b), nil
}

// verify checks sig over signed, in the signature format §6.5.5 gives each algorithm: an ASN.1 DER
// Ecdsa-Sig-Value for ES256, the 64 bytes of RFC 8032 for EdDSA, and RSASSA-PKCS1-v1_5 with
// SHA-256, not ASN.1 wrapped, for RS256.
func (k publicKey) verify(signed, sig []byte) error {
	var ok bool
	switch k.alg {
	case ES256:
		digest := sha256.Sum256(signed)
		ok = ecdsa.VerifyASN1(k.ecdsa, digest[:], sig)
	case EdDSA:
		ok = ed25519.Verify(k.ed25519, signed, sig)
	case RS256:
		digest := sha256.Sum256(signed)
		ok = rsa.VerifyPKCS1v15(k.rsa, crypto.SHA256, digest[:], sig) == nil
	}
	if !ok {
		return fmt.Errorf("webauthn: the signature does not verify with the credential's %s public key", k.alg)
	}
	return nil
}
