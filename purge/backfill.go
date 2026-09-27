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
	// Runs are the finished runs whose files are recorded now, and Artifacts the references
	// written for them.
	Runs      int
	Artifacts int

	// Unread are the envelopes of those runs that name nothing: gone from the store, as one the
	// envelope purge let go of and the collection took is, or not an envelope. What they named
	// is recorded by nothing, and is an orphan the collection takes in its time.
	Unread int

	// Left are the runs it could not record, since writers held objects of theirs each time it
	// tried, and Unreadable those whose envelopes the store would not give back, Trouble saying
	// why for the first of them. Both are recorded by a later Backfill or pass, and until then
	// the orphan sweep leaves their namespaces alone.
	Left       int
	Unreadable int
	Trouble    error
}

// backfillRounds is how many times Backfill goes through the runs it could not record because a
// writer held one of their objects, and backfillPause how long it waits before each: a decision
// holds its objects for as long as its transaction, which is milliseconds, so a second later they
// are free unless a controller is deciding the same bytes over and over, which is left for the next
// Backfill, and for the controller's passes, rather than waited out.
const (
	backfillRounds = 3
	backfillPause  = time.Second
)

// Backfill records the artifact files of the finished runs whose files are not recorded: those v0.2
// finished, which recorded no reference for a file of an output its workflow gave no retain, and
// any a v0.2 controller finished while an upgrade replaced it. Each file the envelopes a run's
// steps published name is recorded as an artifact of the run, expiring its namespace's
// max_retention_days after the run finished, so that the purges retire it and the collection
// deletes it in time, as they do any other. init and migrate call it at every run, before the
// controller starts, and a run with nothing left to record reads one empty batch; the controller
// that leads records what they left, a batch a call, in its passes.
//
// It goes runs at a time, the default where runs is zero, each batch one transaction of its own
// once its envelopes are read, so that a large v0.2 store holds no transaction open for longer
// than a batch and a Backfill cut short leaves what it did not reach for the next, which records
// nothing twice. An envelope that is gone from the store, or that is not an envelope, names
// nothing that can be recorded. A run with an envelope the store cannot give back is left waiting,
// whole, and said in Unreadable and Trouble rather than ending Backfill: init failing on it would
// keep every service of the installation from starting, over one file.
func Backfill(ctx context.Context, pool *db.Pool, objects artifact.Objects, runs int) (Backfilled, error) {
	if pool == nil || objects == nil {
		return Backfilled{}, errors.New("purge: recording the artifact files of v0.2 needs the database and the object store")
	}
	if runs <= 0 {
		runs = DefaultRuns
	}
	var out Backfilled
	b := newBackfilling()
	for round := range backfillRounds {
		if round > 0 {
			if out.Left == 0 {
				break
			}
			select {
			case <-ctx.Done():
				b.said(&out)
				return out, ctx.Err()
			case <-time.After(backfillPause):
			}
		}
		out.Left = 0
		var after db.Unrecorded
		for {
			batch, err := pool.UnrecordedRuns(ctx, after, runs)
			if err != nil {
				b.said(&out)
				return out, err
			}
			if len(batch) == 0 {
				break
			}
			after = batch[len(batch)-1]
			recorded, err := b.record(ctx, pool, objects, batch)
			out.Runs += recorded.Runs
			out.Artifacts += recorded.Artifacts
			out.Left += recorded.Left
			if err != nil {
				b.said(&out)
				return out, err
			}
		}
	}
	b.said(&out)
	return out, nil
}

// backfilling is what one Backfill, or one call of a pass, found it could not read.
type backfilling struct {
	// unread are the envelopes that name nothing, by run and digest, since a run left in one
	// round has its envelopes read again in the next.
	unread map[[3]string]bool

	// failed are the runs with an envelope the store would not give back, and why.
	failed map[[2]string]error
}

func newBackfilling() *backfilling {
	return &backfilling{unread: map[[3]string]bool{}, failed: map[[2]string]error{}}
}

// record reads the envelopes of batch and records the files they name, leaving out a run whose
// envelope the store would not give back.
func (b *backfilling) record(ctx context.Context, pool *db.Pool, objects artifact.Objects, batch []db.Unrecorded) (db.Recorded, error) {
	recordings := make([]db.Recording, 0, len(batch))
	for _, u := range batch {
		run := [2]string{u.Namespace, string(u.Run)}
		r := db.Recording{Namespace: u.Namespace, Run: u.Run}
		var failed error
		for _, digest := range u.Envelopes {
			e, err := artifact.GetEnvelope(ctx, objects, u.Namespace, digest, agk.DefaultLimits())
			switch {
			case errors.Is(err, fs.ErrNotExist), errors.Is(err, artifact.ErrNotAnEnvelope):
				b.unread[[3]string{u.Namespace, string(u.Run), digest}] = true
				continue
			case err != nil:
				if ctx.Err() != nil {
					return db.Recorded{}, ctx.Err()
				}
				failed = fmt.Errorf("an envelope of run %s of namespace %s could not be read: %w", u.Run, u.Namespace, err)
			}
			if failed != nil {
				break
			}
			for _, item := range e.Items {
				r.Files = append(r.Files, item.Files...)
			}
		}
		if failed != nil {
			b.failed[run] = failed
			continue
		}
		delete(b.failed, run)
		recordings = append(recordings, r)
	}
	return pool.RecordUnrecorded(ctx, recordings)
}

// said puts what could not be read into out.
func (b *backfilling) said(out *Backfilled) {
	out.Unread = len(b.unread)
	out.Unreadable = len(b.failed)
	out.Trouble = b.trouble()
}

// trouble is why one of the runs left unreadable is, or nil where none is.
func (b *backfilling) trouble() error {
	for _, err := range b.failed {
		return err
	}
	return nil
}

// backfill records, in a pass, the files of a batch of finished runs that are still to be
// recorded, going on from the run the last call stopped at: those an init or a migrate left, where
// a writer held their objects, the store would not give an envelope back, or migrate was given no
// store, and any a v0.2 controller finished while the upgrade replaced it, after init had recorded
// the rest. It comes before the orphan sweep, which leaves a namespace alone while one of its
// runs is still to be recorded.
func (p *Purger) backfill(ctx context.Context, out *Purged) (bool, error) {
	batch, err := p.Pool.UnrecordedRuns(ctx, p.recording, p.runs())
	if err == nil && len(batch) == 0 && p.recording.Namespace != "" {
		// Past the last run, and round to the first again, which a run left behind is.
		p.recording = db.Unrecorded{}
		batch, err = p.Pool.UnrecordedRuns(ctx, p.recording, p.runs())
	}
	if err != nil || len(batch) == 0 {
		return false, err
	}
	p.recording = batch[len(batch)-1]
	b := newBackfilling()
	recorded, err := b.record(ctx, p.Pool, p.Objects, batch)
	out.Recorded += recorded.Runs
	if err != nil {
		return false, err
	}
	if err := b.trouble(); err != nil {
		return false, fmt.Errorf("the files of a finished run are still to be recorded, and are tried again next round: %w", err)
	}
	return len(batch) == p.runs(), nil
}
