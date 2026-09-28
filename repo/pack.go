package repo

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// A pack, version 2: "PACK", the version and the number of entries, each a 32-bit big-endian
// integer; the entries; and the SHA-1 of everything before it. An entry is a header giving its
// kind and the size of its content once inflated, what a delta is made against where it is one,
// and the content as a zlib stream.

const (
	packHeaderLen  = 12
	packTrailerLen = 20
)

// The kinds of entry, as an entry's header numbers them: the four types of object, which are
// Type's values, and the two kinds of delta.
const (
	// kindOfsDelta is a delta against the entry that begins so many bytes before this one.
	kindOfsDelta = 6
	// kindRefDelta is a delta against the object named by the ID that follows the header, which
	// in a thin pack is one the receiving repository already holds.
	kindRefDelta = 7
)

// readPackHeader reads the twelve bytes a pack begins with and answers how many entries it holds.
func readPackHeader(b []byte) (uint32, error) {
	switch {
	case len(b) < packHeaderLen:
		return 0, errors.New("repo: the pack ends inside its header")
	case string(b[:4]) != "PACK":
		return 0, fmt.Errorf("repo: a pack begins with PACK, and this one with %q", b[:4])
	case binary.BigEndian.Uint32(b[4:]) != 2:
		return 0, fmt.Errorf("repo: a pack of version %d, and version 2 is the one read", binary.BigEndian.Uint32(b[4:]))
	}
	return binary.BigEndian.Uint32(b[8:]), nil
}

func appendPackHeader(b []byte, count uint32) []byte {
	b = append(b, "PACK"...)
	b = binary.BigEndian.AppendUint32(b, 2)
	return binary.BigEndian.AppendUint32(b, count)
}

// appendEntryHeader writes an entry's kind and size: the kind in bits 4 to 6 of the first byte, the
// size's four low bits below it, and the rest seven bits a byte, the high bit of each saying
// another follows.
func appendEntryHeader(b []byte, kind byte, size int64) []byte {
	c := kind<<4 | byte(size&0x0f)
	size >>= 4
	for size > 0 {
		b = append(b, c|0x80)
		c = byte(size & 0x7f)
		size >>= 7
	}
	return append(b, c)
}

// readEntryHeader reads an entry's kind and size, refusing a size past max, which is read before
// any byte of what it sizes.
func readEntryHeader(r io.ByteReader, max int64) (byte, int64, error) {
	c, err := r.ReadByte()
	if err != nil {
		return 0, 0, err
	}
	kind := c >> 4 & 7
	size := int64(c & 0x0f)
	for shift := 4; c&0x80 != 0; shift += 7 {
		if c, err = r.ReadByte(); err != nil {
			return 0, 0, err
		}
		if shift > 56 || int64(c&0x7f) > (max>>shift) {
			return 0, 0, fmt.Errorf("an entry of more than %d bytes", max)
		}
		size |= int64(c&0x7f) << shift
	}
	if size > max {
		return 0, 0, fmt.Errorf("an entry of %d bytes, more than %d", size, max)
	}
	return kind, size, nil
}

// readOfsDelta reads how far before its own entry an OFS_DELTA's base begins: seven bits a byte,
// most significant first, each byte after the first adding one before its bits are shifted in so
// that no distance has two spellings.
func readOfsDelta(r io.ByteReader) (int64, error) {
	c, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	distance := int64(c & 0x7f)
	for c&0x80 != 0 {
		if c, err = r.ReadByte(); err != nil {
			return 0, err
		}
		if distance >= 1<<49 {
			return 0, errors.New("a delta whose base is further back than any pack reaches")
		}
		distance = (distance+1)<<7 | int64(c&0x7f)
	}
	return distance, nil
}

// counter reads a pack from a byte reader and counts what it hands on, which is where an entry
// ends once its zlib stream has been read: compress/flate reads no byte past its stream from a
// reader that has ReadByte, and compress/zlib reads its checksum from the same reader.
type counter struct {
	r *bufio.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *counter) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}
