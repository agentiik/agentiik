// Package purge runs the retention purges and the collection on an installation's database and its
// built-in object store.
//
// "Retention is declared per workflow within the namespace ceiling, with separate purges for
// envelopes, artifacts and logs, and garbage collection of artifacts whose reference count reaches
// zero." Package db holds each of them as a sweep bounded to a batch per call, and knows nothing of
// the store; this package calls them in turn, deletes from the store what they hand it, and tells
// the database once it has. Without it the store only grows: a quota frees its room at an
// artifact's expiry, since it counts by the expiry, but no byte leaves the disk.
//
// # Who, and when
//
// The controller that leads, and no other: one pass as its term begins and one every Every after,
// for as long as the term lasts. Leading, where it is set, is asked before every call, so a
// controller that has lost its term stops at the next call rather than at the end of the pass.
// Nothing here would be wrong twice at once, since every sweep takes its rows with a lock it skips
// on and marks what it is done with, but two collectors deleting the same bytes and two log purges
// deleting the same chunks is work the installation pays for twice.
//
// # A batch at a time
//
// Every call is a transaction of its own over at most a batch of rows, and a pass calls each purge
// again while it comes back full, at most Calls times, then leaves the rest for the next pass. So a
// backlog, the months of expired rows an installation upgraded from v0.2 holds at its first pass,
// is worked through over as many passes as it takes, and never holds a lock for longer than one
// batch does: the runs are decided and dispatched between two calls as they would be at any other
// time.
//
// # What is deleted, and when
//
// An object is deleted from the store only once its count has sat at zero for the grace, no live
// artifact names it, and no write of it has held it within the grace, which a result heard late
// still finds its bytes by: package db's purge.go sets out the claim, the deletion under a lock and
// the confirmation, and why a crash between any two of them leaves nothing that the next pass does
// not finish. A log's objects are deleted once its run's retention
// has run out, since nothing is added to a log past it. Each deletion is recorded only once the
// store has answered it, and one the store refused is left for the next pass and said.
//
// # Orphans
//
// The collection finds its work in the database, and a file whose row was never written is one it
// would never see: the outputs of an attempt that failed or was lost, and whatever v0.2 left. So a
// pass also walks the store, a batch of entries a call, a namespace's sha256 directory at a time
// and never its logs, and hands the collection each file that no row names, no live artifact
// names, no write holds and no envelope of a run still under way names, once its bytes are older
// than the grace; the collection deletes it as any other. A round of the whole store may take many
// passes, each going on from where the last stopped. A namespace whose v0.2 runs still wait for
// their files to be recorded is not walked, since those files would be taken for orphans.
//
// # What v0.2 left
//
// Backfill records the files v0.2 recorded nothing for, from the envelopes of the runs it finished,
// and is what init and migrate call: see its comment.
package purge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
)

// Every is how often a pass comes round, a constant rather than a setting.
//
// Ten minutes because nothing waits on a purge: an artifact past
// its expiry is refused whichever pass retires it, a quota counts by the expiry rather than by what
// was retired, and an object is collected a day after its count reached zero at the soonest. A pass
// more often buys load and nothing else, and one far rarer lets more gather between two passes than
// the bound on a pass then takes.
const Every = 10 * time.Minute

// Bounds on a pass, where a Purger is given none.
const (
	// DefaultBatch is how many rows one call of a purge takes: references, logs or the objects
	// they were written to, lapsed writes, or objects to collect.
	DefaultBatch = 1000

	// DefaultRuns is how many runs one call of the envelope purge takes, and how many the log
	// purge stamps, fewer than rows because each run is its document and its grants read, or
	// every task of it held.
	DefaultRuns = 100

	// DefaultCalls is how many calls of each purge one pass makes at most. At the defaults a pass
	// retires up to ten thousand references and collects up to ten thousand objects, which is
	// well over a million a day each.
	DefaultCalls = 10
)

// Purged is what one pass removed.
type Purged struct {
	// Artifacts are the references past their retain that were retired.
	Artifacts int

	// Runs are the finished runs past their retention whose envelopes were let go.
	Runs int

	// Logs are the logs deleted from the store whole.
	Logs int

	// Uploads are the writes whose room had lapsed that were forgotten.
	Uploads int

	// Orphans are the files of the store no row named that were handed to the collection, which
	// deletes them among the Objects.
	Orphans int

	// Objects are the objects deleted from the store, and Bytes their size.
	Objects int
	Bytes   int64
}

// Removed says whether the pass removed anything retention decides: a reference, a run's
// envelopes, a log, an orphan or an object. Forgetting writes that have lapsed is bookkeeping, and
// is not.
func (p Purged) Removed() bool { return p.Artifacts+p.Runs+p.Logs+p.Orphans+p.Objects > 0 }

// Purger runs the purges and the collection.
type Purger struct {
	Pool *db.Pool

	// Objects is the built-in store, AGK_OBJECTS_DIR, which the logs and the collected objects
	// are deleted from.
	Objects artifact.Removable

	// Leading answers nil while this process may purge, and is asked before every call, which a
	// pass stops at the first time it answers anything else. Nil purges regardless, which only a
	// test does.
	Leading func(context.Context) error

	// Every is how often Run passes, Every where zero. Batch, Runs and Calls bound a pass, their
	// defaults where zero.
	Every time.Duration
	Batch int
	Runs  int
	Calls int

	// Grace is how long an object sits at a count of zero before it is collected, db.DefaultGrace
	// where zero.
	Grace time.Duration

	// Passed is told what each pass of Run removed, nothing included, and Trouble why one did not
	// finish, before Passed is told of it.
	Passed  func(Purged)
	Trouble func(error)

	// walking is where the orphan sweep stands in the store, kept from one pass to the next.
	walking walking
}

// Run passes at once and every Every after, until ctx is done.
func (p *Purger) Run(ctx context.Context) {
	defer p.walking.stop()
	every := p.Every
	if every <= 0 {
		every = Every
	}
	for {
		purged, err := p.Pass(ctx)
		// Why the pass did not finish is said before what it removed, so that Passed is the last
		// word on a pass: whoever stops Run once it has heard of a pass has heard all of it.
		if err != nil && ctx.Err() == nil && p.Trouble != nil {
			p.Trouble(err)
		}
		if p.Passed != nil {
			p.Passed(purged)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// Pass runs each purge in turn, then the orphan sweep and the collection, and answers what it
// removed. A Purger passes once at a time.
//
// A purge that fails is said in the error and ends that purge's part of the pass, and the next
// one goes on: an object store refusing to delete a log is no reason to leave references past
// their expiry unretired. Leading refusing, or ctx ending, ends the pass there. What was removed
// before is answered either way.
func (p *Purger) Pass(ctx context.Context) (Purged, error) {
	if p.Pool == nil || p.Objects == nil {
		return Purged{}, errors.New("purge: a purger needs the database and the object store")
	}
	var out Purged
	var failed []error
	for _, step := range []struct {
		what string
		call func(context.Context, *Purged) (bool, error)
	}{
		{"the artifact purge", p.artifacts},
		{"the envelope purge", p.envelopes},
		{"the log purge", p.logs},
		{"the log purge", p.logsGone},
		{"the purge of lapsed writes", p.uploads},
		{"the orphan sweep", p.orphans},
		{"the collection", p.collect},
	} {
		for range p.calls() {
			if err := ctx.Err(); err != nil {
				return out, errors.Join(append(failed, err)...)
			}
			if p.Leading != nil {
				if err := p.Leading(ctx); err != nil {
					return out, errors.Join(append(failed, fmt.Errorf("purge: this controller no longer leads, and purges nothing: %w", err))...)
				}
			}
			full, err := step.call(ctx, &out)
			if err != nil {
				failed = append(failed, fmt.Errorf("purge: %s did not finish, and the next pass takes it up again: %w", step.what, err))
				break
			}
			if !full {
				break
			}
		}
	}
	return out, errors.Join(failed...)
}

// artifacts retires the references past their retain.
func (p *Purger) artifacts(ctx context.Context, out *Purged) (bool, error) {
	n, err := p.Pool.ExpireArtifacts(ctx, p.batch())
	out.Artifacts += n
	return n == p.batch(), err
}

// envelopes lets go of the envelopes of runs past their retention.
func (p *Purger) envelopes(ctx context.Context, out *Purged) (bool, error) {
	n, err := p.Pool.PurgeEnvelopes(ctx, p.runs())
	out.Runs += n
	return n == p.runs(), err
}

// logs deletes the objects of logs past their run's retention, and records those it deleted.
//
// A key the store refused is left out of what is recorded, so that the next pass is handed it
// again, and the others are recorded all the same.
func (p *Purger) logs(ctx context.Context, out *Purged) (bool, error) {
	due, err := p.Pool.ExpiredLogs(ctx, p.batch())
	if err != nil || len(due) == 0 {
		return false, err
	}
	keys := 0
	var refused []error
	for i := range due {
		deleted := make([]string, 0, len(due[i].Keys))
		for _, key := range due[i].Keys {
			keys++
			if _, err := p.Objects.Remove(ctx, key); err != nil {
				refused = append(refused, err)
				continue
			}
			deleted = append(deleted, key)
		}
		due[i].Keys = deleted
	}
	n, err := p.Pool.LogsPurged(ctx, due)
	out.Logs += n
	if err != nil {
		return false, err
	}
	if len(refused) > 0 {
		return false, fmt.Errorf("%d objects of logs could not be deleted from the store, the first: %w", len(refused), refused[0])
	}
	return len(due) == p.batch() || keys == p.batch(), nil
}

// logsGone stamps the runs none of whose tasks holds a log any more, so that the log purge looks
// no further at them.
func (p *Purger) logsGone(ctx context.Context, _ *Purged) (bool, error) {
	n, err := p.Pool.LogsGone(ctx, p.runs())
	return n == p.runs(), err
}

// uploads forgets writes whose room lapsed.
func (p *Purger) uploads(ctx context.Context, out *Purged) (bool, error) {
	n, err := p.Pool.PurgeUploads(ctx, p.batch())
	out.Uploads += n
	return n == p.batch(), err
}

// collect claims objects nothing references, deletes them from the store and records it.
//
// What the store deleted is recorded even where it refused others, which are left claimed for the
// next pass. An object is counted where this pass deleted its bytes, and not where they were gone
// already: a pass that died after deleting them, or another controller's at the same moment, has.
func (p *Purger) collect(ctx context.Context, out *Purged) (bool, error) {
	claimed, err := p.Pool.Collectable(ctx, p.Grace, p.batch())
	if err != nil || len(claimed) == 0 {
		return false, err
	}
	gone, refused := p.Pool.Collecting(ctx, claimed, func(ctx context.Context, o db.Object) error {
		removed, err := p.Objects.Remove(ctx, o.Key)
		if removed {
			out.Objects++
			out.Bytes += o.Size
		}
		return err
	})
	if _, err := p.Pool.Collected(ctx, gone); err != nil {
		return false, errors.Join(refused, err)
	}
	if refused != nil {
		return false, refused
	}
	return len(claimed) == p.batch(), nil
}

func (p *Purger) batch() int {
	if p.Batch > 0 {
		return p.Batch
	}
	return DefaultBatch
}

func (p *Purger) runs() int {
	if p.Runs > 0 {
		return p.Runs
	}
	return DefaultRuns
}

func (p *Purger) calls() int {
	if p.Calls > 0 {
		return p.Calls
	}
	return DefaultCalls
}
