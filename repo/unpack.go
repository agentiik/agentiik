package repo

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/agentiik/agentiik/agk"
)

// UnpackOptions says what a pack being received is resolved against, bounded by, and shown to.
type UnpackOptions struct {
	// Bases finds the objects a thin pack's deltas are made against, which the repository holds
	// already and the pack does not. Nil where the pack must hold every base itself.
	Bases Lookup
	// MaxObjectBytes bounds one object, artifact_max_bytes; zero is its default. An object is
	// refused past it before a byte of its content is inflated.
	MaxObjectBytes int64
	// Visit is handed every commit, tree and tag the pack holds, once it has been checked, with
	// its ID and its content, which it must not keep. An error it answers stops the unpacking and
	// is answered by Unpack. Objects are handed over as they are read, before the pack is known to
	// be whole, so nothing Visit learns counts until Unpack has answered no error.
	Visit func(id ID, t Type, data []byte) error
}

// Unpacked is what Unpack wrote.
type Unpacked struct {
	// Objects are the objects of the pack written, sorted by ID, for its index.
	Objects []PackedObject
	// Checksum is the checksum of the pack written, which its index carries and its name is made
	// from.
	Checksum [20]byte
}

// Unpack reads the pack of size bytes that r holds, a pack pushed and written to a temporary file,
// and writes every object it holds whole to w as a pack of its own, which depends on no other.
//
// The pack is refused unless it is whole: a version 2 header, no more than MaxPackBytes and
// MaxPackObjects, every entry inflating to the size its header gives with a zlib checksum that
// matches, nothing after the last entry but a trailer that is the SHA-1 of what comes before it,
// and every object in it once. Every delta is resolved, against an entry of the pack or, in a thin
// pack, against an object Bases holds, through no more than MaxDeltaDepth deltas, and applied
// only where it reads inside its base and writes exactly the size it gives. Every commit, tree and
// tag is held to what git fsck --strict holds it to, and refused with an *ObjectError naming
// git's check.
//
// Whole entries are copied as they were sent, header rewritten and zlib stream untouched, and
// resolved deltas compressed once. Memory is bounded whatever the pack holds: a blob sent whole is
// inflated as it is read and never held, and what is held, a commit, a tree, a tag, a delta and
// its base, is at most MaxHeldBytes, with DeltaBaseCacheBytes of bases kept for the deltas made
// against them.
func Unpack(ctx context.Context, r io.ReaderAt, size int64, w io.Writer, opts UnpackOptions) (*Unpacked, error) {
	u, err := newUnpacker(ctx, r, size, w, opts)
	if err != nil {
		return nil, err
	}
	return u.run()
}

func newUnpacker(ctx context.Context, r io.ReaderAt, size int64, w io.Writer, opts UnpackOptions) (*unpacker, error) {
	if opts.MaxObjectBytes <= 0 {
		opts.MaxObjectBytes = agk.DefaultArtifactMaxBytes
	}
	switch {
	case size > MaxPackBytes:
		return nil, fmt.Errorf("repo: a pack of %d bytes, more than the %d one push may send", size, MaxPackBytes)
	case size < packHeaderLen+packTrailerLen:
		return nil, fmt.Errorf("repo: a pack of %d bytes, shorter than a header and a trailer", size)
	}
	return &unpacker{ctx: ctx, r: r, w: w, end: size - packTrailerLen, opts: opts, refKids: map[ID][]int32{}}, nil
}

func (u *unpacker) run() (*Unpacked, error) {
	if err := u.read(); err != nil {
		return nil, err
	}
	if err := u.resolve(); err != nil {
		return nil, err
	}
	checksum, err := u.out.Close()
	if err != nil {
		return nil, err
	}
	return &Unpacked{Objects: u.out.Objects(), Checksum: checksum}, nil
}

// packEntry is one entry of the pack being read: some sixty bytes, which with the lists of what is
// made against what is what bounds the memory MaxPackObjects entries take.
type packEntry struct {
	// offset is where the entry begins, and data where its zlib stream does.
	offset, data int64
	// size is the size of its content once inflated: the object's, or the delta's.
	size int64
	// base is the position of the entry an OFS_DELTA is made against, or of the ID a REF_DELTA
	// names in the unpacker's refBases.
	base int32
	kind byte
	id   ID
}

func (e *packEntry) isDelta() bool { return e.kind == kindOfsDelta || e.kind == kindRefDelta }

type unpacker struct {
	ctx  context.Context
	r    io.ReaderAt
	w    io.Writer
	end  int64
	opts UnpackOptions
	out  *PackWriter
	z    io.ReadCloser

	entries []packEntry
	// refBases are the IDs REF_DELTA entries name, and refKids the entries made against each of
	// them not resolved yet. ofsKids are the entries made against each entry by offset: those of
	// entry i are ofsKids[ofsFrom[i]:ofsFrom[i+1]].
	refBases []ID
	refKids  map[ID][]int32
	ofsKids  []int32
	ofsFrom  []int32
	resolved int
	// held is how many bytes the frames of the resolution hold, and peak the most they have held
	// at once, which a test holds to the bound.
	held, peak int64
}

// read is the first pass: every entry's header, where it begins and ends, and the ID of every
// object sent whole, which is checked, handed to Visit and written as it is read.
func (u *unpacker) read() error {
	sum := sha1.New()
	c := &counter{r: bufio.NewReaderSize(io.TeeReader(io.NewSectionReader(u.r, 0, u.end), sum), 64<<10)}
	var head [packHeaderLen]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return fmt.Errorf("repo: the pack's header: %w", err)
	}
	count, err := readPackHeader(head[:])
	if err != nil {
		return err
	}
	if count > MaxPackObjects {
		return fmt.Errorf("repo: a pack of %d objects, more than the %d one push may send", count, MaxPackObjects)
	}
	// A header counting more entries than the bytes after it could hold is refused before memory
	// is taken for them: an entry is one byte of header and eight of zlib stream at the least.
	if int64(count) > (u.end-packHeaderLen)/9 {
		return fmt.Errorf("repo: a pack of %d bytes cannot hold the %d objects its header counts", u.end+packTrailerLen, count)
	}
	if u.out, err = NewPackWriter(u.w, int(count)); err != nil {
		return err
	}
	u.entries = make([]packEntry, 0, count)
	for range count {
		if err := u.ctx.Err(); err != nil {
			return err
		}
		offset := c.n
		if err := u.readEntry(c); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("repo: the entry at offset %d: %w", offset, err)
		}
	}
	if c.n != u.end {
		return fmt.Errorf("repo: %d bytes after the last of the pack's %d entries, before its trailer", u.end-c.n, count)
	}
	var trailer [packTrailerLen]byte
	if _, err := u.r.ReadAt(trailer[:], u.end); err != nil {
		return fmt.Errorf("repo: the pack's trailer: %w", err)
	}
	if !bytes.Equal(sum.Sum(nil), trailer[:]) {
		return errors.New("repo: the pack's trailer is not the checksum of what comes before it")
	}
	return nil
}

// readEntry reads one entry, the counter standing where it begins.
func (u *unpacker) readEntry(c *counter) error {
	e := packEntry{offset: c.n}
	kind, size, err := readEntryHeader(c, max(u.opts.MaxObjectBytes, maxHeld))
	if err != nil {
		return err
	}
	e.kind, e.size = kind, size
	switch {
	case kind == byte(TypeBlob):
		if size > u.opts.MaxObjectBytes {
			return fmt.Errorf("a blob of %d bytes, more than the %d an object may be", size, u.opts.MaxObjectBytes)
		}
	case Type(kind).valid(), e.isDelta():
		if size > maxHeld {
			return fmt.Errorf("%s of %d bytes, which is held whole, and the most that is is %d", kindName(kind), size, maxHeld)
		}
	default:
		return fmt.Errorf("an entry of kind %d, which is none of git's", kind)
	}
	switch kind {
	case kindOfsDelta:
		distance, err := readOfsDelta(c)
		if err != nil {
			return err
		}
		k, ok := slices.BinarySearchFunc(u.entries, e.offset-distance, func(b packEntry, off int64) int { return cmp.Compare(b.offset, off) })
		if distance <= 0 || !ok {
			return fmt.Errorf("a delta whose base is %d bytes before it, where no entry of the pack begins", distance)
		}
		e.base = int32(k)
	case kindRefDelta:
		var base ID
		if _, err := io.ReadFull(c, base[:]); err != nil {
			return err
		}
		e.base = int32(len(u.refBases))
		u.refBases = append(u.refBases, base)
	}
	e.data = c.n
	if u.z == nil {
		u.z, err = zlib.NewReader(c)
	} else {
		err = u.z.(zlib.Resetter).Reset(c, nil)
	}
	if err != nil {
		return fmt.Errorf("a zlib stream that does not begin as one: %w", err)
	}
	if e.isDelta() {
		n, err := io.Copy(io.Discard, io.LimitReader(u.z, size))
		if err == nil && n < size {
			err = fmt.Errorf("a zlib stream of %d bytes where its header gives %d", n, size)
		}
		if err == nil {
			err = atEnd(u.z)
		}
		if err != nil {
			return err
		}
		u.entries = append(u.entries, e)
		return nil
	}
	t := Type(kind)
	h := newObjectHash(t, size)
	var data []byte
	if t == TypeBlob {
		// A blob is never held: it is hashed as it is inflated, and copied as it was sent.
		var n int64
		if n, err = io.Copy(h, io.LimitReader(u.z, size)); err == nil && n < size {
			err = fmt.Errorf("a zlib stream of %d bytes where its header gives %d", n, size)
		}
	} else {
		// Read as it arrives rather than allocated at the size the header claims, so that a
		// header claiming more than its stream holds costs what the stream holds.
		if data, err = io.ReadAll(io.LimitReader(u.z, size)); err == nil && int64(len(data)) < size {
			err = fmt.Errorf("a zlib stream of %d bytes where its header gives %d", len(data), size)
		}
		h.Write(data)
	}
	if err == nil {
		err = atEnd(u.z)
	}
	if err != nil {
		return err
	}
	e.id = sumID(h)
	u.entries = append(u.entries, e)
	u.resolved++
	if t != TypeBlob {
		if err := u.check(e.id, t, data); err != nil {
			return err
		}
	}
	return u.out.addDeflated(e.id, t, size, io.NewSectionReader(u.r, e.data, c.n-e.data))
}

func kindName(kind byte) string {
	switch kind {
	case kindOfsDelta, kindRefDelta:
		return "a delta"
	}
	return "a " + Type(kind).String()
}

// check holds a commit, a tree or a tag to what git fsck --strict holds it to, and hands it to
// Visit.
func (u *unpacker) check(id ID, t Type, data []byte) error {
	var err error
	switch t {
	case TypeCommit:
		_, err = ParseCommit(data)
	case TypeTree:
		_, err = ParseTree(data)
	case TypeTag:
		_, err = ParseTag(data)
	}
	if oe := (*ObjectError)(nil); errors.As(err, &oe) {
		oe.ID = id
	}
	if err == nil && u.opts.Visit != nil {
		err = u.opts.Visit(id, t, data)
	}
	return err
}

// frame is an object on the path from the object a chain of deltas is made against down to the
// delta being resolved: an entry of the pack, or the base a thin pack names, and its content
// while it is held.
type frame struct {
	// entry is the entry's position, or -1 for a base the repository holds.
	entry int32
	id    ID
	t     Type
	data  []byte
}

// resolve is the second pass: every delta, resolved from the object its chain is made against,
// depth first, so that the deltas made against one object are resolved while it is held. The
// objects sent whole come first, then the bases a thin pack names, which only the repository
// holds.
func (u *unpacker) resolve() error {
	n := len(u.entries)
	u.ofsFrom = make([]int32, n+1)
	for _, e := range u.entries {
		if e.kind == kindOfsDelta {
			u.ofsFrom[e.base+1]++
		}
	}
	for i := 1; i <= n; i++ {
		u.ofsFrom[i] += u.ofsFrom[i-1]
	}
	u.ofsKids = make([]int32, u.ofsFrom[n])
	next := slices.Clone(u.ofsFrom[:n])
	for i, e := range u.entries {
		switch e.kind {
		case kindOfsDelta:
			u.ofsKids[next[e.base]] = int32(i)
			next[e.base]++
		case kindRefDelta:
			base := u.refBases[e.base]
			u.refKids[base] = append(u.refKids[base], int32(i))
		}
	}
	for i, e := range u.entries {
		if !e.isDelta() {
			if err := u.from(&frame{entry: int32(i), id: e.id, t: Type(e.kind)}); err != nil {
				return err
			}
		}
	}
	// A thin base may be one only once a delta of the pack has been resolved against another,
	// so the bases are looked up until a round finds none.
	for progress := true; progress && len(u.refKids) > 0 && u.opts.Bases != nil; {
		progress = false
		for _, id := range u.unresolvedBases() {
			if _, still := u.refKids[id]; !still {
				continue
			}
			t, data, err := ReadObject(u.ctx, u.opts.Bases, id, maxHeld)
			if errors.Is(err, ErrMissing) {
				continue
			}
			if err != nil {
				return fmt.Errorf("repo: the base %s of a thin pack: %w", id, err)
			}
			progress = true
			root := &frame{entry: -1, id: id, t: t, data: data}
			u.hold(data)
			if err := u.from(root); err != nil {
				return err
			}
		}
	}
	if missing := u.unresolvedBases(); len(missing) > 0 {
		return fmt.Errorf("repo: a delta against %s, which neither the pack nor the repository holds", missing[0])
	}
	if u.resolved != n {
		return fmt.Errorf("repo: %d of the pack's entries were never resolved", n-u.resolved)
	}
	return nil
}

func (u *unpacker) unresolvedBases() []ID {
	ids := make([]ID, 0, len(u.refKids))
	for id := range u.refKids {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b ID) int { return bytes.Compare(a[:], b[:]) })
	return ids
}

// kids are the entries made against an object, which are taken from what is left to resolve.
func (u *unpacker) kids(f *frame) []int32 {
	var kids []int32
	if f.entry >= 0 {
		kids = u.ofsKids[u.ofsFrom[f.entry]:u.ofsFrom[f.entry+1]]
	}
	if byID, ok := u.refKids[f.id]; ok {
		kids = append(slices.Clip(kids), byID...)
		delete(u.refKids, f.id)
	}
	return kids
}

// from resolves every delta whose chain begins at root.
func (u *unpacker) from(root *frame) error {
	defer u.release(root)
	kids := u.kids(root)
	if len(kids) == 0 {
		return nil
	}
	return u.descend([]*frame{root}, kids)
}

// descend resolves the deltas made against the last frame of stack, and those made against them.
func (u *unpacker) descend(stack []*frame, kids []int32) error {
	parent := stack[len(stack)-1]
	for _, k := range kids {
		if err := u.ctx.Err(); err != nil {
			return err
		}
		e := &u.entries[k]
		if len(stack) > MaxDeltaDepth {
			return fmt.Errorf("repo: the entry at offset %d: a chain of more than %d deltas", e.offset, MaxDeltaDepth)
		}
		base, err := u.dataOf(stack)
		if err != nil {
			return err
		}
		data, err := u.apply(base, k)
		if err != nil {
			return err
		}
		f := &frame{entry: k, id: HashObject(parent.t, data), t: parent.t, data: data}
		u.hold(data)
		e.id = f.id
		u.resolved++
		if f.t != TypeBlob {
			if err := u.check(f.id, f.t, data); err != nil {
				return err
			}
		}
		if err := u.out.add(f.id, f.t, data); err != nil {
			return err
		}
		if grandkids := u.kids(f); len(grandkids) > 0 {
			stack = append(stack, f)
			u.trim(stack)
			if err := u.descend(stack, grandkids); err != nil {
				return err
			}
			stack = stack[:len(stack)-1]
		}
		u.release(f)
	}
	return nil
}

// apply resolves the delta at entry k against base.
func (u *unpacker) apply(base []byte, k int32) ([]byte, error) {
	e := &u.entries[k]
	end := u.end
	if int(k)+1 < len(u.entries) {
		end = u.entries[k+1].offset
	}
	delta, err := inflate(io.NewSectionReader(u.r, e.data, end-e.data), e.size)
	if err == nil {
		var data []byte
		if data, err = applyDelta(base, delta, maxHeld); err == nil {
			return data, nil
		}
	}
	return nil, fmt.Errorf("repo: the entry at offset %d: %w", e.offset, err)
}

// dataOf answers the content of the last frame of stack, resolving it again where it was let go:
// from the nearest frame above it still holding its content, or from the root, read again.
func (u *unpacker) dataOf(stack []*frame) ([]byte, error) {
	top := len(stack) - 1
	if stack[top].data != nil {
		return stack[top].data, nil
	}
	j := top
	for j >= 0 && stack[j].data == nil {
		j--
	}
	if j < 0 {
		if err := u.load(stack[0]); err != nil {
			return nil, err
		}
		j = 0
	}
	for k := j + 1; k <= top; k++ {
		data, err := u.apply(stack[k-1].data, stack[k].entry)
		if err != nil {
			return nil, err
		}
		stack[k].data = data
		u.hold(data)
		u.trim(stack[:k+1])
	}
	return stack[top].data, nil
}

// load reads the content of the object a chain of deltas is made against.
func (u *unpacker) load(root *frame) error {
	if root.entry < 0 {
		_, data, err := ReadObject(u.ctx, u.opts.Bases, root.id, maxHeld)
		if err != nil {
			return fmt.Errorf("repo: the base %s of a thin pack: %w", root.id, err)
		}
		root.data = data
	} else {
		e := &u.entries[root.entry]
		if e.size > maxHeld {
			return fmt.Errorf("repo: the entry at offset %d: a delta against a %s of %d bytes, and what a delta is made against is held whole, at most %d", e.offset, root.t, e.size, maxHeld)
		}
		end := u.end
		if int(root.entry)+1 < len(u.entries) {
			end = u.entries[root.entry+1].offset
		}
		data, err := inflate(io.NewSectionReader(u.r, e.data, end-e.data), e.size)
		if err != nil {
			return fmt.Errorf("repo: the entry at offset %d: %w", e.offset, err)
		}
		root.data = data
	}
	u.hold(root.data)
	return nil
}

// trim lets go of the content of the frames nearest the root while more than DeltaBaseCacheBytes
// is held, keeping the last, which the next delta is made against.
func (u *unpacker) trim(stack []*frame) {
	for i := 0; i < len(stack)-1 && u.held > deltaBaseCache; i++ {
		u.release(stack[i])
	}
}

func (u *unpacker) hold(data []byte) {
	u.held += int64(len(data))
	u.peak = max(u.peak, u.held)
}

func (u *unpacker) release(f *frame) {
	u.held -= int64(len(f.data))
	f.data = nil
}
