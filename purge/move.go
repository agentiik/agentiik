package purge

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/repo/store"
)

// Moving workflows between namespaces, which the controller that leads carries out after the answer
// that asked for it: "a move answers 202 with Location", the workflow frozen until it is done.
//
// Every object the workflow's rows name is kept under a key its namespace begins, and a namespace's
// keys are never read for another: so the move copies each of them under the target's keys first,
// what a version, an envelope and an artifact name, the chunks of its logs and its packs, and only
// then changes the rows (db.Pool.CompleteMove), which from then on name the copies. A copy already
// under the target's key is not made again, which also makes a move that died half copied cheap to
// take up, or one carried out the pass after a chunk a task still stopping shipped was indexed. What
// was copied stays under the source's keys for the grace, for a read or a fetch that began before
// the rows changed: the objects counted as the source lets them go, the logs and packs kept in
// moved_objects until movedGone deletes them, unless a move back names them again first.

// moves carries out the moves asked, each once its runs have finished and its packs are settled.
func (p *Purger) moves(ctx context.Context, out *Purged) (bool, error) {
	asked, err := p.Pool.Moves(ctx, p.batch())
	if err != nil {
		return false, err
	}
	var failed []error
	for _, m := range asked {
		moved, err := p.move(ctx, m)
		if err != nil {
			failed = append(failed, err)
			continue
		}
		if moved {
			out.Moved++
		}
	}
	return false, errors.Join(failed...)
}

// move carries out one move, and answers whether it was carried out.
func (p *Purger) move(ctx context.Context, m db.Move) (bool, error) {
	objects, err := p.Pool.ObjectsOf(ctx, m)
	if err != nil || !objects.Ready {
		return false, err
	}
	// Every key is under the namespace's storage name, the name it was created with, which a rename
	// leaves as it was.
	for _, digest := range objects.Digests {
		if err := p.copied(ctx, artifact.Key(m.Storage, digest), artifact.Key(m.TargetStorage, digest), false); err != nil {
			return false, err
		}
	}
	for _, key := range objects.Logs {
		to, ok := rekeyed(key, m.Storage, m.TargetStorage)
		if !ok {
			continue
		}
		if err := p.copied(ctx, key, to, false); err != nil {
			return false, err
		}
	}
	for _, name := range objects.Packs {
		for _, key := range [][2]string{
			{store.PackKey(m.Storage, m.Repository, name), store.PackKey(m.TargetStorage, m.Repository, name)},
			{store.IdxKey(m.Storage, m.Repository, name), store.IdxKey(m.TargetStorage, m.Repository, name)},
		} {
			if err := p.copied(ctx, key[0], key[1], false); err != nil {
				return false, err
			}
		}
	}

	moved, err := p.Pool.CompleteMove(ctx, m, objects, p.grace())
	if err != nil || !moved.Done {
		return false, err
	}
	// And again for an object the target's collection had claimed, or held no row of, when the
	// move counted it there: the count keeps any collection away from it now, and the bytes
	// under the target's key may be what the collection deleted, or never there.
	for _, digest := range moved.MustWrite {
		if err := p.copied(ctx, artifact.Key(m.Storage, digest), artifact.Key(m.TargetStorage, digest), true); err != nil {
			return true, fmt.Errorf("purge: %s/%s moved to %s, and %s could not be written again under %s: %w", m.Namespace, m.Workflow, m.Target, digest, m.Target, err)
		}
	}
	return true, nil
}

// copied copies the object at from to to, unless to holds it already and again is false. An object
// from does not hold is not copied: what named it names nothing in the target either, as it named
// nothing in the source.
func (p *Purger) copied(ctx context.Context, from, to string, again bool) error {
	if !again {
		held, err := p.Objects.Has(ctx, to)
		if err != nil {
			return fmt.Errorf("purge: whether %s is held could not be read: %w", to, err)
		}
		if held {
			return nil
		}
	}
	r, err := p.Objects.Open(ctx, from)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("purge: %s could not be read: %w", from, err)
	}
	defer r.Close()
	if err := p.Objects.Put(ctx, to, r); err != nil {
		return fmt.Errorf("purge: %s could not be copied to %s: %w", from, to, err)
	}
	return nil
}

// rekeyed is a key of the source's namespace under the target's, the rest of it kept.
func rekeyed(key, from, to string) (string, bool) {
	rest, ok := strings.CutPrefix(key, from+"/")
	if !ok {
		return "", false
	}
	return to + "/" + rest, true
}

// movedGone deletes what moves left under the namespaces they left once the grace has passed.
func (p *Purger) movedGone(ctx context.Context, out *Purged) (bool, error) {
	due, err := p.Pool.MovedObjectsDue(ctx, p.batch())
	if err != nil || len(due) == 0 {
		return false, err
	}
	var gone []db.MovedObject
	var failed []error
	for _, o := range due {
		if _, err := p.Objects.Remove(ctx, o.Key); err != nil {
			failed = append(failed, fmt.Errorf("purge: %s, which a move left, could not be deleted: %w", o.Key, err))
			continue
		}
		gone = append(gone, o)
	}
	if err := p.Pool.MovedObjectsGone(ctx, gone); err != nil {
		failed = append(failed, err)
	}
	out.MovedLeft += len(gone)
	return len(due) == p.batch(), errors.Join(failed...)
}

// grace is how long what a move leaves is kept, db.DefaultGrace where Grace is zero.
func (p *Purger) grace() time.Duration {
	if p.Grace > 0 {
		return p.Grace
	}
	return db.DefaultGrace
}
