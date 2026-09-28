package repo

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"io"
	"slices"
	"testing"
)

// The readers of what anyone with workflow:write can push, fuzzed. Each target's seed corpus is in
// testdata/fuzz: what git itself writes, a pack, an index, a delta, objects and packets, beside the
// cases below. go test runs the seeds; go test -fuzz=FuzzUnpack -fuzztime=10s explores from them.

// fuzzBounds keeps what one input can make a target allocate to a mebibyte.
func fuzzBounds(f *testing.F) {
	held, cache := maxHeld, deltaBaseCache
	maxHeld, deltaBaseCache = 1<<20, 4<<10
	f.Cleanup(func() { maxHeld, deltaBaseCache = held, cache })
}

func FuzzUnpack(f *testing.F) {
	fuzzBounds(f)
	b := &builder{}
	base := []byte("a base\nwith lines\n")
	d, _ := edit(base, "and one more\n")
	b.ofs(b.whole(TypeBlob, base), d)
	b.ref(HashObject(TypeBlob, []byte("thin")), delta(4, 4, copyOp(0, 4)))
	tree, _ := EncodeTree([]TreeEntry{{Name: "a", Mode: ModeFile, ID: HashObject(TypeBlob, base)}})
	b.whole(TypeTree, tree)
	f.Add(b.all())
	f.Add((&builder{}).all())
	bases := memory{}
	bases.put(TypeBlob, []byte("thin"))
	f.Fuzz(func(t *testing.T, pack []byte) {
		// The trailer is made to match, since a mutation that breaks it is refused before anything
		// past it is reached, and the tests refuse a trailer that does not match.
		if len(pack) >= packHeaderLen+packTrailerLen {
			pack = resum(pack)
		}
		var out bytes.Buffer
		u, err := Unpack(context.Background(), bytes.NewReader(pack), int64(len(pack)), &out, UnpackOptions{Bases: bases, MaxObjectBytes: 1 << 20})
		if err != nil {
			return
		}
		// What is written is a pack of whole objects, each the object its ID names...
		st := store(t, out.Bytes(), u)
		for _, o := range u.Objects {
			typ, data, err := ReadObject(context.Background(), st, o.ID, 1<<20)
			if err != nil || typ != o.Type || HashObject(typ, data) != o.ID {
				t.Fatalf("the pack written serves %s as a %s: %v", o.ID, typ, err)
			}
		}
		// ...which reads again as the same objects, with nothing to resolve.
		var again bytes.Buffer
		u2, err := Unpack(context.Background(), bytes.NewReader(out.Bytes()), int64(out.Len()), &again, UnpackOptions{MaxObjectBytes: 1 << 20})
		if err != nil {
			t.Fatalf("the pack written is refused: %s", err)
		}
		if len(u2.Objects) != len(u.Objects) {
			t.Fatalf("the pack written reads as %d objects, and %d were written", len(u2.Objects), len(u.Objects))
		}
		for i := range u.Objects {
			if u2.Objects[i].ID != u.Objects[i].ID {
				t.Fatalf("the pack written reads as other objects")
			}
		}
	})
}

func FuzzIdx(f *testing.F) {
	var idx bytes.Buffer
	WriteIdx(&idx, []PackedObject{{ID: ID{1}, Offset: 12}, {ID: ID{0xff, 2}, Offset: 1 << 33, CRC32: 7}, {ID: ID{0xff, 3}, Offset: 99}}, [20]byte{9})
	f.Add(idx.Bytes())
	idx.Reset()
	WriteIdx(&idx, nil, [20]byte{})
	f.Add(idx.Bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		// The checksum is made to match, as the pack's trailer is for FuzzUnpack.
		if len(data) >= sha1.Size {
			data = bytes.Clone(data)
			sum := sha1.Sum(data[:len(data)-sha1.Size])
			copy(data[len(data)-sha1.Size:], sum[:])
		}
		x, err := ParseIdx(data)
		if err != nil {
			return
		}
		objects := make([]PackedObject, x.Len())
		for i := range objects {
			objects[i] = PackedObject{ID: x.ID(i), Offset: x.Offset(i), CRC32: x.CRC32(i)}
			if j, ok := x.Find(x.ID(i)); !ok || j != i {
				t.Fatalf("the index finds its object %d at %d, %t", i, j, ok)
			}
		}
		// An index read is the index written back: it has one spelling.
		var again bytes.Buffer
		if err := WriteIdx(&again, objects, x.Checksum()); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(again.Bytes(), data) {
			t.Fatalf("an index of %d bytes is written back as %d others", len(data), again.Len())
		}
	})
}

// applyPlainly is a delta's result as the format describes it, instruction by instruction, with
// none of the checks made before the result is allocated: what applyDelta answers is held to it.
func applyPlainly(base, d []byte) ([]byte, bool) {
	baseSize, resultSize, n, err := deltaSizes(d)
	if err != nil || baseSize != uint64(len(base)) || resultSize > 1<<20 {
		return nil, false
	}
	var out []byte
	for i := n; i < len(d); {
		op := d[i]
		i++
		if op == 0 {
			return nil, false
		}
		if op&0x80 == 0 {
			if i+int(op) > len(d) {
				return nil, false
			}
			out = append(out, d[i:i+int(op)]...)
			i += int(op)
			continue
		}
		var fields [7]uint64
		for bit := range 7 {
			if op&(1<<bit) != 0 {
				if i >= len(d) {
					return nil, false
				}
				fields[bit] = uint64(d[i])
				i++
			}
		}
		offset := fields[0] | fields[1]<<8 | fields[2]<<16 | fields[3]<<24
		length := fields[4] | fields[5]<<8 | fields[6]<<16
		if length == 0 {
			length = 0x10000
		}
		if offset+length > uint64(len(base)) {
			return nil, false
		}
		out = append(out, base[offset:offset+length]...)
		if uint64(len(out)) > resultSize {
			return nil, false
		}
	}
	return out, uint64(len(out)) == resultSize
}

func FuzzDelta(f *testing.F) {
	d, _ := edit([]byte("0123456789"), "abc")
	f.Add([]byte("0123456789"), d)
	f.Add([]byte("0123456789"), delta(10, 5, copyOp(8, 5)))
	f.Add([]byte{}, delta(0, 0))
	f.Fuzz(func(t *testing.T, base, d []byte) {
		got, err := applyDelta(base, d, 1<<20)
		want, ok := applyPlainly(base, d)
		if (err == nil) != ok || (ok && !bytes.Equal(got, want)) {
			t.Fatalf("applyDelta answers %d bytes and %v, where the delta is %d bytes and %t", len(got), err, len(want), ok)
		}
	})
}

// written is a packet as it reads, so that what is read and what is written again compare.
type written struct {
	kind PktKind
	data string
}

func readPackets(r io.Reader) ([]written, error) {
	var got []written
	pkts := NewPktReader(r)
	for {
		kind, data, err := pkts.Next()
		if err != nil {
			return got, err
		}
		got = append(got, written{kind, string(data)})
	}
}

func FuzzPktLine(f *testing.F) {
	f.Add([]byte("0014command=ls-refs\n000100090peel0000"))
	f.Add([]byte("0006\x01x0006\x02p0008\x03err0000"))
	f.Add([]byte("0002000300040005x"))
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := readPackets(bytes.NewReader(data))
		if err == nil {
			t.Fatal("packets read with no end")
		}
		var again bytes.Buffer
		for _, p := range got {
			switch p.kind {
			case PktData:
				if err := WritePktString(&again, p.data); err != nil {
					t.Fatal(err)
				}
			case PktFlush:
				WriteFlush(&again)
			case PktDelim:
				WriteDelim(&again)
			case PktResponseEnd:
				WriteResponseEnd(&again)
			}
		}
		// What was read is written back in as many bytes, and reads as the same packets.
		if !bytes.EqualFold(again.Bytes(), data[:again.Len()]) {
			t.Fatalf("%q is written back as %q", data, again.Bytes())
		}
		back, err := readPackets(&again)
		if !errors.Is(err, io.EOF) || !slices.Equal(back, got) {
			t.Fatalf("%v reads back as %v: %v", got, back, err)
		}
		// And a side-band response of it is read to an error or its end, never past it.
		io.Copy(io.Discard, NewSidebandReader(NewPktReader(bytes.NewReader(data))))
	})
}

func FuzzCommit(f *testing.F) {
	f.Add([]byte(head("A <a@example.com> 1 +0000")))
	f.Add([]byte("tree " + emptyID + "\nparent " + blobID + "\nauthor  <> 0 -0000\ncommitter C <c> 1 +0100\ngpgsig a\n \n b\nmergetag\n\nmessage"))
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := ParseCommit(data)
		if err != nil {
			return
		}
		again, err := c.Encode()
		if err != nil {
			t.Fatalf("%q reads, and is not written back: %s", data, err)
		}
		back, err := ParseCommit(again)
		if err != nil || !back.equal(c) {
			t.Fatalf("%q reads as %+v, and written back as %+v", data, c, back)
		}
	})
}

func FuzzTag(f *testing.F) {
	f.Add([]byte("object " + blobID + "\ntype blob\ntag v1\ntagger T <t@example.com> 1 +0000\n\nmessage\n"))
	f.Add([]byte("object " + blobID + "\ntype commit\ntag v2.6.11\ntagger\n\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		tag, err := ParseTag(data)
		if err != nil {
			return
		}
		again, err := tag.Encode()
		if err != nil {
			t.Fatalf("%q reads, and is not written back: %s", data, err)
		}
		back, err := ParseTag(again)
		if err != nil || !back.equal(tag) {
			t.Fatalf("%q reads as %+v, and written back as %+v", data, tag, back)
		}
	})
}

func FuzzTree(f *testing.F) {
	blob := HashObject(TypeBlob, []byte("x"))
	f.Add([]byte(entry("100644", "a", blob) + entry("100644", "a-b", blob) + entry("40000", "a", HashObject(TypeTree, nil))))
	f.Add([]byte(entry("100644", "a.b", blob) + entry("40000", "a", HashObject(TypeTree, nil)) + entry("160000", "sub", blob)))
	f.Fuzz(func(t *testing.T, data []byte) {
		entries, err := ParseTree(data)
		if err != nil {
			return
		}
		// A tree has one spelling, so what reads is written back byte for byte.
		again, err := EncodeTree(entries)
		if err != nil || !bytes.Equal(again, data) {
			t.Fatalf("%q reads as %v, and is written back as %q: %v", data, entries, again, err)
		}
	})
}
