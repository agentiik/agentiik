package db

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// What holds a file of the store against the orphan sweep, against a real PostgreSQL: a row
// counting it, a live artifact naming it, a write under way, and a run of its namespace waiting for
// its v0.2 files to be recorded. Each is asked again in the statement that hands a file over, so
// that one arriving after the first look still holds it.
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
		orphans = append(orphans, Orphan{Digest: d, Size: 1, Written: time.Now().Add(-48 * time.Hour).Truncate(time.Second)})
	}

	if got, err := pool.Unnamed(ctx, "finance", all); err != nil || !slices.Equal(got, []string{free}) {
		t.Fatalf("of the four, nothing holds %v: %v", got, err)
	}
	if got, err := pool.Sweepable(ctx); err != nil || !slices.Contains(got, "finance") {
		t.Fatalf("the namespaces to sweep are %v: %v", got, err)
	}

	// A run of the namespace waiting for its v0.2 files, found after the first look.
	if _, err := conn.Exec(ctx, `insert into artifacts_unrecorded (namespace, run_id) values ('finance', $1)`, financeRun); err != nil {
		t.Fatal(err)
	}
	if n, err := pool.Orphaned(ctx, "finance", orphans); err != nil || n != 0 {
		t.Errorf("with a run of the namespace waiting for its v0.2 files, %d orphans were handed over: %v", n, err)
	}
	if got, err := pool.Sweepable(ctx); err != nil || slices.Contains(got, "finance") {
		t.Errorf("with a run waiting for its v0.2 files, the namespaces to sweep are %v: %v", got, err)
	}
	if _, err := conn.Exec(ctx, `delete from artifacts_unrecorded`); err != nil {
		t.Fatal(err)
	}

	if n, err := pool.Orphaned(ctx, "finance", orphans); err != nil || n != 1 {
		t.Fatalf("%d orphans were handed over, and only the one nothing holds should be: %v", n, err)
	}
	var refs int
	var collectable time.Time
	if err := conn.QueryRow(ctx, `select refs, collectable_at from artifact_objects where digest = 'sha256:' || $1`, free).Scan(&refs, &collectable); err != nil {
		t.Fatal(err)
	}
	if refs != 0 || !collectable.Equal(orphans[0].Written) {
		t.Errorf("the orphan was handed over counted %d, collectable from %s, and it was written %s", refs, collectable, orphans[0].Written)
	}
	if n, err := pool.Orphaned(ctx, "finance", orphans); err != nil || n != 0 {
		t.Errorf("handed over again, %d orphans were: %v", n, err)
	}
}
