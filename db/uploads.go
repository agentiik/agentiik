package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// max_artifact_bytes: "Total live artifact storage; beyond it, new writes are refused."
//
// What a namespace holds is the bytes of its live artifacts, each digest once: "identical bytes
// are stored once", and deduplication is scoped per namespace, so a namespace pays for its own
// copy once however many of its references name it, and never for another namespace's. Live is
// what Resolve calls live, a reference neither retired nor past its expiry, so the room an expired
// artifact took is free again at its expiry and not at whichever sweep retires it.
//
// A write cannot be counted by its reference alone, since a runner posts an object before the
// controller has heard of the result that references it: two uploads that each fit alone would
// both be let through, and so would every upload of one task. So room is made for an object before
// its bytes are read, as a row of artifact_uploads counted beside the references, under a lock on
// the namespace's row that holds the next write until the count is done. The row is at the length
// of the request carrying the bytes until they are stored, then at their size, and lapses with the
// policy or the URL they were written with, which expires with the task: by then the result that
// references them has been sent, or never will be.
//
// Envelopes are written through the same form and cannot be told apart from an artifact there, so
// they are held to the quota as they are written and counted while their upload is, and not once
// it lapses: an envelope is what a step published, not an artifact it produced.

// heldBytes is the bytes a namespace holds against max_artifact_bytes, as a query over the
// namespaces the where clauses select: one namespace with $1, or all of them.
func heldBytes(artifacts, uploads string) string {
	return `select namespace, digest, max(bytes) as bytes from (
		  select namespace, digest, size_bytes as bytes from artifacts
		   where status = 'live' and expires_at > now()` + artifacts + `
		  union all
		  select namespace, digest, bytes from artifact_uploads
		   where until > now()` + uploads + `
		) held group by namespace, digest`
}

// NoRoom is an object refused because its namespace holds too much of its max_artifact_bytes for
// it to fit.
type NoRoom struct {
	Namespace string
	Limit     int64

	// Held is what the namespace holds, its live artifacts and its uploads, and Asked the most
	// the object refused may be, which is negative for one of no stated length.
	Held, Asked int64
}

func (r *NoRoom) Error() string {
	object := fmt.Sprintf("an object of up to %d bytes", r.Asked)
	if r.Asked < 0 {
		object = "an object of no stated length"
	}
	return fmt.Sprintf("db: namespace %s holds %d bytes of live artifacts and uploads against its max_artifact_bytes, %d, and has no room for %s", r.Namespace, r.Held, r.Limit, object)
}

// Room is what MakeRoom held for one object, for Stored or Unwritten to settle.
type Room struct {
	namespace, digest string
	until             time.Time
	bound             int64

	// held is false where nothing was: the namespace sets no quota, or already holds the
	// digest.
	held bool
}

// Held says whether room was held, which a caller settles, or the object needed none.
func (r Room) Held() bool { return r.held }

// Bound is the most the object may be where room was held for it, which a caller holds its bytes
// to as they arrive.
func (r Room) Bound() int64 { return r.bound }

// MakeRoom holds room for one object of up to bound bytes, until until, or refuses it with a
// *NoRoom. A negative bound is an object of no stated length, which is given whatever room is left.
//
// Nothing is held, and nothing is locked, where the namespace sets no max_artifact_bytes, as before
// v0.3.0, or already holds the digest: the object is then bytes the namespace already counts, and a
// replay writing what it wrote before is refused nothing. Otherwise the namespace's row is locked
// before anything is counted, so that two writes in the namespace at once count one after the
// other, the second counting the room the first made once the first has committed.
//
// digest is sixty-four lowercase hexadecimal characters, as a key writes it.
func (n *NS) MakeRoom(ctx context.Context, digest string, bound int64, until time.Time) (Room, error) {
	if !hexDigest.MatchString(digest) {
		return Room{}, fmt.Errorf("db: %q is not a digest", digest)
	}
	// To the microsecond the column holds, since the room is settled by this instant.
	until = until.UTC().Truncate(time.Microsecond)
	var limit int64
	err := n.tx.QueryRow(ctx,
		`select max_artifact_bytes from namespaces
		 where name = $1 and max_artifact_bytes is not null
		 for no key update`, n.namespace).Scan(&limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return Room{}, nil
	}
	if err != nil {
		return Room{}, fmt.Errorf("db: the max_artifact_bytes of namespace %s could not be read: %w", n.namespace, err)
	}

	// The uploads that lapsed go first, since they count nothing any more and one of them may
	// hold the key this one is about to take.
	if _, err := n.tx.Exec(ctx,
		`delete from artifact_uploads where namespace = $1 and until <= now()`, n.namespace); err != nil {
		return Room{}, fmt.Errorf("db: the lapsed uploads of namespace %s could not be let go: %w", n.namespace, err)
	}
	stored := "sha256:" + digest
	var held int64
	var counted bool
	if err := n.tx.QueryRow(ctx,
		`select coalesce(sum(bytes), 0)::bigint, coalesce(bool_or(digest = $2), false)
		 from (`+heldBytes(` and namespace = $1`, ` and namespace = $1`)+`) counted`,
		n.namespace, stored).Scan(&held, &counted); err != nil {
		return Room{}, fmt.Errorf("db: what namespace %s holds against its max_artifact_bytes could not be counted: %w", n.namespace, err)
	}
	if counted {
		return Room{}, nil
	}
	asked := bound
	if bound < 0 {
		bound = limit - held
	}
	if bound < 0 || (asked < 0 && bound == 0) || held+bound > limit {
		return Room{}, &NoRoom{Namespace: n.namespace, Limit: limit, Held: held, Asked: asked}
	}
	if _, err := n.tx.Exec(ctx,
		`insert into artifact_uploads (namespace, digest, bytes, until) values ($1, $2, $3, $4)`,
		n.namespace, stored, bound, until); err != nil {
		return Room{}, fmt.Errorf("db: room for %s could not be held: %w", stored, err)
	}
	return Room{namespace: n.namespace, digest: stored, until: until, bound: bound, held: true}, nil
}

// Stored settles room held for an object whose bytes are in: it counts their size, which the bound
// was the most of, until it lapses.
func (n *NS) Stored(ctx context.Context, r Room, size int64) error {
	if !r.held {
		return nil
	}
	if r.namespace != n.namespace {
		return fmt.Errorf("db: room held in namespace %s settled in %s", r.namespace, n.namespace)
	}
	if _, err := n.tx.Exec(ctx,
		`update artifact_uploads set bytes = $4 where namespace = $1 and digest = $2 and until = $3`,
		r.namespace, r.digest, r.until, size); err != nil {
		return fmt.Errorf("db: the upload of %s could not be counted at its size: %w", r.digest, err)
	}
	return nil
}

// Unwritten gives back room held for an object whose bytes never arrived.
func (n *NS) Unwritten(ctx context.Context, r Room) error {
	if !r.held {
		return nil
	}
	if r.namespace != n.namespace {
		return fmt.Errorf("db: room held in namespace %s given back in %s", r.namespace, n.namespace)
	}
	if _, err := n.tx.Exec(ctx,
		`delete from artifact_uploads where namespace = $1 and digest = $2 and until = $3`,
		r.namespace, r.digest, r.until); err != nil {
		return fmt.Errorf("db: the room held for %s could not be given back: %w", r.digest, err)
	}
	return nil
}
