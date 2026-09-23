package db

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/jackc/pgx/v5"
)

// The reference half of the artifact store.
//
// Package artifact holds the bytes and knows nothing of who points at them. This is the
// other half: the row binding a run, a step and a port to a digest, which is what expiry
// acts on. "Expiry applies to the reference, never to the object. The row in artifacts
// binding a run, a step and a port to a digest is what is dropped; the physical
// sha256/<digest> object is collected once its reference count reaches zero, and not
// before."

// Status is what has become of a reference.
//
// A retired reference is kept rather than deleted, because a later request has to be able
// to tell this existed and is finished from this never existed, and because "the run detail
// keeps showing the artifact's name, size and digest with its collection recorded".
type Status string

const (
	// Live is a reference that can still be fetched.
	Live Status = "live"

	// Expired is a reference whose duration ran out.
	Expired Status = "expired"

	// Collected is a reference whose fetch budget was spent. The word is the
	// documentation's: "A one-shot artifact collected by its owner".
	Collected Status = "collected"
)

// ErrNoArtifact is nothing of that name was ever written here. An API answers 404.
var ErrNoArtifact = errors.New("db: no artifact of that name was written on that port")

// ErrGone is it existed and is finished. An API answers 410 and not 404, "so that a client
// can tell this existed and is finished from this never existed". Resolve returns what it
// found alongside this error, because the answer still carries the name, the size and the
// digest.
var ErrGone = errors.New("db: that artifact has expired or been collected")

// trimAlgorithm turns the stored sha256:<hex> back into the bare hex an envelope carries.
func trimAlgorithm(stored string) string { return strings.TrimPrefix(stored, "sha256:") }

// hexDigest is the digest as an envelope writes it: "sixty-four lowercase hexadecimal
// characters", with no algorithm in front. The column holds the algorithm too, and the two
// forms are converted at this boundary and nowhere else.
var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Reference is one artifact, as it is written.
//
// For and Fetches are the two mechanisms of retain, which "answer two different questions:
// a duration answers how long may this be fetched, and a single fetch answers may this be
// fetched more than once". For is required, one-shot or not: "a one-shot artifact still has
// a duration, which is what expires it when nobody ever comes for it". Fetches is zero
// where the workflow declared no budget.
type Reference struct {
	URI       agk.URI
	Digest    string // sixty-four lowercase hexadecimal characters, as the envelope carries it
	Size      int64
	MediaType string
	For       time.Duration
	Fetches   int
}

// Written is what recording a reference settled.
type Written struct {
	// Key is the physical key the bytes are under, <namespace>/sha256/<digest>.
	Key string

	// ExpiresAt is when the reference stops being fetchable, capped by the namespace:
	// "retain is capped by the namespace quota and cannot exceed it".
	ExpiresAt time.Time

	// Fetches is what is left of the budget, zero where there is none.
	Fetches int

	// Status is what the reference is, which is Live for anything just written and
	// whatever it had become for a reference that was already there.
	Status Status

	// New is false where this exact reference was already recorded, which a retried
	// task and a replay that recomputes identical bytes both produce.
	New bool

	// MustWriteBytes is a sweep was about to delete this object, so write the bytes
	// again before trusting the reference. See the collection protocol in purge.go: the
	// reference is safe either way, and what is not safe is assuming the object survived
	// the sweep that had already claimed it.
	MustWriteBytes bool
}

// WriteArtifact records one reference and the object behind it.
//
// The object row is what the count is on, the artifact row is the reference, and the two
// move together: a new reference raises the count by one, and only a reference that is
// genuinely new does. Writing the same reference twice is therefore not an error and not a
// second count, which is what lets a retried task and a replay both write what they always
// wrote. The same URI naming different bytes is an error, because that is a URI that stopped
// meaning one thing.
func (n *NS) WriteArtifact(ctx context.Context, r Reference) (Written, error) {
	return writeArtifact(ctx, n.tx, n.namespace, r)
}

// writeArtifact is the work, on whichever door asked for it. The controller records a run's
// artifacts through the installation door, because a run is decided from outside a namespace.
func writeArtifact(ctx context.Context, tx pgx.Tx, namespace string, r Reference) (Written, error) {
	if err := r.URI.Run.Validate(); err != nil {
		return Written{}, fmt.Errorf("db: the artifact's run: %w", err)
	}
	if err := r.URI.Step.Validate(); err != nil {
		return Written{}, fmt.Errorf("db: the artifact's step: %w", err)
	}
	if err := r.URI.Port.Validate(); err != nil {
		return Written{}, fmt.Errorf("db: the artifact's port: %w", err)
	}
	if r.URI.Name == "" || strings.Contains(r.URI.Name, "/") {
		return Written{}, fmt.Errorf("db: %q is not an artifact name: a name is one segment and carries no slash", r.URI.Name)
	}
	if !hexDigest.MatchString(r.Digest) {
		return Written{}, fmt.Errorf("db: %q is not a digest: sixty-four lowercase hexadecimal characters, in full, since it is the <digest> the physical key sha256/<digest> is built from and an elided one addresses nothing", r.Digest)
	}
	if r.Size < 0 {
		return Written{}, fmt.Errorf("db: an artifact of %d bytes", r.Size)
	}
	if r.For <= 0 {
		return Written{}, errors.New("db: the reference says how many fetches it survives and not how long it lives: retain always carries for, and a one-shot artifact still has a duration, which is what expires it when nobody ever comes for it")
	}
	if r.Fetches < 0 {
		return Written{}, fmt.Errorf("db: a fetch budget of %d", r.Fetches)
	}
	if r.MediaType == "" {
		r.MediaType = "application/octet-stream"
	}

	ceiling, err := retentionCeiling(ctx, tx, namespace)
	if err != nil {
		return Written{}, err
	}

	stored := "sha256:" + r.Digest
	out := Written{Key: artifact.Key(namespace, r.Digest), Fetches: r.Fetches}

	// The object first, because the reference has a foreign key onto it, and counted up
	// before the reference is written rather than after: a count that is momentarily too
	// high keeps an object alive that nothing needed, and a count that is momentarily too
	// low lets a sweep delete an object something did.
	mustWrite, _, err := raise(ctx, tx, namespace, stored, r.Size, r.MediaType)
	if err != nil {
		return Written{}, err
	}
	out.MustWriteBytes = mustWrite

	// fetches_left is null where there is no budget, rather than zero, because zero left
	// is what a spent budget looks like and a spent one is retired.
	var budget *int
	if r.Fetches > 0 {
		budget = &r.Fetches
	}
	var expires time.Time
	var status Status
	var existing string
	err = tx.QueryRow(ctx,
		`insert into artifacts (namespace, run_id, step, port, name, digest, size_bytes, media_type,
		                        expires_at, fetches_left)
		 values ($1, $2, $3, $4, $5, $6, $7, $8,
		         now() + least($9::bigint * interval '1 second', $10::int * interval '1 day'), $11)
		 on conflict (namespace, run_id, step, port, name) do nothing
		 returning expires_at, status, digest`,
		namespace, string(r.URI.Run), string(r.URI.Step), string(r.URI.Port), r.URI.Name,
		stored, r.Size, r.MediaType, int64(r.For/time.Second), ceiling, budget).Scan(&expires, &status, &existing)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The reference was already there, so the count this call raised is one too
		// many and comes straight back down. Idempotent where the reference says the
		// same thing, and an error where it does not.
		if err := lower(ctx, tx, namespace, stored, ""); err != nil {
			return Written{}, err
		}
		var left *int
		if err := tx.QueryRow(ctx,
			`select digest, expires_at, status, fetches_left from artifacts
			 where namespace = $1 and run_id = $2 and step = $3 and port = $4 and name = $5`,
			namespace, string(r.URI.Run), string(r.URI.Step), string(r.URI.Port), r.URI.Name,
		).Scan(&existing, &expires, &status, &left); err != nil {
			return Written{}, fmt.Errorf("db: the reference could not be read back: %w", err)
		}
		if existing != stored {
			return Written{}, fmt.Errorf("db: %s already names %s and this writes %s: a logical URI resolves to one physical key, and two digests under one URI is a URI that stopped meaning one thing", r.URI, existing, stored)
		}
		out.ExpiresAt, out.Status = expires, status
		out.Fetches = 0
		if left != nil {
			out.Fetches = *left
		}
		return out, nil
	case err != nil:
		return Written{}, fmt.Errorf("db: the reference could not be recorded: %w", err)
	}

	out.ExpiresAt, out.Status = expires, status
	out.New = true
	return out, nil
}

// Resolved is what a logical URI resolves to.
type Resolved struct {
	URI agk.URI

	// Digest is the sixty-four hexadecimal characters an envelope carries, and Key is
	// the physical key those characters address, namespace and all.
	Digest string
	Key    string

	Size      int64
	MediaType string

	Status    Status
	ExpiresAt time.Time

	// RetiredAt is when a retired reference was retired, and the zero time on a live
	// one.
	RetiredAt time.Time

	// Fetches is what is left of the budget, zero where there is none.
	Fetches int
}

// Resolve turns agk://run/<run>/<step>/<port>/<name> into the physical key it addresses.
//
// A reference that has expired or been collected resolves to what it was, with ErrGone
// beside it: the name, the size and the digest are what the run detail keeps showing, and
// the error is what tells an API to answer 410 rather than 404.
func (n *NS) Resolve(ctx context.Context, u agk.URI) (Resolved, error) {
	out := Resolved{URI: u}
	var stored string
	var retired *time.Time
	var left *int
	err := n.tx.QueryRow(ctx,
		`select digest, size_bytes, media_type, status, expires_at, retired_at, fetches_left
		 from artifacts
		 where namespace = $1 and run_id = $2 and step = $3 and port = $4 and name = $5`,
		n.namespace, string(u.Run), string(u.Step), string(u.Port), u.Name,
	).Scan(&stored, &out.Size, &out.MediaType, &out.Status, &out.ExpiresAt, &retired, &left)
	if errors.Is(err, pgx.ErrNoRows) {
		return Resolved{URI: u}, ErrNoArtifact
	}
	if err != nil {
		return Resolved{URI: u}, fmt.Errorf("db: %s could not be resolved: %w", u, err)
	}

	out.Digest = trimAlgorithm(stored)
	out.Key = artifact.Key(n.namespace, out.Digest)
	if retired != nil {
		out.RetiredAt = *retired
	}
	if left != nil {
		out.Fetches = *left
	}
	if out.Status != Live {
		return out, ErrGone
	}
	return out, nil
}

// Fetched counts one fetch against the budget, and answers what is left.
//
// Call it when the response completes and never before: "A fetch counts when the response
// completes. A transfer that is interrupted, refused, or ranged over part of the object does
// not consume the artifact, so a client that loses its connection retries against an
// artifact still there."
//
// Spending the last fetch retires the reference here and now, not at the next sweep: "Once
// consumed, the reference is gone immediately rather than at the next sweep." An artifact
// with no budget is unaffected, and answers zero.
func (n *NS) Fetched(ctx context.Context, u agk.URI) (left int, err error) {
	var status Status
	var remaining *int
	var stored string
	err = n.tx.QueryRow(ctx,
		`update artifacts
		 set fetches_left = case when fetches_left is null then null else fetches_left - 1 end
		 where namespace = $1 and run_id = $2 and step = $3 and port = $4 and name = $5
		   and status = 'live'
		 returning fetches_left, status, digest`,
		n.namespace, string(u.Run), string(u.Step), string(u.Port), u.Name,
	).Scan(&remaining, &status, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		// Either it was never there or it is already retired, and the difference is
		// worth keeping: one is 404 and the other is 410.
		if _, err := n.Resolve(ctx, u); err != nil {
			return 0, err
		}
		return 0, ErrNoArtifact
	}
	if err != nil {
		return 0, fmt.Errorf("db: the fetch against %s could not be counted: %w", u, err)
	}
	if remaining == nil {
		return 0, nil
	}
	if *remaining > 0 {
		return *remaining, nil
	}
	if err := n.retire(ctx, u, Collected, stored); err != nil {
		return 0, err
	}
	return 0, nil
}

// retire drops one reference and lowers the count behind it.
//
// The object is not deleted here and cannot be: "the physical sha256/<digest> object is
// collected once its reference count reaches zero, and not before", and what reaching zero
// does is mark the object collectable. The collector is the one that deletes, after a grace
// period, and it is in purge.go.
func (n *NS) retire(ctx context.Context, u agk.URI, status Status, stored string) error {
	if _, err := n.tx.Exec(ctx,
		`update artifacts set status = $6, retired_at = now(), fetches_left = null
		 where namespace = $1 and run_id = $2 and step = $3 and port = $4 and name = $5
		   and status = 'live'`,
		n.namespace, string(u.Run), string(u.Step), string(u.Port), u.Name, string(status)); err != nil {
		return fmt.Errorf("db: %s could not be retired: %w", u, err)
	}
	return lower(ctx, n.tx, n.namespace, stored, u.Run)
}

// retentionCeiling is what the namespace lets a workflow ask for.
//
// "retain is capped by the namespace quota and cannot exceed it; a workflow may always ask
// for less." Read here rather than trusted from the caller, because a cap a caller applies
// is a cap a caller can forget.
func retentionCeiling(ctx context.Context, tx pgx.Tx, namespace string) (int, error) {
	var days int
	err := tx.QueryRow(ctx,
		`select max_retention_days from namespaces where name = $1`, namespace).Scan(&days)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("db: namespace %q has no row, so the ceiling retain is capped by is unknown and nothing may be written under it", namespace)
	}
	if err != nil {
		return 0, fmt.Errorf("db: the retention ceiling could not be read: %w", err)
	}
	return days, nil
}
