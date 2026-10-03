package purge_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/purge"
)

// Backfill: the files of the runs v0.2 finished, which v0.2 recorded no reference for, recorded
// from the envelopes their steps published.

// v02 is a run of finance as a v0.2 controller left it: finished age ago, its step archive having
// published on port out an envelope naming files, whose bytes are in the store and which nothing
// counts, and its files not recorded. It answers the run and the envelope's digest.
func (in *installation) v02(t *testing.T, age time.Duration, files ...file) (agk.RunID, string) {
	t.Helper()
	run := in.run(t)
	for _, f := range files {
		in.put(t, f.content)
	}
	envelope, size := in.named(t, run, files...)
	in.exec(t,
		fmt.Sprintf(`update runs set state = 'succeeded', finished_at = now() - interval '%d seconds',
		               expires_at = now() - interval '%d seconds' + interval '90 days' where id = '%s'`, int(age.Seconds()), int(age.Seconds()), run),
		fmt.Sprintf(`update steps set ports = '{"out": {"digest": "sha256:%s", "size": %d, "items": 1}}' where run_id = '%s'`, envelope, size, run))
	return run, envelope
}

// refs is the count on the object content is, and -1 where it has no row.
func (in *installation) refs(t *testing.T, content string) int {
	t.Helper()
	digest, _ := in.put(t, content)
	var n int
	if err := in.conn.QueryRow(t.Context(),
		`select coalesce((select refs from artifact_objects where digest = 'sha256:' || $1), -1)`, digest).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Every file the published envelopes name is recorded as an artifact of its run, expiring the
// namespace's max_retention_days after the run finished, and counted once; a reference v0.2 wrote,
// for an output given a retain, is left with its own expiry and count; the run says its files are
// recorded, and is given the same expiry where a controller finishing it after migration 0038 gave
// it none; and a second Backfill records nothing and counts nothing again.
func TestBackfillRecordsWhatV02LeftAndNothingTwice(t *testing.T) {
	in := withInstallation(t)
	in.exec(t, `update namespaces set max_retention_days = 90 where name = 'finance'`)
	run, _ := in.v02(t, 10*24*time.Hour, file{"invoices.csv", "invoices"}, file{"totals.json", "totals"}, file{"kept.zip", "kept"})
	digest, _ := in.put(t, "kept")
	if err := in.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		_, err := ns.WriteArtifact(ctx, db.Reference{
			URI: agk.URI{Run: run, Step: "archive", Port: "out", Name: "kept.zip"}, Digest: digest, Size: 4, For: 7 * 24 * time.Hour,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	kept := func() (at time.Time) {
		if err := in.conn.QueryRow(t.Context(), `select expires_at from artifacts where name = 'kept.zip'`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	before := kept()
	in.exec(t, `update runs set expires_at = null where id = '`+string(run)+`'`)

	got, err := purge.Backfill(t.Context(), in.pool, in.store, 0)
	if err != nil || got != (purge.Backfilled{Runs: 1, Artifacts: 2}) {
		t.Fatalf("Backfill recorded %+v: %v", got, err)
	}
	if n := in.count(t, `select count(*) from artifacts
	                     where name in ('invoices.csv', 'totals.json') and status = 'live'
	                       and expires_at = (select finished_at from runs where id = run_id) + interval '90 days'`); n != 2 {
		t.Errorf("%d of the two files v0.2 left are recorded to expire 90 days after their run finished", n)
	}
	if after := kept(); !after.Equal(before) {
		t.Errorf("the reference v0.2 wrote for an output given a retain expired at %s, and expires at %s now", before, after)
	}
	for _, content := range []string{"invoices", "totals", "kept"} {
		if n := in.refs(t, content); n != 1 {
			t.Errorf("the object %q is counted %d times", content, n)
		}
	}
	if n := in.count(t, `select count(*) from runs where files_recorded and expires_at = finished_at + interval '90 days'`); n != 1 {
		t.Error("the run does not say its files are recorded, or was given no expiry")
	}

	if got, err := purge.Backfill(t.Context(), in.pool, in.store, 0); err != nil || got != (purge.Backfilled{}) {
		t.Errorf("a second Backfill recorded %+v: %v", got, err)
	}
	for _, content := range []string{"invoices", "totals", "kept"} {
		if n := in.refs(t, content); n != 1 {
			t.Errorf("after a second Backfill the object %q is counted %d times", content, n)
		}
	}
}

// In a namespace that sets no bound, the files v0.2 left are recorded as kept for ever, written as
// infinity, which no purge reaches; and a run a controller finished with no expiry keeps none, as
// one finished now would.
func TestBackfillKeepsForEverWhatANamespaceSetsNoBoundFor(t *testing.T) {
	in := withInstallation(t)
	run, _ := in.v02(t, 10*24*time.Hour, file{"invoices.csv", "invoices"})
	in.exec(t, `update runs set expires_at = null where id = '`+string(run)+`'`)

	got, err := purge.Backfill(t.Context(), in.pool, in.store, 0)
	if err != nil || got != (purge.Backfilled{Runs: 1, Artifacts: 1}) {
		t.Fatalf("Backfill recorded %+v: %v", got, err)
	}
	if n := in.count(t, `select count(*) from artifacts where name = 'invoices.csv' and status = 'live' and expires_at = 'infinity'`); n != 1 {
		t.Error("the file v0.2 left is not recorded as kept for ever")
	}
	if n := in.count(t, `select count(*) from runs where files_recorded and expires_at is null`); n != 1 {
		t.Error("the run does not say its files are recorded, or was given an expiry")
	}
	if _, err := (&purge.Purger{Pool: in.pool, Objects: in.store}).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := in.count(t, `select count(*) from artifacts where name = 'invoices.csv' and status = 'live'`); n != 1 {
		t.Error("a pass retired a file kept for ever")
	}
}

// Only what the run's own steps published is recorded: a file naming another run, or a step the
// run does not have, or at another size than its object is held at, or whose object is being
// collected, is not; and an envelope gone from the store names nothing, the run being waited for no
// longer all the same.
func TestBackfillRecordsOnlyWhatARunsStepsPublished(t *testing.T) {
	in := withInstallation(t)
	other := in.run(t)
	run := in.run(t)
	item := agk.NewItem(map[string]any{})
	for _, f := range []struct {
		uri     agk.URI
		content string
		size    int64
	}{
		{agk.URI{Run: run, Step: "archive", Port: "out", Name: "ours.bin"}, "ours", 4},
		{agk.URI{Run: other, Step: "archive", Port: "out", Name: "theirs.bin"}, "theirs", 6},
		{agk.URI{Run: run, Step: "render", Port: "out", Name: "nowhere.bin"}, "nowhere", 7},
		{agk.URI{Run: run, Step: "archive", Port: "out", Name: "resized.bin"}, "resized", 7},
		{agk.URI{Run: run, Step: "archive", Port: "out", Name: "going.bin"}, "going", 5},
	} {
		digest, _ := in.put(t, f.content)
		item.Files = append(item.Files, agk.File{Name: f.uri.Name, URI: f.uri, MediaType: "application/octet-stream", Size: f.size, SHA256: digest})
	}
	e := agk.Envelope{Meta: agk.Meta{RunID: run, Step: "archive", Port: "out", Attempt: 1, Count: 1, ProducedAt: time.Now().UTC()}, Items: []agk.Item{item}}
	envelope, size, err := artifact.PutEnvelope(t.Context(), in.store, "finance", e)
	if err != nil {
		t.Fatal(err)
	}
	resized, _ := in.put(t, "resized")
	going, _ := in.put(t, "going")
	in.exec(t,
		`update runs set state = 'succeeded', finished_at = now() - interval '1 day' where id = '`+string(run)+`'`,
		fmt.Sprintf(`update steps set ports = '{"out": {"digest": "sha256:%s", "size": %d, "items": 1}, "gone": {"digest": "sha256:%s", "size": 3, "items": 0}}' where run_id = '%s'`, envelope, size, strings.Repeat("0", 64), run),
		fmt.Sprintf(`insert into artifact_objects (namespace, digest, size_bytes, media_type, refs) values ('finance', 'sha256:%s', 99, 'application/octet-stream', 1)`, resized),
		fmt.Sprintf(`insert into artifact_objects (namespace, digest, size_bytes, media_type, refs, collectable_at, collecting_at)
		             values ('finance', 'sha256:%s', 5, 'application/octet-stream', 0, now() - interval '2 days', now())`, going))

	got, err := purge.Backfill(t.Context(), in.pool, in.store, 0)
	if err != nil || got != (purge.Backfilled{Runs: 1, Artifacts: 1, Unread: 1}) {
		t.Fatalf("Backfill recorded %+v: %v", got, err)
	}
	if n := in.count(t, `select count(*) from artifacts where name = 'ours.bin'`); n != 1 {
		t.Error("the run's own file is not recorded")
	}
	if n := in.count(t, `select count(*) from artifacts where name <> 'ours.bin'`); n != 0 {
		t.Errorf("%d files the run's steps did not publish, or that name other bytes, are recorded", n)
	}
	if in.refs(t, "resized") != 1 || in.refs(t, "going") != 0 {
		t.Error("an object whose size disagrees, or that is being collected, was counted")
	}
	if n := in.count(t, `select count(*) from runs where id = '`+string(run)+`' and files_recorded`); n != 1 {
		t.Error("the run does not say its files are recorded")
	}
}

// cancelling is a store that stops its caller at one key, as a program stopped mid-way is.
type cancelling struct {
	artifact.Removable
	at     string
	cancel context.CancelFunc
}

func (c cancelling) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if key == c.at {
		c.cancel()
		return nil, ctx.Err()
	}
	return c.Removable.Open(ctx, key)
}

// Backfill goes a batch of runs at a time, each batch its own transaction: one stopped in its
// second batch keeps what the first recorded, and the next Backfill records the rest, counting
// nothing twice.
func TestBackfillGoesABatchAtATimeAndACutLosesNothing(t *testing.T) {
	in := withInstallation(t)
	var envelopes []string
	for i := range 5 {
		_, envelope := in.v02(t, 24*time.Hour, file{"out.bin", fmt.Sprintf("output %d", i)})
		envelopes = append(envelopes, envelope)
	}
	// The runs are taken in the order of their identifiers, which is the order they were made in.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stopped := cancelling{Removable: in.store, at: artifact.Key("finance", envelopes[3]), cancel: cancel}

	got, err := purge.Backfill(ctx, in.pool, stopped, 2)
	if !errors.Is(err, context.Canceled) || got.Runs != 2 || got.Artifacts != 2 {
		t.Fatalf("a Backfill stopped in its second batch recorded %+v and said %v", got, err)
	}
	if n := in.count(t, `select count(*) from runs where not files_recorded`); n != 3 {
		t.Errorf("%d runs are still to be recorded, and the three past the first batch should be", n)
	}

	got, err = purge.Backfill(t.Context(), in.pool, in.store, 2)
	if err != nil || got != (purge.Backfilled{Runs: 3, Artifacts: 3}) {
		t.Fatalf("the next Backfill recorded %+v: %v", got, err)
	}
	for i := range 5 {
		if n := in.refs(t, fmt.Sprintf("output %d", i)); n != 1 {
			t.Errorf("output %d is counted %d times", i, n)
		}
	}
}

// A run with an envelope the store will not give back is left waiting, whole, and said, rather
// than ending Backfill, which would keep init failing and every service with it; the other runs
// are recorded, and a Backfill once the store answers records it.
func TestBackfillLeavesARunWhoseEnvelopeTheStoreWillNotGiveBack(t *testing.T) {
	in := withInstallation(t)
	_, broken := in.v02(t, 24*time.Hour, file{"out.bin", "behind a disk that does not answer"})
	in.v02(t, 24*time.Hour, file{"out.bin", "readable"})

	store := refusing{Walkable: in.store.(artifact.Walkable), refuse: map[string]bool{artifact.Key("finance", broken): true}}
	got, err := purge.Backfill(t.Context(), in.pool, store, 0)
	if err != nil || got.Runs != 1 || got.Unreadable != 1 || got.Trouble == nil || !strings.Contains(got.Trouble.Error(), "the disk did not answer") {
		t.Fatalf("with one envelope unreadable, Backfill recorded %+v: %v", got, err)
	}
	if in.refs(t, "behind a disk that does not answer") != -1 {
		t.Error("a file of the run left waiting was recorded")
	}
	if got, err := purge.Backfill(t.Context(), in.pool, in.store, 0); err != nil || got != (purge.Backfilled{Runs: 1, Artifacts: 1}) {
		t.Errorf("once the store answered, Backfill recorded %+v: %v", got, err)
	}
}

// A run whose object a writer holds is left waiting, whole, rather than waited for, since the
// writer may be a decision waiting on what the recording holds; a Backfill once it is free records
// it.
func TestBackfillLeavesARunWhoseObjectAWriterHolds(t *testing.T) {
	in := withInstallation(t)
	in.v02(t, 24*time.Hour, file{"shared.bin", "bytes a decision is counting"}, file{"alone.bin", "bytes nobody else has"})
	digest, _ := in.put(t, "bytes a decision is counting")
	in.exec(t, `insert into artifact_objects (namespace, digest, size_bytes, media_type, refs)
	            values ('finance', 'sha256:`+digest+`', 28, 'application/octet-stream', 1)`)

	writer, err := in.conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(t.Context(), `select 1 from artifact_objects where digest = 'sha256:'||$1 for update`, digest); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	got, err := purge.Backfill(ctx, in.pool, in.store, 0)
	writer.Rollback(t.Context())
	if err != nil || got != (purge.Backfilled{Left: 1}) {
		t.Fatalf("with an object held by a writer, Backfill recorded %+v: %v", got, err)
	}
	if n := in.count(t, `select count(*) from artifacts`); n != 0 {
		t.Errorf("%d files of a run left waiting were recorded", n)
	}

	if got, err := purge.Backfill(t.Context(), in.pool, in.store, 0); err != nil || got != (purge.Backfilled{Runs: 1, Artifacts: 2}) {
		t.Errorf("once the writer let go, Backfill recorded %+v: %v", got, err)
	}
	if in.refs(t, "bytes a decision is counting") != 2 || in.refs(t, "bytes nobody else has") != 1 {
		t.Error("the objects are not counted by the references recorded")
	}
}

// A writer creating the row of an object a run names, as a decision counting the same bytes for
// the first time does, is waited for a moment and no longer: the run is left waiting rather than
// the recording holding what the decision may need next, and a Backfill once the writer is done
// records it.
func TestBackfillWaitsOnlyAMomentForAWriterCreatingAnObject(t *testing.T) {
	in := withInstallation(t)
	in.v02(t, 24*time.Hour, file{"new.bin", "bytes a decision counts for the first time"})
	digest, _ := in.put(t, "bytes a decision counts for the first time")

	writer, err := in.conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(t.Context())
	if _, err := writer.Exec(t.Context(), `insert into artifact_objects (namespace, digest, size_bytes, media_type, refs)
	                                       values ('finance', 'sha256:'||$1, 42, 'application/octet-stream', 1)`, digest); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	began := time.Now()
	got, err := purge.Backfill(ctx, in.pool, in.store, 0)
	if err != nil || got != (purge.Backfilled{Left: 1}) || time.Since(began) > 10*time.Second {
		t.Fatalf("with a writer creating the object's row, Backfill recorded %+v in %s: %v", got, time.Since(began), err)
	}
	if err := writer.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, err := purge.Backfill(t.Context(), in.pool, in.store, 0); err != nil || got != (purge.Backfilled{Runs: 1, Artifacts: 1}) {
		t.Errorf("once the writer was done, Backfill recorded %+v: %v", got, err)
	}
	if n := in.refs(t, "bytes a decision counts for the first time"); n != 2 {
		t.Errorf("the object is counted %d times, by the writer and the recording", n)
	}
}

// A writer that lets go of an object in the moments a Backfill waits between two rounds is gone
// by the next, which records the run it had left.
func TestBackfillRecordsARunOnceAWriterLetsGoOfItsObject(t *testing.T) {
	in := withInstallation(t)
	in.v02(t, 24*time.Hour, file{"shared.bin", "bytes a decision is counting"})
	digest, _ := in.put(t, "bytes a decision is counting")
	in.exec(t, `insert into artifact_objects (namespace, digest, size_bytes, media_type, refs)
	            values ('finance', 'sha256:`+digest+`', 28, 'application/octet-stream', 1)`)

	// The writer's own connection, used by the goroutine letting go of it alone, and waited for
	// before the test ends, since a pgx.Conn is not safe for concurrent use and its cleanup
	// closes it.
	writer, err := dbtest.Superuser(t, in.super).Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(t.Context(), `select 1 from artifact_objects where digest = 'sha256:'||$1 for update`, digest); err != nil {
		t.Fatal(err)
	}
	let := make(chan struct{})
	go func() {
		defer close(let)
		time.Sleep(300 * time.Millisecond)
		writer.Rollback(context.WithoutCancel(t.Context()))
	}()
	got, err := purge.Backfill(t.Context(), in.pool, in.store, 0)
	<-let
	if err != nil || got != (purge.Backfilled{Runs: 1, Artifacts: 1}) {
		t.Errorf("with a writer letting go within a round's pause, Backfill recorded %+v: %v", got, err)
	}
}

// A pass records a batch of runs a call and calls again while the batch comes back full, up to its
// bound of calls, leaving the rest to the next pass.
func TestAPassRecordsABatchOfRunsACall(t *testing.T) {
	in := withInstallation(t)
	for i := range 5 {
		in.v02(t, 24*time.Hour, file{"out.bin", fmt.Sprintf("output %d", i)})
	}
	p := in.purger(2, 2)
	if got := in.pass(t, p); got.Recorded != 4 {
		t.Errorf("a pass of two calls of two runs recorded %d runs", got.Recorded)
	}
	if got := in.pass(t, p); got.Recorded != 1 {
		t.Errorf("the next pass recorded %d runs, and one was left", got.Recorded)
	}
}

// The files of every envelope a v0.2 run keeps are recorded, the envelope of a shard whose step
// never published included, as a decision of this release records them, so that none of them is
// taken for an orphan while the run keeps the envelope naming it.
func TestBackfillRecordsTheFilesOfEveryEnvelopeARunKeeps(t *testing.T) {
	in := withInstallation(t)
	run, published := in.v02(t, 24*time.Hour, file{"out.bin", "published"})
	in.put(t, "a shard's output")
	shard, size := in.named(t, run, file{"shard.bin", "a shard's output"})
	in.exec(t, fmt.Sprintf(`update runs set evaluation = '{"version": 1, "envelopes": [
	            {"step": "archive", "shard": -1, "port": "out", "digest": "%s", "size": 1},
	            {"step": "archive", "shard": 0, "port": "out", "digest": "%s", "size": %d}]}' where id = '%s'`, published, shard, size, run))

	got, err := purge.Backfill(t.Context(), in.pool, in.store, 0)
	if err != nil || got != (purge.Backfilled{Runs: 1, Artifacts: 2}) {
		t.Fatalf("Backfill recorded %+v: %v", got, err)
	}
	if in.refs(t, "a shard's output") != 1 || in.refs(t, "published") != 1 {
		t.Error("the files of the run's envelopes are not each counted once")
	}
}
