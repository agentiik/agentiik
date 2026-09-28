package repo

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"slices"
)

// Pack is a stored pack, read through its index: any object by its ID, and any entry as it is
// stored, for copying into another pack. It reads through an io.ReaderAt, so that a pack in the
// object store is read with ranged reads and never whole.
//
// The packs this module stores hold every object whole, since no stored pack depends on another,
// but a pack holding deltas against its own objects is read as well, as git writes one.
type Pack struct {
	r    io.ReaderAt
	size int64
	idx  *Idx
	// ends is where each entry ends, by its position in the index: where the next entry begins,
	// or the trailer.
	ends []int64
	// starts are the offsets of the entries in the order they are in the pack, and positions
	// their positions in the index, for finding the base of an OFS_DELTA.
	starts    []int64
	positions []int
}

// OpenPack reads a pack of size bytes through its index, and refuses a pack whose header does not
// count the objects the index names, whose trailer is not the checksum the index was written
// for, or whose entries the index places outside it or at one offset twice.
func OpenPack(r io.ReaderAt, size int64, idx *Idx) (*Pack, error) {
	if size < packHeaderLen+packTrailerLen {
		return nil, fmt.Errorf("repo: a pack of %d bytes, shorter than a header and a trailer", size)
	}
	var head [packHeaderLen]byte
	if _, err := r.ReadAt(head[:], 0); err != nil {
		return nil, fmt.Errorf("repo: the pack's header: %w", err)
	}
	count, err := readPackHeader(head[:])
	if err != nil {
		return nil, err
	}
	if int64(count) != int64(idx.Len()) {
		return nil, fmt.Errorf("repo: a pack of %d objects, and an index of %d", count, idx.Len())
	}
	var trailer [packTrailerLen]byte
	if _, err := r.ReadAt(trailer[:], size-packTrailerLen); err != nil {
		return nil, fmt.Errorf("repo: the pack's trailer: %w", err)
	}
	if trailer != idx.Checksum() {
		return nil, errors.New("repo: the pack's trailer is not the checksum its index was written for")
	}
	p := &Pack{r: r, size: size, idx: idx, ends: make([]int64, idx.Len()), starts: make([]int64, idx.Len()), positions: make([]int, idx.Len())}
	for i := range p.positions {
		p.positions[i] = i
	}
	slices.SortFunc(p.positions, func(a, b int) int { return cmp.Compare(idx.Offset(a), idx.Offset(b)) })
	end := size - packTrailerLen
	for k := len(p.positions) - 1; k >= 0; k-- {
		i := p.positions[k]
		off := idx.Offset(i)
		if off < packHeaderLen || off >= end {
			return nil, fmt.Errorf("repo: the index places %s at %d, where no entry of its own can begin", idx.ID(i), off)
		}
		p.starts[k] = off
		p.ends[i] = end
		end = off
	}
	return p, nil
}

// Len is how many objects the pack holds.
func (p *Pack) Len() int { return p.idx.Len() }

// Has is whether the pack holds the object named id.
func (p *Pack) Has(id ID) bool {
	_, ok := p.idx.Find(id)
	return ok
}

// OpenObject answers the object named id. An object stored whole is inflated as it is read, and
// checked against its ID once it has been, so that a pack never serves bytes that are not the
// object it names: the last Read answers an error instead of io.EOF where they are not. An object
// stored as a delta is resolved in memory first.
func (p *Pack) OpenObject(ctx context.Context, id ID) (ObjectReader, error) {
	i, ok := p.idx.Find(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrMissing, id)
	}
	off := p.idx.Offset(i)
	r, kind, size, err := p.entry(off, p.ends[i])
	if err != nil {
		return nil, fmt.Errorf("repo: the object %s: %w", id, err)
	}
	if kind == kindOfsDelta || kind == kindRefDelta {
		t, data, err := p.resolve(ctx, off, p.ends[i], 0)
		if err != nil {
			return nil, fmt.Errorf("repo: the object %s: %w", id, err)
		}
		if got := HashObject(t, data); got != id {
			return nil, fmt.Errorf("repo: the object %s resolves to %s", id, got)
		}
		return &heldObject{Reader: bytes.NewReader(data), t: t}, nil
	}
	z, err := zlib.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("repo: the object %s: %w", id, err)
	}
	t := Type(kind)
	return &streamedObject{z: z, t: t, size: size, left: size, h: newObjectHash(t, size), id: id}, nil
}

// entry reads the header of the entry at off, which ends at end, and answers a reader of what
// follows it: the delta's base for a delta, then the zlib stream.
func (p *Pack) entry(off, end int64) (*bufio.Reader, byte, int64, error) {
	r := bufio.NewReaderSize(io.NewSectionReader(p.r, off, end-off), 16<<10)
	kind, size, err := readEntryHeader(r, 1<<62)
	if err != nil {
		return nil, 0, 0, err
	}
	if kind == 0 || kind == 5 || kind > kindRefDelta {
		return nil, 0, 0, fmt.Errorf("an entry of kind %d, which is none of git's", kind)
	}
	return r, kind, size, nil
}

// resolve reads the entry at off whole, applying deltas down to the object it is made from.
func (p *Pack) resolve(ctx context.Context, off, end int64, depth int) (Type, []byte, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	r, kind, size, err := p.entry(off, end)
	if err != nil {
		return 0, nil, err
	}
	if size > maxHeld {
		return 0, nil, fmt.Errorf("an entry of %d bytes to be held whole, and the most is %d", size, maxHeld)
	}
	if kind != kindOfsDelta && kind != kindRefDelta {
		data, err := inflate(r, size)
		return Type(kind), data, err
	}
	if depth == MaxDeltaDepth {
		return 0, nil, fmt.Errorf("a chain of more than %d deltas", MaxDeltaDepth)
	}
	var baseOff, baseEnd int64
	if kind == kindOfsDelta {
		distance, err := readOfsDelta(r)
		if err != nil {
			return 0, nil, err
		}
		baseOff = off - distance
		i, ok := p.at(baseOff)
		if distance <= 0 || !ok {
			return 0, nil, fmt.Errorf("a delta whose base is %d bytes before it, where no entry begins", distance)
		}
		baseEnd = p.ends[i]
	} else {
		var base ID
		if _, err := io.ReadFull(r, base[:]); err != nil {
			return 0, nil, err
		}
		i, ok := p.idx.Find(base)
		if !ok {
			return 0, nil, fmt.Errorf("a delta against %s, which the pack does not hold, and a stored pack depends on no other", base)
		}
		baseOff, baseEnd = p.idx.Offset(i), p.ends[i]
	}
	delta, err := inflate(r, size)
	if err != nil {
		return 0, nil, err
	}
	t, base, err := p.resolve(ctx, baseOff, baseEnd, depth+1)
	if err != nil {
		return 0, nil, err
	}
	data, err := applyDelta(base, delta, maxHeld)
	return t, data, err
}

// at answers the position in the index of the entry that begins at off.
func (p *Pack) at(off int64) (int, bool) {
	k, ok := slices.BinarySearch(p.starts, off)
	if !ok {
		return 0, false
	}
	return p.positions[k], true
}

// inflate reads a zlib stream holding exactly size bytes, to its end and checksum.
func inflate(r io.Reader, size int64) ([]byte, error) {
	z, err := zlib.NewReader(r)
	if err != nil {
		return nil, err
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(z, data); err != nil {
		return nil, fmt.Errorf("a zlib stream shorter than the %d bytes its header gives: %w", size, err)
	}
	if err := atEnd(z); err != nil {
		return nil, err
	}
	return data, nil
}

// atEnd reads a zlib stream past the content it was expected to hold, which is where zlib reads
// its checksum, and refuses a stream holding more.
func atEnd(z io.Reader) error {
	var b [1]byte
	n, err := z.Read(b[:])
	for n == 0 && err == nil {
		n, err = z.Read(b[:])
	}
	switch {
	case n > 0:
		return errors.New("a zlib stream longer than the size its header gives")
	case err != io.EOF:
		return err
	}
	return nil
}

// heldObject is an object resolved in memory.
type heldObject struct {
	*bytes.Reader
	t Type
}

func (o *heldObject) Type() Type   { return o.t }
func (o *heldObject) Size() int64  { return o.Reader.Size() }
func (o *heldObject) Close() error { return nil }

// streamedObject is an object inflated as it is read, and hashed as it is, so that its last Read
// says whether it was the object it was opened as.
type streamedObject struct {
	z    io.ReadCloser
	t    Type
	size int64
	left int64
	h    hash.Hash
	id   ID
	err  error
}

func (o *streamedObject) Type() Type  { return o.t }
func (o *streamedObject) Size() int64 { return o.size }

func (o *streamedObject) Read(b []byte) (int, error) {
	if o.err != nil {
		return 0, o.err
	}
	if o.left == 0 {
		o.err = io.EOF
		if err := atEnd(o.z); err != nil {
			o.err = fmt.Errorf("repo: the object %s: %w", o.id, err)
		} else if got := sumID(o.h); got != o.id {
			o.err = fmt.Errorf("repo: the object %s reads as %s", o.id, got)
		}
		return 0, o.err
	}
	if int64(len(b)) > o.left {
		b = b[:o.left]
	}
	n, err := o.z.Read(b)
	o.h.Write(b[:n])
	o.left -= int64(n)
	if err == io.EOF && o.left > 0 {
		err = fmt.Errorf("repo: the object %s: a zlib stream shorter than the %d bytes its header gives", o.id, o.size)
	}
	if err != nil && err != io.EOF {
		o.err = err
		return n, err
	}
	return n, nil
}

func (o *streamedObject) Close() error { return o.z.Close() }

// copyEntry writes the entry of the object at position i as it is stored to w, and refuses it where
// it is a delta or where its CRC32 is not the one the index holds, answering its type and size.
func (p *Pack) copyEntry(w io.Writer, i int) (Type, error) {
	off, end := p.idx.Offset(i), p.ends[i]
	_, kind, _, err := p.entry(off, end)
	if err != nil {
		return 0, err
	}
	if kind == kindOfsDelta || kind == kindRefDelta {
		return 0, errors.New("a delta, which is copied into another pack only with its base")
	}
	crc := crc32.NewIEEE()
	if _, err := io.Copy(io.MultiWriter(w, crc), io.NewSectionReader(p.r, off, end-off)); err != nil {
		return 0, err
	}
	if crc.Sum32() != p.idx.CRC32(i) {
		return 0, errors.New("an entry whose CRC32 is not the one its index holds")
	}
	return Type(kind), nil
}
