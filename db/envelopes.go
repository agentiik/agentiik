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
// "Envelopes and logs are not stored in the database: it keeps only their digests and URIs."
// The bytes are objects in the same content-addressed store the artifacts are in, and they are
// counted there for the same reason: two steps publishing identical bytes share one object, and
// a purge that deleted by digest alone would delete what another step still names.
//
// A port is published by a decision and by nothing else. There was a second door here, a
// namespaced PublishPorts, and two doors is what made the count and the purge disagree: one
// wrote steps.ports and counted, the other wrote the run's document, and the purge could only
// see one of them. What publishes a port is the controller, because publication is something
// the evaluator decides: "A port produces exactly one envelope, once, when the emitting step
// ends."

// envelopeMediaType is what an envelope is. Recorded on the object so that the store can
// hand the bytes back with it, and fixed here because an envelope is one document shape.
const envelopeMediaType = "application/json"

// port is one entry of steps.ports, and the shape the migration fixes.
type port struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	Items  int    `json:"items"`
}

// raise records an object and counts one more thing pointing at it.
//
// It is the half of WriteArtifact that has nothing to do with a reference row, reached from
// both sides so that an envelope and an artifact are counted by the same code. It answers
// whether the bytes have to be written again: a sweep had taken this object, and the window
// between a sweep deleting an object and a reference arriving for it is closed by the writer
// rather than by hoping the two never cross. A brand new object answers false, since nothing
// can have swept what did not exist.
func raise(ctx context.Context, tx pgx.Tx, namespace, stored string, size int64, mediaType string) (mustWriteBytes bool, err error) {
	var held int64
	var collecting *time.Time
	err = tx.QueryRow(ctx,
		`select size_bytes, collecting_at from artifact_objects
		 where namespace = $1 and digest = $2 for update`,
		namespace, stored).Scan(&held, &collecting)
	switch {
	case err == pgx.ErrNoRows:
		if _, err := tx.Exec(ctx,
			`insert into artifact_objects (namespace, digest, size_bytes, media_type, refs)
			 values ($1, $2, $3, $4, 1)`,
			namespace, stored, size, mediaType); err != nil {
			return false, fmt.Errorf("db: the object could not be recorded: %w", err)
		}
		return false, nil
	case err != nil:
		return false, fmt.Errorf("db: the object could not be read: %w", err)
	}
	if held != size {
		return false, fmt.Errorf("db: %s is already held at %d bytes and this one says %d: a digest is computed over the bytes, so two sizes under one digest means one of them was not", stored, held, size)
	}
	if _, err := tx.Exec(ctx,
		`update artifact_objects set refs = refs + 1, collectable_at = null, collecting_at = null
		 where namespace = $1 and digest = $2`,
		namespace, stored); err != nil {
		return false, fmt.Errorf("db: the reference count could not be raised: %w", err)
	}
	return collecting != nil, nil
}

// lower counts one fewer thing pointing at an object, and marks it collectable at zero.
//
// It never deletes. "The physical sha256/<digest> object is collected once its reference
// count reaches zero, and not before", and the collecting is the sweep's, after the grace
// period purge.go sets out.
func lower(ctx context.Context, tx pgx.Tx, namespace, stored string, run agk.RunID) error {
	if _, err := tx.Exec(ctx,
		`update artifact_objects
		 set refs = refs - 1,
		     collectable_at = case when refs - 1 = 0 then now() else null end
		 where namespace = $1 and digest = $2 and refs > 0`,
		namespace, stored); err != nil {
		return fmt.Errorf("db: the reference count could not be lowered: %w", err)
	}
	if run == "" {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`update runs set replay_from_start_only = true
		 where namespace = $1 and id = $2 and not replay_from_start_only`,
		namespace, string(run)); err != nil {
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
