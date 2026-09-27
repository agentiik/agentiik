package db

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// The purges as the controller runs them, against a real PostgreSQL: an object a write is under way
// for is neither claimed nor deleted, a write waits for a deletion under way, a run's counts are
// lowered once however many purges reach it, and a run is done with its logs only once no task can
// hold one.

// superuser is a connection behind the policies, for setting clocks and holding rows as another
// transaction would.
func superuser(t *testing.T, super string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.WithoutCancel(t.Context())) })
	return conn
}

// collectable writes an artifact of finance's run onto digest, spends its one fetch so that nothing
// counts the object, and puts its count at zero two days back, past the grace.
func collectable(t *testing.T, pool *Pool, conn *pgx.Conn, name, digest string) {
	t.Helper()
	u := uri(financeRun, "archive", "out", name)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		if _, err := ns.WriteArtifact(ctx, Reference{URI: u, Digest: digest, Size: 5, For: time.Hour, Fetches: 1}); err != nil {
			return err
		}
		_, err := fetched(ctx, ns, u)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(),
		`update artifact_objects set collectable_at = now() - interval '2 days' where digest = 'sha256:' || $1`, digest); err != nil {
		t.Fatal(err)
	}
}

// uploading records a write of digest in finance, under way for an hour.
func uploading(t *testing.T, pool *Pool, digest string) {
	t.Helper()
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		_, err := ns.Uploading(ctx, digest, time.Now().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// keysOf is the keys of the objects given.
func keysOf(objects []Object) []string {
	var out []string
	for _, o := range objects {
		out = append(out, o.Key)
	}
	return out
}

// An object a write is under way for, and one a live artifact names whatever its count says, are
// not claimed, and not deleted either where a claim was made before: the bytes of the first are the
// runner's, which nothing could write again, and the second is a count gone wrong.
func TestTheCollectorPassesByAnObjectBeingWrittenOrNamed(t *testing.T) {
	pool, super := opened(t)
	conn := superuser(t, super)
	written, named := digestOf("4"), digestOf("5")

	collectable(t, pool, conn, "written.bin", written)
	uploading(t, pool, written)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		_, err := ns.WriteArtifact(ctx, Reference{URI: uri(financeRun, "archive", "out", "named.bin"), Digest: named, Size: 5, For: time.Hour})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(),
		`update artifact_objects set refs = 0, collectable_at = now() - interval '2 days' where digest = 'sha256:' || $1`, named); err != nil {
		t.Fatal(err)
	}

	claimed, err := pool.Collectable(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("the collector claimed %q", keysOf(claimed))
	}

	// Claimed all the same, as a sweep that claimed them before either was so would have.
	if _, err := conn.Exec(t.Context(), `update artifact_objects set collecting_at = now()`); err != nil {
		t.Fatal(err)
	}
	both := []Object{
		{Namespace: "finance", Digest: written, Key: "finance/sha256/" + written},
		{Namespace: "finance", Digest: named, Key: "finance/sha256/" + named},
	}
	var removed []string
	gone, err := pool.Collecting(t.Context(), both, func(_ context.Context, o Object) error {
		removed = append(removed, o.Key)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 0 || len(removed) != 0 {
		t.Fatalf("the collector deleted %q", removed)
	}

	// Once the write has lapsed, its object goes, and the named one never does.
	if _, err := conn.Exec(t.Context(), `update artifact_uploads set until = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	claimed, err = pool.Collectable(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"finance/sha256/" + written}; !slices.Equal(keysOf(claimed), want) {
		t.Fatalf("once the write lapsed the collector claimed %q, want %q", keysOf(claimed), want)
	}
	gone, err = pool.Collecting(t.Context(), claimed, func(context.Context, Object) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if n, err := pool.Collected(t.Context(), gone); err != nil || n != 1 {
		t.Errorf("the collection confirmed %d: %v", n, err)
	}
}

// A write recorded after the claim and before the deletion keeps its bytes: the deletion asks about
// writes only once it holds the rows.
func TestAWriteRecordedAfterTheClaimKeepsItsBytes(t *testing.T) {
	pool, super := opened(t)
	conn := superuser(t, super)
	d := digestOf("6")
	collectable(t, pool, conn, "late.bin", d)

	claimed, err := pool.Collectable(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("the collector claimed %q", keysOf(claimed))
	}
	uploading(t, pool, d)
	removed := 0
	gone, err := pool.Collecting(t.Context(), claimed, func(context.Context, Object) error {
		removed++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 || len(gone) != 0 {
		t.Errorf("the collector deleted the bytes of an object a write had begun on since its claim")
	}
	if n, err := pool.Collected(t.Context(), gone); err != nil || n != 0 {
		t.Errorf("the collection confirmed %d: %v", n, err)
	}
}

// A write of an object whose bytes are being deleted waits for the deletion, and so writes its
// bytes once they are gone rather than before.
func TestAWriteOfAnObjectBeingDeletedWaitsForTheDeletion(t *testing.T) {
	pool, super := opened(t)
	conn := superuser(t, super)
	d := digestOf("9")
	collectable(t, pool, conn, "racing.bin", d)
	claimed, err := pool.Collectable(t.Context(), 0, 0)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("the collector claimed %q: %v", keysOf(claimed), err)
	}

	deleting, release := make(chan struct{}), make(chan struct{})
	// Let go on every way out, so that a failure below ends the test rather than leaving the
	// deletion holding its transaction until the binary times out.
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	defer letGo()
	deleted := make(chan []Object, 1)
	go func() {
		gone, err := pool.Collecting(t.Context(), claimed, func(context.Context, Object) error {
			close(deleting)
			<-release
			return nil
		})
		if err != nil {
			t.Error(err)
		}
		deleted <- gone
	}()
	<-deleting

	recorded := make(chan error, 1)
	go func() {
		recorded <- pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			_, err := ns.Uploading(ctx, d, time.Now().Add(time.Hour))
			return err
		})
	}()
	select {
	case err := <-recorded:
		t.Fatalf("a write was recorded while the object's bytes were being deleted: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	letGo()
	if gone := <-deleted; len(gone) != 1 {
		t.Errorf("the deletion under way deleted %q", keysOf(gone))
	}
	select {
	case err := <-recorded:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the write was never recorded once the deletion was over")
	}
}

// A write under way counts nothing against max_artifact_bytes: it is held back from the collector,
// and the room, where the namespace sets a quota, is MakeRoom's to make.
func TestAWriteUnderWayCountsNothingAgainstTheQuota(t *testing.T) {
	pool, _ := bounded(t, 1000)
	uploading(t, pool, digestOf("a"))
	if _, err := makeRoom(t, pool, "finance", digestOf("b"), 1000); err != nil {
		t.Errorf("a write under way took room: %v", err)
	}
}

// Only the writes whose room lapsed are forgotten.
func TestOnlyLapsedWritesAreForgotten(t *testing.T) {
	pool, super := opened(t)
	conn := superuser(t, super)
	uploading(t, pool, digestOf("a"))
	uploading(t, pool, digestOf("b"))
	if _, err := conn.Exec(t.Context(),
		`update artifact_uploads set until = now() - interval '1 second' where digest = 'sha256:' || $1`, digestOf("a")); err != nil {
		t.Fatal(err)
	}
	if n, err := pool.PurgeUploads(t.Context(), 0); err != nil || n != 1 {
		t.Fatalf("the purge forgot %d writes: %v", n, err)
	}
	var left []string
	rows, err := conn.Query(t.Context(), `select digest from artifact_uploads`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		left = append(left, d)
	}
	if want := []string{"sha256:" + digestOf("b")}; !slices.Equal(left, want) {
		t.Errorf("the writes left are %q, want %q", left, want)
	}
}

// decided writes a decision of finance's run publishing digest on archive's port ok, and makes the
// run finished and past its retention.
func decided(t *testing.T, pool *Pool, conn *pgx.Conn, digest string) {
	t.Helper()
	document, err := json.Marshal(map[string]any{"version": 1, "envelopes": []map[string]any{
		{"step": "archive", "shard": PublishedByTheStep, "port": "ok", "digest": digest, "size": 128},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		return w.SaveDecision(ctx, Decision{
			Namespace: "finance", Run: financeRun, Was: 0, Seq: 1,
			Document: document, State: agk.Running, StartedAt: time.Now().UTC(),
			Envelopes: []EnvelopeRef{{Step: "archive", Shard: PublishedByTheStep, Port: "ok", Digest: digest, Size: 128, Items: 1}},
		})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(),
		`update runs set finished_at = now(), expires_at = now() - interval '1 minute' where id = $1`, financeRun); err != nil {
		t.Fatal(err)
	}
}

// A run another purge holds is passed by, and a run purged is never taken again, even where one of
// its steps has lost its stamp: its counts are lowered once however many purges reach it.
func TestARunsEnvelopesAreLetGoOfOnce(t *testing.T) {
	pool, super := opened(t)
	conn := superuser(t, super)
	d := digestOf("c")
	decided(t, pool, conn, d)

	// A second purge at once, a controller's that has not noticed yet that it no longer leads.
	other, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(t.Context(), `select 1 from runs where id = $1 for update`, financeRun); err != nil {
		t.Fatal(err)
	}
	// Bounded, since a purge that waited for the row would wait on this test for ever.
	waiting, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if n, err := pool.PurgeEnvelopes(waiting, 0); err != nil || n != 0 {
		t.Fatalf("the purge took %d runs while another held the only one: %v", n, err)
	}
	if got := refsOf(t, pool, "finance", d); got != 1 {
		t.Fatalf("the envelope is counted %d times while its run was held", got)
	}
	if err := other.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	if n, err := pool.PurgeEnvelopes(t.Context(), 0); err != nil || n != 1 {
		t.Fatalf("the purge took %d runs: %v", n, err)
	}
	if got := refsOf(t, pool, "finance", d); got != 0 {
		t.Fatalf("the envelope of the purged run is counted %d times", got)
	}

	// Counted again by something else, and the step's stamp lost: the run's own stamp keeps the
	// purge from lowering the count a second time.
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		_, err := ns.WriteArtifact(ctx, Reference{URI: uri(financeRun, "render", "out", "same.json"), Digest: d, Size: 128, For: time.Hour})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `update steps set envelopes_purged_at = null where run_id = $1`, financeRun); err != nil {
		t.Fatal(err)
	}
	if n, err := pool.PurgeEnvelopes(t.Context(), 0); err != nil || n != 0 {
		t.Errorf("the purge took %d runs again: %v", n, err)
	}
	if got := refsOf(t, pool, "finance", d); got != 1 {
		t.Errorf("a purged run lowered a count again, to %d", got)
	}
}

// A run whose steps were all stamped before the run had a stamp of its own has had its counts
// lowered already, and is stamped without lowering them again.
func TestARunWhoseStepsWereStampedLowersNothing(t *testing.T) {
	pool, super := opened(t)
	conn := superuser(t, super)
	d := digestOf("d")
	decided(t, pool, conn, d)
	if _, err := conn.Exec(t.Context(), `update steps set envelopes_purged_at = now() - interval '1 day' where run_id = $1`, financeRun); err != nil {
		t.Fatal(err)
	}
	if n, err := pool.PurgeEnvelopes(t.Context(), 0); err != nil || n != 0 {
		t.Fatalf("the purge let go of the envelopes of %d runs: %v", n, err)
	}
	if got := refsOf(t, pool, "finance", d); got != 1 {
		t.Errorf("the envelope is counted %d times", got)
	}
	var stamped bool
	if err := conn.QueryRow(t.Context(), `select envelopes_purged_at is not null from runs where id = $1`, financeRun).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if !stamped {
		t.Error("the run was left for the purge to take again")
	}
}

// expired makes finance's run finished and past its retention, its task holding no log.
func expired(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	for _, stmt := range []string{
		`update runs set started_at = now() - interval '2 days', finished_at = now() - interval '1 day',
		                 expires_at = now() - interval '1 second' where id = '` + financeRun + `'`,
		`update tasks set log_uri = null where run_id = '` + financeRun + `'`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
}

// logsDone says whether finance's run is stamped as done with its logs.
func logsDone(t *testing.T, conn *pgx.Conn) bool {
	t.Helper()
	var done bool
	if err := conn.QueryRow(t.Context(), `select logs_purged_at is not null from runs where id = $1`, financeRun).Scan(&done); err != nil {
		t.Fatal(err)
	}
	return done
}

// A run is done with its logs once none of its tasks holds one, and not while one does.
func TestARunIsDoneWithItsLogsOnceNoTaskHoldsOne(t *testing.T) {
	pool, super := opened(t)
	conn := superuser(t, super)
	expired(t, conn)
	if _, err := conn.Exec(t.Context(), `update tasks set log_uri = 'agk://log/`+financeRun+`/`+financeRun+`%2Farchive%2F1'`); err != nil {
		t.Fatal(err)
	}
	if n, err := pool.LogsGone(t.Context(), 0); err != nil || n != 0 || logsDone(t, conn) {
		t.Fatalf("a run whose task holds a log was stamped done with it (%d, %v)", n, err)
	}
	if _, err := conn.Exec(t.Context(), `update tasks set log_uri = null`); err != nil {
		t.Fatal(err)
	}
	if n, err := pool.LogsGone(t.Context(), 0); err != nil || n != 1 || !logsDone(t, conn) {
		t.Fatalf("a run none of whose tasks holds a log was not stamped done with them (%d, %v)", n, err)
	}

	// And the log purge looks no further at it, which is what the stamp is for.
	if _, err := conn.Exec(t.Context(), `update tasks set log_uri = 'agk://log/`+financeRun+`/`+financeRun+`%2Farchive%2F1'`); err != nil {
		t.Fatal(err)
	}
	if logs, err := pool.ExpiredLogs(t.Context(), 0); err != nil || len(logs) != 0 {
		t.Errorf("the log purge looked at a run done with its logs: %+v, %v", logs, err)
	}
}

// A shipment under way when the run is stamped holds its task, as ShippingLog does: the stamp waits
// for it, and a shipment that named its log meanwhile keeps the run from being stamped.
func TestTheLogStampWaitsForAShipmentUnderWay(t *testing.T) {
	pool, super := opened(t)
	conn := superuser(t, super)
	expired(t, conn)

	shipping, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer shipping.Rollback(context.WithoutCancel(t.Context()))
	if _, err := shipping.Exec(t.Context(), `select 1 from tasks where run_id = $1 for key share`, financeRun); err != nil {
		t.Fatal(err)
	}
	stamped := make(chan int, 1)
	go func() {
		n, err := pool.LogsGone(t.Context(), 0)
		if err != nil {
			t.Error(err)
		}
		stamped <- n
	}()
	select {
	case n := <-stamped:
		t.Fatalf("the stamp went ahead, %d runs, with a shipment under way", n)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := shipping.Exec(t.Context(),
		`update tasks set log_uri = 'agk://log/`+financeRun+`/`+financeRun+`%2Farchive%2F1' where run_id = $1`, financeRun); err != nil {
		t.Fatal(err)
	}
	if err := shipping.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-stamped:
		if n != 0 || logsDone(t, conn) {
			t.Errorf("a run whose task was given a log while the stamp waited was stamped done with its logs")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stamp never finished once the shipment had")
	}
}

// An installation that ran v0.2.5 holds runs it finished with no expiry, since v0.2.5 recorded
// none, and artifacts long past theirs. The upgrade gives each finished run what its namespace's
// max_retention_days allows, and the purges then take what has run out and nothing else.
func TestAnInstallationOfV025PurgesWhatItHeld(t *testing.T) {
	super, role := migratedAt(t, v025)
	ctx := t.Context()
	conn := superuser(t, super)
	const old, young, running = "01JMZ8V1P9C4XQ7K2N4D6F8H0A", "01M2AAZ9G62NQXFAFCXKRPJEH5", "01M2B0000000000000000000RN"
	for _, stmt := range []string{
		`insert into namespaces (name, max_retention_days) values ('finance', 30)`,
		`insert into workflows (namespace, name) values ('finance', 'monthly-invoicing')`,
		`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at)
		   values ('finance', 'monthly-invoicing', 'a3f9c1e', '{}', 'operator', now())`,
		`insert into runs (namespace, id, workflow, commit, state, trigger, started_at, finished_at)
		   values ('finance', '` + old + `', 'monthly-invoicing', 'a3f9c1e', 'succeeded', 'manual', now() - interval '61 days', now() - interval '60 days'),
		          ('finance', '` + young + `', 'monthly-invoicing', 'a3f9c1e', 'succeeded', 'manual', now() - interval '2 days', now() - interval '1 day')`,
		`insert into runs (namespace, id, workflow, commit, state, trigger, started_at)
		   values ('finance', '` + running + `', 'monthly-invoicing', 'a3f9c1e', 'running', 'manual', now())`,
		`insert into steps (namespace, run_id, step) values ('finance', '` + old + `', 'archive'),
		   ('finance', '` + young + `', 'archive'), ('finance', '` + running + `', 'archive')`,
		`insert into artifact_objects (namespace, digest, size_bytes, media_type, refs)
		   values ('finance', 'sha256:` + digestOf("a") + `', 5, 'application/octet-stream', 1)`,
		`insert into artifacts (namespace, run_id, step, port, name, digest, size_bytes, media_type, expires_at)
		   values ('finance', '` + old + `', 'archive', 'out', 'old.bin', 'sha256:` + digestOf("a") + `', 5,
		           'application/octet-stream', now() - interval '50 days')`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("filling the database as v0.2.5 would have: %s", err)
		}
	}
	if _, err := Provision(ctx, conn, role, "test"); err != nil {
		t.Fatalf("the upgrade was refused: %s", err)
	}

	for run, want := range map[string]string{old: "30 days", young: "30 days", running: "none"} {
		var kept string
		if err := conn.QueryRow(ctx,
			`select coalesce((expires_at - finished_at)::text, 'none') from runs where id = $1`, run).Scan(&kept); err != nil {
			t.Fatal(err)
		}
		if kept != want {
			t.Errorf("run %s is kept %s past its end, want %s", run, kept, want)
		}
	}

	pool, err := Open(ctx, withCredentials(super, role, "test"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if n, err := pool.ExpireArtifacts(ctx, 0); err != nil || n != 1 {
		t.Errorf("the artifact purge retired %d references: %v", n, err)
	}
	if n, err := pool.PurgeEnvelopes(ctx, 0); err != nil || n != 1 {
		t.Errorf("the envelope purge took %d runs: %v", n, err)
	}
	if n, err := pool.LogsGone(ctx, 0); err != nil || n != 1 {
		t.Errorf("the log purge stamped %d runs: %v", n, err)
	}
	var purged []string
	rows, err := conn.Query(ctx, `select id from runs where envelopes_purged_at is not null or logs_purged_at is not null`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		purged = append(purged, id)
	}
	if !slices.Equal(purged, []string{old}) {
		t.Errorf("the purges took the runs %q, and only %s had run out", purged, old)
	}
}

// The artifact purge waits on no row a decision holds: a decision takes its run's row, then the
// objects it counts, then writes its artifacts again, so a purge holding an artifact and waiting
// on either would deadlock with it. A reference whose run or object is held is left for the next
// pass, and one whose run and object are free is retired meanwhile.
func TestTheArtifactPurgeWaitsOnNothingADecisionHolds(t *testing.T) {
	pool, super := opened(t)
	conn := superuser(t, super)
	held, free := digestOf("e"), digestOf("f")
	for name, d := range map[string]string{"held.bin": held} {
		if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			_, err := ns.WriteArtifact(ctx, Reference{URI: uri(financeRun, "archive", "out", name), Digest: d, Size: 5, For: time.Hour})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.In(t.Context(), "team-ops", func(ctx context.Context, ns *NS) error {
		_, err := ns.WriteArtifact(ctx, Reference{URI: uri(opsRun, "archive", "out", "free.bin"), Digest: free, Size: 5, For: time.Hour})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `update artifacts set expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}

	for _, holding := range []string{
		`select 1 from runs where id = '` + financeRun + `' for update`,
		`select 1 from artifact_objects where digest = 'sha256:` + held + `' for update`,
	} {
		decision, err := conn.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decision.Exec(t.Context(), holding); err != nil {
			t.Fatal(err)
		}
		waiting, stop := context.WithTimeout(t.Context(), 5*time.Second)
		n, err := pool.ExpireArtifacts(waiting, 0)
		stop()
		if err != nil {
			t.Fatalf("with %s held, the purge failed: %v", holding, err)
		}
		if err := decision.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := refsOf(t, pool, "finance", held); got != 1 {
			t.Errorf("with %s held, the purge lowered the held object's count to %d", holding, got)
		}
		if holding == `select 1 from runs where id = '`+financeRun+`' for update` && n != 1 {
			t.Errorf("with finance's run held, the purge retired %d references, and team-ops's was free", n)
		}
	}
	if n, err := pool.ExpireArtifacts(t.Context(), 0); err != nil || n != 1 {
		t.Errorf("once nothing was held the purge retired %d: %v", n, err)
	}
}

// The confirmation waits on no object a writer holds either: the row is left claimed, and the next
// sweep claims it again and removes it.
func TestTheConfirmationPassesByAnObjectAWriterHolds(t *testing.T) {
	pool, super := opened(t)
	conn := superuser(t, super)
	d := digestOf("7")
	collectable(t, pool, conn, "held.bin", d)
	claimed, err := pool.Collectable(t.Context(), 0, 0)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("the collector claimed %q: %v", keysOf(claimed), err)
	}
	writer, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(t.Context(), `select 1 from artifact_objects where digest = 'sha256:`+d+`' for update`); err != nil {
		t.Fatal(err)
	}
	waiting, stop := context.WithTimeout(t.Context(), 5*time.Second)
	n, err := pool.Collected(waiting, claimed)
	stop()
	if err != nil || n != 0 {
		t.Fatalf("with the object held, the confirmation removed %d: %v", n, err)
	}
	if err := writer.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n, err := pool.Collected(t.Context(), claimed); err != nil || n != 1 {
		t.Errorf("once nothing held it, the confirmation removed %d: %v", n, err)
	}
}
