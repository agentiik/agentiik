package db

import (
	"context"
	"fmt"
)

// Orphans: the files of the store no row names.
//
// The collection finds its work in artifact_objects, so an object whose row was never written is
// one it never sees: the output of an attempt that failed or was lost, whose result no decision
// recorded, an envelope of a decision that was refused, and whatever v0.2 left behind unrecorded.
// Nothing names such a file, and a writer of the same bytes later holds them first or is told to
// write them again, as with any object collected. So the caller walks the store, and hands the
// collection each file that no row of artifact_objects names, no live artifact names, no write
// holds and no envelope of a run still under way names, once its bytes are older than the grace,
// as a row counting nothing and collectable from that moment. From there it is an object like any
// other: claimed a grace later, deleted under a lock on its row and confirmed, a writer referencing
// it again in between being told to write the bytes again.
//
// The row is written, and committed, a grace before anything is deleted, which is what makes the
// collection's protocol apply to it. A write records itself against the object's row, holding it,
// and a write that began before the row was there held nothing, and was not seen either if it had
// not committed yet: its bytes could be written in the moment between the deletion's look and the
// deletion. A grace later it has long committed, and holds the object for as long as a write does.
//
// A namespace with a finished run whose files are still to be recorded is left alone: those files
// are named by no row until they are, and would be taken for orphans.

// Sweepable answers the namespaces whose objects an orphan sweep may walk, in name order: every
// namespace but those with a finished run whose artifact files are still to be recorded.
func (p *Pool) Sweepable(ctx context.Context) ([]string, error) {
	var out []string
	err := p.Installation(ctx, Collect, func(ctx context.Context, w *Wide) error {
		rows, err := w.tx.Query(ctx, `
			select n.name from namespaces n
			where not exists (select 1 from runs r
			                  where r.namespace = n.name and r.finished_at is not null and not r.files_recorded)
			order by n.name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = nil
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			out = append(out, name)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("db: the namespaces an orphan sweep may walk could not be read: %w", err)
	}
	return out, nil
}

// orphanOf is what keeps a file of namespace $1 whose digest is g.digest, the bare hexadecimal,
// from being taken for an orphan: a row counting it, a live artifact naming it, a write holding it,
// and a finished run of the namespace whose files are still to be recorded.
const orphanOf = `not exists (select 1 from artifact_objects o
	                  where o.namespace = $1 and o.digest = 'sha256:' || g.digest)
	    and not exists (select 1 from artifacts a
	                    where a.namespace = $1 and a.digest = 'sha256:' || g.digest and a.status = 'live')
	    and not exists (select 1 from artifact_uploads u
	                    where u.namespace = $1 and u.digest = 'sha256:' || g.digest and u.until > now())
	    and not exists (select 1 from runs r
	                    where r.namespace = $1 and r.finished_at is not null and not r.files_recorded)`

// Unnamed answers which of digests, bare hexadecimal digests of objects of namespace, nothing in
// the database holds: no row counts it, no live artifact names it and no write holds it, in a
// namespace with no finished run whose files are still to be recorded.
//
// It is the look before the envelopes are read, which only a few files ever need, and decides
// nothing: Orphaned asks all of it again as it writes.
func (p *Pool) Unnamed(ctx context.Context, namespace string, digests []string) ([]string, error) {
	if len(digests) == 0 {
		return nil, nil
	}
	for _, d := range digests {
		if !hexDigest.MatchString(d) {
			return nil, fmt.Errorf("db: %q is not a digest", d)
		}
	}
	var out []string
	err := p.Installation(ctx, Collect, func(ctx context.Context, w *Wide) error {
		rows, err := w.tx.Query(ctx, `
			select g.digest from unnest($2::text[]) as g(digest)
			where `+orphanOf, namespace, digests)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = nil
		for rows.Next() {
			var d string
			if err := rows.Scan(&d); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("db: what holds the objects of namespace %s could not be read: %w", namespace, err)
	}
	return out, nil
}

// LiveEnvelopes answers the envelopes the runs of namespace that have not finished reference,
// published and per shard, the bare hexadecimal of each once: the files they name are the ones
// those runs are working with, which a shard's envelope names before any artifact does, and which
// a run v0.2 began names until a decision of this release records them.
func (p *Pool) LiveEnvelopes(ctx context.Context, namespace string) ([]string, error) {
	var out []string
	err := p.Installation(ctx, Collect, func(ctx context.Context, w *Wide) error {
		rows, err := w.tx.Query(ctx, `
			select distinct e->>'digest'
			from runs r, lateral jsonb_array_elements(coalesce(r.evaluation->'envelopes', '[]'::jsonb)) as e
			where r.namespace = $1 and r.finished_at is null and e ? 'digest'`, namespace)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = nil
		for rows.Next() {
			var d string
			if err := rows.Scan(&d); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("db: the envelopes of the runs of namespace %s under way could not be read: %w", namespace, err)
	}
	return out, nil
}

// Orphan is a file of the store that no row names.
type Orphan struct {
	// Digest is the bare hexadecimal its key ends with.
	Digest string

	Size int64
}

// Orphaned hands orphans of namespace to the collection, each as a row of artifact_objects that
// counts nothing and is collectable from now, and answers how many it handed over.
//
// Each is asked again what Unnamed asked, in the statement that writes its row, and one that
// something holds now is passed by: a write that began since, a reference recorded since, a run of
// the namespace found to be waiting for its files to be recorded.
func (p *Pool) Orphaned(ctx context.Context, namespace string, orphans []Orphan) (int, error) {
	if len(orphans) == 0 {
		return 0, nil
	}
	digests := make([]string, len(orphans))
	sizes := make([]int64, len(orphans))
	for i, o := range orphans {
		if !hexDigest.MatchString(o.Digest) {
			return 0, fmt.Errorf("db: %q is not a digest", o.Digest)
		}
		if o.Size < 0 {
			return 0, fmt.Errorf("db: an object of %d bytes", o.Size)
		}
		digests[i], sizes[i] = o.Digest, o.Size
	}
	var handed int
	err := p.Installation(ctx, Collect, func(ctx context.Context, w *Wide) error {
		tag, err := w.tx.Exec(ctx, `
			insert into artifact_objects (namespace, digest, size_bytes, media_type, refs, collectable_at)
			select $1, 'sha256:' || g.digest, g.size, 'application/octet-stream', 0, now()
			from unnest($2::text[], $3::bigint[]) as g(digest, size)
			where `+orphanOf+`
			order by g.digest
			on conflict (namespace, digest) do nothing`, namespace, digests, sizes)
		if err != nil {
			return err
		}
		handed = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("db: the orphans of namespace %s could not be handed to the collection: %w", namespace, err)
	}
	return handed, nil
}
