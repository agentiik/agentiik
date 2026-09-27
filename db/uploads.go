package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/internal/ulid"
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
// the namespace's row of artifact_room that holds the next write until this one has made its room.
// The row is at the most the object may be until its bytes are in, then at their size, and lapses
// uploadGrace after the policy or the URL it was written with, which expires with the task.
//
// Envelopes are written through the same form and cannot be told apart from an artifact there, so
// they are held to the quota as they are written and counted while their upload is, and not once
// it lapses: an envelope is what a step published, not an artifact it produced.

// uploadGrace is how long room made for a write outlives the policy it was written with: time for
// a result sent at the task's deadline to be heard and its references written, across a failover
// of the controller, so that an object is not left counted by nothing in between.
const uploadGrace = 15 * time.Minute

// recountAfter is how old the count in artifact_room may be before a write counts again, and
// recountBeforeRefusing how old it may be before a write is refused on it rather than counted
// again: between two counts a write adds its room to what was counted and a settlement takes off
// what it gives back, and what expired or lapsed meanwhile is only found by the next.
const (
	recountAfter          = time.Minute
	recountBeforeRefusing = time.Second
)

// heldBytes is the bytes a namespace holds against max_artifact_bytes, as a query over the
// namespaces the where clauses select: one namespace with $1, or all of them. A digest counts once,
// at the most any reference or upload of it says.
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

// Upload is one write room is made for.
type Upload struct {
	// Digest is the object's, sixty-four lowercase hexadecimal characters, as a key writes it.
	Digest string

	// Length is the most the object may be, and negative where the request carrying it states
	// no length, which is then given the room left.
	Length int64

	// Most is artifact_max_bytes, which bounds the room an object of no stated length is
	// given, and zero bounds nothing.
	Most int64

	// Until is when the policy or the URL it is written with expires.
	Until time.Time
}

// Room is what MakeRoom held for one write, for Stored or Unwritten to settle.
type Room struct {
	namespace, id, digest string
	bound                 int64

	// held is false where nothing was: the namespace sets no quota, or holds the object as a
	// live artifact.
	held bool
}

// Held says whether room was held, which a caller settles, or the object needed none.
func (r Room) Held() bool { return r.held }

// Bound is the most the object may be where room was held for it, which a caller holds its bytes
// to as they arrive.
func (r Room) Bound() int64 { return r.bound }

// MakeRoom holds room for one write, or refuses it with a *NoRoom.
//
// Nothing is held, and nothing is locked, where the namespace sets no max_artifact_bytes, as before
// v0.3.0, or already holds the object as a live artifact: the bytes are then bytes it counts, and a
// replay writing what it wrote before is refused nothing. An object another write is making room
// for is held again, to what this write may take beyond the other's room, so that neither write's
// failure leaves the other's bytes counted by nothing.
//
// The namespace's row of artifact_room is locked first, so that two writes in the namespace at once
// make their room one after the other, the second counting what the first made once the first has
// committed.
func (n *NS) MakeRoom(ctx context.Context, u Upload) (Room, error) {
	if !hexDigest.MatchString(u.Digest) {
		return Room{}, fmt.Errorf("db: %q is not a digest", u.Digest)
	}
	var limit int64
	err := n.tx.QueryRow(ctx,
		`select max_artifact_bytes from namespaces where name = $1 and max_artifact_bytes is not null`,
		n.namespace).Scan(&limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return Room{}, nil
	}
	if err != nil {
		return Room{}, fmt.Errorf("db: the max_artifact_bytes of namespace %s could not be read: %w", n.namespace, err)
	}

	var held int64
	var counted *time.Time
	var now time.Time
	room := func() error {
		return n.tx.QueryRow(ctx,
			`select held, counted_at, now() from artifact_room where namespace = $1 for update`,
			n.namespace).Scan(&held, &counted, &now)
	}
	err = room()
	if errors.Is(err, pgx.ErrNoRows) {
		// Made to exist and then locked, since a lock on a row that is not there is no lock at
		// all: two first writes at once both find none, and the insert that does nothing on a
		// conflict waits for the other's, whose row the lock then finds. Counted at the first
		// write, since it holds nothing yet.
		if _, err := n.tx.Exec(ctx,
			`insert into artifact_room (namespace, held) values ($1, 0) on conflict (namespace) do nothing`,
			n.namespace); err != nil {
			return Room{}, fmt.Errorf("db: the room of namespace %s could not be made: %w", n.namespace, err)
		}
		err = room()
	}
	if err != nil {
		return Room{}, fmt.Errorf("db: the room of namespace %s could not be read: %w", n.namespace, err)
	}

	stored := "sha256:" + u.Digest
	var live bool
	var other int64
	if err := n.tx.QueryRow(ctx,
		`select exists (select 1 from artifacts
		                where namespace = $1 and digest = $2 and status = 'live' and expires_at > now()),
		        coalesce((select max(bytes) from artifact_uploads
		                  where namespace = $1 and digest = $2 and until > now()), 0)`,
		n.namespace, stored).Scan(&live, &other); err != nil {
		return Room{}, fmt.Errorf("db: whether namespace %s holds %s could not be read: %w", n.namespace, stored, err)
	}
	if live {
		return Room{}, nil
	}

	// What this write may take, and what that adds to what the namespace holds: an object of no
	// stated length is given the room left, and another write of the same object already holds
	// its own room.
	fit := func() (bound, more int64) {
		bound = u.Length
		if bound < 0 {
			bound = limit - held + other
			if u.Most > 0 && bound > u.Most {
				bound = u.Most
			}
		}
		return bound, max(bound-other, 0)
	}
	bound, more := fit()
	stale := counted == nil || !counted.After(now.Add(-recountAfter)) ||
		(held+more > limit && !counted.After(now.Add(-recountBeforeRefusing)))
	if stale {
		if held, err = n.recount(ctx); err != nil {
			return Room{}, err
		}
		bound, more = fit()
	}
	if (u.Length < 0 && bound <= 0) || held+more > limit {
		return Room{}, &NoRoom{Namespace: n.namespace, Limit: limit, Held: held, Asked: u.Length}
	}

	if _, err := n.tx.Exec(ctx,
		`update artifact_room set held = $2 where namespace = $1`, n.namespace, held+more); err != nil {
		return Room{}, fmt.Errorf("db: the room of namespace %s could not be held: %w", n.namespace, err)
	}
	id := ulid.New()
	until := u.Until.UTC().Add(uploadGrace)
	if _, err := n.tx.Exec(ctx,
		`insert into artifact_uploads (namespace, id, digest, bytes, until) values ($1, $2, $3, $4, $5)`,
		n.namespace, id, stored, bound, until); err != nil {
		return Room{}, fmt.Errorf("db: room for %s could not be held: %w", stored, err)
	}
	return Room{namespace: n.namespace, id: id, digest: stored, bound: bound, held: true}, nil
}

// recount counts what the namespace holds, whole, and writes it down as held now: the uploads that
// lapsed go first, since they count nothing any more.
func (n *NS) recount(ctx context.Context) (int64, error) {
	if _, err := n.tx.Exec(ctx,
		`delete from artifact_uploads where namespace = $1 and until <= now()`, n.namespace); err != nil {
		return 0, fmt.Errorf("db: the lapsed uploads of namespace %s could not be let go: %w", n.namespace, err)
	}
	var held int64
	if err := n.tx.QueryRow(ctx,
		`select coalesce(sum(bytes), 0)::bigint from (`+heldBytes(` and namespace = $1`, ` and namespace = $1`)+`) counted`,
		n.namespace).Scan(&held); err != nil {
		return 0, fmt.Errorf("db: what namespace %s holds against its max_artifact_bytes could not be counted: %w", n.namespace, err)
	}
	if _, err := n.tx.Exec(ctx,
		`update artifact_room set held = $2, counted_at = now() where namespace = $1`, n.namespace, held); err != nil {
		return 0, fmt.Errorf("db: what namespace %s holds could not be written down: %w", n.namespace, err)
	}
	return held, nil
}

// Stored settles room held for an object whose bytes are in: it counts their size, which the bound
// was the most of, until it lapses.
func (n *NS) Stored(ctx context.Context, r Room, size int64) error {
	return n.settle(ctx, r, `update artifact_uploads set bytes = $3 where namespace = $1 and id = $2`, size)
}

// Unwritten gives back room held for an object whose bytes never arrived.
func (n *NS) Unwritten(ctx context.Context, r Room) error {
	return n.settle(ctx, r, `delete from artifact_uploads where namespace = $1 and id = $2`)
}

// settle changes the row of one write, and what the namespace holds by what that changes of its
// object: the most any live reference or upload of the object says, which is what a count takes
// of it. Under the lock MakeRoom takes, and in the same order, so that room given back is there for
// the next write rather than at the next count.
func (n *NS) settle(ctx context.Context, r Room, change string, args ...any) error {
	if !r.held {
		return nil
	}
	if r.namespace != n.namespace {
		return fmt.Errorf("db: room held in namespace %s settled in %s", r.namespace, n.namespace)
	}
	var held int64
	err := n.tx.QueryRow(ctx,
		`select held from artifact_room where namespace = $1 for update`, n.namespace).Scan(&held)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("db: the room of namespace %s could not be read: %w", n.namespace, err)
	}
	before, err := n.takes(ctx, r.digest)
	if err != nil {
		return err
	}
	if _, err := n.tx.Exec(ctx, change, append([]any{r.namespace, r.id}, args...)...); err != nil {
		return fmt.Errorf("db: the upload %s could not be settled: %w", r.id, err)
	}
	after, err := n.takes(ctx, r.digest)
	if err != nil {
		return err
	}
	if _, err := n.tx.Exec(ctx,
		`update artifact_room set held = greatest(held + $2, 0) where namespace = $1`,
		n.namespace, after-before); err != nil {
		return fmt.Errorf("db: the room of namespace %s could not be settled: %w", n.namespace, err)
	}
	return nil
}

// takes is what one object counts for in what its namespace holds.
func (n *NS) takes(ctx context.Context, stored string) (int64, error) {
	var bytes int64
	if err := n.tx.QueryRow(ctx,
		`select greatest(
		   coalesce((select max(size_bytes) from artifacts
		             where namespace = $1 and digest = $2 and status = 'live' and expires_at > now()), 0),
		   coalesce((select max(bytes) from artifact_uploads
		             where namespace = $1 and digest = $2 and until > now()), 0))`,
		n.namespace, stored).Scan(&bytes); err != nil {
		return 0, fmt.Errorf("db: what %s counts for in namespace %s could not be read: %w", stored, n.namespace, err)
	}
	return bytes, nil
}
