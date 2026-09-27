package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/jackc/pgx/v5"
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
// zero, marking them; Collecting hands each to the caller to delete from the store, under a
// lock on its row; Collected removes the rows. A caller that dies in the middle leaves rows
// marked and bytes gone, and the next sweep claims them again and deletes what is already
// deleted, which costs a request. The reverse order, removing the row first, would leak an
// object nothing can ever name again.
//
// # An object being written
//
// A runner writes an object before the controller hears of the result that references it, so
// for that while nothing counts it, and an object whose count reached zero a day before is one
// the collector would take from under the write. Nothing can write those bytes again: they were
// the runner's, and its task is over. So the store records every write in artifact_uploads
// before it reads a byte, Uploading, holding the object's row as it does, and neither the claim
// nor the deletion takes an object with a write under way. The deletion holds the rows it
// deletes the bytes of and asks about writes only once it holds them: a write that records
// itself first is seen, and one that comes after waits for the deletion and writes its bytes
// once they are gone. Nor is an object taken that a live artifact names, whatever its count
// says: a count that went wrong costs an object kept, never one deleted.
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

	URI string

	// Keys are objects its lines were written to, for the caller to delete before it
	// confirms. A log written in many chunks may be handed over across several claims.
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

// PurgeEnvelopes drops the envelopes of runs whose retention has run out, batch runs at a time,
// and answers how many runs it dropped them of.
//
// A run's envelopes are what its decision document references, published and per shard alike,
// and what each of its tasks was handed on its input ports, once per grant, because all of them
// are objects the controller put in the store and an object nothing counts is an object the
// collector never sees. What goes is the count on them, which is what eventually
// lets the bytes be collected. What stays is the record: the digests in steps.ports are the
// record of what was published, and the chapter keeps only digests and URIs in the database
// anyway.
//
// The run's row is taken before anything is lowered and stamped in the same transaction, which
// is what keeps a sweep from taking the same run for ever and two sweeps at once from lowering
// its counts twice: migration 0037 says why the stamp is on the run. A run whose steps were all
// stamped before it had one of its own is stamped and lowers nothing. The counts of the whole
// batch are lowered in one statement, so that a run fanned out to ten thousand shards costs one
// statement and not twenty thousand, held inside one transaction.
func (p *Pool) PurgeEnvelopes(ctx context.Context, batch int) (int, error) {
	batch, err := batchOf(batch)
	if err != nil {
		return 0, err
	}
	var purged int
	err = p.Installation(ctx, Purge, func(ctx context.Context, w *Wide) error {
		purged = 0
		rows, err := w.tx.Query(ctx, `
			select r.namespace, r.id,
			       exists (select 1 from steps s
			               where s.namespace = r.namespace and s.run_id = r.id
			                 and s.envelopes_purged_at is null)
			from runs r
			where r.expires_at is not null and r.expires_at <= now()
			  and r.envelopes_purged_at is null
			order by r.expires_at
			limit $1
			for update of r skip locked`, batch)
		if err != nil {
			return err
		}
		var namespaces, runs, dueNamespaces, dueRuns []string
		for rows.Next() {
			var namespace, run string
			var due bool
			if err := rows.Scan(&namespace, &run, &due); err != nil {
				rows.Close()
				return err
			}
			namespaces, runs = append(namespaces, namespace), append(runs, run)
			if due {
				dueNamespaces, dueRuns = append(dueNamespaces, namespace), append(dueRuns, run)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(runs) == 0 {
			return err
		}

		type object struct{ namespace, digest string }
		counted := map[object]int{}
		for i := range dueRuns {
			held, err := w.envelopesOf(ctx, dueNamespaces[i], agk.RunID(dueRuns[i]))
			if err != nil {
				return err
			}
			handed, err := w.inputsOf(ctx, dueNamespaces[i], agk.RunID(dueRuns[i]))
			if err != nil {
				return err
			}
			for _, e := range append(held, handed...) {
				counted[object{dueNamespaces[i], "sha256:" + e.Digest}]++
			}
		}
		var objectNamespaces, digests []string
		var by []int32
		for o, n := range counted {
			objectNamespaces, digests, by = append(objectNamespaces, o.namespace), append(digests, o.digest), append(by, int32(n))
		}
		if len(digests) > 0 {
			if _, err := w.tx.Exec(ctx, `
				update artifact_objects o
				set refs = greatest(o.refs - g.n, 0),
				    collectable_at = case when o.refs - g.n <= 0 then now() else null end
				from unnest($1::text[], $2::text[], $3::int[]) as g(namespace, digest, n)
				where o.namespace = g.namespace and o.digest = g.digest and o.refs > 0`,
				objectNamespaces, digests, by); err != nil {
				return err
			}
		}
		if len(dueRuns) > 0 {
			if _, err := w.tx.Exec(ctx, `
				update steps s set envelopes_purged_at = now()
				from unnest($1::text[], $2::text[]) as g(namespace, run_id)
				where s.namespace = g.namespace and s.run_id = g.run_id and s.envelopes_purged_at is null`,
				dueNamespaces, dueRuns); err != nil {
				return err
			}
		}
		if _, err := w.tx.Exec(ctx, `
			update runs r set envelopes_purged_at = now()
			from unnest($1::text[], $2::text[]) as g(namespace, id)
			where r.namespace = g.namespace and r.id = g.id`, namespaces, runs); err != nil {
			return err
		}
		purged = len(dueRuns)
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
//
// The keys are every object recorded for the log, those a failed write left behind included,
// and batch bounds them as it bounds the logs, since one log may be written in thousands of
// chunks: a log with more is handed over again, the rest of it, at the next claim. A task whose
// log is being written to is skipped, since its row is held until the chunk and its key are
// recorded, and is claimed on a later pass: claimed now, the object being written would be
// written after the claim and named by nothing the purge still holds. A shipment arriving once
// the run's retention has run out is refused, so a log claimed is one nothing is added to.
func (p *Pool) ExpiredLogs(ctx context.Context, batch int) ([]Log, error) {
	batch, err := batchOf(batch)
	if err != nil {
		return nil, err
	}
	var out []Log
	err = p.Installation(ctx, Purge, func(ctx context.Context, w *Wide) error {
		rows, err := w.tx.Query(ctx, `
			select t.namespace, t.id, t.log_uri
			from tasks t join runs r on r.namespace = t.namespace and r.id = t.run_id
			where t.log_uri is not null
			  and r.expires_at is not null and r.expires_at <= now()
			  and r.logs_purged_at is null
			order by r.expires_at
			limit $1
			for update of t skip locked`, batch)
		if err != nil {
			return err
		}
		index := map[[2]string]int{}
		for rows.Next() {
			var l Log
			if err := rows.Scan(&l.Namespace, &l.Task, &l.URI); err != nil {
				rows.Close()
				return err
			}
			index[[2]string{l.Namespace, l.Task}] = len(out)
			out = append(out, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(out) == 0 {
			return err
		}

		namespaces := make([]string, len(out))
		tasks := make([]string, len(out))
		for i, l := range out {
			namespaces[i], tasks[i] = l.Namespace, l.Task
		}
		keys, err := w.tx.Query(ctx, `
			select o.namespace, o.task_id, o.object_key
			from task_log_objects o
			join unnest($1::text[], $2::text[]) as g(namespace, id)
			  on o.namespace = g.namespace and o.task_id = g.id
			order by o.recorded_at, o.object_key
			limit $3`, namespaces, tasks, batch)
		if err != nil {
			return err
		}
		defer keys.Close()
		for keys.Next() {
			var namespace, task, key string
			if err := keys.Scan(&namespace, &task, &key); err != nil {
				return err
			}
			l := &out[index[[2]string{namespace, task}]]
			l.Keys = append(l.Keys, key)
		}
		return keys.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("db: the logs due for purging could not be read: %w", err)
	}
	return out, nil
}

// LogsPurged records that the objects of those logs are gone from the store, and answers how many
// of the logs are now gone whole.
//
// It forgets the keys it was handed and the chunks they held, and nothing else, so an object
// recorded after the claim is still there for the next one. A log none of whose objects is left
// has its URI taken off its task; where each log stood stays: how many lines it held and whether
// the cap cut it short.
func (p *Pool) LogsPurged(ctx context.Context, logs []Log) (int, error) {
	if len(logs) == 0 {
		return 0, nil
	}
	namespaces := make([]string, len(logs))
	tasks := make([]string, len(logs))
	var keyNamespaces, keyTasks, keys []string
	for i, l := range logs {
		namespaces[i], tasks[i] = l.Namespace, l.Task
		for _, k := range l.Keys {
			keyNamespaces, keyTasks, keys = append(keyNamespaces, l.Namespace), append(keyTasks, l.Task), append(keys, k)
		}
	}
	var cleared int
	err := p.Installation(ctx, Purge, func(ctx context.Context, w *Wide) error {
		if len(keys) > 0 {
			if _, err := w.tx.Exec(ctx, `
				delete from task_log_objects o
				using unnest($1::text[], $2::text[], $3::text[]) as g(namespace, id, object_key)
				where o.namespace = g.namespace and o.task_id = g.id and o.object_key = g.object_key`,
				keyNamespaces, keyTasks, keys); err != nil {
				return err
			}
			if _, err := w.tx.Exec(ctx, `
				delete from task_log_chunks c
				using unnest($1::text[], $2::text[], $3::text[]) as g(namespace, id, object_key)
				where c.namespace = g.namespace and c.task_id = g.id and c.object_key = g.object_key`,
				keyNamespaces, keyTasks, keys); err != nil {
				return err
			}
		}
		if err := w.tx.QueryRow(ctx, `
			select count(*) from unnest($1::text[], $2::text[]) as g(namespace, id)
			where not exists (select 1 from task_log_objects o
			                  where o.namespace = g.namespace and o.task_id = g.id)`,
			namespaces, tasks).Scan(&cleared); err != nil {
			return err
		}
		_, err := w.tx.Exec(ctx, `
			update tasks t set log_uri = null
			from unnest($1::text[], $2::text[]) as g(namespace, id)
			where t.namespace = g.namespace and t.id = g.id
			  and not exists (select 1 from task_log_objects o
			                  where o.namespace = t.namespace and o.task_id = t.id)`, namespaces, tasks)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("db: the purged logs could not be recorded: %w", err)
	}
	return cleared, nil
}

// LogsGone stamps runs whose retention has run out and none of whose tasks holds a log any more,
// batch runs at a time, so that ExpiredLogs looks no further at them, and answers how many it
// stamped.
//
// A run is stamped once nothing can give one of its tasks a log again. A shipment asks the run's
// retention once it holds its task, so the tasks of the runs are held here, waiting for any
// shipment already under way, and whether a task holds a log is asked only then, in a statement
// of its own: a shipment that got in first has named its log by then, and one that comes after
// finds the retention run out and writes nothing.
func (p *Pool) LogsGone(ctx context.Context, batch int) (int, error) {
	batch, err := batchOf(batch)
	if err != nil {
		return 0, err
	}
	var stamped int
	err = p.Installation(ctx, Purge, func(ctx context.Context, w *Wide) error {
		stamped = 0
		rows, err := w.tx.Query(ctx, `
			select r.namespace, r.id
			from runs r
			where r.expires_at is not null and r.expires_at <= now()
			  and r.logs_purged_at is null
			  and not exists (select 1 from tasks t
			                  where t.namespace = r.namespace and t.run_id = r.id
			                    and t.log_uri is not null)
			order by r.expires_at
			limit $1
			for update of r skip locked`, batch)
		if err != nil {
			return err
		}
		var namespaces, runs []string
		for rows.Next() {
			var namespace, run string
			if err := rows.Scan(&namespace, &run); err != nil {
				rows.Close()
				return err
			}
			namespaces, runs = append(namespaces, namespace), append(runs, run)
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(runs) == 0 {
			return err
		}
		if _, err := w.tx.Exec(ctx, `
			select 1 from tasks t
			join unnest($1::text[], $2::text[]) as g(namespace, run_id)
			  on t.namespace = g.namespace and t.run_id = g.run_id
			for update of t`, namespaces, runs); err != nil {
			return err
		}
		tag, err := w.tx.Exec(ctx, `
			update runs r set logs_purged_at = now()
			from unnest($1::text[], $2::text[]) as g(namespace, id)
			where r.namespace = g.namespace and r.id = g.id
			  and not exists (select 1 from tasks t
			                  where t.namespace = r.namespace and t.run_id = r.id
			                    and t.log_uri is not null)`, namespaces, runs)
		if err != nil {
			return err
		}
		stamped = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("db: the runs whose logs are gone could not be recorded: %w", err)
	}
	return stamped, nil
}

// PurgeUploads forgets writes whose room lapsed, batch at a time, and answers how many.
//
// A write holds its row from before its bytes are read until a quarter of an hour after the
// policy it was written with, by when the result that references the object has been heard or
// never will be. Past that the row holds nothing back and counts nothing, and a namespace with
// no max_artifact_bytes never counts its uploads again, so nothing but this lets them go.
func (p *Pool) PurgeUploads(ctx context.Context, batch int) (int, error) {
	batch, err := batchOf(batch)
	if err != nil {
		return 0, err
	}
	var forgotten int
	err = p.Installation(ctx, Purge, func(ctx context.Context, w *Wide) error {
		tag, err := w.tx.Exec(ctx, `
			delete from artifact_uploads
			where (namespace, id) in (
			  select namespace, id from artifact_uploads
			  where until <= now()
			  order by until
			  limit $1
			  for update skip locked
			)`, batch)
		if err != nil {
			return err
		}
		forgotten = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("db: the lapsed uploads could not be forgotten: %w", err)
	}
	return forgotten, nil
}

// Collectable claims objects nothing references any more.
//
// Claimed and not deleted: the caller deletes the keys from the store through Collecting and
// then calls Collected. An object referenced again between the claim and the deletion is not
// lost, because the writer that referenced it was told to write the bytes again. An object a
// write is under way for, or that a live artifact names, is not claimed at all.
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
			  select o.namespace, o.digest from artifact_objects o
			  where o.refs = 0
			    and o.collectable_at is not null
			    and o.collectable_at <= now() - ($1::bigint * interval '1 second')
			    and `+unheld+`
			  order by o.collectable_at
			  limit $2
			  for update of o skip locked
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

// unheld is what keeps an object from the collector whatever its count says, as a condition on
// the object o: a write of it under way, whose bytes nothing else could write again, and a live
// artifact naming it, which its count should have kept and a count gone wrong would not.
const unheld = `not exists (select 1 from artifact_uploads u
	                  where u.namespace = o.namespace and u.digest = o.digest and u.until > now())
	    and not exists (select 1 from artifacts a
	                    where a.namespace = o.namespace and a.digest = o.digest and a.status = 'live')`

// Collecting hands remove each object Collectable claimed whose bytes may go, and answers those
// remove deleted, for Collected to confirm.
//
// Each is handed over under a lock on its row, taken before anything else is asked and held
// until every one is deleted: an object a writer has referenced since the claim, or one a write
// is under way for, is passed by, and a write of one that is held waits until its bytes are
// gone and writes them afresh. Whether a write is under way is asked in a statement of its own
// once the rows are held, since a write that recorded itself after the first statement began
// would be invisible to it. An object remove could not delete is left claimed for the next
// sweep, and said in the error, with the others deleted all the same.
//
// remove must answer nil for bytes that are already gone, which a sweep that died after deleting
// them and before confirming leaves.
func (p *Pool) Collecting(ctx context.Context, claimed []Object, remove func(context.Context, Object) error) ([]Object, error) {
	if len(claimed) == 0 {
		return nil, nil
	}
	namespaces := make([]string, len(claimed))
	digests := make([]string, len(claimed))
	for i, o := range claimed {
		if !hexDigest.MatchString(o.Digest) {
			return nil, fmt.Errorf("db: %q is not a digest", o.Digest)
		}
		namespaces[i], digests[i] = o.Namespace, "sha256:"+o.Digest
	}
	var gone []Object
	var failed []error
	err := p.Installation(ctx, Collect, func(ctx context.Context, w *Wide) error {
		gone, failed = nil, nil
		held, err := objectsOf(ctx, w.tx, `
			select o.namespace, o.digest from artifact_objects o
			join unnest($1::text[], $2::text[]) as g(namespace, digest)
			  on o.namespace = g.namespace and o.digest = g.digest
			where o.refs = 0 and o.collecting_at is not null
			for update of o skip locked`, namespaces, digests)
		if err != nil || len(held) == 0 {
			return err
		}
		heldNamespaces := make([]string, 0, len(held))
		heldDigests := make([]string, 0, len(held))
		for o := range held {
			heldNamespaces, heldDigests = append(heldNamespaces, o[0]), append(heldDigests, o[1])
		}
		free, err := objectsOf(ctx, w.tx, `
			select o.namespace, o.digest
			from unnest($1::text[], $2::text[]) as o(namespace, digest)
			where `+unheld, heldNamespaces, heldDigests)
		if err != nil {
			return err
		}
		for _, o := range claimed {
			if !free[[2]string{o.Namespace, "sha256:" + o.Digest}] {
				continue
			}
			if err := remove(ctx, o); err != nil {
				failed = append(failed, err)
				continue
			}
			gone = append(gone, o)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("db: the collected objects could not be deleted: %w", err)
	}
	if len(failed) > 0 {
		return gone, fmt.Errorf("db: %d of the objects claimed could not be deleted from the store and are left for the next sweep, the first: %w", len(failed), failed[0])
	}
	return gone, nil
}

// objectsOf reads the namespace and digest of each row a query answers.
func objectsOf(ctx context.Context, tx pgx.Tx, query string, args ...any) (map[[2]string]bool, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]string]bool{}
	for rows.Next() {
		var namespace, digest string
		if err := rows.Scan(&namespace, &digest); err != nil {
			return nil, err
		}
		out[[2]string{namespace, digest}] = true
	}
	return out, rows.Err()
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
