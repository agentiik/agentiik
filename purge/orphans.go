package purge

import (
	"context"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
)

// walking is where the orphan sweep stands in the store: the namespaces left to walk in this
// round, the one being walked first, and the walk under way through it.
//
// A round walks every namespace once, a batch of entries a call, and the walk is kept open from
// one pass to the next, so that a store of millions of objects is walked once a round rather than
// read from its start at every pass. A round begins at the first pass after the last one ended,
// and a controller that takes over the lead begins its own.
type walking struct {
	namespaces []string
	walk       artifact.Walk
}

// next leaves the namespace being walked for the one after it.
func (w *walking) next() {
	w.close()
	if len(w.namespaces) > 0 {
		w.namespaces = w.namespaces[1:]
	}
}

func (w *walking) close() {
	if w.walk != nil {
		w.walk.Close()
		w.walk = nil
	}
}

// stop ends the round, letting go of the walk under way.
func (w *walking) stop() {
	w.close()
	w.namespaces = nil
}

// orphans walks a batch of the store's entries, and hands the collection those that are orphans:
// files no row names, no write holds and no envelope of a run still under way names, whose bytes
// are older than the grace. It answers full while the round has more to walk.
//
// Only a store that can be walked has orphans found in it, which the built-in one can. The logs
// are never walked, and nothing is deleted here: the collection deletes what it was handed a grace
// later, under the same locks and fences as any other object, a write that began before the
// orphan was handed over having committed and holding it by then, as package db's orphans.go sets
// out.
func (p *Purger) orphans(ctx context.Context, out *Purged) (bool, error) {
	store, ok := p.Objects.(artifact.Walkable)
	if !ok {
		return false, nil
	}
	w := &p.walking
	if len(w.namespaces) == 0 {
		names, err := p.Pool.Sweepable(ctx)
		if err != nil || len(names) == 0 {
			return false, err
		}
		w.namespaces = names
	}
	namespace := w.namespaces[0]
	if w.walk == nil {
		// The namespace's objects are kept under its storage name, the name it was created with,
		// which a rename leaves as it was.
		storage, err := p.Pool.Storage(ctx, namespace)
		if err != nil {
			w.next()
			return false, fmt.Errorf("the objects of namespace %s could not be found, and are walked again next round: %w", namespace, err)
		}
		walk, err := store.Walk(storage)
		if err != nil {
			w.next()
			return false, fmt.Errorf("the objects of namespace %s could not be walked, and are walked again next round: %w", namespace, err)
		}
		w.walk = walk
	}
	found, more, err := w.walk.Next(ctx, p.batch())
	if err != nil || !more {
		w.next()
	}
	if err != nil {
		return false, fmt.Errorf("the objects of namespace %s could not be walked, and are walked again next round: %w", namespace, err)
	}
	// Asked under the name the namespace answers to now, since a walk lasts several passes and a
	// rename between two of them leaves the rows naming the objects under its new name.
	if namespace, err = p.Pool.CurrentName(ctx, namespace); err != nil {
		return false, fmt.Errorf("the namespace whose objects were walked could not be found: %w", err)
	}
	n, err := p.adopt(ctx, namespace, found)
	out.Orphans += n
	if err != nil {
		return false, err
	}
	return len(w.namespaces) > 0, nil
}

// adopt hands the collection those of found, objects of namespace, that are orphans, and answers
// how many.
func (p *Purger) adopt(ctx context.Context, namespace string, found []artifact.Stored) (int, error) {
	grace := p.Grace
	switch {
	case grace < 0:
		return 0, fmt.Errorf("a collection grace of %s would take an orphan before it was ever one", grace)
	case grace == 0:
		grace = db.DefaultGrace
	}
	// Written before the grace, by this host's clock. A file written since may be one whose row its
	// writer is about to write, having put the bytes first, and one dated in the future by a clock
	// set wrong waits until that is past.
	before := time.Now().Add(-grace)
	old := map[string]artifact.Stored{}
	var digests []string
	for _, s := range found {
		if s.Written.After(before) {
			continue
		}
		old[s.Digest] = s
		digests = append(digests, s.Digest)
	}
	if len(digests) == 0 {
		return 0, nil
	}
	unnamed, err := p.Pool.Unnamed(ctx, namespace, digests)
	if err != nil || len(unnamed) == 0 {
		return 0, err
	}
	named, err := p.namedByLiveRuns(ctx, namespace)
	if err != nil {
		return 0, err
	}
	var orphans []db.Orphan
	for _, d := range unnamed {
		if named[d] {
			continue
		}
		orphans = append(orphans, db.Orphan{Digest: d, Size: old[d].Size})
	}
	return p.Pool.Orphaned(ctx, namespace, orphans)
}

// namedByLiveRuns answers the digests of the files the envelopes of namespace's runs still under
// way name, and of those envelopes. A shard's envelope names files that no artifact names until
// its step publishes, a day or more after they were written where a fan-out runs long, and whose
// write held them only that long.
//
// An envelope it cannot read keeps it from answering at all, since the files that envelope names
// could be any: no orphan of the namespace is taken until it reads, which a run's own envelopes,
// counted for as long as the run is kept, always should.
func (p *Purger) namedByLiveRuns(ctx context.Context, namespace string) (map[string]bool, error) {
	envelopes, err := p.Pool.LiveEnvelopes(ctx, namespace)
	if err != nil {
		return nil, err
	}
	storage, err := p.Pool.Storage(ctx, namespace)
	if err != nil {
		return nil, err
	}
	named := map[string]bool{}
	for _, digest := range envelopes {
		named[digest] = true
		e, err := artifact.GetEnvelope(ctx, p.Objects, storage, digest, agk.DefaultLimits())
		if err != nil {
			return nil, fmt.Errorf("an envelope of a run of namespace %s still under way could not be read, and no orphan of the namespace is taken until it is: %w", namespace, err)
		}
		for _, item := range e.Items {
			for _, f := range item.Files {
				named[f.SHA256] = true
			}
		}
	}
	return named, nil
}
