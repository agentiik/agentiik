package store

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/repo"
)

// Repack writes every object of a repository's live packs into one pack, and makes it live in their
// place, which then sit superseded for the grace before the collection deletes them: "Repack and
// garbage-collect a repository's packfiles without interrupting a clone in progress." It answers the
// pack written and how many it replaced, and nothing where the repository holds fewer than two.
//
// # Every object, reachable or not
//
// The new pack holds every object the live packs hold, each once, whether a ref reaches it or not.
// Leaving out what no ref reaches would need that no push being judged meanwhile relies on it, and
// a push is judged against the objects its repository holds, whichever ref reaches them: an object
// dropped here that a push found held would be gone once the old packs are, leaving a version whose
// objects are not all there. What a repack removes is the duplicates and the count of packs a fetch
// looks an object up in; objects no ref reaches stay until pruning them has rules of its own.
//
// # Written twice rather than kept on a disk
//
// Every entry is copied as it is stored, whole and compressed, so the pack's bytes are known before
// it is written: once through to learn its size and checksum, which the store's Put holds a pack to,
// and once into the store. Reading the packs twice costs what keeping a copy of the largest
// repository on the controller's disk would otherwise, and a repack is rare.
//
// A push landing meanwhile makes a pack of its own live, which the new pack does not replace: only
// the packs that were live when the repack began are superseded, in the transaction that makes the
// new one live, under the lock every push takes to move its refs.
func (s *Store) Repack(ctx context.Context, pool *db.Pool, namespace, workflow string) (db.Pack, int, error) {
	var r db.Repository
	if err := pool.In(ctx, namespace, func(ctx context.Context, n *db.NS) error {
		var err error
		r, err = n.Repository(ctx, workflow)
		return err
	}); err != nil {
		return db.Pack{}, 0, err
	}
	if len(r.Packs) < 2 {
		return db.Pack{}, 0, nil
	}
	objects, err := s.Open(r)
	if err != nil {
		return db.Pack{}, 0, err
	}
	defer objects.Close()

	// Each object once, from the newest pack holding it, in the order the packs list them.
	var ids []repo.ID
	from := map[repo.ID]*packed{}
	for _, p := range objects.packs {
		idx, err := s.index(ctx, namespace, r.Key, p.Name)
		if err != nil {
			return db.Pack{}, 0, err
		}
		p.idx = idx
		for i := range idx.Len() {
			id := idx.ID(i)
			if _, held := from[id]; !held {
				from[id] = p
				ids = append(ids, id)
			}
		}
	}
	write := func(w io.Writer) ([20]byte, []repo.PackedObject, error) {
		pw, err := repo.NewPackWriter(w, len(ids))
		if err != nil {
			return [20]byte{}, nil, err
		}
		for _, id := range ids {
			p := from[id]
			if p.pack == nil {
				if err := objects.open(ctx, p); err != nil {
					return [20]byte{}, nil, err
				}
			}
			if err := pw.Copy(p.pack, id); err != nil {
				return [20]byte{}, nil, err
			}
			if err := ctx.Err(); err != nil {
				return [20]byte{}, nil, err
			}
		}
		sum, err := pw.Close()
		return sum, pw.Objects(), err
	}

	var size counter
	sum, packedObjects, err := write(&size)
	if err != nil {
		return db.Pack{}, 0, fmt.Errorf("store: the repack of %s/%s: %w", namespace, workflow, err)
	}
	reading, writing := io.Pipe()
	go func() {
		_, _, err := write(writing)
		writing.CloseWithError(err)
	}()
	pack, err := s.Put(ctx, pool, namespace, workflow, reading, size.n, &repo.Unpacked{Objects: packedObjects, Checksum: sum})
	reading.CloseWithError(errors.New("store: the repack's pack is no longer read"))
	if err != nil {
		return db.Pack{}, 0, fmt.Errorf("store: the repack of %s/%s: %w", namespace, workflow, err)
	}

	replaced := make([]string, len(r.Packs))
	for i, p := range r.Packs {
		replaced[i] = p.Name
	}
	if err := pool.In(ctx, namespace, func(ctx context.Context, n *db.NS) error {
		return n.Repacked(ctx, workflow, pack.Name, replaced)
	}); err != nil {
		return db.Pack{}, 0, fmt.Errorf("store: the repack of %s/%s: %w", namespace, workflow, err)
	}
	return pack, len(r.Packs), nil
}

// counter counts what is written through it, and keeps none of it.
type counter struct{ n int64 }

func (c *counter) Write(b []byte) (int, error) {
	c.n += int64(len(b))
	return len(b), nil
}

// Collect deletes a pack's files from the store, the index before the pack, since a pack found with
// no index is refused where an index found with no pack is merely missing; and answers nil where they
// are gone already, which a collection that died after deleting them leaves.
func (s *Store) Collect(ctx context.Context, p db.RepositoryPack) error {
	if err := checkKeys(p.Namespace, p.Repository, p.Name); err != nil {
		return err
	}
	remover, ok := s.objects.(interface {
		Remove(context.Context, string) (bool, error)
	})
	if !ok {
		return errors.New("store: that object store deletes nothing, and a pack is collected by deleting its files")
	}
	for _, key := range []string{IdxKey(p.Namespace, p.Repository, p.Name), PackKey(p.Namespace, p.Repository, p.Name)} {
		if _, err := remover.Remove(ctx, key); err != nil {
			return fmt.Errorf("store: %s could not be deleted: %w", key, err)
		}
	}
	// Its index may stay cached: a pack's name is its checksum, so a pack received again under it
	// is the same bytes, with the same index.
	return nil
}
