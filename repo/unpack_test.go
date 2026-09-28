package repo

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"testing"
)

// builder writes packs entry by entry, including the ones git would never write, which is what the
// refusals are about.
type builder struct {
	body    []byte
	offsets []int64
}

func deflate(data []byte) []byte {
	var b bytes.Buffer
	z := zlib.NewWriter(&b)
	z.Write(data)
	z.Close()
	return b.Bytes()
}

// raw appends an entry of kind whose header gives size, with extra after the header, and stream as
// its zlib stream.
func (b *builder) raw(kind byte, size int64, extra, stream []byte) int {
	b.offsets = append(b.offsets, packHeaderLen+int64(len(b.body)))
	b.body = appendEntryHeader(b.body, kind, size)
	b.body = append(b.body, extra...)
	b.body = append(b.body, stream...)
	return len(b.offsets) - 1
}

func (b *builder) whole(t Type, data []byte) int {
	return b.raw(byte(t), int64(len(data)), nil, deflate(data))
}

func (b *builder) ofs(base int, delta []byte) int {
	distance := packHeaderLen + int64(len(b.body)) - b.offsets[base]
	return b.raw(kindOfsDelta, int64(len(delta)), appendOfs(nil, distance), deflate(delta))
}

func (b *builder) ref(base ID, delta []byte) int {
	return b.raw(kindRefDelta, int64(len(delta)), base[:], deflate(delta))
}

// appendOfs is git's encoding of how far back an OFS_DELTA's base is.
func appendOfs(b []byte, distance int64) []byte {
	var tmp [10]byte
	i := len(tmp) - 1
	tmp[i] = byte(distance & 0x7f)
	for distance >>= 7; distance > 0; distance >>= 7 {
		distance--
		i--
		tmp[i] = 0x80 | byte(distance&0x7f)
	}
	return append(b, tmp[i:]...)
}

// pack is the pack of the entries appended, its header counting count of them.
func (b *builder) pack(count int) []byte {
	p := appendPackHeader(nil, uint32(count))
	p = append(p, b.body...)
	sum := sha1.Sum(p)
	return append(p, sum[:]...)
}

func (b *builder) all() []byte { return b.pack(len(b.offsets)) }

// resum gives a pack changed by hand the trailer it would have had.
func resum(p []byte) []byte {
	p = bytes.Clone(p[:len(p)-packTrailerLen])
	sum := sha1.Sum(p)
	return append(p, sum[:]...)
}

func varint(n int) []byte {
	var b []byte
	for {
		c := byte(n & 0x7f)
		if n >>= 7; n > 0 {
			b = append(b, c|0x80)
			continue
		}
		return append(b, c)
	}
}

// delta is a delta against a base of baseSize bytes, giving resultSize, made of ops.
func delta(baseSize, resultSize int, ops ...[]byte) []byte {
	d := append(varint(baseSize), varint(resultSize)...)
	for _, op := range ops {
		d = append(d, op...)
	}
	return d
}

func copyOp(offset, length int) []byte {
	op := []byte{0x80}
	for i := range 4 {
		if c := byte(offset >> (8 * i)); c != 0 {
			op[0] |= 1 << i
			op = append(op, c)
		}
	}
	for i := range 3 {
		if c := byte(length >> (8 * i)); c != 0 {
			op[0] |= 0x10 << i
			op = append(op, c)
		}
	}
	return op
}

func insertOp(data []byte) []byte {
	var ops []byte
	for len(data) > 0 {
		n := min(len(data), 127)
		ops = append(ops, byte(n))
		ops = append(ops, data[:n]...)
		data = data[n:]
	}
	return ops
}

// edit is a delta turning base into base with suffix after it.
func edit(base []byte, suffix string) ([]byte, []byte) {
	result := append(bytes.Clone(base), suffix...)
	var ops [][]byte
	for off := 0; off < len(base); off += 0x10000 {
		ops = append(ops, copyOp(off, min(0x10000, len(base)-off)))
	}
	ops = append(ops, insertOp([]byte(suffix)))
	return delta(len(base), len(result), ops...), result
}

func unpackErr(t *testing.T, pack []byte, opts UnpackOptions) error {
	t.Helper()
	_, err := Unpack(context.Background(), bytes.NewReader(pack), int64(len(pack)), io.Discard, opts)
	return err
}

func TestAPackThatIsNotWholeIsRefused(t *testing.T) {
	blob := bytes.Repeat([]byte("a line of a file\n"), 400)
	good := func() *builder {
		b := &builder{}
		b.whole(TypeBlob, blob)
		return b
	}
	cut := good().all()
	cut = resum(append(cut[:len(cut)/2], make([]byte, packTrailerLen)...))
	junk := good()
	junk.body = append(junk.body, "junk"...)
	notZlib := &builder{}
	notZlib.raw(byte(TypeBlob), 5, nil, []byte("not a zlib stream"))
	short := &builder{}
	short.raw(byte(TypeBlob), 10, nil, deflate([]byte("12345")))
	long := &builder{}
	long.raw(byte(TypeBlob), 5, nil, deflate([]byte("1234567890")))
	checksum := &builder{}
	stream := deflate([]byte("hello"))
	stream[len(stream)-1] ^= 1
	checksum.raw(byte(TypeBlob), 5, nil, stream)
	kind5, kind0 := &builder{}, &builder{}
	kind5.raw(5, 5, nil, deflate([]byte("hello")))
	kind0.raw(0, 5, nil, deflate([]byte("hello")))
	large := &builder{}
	large.whole(TypeBlob, []byte("eleven byte"))

	for name, c := range map[string]struct {
		pack []byte
		opts UnpackOptions
		says string
	}{
		"a trailer that is not the checksum":       {flipLast(good().all()), UnpackOptions{}, "trailer is not the checksum"},
		"a pack cut short inside an entry":         {cut, UnpackOptions{}, "unexpected EOF"},
		"bytes after the last entry":               {junk.all(), UnpackOptions{}, "after the last"},
		"more entries counted than it holds":       {good().pack(2), UnpackOptions{}, "unexpected EOF"},
		"more entries counted than it could hold":  {good().pack(1000), UnpackOptions{}, "cannot hold"},
		"more entries than a push may send":        {good().pack(MaxPackObjects + 1), UnpackOptions{}, "more than the 1048576"},
		"a version other than 2":                   {versioned(good().all(), 3), UnpackOptions{}, "version 3"},
		"a zlib stream that is not one":            {notZlib.all(), UnpackOptions{}, "zlib"},
		"a zlib stream shorter than its header":    {short.all(), UnpackOptions{}, "where its header gives 10"},
		"a zlib stream longer than its header":     {long.all(), UnpackOptions{}, "longer than"},
		"a zlib stream whose checksum is not":      {checksum.all(), UnpackOptions{}, "checksum"},
		"an entry of the kind git reserves":        {kind5.all(), UnpackOptions{}, "kind 5"},
		"an entry of kind 0":                       {kind0.all(), UnpackOptions{}, "kind 0"},
		"a blob larger than an object may be":      {large.all(), UnpackOptions{MaxObjectBytes: 10}, "more than the 10"},
		"a header and a trailer, and nothing else": {[]byte("PACK"), UnpackOptions{}, "shorter than a header"},
		"no pack at all, but a pack-sized trailer": {resum(append([]byte("KCAP\x00\x00\x00\x02\x00\x00\x00\x00"), make([]byte, 20)...)), UnpackOptions{}, "begins with PACK"},
	} {
		t.Run(name, func(t *testing.T) {
			err := unpackErr(t, c.pack, c.opts)
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Errorf("read with %v, where it says %q", err, c.says)
			}
		})
	}

	if _, err := Unpack(context.Background(), bytes.NewReader(nil), MaxPackBytes+1, io.Discard, UnpackOptions{}); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("a pack past MaxPackBytes reads with %v, where it is refused before a byte is read", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := good().all()
	if _, err := Unpack(ctx, bytes.NewReader(p), int64(len(p)), io.Discard, UnpackOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("a pack read under a context cancelled reads with %v", err)
	}
}

func flipLast(p []byte) []byte {
	p = bytes.Clone(p)
	p[len(p)-1] ^= 1
	return p
}

func versioned(p []byte, v uint32) []byte {
	p = bytes.Clone(p)
	binary.BigEndian.PutUint32(p[4:], v)
	return resum(p)
}

func TestADeltaIsAppliedOnlyWithinItsBase(t *testing.T) {
	base := []byte("0123456789")
	for name, c := range map[string]struct {
		delta []byte
		says  string
	}{
		"a copy past the base's end":         {delta(10, 5, copyOp(8, 5)), "copying bytes 8 to 13 of a base of 10"},
		"a base of another size":             {delta(11, 5, copyOp(0, 5)), "applied to one of 10"},
		"a result shorter than it says":      {delta(10, 6, copyOp(0, 5)), "where it said 6"},
		"a result longer than it says":       {delta(10, 4, copyOp(0, 5)), "writing past the 4 bytes"},
		"the instruction git reserves":       {append(delta(10, 1), 0), "the instruction 0"},
		"an insert past the delta's end":     {append(delta(10, 5), 5, 'a'), "ends inside the bytes it inserts"},
		"a copy that ends inside its fields": {append(delta(10, 5), 0x91), "ends inside a copy instruction"},
		"sizes that do not end":              {[]byte{0x80, 0x80}, "ends inside its sizes"},
	} {
		t.Run(name, func(t *testing.T) {
			b := &builder{}
			b.ofs(b.whole(TypeBlob, base), c.delta)
			if err := unpackErr(t, b.all(), UnpackOptions{}); err == nil || !strings.Contains(err.Error(), c.says) {
				t.Errorf("read with %v, where it says %q", err, c.says)
			}
			if _, err := applyDelta(base, c.delta, 1<<20); err == nil {
				t.Error("applyDelta applies it")
			}
		})
	}
}

func TestAnOfsDeltaNamesTheBeginningOfAnEarlierEntry(t *testing.T) {
	d, _ := edit([]byte("base"), "!")
	for name, distance := range map[string]int64{
		"no distance":             0,
		"inside the entry before": 3,
		"before the pack begins":  1000,
	} {
		b := &builder{}
		b.whole(TypeBlob, []byte("base"))
		b.raw(kindOfsDelta, int64(len(d)), appendOfs(nil, distance), deflate(d))
		if err := unpackErr(t, b.all(), UnpackOptions{}); err == nil || !strings.Contains(err.Error(), "where no entry of the pack begins") {
			t.Errorf("a base %s reads with %v", name, err)
		}
	}
}

// chain is a pack of a blob and n deltas, each made against the one before it by offset.
func chain(n int) ([]byte, []byte) {
	b := &builder{}
	data := []byte("the first version\n")
	prev := b.whole(TypeBlob, data)
	for i := range n {
		var d []byte
		d, data = edit(data, fmt.Sprintf("version %d\n", i+1))
		prev = b.ofs(prev, d)
	}
	return b.all(), data
}

func TestAChainOfFiftyDeltasIsResolvedAndOneOfFiftyOneIsNot(t *testing.T) {
	pack, last := chain(MaxDeltaDepth)
	_, u := unpack(t, pack, UnpackOptions{})
	if len(u.Objects) != MaxDeltaDepth+1 || !holds(u, HashObject(TypeBlob, last)) {
		t.Errorf("a chain of %d deltas reads as %d objects", MaxDeltaDepth, len(u.Objects))
	}
	pack, _ = chain(MaxDeltaDepth + 1)
	if err := unpackErr(t, pack, UnpackOptions{}); err == nil || !strings.Contains(err.Error(), "a chain of more than 50 deltas") {
		t.Errorf("a chain of %d deltas reads with %v", MaxDeltaDepth+1, err)
	}
	// A stored pack is held to the same bound, however it was written.
	b := &builder{}
	data := []byte("x")
	prev := b.whole(TypeBlob, data)
	for i := range MaxDeltaDepth + 1 {
		var d []byte
		d, data = edit(data, fmt.Sprint(i))
		prev = b.ofs(prev, d)
	}
	st := storeBuilt(t, b)
	if _, err := st.OpenObject(context.Background(), HashObject(TypeBlob, data)); err == nil || !strings.Contains(err.Error(), "a chain of more than 50 deltas") {
		t.Errorf("a stored chain of %d deltas opens with %v", MaxDeltaDepth+1, err)
	}
}

// storeBuilt opens a pack built by hand through an index written for it, deltas and all, since
// Unpack would write it whole.
func storeBuilt(t *testing.T, b *builder) *Pack {
	t.Helper()
	p := b.all()
	objects := make([]PackedObject, len(b.offsets))
	data := map[int][]byte{}
	for i, off := range b.offsets {
		// Resolve every entry by hand, in order, since each is made against an earlier one.
		c := &counter{r: bufioReader(p[off:])}
		kind, size, err := readEntryHeader(c, 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		var content []byte
		if kind == kindOfsDelta {
			distance, _ := readOfsDelta(c)
			d, _ := inflate(c, size)
			base := data[indexOf(b.offsets, off-distance)]
			if content, err = applyDelta(base, d, 1<<30); err != nil {
				t.Fatal(err)
			}
		} else {
			content, _ = inflate(c, size)
		}
		data[i] = content
		objects[i] = PackedObject{ID: HashObject(TypeBlob, content), Offset: off}
	}
	var idx bytes.Buffer
	if err := WriteIdx(&idx, objects, [20]byte(p[len(p)-20:])); err != nil {
		t.Fatal(err)
	}
	x, err := ParseIdx(idx.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	pack, err := OpenPack(bytes.NewReader(p), int64(len(p)), x)
	if err != nil {
		t.Fatal(err)
	}
	return pack
}

func bufioReader(b []byte) *bufio.Reader { return bufio.NewReader(bytes.NewReader(b)) }

func indexOf(offsets []int64, off int64) int {
	for i, o := range offsets {
		if o == off {
			return i
		}
	}
	return -1
}

func holds(u *Unpacked, id ID) bool {
	for _, o := range u.Objects {
		if o.ID == id {
			return true
		}
	}
	return false
}

// memory is a repository holding the objects put in it.
type memory map[ID][]byte

func (m memory) put(t Type, data []byte) ID {
	id := HashObject(t, data)
	m[id] = data
	return id
}

func (m memory) OpenObject(_ context.Context, id ID) (ObjectReader, error) {
	data, ok := m[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrMissing, id)
	}
	return &heldObject{Reader: bytes.NewReader(data), t: TypeBlob}, nil
}

func TestDeltasAreResolvedWhateverOrderTheyComeIn(t *testing.T) {
	repo := memory{}
	thin := []byte(strings.Repeat("a base the repository holds\n", 50))
	thinID := repo.put(TypeBlob, thin)

	b := &builder{}
	want := map[ID]bool{}
	// A REF_DELTA against an object resolved from a thin base, which comes after it in the pack:
	// its base is known only once the thin one has been resolved.
	d1, r1 := edit(thin, "one\n")
	d2, r2 := edit(r1, "two\n")
	b.ref(HashObject(TypeBlob, r1), d2)
	b.ref(thinID, d1)
	// An OFS_DELTA against that REF_DELTA, and a REF_DELTA against an object sent whole later.
	d3, r3 := edit(r2, "three\n")
	b.ofs(0, d3)
	whole := []byte(strings.Repeat("sent whole\n", 20))
	d4, r4 := edit(whole, "four\n")
	b.ref(HashObject(TypeBlob, whole), d4)
	b.whole(TypeBlob, whole)
	for _, data := range [][]byte{r1, r2, r3, r4, whole} {
		want[HashObject(TypeBlob, data)] = true
	}
	out, u := unpack(t, b.all(), UnpackOptions{Bases: repo})
	got := map[ID]bool{}
	for _, o := range u.Objects {
		got[o.ID] = true
	}
	if !maps.Equal(got, want) {
		t.Errorf("read %d objects, and the pack holds %d", len(got), len(want))
	}
	st := store(t, out, u)
	for id := range want {
		if _, _, err := ReadObject(context.Background(), st, id, 1<<20); err != nil {
			t.Error(err)
		}
	}
	if err := unpackErr(t, b.all(), UnpackOptions{}); err == nil || !strings.Contains(err.Error(), "a delta against "+thinID.String()) {
		t.Errorf("without the repository, the pack reads with %v, which does not name the base it lacks", err)
	}
}

func TestResolvingDeltasHoldsItsBasesWithinTheCache(t *testing.T) {
	// A blob and a chain of twelve deltas, each made against the one before, then a second delta
	// against each link, which comes after the whole chain: once the chain's end is resolved, every
	// link is needed again, and one let go is resolved again from the blob, down the chain.
	build := func(root []byte, b *builder, rootIndex int, thinBase *ID) map[ID]bool {
		want := map[ID]bool{}
		type link struct {
			index int
			data  []byte
		}
		chain := []link{{rootIndex, root}}
		against := func(l link, d []byte) int {
			if l.index < 0 {
				return b.ref(*thinBase, d)
			}
			return b.ofs(l.index, d)
		}
		for i := range 12 {
			d, data := edit(chain[i].data, fmt.Sprintf("link %d\n", i))
			chain = append(chain, link{against(chain[i], d), data})
			want[HashObject(TypeBlob, data)] = true
		}
		for i, l := range chain {
			d, data := edit(l.data, fmt.Sprintf("beside link %d\n", i))
			against(l, d)
			want[HashObject(TypeBlob, data)] = true
		}
		return want
	}
	root := bytes.Repeat([]byte("a kilobyte, near enough, of a file that changes little\n"), 20)
	b := &builder{}
	want := build(root, b, b.whole(TypeBlob, root), nil)
	want[HashObject(TypeBlob, root)] = true
	repo := memory{}
	thinRoot := append(bytes.Clone(root), "held by the repository\n"...)
	thinID := repo.put(TypeBlob, thinRoot)
	want2 := build(thinRoot, b, -1, &thinID)
	pack := b.all()

	for _, cache := range []int64{DeltaBaseCacheBytes, 3 << 10, 1} {
		t.Run(fmt.Sprint(cache), func(t *testing.T) {
			defer func(was int64) { deltaBaseCache = was }(deltaBaseCache)
			deltaBaseCache = cache
			var out bytes.Buffer
			u, err := newUnpacker(context.Background(), bytes.NewReader(pack), int64(len(pack)), &out, UnpackOptions{Bases: repo})
			if err != nil {
				t.Fatal(err)
			}
			unpacked, err := u.run()
			if err != nil {
				t.Fatal(err)
			}
			got := map[ID]bool{}
			for _, o := range unpacked.Objects {
				got[o.ID] = true
			}
			all := maps.Clone(want)
			maps.Copy(all, want2)
			if !maps.Equal(got, all) {
				t.Fatalf("read %d objects, and the pack holds %d", len(got), len(all))
			}
			// What is held at once is the cache, the base the next delta is made against, and
			// that delta's result.
			largest := int64(len(root)) + 200
			if u.peak > max(cache, largest)+2*largest {
				t.Errorf("held %d bytes at once, with a cache of %d and objects of %d at most", u.peak, cache, largest)
			}
			if u.held != 0 {
				t.Errorf("still holds %d bytes once every delta is resolved", u.held)
			}
		})
	}
}

func TestWhatIsHeldWholeIsBounded(t *testing.T) {
	defer func(was int64) { maxHeld = was }(maxHeld)
	maxHeld = 100
	big := bytes.Repeat([]byte("x"), 150)

	b := &builder{}
	b.whole(TypeTree, big)
	if err := unpackErr(t, b.all(), UnpackOptions{}); err == nil || !strings.Contains(err.Error(), "held whole") {
		t.Errorf("a tree larger than what is held reads with %v", err)
	}
	b = &builder{}
	d, _ := edit(big, "!")
	b.ofs(b.whole(TypeBlob, big), d)
	if err := unpackErr(t, b.all(), UnpackOptions{}); err == nil || !strings.Contains(err.Error(), "held whole") {
		t.Errorf("a delta whose result is larger than what is held reads with %v", err)
	}
	b = &builder{}
	b.ofs(b.whole(TypeBlob, big), delta(150, 10, copyOp(0, 10)))
	if err := unpackErr(t, b.all(), UnpackOptions{}); err == nil || !strings.Contains(err.Error(), "a delta against a blob of 150 bytes") {
		t.Errorf("a delta against a blob larger than what is held reads with %v", err)
	}
	b = &builder{}
	b.whole(TypeTree, big[:50])
	if err := unpackErr(t, b.all(), UnpackOptions{MaxObjectBytes: 40}); err == nil || !strings.Contains(err.Error(), "a tree of 50 bytes, more than the 40 an object may be") {
		t.Errorf("a tree larger than artifact_max_bytes reads with %v", err)
	}
	b = &builder{}
	d, _ = edit(big[:30], "twenty more bytes...")
	b.ofs(b.whole(TypeBlob, big[:30]), d)
	if err := unpackErr(t, b.all(), UnpackOptions{MaxObjectBytes: 40}); err == nil || !strings.Contains(err.Error(), "result is 50 bytes, more than the 40") {
		t.Errorf("a delta whose result is larger than artifact_max_bytes reads with %v", err)
	}
	b = &builder{}
	b.whole(TypeBlob, bytes.Repeat([]byte("y"), 1000))
	if _, err := Unpack(context.Background(), bytes.NewReader(b.all()), int64(len(b.all())), io.Discard, UnpackOptions{}); err != nil {
		t.Errorf("a blob sent whole, which is never held, is refused with %v", err)
	}
}

func TestAnObjectIsInAPackOnce(t *testing.T) {
	b := &builder{}
	b.whole(TypeBlob, []byte("same"))
	b.whole(TypeBlob, []byte("same"))
	if err := unpackErr(t, b.all(), UnpackOptions{}); err == nil || !strings.Contains(err.Error(), "twice in one pack") {
		t.Errorf("an object sent twice reads with %v", err)
	}
	b = &builder{}
	d, _ := edit([]byte("base"), "")
	b.ofs(b.whole(TypeBlob, []byte("base")), d)
	if err := unpackErr(t, b.all(), UnpackOptions{}); err == nil || !strings.Contains(err.Error(), "twice in one pack") {
		t.Errorf("an object sent whole and as a delta reads with %v", err)
	}
}

func TestAnObjectGitFsckRefusesIsRefusedInAPushedPack(t *testing.T) {
	tree, _ := EncodeTree([]TreeEntry{{Name: "a", Mode: ModeFile, ID: HashObject(TypeBlob, []byte("a"))}})
	bad := []byte(strings.Replace(string(tree), "a\x00", ".git\x00", 1))
	bad = []byte(strings.Replace(string(bad), "100644 ", "40000 ", 1))
	for name, build := range map[string]func(*builder){
		"sent whole": func(b *builder) { b.whole(TypeTree, bad) },
		"resolved from a delta": func(b *builder) {
			b.ofs(b.whole(TypeTree, tree), delta(len(tree), len(bad), insertOp(bad)))
		},
	} {
		b := &builder{}
		build(b)
		err := unpackErr(t, b.all(), UnpackOptions{})
		var oe *ObjectError
		if !errors.As(err, &oe) || oe.Check != checkHasDotgit || oe.ID != HashObject(TypeTree, bad) {
			t.Errorf("a tree holding .git %s reads with %v", name, err)
		}
	}
	b := &builder{}
	b.whole(TypeCommit, []byte("tree "+emptyID+"\n"))
	visited := false
	err := unpackErr(t, b.all(), UnpackOptions{Visit: func(ID, Type, []byte) error { visited = true; return nil }})
	var oe *ObjectError
	if !errors.As(err, &oe) || oe.Check != checkMissingAuthor || visited {
		t.Errorf("a commit with no author reads with %v, and is handed to Visit: %t", err, visited)
	}
	stop := errors.New("stop")
	b = &builder{}
	b.whole(TypeTree, tree)
	if err := unpackErr(t, b.all(), UnpackOptions{Visit: func(ID, Type, []byte) error { return stop }}); !errors.Is(err, stop) {
		t.Errorf("Visit's refusal is answered as %v", err)
	}
}
