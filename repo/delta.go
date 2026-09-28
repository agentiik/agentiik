package repo

import (
	"errors"
	"fmt"
)

// A delta, as a pack carries one against its base: the base's size and the result's, each as a
// little-endian base-128 number, then instructions that either copy a range of the base or insert
// the bytes that follow them.

// deltaSizes reads the two sizes a delta begins with, and answers where its instructions begin.
func deltaSizes(delta []byte) (base, result uint64, n int, err error) {
	if base, n, err = deltaVarint(delta); err != nil {
		return 0, 0, 0, err
	}
	var m int
	if result, m, err = deltaVarint(delta[n:]); err != nil {
		return 0, 0, 0, err
	}
	return base, result, n + m, nil
}

func deltaVarint(b []byte) (uint64, int, error) {
	var v uint64
	for i, c := range b {
		if i == 10 || (i == 9 && c > 1) {
			return 0, 0, errors.New("a delta size past what 64 bits hold")
		}
		v |= uint64(c&0x7f) << (7 * i)
		if c&0x80 == 0 {
			return v, i + 1, nil
		}
	}
	return 0, 0, errors.New("a delta that ends inside its sizes")
}

// applyDelta is git's patch_delta: the result of delta against base, refusing a delta whose base is
// not the size it says, whose result would be larger than max, an instruction that copies from
// outside the base or writes past the result's size, the reserved instruction zero, and a result
// shorter than the size the delta gave it.
//
// The instructions are read and checked once before the result is allocated, so that a delta of a
// few bytes claiming a large result is refused before its memory is taken.
func applyDelta(base, delta []byte, max int64) ([]byte, error) {
	baseSize, resultSize, n, err := deltaSizes(delta)
	switch {
	case err != nil:
		return nil, err
	case baseSize != uint64(len(base)):
		return nil, fmt.Errorf("a delta against a base of %d bytes, applied to one of %d", baseSize, len(base))
	case resultSize > uint64(max):
		return nil, fmt.Errorf("a delta whose result is %d bytes, more than the %d an object resolved from a delta may be", resultSize, max)
	}
	if err := replay(base, delta[n:], resultSize, nil); err != nil {
		return nil, err
	}
	out := make([]byte, 0, resultSize)
	replay(base, delta[n:], resultSize, func(b []byte) { out = append(out, b...) })
	return out, nil
}

// replay reads a delta's instructions and hands each range they write to emit, where there is one.
func replay(base, ins []byte, resultSize uint64, emit func([]byte)) error {
	var written uint64
	for i := 0; i < len(ins); {
		op := ins[i]
		i++
		var b []byte
		switch {
		case op&0x80 != 0:
			// Copy: the bits of op say which bytes of the offset, then of the length, follow.
			var offset, length uint64
			for bit := range 7 {
				if op&(1<<bit) == 0 {
					continue
				}
				if i == len(ins) {
					return errors.New("a delta that ends inside a copy instruction")
				}
				if bit < 4 {
					offset |= uint64(ins[i]) << (8 * bit)
				} else {
					length |= uint64(ins[i]) << (8 * (bit - 4))
				}
				i++
			}
			if length == 0 {
				length = 0x10000
			}
			if offset+length > uint64(len(base)) {
				return fmt.Errorf("a delta copying bytes %d to %d of a base of %d", offset, offset+length, len(base))
			}
			b = base[offset : offset+length]
		case op != 0:
			// Insert: op is how many bytes follow, to be written as they are.
			if int(op) > len(ins)-i {
				return errors.New("a delta that ends inside the bytes it inserts")
			}
			b = ins[i : i+int(op)]
			i += int(op)
		default:
			return errors.New("a delta holding the instruction 0, which git reserves")
		}
		if written += uint64(len(b)); written > resultSize {
			return fmt.Errorf("a delta writing past the %d bytes it said its result is", resultSize)
		}
		if emit != nil {
			emit(b)
		}
	}
	if written != resultSize {
		return fmt.Errorf("a delta whose result is %d bytes where it said %d", written, resultSize)
	}
	return nil
}
