package webauthn

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"unicode/utf8"
)

// The CBOR this package reads (RFC 8949): an attestation object, a COSE key and the extensions of
// authenticator data, and nothing else. It is a subset chosen so that one item has one encoding,
// because two encodings of one value are where two parsers start to disagree about what a
// credential says:
//
//   - integers, byte strings, text, arrays and maps, and the two booleans an authenticator
//     extension may answer with (hmac-secret answers true at registration). Tags, floats, null,
//     undefined and the other simple values appear in nothing WebAuthn defines, and are refused;
//   - definite lengths only. An indefinite length has no place in the CTAP2 canonical form
//     authenticators write (§6.5.1), and it is how a length stops being something the decoder can
//     check against what is left before it allocates;
//   - every head in its shortest form (RFC 8949 §4.2.1), so 1 is written 0x01 and never 0x18 0x01.
//     The canonical form requires it, and it is where it matters: a longer head is a second
//     spelling of the same key or length;
//   - map keys that are integers or text, each once. A duplicate key is two answers to one
//     question, and which one a reader keeps is the kind of detail an attacker chooses for it;
//   - no bytes after the item, where the item is the whole input.
//
// The order of a map's keys is not checked, although the canonical form sorts them. Once a key
// appears only once, order changes nothing a reader of the map can see, and the key bytes this
// package keeps are the authenticator's own rather than a re-encoding a sort would disturb.

// maxInput is the most bytes any input of a ceremony may hold: client data, an attestation object,
// authenticator data, and each CBOR item. A registration with a 1023-byte credential ID and an
// 8192-bit RSA key is just over 2 KiB, so sixteen is room for anything that can pass, with the
// certificate chain of an attestation this package refuses by name rather than for its size, and
// small enough that no input costs the parse anything worth measuring.
const maxInput = 16 << 10

// maxDepth is how deeply arrays and maps may nest. The deepest structure WebAuthn defines is the
// certificate chain inside a compound attestation statement (§8.9), four levels down, and no
// authenticator extension answers with more than one.
const maxDepth = 4

// decodeCBOR reads the one CBOR item b holds, and refuses a byte after it.
func decodeCBOR(b []byte) (any, error) {
	v, n, err := decodeCBORPrefix(b)
	if err != nil {
		return nil, err
	}
	if n != len(b) {
		return nil, fmt.Errorf("cbor: %d bytes follow the item", len(b)-n)
	}
	return v, nil
}

// decodeCBORPrefix reads the CBOR item b starts with and says how many bytes it took. Authenticator
// data needs it: the credential public key is followed by the extensions, and where one ends is
// known only by reading it.
//
// An integer is an int64, a byte string a []byte of its own, text a string, an array an []any and
// a map a map[any]any whose keys are int64 or string.
func decodeCBORPrefix(b []byte) (any, int, error) {
	if len(b) > maxInput {
		return nil, 0, fmt.Errorf("cbor: the input is %d bytes, more than the %d read", len(b), maxInput)
	}
	d := decoder{in: b}
	v, err := d.item(0)
	if err != nil {
		return nil, 0, err
	}
	return v, d.off, nil
}

type decoder struct {
	in  []byte
	off int
}

// Major types, RFC 8949 §3.1.
const (
	majorUint   = 0
	majorNeg    = 1
	majorBytes  = 2
	majorText   = 3
	majorArray  = 4
	majorMap    = 5
	majorTag    = 6
	majorSimple = 7
)

// item reads one item, depth being the number of arrays and maps it sits inside.
func (d *decoder) item(depth int) (any, error) {
	if d.off >= len(d.in) {
		return nil, fmt.Errorf("cbor: the input ends inside an item")
	}
	if initial := d.in[d.off]; initial>>5 == majorSimple {
		d.off++
		switch initial & 0x1f {
		case 20:
			return false, nil
		case 21:
			return true, nil
		}
		return nil, fmt.Errorf("cbor: 0x%02x is a float or a simple value other than true and false, and none is read", initial)
	}
	major, arg, err := d.head()
	if err != nil {
		return nil, err
	}
	left := uint64(len(d.in) - d.off)
	switch major {
	case majorUint:
		if arg > math.MaxInt64 {
			return nil, fmt.Errorf("cbor: the integer %d does not fit in 64 signed bits", arg)
		}
		return int64(arg), nil
	case majorNeg:
		if arg > math.MaxInt64 {
			return nil, fmt.Errorf("cbor: the integer -1-%d does not fit in 64 signed bits", arg)
		}
		return -1 - int64(arg), nil
	case majorBytes, majorText:
		if arg > left {
			return nil, fmt.Errorf("cbor: a string of %d bytes runs past the end of the input", arg)
		}
		s := d.in[d.off : d.off+int(arg)]
		d.off += int(arg)
		if major == majorText {
			if !utf8.Valid(s) {
				return nil, fmt.Errorf("cbor: text is not valid UTF-8")
			}
			return string(s), nil
		}
		return bytes.Clone(s), nil
	case majorArray:
		if depth >= maxDepth {
			return nil, fmt.Errorf("cbor: arrays and maps nest deeper than %d", maxDepth)
		}
		// Every item takes a byte at least, so a count larger than what is left is a lie told to
		// make the decoder allocate, and is refused before it does.
		if arg > left {
			return nil, fmt.Errorf("cbor: an array of %d items runs past the end of the input", arg)
		}
		a := make([]any, arg)
		for i := range a {
			if a[i], err = d.item(depth + 1); err != nil {
				return nil, err
			}
		}
		return a, nil
	case majorMap:
		if depth >= maxDepth {
			return nil, fmt.Errorf("cbor: arrays and maps nest deeper than %d", maxDepth)
		}
		if arg > left/2 {
			return nil, fmt.Errorf("cbor: a map of %d entries runs past the end of the input", arg)
		}
		m := make(map[any]any, arg)
		for range arg {
			k, err := d.item(depth + 1)
			if err != nil {
				return nil, err
			}
			switch k.(type) {
			case int64, string:
			default:
				return nil, fmt.Errorf("cbor: a map key is a %T, and only integers and text are read as keys", k)
			}
			if _, dup := m[k]; dup {
				return nil, fmt.Errorf("cbor: the map key %#v appears twice", k)
			}
			if m[k], err = d.item(depth + 1); err != nil {
				return nil, err
			}
		}
		return m, nil
	}
	return nil, fmt.Errorf("cbor: a tag is refused, and nothing WebAuthn defines is tagged")
}

// head reads the initial byte of an item and the argument that follows it, RFC 8949 §3.
func (d *decoder) head() (major byte, arg uint64, err error) {
	initial := d.in[d.off]
	d.off++
	major, info := initial>>5, initial&0x1f
	var size int
	switch {
	case info < 24:
		return major, uint64(info), nil
	case info == 24:
		size = 1
	case info == 25:
		size = 2
	case info == 26:
		size = 4
	case info == 27:
		size = 8
	case info == 31:
		return 0, 0, fmt.Errorf("cbor: additional information 31, an indefinite length or a break, is refused")
	default:
		return 0, 0, fmt.Errorf("cbor: additional information %d is reserved", info)
	}
	if len(d.in)-d.off < size {
		return 0, 0, fmt.Errorf("cbor: the input ends inside an item")
	}
	var next [8]byte
	copy(next[8-size:], d.in[d.off:d.off+size])
	d.off += size
	arg = binary.BigEndian.Uint64(next[:])
	// The smallest argument each size may carry: anything below it fits in a shorter head.
	if arg < [...]uint64{1: 24, 2: 1 << 8, 4: 1 << 16, 8: 1 << 32}[size] {
		return 0, 0, fmt.Errorf("cbor: %d is written on %d bytes, where the shortest form is required", arg, size)
	}
	return major, arg, nil
}
