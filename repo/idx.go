package repo

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
)

// An index, version 2, as git writes one beside a pack: a magic number and the version; 256
// cumulative counts of the objects by the first byte of their ID; the IDs, sorted; the CRC32 of
// each object's entry; its offset in the pack, in 31 bits or, with the high bit set, as the
// position of a 64-bit offset in the table after; the pack's checksum; and the SHA-1 of all of it.

var idxMagic = []byte{0xff, 't', 'O', 'c'}

const (
	idxHeaderLen = 8 + 256*4
	// idxLargeOffset is the first offset held in the table of 64-bit offsets. Git holds every
	// offset below it in 31 bits, and an index written the same way is the same bytes as git's.
	idxLargeOffset = 1 << 31
)

// PackedObject is one object of a pack, as its index names it.
type PackedObject struct {
	ID   ID
	Type Type
	// Offset is where its entry begins in the pack.
	Offset int64
	// CRC32 is the checksum of its entry as it is stored, header and zlib stream, which is what
	// lets an entry be copied into another pack without being inflated and still be checked.
	CRC32 uint32
}

// Idx is a pack's index, read.
type Idx struct {
	n        int
	fanout   []byte
	ids      []byte
	crcs     []byte
	offsets  []byte
	large    []byte
	checksum [20]byte
}

// ParseIdx reads an index, and refuses one that is not version 2, whose counts are not cumulative,
// whose IDs are not in order or appear twice or sit under the wrong first byte, whose offsets name
// a 64-bit offset that is not there or that belongs in 31 bits, or whose own checksum does not
// match. The index keeps data, which must not change while it is in use.
func ParseIdx(data []byte) (*Idx, error) {
	if len(data) < idxHeaderLen+2*sha1.Size || !bytes.Equal(data[:4], idxMagic) {
		return nil, errors.New("repo: not a pack index: it does not begin with the index magic number")
	}
	if v := binary.BigEndian.Uint32(data[4:]); v != 2 {
		return nil, fmt.Errorf("repo: a pack index of version %d, and version 2 is the one read", v)
	}
	body, sum := data[:len(data)-sha1.Size], data[len(data)-sha1.Size:]
	if got := sha1.Sum(body); !bytes.Equal(got[:], sum) {
		return nil, errors.New("repo: the pack index does not match its checksum")
	}
	x := &Idx{fanout: data[8:idxHeaderLen]}
	var last uint32
	for b := range 256 {
		c := binary.BigEndian.Uint32(x.fanout[4*b:])
		if c < last {
			return nil, fmt.Errorf("repo: the pack index counts %d objects up to %02x and %d up to %02x", last, b-1, c, b)
		}
		last = c
	}
	x.n = int(last)
	fixed := idxHeaderLen + x.n*(sha1.Size+4+4) + 2*sha1.Size
	if x.n > (len(data)-idxHeaderLen)/(sha1.Size+8) || len(data) < fixed || (len(data)-fixed)%8 != 0 {
		return nil, fmt.Errorf("repo: a pack index of %d bytes cannot hold the %d objects it counts", len(data), x.n)
	}
	at := idxHeaderLen
	x.ids = data[at : at+x.n*sha1.Size]
	at += x.n * sha1.Size
	x.crcs = data[at : at+4*x.n]
	at += 4 * x.n
	x.offsets = data[at : at+4*x.n]
	at += 4 * x.n
	x.large = data[at : len(data)-2*sha1.Size]
	copy(x.checksum[:], data[len(data)-2*sha1.Size:])
	// The 64-bit offsets are held in the order the objects naming them are, each named once, as git
	// writes them, so that an index has one spelling and is the same bytes written back.
	larges := 0
	for i := range x.n {
		id := x.ids[i*sha1.Size : (i+1)*sha1.Size]
		if i > 0 && bytes.Compare(x.ids[(i-1)*sha1.Size:i*sha1.Size], id) >= 0 {
			return nil, fmt.Errorf("repo: the pack index holds %x after %x, which is out of order or twice", id, x.ids[(i-1)*sha1.Size:i*sha1.Size])
		}
		first := int(id[0])
		below := uint32(0)
		if first > 0 {
			below = binary.BigEndian.Uint32(x.fanout[4*(first-1):])
		}
		if uint32(i) < below || uint32(i) >= binary.BigEndian.Uint32(x.fanout[4*first:]) {
			return nil, fmt.Errorf("repo: the pack index holds %x where its counts say objects beginning with %02x are not", id, first)
		}
		if raw := binary.BigEndian.Uint32(x.offsets[4*i:]); raw&0x80000000 != 0 {
			if int(raw&0x7fffffff) != larges {
				return nil, fmt.Errorf("repo: the pack index names 64-bit offset %d where the next is %d", raw&0x7fffffff, larges)
			}
			larges++
		}
		if _, err := x.offset(i); err != nil {
			return nil, err
		}
	}
	if larges*8 != len(x.large) {
		return nil, fmt.Errorf("repo: the pack index holds %d 64-bit offsets and names %d", len(x.large)/8, larges)
	}
	return x, nil
}

// Len is how many objects the index names.
func (x *Idx) Len() int { return x.n }

// ID is the i-th object's ID, in the index's order.
func (x *Idx) ID(i int) ID { return ID(x.ids[i*sha1.Size : (i+1)*sha1.Size]) }

// CRC32 is the checksum of the i-th object's entry.
func (x *Idx) CRC32(i int) uint32 { return binary.BigEndian.Uint32(x.crcs[4*i:]) }

// Offset is where the i-th object's entry begins in the pack.
func (x *Idx) Offset(i int) int64 {
	off, _ := x.offset(i)
	return off
}

func (x *Idx) offset(i int) (int64, error) {
	off := binary.BigEndian.Uint32(x.offsets[4*i:])
	if off&0x80000000 == 0 {
		if off < packHeaderLen {
			return 0, fmt.Errorf("repo: the pack index places an object at %d, inside the pack's header", off)
		}
		return int64(off), nil
	}
	at := int(off&0x7fffffff) * 8
	if at+8 > len(x.large) {
		return 0, fmt.Errorf("repo: the pack index names 64-bit offset %d of the %d it holds", off&0x7fffffff, len(x.large)/8)
	}
	large := binary.BigEndian.Uint64(x.large[at:])
	if large < idxLargeOffset || large > 1<<62 {
		return 0, fmt.Errorf("repo: the pack index holds %d as a 64-bit offset", large)
	}
	return int64(large), nil
}

// Checksum is the checksum of the pack the index is for, the SHA-1 its last twenty bytes hold.
func (x *Idx) Checksum() [20]byte { return x.checksum }

// Find answers the position of the object named id, and whether the index names it at all.
func (x *Idx) Find(id ID) (int, bool) {
	lo := 0
	if id[0] > 0 {
		lo = int(binary.BigEndian.Uint32(x.fanout[4*(int(id[0])-1):]))
	}
	hi := int(binary.BigEndian.Uint32(x.fanout[4*int(id[0]):]))
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		switch bytes.Compare(x.ids[mid*sha1.Size:(mid+1)*sha1.Size], id[:]) {
		case 0:
			return mid, true
		case -1:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return 0, false
}

// WriteIdx writes the index of a pack holding objects, whose checksum is checksum, as git writes
// one: the same pack gives the same bytes as git index-pack does. Objects appearing twice are
// refused, since an index names each once.
func WriteIdx(w io.Writer, objects []PackedObject, checksum [20]byte) error {
	sorted := slices.Clone(objects)
	slices.SortFunc(sorted, func(a, b PackedObject) int { return bytes.Compare(a.ID[:], b.ID[:]) })
	h := sha1.New()
	b := append([]byte(nil), idxMagic...)
	b = binary.BigEndian.AppendUint32(b, 2)
	var fanout [256]uint32
	for i, o := range sorted {
		if i > 0 && sorted[i-1].ID == o.ID {
			return fmt.Errorf("repo: the object %s twice in one pack", o.ID)
		}
		if o.Offset < packHeaderLen {
			return fmt.Errorf("repo: the object %s at offset %d, inside the pack's header", o.ID, o.Offset)
		}
		fanout[o.ID[0]]++
	}
	var count uint32
	for i := range fanout {
		count += fanout[i]
		b = binary.BigEndian.AppendUint32(b, count)
	}
	for _, o := range sorted {
		b = append(b, o.ID[:]...)
	}
	for _, o := range sorted {
		b = binary.BigEndian.AppendUint32(b, o.CRC32)
	}
	var large []byte
	for _, o := range sorted {
		if o.Offset < idxLargeOffset {
			b = binary.BigEndian.AppendUint32(b, uint32(o.Offset))
			continue
		}
		b = binary.BigEndian.AppendUint32(b, 0x80000000|uint32(len(large)/8))
		large = binary.BigEndian.AppendUint64(large, uint64(o.Offset))
	}
	b = append(b, large...)
	b = append(b, checksum[:]...)
	h.Write(b)
	b = h.Sum(b)
	_, err := w.Write(b)
	return err
}
