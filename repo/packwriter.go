package repo

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"math"
	"slices"
)

// PackWriter writes a pack of whole objects: no entry is a delta, so the pack depends on nothing
// but itself, and any of its entries can be copied into another pack as it is.
type PackWriter struct {
	out     *packSink
	count   int
	objects []PackedObject
	z       *zlib.Writer
	closed  bool
}

// packSink is where a pack's bytes go, and what they are counted and checksummed by.
type packSink struct {
	w   io.Writer
	sum hash.Hash
	crc hash.Hash32
	n   int64
	err error
}

func (s *packSink) Write(b []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.w.Write(b)
	s.sum.Write(b[:n])
	s.crc.Write(b[:n])
	s.n += int64(n)
	if err == nil && n < len(b) {
		err = io.ErrShortWrite
	}
	s.err = err
	return n, err
}

// NewPackWriter begins a pack of count objects on w. A pack's header counts its entries before
// any of them, so the count is known first; Close refuses a pack that did not get as many.
func NewPackWriter(w io.Writer, count int) (*PackWriter, error) {
	if count < 0 || count > math.MaxUint32 {
		return nil, fmt.Errorf("repo: a pack of %d objects, and a pack counts to %d", count, uint32(math.MaxUint32))
	}
	p := &PackWriter{out: &packSink{w: w, sum: sha1.New(), crc: crc32.NewIEEE()}, count: count}
	p.out.Write(appendPackHeader(nil, uint32(count)))
	return p, p.out.err
}

// begin starts an entry, and answers where it begins.
func (p *PackWriter) begin() (int64, error) {
	switch {
	case p.closed:
		return 0, errors.New("repo: an object written to a pack already closed")
	case len(p.objects) == p.count:
		return 0, fmt.Errorf("repo: more than the %d objects the pack was begun with", p.count)
	}
	p.out.crc.Reset()
	return p.out.n, p.out.err
}

func (p *PackWriter) end(id ID, t Type, offset int64) error {
	if p.out.err != nil {
		return p.out.err
	}
	p.objects = append(p.objects, PackedObject{ID: id, Type: t, Offset: offset, CRC32: p.out.crc.Sum32()})
	return nil
}

// Add writes an object whole, compressed, and answers its ID.
func (p *PackWriter) Add(t Type, data []byte) (ID, error) {
	if !t.valid() {
		return ID{}, fmt.Errorf("repo: an object of %s", t)
	}
	id := HashObject(t, data)
	return id, p.add(id, t, data)
}

// add writes an object whose ID the caller has computed.
func (p *PackWriter) add(id ID, t Type, data []byte) error {
	offset, err := p.begin()
	if err != nil {
		return err
	}
	p.out.Write(appendEntryHeader(nil, byte(t), int64(len(data))))
	if p.z == nil {
		p.z = zlib.NewWriter(p.out)
	} else {
		p.z.Reset(p.out)
	}
	p.z.Write(data)
	if err := p.z.Close(); err != nil && p.out.err == nil {
		return err
	}
	return p.end(id, t, offset)
}

// addDeflated writes an object whose content is already the zlib stream stream holds, as it is.
// The caller has inflated it and checked it against its ID and size.
func (p *PackWriter) addDeflated(id ID, t Type, size int64, stream io.Reader) error {
	offset, err := p.begin()
	if err != nil {
		return err
	}
	p.out.Write(appendEntryHeader(nil, byte(t), size))
	if _, err := io.Copy(p.out, stream); err != nil && p.out.err == nil {
		return err
	}
	return p.end(id, t, offset)
}

// Copy writes the object named id as the stored pack from holds it, header and zlib stream, with
// nothing inflated or compressed again. The entry is checked against the CRC32 its index holds as
// it is copied; a mismatch is found only once its bytes are written, and fails the pack.
func (p *PackWriter) Copy(from *Pack, id ID) error {
	i, ok := from.idx.Find(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrMissing, id)
	}
	offset, err := p.begin()
	if err != nil {
		return err
	}
	t, err := from.copyEntry(p.out, i)
	if err != nil {
		if p.out.err == nil {
			p.out.err = fmt.Errorf("repo: the object %s: %w", id, err)
		}
		return p.out.err
	}
	return p.end(id, t, offset)
}

// Close writes the pack's trailer, once every object it was begun with has been written, and
// answers its checksum. A pack holding an object twice is refused before its trailer, since its
// index could name only one of them.
func (p *PackWriter) Close() ([20]byte, error) {
	if p.closed {
		return [20]byte{}, errors.New("repo: a pack closed twice")
	}
	p.closed = true
	if p.out.err != nil {
		return [20]byte{}, p.out.err
	}
	if len(p.objects) != p.count {
		return [20]byte{}, fmt.Errorf("repo: a pack begun with %d objects and given %d", p.count, len(p.objects))
	}
	slices.SortFunc(p.objects, func(a, b PackedObject) int { return bytes.Compare(a.ID[:], b.ID[:]) })
	for i := 1; i < len(p.objects); i++ {
		if p.objects[i].ID == p.objects[i-1].ID {
			return [20]byte{}, fmt.Errorf("repo: the object %s twice in one pack", p.objects[i].ID)
		}
	}
	var sum [20]byte
	p.out.sum.Sum(sum[:0])
	p.out.Write(sum[:])
	return sum, p.out.err
}

// Objects are the objects written, sorted by ID once the pack is closed, for its index.
func (p *PackWriter) Objects() []PackedObject { return p.objects }
