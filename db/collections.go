package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Collections: "a connector of a principal's own: the workflows they choose, each one a tool, served
// at /mcp/collections/{id}". What the store keeps of one is its owner, its name, its description and
// its members, each a workflow by namespace and name with the ref it is read at and the name its tool
// goes by where its owner wrote them. What each member offers is not kept: it is read from the entry
// point at the head of the member's ref whenever it is asked, so that a collection follows a workflow
// as its triggers follow the default branch.
//
// Read and written through the installation's handle alone, for the principal it is: every method
// names the owner, and a collection of another principal's is answered as one that does not exist.

// CollectionsPerPrincipal and MembersPerCollection are the bounds the documentation sets: "a
// collection holds at most 100 members, and a principal at most 50 collections", since "a client
// reads the whole tool list into a model's context at every list. A hundred tools is past what a
// model chooses among well, and somebody needing more needs a second collection more than a longer
// one."
const (
	CollectionsPerPrincipal = 50
	MembersPerCollection    = 100
)

// ErrNoCollection is no collection of that identifier among the principal's: another principal's,
// one removed, or one never made, which are one answer since asking must teach nothing of another's.
var ErrNoCollection = errors.New("db: no collection of that identifier")

// ErrCollectionNameHeld is a name another of the principal's collections holds.
var ErrCollectionNameHeld = errors.New("db: another collection of the principal's holds that name")

// ErrTooManyCollections is a principal holding as many collections as it may.
var ErrTooManyCollections = fmt.Errorf("db: the principal holds %d collections, the most a principal holds", CollectionsPerPrincipal)

// ErrTooManyMembers is a collection holding as many members as it may.
var ErrTooManyMembers = fmt.Errorf("db: the collection holds %d members, the most a collection holds", MembersPerCollection)

// Collection is one collection and its members, in the order they were added.
type Collection struct {
	ID          string
	Principal   string
	Name        string
	Description string
	CreatedAt   time.Time
	Members     []CollectionMember
}

// CollectionMember is one workflow of a collection: Ref empty for its default branch, and As empty
// for the name its mcp block gives.
type CollectionMember struct {
	Namespace string
	Workflow  string
	Ref       string
	As        string
}

// Collections answers the principal's collections by name, each with its members.
func (w *Wide) Collections(ctx context.Context, principal string) ([]Collection, error) {
	rows, err := w.tx.Query(ctx,
		`select id, principal, name, description, created_at from collections where principal = $1 order by name`, principal)
	if err != nil {
		return nil, fmt.Errorf("db: the collections of %s could not be read: %w", principal, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Collection, error) {
		var c Collection
		err := row.Scan(&c.ID, &c.Principal, &c.Name, &c.Description, &c.CreatedAt)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("db: the collections of %s could not be read: %w", principal, err)
	}
	for i := range out {
		if out[i].Members, err = w.members(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Collection answers one of the principal's collections, or ErrNoCollection.
func (w *Wide) Collection(ctx context.Context, principal, id string) (Collection, error) {
	return w.collection(ctx, principal, id, "")
}

// collection reads one collection, under the lock lock names where it names one, so that a write
// reading it first holds it until the write is over.
func (w *Wide) collection(ctx context.Context, principal, id, lock string) (Collection, error) {
	var c Collection
	err := w.tx.QueryRow(ctx,
		`select id, principal, name, description, created_at from collections where id = $1 and principal = $2 `+lock,
		id, principal).Scan(&c.ID, &c.Principal, &c.Name, &c.Description, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Collection{}, ErrNoCollection
	}
	if err != nil {
		return Collection{}, fmt.Errorf("db: collection %s could not be read: %w", id, err)
	}
	if c.Members, err = w.members(ctx, id); err != nil {
		return Collection{}, err
	}
	return c, nil
}

// members reads a collection's members in the order they were added.
func (w *Wide) members(ctx context.Context, id string) ([]CollectionMember, error) {
	rows, err := w.tx.Query(ctx,
		`select namespace, workflow, coalesce(ref, ''), coalesce(as_name, '')
		 from collection_members where collection = $1 order by position`, id)
	if err != nil {
		return nil, fmt.Errorf("db: the members of collection %s could not be read: %w", id, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (CollectionMember, error) {
		var m CollectionMember
		err := row.Scan(&m.Namespace, &m.Workflow, &m.Ref, &m.As)
		return m, err
	})
	if err != nil {
		return nil, fmt.Errorf("db: the members of collection %s could not be read: %w", id, err)
	}
	return out, nil
}

// CreateCollection makes a collection of the principal's, holding no member, under the identifier
// given, which the caller mints. It refuses a name another of the principal's collections holds
// with ErrCollectionNameHeld, and a principal holding CollectionsPerPrincipal already with
// ErrTooManyCollections.
//
// The principal's row is held for the length of the transaction, so that two makings at once are
// counted one after the other rather than each finding room for itself.
func (w *Wide) CreateCollection(ctx context.Context, principal, kind, id, name, description string) (Collection, error) {
	if _, err := w.tx.Exec(ctx, `select 1 from principals where id = $1 and kind = $2 for update`, principal, kind); err != nil {
		return Collection{}, fmt.Errorf("db: %s could not be held: %w", principal, err)
	}
	var held int
	if err := w.tx.QueryRow(ctx, `select count(*) from collections where principal = $1`, principal).Scan(&held); err != nil {
		return Collection{}, fmt.Errorf("db: the collections of %s could not be counted: %w", principal, err)
	}
	if held >= CollectionsPerPrincipal {
		return Collection{}, ErrTooManyCollections
	}
	var c Collection
	err := w.tx.QueryRow(ctx,
		`insert into collections (id, principal, principal_kind, name, description) values ($1, $2, $3, $4, $5)
		 returning id, principal, name, description, created_at`,
		id, principal, kind, name, description).Scan(&c.ID, &c.Principal, &c.Name, &c.Description, &c.CreatedAt)
	if err := collectionNamed(err); err != nil {
		return Collection{}, fmt.Errorf("db: the collection could not be made: %w", err)
	}
	c.Members = []CollectionMember{}
	return c, nil
}

// collectionNamed reads a write refused for the name its owner tells collections apart by.
func collectionNamed(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.ConstraintName == "collections_principal_name_key" {
		return ErrCollectionNameHeld
	}
	return err
}

// UpdateCollection sets the name, the description or both of one of the principal's collections,
// each nil left as it is, and answers it as it now stands.
func (w *Wide) UpdateCollection(ctx context.Context, principal, id string, name, description *string) (Collection, error) {
	tag, err := w.tx.Exec(ctx,
		`update collections set name = coalesce($3, name), description = coalesce($4, description)
		 where id = $1 and principal = $2`, id, principal, name, description)
	if err := collectionNamed(err); err != nil {
		return Collection{}, fmt.Errorf("db: collection %s could not be changed: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return Collection{}, ErrNoCollection
	}
	return w.Collection(ctx, principal, id)
}

// DeleteCollection removes one of the principal's collections with its members.
func (w *Wide) DeleteCollection(ctx context.Context, principal, id string) error {
	tag, err := w.tx.Exec(ctx, `delete from collections where id = $1 and principal = $2`, id, principal)
	if err != nil {
		return fmt.Errorf("db: collection %s could not be removed: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoCollection
	}
	return nil
}

// HoldCollection reads one of the principal's collections and holds it until the transaction ends,
// for a write that judges a member against the others first: two members written at once are judged
// one after the other, so that neither takes a tool name the other was given.
func (w *Wide) HoldCollection(ctx context.Context, principal, id string) (Collection, error) {
	return w.collection(ctx, principal, id, "for update")
}

// WriteCollectionMember writes a member of one of the principal's collections whole: a workflow
// already a member keeps its place and takes the ref and the name written, and one added goes last,
// refused with ErrTooManyMembers past MembersPerCollection. It answers the collection as it now
// stands.
func (w *Wide) WriteCollectionMember(ctx context.Context, principal, id string, m CollectionMember) (Collection, error) {
	c, err := w.HoldCollection(ctx, principal, id)
	if err != nil {
		return Collection{}, err
	}
	tag, err := w.tx.Exec(ctx,
		`update collection_members set ref = $4, as_name = $5 where collection = $1 and namespace = $2 and workflow = $3`,
		id, m.Namespace, m.Workflow, nilIfEmpty(m.Ref), nilIfEmpty(m.As))
	if err != nil {
		return Collection{}, fmt.Errorf("db: a member of collection %s could not be written: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		if len(c.Members) >= MembersPerCollection {
			return Collection{}, ErrTooManyMembers
		}
		if _, err := w.tx.Exec(ctx,
			`insert into collection_members (collection, namespace, workflow, ref, as_name, position)
			 values ($1::text, $2, $3, $4, $5, (select coalesce(max(position), 0) + 1 from collection_members where collection = $1::text))`,
			id, m.Namespace, m.Workflow, nilIfEmpty(m.Ref), nilIfEmpty(m.As)); err != nil {
			return Collection{}, fmt.Errorf("db: a member of collection %s could not be written: %w", id, err)
		}
	}
	return w.Collection(ctx, principal, id)
}

// RemoveCollectionMember takes a workflow out of one of the principal's collections, and answers
// the collection as it now stands. Taking out a workflow that is not a member changes nothing.
func (w *Wide) RemoveCollectionMember(ctx context.Context, principal, id, namespace, workflow string) (Collection, error) {
	if _, err := w.HoldCollection(ctx, principal, id); err != nil {
		return Collection{}, err
	}
	if _, err := w.tx.Exec(ctx,
		`delete from collection_members where collection = $1 and namespace = $2 and workflow = $3`,
		id, namespace, workflow); err != nil {
		return Collection{}, fmt.Errorf("db: a member of collection %s could not be taken out: %w", id, err)
	}
	return w.Collection(ctx, principal, id)
}
