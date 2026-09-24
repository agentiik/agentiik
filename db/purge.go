package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
)

// The sweeps.
//
// "Retention is declared per workflow within the namespace ceiling, with separate purges for
// envelopes, artifacts and logs, and garbage collection of artifacts whose reference count
// reaches zero." Four things, and they are separate because they act on different rows
// against different clocks: an artifact carries its own instant, an envelope and a log live
// by the run's, and collection is not a retention at all but what falls out of the other
// three.
//
// None of them has a namespace, which is why each goes through the Installation door with
// its reason named. A sweep bound to one namespace would sweep one tenant and leave the rest.
//
// # Nothing here deletes an object
//
// Three of the four only lower counts and stamp rows. The fourth, collection, is the one
// thing that deletes bytes, and even it does not do the deleting: package db does not reach
// the object store, and it answers with the keys for the caller to delete. That is not
// squeamishness about layering, it is the only order that neither leaks nor dangles.
//
// The protocol is claim, delete, confirm. Collectable claims objects whose count reached
// zero, marking them; the caller deletes those keys from the store; Collected removes the
// rows. A caller that dies in the middle leaves rows marked and bytes gone, and the next
// sweep claims them again and deletes what is already deleted, which costs a request. The
// reverse order, removing the row first, would leak an object nothing can ever name again.
//
// # The grace period
//
// An object becomes collectable the moment its count reaches zero, and is collected no
// sooner than grace afterwards. The window is not caution for its own sake: content
// addressing means a run that writes the same bytes tomorrow will reference the object that
// is there rather than write a new one, and collecting the instant the last reference went
// would turn every such write into an upload. A day is the usual answer, and an installation
// that wants its store smaller sooner sets it lower.
//
// Inside the window a writer simply clears the mark. Crossing it, the writer is told to write
// the bytes again through Written.MustWriteBytes, which is what closes the one gap the grace
// alone cannot: a sweep that has claimed an object and not yet deleted it, while a reference
// arrives for it.

// defaultBatch is how many rows one call of a sweep takes.
//
// A sweep with no bound is a transaction that can run for an hour and hold a lock the whole
// time. The controller calls these on its interval and calls them again while they come back
// full, which spreads the work across transactions instead of gathering it into one.
const defaultBatch = 1000

// DefaultGrace is how long an object sits at a count of zero before it may be collected.
const DefaultGrace = 24 * time.Hour

// Object is one object a sweep has claimed, for the caller to delete from the store.
type Object struct {
	Namespace string

	// Digest is the bare hexadecimal, and Key is what the store is asked for.
	Digest string
	Key    string

	Size int64
}

// Log is one task's log, as the database holds it: a URI and never the lines.
type Log struct {
	Namespace string
	Task      string
	URI       string

	// Keys are the objects its lines were written to, one a chunk, for the caller to delete
	// before it confirms: the index to them is the one thing that can name them.
	Keys []string
}

// ExpireArtifacts retires every reference whose duration has run out.
//
// It retires the reference and lowers the count behind it, and it deletes nothing:
// "Expiry applies to the reference, never to the object." A run that loses an artifact is
// marked replayable from the start only in the same transaction, because the two facts are
// one fact and a reader that saw one without the other would offer a replay that cannot run.
func (p *Pool) ExpireArtifacts(ctx context.Context, batch int) (int, error) {
	batch, err := batchOf(batch)
	if err != nil {
		return 0, err
	}
	var retired int
	err = p.Installation(ctx, Purge, func(ctx context.Context, w *Wide) error {
		return w.tx.QueryRow(ctx, `
			with doomed as (
			  update artifacts set status = 'expired', retired_at = now(), fetches_left = null
			  where (namespace, run_id, step, port, name) in (
			    select namespace, run_id, step, port, name from artifacts
			    where status = 'live' and expires_at <= now()
			    order by expires_at
			    limit $1
			    for update skip locked
			  )
			  returning namespace, run_id, digest
			), marked as (
			  update runs r set replay_from_start_only = true
			  from (select distinct namespace, run_id from doomed) d
			  where r.namespace = d.namespace and r.id = d.run_id and not r.replay_from_start_only
			  returning 1
			), counted as (
			  select namespace, digest, count(*)::int as n from doomed group by 1, 2
			), lowered as (
			  update artifact_objects o
			  set refs = greatest(o.refs - c.n, 0),
			      collectable_at = case when o.refs - c.n <= 0 then now() else null end
			  from counted c
			  where o.namespace = c.namespace and o.digest = c.digest
			  returning 1
			)
			select (select count(*) from doomed)::int`, batch).Scan(&retired)
	})
	if err != nil {
		return 0, fmt.Errorf("db: the artifact purge failed: %w", err)
	}
	return retired, nil
}

// PurgeEnvelopes drops the envelopes of runs whose retention has run out.
//
// A run's envelopes are what its decision document references, published and per shard alike,
// because both are objects the controller put in the store and an object nothing counts is an
// object the collector never sees. What goes is the count on them, which is what eventually
// lets the bytes be collected. What stays is the record: the digests in steps.ports are the
// record of what was published, and the chapter keeps only digests and URIs in the database
// anyway. The stamp is what keeps a sweep from taking the same run for ever.
func (p *Pool) PurgeEnvelopes(ctx context.Context, batch int) (int, error) {
	batch, err := batchOf(batch)
	if err != nil {
		return 0, err
	}
	var purged int
	err = p.Installation(ctx, Purge, func(ctx context.Context, w *Wide) error {
		rows, err := w.tx.Query(ctx, `
			select r.namespace, r.id
			from runs r
			where r.expires_at is not null and r.expires_at <= now()
			  and exists (select 1 from steps s
			              where s.namespace = r.namespace and s.run_id = r.id
			                and s.envelopes_purged_at is null)
			order by r.expires_at
			limit $1`, batch)
		if err != nil {
			return err
		}
		type due struct {
			namespace string
			run       agk.RunID
		}
		var expired []due
		for rows.Next() {
			var d due
			if err := rows.Scan(&d.namespace, &d.run); err != nil {
				rows.Close()
				return err
			}
			expired = append(expired, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, d := range expired {
			held, err := w.envelopesOf(ctx, d.namespace, d.run)
			if err != nil {
				return err
			}
			for _, e := range held {
				if err := lower(ctx, w.tx, d.namespace, "sha256:"+e.Digest, ""); err != nil {
					return err
				}
			}
			if _, err := w.tx.Exec(ctx,
				`update steps set envelopes_purged_at = now()
				 where namespace = $1 and run_id = $2 and envelopes_purged_at is null`,
				d.namespace, string(d.run)); err != nil {
				return err
			}
			purged++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("db: the envelope purge failed: %w", err)
	}
	return purged, nil
}

// ExpiredLogs claims the logs of runs whose retention has run out.
//
// A log is a URI and not a digest: one task wrote it, nothing else names it, and there is
// nothing to count. So it is claim, delete, confirm, with LogsPurged as the confirmation, and
// the line count and the truncation flag stay behind as the record that there was a log.
func (p *Pool) ExpiredLogs(ctx context.Context, batch int) ([]Log, error) {
	batch, err := batchOf(batch)
	if err != nil {
		return nil, err
	}
	var out []Log
	err = p.Installation(ctx, Purge, func(ctx context.Context, w *Wide) error {
		rows, err := w.tx.Query(ctx, `
			select t.namespace, t.id, t.log_uri,
			       coalesce((select array_agg(c.object_key order by c.seq) from task_log_chunks c
			                 where c.namespace = t.namespace and c.task_id = t.id
			                   and c.object_key is not null), '{}')
			from tasks t join runs r on r.namespace = t.namespace and r.id = t.run_id
			where t.log_uri is not null
			  and r.expires_at is not null and r.expires_at <= now()
			order by r.expires_at
			limit $1`, batch)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l Log
			if err := rows.Scan(&l.Namespace, &l.Task, &l.URI, &l.Keys); err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("db: the logs due for purging could not be read: %w", err)
	}
	return out, nil
}

// LogsPurged records that those logs are gone from the store.
//
// The index to their chunks goes with them, since it names objects that are no longer there, and
// where each log stood stays: how many lines it held and whether the cap cut it short.
func (p *Pool) LogsPurged(ctx context.Context, logs []Log) (int, error) {
	if len(logs) == 0 {
		return 0, nil
	}
	namespaces := make([]string, len(logs))
	tasks := make([]string, len(logs))
	for i, l := range logs {
		namespaces[i], tasks[i] = l.Namespace, l.Task
	}
	var cleared int
	err := p.Installation(ctx, Purge, func(ctx context.Context, w *Wide) error {
		tag, err := w.tx.Exec(ctx, `
			update tasks t set log_uri = null
			from unnest($1::text[], $2::text[]) as g(namespace, id)
			where t.namespace = g.namespace and t.id = g.id`, namespaces, tasks)
		if err != nil {
			return err
		}
		cleared = int(tag.RowsAffected())
		_, err = w.tx.Exec(ctx, `
			delete from task_log_chunks c
			using unnest($1::text[], $2::text[]) as g(namespace, id)
			where c.namespace = g.namespace and c.task_id = g.id`, namespaces, tasks)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("db: the purged logs could not be recorded: %w", err)
	}
	return cleared, nil
}

// Collectable claims objects nothing references any more.
//
// Claimed and not deleted: the caller deletes the keys from the store and then calls
// Collected. An object referenced again between the claim and the deletion is not lost,
// because the writer that referenced it was told to write the bytes again.
//
// grace is how long an object must have sat at a count of zero. Zero means DefaultGrace; a
// negative one is refused, since collecting an object before it was ever collectable is not a
// setting anybody wants and is far too easy to write by accident.
func (p *Pool) Collectable(ctx context.Context, grace time.Duration, batch int) ([]Object, error) {
	if grace < 0 {
		return nil, fmt.Errorf("db: a collection grace of %s would collect an object before its count reached zero", grace)
	}
	if grace == 0 {
		grace = DefaultGrace
	}
	batch, err := batchOf(batch)
	if err != nil {
		return nil, err
	}
	var out []Object
	err = p.Installation(ctx, Collect, func(ctx context.Context, w *Wide) error {
		rows, err := w.tx.Query(ctx, `
			update artifact_objects set collecting_at = now()
			where (namespace, digest) in (
			  select namespace, digest from artifact_objects
			  where refs = 0
			    and collectable_at is not null
			    and collectable_at <= now() - ($1::bigint * interval '1 second')
			  order by collectable_at
			  limit $2
			  for update skip locked
			)
			returning namespace, digest, size_bytes`, int64(grace/time.Second), batch)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var o Object
			var stored string
			if err := rows.Scan(&o.Namespace, &stored, &o.Size); err != nil {
				return err
			}
			o.Digest = trimAlgorithm(stored)
			o.Key = artifact.Key(o.Namespace, o.Digest)
			out = append(out, o)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("db: the collectable objects could not be claimed: %w", err)
	}
	return out, nil
}

// Collected removes the rows of objects whose bytes are gone.
//
// An object referenced again since the claim is left alone, which is why the count is checked
// here and not only when it was claimed: the row is the only thing that knows the object was
// wanted after all, and deleting it would leave bytes nothing can name. It answers how many
// rows it removed, so a caller can see when the two numbers differ.
func (p *Pool) Collected(ctx context.Context, objects []Object) (int, error) {
	if len(objects) == 0 {
		return 0, nil
	}
	namespaces := make([]string, len(objects))
	digests := make([]string, len(objects))
	for i, o := range objects {
		if !hexDigest.MatchString(o.Digest) {
			return 0, fmt.Errorf("db: %q is not a digest", o.Digest)
		}
		namespaces[i], digests[i] = o.Namespace, "sha256:"+o.Digest
	}
	var removed int
	err := p.Installation(ctx, Collect, func(ctx context.Context, w *Wide) error {
		tag, err := w.tx.Exec(ctx, `
			delete from artifact_objects o
			using unnest($1::text[], $2::text[]) as g(namespace, digest)
			where o.namespace = g.namespace and o.digest = g.digest
			  and o.refs = 0 and o.collecting_at is not null`, namespaces, digests)
		if err != nil {
			return err
		}
		removed = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("db: the collected objects could not be recorded: %w", err)
	}
	return removed, nil
}

// batchOf bounds one call of a sweep.
func batchOf(batch int) (int, error) {
	if batch < 0 {
		return 0, errors.New("db: a sweep of a negative number of rows")
	}
	if batch == 0 {
		return defaultBatch, nil
	}
	return batch, nil
}
