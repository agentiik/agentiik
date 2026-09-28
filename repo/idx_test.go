package repo

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestAnIndexOfLargeOffsetsIsTheOneGitReads(t *testing.T) {
	objects := []PackedObject{
		{ID: HashObject(TypeBlob, []byte("a")), Offset: 12, CRC32: 1},
		{ID: HashObject(TypeBlob, []byte("b")), Offset: 1<<31 - 1, CRC32: 2},
		{ID: HashObject(TypeBlob, []byte("c")), Offset: 1 << 31, CRC32: 3},
		{ID: HashObject(TypeBlob, []byte("d")), Offset: 5 << 32, CRC32: 4},
		{ID: HashObject(TypeBlob, []byte("e")), Offset: 1 << 33, CRC32: 5},
	}
	var idx bytes.Buffer
	if err := WriteIdx(&idx, objects, [20]byte{7}); err != nil {
		t.Fatal(err)
	}
	x, err := ParseIdx(idx.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	git(t, dir, nil, "init", "-q", ".")
	shown := string(git(t, dir, idx.Bytes(), "show-index"))
	for _, o := range objects {
		if line := fmt.Sprintf("%d %s (%08x)", o.Offset, o.ID, o.CRC32); !strings.Contains(shown, line) {
			t.Errorf("git show-index does not read %s:\n%s", line, shown)
		}
		i, ok := x.Find(o.ID)
		if !ok || x.Offset(i) != o.Offset || x.CRC32(i) != o.CRC32 {
			t.Errorf("the index reads %s at %d with %08x", o.ID, x.Offset(i), x.CRC32(i))
		}
	}
	if _, ok := x.Find(HashObject(TypeBlob, []byte("f"))); ok {
		t.Error("the index finds an object it does not name")
	}
	if x.Checksum() != ([20]byte{7}) {
		t.Errorf("the index is for the pack %x", x.Checksum())
	}
}

// rechecksum gives an index changed by hand the checksum it would have had.
func rechecksum(b []byte) []byte {
	b = bytes.Clone(b)
	sum := sha1.Sum(b[:len(b)-sha1.Size])
	copy(b[len(b)-sha1.Size:], sum[:])
	return b
}

func TestAnIndexThatIsNotOneIsRefused(t *testing.T) {
	a, b := HashObject(TypeBlob, []byte("a")), HashObject(TypeBlob, []byte("b"))
	if bytes.Compare(a[:], b[:]) > 0 {
		a, b = b, a
	}
	var good bytes.Buffer
	WriteIdx(&good, []PackedObject{{ID: a, Offset: 12}, {ID: b, Offset: 1 << 32}}, [20]byte{})
	at := func(n int) int { return idxHeaderLen + n }
	idsAt, offsetsAt := at(0), at(2*20+2*4)
	change := func(f func(b []byte)) []byte {
		c := bytes.Clone(good.Bytes())
		f(c)
		return rechecksum(c)
	}
	for name, c := range map[string]struct {
		idx  []byte
		says string
	}{
		"another magic number":           {change(func(b []byte) { b[0] = 'x' }), "magic number"},
		"version 1":                      {change(func(b []byte) { binary.BigEndian.PutUint32(b[4:], 1) }), "version 1"},
		"a checksum that does not match": {flipLast(good.Bytes()), "does not match its checksum"},
		"counts that go down":            {change(func(b []byte) { binary.BigEndian.PutUint32(b[8+4*int(a[0]):], 5) }), "counts"},
		"more objects than it holds":     {change(func(b []byte) { binary.BigEndian.PutUint32(b[8+4*255:], 3) }), "cannot hold"},
		"an ID twice": {change(func(b []byte) {
			copy(b[idsAt+20:], b[idsAt:idsAt+20])
		}), "out of order or twice"},
		"an offset inside the pack's header": {change(func(b []byte) { binary.BigEndian.PutUint32(b[offsetsAt:], 4) }), "inside the pack's header"},
		"a 64-bit offset that is not there": {change(func(b []byte) {
			binary.BigEndian.PutUint32(b[offsetsAt+4:], 0x80000001)
		}), "64-bit offset 1 where the next is 0"},
		"a 64-bit offset that fits in 31 bits": {change(func(b []byte) {
			binary.BigEndian.PutUint64(b[offsetsAt+8:], 100)
		}), "as a 64-bit offset"},
		"a 64-bit offset nothing names": {rechecksum(append(bytes.Clone(good.Bytes()[:len(good.Bytes())-40]), append(make([]byte, 8), good.Bytes()[len(good.Bytes())-40:]...)...)), "holds 2 64-bit offsets and names 1"},
		"nothing at all":                {nil, "magic number"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseIdx(c.idx); err == nil || !strings.Contains(err.Error(), c.says) {
				t.Errorf("read with %v, where it says %q", err, c.says)
			}
		})
	}
	if _, err := ParseIdx(good.Bytes()); err != nil {
		t.Errorf("the index the cases change is itself refused: %s", err)
	}
	if err := WriteIdx(io.Discard, []PackedObject{{ID: a, Offset: 12}, {ID: a, Offset: 40}}, [20]byte{}); err == nil {
		t.Error("an index naming an object twice is written")
	}
}

func TestAPackIsOpenedOnlyThroughItsOwnIndex(t *testing.T) {
	var pack bytes.Buffer
	w, _ := NewPackWriter(&pack, 2)
	a, _ := w.Add(TypeBlob, []byte("a"))
	w.Add(TypeBlob, []byte("b"))
	checksum, _ := w.Close()
	var idx bytes.Buffer
	WriteIdx(&idx, w.Objects(), checksum)
	x, _ := ParseIdx(idx.Bytes())
	p := pack.Bytes()
	if _, err := OpenPack(bytes.NewReader(p), int64(len(p)), x); err != nil {
		t.Fatal(err)
	}
	other := flipLast(p)
	if _, err := OpenPack(bytes.NewReader(other), int64(len(other)), x); err == nil || !strings.Contains(err.Error(), "trailer") {
		t.Errorf("a pack whose trailer is not the index's opens with %v", err)
	}
	counted := resum(append(appendPackHeader(nil, 3), p[packHeaderLen:]...))
	if _, err := OpenPack(bytes.NewReader(counted), int64(len(counted)), x); err == nil || !strings.Contains(err.Error(), "3 objects") {
		t.Errorf("a pack counting another number of objects opens with %v", err)
	}
	// An index naming an object for bytes that are another's: the object is served only as far as
	// its last read, which answers the error.
	objects := w.Objects()
	for i := range objects {
		if objects[i].ID == a {
			objects[i].ID = HashObject(TypeBlob, []byte("not a"))
		}
	}
	var lying bytes.Buffer
	WriteIdx(&lying, objects, checksum)
	lx, _ := ParseIdx(lying.Bytes())
	lp, err := OpenPack(bytes.NewReader(p), int64(len(p)), lx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadObject(context.Background(), lp, HashObject(TypeBlob, []byte("not a")), 10); err == nil || !strings.Contains(err.Error(), "reads as "+a.String()) {
		t.Errorf("an object whose bytes are another's reads with %v", err)
	}
}
