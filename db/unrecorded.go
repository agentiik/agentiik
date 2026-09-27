package db

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5/pgconn"
)

// The artifact files v0.2 left unrecorded.
//
// v0.2 recorded a reference only for an output its workflow gave a retain, so the files of every
// other output are named by the envelopes their steps published and by no row: nothing counts
// them, nothing expires them, and the collection never reaches them. A decision of this release
// records every file of every envelope its run keeps and sets runs.files_recorded, so a finished
// run that has it unset was finished by a controller that did not, which migration 0041 sets out.
// init, migrate and the controller that leads record their files as a decision would have, an
// artifact of the run for every file its envelopes name, the shards' included, expiring the
// namespace's max_retention_days after the run finished, as migration 0038 dates the run's
// envelopes and logs, which a run finished after that migration is given here too: no file is
// collected while an envelope naming it is kept. From there the purges retire and collect them as
// any other.
//
// Package db does not reach the object store, where the envelopes are, so it is two calls:
// UnrecordedRuns answers the runs and the envelopes they keep, the caller reads the
// files those name, and RecordUnrecorded records them and sets the run's column in one
// transaction. A run is recorded whole or not at all, and a caller cut short between two batches
// leaves the runs it did not reach for its next call, which records nothing twice: a reference
// already there is left as it is and counted once.

// recordingWaits is the longest RecordUnrecorded waits for a lock. It takes the rows it records
// only where nobody holds them, but writing an object's row may meet a writer creating the same
// row at that moment, a decision counting the same bytes, which it can only wait for, and the
// decision may be waiting for an object this holds. Past this it lets the decision go first and
// leaves its runs for a later call, rather than wait the second PostgreSQL takes to see a deadlock
// and end one of the two, which may be the decision.
const recordingWaits = "200ms"

// lockNotAvailable is PostgreSQL's code for a lock not taken within lock_timeout.
const lockNotAvailable = "55P03"

// Unrecorded is one finished run whose artifact files are still to be recorded.
type Unrecorded struct {
	Namespace string
	Run       agk.RunID

	// Envelopes are the digests of the envelopes the run keeps, the bare hexadecimal, whose files
	// are its artifacts: what its steps published, and what their shards produced, as a decision
	// records them, which includes the shards of a step that never published because another
	// failed.
	Envelopes []string
}

// UnrecordedRuns answers at most batch runs whose artifact files are still to be recorded, those
// after the run of after's namespace and identifier in that order, from the first where after's
// namespace is empty, with the envelopes they keep: those their decision references, published and
// per shard, and those their steps published, each once.
//
// A caller goes on from the last run it was answered rather than from the first left, so that a
// run another caller is recording at that moment, or one whose objects a writer held, is passed by
// and never read again in a loop.
func (p *Pool) UnrecordedRuns(ctx context.Context, after Unrecorded, batch int) ([]Unrecorded, error) {
	batch, err := batchOf(batch)
	if err != nil {
		return nil, err
	}
	var out []Unrecorded
	err = p.Installation(ctx, Purge, func(ctx context.Context, w *Wide) error {
		out = nil
		query := `select namespace, id from runs
			where finished_at is not null and not files_recorded
			order by namespace, id limit $1`
		args := []any{batch}
		if after.Namespace != "" {
			query = `select namespace, id from runs
				where finished_at is not null and not files_recorded
				  and (namespace, id) > ($2::text, $3::text)
				order by namespace, id limit $1`
			args = append(args, after.Namespace, string(after.Run))
		}
		runs, err := pairsOf(ctx, w.tx, query, args...)
		if err != nil || len(runs) == 0 {
			return err
		}
		index := map[[2]string]int{}
		for i, r := range runs {
			index[r] = i
			out = append(out, Unrecorded{Namespace: r[0], Run: agk.RunID(r[1])})
		}
		rows, err := w.tx.Query(ctx, `
			select namespace, run_id, digest from (
			  select s.namespace, s.run_id, substr(p.value->>'digest', 8) as digest
			  from steps s
			  join unnest($1::text[], $2::text[]) as g(namespace, run_id)
			    on s.namespace = g.namespace and s.run_id = g.run_id,
			  lateral jsonb_each(s.ports) as p
			  where p.value ? 'digest'
			  union
			  select r.namespace, r.id, e->>'digest'
			  from runs r
			  join unnest($1::text[], $2::text[]) as g(namespace, run_id)
			    on r.namespace = g.namespace and r.id = g.run_id,
			  lateral jsonb_array_elements(coalesce(r.evaluation->'envelopes', '[]'::jsonb)) as e
			  where e ? 'digest'
			) kept
			order by namespace, run_id, digest`, runs.first(), runs.second())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var namespace, run, digest string
			if err := rows.Scan(&namespace, &run, &digest); err != nil {
				return err
			}
			u := &out[index[[2]string{namespace, run}]]
			u.Envelopes = append(u.Envelopes, digest)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("db: the runs whose artifact files are still to be recorded could not be read: %w", err)
	}
	return out, nil
}

// Recording is one run's files, as the envelopes its steps published name them, for
// RecordUnrecorded.
type Recording struct {
	Namespace string
	Run       agk.RunID
	Files     []agk.File
}

// Recorded is what RecordUnrecorded did.
type Recorded struct {
	// Runs are the runs whose files are recorded, and which are waited for no longer.
	Runs int

	// Artifacts are the references it wrote, a reference already there being left as it was.
	Artifacts int

	// Left are the runs it could not record now, since a writer held an object one of their
	// files is: they are still waited for, for a later call to record.
	Left int
}

// RecordUnrecorded records the files of each run as its artifacts, expiring its namespace's
// max_retention_days after it finished, and sets the run's files_recorded, all in one
// transaction. A run given no expiry, as one a v0.2 controller finished after migration 0038 is,
// is given the same, for its envelopes and logs.
//
// A file is recorded under the URI its envelope names, as a decision records it, where that URI is
// of the run itself and of one of its steps: a file of another run is recorded, if at all, by that
// run's own publication, and dated by that run's end. A reference already there is left as it was,
// with its own expiry and its count, which is how an output v0.2 gave a retain keeps what it was
// given, and a run recorded twice is counted once.
//
// An object is counted up by the references written, under a lock on its row, as a decision
// counts it. It waits on nothing it can pass by: a run another call is recording is passed by, and
// so is a file whose object a writer holds at that moment, its run being left waiting, whole, for a
// later call; a decision holds its objects while it writes its artifacts, and a recording that
// waited for it could wait on a decision waiting on it. What it cannot pass by, a writer creating
// an object's row as this does, it waits for recordingWaits at the most, and leaves the whole
// batch for a later call past it. A file whose object is being collected is not recorded,
// since its bytes may be gone by now, nor one whose size is not its object's: a reference that
// named no bytes would be answered as an artifact nobody can fetch.
func (p *Pool) RecordUnrecorded(ctx context.Context, runs []Recording) (Recorded, error) {
	if len(runs) == 0 {
		return Recorded{}, nil
	}
	var out Recorded
	var taken pairs
	err := p.Installation(ctx, Purge, func(ctx context.Context, w *Wide) error {
		out = Recorded{}
		if _, err := w.tx.Exec(ctx, `select set_config('lock_timeout', $1, true)`, recordingWaits); err != nil {
			return err
		}
		var asked pairs
		for _, r := range runs {
			asked = append(asked, [2]string{r.Namespace, string(r.Run)})
		}
		var err error
		taken, err = pairsOf(ctx, w.tx, `
			select r.namespace, r.id from runs r
			join unnest($1::text[], $2::text[]) as g(namespace, id)
			  on r.namespace = g.namespace and r.id = g.id
			where r.finished_at is not null and not r.files_recorded
			for update of r skip locked`, asked.first(), asked.second())
		if err != nil || len(taken) == 0 {
			return err
		}
		held := map[[2]string]bool{}
		for _, r := range taken {
			held[r] = true
		}

		type object struct{ namespace, digest string }
		type reference struct {
			object
			run                   [2]string
			step, port, name, typ string
			size                  int64
		}
		var refs []reference
		sizes := map[object]int64{}
		types := map[object]string{}
		naming := map[object][][2]string{}
		spoilt := map[object]bool{}
		seen := map[string]bool{}
		for _, r := range runs {
			run := [2]string{r.Namespace, string(r.Run)}
			if !held[run] {
				continue
			}
			for _, f := range r.Files {
				if f.URI.Run != r.Run || f.URI.Name == "" || strings.Contains(f.URI.Name, "/") ||
					f.URI.Step.Validate() != nil || f.URI.Port.Validate() != nil ||
					!hexDigest.MatchString(f.SHA256) || f.Size < 0 {
					continue
				}
				uri := r.Namespace + "\x00" + f.URI.String()
				if seen[uri] {
					continue
				}
				seen[uri] = true
				o := object{r.Namespace, "sha256:" + f.SHA256}
				if size, ok := sizes[o]; ok && size != f.Size {
					spoilt[o] = true
				}
				typ := f.MediaType
				if typ == "" {
					typ = "application/octet-stream"
				}
				if _, ok := types[o]; !ok {
					sizes[o], types[o] = f.Size, typ
				}
				naming[o] = append(naming[o], run)
				refs = append(refs, reference{object: o, run: run, step: string(f.URI.Step), port: string(f.URI.Port), name: f.URI.Name, typ: typ, size: f.Size})
			}
		}

		// The objects that are there already, then those of them this can hold, in one order: one
		// there and not held is one a writer holds, and the runs naming it wait for a later call.
		objects := make([]object, 0, len(sizes))
		for o := range sizes {
			objects = append(objects, o)
		}
		sort.Slice(objects, func(i, j int) bool {
			if objects[i].namespace != objects[j].namespace {
				return objects[i].namespace < objects[j].namespace
			}
			return objects[i].digest < objects[j].digest
		})
		var wanted pairs
		for _, o := range objects {
			wanted = append(wanted, [2]string{o.namespace, o.digest})
		}
		there, err := pairsOf(ctx, w.tx, `
			select o.namespace, o.digest from artifact_objects o
			join unnest($1::text[], $2::text[]) as g(namespace, digest)
			  on o.namespace = g.namespace and o.digest = g.digest`, wanted.first(), wanted.second())
		if err != nil {
			return err
		}
		rows, err := w.tx.Query(ctx, `
			select o.namespace, o.digest, o.size_bytes, o.collecting_at is not null
			from artifact_objects o
			join unnest($1::text[], $2::text[]) as g(namespace, digest)
			  on o.namespace = g.namespace and o.digest = g.digest
			order by o.namespace, o.digest
			for update of o skip locked`, wanted.first(), wanted.second())
		if err != nil {
			return err
		}
		locked := map[object]bool{}
		for rows.Next() {
			var o object
			var size int64
			var collecting bool
			if err := rows.Scan(&o.namespace, &o.digest, &size, &collecting); err != nil {
				rows.Close()
				return err
			}
			locked[o] = true
			if collecting || size != sizes[o] {
				spoilt[o] = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		busy := map[[2]string]bool{}
		for _, t := range there {
			if o := (object{t[0], t[1]}); !locked[o] {
				for _, run := range naming[o] {
					busy[run] = true
				}
			}
		}

		var namespaces, runIDs, steps, ports, names, digests, media []string
		var bytes []int64
		for _, r := range refs {
			if spoilt[r.object] || busy[r.run] {
				continue
			}
			namespaces, runIDs, steps = append(namespaces, r.namespace), append(runIDs, r.run[1]), append(steps, r.step)
			ports, names, digests = append(ports, r.port), append(names, r.name), append(digests, r.digest)
			media, bytes = append(media, r.typ), append(bytes, r.size)
		}
		counted := map[object]int32{}
		if len(names) > 0 {
			written, err := pairsOf(ctx, w.tx, `
				insert into artifacts (namespace, run_id, step, port, name, digest, size_bytes, media_type, expires_at)
				select g.namespace, g.run_id, g.step, g.port, g.name, g.digest, g.size, g.media_type,
				       r.finished_at + n.max_retention_days * interval '1 day'
				from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::bigint[], $8::text[])
				  as g(namespace, run_id, step, port, name, digest, size, media_type)
				join runs r on r.namespace = g.namespace and r.id = g.run_id
				join namespaces n on n.name = g.namespace
				join steps s on s.namespace = g.namespace and s.run_id = g.run_id and s.step = g.step
				where r.finished_at is not null
				on conflict (namespace, run_id, step, port, name) do nothing
				returning namespace, digest`,
				namespaces, runIDs, steps, ports, names, digests, bytes, media)
			if err != nil {
				return err
			}
			for _, o := range written {
				counted[object{o[0], o[1]}]++
			}
			out.Artifacts = len(written)
		}

		// Counted up by what was written, in the order the rows were locked in, and never where
		// the row is another size: a row that appeared since it was looked for is a writer's, which
		// counts its own reference, and one at another size names other bytes.
		var objectNamespaces, objectDigests, objectTypes []string
		var objectSizes []int64
		var by []int32
		for _, o := range objects {
			if counted[o] == 0 {
				continue
			}
			objectNamespaces, objectDigests = append(objectNamespaces, o.namespace), append(objectDigests, o.digest)
			objectSizes, objectTypes, by = append(objectSizes, sizes[o]), append(objectTypes, types[o]), append(by, counted[o])
		}
		if len(by) > 0 {
			tag, err := w.tx.Exec(ctx, `
				insert into artifact_objects (namespace, digest, size_bytes, media_type, refs)
				select * from unnest($1::text[], $2::text[], $3::bigint[], $4::text[], $5::int[])
				on conflict (namespace, digest) do update
				set refs = artifact_objects.refs + excluded.refs, collectable_at = null, collecting_at = null
				where artifact_objects.size_bytes = excluded.size_bytes`,
				objectNamespaces, objectDigests, objectSizes, objectTypes, by)
			if err != nil {
				return err
			}
			if int(tag.RowsAffected()) != len(by) {
				return fmt.Errorf("an object was recorded at another size while its references were being written, and the %d runs of this batch are left for the next recording", len(taken))
			}
		}

		var doneNamespaces, doneRuns []string
		for _, r := range taken {
			if busy[r] {
				out.Left++
				continue
			}
			doneNamespaces, doneRuns = append(doneNamespaces, r[0]), append(doneRuns, r[1])
		}
		if len(doneRuns) > 0 {
			if _, err := w.tx.Exec(ctx, `
				update runs r
				set files_recorded = true,
				    expires_at = coalesce(r.expires_at, r.finished_at + n.max_retention_days * interval '1 day')
				from unnest($1::text[], $2::text[]) as g(namespace, id), namespaces n
				where r.namespace = g.namespace and r.id = g.id and n.name = r.namespace`, doneNamespaces, doneRuns); err != nil {
				return err
			}
		}
		out.Runs = len(doneRuns)
		return nil
	})
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == lockNotAvailable {
		return Recorded{Left: len(taken)}, nil
	}
	if err != nil {
		return Recorded{}, fmt.Errorf("db: the artifact files still to be recorded could not be recorded: %w", err)
	}
	return out, nil
}
