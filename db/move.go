package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/audit"
	"github.com/jackc/pgx/v5"
)

// Moving a workflow to another namespace.
//
// "A move answers 202 with Location, whatever else the body names, and carries everything too,
// grants included, runs and artifacts re-keyed and re-counted under the target, the workflow frozen
// meanwhile, its pushes, runs and replays answered 409; it is refused with 409 while a run of it is
// queued or running, where the target's quotas cannot hold what moves, and where it names a secret
// the target does not declare."
//
// It is two acts. AskMove, in the answer, judges the move, holds the name in the target and freezes
// the workflow, one row of workflow_moves. CompleteMove, by the controller that leads, once the
// objects the workflow's rows name are copied under the target's keys, changes the workflow's row,
// which every key naming it carries through (migrations 0053 and 0057), counts its objects in the
// target as it lets them go in the source, rewrites the keys of its logs, keeps what it left under
// the source's for the grace, and records the move in both namespaces.

// ErrWorkflowMoving is a workflow frozen by a move asked and not yet carried out: nothing that
// writes it is let in until it is done, a push, a run, a replay, a rename or a deletion.
var ErrWorkflowMoving = errors.New("db: the workflow is being moved to another namespace")

// ErrRunsGoing is a workflow a run of which has not finished, queued, running or waiting, which a
// move refuses: the document such a run is decided from names the namespace it started in.
var ErrRunsGoing = errors.New("db: a run of the workflow has not finished")

// SharedOutputs is a run of the workflow that republished outputs a run of another workflow of its
// namespace made, or one of another workflow that republished outputs of one of its runs: "a hit
// republishes the same envelopes without starting a container", naming the files the first run
// made, which a move would leave in one namespace while the run naming them is in the other.
type SharedOutputs struct {
	Run, From agk.RunID
}

func (e *SharedOutputs) Error() string {
	return fmt.Sprintf("db: run %s republished what run %s made", e.Run, e.From)
}

// NoRoomToMove is a move whose target's max_artifact_bytes cannot hold the live artifacts that
// move, beside what it holds.
type NoRoomToMove struct {
	Held, Moving, Limit int64
}

func (e *NoRoomToMove) Error() string {
	return fmt.Sprintf("db: the target holds %d bytes of its %d, and the move brings %d", e.Held, e.Limit, e.Moving)
}

// unfinished are the states of a run that has not finished, which a move refuses and waits on.
const unfinished = `('queued', 'running', 'waiting')`

// nameKept takes the lock every creation of a workflow's name in a namespace takes, and refuses a
// name a move holds there: a workflow created, pushed or renamed under it, or another move to it.
// The lock is an advisory one on the name, since the two things that can hold a name, a row of
// workflows and a row of move_targets, are two tables that no one row lock covers.
func nameKept(ctx context.Context, tx pgx.Tx, namespace, name string) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1, 57))`,
		"workflow:"+namespace+"/"+name); err != nil {
		return fmt.Errorf("db: the name %s/%s could not be held: %w", namespace, name, err)
	}
	var held bool
	if err := tx.QueryRow(ctx,
		`select exists (select 1 from move_targets where namespace = $1 and name = $2)`,
		namespace, name).Scan(&held); err != nil {
		return fmt.Errorf("db: whether a move holds %s/%s could not be read: %w", namespace, name, err)
	}
	if held {
		return fmt.Errorf("%w: %s/%s, which a workflow moving there holds", ErrWorkflowExists, namespace, name)
	}
	return nil
}

// moving answers ErrWorkflowMoving where a move of the workflow was asked and not carried out.
func moving(ctx context.Context, tx pgx.Tx, namespace, workflow string) error {
	var asked bool
	if err := tx.QueryRow(ctx,
		`select exists (select 1 from workflow_moves where namespace = $1 and workflow = $2)`,
		namespace, workflow).Scan(&asked); err != nil {
		return fmt.Errorf("db: whether %s is moving could not be read: %w", workflow, err)
	}
	if asked {
		return fmt.Errorf("%w: %s/%s", ErrWorkflowMoving, namespace, workflow)
	}
	return nil
}

// Moving says whether a move of the workflow was asked and not carried out, for a caller that
// answers so before it writes anything.
func (n *NS) Moving(ctx context.Context, workflow string) error {
	return moving(ctx, n.tx, n.namespace, workflow)
}

// Change is what a request asking a move changes besides, made first in the move's transaction and
// in the workflow's namespace, so that a move refused changes nothing: "each field is under its own
// permission, and a request naming several changes nothing unless every one of them is held". It
// answers the name the workflow has once it is made, which is the name the move holds in the
// target, and the audit records it makes, which are appended last.
type Change func(ctx context.Context, ns *NS) (name string, records []audit.Record, err error)

// ChangeRefused is what a Change was refused for, told apart from what the move is refused for,
// since both can be a name taken: the change's in the namespace the workflow is in, the move's in
// the target.
type ChangeRefused struct{ Err error }

func (e *ChangeRefused) Error() string { return e.Err.Error() }
func (e *ChangeRefused) Unwrap() error { return e.Err }

// AskMove judges a move of a workflow to target and, where it may be made, holds its name there and
// freezes it until the controller carries it out. change, where it is not nil, is made first, and
// is undone with everything else where the move is refused.
//
// In one transaction on the installation, since it reads both namespaces: the workflow's row under
// the lock every writer of its repository takes, then the name in the target under the lock every
// creation of a name takes. It is refused, in this order: ChangeRefused; ErrNoWorkflow;
// ErrWorkflowMoving where a move of it was asked already; ErrNoNamespace where the target does not
// exist; ErrRunsGoing while a run of it has not finished; SharedOutputs where one of its runs and a
// run of another workflow share outputs a cache hit republished; ErrWorkflowExists or
// ErrWorkflowPurging where the target holds the name; NoRoomToMove where the target's
// max_artifact_bytes cannot hold its live artifacts beside those of the moves asked to it before.
//
// Its cache entries go at once, which it could not be refused for: a cache is what it costs to make
// an output again, and an entry another workflow's run found while this one waits would be outputs
// shared across the move.
func (p *Pool) AskMove(ctx context.Context, namespace, workflow, target, by string, at time.Time, change Change) error {
	return p.Installation(ctx, WorkflowMove, func(ctx context.Context, w *Wide) error {
		tx := w.tx
		var records []audit.Record
		if change != nil {
			var refused error
			if err := w.within(ctx, namespace, func(ctx context.Context, ns *NS) error {
				workflow, records, refused = change(ctx, ns)
				return refused
			}); err != nil {
				if refused != nil {
					return &ChangeRefused{Err: refused}
				}
				return err
			}
		}
		var one int
		err := tx.QueryRow(ctx,
			`select 1 from workflows where namespace = $1 and name = $2 and deleted_at is null for no key update`,
			namespace, workflow).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s/%s", ErrNoWorkflow, namespace, workflow)
		}
		if err != nil {
			return fmt.Errorf("db: workflow %s could not be locked: %w", workflow, err)
		}
		if err := moving(ctx, tx, namespace, workflow); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `select 1 from namespaces where name = $1`, target).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNoNamespace, target)
		}
		if err != nil {
			return fmt.Errorf("db: namespace %s could not be read: %w", target, err)
		}
		var going bool
		if err := tx.QueryRow(ctx,
			`select exists (select 1 from runs where namespace = $1 and workflow = $2 and state in `+unfinished+`)`,
			namespace, workflow).Scan(&going); err != nil {
			return fmt.Errorf("db: the runs of %s could not be read: %w", workflow, err)
		}
		if going {
			return fmt.Errorf("%w: %s/%s", ErrRunsGoing, namespace, workflow)
		}
		if err := sharedOutputs(ctx, tx, namespace, workflow); err != nil {
			return err
		}
		if err := nameKept(ctx, tx, target, workflow); err != nil {
			return err
		}
		var held, purging bool
		if err := tx.QueryRow(ctx,
			`select count(*) > 0, coalesce(bool_or(deleted_at is not null), false) from workflows where namespace = $1 and name = $2`,
			target, workflow).Scan(&held, &purging); err != nil {
			return fmt.Errorf("db: whether %s/%s is taken could not be read: %w", target, workflow, err)
		}
		switch {
		case purging:
			return fmt.Errorf("%w: %s/%s", ErrWorkflowPurging, target, workflow)
		case held:
			return fmt.Errorf("%w: %s/%s", ErrWorkflowExists, target, workflow)
		}
		if err := roomToMove(ctx, tx, namespace, workflow, target); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`insert into workflow_moves (namespace, workflow, target, asked_by, asked_at) values ($1, $2, $3, $4, $5)`,
			namespace, workflow, target, by, at); err != nil {
			return fmt.Errorf("db: the move of %s could not be recorded: %w", workflow, err)
		}
		if _, err := tx.Exec(ctx,
			`insert into move_targets (namespace, name, from_namespace) values ($1, $2, $3)`,
			target, workflow, namespace); err != nil {
			return fmt.Errorf("db: the name %s could not be held in %s: %w", workflow, target, err)
		}
		if _, err := tx.Exec(ctx,
			`delete from step_cache c using runs r
			 where r.namespace = c.namespace and r.id = c.run_id and r.namespace = $1 and r.workflow = $2`,
			namespace, workflow); err != nil {
			return fmt.Errorf("db: the cache entries of %s could not be let go: %w", workflow, err)
		}
		for _, r := range records {
			if err := w.AuditIn(ctx, namespace, r); err != nil {
				return err
			}
		}
		return nil
	})
}

// sharedOutputs refuses a workflow one of whose runs republished outputs a run of another workflow
// of its namespace made, or the other way round. A run gone since no longer holds what it made, and
// shares nothing any more.
func sharedOutputs(ctx context.Context, tx pgx.Tx, namespace, workflow string) error {
	var run, from string
	err := tx.QueryRow(ctx, `
		select t.run_id, t.memoised_from
		from tasks t
		join runs r on r.namespace = t.namespace and r.id = t.run_id
		join runs o on o.namespace = t.namespace and o.id = t.memoised_from
		where t.namespace = $1 and t.memoised_from is not null
		  and (r.workflow = $2) <> (o.workflow = $2)
		order by t.run_id
		limit 1`, namespace, workflow).Scan(&run, &from)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("db: the outputs the runs of %s share could not be read: %w", workflow, err)
	}
	return &SharedOutputs{Run: agk.RunID(run), From: agk.RunID(from)}
}

// roomToMove refuses a move whose target's max_artifact_bytes cannot hold the workflow's live
// artifacts beside what it holds, each digest counted once, as the quota counts one, and those the
// target holds already not at all. The moves asked to the target and not yet carried out bring
// theirs too, and are counted with this one's: two moves asked one after the other, each within the
// quota alone, would otherwise be carried out beyond it together. The moves into one target are
// judged one at a time for that, under a lock on the target no other write takes.
func roomToMove(ctx context.Context, tx pgx.Tx, namespace, workflow, target string) error {
	var limit *int64
	if err := tx.QueryRow(ctx, `select max_artifact_bytes from namespaces where name = $1`, target).Scan(&limit); err != nil {
		return fmt.Errorf("db: the quota of %s could not be read: %w", target, err)
	}
	if limit == nil {
		return nil
	}
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1, 57))`, "moves-into:"+target); err != nil {
		return fmt.Errorf("db: the moves into %s could not be held: %w", target, err)
	}
	var held, bringing int64
	if err := tx.QueryRow(ctx, `
		with held as (`+heldBytes(" and namespace = $1", " and namespace = $1")+`),
		moving as (
		  select a.digest, max(a.size_bytes) as bytes
		  from artifacts a
		  join runs r on r.namespace = a.namespace and r.id = a.run_id
		  join (select $2::text as namespace, $3::text as workflow
		        union
		        select namespace, workflow from workflow_moves where target = $1) as m
		    on m.namespace = r.namespace and m.workflow = r.workflow
		  where a.status = 'live' and a.expires_at > now()
		  group by a.digest
		)
		select (select coalesce(sum(bytes), 0) from held)::bigint,
		       (select coalesce(sum(m.bytes), 0) from moving m
		        where not exists (select 1 from held h where h.digest = m.digest))::bigint`,
		target, namespace, workflow).Scan(&held, &bringing); err != nil {
		return fmt.Errorf("db: what %s holds could not be counted: %w", target, err)
	}
	if held+bringing > *limit {
		return &NoRoomToMove{Held: held, Moving: bringing, Limit: *limit}
	}
	return nil
}

// Move is a move asked and not yet carried out.
type Move struct {
	Namespace, Workflow, Target string
	AskedBy                     string
	AskedAt                     time.Time

	// Repository is the key its packs are kept under, which the move keeps.
	Repository string
}

// Moves reads the moves asked and not yet carried out, the oldest first, at most batch of them.
func (p *Pool) Moves(ctx context.Context, batch int) ([]Move, error) {
	var out []Move
	err := p.Installation(ctx, WorkflowMove, func(ctx context.Context, w *Wide) error {
		rows, err := w.tx.Query(ctx, `
			select m.namespace, m.workflow, m.target, m.asked_by, m.asked_at, w.repository
			from workflow_moves m
			join workflows w on w.namespace = m.namespace and w.name = m.workflow
			order by m.asked_at, m.namespace, m.workflow
			limit $1`, batch)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Move, error) {
			var m Move
			err := row.Scan(&m.Namespace, &m.Workflow, &m.Target, &m.AskedBy, &m.AskedAt, &m.Repository)
			return m, err
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("db: the moves asked could not be read: %w", err)
	}
	return out, nil
}

// MoveObjects are what a move copies under the target's keys before it is carried out: the digests
// of the objects the workflow's rows count, without the sha256: they are stored under, the keys of
// its logs, and the names of its packs. Ready is false while it cannot be carried out yet, a run of
// it not finished or a pack of it being collected, and the copying waits with it.
//
// Indexed are the keys of Logs whose chunks were indexed as they were read, whose objects were
// written then: the move is carried out only where no other chunk has been indexed since, which a
// task still stopping as its run ended can ship, and which the copy could not have seen.
type MoveObjects struct {
	Digests []string
	Logs    []string
	Indexed []string
	Packs   []string
	Ready   bool
}

// ObjectsOf reads what a move copies: every object a version of the workflow names in its tree, an
// envelope a run whose envelopes are kept names in its document or a grant of it counted, and a live
// artifact names; every key a log of its runs is written under; every pack of its repository live
// or superseded. It is read before the copying and counted again when the move is carried out, so
// what it names is at least what the move counts.
func (p *Pool) ObjectsOf(ctx context.Context, m Move) (MoveObjects, error) {
	var out MoveObjects
	err := p.Installation(ctx, WorkflowMove, func(ctx context.Context, w *Wide) error {
		ready, err := moveReady(ctx, w.tx, m)
		if err != nil || !ready {
			return err
		}
		counted, err := w.movingCounts(ctx, m)
		if err != nil {
			return err
		}
		for _, c := range counted {
			out.Digests = append(out.Digests, strings.TrimPrefix(c.digest, "sha256:"))
		}
		if out.Logs, err = movingLogs(ctx, w.tx, m); err != nil {
			return err
		}
		if out.Indexed, err = indexedLogs(ctx, w.tx, m.Namespace, m.Workflow); err != nil {
			return err
		}
		rows, err := w.tx.Query(ctx, `
			select name from git_packs
			where namespace = $1 and repository = $2 and state in ('live', 'superseded')
			order by name`, m.Namespace, m.Repository)
		if err != nil {
			return err
		}
		if out.Packs, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		out.Ready = true
		return nil
	})
	if err != nil {
		return MoveObjects{}, fmt.Errorf("db: what the move of %s/%s copies could not be read: %w", m.Namespace, m.Workflow, err)
	}
	return out, nil
}

// moveReady says whether a move can be carried out now: no run of the workflow unfinished, since a
// run let in before the move was asked may still be finishing; no pack of its repository being
// collected, whose files the collection is deleting under the source's keys; and no outputs shared
// with a run of another workflow, which a cache hit the controller had found as the move was asked
// can still have republished, and which the move waits out as it would have been refused for them.
func moveReady(ctx context.Context, tx pgx.Tx, m Move) (bool, error) {
	var waiting bool
	err := tx.QueryRow(ctx, `
		select exists (select 1 from runs where namespace = $1 and workflow = $2 and state in `+unfinished+`)
		    or exists (select 1 from git_packs where namespace = $1 and repository = $3 and state = 'collecting')`,
		m.Namespace, m.Workflow, m.Repository).Scan(&waiting)
	if err != nil {
		return false, fmt.Errorf("db: whether the move of %s can be carried out could not be read: %w", m.Workflow, err)
	}
	if waiting {
		return false, nil
	}
	var shared *SharedOutputs
	switch err := sharedOutputs(ctx, tx, m.Namespace, m.Workflow); {
	case errors.As(err, &shared):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// indexedLogs are the keys of the chunks the logs of the workflow's runs hold in namespace, in the
// order of the keys: those the store was written at, since a chunk is indexed once its object is
// written and not before.
func indexedLogs(ctx context.Context, tx pgx.Tx, namespace, workflow string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		select distinct c.object_key from task_log_chunks c
		join tasks t on t.namespace = c.namespace and t.id = c.task_id
		join runs r on r.namespace = t.namespace and r.id = t.run_id
		where r.namespace = $1 and r.workflow = $2 and c.object_key is not null
		order by 1`, namespace, workflow)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// movedCount is how many times the workflow's rows count one object, and what it is.
type movedCount struct {
	digest    string
	n         int
	size      int64
	mediaType string
}

// movingCounts are the counts the workflow's rows hold on the source's objects, each by as many as
// the purges will one day lower it by: one per distinct digest of each version's tree, as a
// deletion lowers them; one per envelope of the document and per input a grant counted of each run
// whose envelopes are kept, as the envelope purge lowers them; and one per live artifact, as the
// artifact purge lowers it. In digest order, which is the order they are raised and lowered in.
func (w *Wide) movingCounts(ctx context.Context, m Move) ([]movedCount, error) {
	byDigest := map[string]*movedCount{}
	add := func(digest string, size int64, mediaType string) {
		c, ok := byDigest[digest]
		if !ok {
			c = &movedCount{digest: digest, size: size, mediaType: mediaType}
			byDigest[digest] = c
		}
		c.n++
	}

	rows, err := w.tx.Query(ctx, `
		select distinct v.commit, f->>'sha256', (f->>'size')::bigint
		from workflow_versions v, jsonb_array_elements(coalesce(v.tree, '[]'::jsonb)) as f
		where v.namespace = $1 and v.workflow = $2`, m.Namespace, m.Workflow)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var commit, digest string
		var size int64
		if err := rows.Scan(&commit, &digest, &size); err != nil {
			rows.Close()
			return nil, err
		}
		add("sha256:"+digest, size, treeMediaType)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	runs, err := w.tx.Query(ctx, `
		select r.id from runs r
		where r.namespace = $1 and r.workflow = $2 and r.envelopes_purged_at is null
		  and exists (select 1 from steps s
		              where s.namespace = r.namespace and s.run_id = r.id and s.envelopes_purged_at is null)
		order by r.id`, m.Namespace, m.Workflow)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(runs, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		held, err := w.envelopesOf(ctx, m.Namespace, agk.RunID(id))
		if err != nil {
			return nil, err
		}
		handed, err := w.inputsOf(ctx, m.Namespace, agk.RunID(id))
		if err != nil {
			return nil, err
		}
		for _, e := range append(held, handed...) {
			add("sha256:"+e.Digest, e.Size, envelopeMediaType)
		}
	}

	artifacts, err := w.tx.Query(ctx, `
		select a.digest, a.size_bytes, a.media_type
		from artifacts a
		join runs r on r.namespace = a.namespace and r.id = a.run_id
		where r.namespace = $1 and r.workflow = $2 and a.status = 'live'`, m.Namespace, m.Workflow)
	if err != nil {
		return nil, err
	}
	for artifacts.Next() {
		var digest, mediaType string
		var size int64
		if err := artifacts.Scan(&digest, &size, &mediaType); err != nil {
			artifacts.Close()
			return nil, err
		}
		add(digest, size, mediaType)
	}
	artifacts.Close()
	if err := artifacts.Err(); err != nil {
		return nil, err
	}

	// What the source's row says of an object is what the target's is made of, where it has one:
	// an envelope a grant counted carries no media type of its own.
	digests := make([]string, 0, len(byDigest))
	for d := range byDigest {
		digests = append(digests, d)
	}
	held, err := w.tx.Query(ctx,
		`select digest, size_bytes, media_type from artifact_objects where namespace = $1 and digest = any($2)`,
		m.Namespace, digests)
	if err != nil {
		return nil, err
	}
	for held.Next() {
		var digest, mediaType string
		var size int64
		if err := held.Scan(&digest, &size, &mediaType); err != nil {
			held.Close()
			return nil, err
		}
		byDigest[digest].size, byDigest[digest].mediaType = size, mediaType
	}
	held.Close()
	if err := held.Err(); err != nil {
		return nil, err
	}
	// An input a grant counted names no size, and where the source keeps no row of its object
	// the target's says what it weighs, if it keeps one: a count is raised on the row it has.
	var unknown []string
	for d, c := range byDigest {
		if c.size == 0 {
			unknown = append(unknown, d)
		}
	}
	if len(unknown) > 0 {
		there, err := w.tx.Query(ctx,
			`select digest, size_bytes, media_type from artifact_objects where namespace = $1 and digest = any($2)`,
			m.Target, unknown)
		if err != nil {
			return nil, err
		}
		for there.Next() {
			var digest, mediaType string
			var size int64
			if err := there.Scan(&digest, &size, &mediaType); err != nil {
				there.Close()
				return nil, err
			}
			byDigest[digest].size, byDigest[digest].mediaType = size, mediaType
		}
		there.Close()
		if err := there.Err(); err != nil {
			return nil, err
		}
	}

	out := make([]movedCount, 0, len(byDigest))
	for _, c := range byDigest {
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b movedCount) int { return strings.Compare(a.digest, b.digest) })
	return out, nil
}

// movingLogs are the keys every log of the workflow's runs is written under, in the order of the
// keys, each once.
func movingLogs(ctx context.Context, tx pgx.Tx, m Move) ([]string, error) {
	rows, err := tx.Query(ctx, `
		select object_key from task_log_chunks c
		join tasks t on t.namespace = c.namespace and t.id = c.task_id
		join runs r on r.namespace = t.namespace and r.id = t.run_id
		where r.namespace = $1 and r.workflow = $2
		union
		select object_key from task_log_objects o
		join tasks t on t.namespace = o.namespace and t.id = o.task_id
		join runs r on r.namespace = t.namespace and r.id = t.run_id
		where r.namespace = $1 and r.workflow = $2
		order by 1`, m.Namespace, m.Workflow)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Moved is what carrying a move out settled.
type Moved struct {
	// Done is false where the move could not be carried out yet, or is no longer asked.
	Done bool

	// MustWrite are the digests of objects the target's collection had claimed, or held no row of,
	// when the move counted them there: their bytes are written again under the target's keys,
	// as a version's are (Saved.MustWriteBytes, Saved.Recorded).
	MustWrite []string
}

// CompleteMove carries a move out, in one transaction on the installation: what the workflow's rows
// count is counted under the target and let go under the source, digest by digest; the workflow's
// row changes namespace, and every key naming it, its versions, refs, runs, grants, pins, manifests,
// packs and all a run holds follows (0053, 0057); its logs' keys are rewritten under the target;
// what the move leaves under the source's keys, logs and packs, is kept for grace and then deleted;
// the expiry of its runs and artifacts is brought within the target's max_retention_days; and the
// move is recorded as workflow.update in both namespaces, as the one who asked for it.
//
// The objects are copied under the target's keys before it, those ObjectsOf read as copied, and it
// is Done false, having changed nothing, while the move is not ready or no longer asked, and where a
// chunk of its logs was indexed since copied was read, which the next pass copies.
//
// The logs of the workflow's runs and its live artifacts are held first, in that order, which is
// the order a shipment holds a log and then its task in: a shipment writing a chunk finishes before
// the move reads what it counts and rewrites, and one after it waits for the move and is then
// refused, its runner shipping it again under the target's keys; and a fetch that would spend an
// artifact's last fetch cannot retire it between the move counting it and letting it go.
func (p *Pool) CompleteMove(ctx context.Context, m Move, copied MoveObjects, grace time.Duration) (Moved, error) {
	var out Moved
	err := p.Installation(ctx, WorkflowMove, func(ctx context.Context, w *Wide) error {
		out = Moved{}
		tx := w.tx
		var asked bool
		if err := tx.QueryRow(ctx, `
			select exists (
			  select 1 from workflow_moves mv
			  join workflows wf on wf.namespace = mv.namespace and wf.name = mv.workflow
			  where mv.namespace = $1 and mv.workflow = $2 and mv.target = $3
			  for update of wf, mv)`, m.Namespace, m.Workflow, m.Target).Scan(&asked); err != nil {
			return fmt.Errorf("db: the move could not be locked: %w", err)
		}
		if !asked {
			return nil
		}
		ready, err := moveReady(ctx, tx, m)
		if err != nil || !ready {
			return err
		}
		if _, err := tx.Exec(ctx, `
			select 1 from task_logs l
			join tasks t on t.namespace = l.namespace and t.id = l.task_id
			join runs r on r.namespace = t.namespace and r.id = t.run_id
			where r.namespace = $1 and r.workflow = $2
			order by l.task_id
			for update of l`, m.Namespace, m.Workflow); err != nil {
			return fmt.Errorf("db: the logs of %s could not be held: %w", m.Workflow, err)
		}
		if _, err := tx.Exec(ctx, `
			select 1 from artifacts a
			join runs r on r.namespace = a.namespace and r.id = a.run_id
			where r.namespace = $1 and r.workflow = $2 and a.status = 'live'
			for update of a`, m.Namespace, m.Workflow); err != nil {
			return fmt.Errorf("db: the artifacts of %s could not be held: %w", m.Workflow, err)
		}

		counted, err := w.movingCounts(ctx, m)
		if err != nil {
			return err
		}
		for _, c := range counted {
			mustWrite, created, err := raise(ctx, tx, m.Target, c.digest, c.size, c.mediaType)
			if err != nil {
				return err
			}
			if c.n > 1 {
				if _, err := tx.Exec(ctx,
					`update artifact_objects set refs = refs + $3 where namespace = $1 and digest = $2`,
					m.Target, c.digest, c.n-1); err != nil {
					return fmt.Errorf("db: %s could not be counted in %s: %w", c.digest, m.Target, err)
				}
			}
			if mustWrite || created {
				out.MustWrite = append(out.MustWrite, strings.TrimPrefix(c.digest, "sha256:"))
			}
		}
		if len(counted) > 0 {
			digests, by := make([]string, len(counted)), make([]int32, len(counted))
			for i, c := range counted {
				digests[i], by[i] = c.digest, int32(c.n)
			}
			if _, err := tx.Exec(ctx, `
				update artifact_objects o
				set refs = greatest(o.refs - g.n, 0),
				    collectable_at = case when o.refs - g.n <= 0 then now() else null end
				from unnest($2::text[], $3::int[]) as g(digest, n)
				where o.namespace = $1 and o.digest = g.digest and o.refs > 0`,
				m.Namespace, digests, by); err != nil {
				return fmt.Errorf("db: the objects of %s could not be let go in %s: %w", m.Workflow, m.Namespace, err)
			}
		}

		// What stays under the source's keys until the grace has passed: the logs, which are
		// copied under the target's, and the packs, live, superseded and those a push is
		// writing, which the move refuses at its end.
		logs, err := movingLogs(ctx, tx, m)
		if err != nil {
			return err
		}
		packs, err := tx.Query(ctx,
			`select name from git_packs where namespace = $1 and repository = $2 order by name`,
			m.Namespace, m.Repository)
		if err != nil {
			return err
		}
		names, err := pgx.CollectRows(packs, pgx.RowTo[string])
		if err != nil {
			return err
		}
		left := slices.Clone(logs)
		for _, name := range names {
			prefix := m.Namespace + "/git/" + m.Repository + "/pack-" + name
			left = append(left, prefix+".pack", prefix+".idx")
		}
		if len(left) > 0 {
			if _, err := tx.Exec(ctx, `
				insert into moved_objects (namespace, key, delete_after)
				select $1, k, $3 from unnest($2::text[]) as k
				on conflict (namespace, key) do update set delete_after = excluded.delete_after`,
				m.Namespace, left, time.Now().Add(grace)); err != nil {
				return fmt.Errorf("db: what the move leaves in %s could not be recorded: %w", m.Namespace, err)
			}
		}
		if _, err := tx.Exec(ctx,
			`delete from git_packs where namespace = $1 and repository = $2 and state = 'receiving'`,
			m.Namespace, m.Repository); err != nil {
			return fmt.Errorf("db: the packs being received for %s could not be let go: %w", m.Workflow, err)
		}

		if _, err := tx.Exec(ctx,
			`delete from workflow_moves where namespace = $1 and workflow = $2`, m.Namespace, m.Workflow); err != nil {
			return fmt.Errorf("db: the move of %s could not be closed: %w", m.Workflow, err)
		}
		if _, err := tx.Exec(ctx,
			`update workflows set namespace = $3 where namespace = $1 and name = $2`,
			m.Namespace, m.Workflow, m.Target); err != nil {
			return fmt.Errorf("db: %s could not be moved to %s: %w", m.Workflow, m.Target, err)
		}
		// Every task of the workflow's runs is held now, as the rows followed the workflow's, and a
		// shipment that held one first has finished: whatever it indexed is read here.
		indexed, err := indexedLogs(ctx, tx, m.Target, m.Workflow)
		if err != nil {
			return err
		}
		for _, key := range indexed {
			if _, found := slices.BinarySearch(copied.Indexed, key); !found {
				return errIndexedSince
			}
		}

		// The logs under the target's keys, which the copy wrote.
		for _, table := range []string{"task_log_chunks", "task_log_objects"} {
			if _, err := tx.Exec(ctx, `
				update `+table+` l set object_key = $2 || substr(l.object_key, length($1) + 1)
				from tasks t, runs r
				where t.namespace = l.namespace and t.id = l.task_id
				  and r.namespace = t.namespace and r.id = t.run_id
				  and r.namespace = $2 and r.workflow = $3 and starts_with(l.object_key, $1 || '/')`,
				m.Namespace, m.Target, m.Workflow); err != nil {
				return fmt.Errorf("db: the logs of %s could not be moved: %w", m.Workflow, err)
			}
		}

		// What an earlier move of the workflow left under the target's keys is what its rows name
		// again, and is kept: a workflow moved back within the grace names the very copies the
		// first move left there, which the copy found held and did not write again.
		back := make([]string, 0, len(left))
		for _, key := range left {
			if to, ok := strings.CutPrefix(key, m.Namespace+"/"); ok {
				back = append(back, m.Target+"/"+to)
			}
		}
		if _, err := tx.Exec(ctx,
			`delete from moved_objects where namespace = $1 and key = any($2)`, m.Target, back); err != nil {
			return fmt.Errorf("db: what an earlier move left in %s could not be kept: %w", m.Target, err)
		}

		// Kept no longer than the target keeps anything: "retain is capped by the namespace quota
		// and cannot exceed it", reckoned from when the run finished and the artifact was written,
		// as each was.
		var days int
		if err := tx.QueryRow(ctx, `select max_retention_days from namespaces where name = $1`, m.Target).Scan(&days); err != nil {
			return fmt.Errorf("db: the retention of %s could not be read: %w", m.Target, err)
		}
		keep := fmt.Sprintf("%d days", days)
		if _, err := tx.Exec(ctx, `
			update runs set expires_at = least(expires_at, finished_at + $3::interval)
			where namespace = $1 and workflow = $2 and expires_at is not null and finished_at is not null
			  and expires_at > finished_at + $3::interval`, m.Target, m.Workflow, keep); err != nil {
			return fmt.Errorf("db: the runs of %s could not be held to %s's retention: %w", m.Workflow, m.Target, err)
		}
		if _, err := tx.Exec(ctx, `
			update artifacts a set expires_at = a.created_at + $3::interval
			from runs r
			where r.namespace = a.namespace and r.id = a.run_id and r.namespace = $1 and r.workflow = $2
			  and a.status = 'live' and a.expires_at > a.created_at + $3::interval`, m.Target, m.Workflow, keep); err != nil {
			return fmt.Errorf("db: the artifacts of %s could not be held to %s's retention: %w", m.Workflow, m.Target, err)
		}
		// Both namespaces hold something else now, which the next write into either counts.
		if _, err := tx.Exec(ctx,
			`update artifact_room set counted_at = null where namespace in ($1, $2)`, m.Namespace, m.Target); err != nil {
			return fmt.Errorf("db: what %s and %s hold could not be counted again: %w", m.Namespace, m.Target, err)
		}

		out.Done = true
		record := audit.Record{Actor: m.AskedBy, Action: audit.WorkflowUpdate, Target: m.Workflow, Result: audit.Done,
			Detail: map[string]any{"namespace": m.Target, "was": map[string]any{"namespace": m.Namespace}}}
		if err := w.AuditIn(ctx, m.Namespace, record); err != nil {
			return err
		}
		return w.AuditIn(ctx, m.Target, record)
	})
	if errors.Is(err, errIndexedSince) {
		return Moved{}, nil
	}
	if err != nil {
		return Moved{}, fmt.Errorf("db: the move of %s/%s to %s could not be carried out: %w", m.Namespace, m.Workflow, m.Target, err)
	}
	return out, nil
}

// errIndexedSince ends a move's transaction where a chunk of its logs was indexed after its objects
// were read, whose object the copy could not have seen.
var errIndexedSince = errors.New("db: a chunk of the workflow's logs was indexed since its objects were copied")

// MovedObject is a key a move left under the namespace it left.
type MovedObject struct {
	Namespace, Key string
}

// MovedObjectsDue reads at most batch of the keys moves left whose grace has passed, the longest
// waiting first.
func (p *Pool) MovedObjectsDue(ctx context.Context, batch int) ([]MovedObject, error) {
	var out []MovedObject
	err := p.Installation(ctx, Collect, func(ctx context.Context, w *Wide) error {
		rows, err := w.tx.Query(ctx, `
			select namespace, key from moved_objects where delete_after <= now()
			order by delete_after, namespace, key limit $1`, batch)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (MovedObject, error) {
			var o MovedObject
			err := row.Scan(&o.Namespace, &o.Key)
			return o, err
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("db: what moves left could not be read: %w", err)
	}
	return out, nil
}

// MovedObjectsGone forgets keys whose objects were deleted.
func (p *Pool) MovedObjectsGone(ctx context.Context, gone []MovedObject) error {
	if len(gone) == 0 {
		return nil
	}
	namespaces, keys := make([]string, len(gone)), make([]string, len(gone))
	for i, o := range gone {
		namespaces[i], keys[i] = o.Namespace, o.Key
	}
	err := p.Installation(ctx, Collect, func(ctx context.Context, w *Wide) error {
		_, err := w.tx.Exec(ctx, `
			delete from moved_objects o using unnest($1::text[], $2::text[]) as g(namespace, key)
			where o.namespace = g.namespace and o.key = g.key`, namespaces, keys)
		return err
	})
	if err != nil {
		return fmt.Errorf("db: what moves left could not be forgotten: %w", err)
	}
	return nil
}
