// Package password hashes and verifies the passwords of the password fallback, with Argon2id at the
// OWASP baseline the documentation names: "19 MiB of memory, 2 iterations, 1 degree of parallelism".
//
// A hash is kept in the self-describing form the reference implementation writes, the PHC string
// format, "$argon2id$v=19$m=19456,t=2,p=1$" followed by the salt and the key in unpadded standard
// base64, so that "a later cost still reads an older hash": verifying reads the parameters from the
// hash rather than from this package.
//
// # What a hash may ask for
//
// Verifying costs what the hash says, and the API bounds how many run at once by the memory one
// baseline hash holds, MemoryBytes. So a hash asking for more memory, more iterations or more
// parallelism than the baseline is refused as unreadable rather than run: every hash this release
// writes is at the baseline, one a later release writes at a higher cost is one this release was
// not sized for, and one nobody wrote is somebody who can write to the credentials table, which the
// API should not spend its memory on. An older hash at a lower cost reads.
//
// # What is compared
//
// The key derived from the password given is compared with the one kept in constant time, since
// comparing byte by byte and stopping at the first difference would tell whoever can time the
// answer how many bytes they had right.
//
// The password is hashed as the bytes it was sent as, prepared by no profile: a password is set and
// checked through the same route, so both sides see the same bytes.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// The baseline, OWASP's first recommendation for Argon2id: 19 MiB of memory, written in KiB as the
// algorithm takes it, 2 iterations, and 1 degree of parallelism.
const (
	Memory      = 19 * 1024
	Iterations  = 2
	Parallelism = 1
)

// MemoryBytes is what one hash at the baseline holds while it runs, which is what bounding how many
// run at once bounds.
const MemoryBytes = Memory * 1024

// saltBytes and keyBytes are the salt drawn for each hash and the key it derives: 128 bits of salt,
// which the RFC recommends, and a 256-bit key.
const (
	saltBytes = 16
	keyBytes  = 32
)

// The shortest and longest salt and key a hash may carry: the reference implementation's shortest
// salt, 8 bytes, a key of 16 bytes at least, since a shorter one is a guess away from a collision,
// and 64 for both, more than any hasher writes.
const (
	saltMin, saltMax = 8, 64
	keyMin, keyMax   = 16, 64
)

// version is the one version of Argon2 the implementation computes, 0x13, as a hash writes it.
const version = 19

// ErrUnreadable is a hash this package does not verify: not an Argon2id hash in the PHC string
// format, or one asking for more than the baseline.
var ErrUnreadable = errors.New("password: the hash is not one this release reads")

// b64 is how the PHC string format writes bytes: standard base64 with no padding.
var b64 = base64.RawStdEncoding

// Hash hashes a password at the baseline, with a salt of its own, and answers the encoded hash.
func Hash(password string) (string, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password: a salt could not be drawn: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, Iterations, Memory, Parallelism, keyBytes)
	return encode(params{memory: Memory, iterations: Iterations, parallelism: Parallelism}, salt, key), nil
}

// Verify answers whether password is the one encoded was made from. An error is a hash it does not
// read, ErrUnreadable, and never a password that does not match.
func Verify(encoded, password string) (bool, error) {
	p, salt, key, err := decode(encoded)
	if err != nil {
		return false, err
	}
	derived := argon2.IDKey([]byte(password), salt, p.iterations, p.memory, p.parallelism, uint32(len(key)))
	return subtle.ConstantTimeCompare(derived, key) == 1, nil
}

// params are a hash's costs, as its PHC string writes them.
type params struct {
	memory, iterations uint32
	parallelism        uint8
}

func encode(p params, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		version, p.memory, p.iterations, p.parallelism, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// decode reads a PHC string of Argon2id, refusing one with a part missing, out of order or written
// twice, parameters above the baseline, or a salt or a key outside their bounds.
func decode(encoded string) (params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(version) {
		return params{}, nil, nil, fmt.Errorf("%w: it is not an Argon2id hash of version %d in the PHC string format", ErrUnreadable, version)
	}
	costs := strings.Split(parts[3], ",")
	if len(costs) != 3 {
		return params{}, nil, nil, fmt.Errorf("%w: it writes %d costs, and Argon2id's are m, t and p", ErrUnreadable, len(costs))
	}
	var values [3]uint64
	for i, name := range []string{"m", "t", "p"} {
		digits, ok := strings.CutPrefix(costs[i], name+"=")
		// Decimal digits alone, and no leading zero, so that a cost has one spelling.
		if !ok || digits == "" || strings.Trim(digits, "0123456789") != "" || (len(digits) > 1 && digits[0] == '0') {
			return params{}, nil, nil, fmt.Errorf("%w: its costs are not m, t and p written in decimal, in that order", ErrUnreadable)
		}
		v, err := strconv.ParseUint(digits, 10, 32)
		if err != nil {
			return params{}, nil, nil, fmt.Errorf("%w: its cost %s is past what Argon2id takes", ErrUnreadable, name)
		}
		values[i] = v
	}
	p := params{memory: uint32(values[0]), iterations: uint32(values[1]), parallelism: uint8(min(values[2], 255))}
	switch {
	case values[2] < 1 || values[2] > Parallelism:
		return params{}, nil, nil, fmt.Errorf("%w: it asks for a parallelism of %d, and this release reads 1 to %d", ErrUnreadable, values[2], Parallelism)
	case p.iterations < 1 || p.iterations > Iterations:
		return params{}, nil, nil, fmt.Errorf("%w: it asks for %d iterations, and this release reads 1 to %d", ErrUnreadable, p.iterations, Iterations)
	case p.memory < 8*uint32(p.parallelism) || p.memory > Memory:
		// Argon2 takes at least 8 KiB for each lane.
		return params{}, nil, nil, fmt.Errorf("%w: it asks for %d KiB of memory, and this release reads %d to %d", ErrUnreadable, p.memory, 8*uint32(p.parallelism), Memory)
	}
	salt, err := b64.Strict().DecodeString(parts[4])
	if err != nil || len(salt) < saltMin || len(salt) > saltMax {
		return params{}, nil, nil, fmt.Errorf("%w: its salt is not %d to %d bytes of unpadded base64", ErrUnreadable, saltMin, saltMax)
	}
	key, err := b64.Strict().DecodeString(parts[5])
	if err != nil || len(key) < keyMin || len(key) > keyMax {
		return params{}, nil, nil, fmt.Errorf("%w: its key is not %d to %d bytes of unpadded base64", ErrUnreadable, keyMin, keyMax)
	}
	return p, salt, key, nil
}
