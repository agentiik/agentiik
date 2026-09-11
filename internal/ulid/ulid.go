// Package ulid mints the twenty-six character identifier a run and an item are known by.
//
// It is internal because identity is a promise the engine keeps rather than a shape it
// publishes: callers reach it through agk.NewRunID and agk.NewItem, which is also what
// makes it swappable for a third party implementation without touching a call site.
//
// The value is a ULID: a 48 bit millisecond timestamp followed by 80 bits of
// randomness, written in Crockford base32. The timestamp leading means the text sorts
// in the order the identifiers were minted, which is what a run inspector reading a
// list of items relies on.
package ulid

import (
	"crypto/rand"
	"sync"
	"time"
)

// crockford is base32 without the letters that are read for one another: I, L, O and U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	mu      sync.Mutex
	lastMS  uint64
	entropy [10]byte
)

// New returns one identifier. It is safe for concurrent use, and two calls inside the
// same millisecond return values that still sort in the order they were made: the
// second increments the first's randomness rather than drawing again. Without that,
// items minted in one burst would sort arbitrarily among themselves, and the identifier
// would stop being the thing a list is ordered by.
func New() string {
	ms := uint64(time.Now().UTC().UnixMilli())

	mu.Lock()
	switch {
	case ms > lastMS:
		lastMS = ms
		// crypto/rand.Read never returns an error and always fills the slice, so
		// there is nothing here to recover from.
		_, _ = rand.Read(entropy[:])
	default:
		// The clock did not move, or moved backwards. Either way the value has to
		// be greater than the last one, so carry the last millisecond and add one
		// to the randomness.
		ms = lastMS
		if !increment(&entropy) {
			// Eighty bits of carry is not something a run reaches, but a value
			// that repeats would be, so spend a millisecond instead.
			lastMS++
			ms = lastMS
			_, _ = rand.Read(entropy[:])
		}
	}
	e := entropy
	mu.Unlock()

	var b [16]byte
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*uint(i)))
	}
	copy(b[6:], e[:])
	return encode(b)
}

// increment adds one to the randomness and reports whether it stayed inside its eighty
// bits.
func increment(e *[10]byte) bool {
	for i := len(e) - 1; i >= 0; i-- {
		e[i]++
		if e[i] != 0 {
			return true
		}
	}
	return false
}

// encode writes the sixteen bytes as twenty-six base32 characters. The value is 128
// bits and the text holds 130, so the first character carries three bits and the two
// missing ones are zero, which is why a ULID never starts above 7.
func encode(b [16]byte) string {
	var out [26]byte
	for i := range out {
		var v byte
		for j := 0; j < 5; j++ {
			pos := i*5 + j - 2
			if pos < 0 {
				continue
			}
			v = v<<1 | (b[pos/8]>>(7-uint(pos%8)))&1
		}
		out[i] = crockford[v]
	}
	return string(out[:])
}
