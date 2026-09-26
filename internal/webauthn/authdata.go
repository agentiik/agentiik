package webauthn

import (
	"encoding/binary"
	"fmt"
)

// The flags of authenticator data, §6.1. Bits 1 and 5 are reserved and not read, so that a later
// level assigning them refuses no authenticator here.
const (
	flagUP byte = 1 << 0 // the user was present
	flagUV byte = 1 << 2 // the user was verified
	flagBE byte = 1 << 3 // the credential is eligible for backup, which is to say synced
	flagBS byte = 1 << 4 // the credential is backed up now
	flagAT byte = 1 << 6 // attested credential data follows
	flagED byte = 1 << 7 // extensions follow
)

// maxCredentialID is the longest credential ID there is (§6.5.1), and the longest a registration
// accepts (§7.1 step 25).
const maxCredentialID = 1023

// authenticatorData is authenticator data (§6.1), read.
type authenticatorData struct {
	rpIDHash  [32]byte
	flags     byte
	signCount uint32

	// attested is the attested credential data (§6.5.1), which follows where AT is set.
	attested *attestedCredential

	// extensions are the extension outputs as the authenticator wrote them, a CBOR map read to
	// know where it ends and not interpreted: this installation asks for no extension, and §7.1
	// step 28 lets a Relying Party ignore any it did not.
	extensions []byte
}

// attestedCredential is the attested credential data of a registration, §6.5.1.
type attestedCredential struct {
	aaguid [16]byte
	id     []byte

	// publicKey is the COSE_Key as the authenticator wrote it, which is what is stored, and key is
	// what it says, read.
	publicKey []byte
	key       publicKey
}

// parseAuthenticatorData reads authenticator data (§6.1): 37 bytes, the attested credential data
// where AT is set, the extensions where ED is set, and nothing after them. The data describes its
// own length, so a byte past what it describes is one two readers could disagree about, and is
// refused.
func parseAuthenticatorData(b []byte) (authenticatorData, error) {
	if len(b) > maxInput {
		return authenticatorData{}, fmt.Errorf("webauthn: the authenticator data is %d bytes, more than the %d read", len(b), maxInput)
	}
	if len(b) < 37 {
		return authenticatorData{}, fmt.Errorf("webauthn: the authenticator data is %d bytes, shorter than the 37 it always has", len(b))
	}
	var ad authenticatorData
	copy(ad.rpIDHash[:], b[:32])
	ad.flags = b[32]
	ad.signCount = binary.BigEndian.Uint32(b[33:37])
	rest := b[37:]

	if ad.flags&flagAT != 0 {
		if len(rest) < 18 {
			return authenticatorData{}, fmt.Errorf("webauthn: the authenticator data ends inside its attested credential data")
		}
		var ac attestedCredential
		copy(ac.aaguid[:], rest[:16])
		n := int(binary.BigEndian.Uint16(rest[16:18]))
		rest = rest[18:]
		switch {
		case n == 0:
			return authenticatorData{}, fmt.Errorf("webauthn: the credential ID is empty")
		case n > maxCredentialID:
			return authenticatorData{}, fmt.Errorf("webauthn: a credential ID of %d bytes is longer than the %d one may be", n, maxCredentialID)
		case n > len(rest):
			return authenticatorData{}, fmt.Errorf("webauthn: the authenticator data ends inside its credential ID")
		}
		ac.id = append([]byte(nil), rest[:n]...)
		rest = rest[n:]

		// The key is followed by the extensions, if any, and only reading it says where it ends.
		v, used, err := decodeCBORPrefix(rest)
		if err != nil {
			return authenticatorData{}, fmt.Errorf("webauthn: the credential public key cannot be read: %w", err)
		}
		if ac.key, err = parsePublicKey(v); err != nil {
			return authenticatorData{}, err
		}
		ac.publicKey = append([]byte(nil), rest[:used]...)
		rest = rest[used:]
		ad.attested = &ac
	}

	if ad.flags&flagED != 0 {
		v, used, err := decodeCBORPrefix(rest)
		if err != nil {
			return authenticatorData{}, fmt.Errorf("webauthn: the extensions of the authenticator data cannot be read: %w", err)
		}
		if _, ok := v.(map[any]any); !ok {
			return authenticatorData{}, fmt.Errorf("webauthn: the extensions of the authenticator data are not a map")
		}
		ad.extensions = append([]byte(nil), rest[:used]...)
		rest = rest[used:]
	}

	if len(rest) != 0 {
		return authenticatorData{}, fmt.Errorf("webauthn: %d bytes follow the authenticator data", len(rest))
	}
	return ad, nil
}
