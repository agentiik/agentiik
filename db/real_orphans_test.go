package db

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// What holds a file of the store against the orphan sweep, against a real PostgreSQL: a row
// counting it, a live artifact naming it, a write under way, and a finished run of its namespace
// whose files are still to be recorded. Each is asked again in the statement that hands a file
// over, so that one arriving after the first look still holds it; and a file handed over is
// collectable from then, a grace before it is collected.
func TestAnOrphanIsHandedOverOnlyWhereNothingHoldsIt(t *testing.T) {
	pool, super := opened(t)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.WithoutCancel(ctx))

	free, counted, named, written := digestOf("1"), digestOf("2"), digestOf("3"), digestOf("4")
	if err := pool.In(ctx, "finance", func(ctx context.Context, ns *NS) error {
		if _, err := ns.WriteArtifact(ctx, Reference{URI: uri(financeRun, "archive", "out", "counted.bin"), Digest: counted, Size: 1, For: time.Hour}); err != nil {
			return err
		}
		if _, err := ns.WriteArtifact(ctx, Reference{URI: uri(financeRun, "archive", "out", "named.bin"), Digest: named, Size: 1, For: time.Hour}); err != nil {
			return err
		}
		_, err := ns.Uploading(ctx, written, time.Now().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A count gone wrong, the row gone while a live artifact names it.
	if _, err := conn.Exec(ctx, `delete from artifact_objects where digest = 'sha256:' || $1`, named); err != nil {
		t.Fatal(err)
	}
	all := []string{free, counted, named, written}
	var orphans []Orphan
	for _, d := range all {
		orphans = append(orphans, Orphan{Digest: d, Size: 1})
	}

	if got, err := pool.Unnamed(ctx, "finance", all); err != nil || !slices.Equal(got, []string{free}) {
		t.Fatalf("of the four, nothing holds %v: %v", got, err)
	}
	if got, err := pool.Sweepable(ctx); err != nil || !slices.Contains(got, "finance") {
		t.Fatalf("the namespaces to sweep are %v: %v", got, err)
	}

	// A run of the namespace finished by a controller that recorded none of its files, found after
	// the first look.
	if _, err := conn.Exec(ctx, `update runs set state = 'succeeded', started_at = now(), finished_at = now() where id = $1`, financeRun); err != nil {
		t.Fatal(err)
	}
	if n, err := pool.Orphaned(ctx, "finance", orphans); err != nil || n != 0 {
		t.Errorf("with a finished run of the namespace whose files are not recorded, %d orphans were handed over: %v", n, err)
	}
	if got, err := pool.Sweepable(ctx); err != nil || slices.Contains(got, "finance") {
		t.Errorf("with a finished run whose files are not recorded, the namespaces to sweep are %v: %v", got, err)
	}
	if _, err := conn.Exec(ctx, `update runs set files_recorded = true`); err != nil {
		t.Fatal(err)
	}

	if n, err := pool.Orphaned(ctx, "finance", orphans); err != nil || n != 1 {
		t.Fatalf("%d orphans were handed over, and only the one nothing holds should be: %v", n, err)
	}
	var refs int
	var since time.Duration
	if err := conn.QueryRow(ctx, `select refs, extract(epoch from now() - collectable_at)::bigint * 1000000000 from artifact_objects where digest = 'sha256:' || $1`, free).Scan(&refs, &since); err != nil {
		t.Fatal(err)
	}
	if refs != 0 || since < 0 || since > time.Minute {
		t.Errorf("the orphan was handed over counted %d, collectable for %s, and it should be from the moment it was handed over", refs, since)
	}
	if n, err := pool.Orphaned(ctx, "finance", orphans); err != nil || n != 0 {
		t.Errorf("handed over again, %d orphans were: %v", n, err)
	}
}

// A run a controller of this release decides says its files are recorded, since every decision
// writes a reference for every file its steps have published; one finished by a controller that
// does not, as v0.2's, is still to be recorded.
func TestADecisionSaysItsRunsFilesAreRecorded(t *testing.T) {
	pool, super := opened(t)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	unrecorded := func() []Unrecorded {
		t.Helper()
		runs, err := pool.UnrecordedRuns(ctx, Unrecorded{}, 0)
		if err != nil {
			t.Fatal(err)
		}
		return runs
	}

	if _, err := conn.Exec(ctx, `update runs set state = 'succeeded', started_at = now(), finished_at = now() where id = $1`, financeRun); err != nil {
		t.Fatal(err)
	}
	if runs := unrecorded(); len(runs) != 1 || runs[0].Run != financeRun {
		t.Fatalf("a run finished by a controller that records nothing is not to be recorded: %v", runs)
	}
	finished := time.Now().UTC()
	if err := pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
		return w.SaveDecision(ctx, Decision{
			Namespace: "finance", Run: financeRun, Was: 0, Seq: 1,
			Document: []byte(`{"version":1}`), State: agk.Succeeded,
			StartedAt: finished.Add(-time.Hour), FinishedAt: finished,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if runs := unrecorded(); len(runs) != 0 {
		t.Errorf("a run a decision of this release finished is still to be recorded: %v", runs)
	}
}
