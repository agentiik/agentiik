package webauthn

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"testing"
)

// The authenticator data parser, fuzzed. Whatever it accepts it has read whole: the parts it
// returns, written back in the order §6.1 gives them, are the input byte for byte, with the
// attested credential data present exactly where AT is set, the extensions exactly where ED is,
// and a key of an algorithm this package verifies.
func FuzzParseAuthenticatorData(f *testing.F) {
	for _, alg := range []Algorithm{ES256, EdDSA} {
		a := newAuthenticator(f, alg)
		r := a.register(testCeremony(f))
		f.Add(r.data.bytes())
		r.data.flags |= flagED
		r.data.extensions = enc(pairs{{"credProtect", 2}, {"hmac-secret", true}})
		f.Add(r.data.bytes())
	}
	f.Add(slices.Concat(make([]byte, 32), []byte{flagUP | flagUV}, []byte{0, 0, 0, 9}))
	for _, v := range vectorsForFuzzing(f) {
		f.Add(v.authData)
		f.Add(v.assertionData)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		ad, err := parseAuthenticatorData(in)
		if err != nil {
			return
		}
		again := slices.Concat(ad.rpIDHash[:], []byte{ad.flags})
		again = binary.BigEndian.AppendUint32(again, ad.signCount)
		if (ad.attested != nil) != (ad.flags&flagAT != 0) {
			t.Fatalf("%x: attested credential data read is %v, where AT is %v", in, ad.attested != nil, ad.flags&flagAT != 0)
		}
		if ad.attested != nil {
			if n := len(ad.attested.id); n == 0 || n > maxCredentialID {
				t.Fatalf("%x: a credential ID of %d bytes was taken", in, n)
			}
			if !slices.Contains(Algorithms(), ad.attested.key.alg) {
				t.Fatalf("%x: a key for %s was taken", in, ad.attested.key.alg)
			}
			again = append(again, ad.attested.aaguid[:]...)
			again = binary.BigEndian.AppendUint16(again, uint16(len(ad.attested.id)))
			again = slices.Concat(again, ad.attested.id, ad.attested.publicKey)
		}
		if (ad.extensions != nil) != (ad.flags&flagED != 0) {
			t.Fatalf("%x: extensions read are %v, where ED is %v", in, ad.extensions != nil, ad.flags&flagED != 0)
		}
		again = append(again, ad.extensions...)
		if !bytes.Equal(again, in) {
			t.Fatalf("%x read as %x", in, again)
		}
	})
}

// The parts of authenticator data are read where §6.1 puts them.
func TestAuthenticatorDataIsReadWhereTheSpecificationPutsIt(t *testing.T) {
	a := newAuthenticator(t, ES256)
	r := a.register(testCeremony(t))
	r.data.signCount = 0x01020304
	r.data.flags |= flagED
	r.data.extensions = enc(pairs{{"credProtect", 3}})
	ad, err := parseAuthenticatorData(r.data.bytes())
	if err != nil {
		t.Fatal(err)
	}
	if ad.rpIDHash != sha256.Sum256([]byte(testRPID)) || ad.signCount != 0x01020304 || ad.flags != r.data.flags {
		t.Fatalf("read %x, flags %08b and count %x", ad.rpIDHash, ad.flags, ad.signCount)
	}
	if ad.attested.aaguid != a.aaguid || !bytes.Equal(ad.attested.id, a.id) || !bytes.Equal(ad.attested.publicKey, enc(a.cose)) {
		t.Fatalf("read the credential %x with AAGUID %x and key %x", ad.attested.id, ad.attested.aaguid, ad.attested.publicKey)
	}
	if !bytes.Equal(ad.extensions, r.data.extensions) {
		t.Fatalf("read the extensions %x, where they are %x", ad.extensions, r.data.extensions)
	}
}
