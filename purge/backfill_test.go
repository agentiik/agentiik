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
	"github.com/agentiik/agentiik/purge"
)

// Backfill: the files of the runs v0.2 finished, which v0.2 recorded no reference for, recorded
// from the envelopes their steps published, as migration 0039 lists those runs.

// v02 is a run of finance as v0.2 left it: finished age ago, its step archive having published on
// port out an envelope naming files, whose bytes are in the store and which nothing counts, and
// waiting for them to be recorded. It answers the run and the envelope's digest.
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
		fmt.Sprintf(`update steps set ports = '{"out": {"digest": "sha256:%s", "size": %d, "items": 1}}' where run_id = '%s'`, envelope, size, run),
		`insert into artifacts_unrecorded (namespace, run_id) values ('finance', '`+string(run)+`')`)
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
// for an output given a retain, is left with its own expiry and count; the run is waited for no
// longer; and a second Backfill records nothing and counts nothing again.
func TestBackfillRecordsWhatV02LeftAndNothingTwice(t *testing.T) {
	in := withInstallation(t)
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
	if n := in.count(t, `select count(*) from artifacts_unrecorded`); n != 0 {
		t.Errorf("%d runs are still waiting to be recorded", n)
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
		             values ('finance', 'sha256:%s', 5, 'application/octet-stream', 0, now() - interval '2 days', now())`, going),
		`insert into artifacts_unrecorded (namespace, run_id) values ('finance', '`+string(run)+`')`)

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
	if n := in.count(t, `select count(*) from artifacts_unrecorded`); n != 0 {
		t.Error("the run is still waited for")
	}
}

// refusing is a store whose objects under one namespace's keys cannot be read for a while, as a
// disk that stopped answering.
type refusing struct {
	artifact.Removable
	refuse map[string]bool
}

func (r refusing) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if r.refuse[key] {
		return nil, errors.New("the disk did not answer")
	}
	return r.Removable.Open(ctx, key)
}

// Backfill goes a batch of runs at a time, each batch its own transaction: one cut short by an
// envelope the store would not read keeps what the batches before it recorded, and the next
// Backfill records the rest, counting nothing twice.
func TestBackfillGoesABatchAtATimeAndACutLosesNothing(t *testing.T) {
	in := withInstallation(t)
	var envelopes []string
	for i := range 5 {
		_, envelope := in.v02(t, 24*time.Hour, file{"out.bin", fmt.Sprintf("output %d", i)})
		envelopes = append(envelopes, envelope)
	}
	// The runs are taken in the order of their identifiers, which is the order they were made in.
	broken := refusing{Removable: in.store, refuse: map[string]bool{artifact.Key("finance", envelopes[3]): true}}

	got, err := purge.Backfill(t.Context(), in.pool, broken, 2)
	if err == nil || got.Runs != 2 || got.Artifacts != 2 {
		t.Fatalf("a Backfill cut short in its second batch recorded %+v and said %v", got, err)
	}
	if n := in.count(t, `select count(*) from artifacts_unrecorded`); n != 3 {
		t.Errorf("%d runs are waiting, and the three past the first batch should be", n)
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
