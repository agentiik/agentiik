package purge_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/purge"
)

// The orphan sweep: a file of the store no row names is handed to the collection once it is older
// than the grace, unless a write holds it or a run still under way names it, and collected a grace
// later; and nothing but the objects of a namespace's sha256 directory is ever looked at.

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

// An orphan older than the grace is handed to the collection, and deleted a grace later, counted as
// an orphan when it is handed over and as an object when it goes; one written within the grace
// stays, since its writer may be about to record it; and a pass after that finds nothing.
func TestAnOrphanOlderThanTheGraceGoesAndAYoungerOneStays(t *testing.T) {
	in := withInstallation(t)
	_, old := in.aged(t, "an attempt that failed", 25*time.Hour)
	_, young := in.aged(t, "a write about to be recorded", 23*time.Hour)

	p := in.purger(0, 0)
	if got, want := in.pass(t, p), (purge.Purged{Orphans: 1}); got != want || !got.Removed() {
		t.Fatalf("the pass that found the orphan removed %+v, want %+v, which says it removed something", got, want)
	}
	if !in.held(t, old) {
		t.Fatal("the orphan went in the pass that found it, before the grace a write that could not see it is given")
	}
	in.past(t)
	if got, want := in.pass(t, p), (purge.Purged{Objects: 1, Bytes: int64(len("an attempt that failed"))}); got != want {
		t.Fatalf("the pass a grace later removed %+v, want %+v", got, want)
	}
	if in.held(t, old) || !in.held(t, young) {
		t.Errorf("a grace later the orphan is held %t, the file written within the grace %t", in.held(t, old), in.held(t, young))
	}
	if n := in.count(t, `select count(*) from artifact_objects`); n != 0 {
		t.Errorf("%d rows are left in artifact_objects, and the orphan's row goes with its bytes", n)
	}
	if got := in.pass(t, p); got.Removed() {
		t.Errorf("a pass after it removed %+v", got)
	}
}

// A write of an orphan's bytes that began before the orphan was handed over, and could neither see
// its row nor be seen, has committed by the time the orphan is collectable, and holds it; the bytes
// it wrote stay.
func TestAWriteBegunBeforeAnOrphanWasHandedOverKeepsIt(t *testing.T) {
	in := withInstallation(t)
	digest, key := in.aged(t, "the same bytes as yesterday's attempt", 48*time.Hour)

	recorded, commit := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(commit) }) }
	// Let go of the write whichever way the test ends, since a pool closing waits on its
	// connection.
	defer release()
	done := make(chan error, 1)
	go func() {
		done <- in.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
			if _, err := ns.Uploading(ctx, digest, time.Now().Add(time.Hour)); err != nil {
				return err
			}
			close(recorded)
			<-commit
			return nil
		})
	}()
	<-recorded

	p := in.purger(0, 0)
	if got := in.pass(t, p); got.Orphans != 1 || got.Objects != 0 || !in.held(t, key) {
		t.Fatalf("with a write under way that it cannot see, the pass removed %+v, and the file is held %t", got, in.held(t, key))
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := in.store.Put(t.Context(), key, strings.NewReader("the same bytes as yesterday's attempt")); err != nil {
		t.Fatal(err)
	}
	in.past(t)
	if got := in.pass(t, p); got.Objects != 0 || !in.held(t, key) {
		t.Errorf("a grace later, with the write held, the pass removed %+v, and the file is held %t", got, in.held(t, key))
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
	if got := in.pass(t, p); got.Orphans != 0 {
		t.Fatalf("a file a write holds: the pass removed %+v", got)
	}
	in.exec(t, `update artifact_uploads set until = now() - interval '1 second'`)
	if got := in.pass(t, p); got.Uploads != 1 || got.Orphans != 1 {
		t.Errorf("once the write lapsed the pass removed %+v", got)
	}
	in.past(t)
	if got := in.pass(t, p); got.Objects != 1 || in.held(t, key) {
		t.Errorf("a grace later the pass removed %+v, and the file is held %t", got, in.held(t, key))
	}
}

// A file an envelope of a run still under way names stays, since the step that wrote it has not
// published yet and nothing else names it; once the run has finished without recording it, as a
// step that failed leaves its shards' files, it goes. The envelope stays, counted by its run.
func TestAFileARunUnderWayNamesStays(t *testing.T) {
	in := withInstallation(t)
	run := in.run(t)
	_, key := in.aged(t, "a shard's output", 48*time.Hour)
	envelope, size := in.named(t, run, file{"out.bin", "a shard's output"})
	in.working(t, run, envelope, size)
	in.age(t, artifact.Key("finance", envelope), 48*time.Hour)

	p := in.purger(0, 0)
	if got := in.pass(t, p); got.Orphans != 0 {
		t.Fatalf("the file a run under way names: the pass removed %+v", got)
	}
	in.finish(t, run, false)
	if got := in.pass(t, p); got.Orphans != 1 {
		t.Fatalf("once its run finished the pass removed %+v", got)
	}
	in.past(t)
	in.pass(t, p)
	if in.held(t, key) || !in.held(t, artifact.Key("finance", envelope)) {
		t.Errorf("a grace later the file is held %t, its envelope %t", in.held(t, key), in.held(t, artifact.Key("finance", envelope)))
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

	p := in.purger(0, 0)
	for range 2 {
		if got := in.pass(t, p); got.Removed() {
			t.Errorf("the pass removed %+v", got)
		}
		in.past(t)
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

// opening is a store counting the walks opened on it.
type opening struct {
	artifact.Walkable
	opened int
}

func (o *opening) Walk(namespace string) (artifact.Walk, error) {
	o.opened++
	return o.Walkable.Walk(namespace)
}

// The store is walked a batch of entries a call and a bound of calls a pass, and the next pass goes
// on from where the last stopped rather than from the start: a namespace's walk is opened once a
// round, and a round begins again once the last has ended.
func TestTheStoreIsWalkedABatchAtATime(t *testing.T) {
	in := withInstallation(t)
	for i := range 5 {
		in.aged(t, fmt.Sprintf("orphan %d", i), 48*time.Hour)
	}

	p := in.purger(2, 1)
	store := &opening{Walkable: in.store.(artifact.Walkable)}
	p.Objects = store
	for i, want := range []int{2, 2, 1, 0} {
		if got := in.pass(t, p); got.Orphans != want {
			t.Errorf("pass %d handed over %d orphans, want %d", i+1, got.Orphans, want)
		}
	}
	if store.opened != 1 {
		t.Errorf("the namespace was walked from its start %d times in one round", store.opened)
	}
	if n := in.count(t, `select count(*) from artifact_objects where refs = 0`); n != 5 {
		t.Errorf("%d orphans were handed over in the round, and the store holds 5", n)
	}
	in.pass(t, p)
	if store.opened != 2 {
		t.Errorf("the pass after a round ended opened %d walks in all, and it begins the next round", store.opened)
	}
}

// refusing is a store whose objects under some keys cannot be read for a while, as a disk that
// stopped answering.
type refusing struct {
	artifact.Walkable
	refuse map[string]bool
}

func (r refusing) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if r.refuse[key] {
		return nil, errors.New("the disk did not answer")
	}
	return r.Walkable.Open(ctx, key)
}

// A file of a run v0.2 finished, which no row names until its files are recorded, is recorded by
// the pass rather than taken for an orphan; and while a run of the namespace cannot be recorded,
// its envelope unreadable, no file of the namespace is taken for one, and the pass says why.
func TestAFileOfARunStillToBeRecordedIsNeverAnOrphan(t *testing.T) {
	in := withInstallation(t)
	_, envelope := in.v02(t, 24*time.Hour, file{"out.bin", "a file v0.2 recorded nothing for"})
	_, key := in.put(t, "a file v0.2 recorded nothing for")
	in.age(t, key, 48*time.Hour)
	_, orphan := in.aged(t, "an attempt that failed", 48*time.Hour)

	p := in.purger(0, 0)
	p.Objects = refusing{Walkable: in.store.(artifact.Walkable), refuse: map[string]bool{artifact.Key("finance", envelope): true}}
	got, err := p.Pass(t.Context())
	if err == nil || !strings.Contains(err.Error(), "the files v0.2 left") || got.Orphans != 0 {
		t.Fatalf("with the run's envelope unreadable, the pass removed %+v and said %v", got, err)
	}
	p.Objects = in.store
	if got := in.pass(t, p); got.Recorded != 1 || got.Orphans != 1 {
		t.Fatalf("once the envelope reads, the pass removed %+v, and the run was to be recorded and the orphan handed over", got)
	}
	in.past(t)
	in.pass(t, p)
	if !in.held(t, key) || in.held(t, orphan) {
		t.Errorf("a grace later the recorded file is held %t, the orphan %t", in.held(t, key), in.held(t, orphan))
	}
}

// A pass cut short after handing an orphan to the collection, by a controller that stopped leading,
// leaves a row the next leader's pass collects in its time; and an orphan referenced in between, by
// a result heard at last, stays.
func TestAnOrphanHandedOverIsFinishedByTheNextLeader(t *testing.T) {
	in := withInstallation(t)
	_, first := in.aged(t, "handed over, then collected", 48*time.Hour)
	second, kept := in.aged(t, "handed over, then referenced", 48*time.Hour)

	var calls int
	p := in.purger(0, 0)
	p.Leading = func(context.Context) error {
		calls++
		// The purges before the sweep each make one call on a store with nothing else to do, and
		// the sweep one a namespace and one more: the collection's is refused.
		if calls > 8 {
			return errors.New("another controller leads")
		}
		return nil
	}
	got, err := p.Pass(t.Context())
	if err == nil || got.Orphans != 2 {
		t.Fatalf("the pass cut short removed %+v and said %v", got, err)
	}
	if in.count(t, `select count(*) from artifact_objects where refs = 0 and collectable_at is not null`) != 2 {
		t.Fatal("the orphans handed over are not left as rows the collection takes")
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

	in.past(t)
	next := in.purger(0, 0)
	if got := in.pass(t, next); got.Orphans != 0 || got.Objects != 1 || in.held(t, first) || !in.held(t, kept) {
		t.Errorf("the next leader's pass removed %+v; the first orphan is held %t, the one referenced since %t", got, in.held(t, first), in.held(t, kept))
	}
}

// failingWalk is a store one namespace of which cannot be walked.
type failingWalk struct {
	artifact.Walkable
	namespace string
}

func (f failingWalk) Walk(namespace string) (artifact.Walk, error) {
	if namespace == f.namespace {
		return nil, errors.New("the directory cannot be listed")
	}
	return f.Walkable.Walk(namespace)
}

// A namespace whose objects cannot be walked is said, and the round goes on to the next at the
// next pass rather than stopping there; the round after tries the first again.
func TestANamespaceThatCannotBeWalkedHoldsUpNoOther(t *testing.T) {
	in := withInstallation(t)
	in.exec(t, `insert into namespaces (name) values ('team-ops')`)
	stuck := artifact.Key("finance", fmt.Sprintf("%064x", 1))
	walked := artifact.Key("team-ops", fmt.Sprintf("%064x", 2))
	for _, key := range []string{stuck, walked} {
		if err := in.store.Put(t.Context(), key, strings.NewReader("an orphan")); err != nil {
			t.Fatal(err)
		}
		in.age(t, key, 48*time.Hour)
	}

	p := in.purger(0, 0)
	p.Objects = failingWalk{Walkable: in.store.(artifact.Walkable), namespace: "finance"}
	got, err := p.Pass(t.Context())
	if err == nil || !strings.Contains(err.Error(), "namespace finance could not be walked") || got.Orphans != 0 {
		t.Fatalf("with finance unwalkable, the pass removed %+v and said %v", got, err)
	}
	if got := in.pass(t, p); got.Orphans != 1 || in.count(t, `select count(*) from artifact_objects where namespace = 'team-ops'`) != 1 {
		t.Errorf("the pass after it handed over %d orphans, and the one of the namespace after finance was due", got.Orphans)
	}
	p.Objects = in.store
	if got := in.pass(t, p); got.Orphans != 1 || in.count(t, `select count(*) from artifact_objects where namespace = 'finance'`) != 1 {
		t.Errorf("the next round, finance walkable again, handed over %d orphans", got.Orphans)
	}
}
