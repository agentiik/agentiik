package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// What a step published, which the database holds as digests and never as bytes.
//
// "Envelopes and logs are not stored in the database: it keeps only their digests and
// URIs." The bytes are objects in the same content-addressed store the artifacts are in,
// and they are counted there for the same reason: two steps publishing identical bytes
// share one object, and a purge that deleted by digest alone would delete what another step
// still names.

// envelopeMediaType is what an envelope is. Recorded on the object so that the store can
// hand the bytes back with it, and fixed here because an envelope is one document shape.
const envelopeMediaType = "application/json"

// Published is one output port of one step, as it was published.
//
// One call publishes every port of a step, because that is what publication is: "what is
// published describes the step and not one of its shards: shard envelopes are concatenated
// port by port before publication".
type Published struct {
	Port   agk.Port
	Digest string // sixty-four lowercase hexadecimal characters
	Size   int64
	Items  int
}

// port is one entry of steps.ports, and the shape the migration fixes.
type port struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	Items  int    `json:"items"`
}

// PublishPorts records the envelope digests of one step.
//
// Republishing is a diff and not a second count: a port republished with the digest it
// already had changes nothing, a port whose digest changed lowers the count on what it named
// and raises it on what it names now, and a port that is no longer published lowers its
// count. A retried attempt that recomputes identical bytes therefore writes nothing, which
// is the same promise content addressing makes everywhere else.
func (n *NS) PublishPorts(ctx context.Context, run agk.RunID, step agk.Step, ports []Published) error {
	if err := run.Validate(); err != nil {
		return fmt.Errorf("db: the step's run: %w", err)
	}
	if err := step.Validate(); err != nil {
		return fmt.Errorf("db: the step: %w", err)
	}
	next := map[string]port{}
	for _, p := range ports {
		if err := p.Port.Validate(); err != nil {
			return fmt.Errorf("db: a published port: %w", err)
		}
		if !hexDigest.MatchString(p.Digest) {
			return fmt.Errorf("db: port %s publishes %q, and a digest is sixty-four lowercase hexadecimal characters", p.Port, p.Digest)
		}
		if p.Size < 0 || p.Items < 0 {
			return fmt.Errorf("db: port %s publishes %d bytes and %d items", p.Port, p.Size, p.Items)
		}
		if _, twice := next[string(p.Port)]; twice {
			return fmt.Errorf("db: port %s is published twice in one call, and a step publishes each of its ports once", p.Port)
		}
		next[string(p.Port)] = port{Digest: "sha256:" + p.Digest, Size: p.Size, Items: p.Items}
	}

	// The row is locked while the diff is taken, so that two publications of one step
	// cannot both read the same previous state and each raise a count for it.
	var raw []byte
	err := n.tx.QueryRow(ctx,
		`select ports from steps
		 where namespace = $1 and run_id = $2 and step = $3 for update`,
		n.namespace, string(run), string(step)).Scan(&raw)
	if err == pgx.ErrNoRows {
		return fmt.Errorf("db: step %s of run %s has no row, and a port is published by the step that has one", step, run)
	}
	if err != nil {
		return fmt.Errorf("db: the published ports could not be read: %w", err)
	}
	previous := map[string]port{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &previous); err != nil {
			return fmt.Errorf("db: the published ports of step %s could not be read: %w", step, err)
		}
	}

	for name, was := range previous {
		if now, still := next[name]; still && now.Digest == was.Digest {
			continue
		}
		if err := n.lower(ctx, was.Digest, run); err != nil {
			return err
		}
	}
	for name, now := range next {
		if was, had := previous[name]; had && was.Digest == now.Digest {
			continue
		}
		if _, err := n.raise(ctx, now.Digest, now.Size, envelopeMediaType); err != nil {
			return err
		}
	}

	encoded, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("db: the published ports could not be written: %w", err)
	}
	if _, err := n.tx.Exec(ctx,
		`update steps set ports = $4 where namespace = $1 and run_id = $2 and step = $3`,
		n.namespace, string(run), string(step), encoded); err != nil {
		return fmt.Errorf("db: the published ports could not be written: %w", err)
	}
	return nil
}

// raise records an object and counts one more thing pointing at it.
//
// It is the half of WriteArtifact that has nothing to do with a reference row, reached from
// both sides so that an envelope and an artifact are counted by the same code. It answers
// whether the bytes have to be written again: a sweep had taken this object, and the window
// between a sweep deleting an object and a reference arriving for it is closed by the writer
// rather than by hoping the two never cross. A brand new object answers false, since nothing
// can have swept what did not exist.
func (n *NS) raise(ctx context.Context, stored string, size int64, mediaType string) (mustWriteBytes bool, err error) {
	var held int64
	var collecting *time.Time
	err = n.tx.QueryRow(ctx,
		`select size_bytes, collecting_at from artifact_objects
		 where namespace = $1 and digest = $2 for update`,
		n.namespace, stored).Scan(&held, &collecting)
	switch {
	case err == pgx.ErrNoRows:
		if _, err := n.tx.Exec(ctx,
			`insert into artifact_objects (namespace, digest, size_bytes, media_type, refs)
			 values ($1, $2, $3, $4, 1)`,
			n.namespace, stored, size, mediaType); err != nil {
			return false, fmt.Errorf("db: the object could not be recorded: %w", err)
		}
		return false, nil
	case err != nil:
		return false, fmt.Errorf("db: the object could not be read: %w", err)
	}
	if held != size {
		return false, fmt.Errorf("db: %s is already held at %d bytes and this one says %d: a digest is computed over the bytes, so two sizes under one digest means one of them was not", stored, held, size)
	}
	if _, err := n.tx.Exec(ctx,
		`update artifact_objects set refs = refs + 1, collectable_at = null, collecting_at = null
		 where namespace = $1 and digest = $2`,
		n.namespace, stored); err != nil {
		return false, fmt.Errorf("db: the reference count could not be raised: %w", err)
	}
	return collecting != nil, nil
}

// lower counts one fewer thing pointing at an object, and marks it collectable at zero.
//
// It never deletes. "The physical sha256/<digest> object is collected once its reference
// count reaches zero, and not before", and the collecting is the sweep's, after the grace
// period purge.go sets out.
func (n *NS) lower(ctx context.Context, stored string, run agk.RunID) error {
	if _, err := n.tx.Exec(ctx,
		`update artifact_objects
		 set refs = refs - 1,
		     collectable_at = case when refs - 1 = 0 then now() else null end
		 where namespace = $1 and digest = $2 and refs > 0`,
		n.namespace, stored); err != nil {
		return fmt.Errorf("db: the reference count could not be lowered: %w", err)
	}
	if run == "" {
		return nil
	}
	if _, err := n.tx.Exec(ctx,
		`update runs set replay_from_start_only = true
		 where namespace = $1 and id = $2 and not replay_from_start_only`,
		n.namespace, string(run)); err != nil {
		return fmt.Errorf("db: the run could not be marked replayable from the start only: %w", err)
	}
	return nil
}

// Ports answers what a step published, in the form the run detail shows.
type Ports map[agk.Port]Envelope

// Envelope is one published envelope: what the database keeps of it, which is its digest,
// its size and how many items it carried.
type Envelope struct {
	Digest string
	Size   int64
	Items  int

	// PurgedAt is when the bytes behind the digest were purged, and the zero time while
	// they are still there. The digest outlives them: it is the record of what was
	// published.
	PurgedAt time.Time
}

// PublishedPorts reads back what a step published.
func (n *NS) PublishedPorts(ctx context.Context, run agk.RunID, step agk.Step) (Ports, error) {
	var raw []byte
	var purged *time.Time
	err := n.tx.QueryRow(ctx,
		`select ports, envelopes_purged_at from steps
		 where namespace = $1 and run_id = $2 and step = $3`,
		n.namespace, string(run), string(step)).Scan(&raw, &purged)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("db: step %s of run %s has no row", step, run)
	}
	if err != nil {
		return nil, fmt.Errorf("db: the published ports could not be read: %w", err)
	}
	held := map[string]port{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &held); err != nil {
			return nil, fmt.Errorf("db: the published ports of step %s could not be read: %w", step, err)
		}
	}
	out := Ports{}
	for name, p := range held {
		e := Envelope{Digest: trimAlgorithm(p.Digest), Size: p.Size, Items: p.Items}
		if purged != nil {
			e.PurgedAt = *purged
		}
		out[agk.Port(name)] = e
	}
	return out, nil
}
