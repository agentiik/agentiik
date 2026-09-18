package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"
)

// Sealing a value, which is envelope encryption written out.
//
// "Envelope encryption with AES-256-GCM data keys, wrapped by a master key held outside the
// database." One data key per secret, a fresh one on every write, and the master key wraps it.
//
// What the page does not say, and what this decides. Each of these was argued over: the first
// version of this file got four of them wrong, and the exploits that found them are the tests
// below.
//
//   - A data key is fresh on every write, never reused across values and never reused across
//     writes of one value. A key used once has no nonce reuse to worry about, which turns a
//     collision from unlikely into impossible.
//   - The master key wraps through a key derived per write rather than directly. The same
//     argument has to hold for the wrapping layer, and it did not: one long-lived GCM key with
//     random nonces is bounded by the birthday paradox, and a repeat there leaks the GHASH
//     subkey and lets anybody who can read the table forge wrapped keys. A salt per write and
//     HKDF over it makes the wrapping key single use as well, for the cost of one column.
//   - The additional data is length-prefixed framing over raw bytes rather than a JSON
//     document. JSON was chosen first because it looked unambiguous, and it is not: Go's
//     encoder replaces every invalid UTF-8 byte with U+FFFD, so two secrets whose names differ
//     only in an invalid byte bind identically and one ciphertext opens as the other. Framing
//     is injective over arbitrary bytes, and a name that is not valid UTF-8 is refused anyway.
//   - The binding carries the master key's identifier, the namespace, the name and the
//     version. The version comes from the caller on the way in and on the way out, never out
//     of the record being authenticated: reading it from the record made it authenticate
//     nothing, and a whole row copied back from a backup undid a rotation silently.
//   - Rotation is a write, so there is no unwrap-and-rewrap here. A new master key is a second
//     key beside the first, values keep opening under whichever sealed them, and a value moves
//     when it is next written.

// domain is what everything sealed by this version of this package is bound under. A second
// version of the format changes it, and everything sealed under the first stops opening under
// the second by construction rather than by a check somebody could forget.
const domain = "agentiik.secret.v1"

// saltLength is how much salt a wrapping key is derived over. Thirty-two bytes, so that two
// writes deriving one wrapping key is as unlikely as two writes sharing a data key.
const saltLength = 32

// maxValue is the largest secret this will seal.
//
// A megabyte, which is far more than a credential and far less than the point where GCM has
// anything to say about it. A bound exists so that a caller handing this a file by accident is
// refused rather than served.
const maxValue = 1 << 20

// Sealed is a value as the database holds it. Every field of it is safe to store, to log and to
// back up: none of them opens anything without the master key, which is not here.
type Sealed struct {
	// Master names the key that wrapped the data key, so that a store holding values from
	// two generations can open both and a value naming a key nobody has says so.
	Master string `json:"master"`

	// Version is the write this is, kept beside the ciphertext so that a row can be read
	// without the store having to remember. It is not what the binding uses: the version
	// the binding uses arrives from the caller, because a number that travels with the
	// bytes it is meant to authenticate authenticates nothing.
	Version int `json:"version"`

	// Salt is what the wrapping key for this one write was derived over.
	Salt []byte `json:"salt"`

	// Key is the data key, wrapped. WrapNonce is what wrapped it.
	Key       []byte `json:"key"`
	WrapNonce []byte `json:"wrap_nonce"`

	// Value is the ciphertext and Nonce is what sealed it.
	Value []byte `json:"value"`
	Nonce []byte `json:"nonce"`
}

// Seal encrypts one value for one secret of one namespace, at one version.
//
// The version is the caller's: it comes from the row this will be written to, and the store is
// what allocates it. Sealing the same version twice is not refused here and cannot be, since
// this package holds no rows; a store that let two writes share a version would be a store whose
// rollback protection had a gap, and that is a uniqueness the database is there to keep.
func (m *Master) Seal(namespace, name string, version int, value []byte) (Sealed, error) {
	if err := check(m, namespace, name, version); err != nil {
		return Sealed{}, err
	}
	if len(value) > maxValue {
		return Sealed{}, fmt.Errorf("secret: %s/%s is %d bytes and a secret is at most %d: a value that size is a file, and a file is an artifact", namespace, name, len(value), maxValue)
	}

	// The data key, used for this write and never again.
	data, err := randomBytes(KeyLength)
	if err != nil {
		return Sealed{}, err
	}
	defer wipe(data)

	salt, err := randomBytes(saltLength)
	if err != nil {
		return Sealed{}, err
	}
	aad := bind(m.id, namespace, name, version)

	outer, err := m.wrapping(salt, aad)
	if err != nil {
		return Sealed{}, err
	}
	inner, err := gcm(data)
	if err != nil {
		return Sealed{}, fmt.Errorf("secret: the data key could not be used: %w", err)
	}
	nonce, err := randomBytes(inner.NonceSize())
	if err != nil {
		return Sealed{}, err
	}
	wrapNonce, err := randomBytes(outer.NonceSize())
	if err != nil {
		return Sealed{}, err
	}

	return Sealed{
		Master: m.id, Version: version, Salt: salt,
		Nonce: nonce, WrapNonce: wrapNonce,
		Value: inner.Seal(nil, nonce, value, aad),
		Key:   outer.Seal(nil, wrapNonce, data, aad),
	}, nil
}

// Open decrypts one value, and refuses one that does not belong where it was found.
//
// Every part of the binding is the caller's own knowledge of the row it read: the namespace, the
// name and the version. None of it comes out of the record, which is the difference between an
// additional data that authenticates and one that agrees with whatever it is given. A ciphertext
// moved into another secret's row, or a row restored from before a rotation, fails to open.
func (m *Master) Open(namespace, name string, version int, s Sealed) ([]byte, error) {
	if err := check(m, namespace, name, version); err != nil {
		return nil, err
	}
	if s.Master != m.id {
		return nil, fmt.Errorf("%w: it names %q and this one is %q", ErrNotMine, s.Master, m.id)
	}
	// Refused before any cipher runs, so that a row whose stored version disagrees with the
	// row it was found in is a fault somebody reads rather than a decryption failure they
	// have to work out.
	if s.Version != version {
		return nil, fmt.Errorf("secret: %s/%s holds version %d and the row it was read from is version %d", namespace, name, s.Version, version)
	}
	if len(s.Salt) != saltLength {
		return nil, fmt.Errorf("secret: the salt of %s/%s is %d bytes and this format uses %d", namespace, name, len(s.Salt), saltLength)
	}

	aad := bind(m.id, namespace, name, version)
	outer, err := m.wrapping(s.Salt, aad)
	if err != nil {
		return nil, err
	}
	// Checked rather than passed through, because GCM panics on a nonce of the wrong length
	// rather than refusing it, and one truncated column would take the process down on every
	// request that touched that secret.
	if len(s.WrapNonce) != outer.NonceSize() {
		return nil, fmt.Errorf("secret: the wrapping nonce of %s/%s is %d bytes and GCM takes %d", namespace, name, len(s.WrapNonce), outer.NonceSize())
	}
	data, err := outer.Open(nil, s.WrapNonce, s.Key, aad)
	if err != nil {
		return nil, fmt.Errorf("secret: the data key of %s/%s could not be unwrapped: %w", namespace, name, err)
	}
	defer wipe(data)

	inner, err := gcm(data)
	if err != nil {
		return nil, fmt.Errorf("secret: the data key could not be used: %w", err)
	}
	if len(s.Nonce) != inner.NonceSize() {
		return nil, fmt.Errorf("secret: the nonce of %s/%s is %d bytes and GCM takes %d", namespace, name, len(s.Nonce), inner.NonceSize())
	}
	value, err := inner.Open(nil, s.Nonce, s.Value, aad)
	if err != nil {
		return nil, fmt.Errorf("secret: %s/%s could not be opened: %w", namespace, name, err)
	}
	return value, nil
}

// wrapping derives the key that wraps one data key, for one write.
//
// The master key is never used as a GCM key itself. It wraps every data key of every secret of
// every namespace for as long as an installation holds it, and one long-lived GCM key with random
// nonces is bounded by the birthday paradox: a repeat leaks the GHASH subkey, and this package's
// own threat model assumes a reader of the table. A salt per write makes the wrapping key single
// use, so the argument that holds for the data key holds here too.
//
// The binding goes into the derivation as well as into the seal, so a wrapping key derived for
// one secret cannot be used for another even if a salt were somehow repeated.
func (m *Master) wrapping(salt, aad []byte) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, m.key, salt, string(aad), KeyLength)
	if err != nil {
		return nil, fmt.Errorf("secret: the wrapping key could not be derived: %w", err)
	}
	defer wipe(key)
	aead, err := gcm(key)
	if err != nil {
		return nil, fmt.Errorf("secret: the wrapping key could not be used: %w", err)
	}
	return aead, nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// check is what both halves refuse before anything else happens.
func check(m *Master, namespace, name string, version int) error {
	switch {
	case m == nil:
		return errors.New("secret: no master key")
	case namespace == "" || name == "":
		return errors.New("secret: a value has to belong to a namespace and a name, which is what binds it to where it lives")
	case !utf8.ValidString(namespace) || !utf8.ValidString(name):
		// The framing below is injective over arbitrary bytes, so this is belt on top of
		// braces. It is here because a name that is not text is a name somebody is doing
		// something with, and refusing it early says so.
		return errors.New("secret: a namespace and a name are text")
	case version < 1:
		return fmt.Errorf("secret: version %d, and a written value is the first", version)
	}
	return nil
}

// bind is the additional data: which key, which namespace, which secret and which write.
//
// Length-prefixed framing rather than a joined string or a JSON document. A separator somebody
// can put in a name is not a separator, and JSON is not a fix for that: Go's encoder replaces
// every invalid UTF-8 byte with U+FFFD, so two names differing only in an invalid byte encode
// identically and one ciphertext opens as the other. That was the first version of this
// function, and it was broken exactly that way.
func bind(master, namespace, name string, version int) []byte {
	var aad []byte
	for _, part := range []string{domain, master, namespace, name} {
		aad = binary.BigEndian.AppendUint64(aad, uint64(len(part)))
		aad = append(aad, part...)
	}
	return binary.BigEndian.AppendUint64(aad, uint64(version))
}

// randomBytes is the one place key material comes from.
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("secret: the system would not give us %d random bytes: %w", n, err)
	}
	return b, nil
}

// wipe clears key material the moment it is finished with.
//
// It is worth being honest about what this buys, which is less than it looks. Go moves values,
// so a copy may survive elsewhere, and nothing here can reach a copy the runtime made. What it
// does is shorten the window in which the live one sits in a heap that might be dumped.
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
