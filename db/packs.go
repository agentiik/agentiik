package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// A repository's packs over their life: receiving while a push writes one, live once the push has
// moved its refs, superseded once a repack has written its objects into another, and collecting
// once the collection has claimed it, a receiving one a push never made live or a superseded one,
// each past the grace, after which its files are deleted from the store and then its row.
//
// The grace is what keeps a clone in progress from being cut short: a fetch reads a pack a range at
// a time, from the list of live packs it read as it began, so a pack a repack superseded is still
// read by a fetch that began before, and is deleted only once no fetch that began before it was
// superseded can still be running. A receiving pack is one a push is writing, or one a push refused
// after writing it, and is collected once no push could still be writing it.

// RepackAbove is how many live packs a repository holds before the repack writes their objects into
// one: sixteen, since a fetch looks an object up in every live pack's index, newest first, and
// every push that sends objects makes a pack of its own. A repository pushed to a few times a day
// is repacked every week or two, and one that nobody pushes to never.
const RepackAbove = 16

// ErrPackCollected is a pack a push sends again, by the checksum that names it, while the
// collection deletes its files: the push is refused, and sent again once the files are gone.
var ErrPackCollected = errors.New("db: that pack is being collected")

// RepositoryPack is one pack of one repository, named for the store: the namespace, the repository's
// key its files are kept under, and the pack.
type RepositoryPack struct {
	Namespace  string
	Repository string
	Pack
}

// CollectablePacks claims up to batch packs past the grace, receiving ones a push never made live
// and superseded ones a repack replaced, and answers them for the store to delete their files. A
// pack a pass claimed and did not finish with is claimed again, whatever its age, since its files
// may be half deleted. grace is DefaultGrace where zero, and refused where negative, for the reason
// Collectable refuses one.
func (p *Pool) CollectablePacks(ctx context.Context, grace time.Duration, batch int) ([]RepositoryPack, error) {
	if grace < 0 {
		return nil, fmt.Errorf("db: a collection grace of %s would collect a pack a fetch may still read", grace)
	}
	if grace == 0 {
		grace = DefaultGrace
	}
	batch, err := batchOf(batch)
	if err != nil {
		return nil, err
	}
	var out []RepositoryPack
	err = p.Installation(ctx, Collect, func(ctx context.Context, w *Wide) error {
		rows, err := w.tx.Query(ctx, `
			update git_packs set state = 'collecting'
			where (namespace, repository, name) in (
			  select namespace, repository, name from git_packs
			  where state = 'collecting'
			     or (state = 'receiving' and created_at <= now() - ($1::bigint * interval '1 second'))
			     or (state = 'superseded' and superseded_at <= now() - ($1::bigint * interval '1 second'))
			  order by namespace, repository, name
			  limit $2
			  for update skip locked
			)
			returning namespace, repository, name, size, objects, created_at`, int64(grace/time.Second), batch)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (RepositoryPack, error) {
			var r RepositoryPack
			err := row.Scan(&r.Namespace, &r.Repository, &r.Name, &r.Size, &r.Objects, &r.CreatedAt)
			return r, err
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("db: the collectable packs could not be claimed: %w", err)
	}
	return out, nil
}

// PacksCollected forgets the packs CollectablePacks claimed whose files the store has deleted, and
// answers how many it forgot.
func (p *Pool) PacksCollected(ctx context.Context, packs []RepositoryPack) (int, error) {
	if len(packs) == 0 {
		return 0, nil
	}
	namespaces := make([]string, len(packs))
	repositories := make([]string, len(packs))
	names := make([]string, len(packs))
	for i, r := range packs {
		namespaces[i], repositories[i], names[i] = r.Namespace, r.Repository, r.Name
	}
	var forgotten int
	err := p.Installation(ctx, Collect, func(ctx context.Context, w *Wide) error {
		tag, err := w.tx.Exec(ctx, `
			delete from git_packs
			where (namespace, repository, name) in (select * from unnest($1::text[], $2::text[], $3::text[]))
			  and state = 'collecting'`, namespaces, repositories, names)
		forgotten = int(tag.RowsAffected())
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("db: the collected packs could not be forgotten: %w", err)
	}
	return forgotten, nil
}

// Repackable answers up to batch workflows whose repositories hold more than most live packs, the
// ones holding most first.
func (p *Pool) Repackable(ctx context.Context, most, batch int) ([]Target, error) {
	batch, err := batchOf(batch)
	if err != nil {
		return nil, err
	}
	var out []Target
	err = p.Installation(ctx, Collect, func(ctx context.Context, w *Wide) error {
		rows, err := w.tx.Query(ctx, `
			select w.namespace, w.name from git_packs g
			join workflows w on w.namespace = g.namespace and w.repository = g.repository
			where g.state = 'live'
			group by w.namespace, w.name
			having count(*) > $1
			order by count(*) desc, w.namespace, w.name
			limit $2`, most, batch)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Target, error) {
			var t Target
			err := row.Scan(&t.Namespace, &t.Workflow)
			return t, err
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("db: the repositories to repack could not be read: %w", err)
	}
	return out, nil
}

// Target is one workflow, by its namespace and its name.
type Target struct {
	Namespace string
	Workflow  string
}

// Repacked makes the pack a repack wrote live in place of the packs it replaced, under the lock
// every push takes to move its refs, and marks those superseded from now: a fetch that began before
// reads them for the grace, and one that begins after reads the new pack. A pack it replaced that a
// push has made live since, or that is no longer live, is left as it is. The new pack no longer
// receiving, which the collection took past the grace, is ErrNoPack.
func (n *NS) Repacked(ctx context.Context, workflow, name string, replaced []string) error {
	var key string
	err := n.tx.QueryRow(ctx,
		`select repository from workflows where namespace = $1 and name = $2 for no key update`,
		n.namespace, workflow).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s/%s", ErrNoWorkflow, n.namespace, workflow)
	}
	if err != nil {
		return fmt.Errorf("db: the repository of %s could not be locked: %w", workflow, err)
	}
	tag, err := n.tx.Exec(ctx,
		`update git_packs set state = 'live'
		 where namespace = $1 and repository = $2 and name = $3 and state in ('receiving', 'live')`,
		n.namespace, key, name)
	if err != nil {
		return fmt.Errorf("db: pack %s of %s could not be made live: %w", name, workflow, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s of %s/%s", ErrNoPack, name, n.namespace, workflow)
	}
	if _, err := n.tx.Exec(ctx,
		`update git_packs set state = 'superseded', superseded_at = now()
		 where namespace = $1 and repository = $2 and name = any($3) and name <> $4 and state = 'live'`,
		n.namespace, key, replaced, name); err != nil {
		return fmt.Errorf("db: the packs pack %s replaces in %s could not be superseded: %w", name, workflow, err)
	}
	return nil
}
