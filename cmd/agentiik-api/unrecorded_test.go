package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/purge"
	"github.com/jackc/pgx/v5"
)

// The artifact files a v0.2.5 installation wrote with no row, recorded by this release's init and
// migrate from the envelopes of the runs it finished, and collected by the purges once they have run
// out, with nothing done by hand; and the files of a run still under way kept.

// v025Store is an installation as v0.2.5 left its database and its object store, a month and more
// after it began: the database migrated as far as v0.2.5 migrated it, and every file written forty
// days ago.
type v025Store struct {
	t       *testing.T
	admin   *pgx.Conn
	objects artifact.Removable

	// The files, by what they are: old is a run finished forty days ago, past the namespace's 30
	// days, recent one finished yesterday, live one still running, and orphan the output of an
	// attempt that failed, which nothing names.
	keys map[string]string
}

// fileOf is a file as an envelope names it, of run's step archive on port out.
func fileOf(run agk.RunID, name, content string) agk.File {
	sum := sha256.Sum256([]byte(content))
	return agk.File{
		Name: name, URI: agk.URI{Run: run, Step: "archive", Port: "out", Name: name},
		MediaType: "application/octet-stream", Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:]),
	}
}

func aV025Store(t *testing.T, admin *pgx.Conn, dir string) *v025Store {
	t.Helper()
	ctx := t.Context()
	if _, err := db.MigrateThrough(ctx, admin, v025); err != nil {
		t.Fatalf("the database could not be migrated as v0.2.5 migrated it: %s", err)
	}
	s := &v025Store{t: t, admin: admin, objects: artifact.Dir(dir), keys: map[string]string{}}
	const (
		old    = "01JMZ8V1P9C4XQ7K2N4D6F8H0A"
		recent = "01JMZ8V1P9C4XQ7K2N4D6F8H0B"
		live   = "01JMZ8V1P9C4XQ7K2N4D6F8H0C"
	)
	s.exec(
		`insert into namespaces (name, max_retention_days) values ('finance', 30)`,
		`insert into workflows (namespace, name) values ('finance', 'monthly-invoicing')`,
		`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at)
		   values ('finance', 'monthly-invoicing', 'a3f9c1e', '{}', 'operator', now() - interval '50 days')`)

	// Each file's bytes, then the envelope naming them, written as v0.2.5's runner and controller
	// wrote them; v0.2.5 counted the envelope and recorded nothing of a file its workflow gave no
	// retain.
	envelope := func(run agk.RunID, shard int, files ...agk.File) string {
		item := agk.NewItem(map[string]any{})
		item.Files = files
		e := agk.Envelope{Meta: agk.Meta{RunID: run, Step: "archive", Port: "out", Attempt: 1, Count: 1, ProducedAt: time.Now().UTC()}, Items: []agk.Item{item}}
		digest, size, err := artifact.PutEnvelope(ctx, s.objects, "finance", e)
		if err != nil {
			t.Fatal(err)
		}
		s.exec(
			fmt.Sprintf(`insert into artifact_objects (namespace, digest, size_bytes, media_type, refs)
			             values ('finance', 'sha256:%s', %d, 'application/json', 1)`, digest, size),
			fmt.Sprintf(`update runs set evaluation = '{"version": 1, "envelopes": [{"step": "archive", "shard": %d, "port": "out", "digest": "%s", "size": %d}]}'
			             where id = '%s'`, shard, digest, size, run))
		if shard == db.PublishedByTheStep {
			s.exec(fmt.Sprintf(`update steps set ports = '{"out": {"digest": "sha256:%s", "size": %d, "items": 1}}' where run_id = '%s'`, digest, size, run))
		}
		s.keys["envelope of "+string(run)] = artifact.Key("finance", digest)
		return digest
	}
	put := func(what, content string) {
		sum := sha256.Sum256([]byte(content))
		key := artifact.Key("finance", hex.EncodeToString(sum[:]))
		if err := s.objects.Put(ctx, key, strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
		s.keys[what] = key
	}

	for run, state := range map[string]string{old: "succeeded", recent: "succeeded", live: "running"} {
		finished := map[string]string{old: "now() - interval '40 days'", recent: "now() - interval '1 day'", live: "null"}[run]
		s.exec(
			`insert into runs (namespace, id, workflow, commit, state, trigger, triggered_by, started_at, finished_at)
			   values ('finance', '`+run+`', 'monthly-invoicing', 'a3f9c1e', '`+state+`', 'manual', 'operator', now() - interval '41 days', `+finished+`)`,
			`insert into steps (namespace, run_id, step, state) values ('finance', '`+run+`', 'archive', '`+map[bool]string{true: "pending", false: "succeeded"}[run == live]+`')`)
	}
	put("old", "the invoices of July")
	envelope(old, db.PublishedByTheStep, fileOf(old, "invoices.csv", "the invoices of July"))
	put("recent", "the invoices of August")
	put("recent, given a retain", "the totals of August")
	envelope(recent, db.PublishedByTheStep, fileOf(recent, "invoices.csv", "the invoices of August"), fileOf(recent, "totals.zip", "the totals of August"))
	// An output its workflow gave a retain, which v0.2.5 recorded.
	s.exec(
		`insert into artifact_objects (namespace, digest, size_bytes, media_type, refs)
		   values ('finance', 'sha256:`+fileOf(recent, "totals.zip", "the totals of August").SHA256+`', 20, 'application/octet-stream', 1)`,
		`insert into artifacts (namespace, run_id, step, port, name, digest, size_bytes, media_type, expires_at)
		   values ('finance', '`+recent+`', 'archive', 'out', 'totals.zip', 'sha256:`+fileOf(recent, "totals.zip", "the totals of August").SHA256+`',
		           20, 'application/octet-stream', now() + interval '20 days')`)
	put("live", "a shard's invoices")
	envelope(live, 0, fileOf(live, "invoices.csv", "a shard's invoices"))
	put("orphan", "an attempt that failed")

	then := time.Now().Add(-40 * 24 * time.Hour)
	for _, key := range s.keys {
		if err := os.Chtimes(filepath.Join(dir, filepath.FromSlash(key)), then, then); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func (s *v025Store) exec(stmts ...string) {
	s.t.Helper()
	for _, stmt := range stmts {
		if _, err := s.admin.Exec(s.t.Context(), stmt); err != nil {
			s.t.Fatalf("filling the database as v0.2.5 would have: %s: %s", stmt, err)
		}
	}
}

// held says which of the files the store still holds.
func (s *v025Store) held() map[string]bool {
	s.t.Helper()
	out := map[string]bool{}
	for what, key := range s.keys {
		ok, err := s.objects.Has(s.t.Context(), key)
		if err != nil {
			s.t.Fatal(err)
		}
		out[what] = ok
	}
	return out
}

// refs is the count on the object of the file what, and -1 where it has no row.
func (s *v025Store) refs(what string) int {
	s.t.Helper()
	var n int
	if err := s.admin.QueryRow(s.t.Context(),
		`select coalesce((select refs from artifact_objects where 'finance/sha256/' || substr(digest, 8) = $1), -1)`, s.keys[what]).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// A v0.2.5 installation upgraded by init: the files v0.2.5 left unrecorded are recorded from the
// envelopes of the runs it finished, each expiring 30 days after its run finished, and a second init
// records nothing again. The purges then retire what has run out, the orphan is collected in the
// first pass, and what was retired is collected once the grace has passed, while the run finished
// yesterday keeps its files and the run still going keeps the file its shard's envelope names.
func TestTheFilesV025LeftAreRecordedByInitAndCollectedInTime(t *testing.T) {
	database := freshDatabase(t)
	ctx := t.Context()
	admin, err := pgx.Connect(ctx, database.Admin.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.WithoutCancel(ctx))
	d := aPreparedDirectory(t)
	s := aV025Store(t, admin, filepath.Join(d.dir, objectsDir))

	c := config.Init{
		Dir: d.dir, Host: "localhost", Namespace: "finance", OperatorToken: theToken,
		Admin: database.Admin, Application: config.Database{URL: database.Application.URL, Role: database.Application.Role},
	}
	if err := initialize(ctx, c, d.at(time.Now().UTC())); err != nil {
		t.Fatalf("init refused the installation v0.2.5 left: %s\n%s", err, d.out.String())
	}
	if !strings.Contains(d.out.String(), "recorded the artifact files of 2 runs v0.2 finished as 2 artifacts") {
		t.Errorf("init said:\n%s", d.out.String())
	}
	var expiries int
	if err := admin.QueryRow(ctx, `select count(*) from artifacts a join runs r on r.namespace = a.namespace and r.id = a.run_id
	                               where a.name = 'invoices.csv' and a.expires_at = r.finished_at + interval '30 days'`).Scan(&expiries); err != nil {
		t.Fatal(err)
	}
	if expiries != 2 {
		t.Errorf("%d of the two files v0.2.5 left are recorded to expire 30 days after their run finished", expiries)
	}
	d.out.Reset()
	if err := initialize(ctx, c, d.at(time.Now().UTC())); err != nil {
		t.Fatalf("a second init failed: %s\n%s", err, d.out.String())
	}
	if strings.Contains(d.out.String(), "artifact files") {
		t.Errorf("a second init said:\n%s", d.out.String())
	}
	for what, want := range map[string]int{"old": 1, "recent": 1, "recent, given a retain": 1, "live": -1, "orphan": -1} {
		if n := s.refs(what); n != want {
			t.Errorf("after two inits the file %s is counted %d, want %d", what, n, want)
		}
	}

	application := database.Application
	application.Password = config.Secret(strings.TrimSpace(d.read(t, apiDir, "database-password")))
	pool, err := db.Open(ctx, application.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	p := &purge.Purger{Pool: pool, Objects: s.objects}
	first, err := p.Pass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Artifacts != 1 || first.Runs != 1 || first.Orphans != 1 || first.Objects != 1 {
		t.Errorf("the first pass after the upgrade removed %+v, and the old run's file, its envelope and the orphan were due", first)
	}
	if _, err := admin.Exec(ctx, `update artifact_objects set collectable_at = now() - interval '2 days' where collectable_at is not null`); err != nil {
		t.Fatal(err)
	}
	second, err := p.Pass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Objects != 2 {
		t.Errorf("the pass past the grace collected %d objects, and the old run's file and its envelope were due", second.Objects)
	}
	want := map[string]bool{
		"old": false, "envelope of 01JMZ8V1P9C4XQ7K2N4D6F8H0A": false, "orphan": false,
		"recent": true, "recent, given a retain": true, "envelope of 01JMZ8V1P9C4XQ7K2N4D6F8H0B": true,
		"live": true, "envelope of 01JMZ8V1P9C4XQ7K2N4D6F8H0C": true,
	}
	for what, held := range s.held() {
		if held != want[what] {
			t.Errorf("after the purges, the file %s is held %t", what, held)
		}
	}
}

// migrate, which an installation that runs no init runs in its place, records the same files where
// it is given the object store, and says it recorded none where it is not, recording them at its
// next run given the store.
func TestMigrateRecordsTheFilesV025LeftWhereItIsGivenTheStore(t *testing.T) {
	b := aBootstrapState(t)
	dir := t.TempDir()
	s := aV025Store(t, b.admin, dir)

	out := b.migrated(theToken, "")
	if !strings.Contains(out, "recorded none of the artifact files of the runs v0.2 finished, since AGK_OBJECTS_DIR is not set") {
		t.Errorf("migrate with no object store said:\n%s", out)
	}
	if n := s.refs("old"); n != -1 {
		t.Errorf("migrate with no object store counted a file %d times", n)
	}

	c := b.database
	c.OperatorToken = theToken
	c.Objects = dir
	var said bytes.Buffer
	if err := migrateAndBootstrap(t.Context(), c, &said); err != nil {
		t.Fatalf("migrate failed: %s\n%s", err, said.String())
	}
	if !strings.Contains(said.String(), "recorded the artifact files of 2 runs v0.2 finished as 2 artifacts") {
		t.Errorf("migrate given the object store said:\n%s", said.String())
	}
	if s.refs("old") != 1 || s.refs("recent") != 1 {
		t.Error("migrate given the object store did not record the files")
	}
	if out := b.migrated(theToken, ""); strings.Contains(out, "artifact files") {
		t.Errorf("once they are recorded, migrate with no object store said:\n%s", out)
	}
}
