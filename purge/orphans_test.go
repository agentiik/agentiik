package purge_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/purge"
)

// The orphan sweep: a file of the store no row names is handed to the collection once it is older
// than the grace, unless a write holds it or a run still under way names it, and nothing but the
// objects of a namespace's sha256 directory is ever looked at.

// age dates the file under key age ago, as a file written then is dated.
func (in *installation) age(t *testing.T, key string, age time.Duration) {
	t.Helper()
	at := time.Now().Add(-age)
	if err := os.Chtimes(filepath.Join(in.dir, filepath.FromSlash(key)), at, at); err != nil {
		t.Fatal(err)
	}
}

// aged writes content to the store, dated age ago, and answers its digest and key.
func (in *installation) aged(t *testing.T, content string, age time.Duration) (string, string) {
	t.Helper()
	digest, key := in.put(t, content)
	in.age(t, key, age)
	return digest, key
}

// file is one file an envelope names, by its name and its bytes.
type file struct{ name, content string }

// named writes an envelope of run's step archive on port out whose one item names files, counted
// once as a decision counts what it references, and answers its digest and size.
func (in *installation) named(t *testing.T, run agk.RunID, files ...file) (string, int64) {
	t.Helper()
	item := agk.NewItem(map[string]any{})
	for _, f := range files {
		sum := sha256.Sum256([]byte(f.content))
		item.Files = append(item.Files, agk.File{
			Name: f.name, URI: agk.URI{Run: run, Step: "archive", Port: "out", Name: f.name},
			MediaType: "application/octet-stream", Size: int64(len(f.content)), SHA256: hex.EncodeToString(sum[:]),
		})
	}
	e := agk.Envelope{Meta: agk.Meta{RunID: run, Step: "archive", Port: "out", Attempt: 1, Count: 1, ProducedAt: time.Now().UTC()}, Items: []agk.Item{item}}
	digest, size, err := artifact.PutEnvelope(t.Context(), in.store, "finance", e)
	if err != nil {
		t.Fatal(err)
	}
	in.exec(t, fmt.Sprintf(`insert into artifact_objects (namespace, digest, size_bytes, media_type, refs)
	            values ('finance', 'sha256:%s', %d, 'application/json', 1)`, digest, size))
	return digest, size
}

// working makes digest an envelope run's decision references as the first shard of its step
// archive, as a fan-out's shard is referenced before its step publishes.
func (in *installation) working(t *testing.T, run agk.RunID, digest string, size int64) {
	t.Helper()
	in.exec(t, fmt.Sprintf(`update runs set evaluation = '{"version": 1, "envelopes": [{"step": "archive", "shard": 0, "port": "out", "digest": "%s", "size": %d}]}'
	            where id = '%s'`, digest, size, run))
}

// An orphan older than the grace is handed to the collection and deleted in the pass that finds it,
// counted as an orphan and as an object; one written within the grace stays, since its writer may
// be about to record it; and a pass after that finds nothing.
func TestAnOrphanOlderThanTheGraceGoesAndAYoungerOneStays(t *testing.T) {
	in := withInstallation(t)
	_, old := in.aged(t, "an attempt that failed", 25*time.Hour)
	_, young := in.aged(t, "a write about to be recorded", 23*time.Hour)

	p := in.purger(0, 0)
	want := purge.Purged{Orphans: 1, Objects: 1, Bytes: int64(len("an attempt that failed"))}
	if got := in.pass(t, p); got != want {
		t.Fatalf("the pass removed %+v, want %+v", got, want)
	}
	if in.held(t, old) || !in.held(t, young) {
		t.Errorf("after the pass the orphan past the grace is held %t, the one within it %t", in.held(t, old), in.held(t, young))
	}
	if n := in.count(t, `select count(*) from artifact_objects`); n != 0 {
		t.Errorf("%d rows are left in artifact_objects, and the orphan's row goes with its bytes", n)
	}
	if got := in.pass(t, p); got.Removed() {
		t.Errorf("a pass after it removed %+v", got)
	}
}

// A file a write holds stays however old it is, since the result that references it may not have
// been heard yet; once the write has lapsed it is an orphan like any other.
func TestAnOrphanAWriteHoldsStays(t *testing.T) {
	in := withInstallation(t)
	digest, key := in.aged(t, "uploaded by a runner", 48*time.Hour)
	if err := in.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		_, err := ns.Uploading(ctx, digest, time.Now().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	p := in.purger(0, 0)
	if got := in.pass(t, p); got.Orphans != 0 || !in.held(t, key) {
		t.Fatalf("a file a write holds: the pass removed %+v, and it is held %t", got, in.held(t, key))
	}
	in.exec(t, `update artifact_uploads set until = now() - interval '1 second'`)
	if got := in.pass(t, p); got.Uploads != 1 || got.Orphans != 1 || got.Objects != 1 || in.held(t, key) {
		t.Errorf("once the write lapsed the pass removed %+v, and the file is held %t", got, in.held(t, key))
	}
}

// A file an envelope of a run still under way names stays, since the step that wrote it has not
// published yet and nothing else names it; once the run has finished without recording it, as a
// step that failed leaves its shards' files, it goes. The envelope stays, counted by its run.
func TestAFileARunUnderWayNamesStays(t *testing.T) {
	in := withInstallation(t)
	run := in.run(t)
	shard, key := in.aged(t, "a shard's output", 48*time.Hour)
	envelope, size := in.named(t, run, file{"out.bin", "a shard's output"})
	in.working(t, run, envelope, size)
	in.age(t, artifact.Key("finance", envelope), 48*time.Hour)

	p := in.purger(0, 0)
	if got := in.pass(t, p); got.Orphans != 0 || !in.held(t, key) {
		t.Fatalf("the file a run under way names: the pass removed %+v, and it is held %t", got, in.held(t, key))
	}
	in.finish(t, run, false)
	if got := in.pass(t, p); got.Orphans != 1 || in.held(t, key) || !in.held(t, artifact.Key("finance", envelope)) {
		t.Errorf("once its run finished the pass removed %+v; the file %s is held %t, its envelope %t", got, shard, in.held(t, key), in.held(t, artifact.Key("finance", envelope)))
	}
}

// An envelope of a run under way that cannot be read keeps every orphan of its namespace, since
// the files it names could be any of them, and the pass says why.
func TestAnEnvelopeThatCannotBeReadKeepsItsNamespacesOrphans(t *testing.T) {
	in := withInstallation(t)
	run := in.run(t)
	_, key := in.aged(t, "an orphan, or a file of the run", 48*time.Hour)
	in.working(t, run, strings.Repeat("d", 64), 10)

	got, err := in.purger(0, 0).Pass(t.Context())
	if err == nil || !strings.Contains(err.Error(), "the orphan sweep") || got.Orphans != 0 || !in.held(t, key) {
		t.Errorf("with an envelope gone from under a run, the pass removed %+v and said %v, and the file is held %t", got, err, in.held(t, key))
	}
}

// The sweep looks at the objects of a namespace's sha256 directory and at nothing else: a log with
// no row, a write being staged, a name that is not a digest, a directory, a namespace the database
// does not have and a file beside the namespaces all stay, however old.
func TestTheSweepTouchesNothingButObjects(t *testing.T) {
	in := withInstallation(t)
	var kept []string
	for _, key := range []string{
		"finance/logs/01JMZ8V1P9C4XQ7K2N4D6F8H0A/task/1/0000000001-" + strings.Repeat("a", 64),
		"finance/sha256/.staging-4242",
		"finance/sha256/" + strings.Repeat("A", 64),
		"finance/other/" + strings.Repeat("b", 64),
		"gone/sha256/" + strings.Repeat("c", 64),
		"stray.txt",
	} {
		if err := in.store.Put(t.Context(), key, strings.NewReader("not an orphan")); err != nil {
			t.Fatal(err)
		}
		in.age(t, key, 30*24*time.Hour)
		kept = append(kept, key)
	}
	if err := os.Mkdir(filepath.Join(in.dir, "finance", "sha256", strings.Repeat("e", 64)), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := in.pass(t, in.purger(0, 0)); got.Removed() {
		t.Errorf("the pass removed %+v", got)
	}
	for _, key := range kept {
		if !in.held(t, key) {
			t.Errorf("%s went", key)
		}
	}
	if _, err := os.Stat(filepath.Join(in.dir, "finance", "sha256", strings.Repeat("e", 64))); err != nil {
		t.Errorf("a directory named as a digest went: %s", err)
	}
}

// The store is walked a batch of entries a call and a bound of calls a pass, and the next pass goes
// on from where the last stopped rather than from the start.
func TestTheStoreIsWalkedABatchAtATime(t *testing.T) {
	in := withInstallation(t)
	var keys []string
	for i := range 5 {
		_, key := in.aged(t, fmt.Sprintf("orphan %d", i), 48*time.Hour)
		keys = append(keys, key)
	}
	left := func() int {
		n := 0
		for _, key := range keys {
			if in.held(t, key) {
				n++
			}
		}
		return n
	}

	p := in.purger(2, 1)
	for i, want := range []struct{ removed, left int }{{2, 3}, {2, 1}, {1, 0}} {
		if got := in.pass(t, p); got.Orphans != want.removed || got.Objects != want.removed {
			t.Errorf("pass %d removed %+v, and %d orphans were due", i+1, got, want.removed)
		}
		if n := left(); n != want.left {
			t.Fatalf("after pass %d, %d orphans are left, want %d", i+1, n, want.left)
		}
	}
	if got := in.pass(t, p); got.Removed() {
		t.Errorf("the pass after the store was walked through removed %+v", got)
	}
}

// A namespace whose runs of v0.2 still wait for their files to be recorded is not swept, since
// those files are named by no row until they are.
func TestANamespaceWaitingForItsV02FilesIsNotSwept(t *testing.T) {
	in := withInstallation(t)
	run := in.run(t)
	in.finish(t, run, false)
	in.exec(t, `insert into artifacts_unrecorded (namespace, run_id) values ('finance', '`+string(run)+`')`)
	_, key := in.aged(t, "a v0.2 file not recorded yet", 30*24*time.Hour)

	p := in.purger(0, 0)
	if got := in.pass(t, p); got.Orphans != 0 || !in.held(t, key) {
		t.Fatalf("a file of a namespace waiting for its v0.2 files: the pass removed %+v, and it is held %t", got, in.held(t, key))
	}
	in.exec(t, `delete from artifacts_unrecorded`)
	if got := in.pass(t, p); got.Orphans != 1 || in.held(t, key) {
		t.Errorf("once recorded, the pass removed %+v, and the file is held %t", got, in.held(t, key))
	}
}

// A pass cut short after handing an orphan to the collection and before collecting it, by a
// controller that stopped leading, leaves a row the next leader's pass collects; and an orphan
// referenced in between, by a result heard at last, stays.
func TestAnOrphanHandedOverIsFinishedByTheNextPass(t *testing.T) {
	in := withInstallation(t)
	_, first := in.aged(t, "handed over, then collected", 48*time.Hour)
	second, kept := in.aged(t, "handed over, then referenced", 48*time.Hour)

	var calls int
	p := in.purger(0, 0)
	p.Leading = func(context.Context) error {
		calls++
		// The purges before the sweep each make one call on a store with nothing else to do, and
		// the sweep one a namespace and one more: the collection's is refused.
		if calls > 7 {
			return errors.New("another controller leads")
		}
		return nil
	}
	got, err := p.Pass(t.Context())
	if err == nil || got.Orphans != 2 || got.Objects != 0 {
		t.Fatalf("the pass cut short removed %+v and said %v", got, err)
	}
	if !in.held(t, first) || !in.held(t, kept) || in.count(t, `select count(*) from artifact_objects where refs = 0 and collectable_at is not null`) != 2 {
		t.Fatal("the orphans handed over are not left as rows the collection takes, with their bytes")
	}

	run := in.run(t)
	if err := in.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		_, err := ns.WriteArtifact(ctx, db.Reference{
			URI:    agk.URI{Run: run, Step: "archive", Port: "out", Name: "late.bin"},
			Digest: second, Size: int64(len("handed over, then referenced")),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	next := in.purger(0, 0)
	if got := in.pass(t, next); got.Orphans != 0 || got.Objects != 1 || in.held(t, first) || !in.held(t, kept) {
		t.Errorf("the next leader's pass removed %+v; the first orphan is held %t, the one referenced since %t", got, in.held(t, first), in.held(t, kept))
	}
}
