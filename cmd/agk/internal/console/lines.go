package console

import (
	"fmt"
	"io"
	"time"

	"github.com/agentiik/agentiik/db"
)

// Lines writes the runs view once as plain lines, for wherever there is no screen to draw it on:
// standard input or output not a terminal, or TERM=dumb. One line per run, newest first, its
// columns separated by tabs with no header, its times in RFC 3339, so that a script greps it, cuts
// it or reads it into a loop without a parser of its own:
//
//	state  run  namespace/workflow  trigger  started  took  by
//
// A run not started yet is dated by its creation, and has taken nothing; one started by no
// principal, as a schedule's is, leaves its last column empty rather than inventing a name.
func Lines(w io.Writer, runs []db.ListedRun, now time.Time) error {
	for _, r := range runs {
		at := r.StartedAt
		if at.IsZero() {
			at = r.CreatedAt
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s/%s\t%s\t%s\t%s\t%s\n",
			r.State, r.Run, r.Namespace, r.Workflow, r.Trigger, at.UTC().Format(time.RFC3339), lasted(r.RunSummary, now), r.TriggeredBy); err != nil {
			return err
		}
	}
	return nil
}
