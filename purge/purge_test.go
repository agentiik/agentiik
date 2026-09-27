package purge_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/agentiik/agentiik/purge"
	"github.com/jackc/pgx/v5"
)

// The purges and the collection against a real PostgreSQL and a directory standing for
// AGK_OBJECTS_DIR: what has run out goes and nothing else does, an object being written or named
// stays, a backlog is taken a batch at a time, only the controller that leads purges, and a pass
// that died halfway is finished by the next.

// installation is one namespace, finance, with the database behind its policies and the store.
type installation struct {
	pool  *db.Pool
	conn  *pgx.Conn
	dir   string
	store artifact.Removable
}

func withInstallation(t *testing.T) *installation {
	t.Helper()
	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	in := &installation{pool: pool, conn: conn, dir: t.TempDir()}
	in.store = artifact.Dir(in.dir)
	in.exec(t,
		`insert into namespaces (name) values ('finance')`,
		`insert into workflows (namespace, name) values ('finance', 'monthly-invoicing')`,
		`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at)
		   values ('finance', 'monthly-invoicing', 'a3f9c1e', '{}', 'alice', now())`)
	return in
}

func (in *installation) exec(t *testing.T, stmts ...string) {
	t.Helper()
	for _, stmt := range stmts {
		if _, err := in.conn.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
}

// purger is a purger of the installation, bounded to batch rows and runs a call and calls calls a
// pass, the defaults where zero.
func (in *installation) purger(batch, calls int) *purge.Purger {
	return &purge.Purger{Pool: in.pool, Objects: in.store, Batch: batch, Runs: batch, Calls: calls}
}

// pass runs one pass and fails the test on an error.
func (in *installation) pass(t *testing.T, p *purge.Purger) purge.Purged {
	t.Helper()
	purged, err := p.Pass(t.Context())
	if err != nil {
		t.Fatalf("the pass failed, having removed %+v: %s", purged, err)
	}
	return purged
}

// run is a run of finance with a step archive, running until finish ends it.
func (in *installation) run(t *testing.T) agk.RunID {
	t.Helper()
	id := agk.NewRunID()
	in.exec(t,
		`insert into runs (namespace, id, workflow, commit, trigger, state, started_at)
		   values ('finance', '`+string(id)+`', 'monthly-invoicing', 'a3f9c1e', 'manual', 'running', now() - interval '2 days')`,
		`insert into steps (namespace, run_id, step) values ('finance', '`+string(id)+`', 'archive')`)
	return id
}

// finish ends a run, and puts its retention a minute in the past where expired, a day ahead
// otherwise.
func (in *installation) finish(t *testing.T, run agk.RunID, expired bool) {
	t.Helper()
	expires := "now() + interval '1 day'"
	if expired {
		expires = "now() - interval '1 minute'"
	}
	in.exec(t, `update runs set state = 'succeeded', finished_at = now() - interval '1 day', expires_at = `+expires+`
	            where id = '`+string(run)+`'`)
}

// put writes content to the store and answers its digest and key.
func (in *installation) put(t *testing.T, content string) (string, string) {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	digest := hex.EncodeToString(sum[:])
	key := artifact.Key("finance", digest)
	if err := in.store.Put(t.Context(), key, strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	return digest, key
}

// artifact writes content as an artifact of run named name, living an hour, or past its retain
// where expired, and answers its key.
func (in *installation) artifact(t *testing.T, run agk.RunID, name, content string, expired bool) string {
	t.Helper()
	digest, key := in.put(t, content)
	if err := in.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		_, err := ns.WriteArtifact(ctx, db.Reference{
			URI:    agk.URI{Run: run, Step: "archive", Port: "out", Name: name},
			Digest: digest, Size: int64(len(content)), For: time.Hour,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if expired {
		in.exec(t, `update artifacts set expires_at = now() - interval '1 minute'
		            where run_id = '`+string(run)+`' and name = '`+name+`'`)
	}
	return key
}

// envelope writes content as the envelope run's step archive published on port ok, through a
// decision as the controller writes one, and answers its key. The run must not have finished.
func (in *installation) envelope(t *testing.T, run agk.RunID, content string) string {
	t.Helper()
	digest, key := in.put(t, content)
	ref := map[string]any{"step": "archive", "shard": db.PublishedByTheStep, "port": "ok", "digest": digest, "size": len(content)}
	document, err := json.Marshal(map[string]any{"version": 1, "envelopes": []any{ref}})
	if err != nil {
		t.Fatal(err)
	}
	if err := in.pool.Installation(t.Context(), db.ControllerSweep, func(ctx context.Context, w *db.Wide) error {
		return w.SaveDecision(ctx, db.Decision{
			Namespace: "finance", Run: run, Was: 0, Seq: 1,
			Document: document, State: agk.Running, StartedAt: time.Now().UTC().Add(-48 * time.Hour),
			Envelopes: []db.EnvelopeRef{{Step: "archive", Shard: db.PublishedByTheStep, Port: "ok", Digest: digest, Size: int64(len(content)), Items: 1}},
		})
	}); err != nil {
		t.Fatal(err)
	}
	return key
}

// log gives run a task whose log was written in chunks objects, each in the store, and answers
// their keys.
func (in *installation) log(t *testing.T, run agk.RunID, chunks int) []string {
	t.Helper()
	task := ulid.New()
	in.exec(t, `insert into tasks (namespace, id, run_id, step, attempt, state, log_uri, log_lines)
	            values ('finance', '`+task+`', '`+string(run)+`', 'archive', 1, 'succeeded',
	                    'agk://log/`+string(run)+`/`+string(run)+`%2Farchive%2F1', `+fmt.Sprint(chunks)+`)`)
	var keys []string
	for i := range chunks {
		key := fmt.Sprintf("finance/logs/%s/%s%%2Farchive%%2F1/%s/%010d-%064d", run, run, task, i+1, i)
		if err := in.store.Put(t.Context(), key, strings.NewReader(fmt.Sprintf("line %d\n", i))); err != nil {
			t.Fatal(err)
		}
		in.exec(t, `insert into task_log_objects (namespace, task_id, object_key) values ('finance', '`+task+`', '`+key+`')`)
		keys = append(keys, key)
	}
	return keys
}

// past puts every object whose count is zero past the grace.
func (in *installation) past(t *testing.T) {
	t.Helper()
	in.exec(t, `update artifact_objects set collectable_at = now() - interval '2 days' where collectable_at is not null`)
}

// held says whether the store holds key.
func (in *installation) held(t *testing.T, key string) bool {
	t.Helper()
	ok, err := in.store.Has(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// count answers what a query counting rows answers.
func (in *installation) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := in.conn.QueryRow(t.Context(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// What has run out goes, the references and a run's envelopes and logs, and the objects nothing
// references once the grace has passed. A run still within its retention keeps all of it, and a
// pass with nothing left to do removes nothing.
func TestAPassRemovesWhatHasRunOutAndNothingElse(t *testing.T) {
	in := withInstallation(t)

	old := in.run(t)
	oldEnvelope := in.envelope(t, old, `{"old":true}`)
	oldArtifact := in.artifact(t, old, "old.bin", "old bytes", true)
	oldLog := in.log(t, old, 2)
	in.finish(t, old, true)

	young := in.run(t)
	youngEnvelope := in.envelope(t, young, `{"young":true}`)
	youngArtifact := in.artifact(t, young, "young.bin", "young bytes", false)
	youngLog := in.log(t, young, 1)
	in.finish(t, young, false)

	p := in.purger(0, 0)
	if got, want := in.pass(t, p), (purge.Purged{Artifacts: 1, Runs: 1, Logs: 1}); got != want {
		t.Fatalf("the first pass removed %+v, want %+v", got, want)
	}
	for _, key := range oldLog {
		if in.held(t, key) {
			t.Errorf("the log chunk %s of a run past its retention is still in the store", key)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(in.dir, "finance", "logs")); err != nil || len(entries) != 1 || entries[0].Name() != string(young) {
		t.Errorf("the directories of the logs left are %v (%v), and only the young run's should be", entries, err)
	}
	// Within the grace, nothing is collected.
	for _, key := range append([]string{oldEnvelope, oldArtifact, youngEnvelope, youngArtifact}, youngLog...) {
		if !in.held(t, key) {
			t.Errorf("%s went on the first pass", key)
		}
	}
	if n := in.count(t, `select count(*) from runs where envelopes_purged_at is not null and logs_purged_at is not null and id = '`+string(old)+`'`); n != 1 {
		t.Error("the run past its retention is not stamped done with its envelopes and its logs")
	}

	in.past(t)
	second := in.pass(t, p)
	if second.Objects != 2 || second.Bytes != int64(len(`{"old":true}`)+len("old bytes")) || second.Artifacts+second.Runs+second.Logs != 0 {
		t.Fatalf("the second pass removed %+v, and two objects of %d bytes were due", second, len(`{"old":true}`)+len("old bytes"))
	}
	for _, key := range []string{oldEnvelope, oldArtifact} {
		if in.held(t, key) {
			t.Errorf("%s, which nothing references, is still in the store", key)
		}
	}
	for _, key := range append([]string{youngEnvelope, youngArtifact}, youngLog...) {
		if !in.held(t, key) {
			t.Errorf("%s, of a run within its retention, went", key)
		}
	}
	if n := in.count(t, `select count(*) from artifact_objects`); n != 2 {
		t.Errorf("%d objects are recorded, and the two of the young run are left", n)
	}

	if got := in.pass(t, p); got.Removed() {
		t.Errorf("a pass with nothing left to do removed %+v", got)
	}
}

// An object whose count is zero past the grace stays while a write of it is under way, whose bytes
// nothing else could write again, and one a live artifact names stays whatever its count says. The
// first goes once the write has lapsed, and its row of the write with it.
func TestAnObjectBeingWrittenOrNamedStays(t *testing.T) {
	in := withInstallation(t)
	run := in.run(t)
	written := in.artifact(t, run, "written.bin", "written bytes", true)
	named := in.artifact(t, run, "named.bin", "named bytes", false)
	in.finish(t, run, false)

	p := in.purger(0, 0)
	in.pass(t, p)
	digest := strings.TrimPrefix(written, "finance/sha256/")
	if err := in.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		_, err := ns.Uploading(ctx, digest, time.Now().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// And the count of the named one gone wrong.
	in.exec(t, `update artifact_objects set refs = 0, collectable_at = now() where digest = 'sha256:' || '`+strings.TrimPrefix(named, "finance/sha256/")+`'`)
	in.past(t)

	if got := in.pass(t, p); got.Objects != 0 {
		t.Fatalf("the pass collected %d objects", got.Objects)
	}
	if !in.held(t, written) || !in.held(t, named) {
		t.Fatalf("an object being written or named went: written %t, named %t", in.held(t, written), in.held(t, named))
	}

	in.exec(t, `update artifact_uploads set until = now() - interval '1 second'`)
	if got, want := in.pass(t, p), (purge.Purged{Uploads: 1, Objects: 1, Bytes: int64(len("written bytes"))}); got != want {
		t.Fatalf("once the write lapsed the pass removed %+v, want %+v", got, want)
	}
	if in.held(t, written) || !in.held(t, named) {
		t.Errorf("once the write lapsed: written held %t, named held %t", in.held(t, written), in.held(t, named))
	}
}

// A backlog, months of rows past their retention as an installation upgraded from v0.2 holds them,
// is taken a batch a call and a bound of calls a pass, the rest left for the passes after.
func TestABacklogIsTakenABatchAtATime(t *testing.T) {
	in := withInstallation(t)
	var runs []agk.RunID
	for i := range 5 {
		run := in.run(t)
		in.envelope(t, run, fmt.Sprintf(`{"run":%d}`, i))
		runs = append(runs, run)
	}
	for i := range 23 {
		in.artifact(t, runs[0], fmt.Sprintf("a%02d.bin", i), fmt.Sprintf("bytes %d", i), true)
	}
	for _, run := range runs {
		in.finish(t, run, true)
	}

	p := in.purger(5, 2)
	p.Runs = 2
	var artifacts, envelopes []int
	for range 4 {
		got := in.pass(t, p)
		artifacts, envelopes = append(artifacts, got.Artifacts), append(envelopes, got.Runs)
	}
	if want := []int{10, 10, 3, 0}; fmt.Sprint(artifacts) != fmt.Sprint(want) {
		t.Errorf("the passes retired %v references, want %v", artifacts, want)
	}
	if want := []int{4, 1, 0, 0}; fmt.Sprint(envelopes) != fmt.Sprint(want) {
		t.Errorf("the passes let go of the envelopes of %v runs, want %v", envelopes, want)
	}

	// And the objects, once past the grace, the same way.
	in.past(t)
	var objects []int
	for range 4 {
		objects = append(objects, in.pass(t, p).Objects)
	}
	if want := []int{10, 10, 8, 0}; fmt.Sprint(objects) != fmt.Sprint(want) {
		t.Errorf("the passes collected %v objects, want %v", objects, want)
	}
}

// A backlog of logs, one object a chunk, is taken a batch of objects a call, a log being deleted
// across calls where it holds more than one takes, and the runs whose logs are all gone are stamped
// a batch of runs a call; a bound of calls a pass for each.
func TestALogBacklogIsTakenABatchAtATime(t *testing.T) {
	in := withInstallation(t)
	var runs []agk.RunID
	for range 4 {
		run := in.run(t)
		in.log(t, run, 3)
		in.finish(t, run, true)
		runs = append(runs, run)
	}
	p := in.purger(5, 2)
	p.Runs = 2
	stamped := func() int {
		return in.count(t, `select count(*) from runs where logs_purged_at is not null`)
	}

	// Two calls of five objects: the first log whole and two of the second, then the rest of the
	// second, the third and one of the fourth. Two calls of two runs stamp the three whose logs
	// are gone.
	if got := in.pass(t, p); got.Logs != 3 {
		t.Errorf("the first pass deleted %d logs whole, want 3", got.Logs)
	}
	if n := stamped(); n != 3 {
		t.Errorf("the first pass stamped %d runs done with their logs, want 3", n)
	}
	if got := in.pass(t, p); got.Logs != 1 {
		t.Errorf("the second pass deleted %d logs whole, want 1", got.Logs)
	}
	if n := stamped(); n != 4 {
		t.Errorf("after the second pass %d runs are stamped done with their logs, want 4", n)
	}
	if n := in.count(t, `select count(*) from task_log_objects`); n != 0 {
		t.Errorf("%d objects of logs are left", n)
	}
	if entries, err := os.ReadDir(filepath.Join(in.dir, "finance", "logs")); err != nil || len(entries) != 0 {
		t.Errorf("the directories of the logs left are %v (%v)", entries, err)
	}
}

// blocking is a store whose removals wait to be let through, for a test to look at the database
// while a pass is in the middle of deleting.
type blocking struct {
	artifact.Removable
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	letGo   sync.Once
}

// free lets every removal through, and may be called any number of times.
func (b *blocking) free() { b.letGo.Do(func() { close(b.release) }) }

func (b *blocking) Remove(ctx context.Context, key string) (bool, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.Removable.Remove(ctx, key)
}

// A pass in the middle of deleting holds nothing a decision needs: a run's artifacts are recorded,
// and a write of another object is made room for, while it waits.
func TestAPassInTheMiddleOfDeletingHoldsUpNothingElse(t *testing.T) {
	in := withInstallation(t)
	run := in.run(t)
	in.artifact(t, run, "gone.bin", "gone bytes", true)
	in.finish(t, run, false)
	p := in.purger(0, 0)
	in.pass(t, p)
	in.past(t)

	store := &blocking{Removable: in.store, entered: make(chan struct{}), release: make(chan struct{})}
	defer store.free()
	p.Objects = store
	passed := make(chan purge.Purged, 1)
	go func() {
		purged, err := p.Pass(t.Context())
		if err != nil {
			t.Error(err)
		}
		passed <- purged
	}()
	<-store.entered

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	other := in.run(t)
	digest, _ := in.put(t, "another run's bytes")
	if err := in.pool.In(ctx, "finance", func(ctx context.Context, ns *db.NS) error {
		if _, err := ns.Uploading(ctx, digest, time.Now().Add(time.Hour)); err != nil {
			return err
		}
		_, err := ns.WriteArtifact(ctx, db.Reference{
			URI:    agk.URI{Run: other, Step: "archive", Port: "out", Name: "other.bin"},
			Digest: digest, Size: int64(len("another run's bytes")), For: time.Hour,
		})
		return err
	}); err != nil {
		t.Fatalf("a write waited on the pass: %s", err)
	}
	store.free()
	if got := <-passed; got.Objects != 1 {
		t.Errorf("the pass collected %d objects", got.Objects)
	}
}

// Only the controller that leads purges: a pass asks the fence before every call, and one whose
// term another controller has taken removes nothing.
func TestOnlyTheControllerThatLeadsPurges(t *testing.T) {
	in := withInstallation(t)
	ctl, err := controller.New(in.pool, "first")
	if err != nil {
		t.Fatal(err)
	}
	term, err := in.pool.BeginTerm(t.Context(), "first")
	if err != nil {
		t.Fatal(err)
	}
	p := in.purger(0, 0)
	p.Leading = func(ctx context.Context) error {
		return ctl.Fenced(ctx, term, func(context.Context, *db.Wide) error { return nil })
	}

	run := in.run(t)
	in.artifact(t, run, "first.bin", "first bytes", true)
	if got := in.pass(t, p); got.Artifacts != 1 {
		t.Fatalf("the leading controller retired %d references", got.Artifacts)
	}

	in.artifact(t, run, "second.bin", "second bytes", true)
	if _, err := in.pool.BeginTerm(t.Context(), "second"); err != nil {
		t.Fatal(err)
	}
	got, err := p.Pass(t.Context())
	if !errors.Is(err, db.ErrFenced) || got.Removed() {
		t.Fatalf("a controller whose term was taken removed %+v and said %v", got, err)
	}
	if n := in.count(t, `select count(*) from artifacts where status = 'live'`); n != 1 {
		t.Errorf("%d references are live, and the one past its retain is the new leader's to retire", n)
	}
}

// failing is a store refusing to remove the keys it names.
type failing struct {
	artifact.Removable
	refuse map[string]bool
}

func (f failing) Remove(ctx context.Context, key string) (bool, error) {
	if f.refuse[key] {
		return false, errors.New("the disk said no")
	}
	return f.Removable.Remove(ctx, key)
}

// A pass that died halfway, having claimed and deleted without recording it, is finished by the
// next, which deletes what is left and records the whole; a store refusing some deletions leaves
// them for the pass after, with the rest recorded; and a pass after that removes nothing.
func TestAPassThatDiedHalfwayIsFinishedByTheNext(t *testing.T) {
	in := withInstallation(t)
	run := in.run(t)
	var keys []string
	for i := range 3 {
		keys = append(keys, in.artifact(t, run, fmt.Sprintf("a%d.bin", i), fmt.Sprintf("bytes %d", i), true))
	}
	in.finish(t, run, true)
	p := in.purger(0, 0)
	in.pass(t, p)
	in.past(t)
	chunks := in.log(t, run, 3)
	in.exec(t, `update runs set logs_purged_at = null`)

	// The collector and the log purge that died: each claimed, deleted the first of its objects,
	// and recorded nothing.
	if claimed, err := in.pool.Collectable(t.Context(), 0, 0); err != nil || len(claimed) != 3 {
		t.Fatalf("the collector claimed %d objects: %v", len(claimed), err)
	}
	if _, err := in.store.Remove(t.Context(), keys[0]); err != nil {
		t.Fatal(err)
	}
	if logs, err := in.pool.ExpiredLogs(t.Context(), 0); err != nil || len(logs) != 1 || len(logs[0].Keys) != 3 {
		t.Fatalf("the log purge claimed %+v: %v", logs, err)
	}
	if _, err := in.store.Remove(t.Context(), chunks[0]); err != nil {
		t.Fatal(err)
	}

	// The next pass, on a store refusing one object and one chunk.
	p.Objects = failing{Removable: in.store, refuse: map[string]bool{keys[2]: true, chunks[2]: true}}
	got, err := p.Pass(t.Context())
	if err == nil || !strings.Contains(err.Error(), "the disk said no") {
		t.Fatalf("a pass on a store refusing deletions said %v", err)
	}
	// The object the pass that died deleted is not counted again.
	if want := (purge.Purged{Objects: 1, Bytes: int64(len("bytes 1"))}); got != want {
		t.Errorf("the pass on a refusing store removed %+v, want %+v", got, want)
	}
	if in.held(t, keys[1]) || !in.held(t, keys[2]) || in.held(t, chunks[1]) || !in.held(t, chunks[2]) {
		t.Errorf("the pass deleted the wrong objects")
	}

	p.Objects = in.store
	if got, want := in.pass(t, p), (purge.Purged{Logs: 1, Objects: 1, Bytes: int64(len("bytes 2"))}); got != want {
		t.Errorf("the pass after removed %+v, want %+v", got, want)
	}
	for _, key := range append(keys, chunks...) {
		if in.held(t, key) {
			t.Errorf("%s is still in the store", key)
		}
	}
	if n := in.count(t, `select count(*) from artifact_objects`) + in.count(t, `select count(*) from task_log_objects`) +
		in.count(t, `select count(*) from tasks where log_uri is not null`); n != 0 {
		t.Errorf("%d rows are left of what was deleted", n)
	}
	if got := in.pass(t, p); got.Removed() {
		t.Errorf("a pass with nothing left removed %+v", got)
	}
}

// Run passes at once, tells what each pass removed, passes again on its interval, and stops when its
// context is done.
func TestRunPassesAtOnceAndOnItsInterval(t *testing.T) {
	in := withInstallation(t)
	run := in.run(t)
	in.artifact(t, run, "gone.bin", "gone bytes", true)

	// running runs p until it has passed n times, and answers what each pass removed and what
	// went wrong.
	running := func(p *purge.Purger, n int) ([]purge.Purged, []error) {
		t.Helper()
		var mu sync.Mutex
		var passes []purge.Purged
		var troubles []error
		p.Passed = func(got purge.Purged) {
			mu.Lock()
			defer mu.Unlock()
			passes = append(passes, got)
		}
		p.Trouble = func(err error) {
			mu.Lock()
			defer mu.Unlock()
			troubles = append(troubles, err)
		}
		ctx, stop := context.WithCancel(t.Context())
		ran := make(chan struct{})
		go func() {
			defer close(ran)
			p.Run(ctx)
		}()
		deadline := time.Now().Add(10 * time.Second)
		for {
			mu.Lock()
			got := len(passes)
			mu.Unlock()
			if got >= n {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("Run passed %d times in 10s, and %d were due", got, n)
			}
			time.Sleep(10 * time.Millisecond)
		}
		stop()
		select {
		case <-ran:
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not stop once its context was done")
		}
		mu.Lock()
		defer mu.Unlock()
		return passes, troubles
	}

	// At once, with the next pass an hour away.
	p := in.purger(0, 0)
	p.Every = time.Hour
	passes, troubles := running(p, 1)
	if len(passes) != 1 || passes[0].Artifacts != 1 || len(troubles) != 0 {
		t.Errorf("the first pass removed %+v, and met %v", passes, troubles)
	}

	// And on the interval.
	p = in.purger(0, 0)
	p.Every = 20 * time.Millisecond
	passes, troubles = running(p, 3)
	if passes[1].Removed() || len(troubles) != 0 {
		t.Errorf("the passes removed %+v, and met %v", passes, troubles)
	}

	// A pass that could not finish is said, each time.
	p = in.purger(0, 0)
	p.Every = 20 * time.Millisecond
	p.Leading = func(context.Context) error { return errors.New("the database did not answer") }
	_, troubles = running(p, 2)
	if len(troubles) < 2 || !strings.Contains(troubles[0].Error(), "the database did not answer") {
		t.Errorf("the passes that could not finish said %v", troubles)
	}
}
