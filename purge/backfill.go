package purge

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
)

// Backfilled is what Backfill recorded.
type Backfilled struct {
	// Runs are the runs v0.2 finished whose files are recorded now, and Artifacts the references
	// written for them.
	Runs      int
	Artifacts int

	// Unread are the envelopes of those runs it could not read: gone from the store, or not an
	// envelope. What they name is recorded by nothing, and is an orphan the collection takes once
	// it is older than the grace.
	Unread int

	// Left are the runs it could not record, since writers held objects of theirs each time it
	// tried: the next Backfill records them, and until then the orphan sweep leaves their
	// namespaces alone.
	Left int
}

// backfillRounds is how many times Backfill goes through the runs it could not record because a
// writer held one of their objects, and backfillPause how long it waits before each: a decision
// holds its objects for as long as its transaction, which is milliseconds, so a second later they
// are free unless a controller is deciding the same bytes over and over, which is left for the next
// Backfill rather than waited out.
const (
	backfillRounds = 3
	backfillPause  = time.Second
)

// Backfill records the artifact files of the runs v0.2 finished, which v0.2 recorded no reference
// for where the workflow gave the output no retain: each file the envelopes a run's steps published
// name is recorded as an artifact of the run, expiring its namespace's max_retention_days after the
// run finished, so that the purges retire it and the collection deletes it in time, as they do any
// other. init and migrate call it at every run, and a run with nothing left to record reads one
// empty batch.
//
// It goes runs at a time, the default where runs is zero, each batch one transaction of its own
// once its envelopes are read, so that a large v0.2 store holds no transaction open for longer
// than a batch and a Backfill cut short leaves what it did not reach for the next, which records
// nothing twice. An envelope that is gone from the store, as one the envelope purge let go of and
// the collection took is, or that is not an envelope, names nothing that can be recorded; one the
// store cannot read ends Backfill, since whether it is there is not known.
func Backfill(ctx context.Context, pool *db.Pool, objects artifact.Objects, runs int) (out Backfilled, err error) {
	if pool == nil || objects == nil {
		return Backfilled{}, errors.New("purge: recording the artifact files of v0.2 needs the database and the object store")
	}
	if runs <= 0 {
		runs = DefaultRuns
	}
	// By run and digest, since a run left in one round has its envelopes read again in the next.
	unread := map[[3]string]bool{}
	defer func() { out.Unread = len(unread) }()
	for round := range backfillRounds {
		if round > 0 {
			if out.Left == 0 {
				break
			}
			select {
			case <-ctx.Done():
				return out, ctx.Err()
			case <-time.After(backfillPause):
			}
		}
		out.Left = 0
		var after db.Unrecorded
		for {
			batch, err := pool.UnrecordedRuns(ctx, after, runs)
			if err != nil {
				return out, err
			}
			if len(batch) == 0 {
				break
			}
			after = batch[len(batch)-1]
			recordings := make([]db.Recording, 0, len(batch))
			for _, u := range batch {
				r := db.Recording{Namespace: u.Namespace, Run: u.Run}
				for _, digest := range u.Envelopes {
					e, err := artifact.GetEnvelope(ctx, objects, u.Namespace, digest, agk.DefaultLimits())
					switch {
					case errors.Is(err, fs.ErrNotExist), errors.Is(err, artifact.ErrNotAnEnvelope):
						unread[[3]string{u.Namespace, string(u.Run), digest}] = true
						continue
					case err != nil:
						return out, fmt.Errorf("purge: an envelope of run %s of namespace %s could not be read, and its files are left for the next recording: %w", u.Run, u.Namespace, err)
					}
					for _, item := range e.Items {
						r.Files = append(r.Files, item.Files...)
					}
				}
				recordings = append(recordings, r)
			}
			recorded, err := pool.RecordUnrecorded(ctx, recordings)
			if err != nil {
				return out, err
			}
			out.Runs += recorded.Runs
			out.Artifacts += recorded.Artifacts
			out.Left += recorded.Left
		}
	}
	return out, nil
}
