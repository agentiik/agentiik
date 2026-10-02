package store

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"sync"

	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/repo"
)

// Prefix is what every key of one repository starts with, <namespace>/git/<repository>/, ending in
// the separator so that it reads as a prefix and never as a name.
func Prefix(namespace, repository string) string {
	return namespace + "/git/" + repository + "/"
}

// PackKey is the key a pack is kept under, <namespace>/git/<repository>/pack-<name>.pack.
func PackKey(namespace, repository, name string) string {
	return Prefix(namespace, repository) + "pack-" + name + ".pack"
}

// IdxKey is the key a pack's index is kept under, <namespace>/git/<repository>/pack-<name>.idx.
func IdxKey(namespace, repository, name string) string {
	return Prefix(namespace, repository) + "pack-" + name + ".idx"
}

// IdxCacheBytes is how many bytes of indexes a Store keeps read: 64 MiB, the indexes of some two
// million objects, where a workflow repository holds thousands in its whole history. Past it the
// least recently read are let go and read again when next asked for. An index larger than all of it
// is read for each lookup that needs it and not kept.
const IdxCacheBytes = 64 << 20

// maxIdxBytes is the largest index read: 1 GiB, the index of some 38 million objects. An index is
// written here and read whole, and this bounds what one written wrong, or a file put in its place,
// makes the API hold.
const maxIdxBytes = 1 << 30

// Store keeps the packs of the workflow repositories of an installation. One is made for the
// process, so that its indexes are read once for every request.
type Store struct {
	objects artifact.Ranged
	indexes *idxCache
}

// New keeps packs in objects, which must read a range of an object without reading the bytes before
// it: a pack runs to gigabytes, and a fetch reads it one entry at a time.
func New(objects artifact.Objects) (*Store, error) {
	ranged, ok := objects.(artifact.Ranged)
	if !ok {
		return nil, errors.New("store: that object store cannot read a range of an object, and a pack is read one entry at a time and never whole: the built-in store, AGK_OBJECTS_DIR, can")
	}
	return &Store{objects: ranged, indexes: newIdxCache(IdxCacheBytes)}, nil
}

// Put writes a pack repo.Unpack wrote, size bytes read from pack, and the index of it, into the
// repository of workflow, and answers it as git_packs records it.
//
// It records the pack receiving first, in a transaction of its own, then writes the pack, then its
// index, so that no file under git/ is ever without a row naming it. The pack is checked as it is
// written: bytes that are not size long, or whose trailer is not the checksum Unpack answered, are
// refused before the key holds them, since the key names that checksum. The caller makes the pack
// live, db.NS.PackLive, in the transaction that moves the refs onto what it holds.
func (s *Store) Put(ctx context.Context, pool *db.Pool, namespace, workflow string, pack io.Reader, size int64, u *repo.Unpacked) (db.Pack, error) {
	if u == nil || size <= 0 {
		return db.Pack{}, fmt.Errorf("store: a pack of %s/%s of %d bytes and no objects written", namespace, workflow, size)
	}
	p := db.Pack{Name: hex.EncodeToString(u.Checksum[:]), Size: size, Objects: len(u.Objects)}
	var idx bytes.Buffer
	if err := repo.WriteIdx(&idx, u.Objects, u.Checksum); err != nil {
		return db.Pack{}, fmt.Errorf("store: the index of pack %s of %s/%s: %w", p.Name, namespace, workflow, err)
	}
	var key, storage string
	err := pool.In(ctx, namespace, func(ctx context.Context, n *db.NS) error {
		var err error
		if key, err = n.ReceivePack(ctx, workflow, p); err != nil {
			return err
		}
		// Kept under the namespace's storage name, which a rename leaves as it was.
		storage, err = n.Storage(ctx)
		return err
	})
	if err != nil {
		return db.Pack{}, err
	}
	if err := checkKeys(storage, key, p.Name); err != nil {
		return db.Pack{}, err
	}
	checked := &checkedPack{r: pack, size: size, sum: u.Checksum, h: sha1.New()}
	if err := s.objects.Put(ctx, PackKey(storage, key, p.Name), checked); err != nil {
		return db.Pack{}, fmt.Errorf("store: pack %s of %s/%s could not be written: %w", p.Name, namespace, workflow, err)
	}
	if err := s.objects.Put(ctx, IdxKey(storage, key, p.Name), &idx); err != nil {
		return db.Pack{}, fmt.Errorf("store: the index of pack %s of %s/%s could not be written: %w", p.Name, namespace, workflow, err)
	}
	return p, nil
}

// checkedPack reads a pack through, and at its end refuses one that is not size bytes, or whose
// trailer is not sum or not the SHA-1 of what comes before it. The last twenty bytes read are held
// back from the hash until the next read shows they are not the trailer.
type checkedPack struct {
	r    io.Reader
	size int64
	sum  [sha1.Size]byte
	h    hash.Hash
	read int64
	tail []byte
}

func (c *checkedPack) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.read += int64(n)
	if c.read > c.size {
		return n, fmt.Errorf("store: the pack runs past the %d bytes it was said to be", c.size)
	}
	c.tail = append(c.tail, b[:n]...)
	if over := len(c.tail) - sha1.Size; over > 0 {
		c.h.Write(c.tail[:over])
		c.tail = append(c.tail[:0], c.tail[over:]...)
	}
	if errors.Is(err, io.EOF) {
		switch {
		case c.read != c.size:
			return n, fmt.Errorf("store: the pack is %d bytes, and was said to be %d", c.read, c.size)
		case !bytes.Equal(c.tail, c.sum[:]):
			return n, errors.New("store: the pack's trailer is not the checksum its objects were written with")
		case !bytes.Equal(c.h.Sum(nil), c.sum[:]):
			return n, errors.New("store: the pack's trailer is not the SHA-1 of what comes before it")
		}
	}
	return n, err
}

// Open answers the objects of a repository's live packs, as the transaction that read r listed
// them. It is closed once the request reading it is done, which lets go of the packs it opened.
func (s *Store) Open(r db.Repository) (*Objects, error) {
	o := &Objects{store: s, namespace: storageOf(r.Storage, r.Namespace), repository: r.Key}
	for _, p := range r.Packs {
		if err := checkKeys(o.namespace, r.Key, p.Name); err != nil {
			return nil, err
		}
		o.packs = append(o.packs, &packed{Pack: p})
	}
	return o, nil
}

// Objects are the objects of one repository's live packs, found by their IDs.
//
// It is a repo.Lookup, for a thin pack's deltas to be resolved against and a pushed tree to be read
// through, and it answers the stored pack holding an object, for a fetch to copy its entry as it is.
// It may be used from several goroutines at once, and is closed once none of them reads through it,
// or through a pack it answered, any more.
type Objects struct {
	store                 *Store
	namespace, repository string

	mu    sync.Mutex
	packs []*packed
}

// packed is one live pack of the lookup, its index read once an object is looked for, and the pack
// itself opened once one is found in it.
type packed struct {
	db.Pack
	idx  *repo.Idx
	r    artifact.RangeReader
	pack *repo.Pack
}

var _ repo.Lookup = (*Objects)(nil)

// Find answers the pack holding the object named id, opened, and an error wrapping repo.ErrMissing
// where no live pack holds it. The pack is the lookup's, and is let go of when it is closed.
func (o *Objects) Find(ctx context.Context, id repo.ID) (*repo.Pack, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	p, err := o.holding(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.pack == nil {
		if err := o.open(ctx, p); err != nil {
			return nil, err
		}
	}
	return p.pack, nil
}

// Has is whether a live pack holds the object named id, which reads their indexes and no pack.
func (o *Objects) Has(ctx context.Context, id repo.ID) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, err := o.holding(ctx, id)
	if errors.Is(err, repo.ErrMissing) {
		return false, nil
	}
	return err == nil, err
}

// OpenObject answers the object named id, read from the pack holding it.
func (o *Objects) OpenObject(ctx context.Context, id repo.ID) (repo.ObjectReader, error) {
	p, err := o.Find(ctx, id)
	if err != nil {
		return nil, err
	}
	return p.OpenObject(ctx, id)
}

// Close lets go of the packs the lookup opened.
func (o *Objects) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	var errs []error
	for _, p := range o.packs {
		if p.r != nil {
			errs = append(errs, p.r.Close())
			p.r, p.pack = nil, nil
		}
	}
	return errors.Join(errs...)
}

// holding answers the pack whose index names id, reading the indexes it has not read yet, newest
// pack first.
func (o *Objects) holding(ctx context.Context, id repo.ID) (*packed, error) {
	for _, p := range o.packs {
		if p.idx == nil {
			idx, err := o.store.index(ctx, o.namespace, o.repository, p.Name)
			if err != nil {
				return nil, err
			}
			p.idx = idx
		}
		if _, ok := p.idx.Find(id); ok {
			return p, nil
		}
	}
	return nil, fmt.Errorf("%w: %s in %s/git/%s", repo.ErrMissing, id, o.namespace, o.repository)
}

// open opens a pack through its index, and refuses one whose size is not what git_packs records.
func (o *Objects) open(ctx context.Context, p *packed) error {
	key := PackKey(o.namespace, o.repository, p.Name)
	r, err := o.store.objects.OpenRange(ctx, key)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("store: pack %s is live and not in the store: %w", key, err)
	}
	if err != nil {
		return fmt.Errorf("store: pack %s: %w", key, err)
	}
	if r.Size() != p.Size {
		r.Close()
		return fmt.Errorf("store: pack %s is %d bytes, and was recorded as %d", key, r.Size(), p.Size)
	}
	pack, err := repo.OpenPack(r, r.Size(), p.idx)
	if err != nil {
		r.Close()
		return fmt.Errorf("store: pack %s: %w", key, err)
	}
	p.r, p.pack = r, pack
	return nil
}

// index answers the index of one pack, read from the store the first time it is asked for, and
// refuses one that is not the index of the pack it is named after.
func (s *Store) index(ctx context.Context, namespace, repository, name string) (*repo.Idx, error) {
	key := IdxKey(namespace, repository, name)
	if idx, ok := s.indexes.get(key); ok {
		return idx, nil
	}
	r, err := s.objects.OpenRange(ctx, key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("store: the index %s is of a live pack and not in the store: %w", key, err)
	}
	if err != nil {
		return nil, fmt.Errorf("store: the index %s: %w", key, err)
	}
	defer r.Close()
	if r.Size() > maxIdxBytes {
		return nil, fmt.Errorf("store: the index %s is %d bytes, past the %d an index is read to", key, r.Size(), maxIdxBytes)
	}
	data := make([]byte, r.Size())
	if n, err := r.ReadAt(data, 0); n < len(data) {
		return nil, fmt.Errorf("store: the index %s: %w", key, err)
	}
	idx, err := repo.ParseIdx(data)
	if err != nil {
		return nil, fmt.Errorf("store: the index %s: %w", key, err)
	}
	if sum := idx.Checksum(); hex.EncodeToString(sum[:]) != name {
		return nil, fmt.Errorf("store: the index %s is the index of pack %x", key, sum)
	}
	s.indexes.add(key, idx, len(data))
	return idx, nil
}

// packName is a pack's name, its checksum, and repositoryKey a workflow's key in the store.
var (
	packName      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	repositoryKey = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// checkKeys refuses what would not make a key of one repository: a namespace that is no single
// segment, a repository that is not a workflow's key, or a name that is not a checksum.
func checkKeys(namespace, repository, name string) error {
	switch {
	case namespace == "" || namespace == "." || namespace == ".." || strings.ContainsAny(namespace, "/\\\x00"):
		return fmt.Errorf("store: namespace %q is not a key segment", namespace)
	case !repositoryKey.MatchString(repository):
		return fmt.Errorf("store: repository %q is not a workflow's key, 32 hexadecimal digits", repository)
	case !packName.MatchString(name):
		return fmt.Errorf("store: pack %q is not named by its checksum, 40 hexadecimal digits", name)
	}
	return nil
}

// storageOf is the first segment of a repository's keys: its namespace's storage name, the name the
// namespace was created with, which a rename leaves as it was; and the namespace's name where the
// record read names no storage name, as one built by hand for a test does, the two being one for a
// namespace never renamed.
func storageOf(storage, namespace string) string {
	if storage != "" {
		return storage
	}
	return namespace
}
