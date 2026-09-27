package main

import (
	"context"
	"fmt"
	"io"

	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/purge"
)

// unrecordedArtifacts records the artifact files of the runs v0.2 finished, from the envelopes they
// keep in the object store at objects, as purge.Backfill does, and says what it recorded. v0.2
// recorded no reference for a file of an output its workflow gave no retain, so nothing would ever
// expire or collect those. init and migrate call it at every run, after the migrations, as they
// give the built-in identities, and a run finding nothing left says nothing.
//
// What it cannot record, a run whose objects a writer held or whose envelope the store would not
// give back, it says and leaves for the next run and for the controller's passes, rather than fail:
// a failed init keeps every service of the installation from starting, and those files are safe
// where they are, since the collection takes no file that no row names in their namespaces until
// they are recorded. objects is empty where migrate runs with no AGK_OBJECTS_DIR, and nothing is
// recorded then; it says so while a run is left, and the controller records them in its passes.
func unrecordedArtifacts(ctx context.Context, pool *db.Pool, objects, verb string, out io.Writer) error {
	if objects == "" {
		left, err := pool.UnrecordedRuns(ctx, db.Unrecorded{}, 1)
		if err != nil {
			return err
		}
		if len(left) > 0 {
			fmt.Fprintf(out, "recorded none of the artifact files of the runs v0.2 finished, since %s is not set: the controller that leads records them in its passes, as %s does where it is given the API's settings\n", config.ObjectsDir, verb)
		}
		return nil
	}
	done, err := purge.Backfill(ctx, pool, artifact.Dir(objects), 0)
	if done.Runs > 0 {
		fmt.Fprintf(out, "recorded the artifact files of %s v0.2 finished as %s, each expiring its namespace's max_retention_days after its run finished, so that the purges expire and collect them\n", counted(done.Runs, "run", "runs"), counted(done.Artifacts, "artifact", "artifacts"))
	}
	if done.Unread > 0 {
		fmt.Fprintf(out, "read %s of the runs v0.2 finished as nothing, gone from the store or no envelope: what they name is recorded by nothing, and the collection takes it in its time\n", counted(done.Unread, "envelope", "envelopes"))
	}
	if done.Left > 0 {
		fmt.Fprintf(out, "left the artifact files of %s v0.2 finished unrecorded, since a writer held their objects: the controller that leads records them in its passes\n", counted(done.Left, "run", "runs"))
	}
	if done.Unreadable > 0 {
		fmt.Fprintf(out, "left the artifact files of %s v0.2 finished unrecorded, since the store would not give back an envelope of theirs, and until it does the collection takes no file that no row names in their namespaces: %s\n", counted(done.Unreadable, "run", "runs"), done.Trouble)
	}
	if err != nil {
		return fmt.Errorf("the artifact files of the runs v0.2 finished could not all be recorded, and the next %s records the rest: %w", verb, err)
	}
	return nil
}

// counted writes a number with the word for it, singular where there is one, as agk writes its own.
func counted(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
