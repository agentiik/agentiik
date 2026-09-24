package db

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A version's tree, against a real PostgreSQL: what it names, what it keeps alive, and what a
// second push of the same commit may change, which is nothing.

// aTree is a repository of three files, two of which hold the same bytes: three paths, two
// objects.
func aTree() []TreeFile {
	return []TreeFile{
		{Path: "scripts/render.sh", SHA256: digestOf("b"), Size: 21, Mode: "0755"},
		{Path: "agentiik.yaml", SHA256: digestOf("a"), Size: 412, Mode: "0644"},
		{Path: "scripts/again.sh", SHA256: digestOf("b"), Size: 21, Mode: "0755"},
	}
}

func aVersion(commit string, tree []TreeFile) Version {
	return Version{
		Workflow: "monthly-invoicing", Commit: commit,
		Entry: "agentiik.yaml", Document: []byte("kind: Workflow"),
		Tree: tree, Author: "alice",
	}
}

func saveVersion(t *testing.T, pool *Pool, v Version) (Saved, error) {
	t.Helper()
	var saved Saved
	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		var err error
		saved, err = ns.SaveVersion(ctx, v)
		return err
	})
	return saved, err
}

// oneFetchAndGone writes an artifact whose single fetch is spent at once, which leaves its object
// at a count of zero: the one kind of object the collector is allowed to take.
func oneFetchAndGone(t *testing.T, pool *Pool, name, digest string, size int64) {
	t.Helper()
	u := uri(financeRun, "archive", "out", name)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		if _, err := ns.WriteArtifact(ctx, Reference{URI: u, Digest: digest, Size: size, For: time.Hour, Fetches: 1}); err != nil {
			return err
		}
		_, err := fetched(ctx, ns, u)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// A run pins its commit, "and neither does a replay of it six months on", which a replay whose
// /agk/repo had been collected underneath it would make untrue. So a version counts one reference
// per distinct object its tree names, and the collector never takes one of them however long it
// waits.
func TestATreeKeepsItsObjectsOutOfTheCollectorsHands(t *testing.T) {
	pool, super := opened(t)

	saved, err := saveVersion(t, pool, aVersion("b4a0d2f", aTree()))
	if err != nil {
		t.Fatal(err)
	}
	if !saved.New || len(saved.MustWriteBytes) != 0 {
		t.Errorf("a new version saved as %+v", saved)
	}
	// Three paths, two objects, one reference each.
	for _, c := range []string{"a", "b"} {
		if got := refsOf(t, pool, "finance", digestOf(c)); got != 1 {
			t.Errorf("%s is referenced %d times by one version", digestOf(c), got)
		}
	}

	// And beside them an object nothing references any more, so that a sweep taking nothing
	// is a sweep that looked and declined rather than one that never ran.
	oneFetchAndGone(t, pool, "spent.bin", digestOf("c"), 3)

	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(),
		`update artifact_objects set collectable_at = now() - interval '200 days'`); err != nil {
		t.Fatal(err)
	}

	claimed, err := pool.Collectable(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Digest != digestOf("c") {
		t.Fatalf("the sweep claimed %+v, and only the spent artifact was due", claimed)
	}
}

// The window the grace alone does not close, for a tree: a version arrives naming an object a
// sweep has already claimed. The version is told which bytes to write again, and the confirmation
// of the sweep leaves the row alone.
func TestATreeObjectASweepHadClaimedIsWrittenAgain(t *testing.T) {
	pool, super := opened(t)

	oneFetchAndGone(t, pool, "script.sh", digestOf("b"), 21)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(),
		`update artifact_objects set collectable_at = now() - interval '2 days'`); err != nil {
		t.Fatal(err)
	}
	claimed, err := pool.Collectable(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("the sweep claimed %d objects", len(claimed))
	}

	saved, err := saveVersion(t, pool, aVersion("b4a0d2f", aTree()))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(saved.MustWriteBytes, []string{digestOf("b")}) {
		t.Errorf("the version was told to write %v again, and the sweep had claimed %s", saved.MustWriteBytes, digestOf("b"))
	}

	removed, err := pool.Collected(t.Context(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("the confirmation removed %d rows, and a version names the object", removed)
	}
	if got := refsOf(t, pool, "finance", digestOf("b")); got != 1 {
		t.Errorf("the object is referenced %d times", got)
	}
}

// The window MustWriteBytes cannot see: a sweep that claimed an object, deleted its bytes and
// confirmed it gone, all before a version raised its reference. No row is left to be found
// claimed, so the version records the object afresh and says so, which is how a caller that skipped
// writing the bytes, because the store held them when it asked, knows to write them after all.
func TestATreeObjectASweepCollectedWholeIsRecordedAfresh(t *testing.T) {
	pool, super := opened(t)

	oneFetchAndGone(t, pool, "script.sh", digestOf("b"), 21)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(),
		`update artifact_objects set collectable_at = now() - interval '2 days'`); err != nil {
		t.Fatal(err)
	}
	claimed, err := pool.Collectable(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := pool.Collected(t.Context(), claimed); err != nil || removed != 1 {
		t.Fatalf("the sweep confirmed %d objects gone: %v", removed, err)
	}

	saved, err := saveVersion(t, pool, aVersion("b4a0d2f", aTree()))
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.MustWriteBytes) != 0 {
		t.Errorf("the version was told to write %v again, and no sweep holds a claim on anything", saved.MustWriteBytes)
	}
	if want := slices.Sorted(slices.Values([]string{digestOf("a"), digestOf("b")})); !slices.Equal(saved.Recorded, want) {
		t.Errorf("the version says it recorded %v afresh, and neither of its objects had a row: %v", saved.Recorded, want)
	}

	// And a second version onto the rows the first one made records none of them afresh.
	saved, err = saveVersion(t, pool, aVersion("c5b1e3a", aTree()))
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Recorded) != 0 {
		t.Errorf("a version onto objects already held says it recorded %v afresh", saved.Recorded)
	}
}

// Two pushes of two commits at once, sharing a file the namespace has never held: both are
// recorded, and the object counts both. A CI job pushing two branches together is the ordinary
// way to arrive here, and one of the two used to fail on the object's primary key.
//
// The first is held open until the second is waiting on it, which is the moment the two used to
// collide, so the test does not depend on the scheduler happening to interleave them.
func TestTwoVersionsReachingOneNewObjectAtOnceAreBothRecorded(t *testing.T) {
	pool, super := opened(t)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())

	shared := []TreeFile{{Path: "agentiik.yaml", SHA256: digestOf("e"), Size: 7, Mode: "0644"}}
	second := make(chan error, 1)
	err = pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		if _, err := ns.SaveVersion(ctx, aVersion("b4a0d2f", shared)); err != nil {
			return err
		}
		go func() {
			_, err := saveVersion(t, pool, aVersion("c5b1e3a", shared))
			second <- err
		}()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var waiting int
			if err := conn.QueryRow(t.Context(),
				`select count(*) from pg_stat_activity
				 where datname = current_database() and wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
				return err
			}
			if waiting > 0 {
				return nil
			}
			if time.Now().After(deadline) {
				return errors.New("the second push never waited on the first")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatalf("the second of two pushes sharing a new file was refused: %s", err)
	}
	if got := refsOf(t, pool, "finance", digestOf("e")); got != 2 {
		t.Errorf("an object two versions name is referenced %d times", got)
	}
}

// "A version is a commit": the same commit again with the same files changes nothing, and the
// same commit with other files is refused rather than quietly left as it was.
func TestASecondPushOfOneCommitIsComparedByItsTree(t *testing.T) {
	pool, _ := opened(t)

	if _, err := saveVersion(t, pool, aVersion("b4a0d2f", aTree())); err != nil {
		t.Fatal(err)
	}

	// The same files in another order are the same tree.
	reordered := aTree()
	slices.Reverse(reordered)
	saved, err := saveVersion(t, pool, aVersion("b4a0d2f", reordered))
	if err != nil {
		t.Fatalf("the same tree again was refused: %s", err)
	}
	if saved.New {
		t.Error("the same version pushed twice was recorded twice")
	}
	if got := refsOf(t, pool, "finance", digestOf("b")); got != 1 {
		t.Errorf("pushing the same version again moved the count to %d", got)
	}

	// One bit of one file is another tree, and so is no tree at all.
	changed := aTree()
	changed[1].Mode = "0755"
	for _, c := range []struct {
		name string
		tree []TreeFile
	}{
		{"a mode that changed", changed},
		{"a file that appeared", append(aTree(), TreeFile{Path: "README.md", SHA256: digestOf("d"), Size: 9, Mode: "0644"})},
		{"no tree at all", nil},
	} {
		_, err := saveVersion(t, pool, aVersion("b4a0d2f", c.tree))
		if !errors.Is(err, ErrOtherTree) {
			t.Errorf("%s answered %v", c.name, err)
		}
	}
	if got := refsOf(t, pool, "finance", digestOf("b")); got != 1 {
		t.Errorf("a refused push moved the count to %d", got)
	}

	var held []TreeFile
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		v, err := ns.Version(ctx, "monthly-invoicing", "b4a0d2f")
		held = v.Tree
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if held[0].Mode != "0644" || len(held) != 3 {
		t.Errorf("a refused push changed the tree to %+v", held)
	}
}

// A push asks before it writes a byte whether its commit is recorded with other files, and the
// asking records nothing: a commit nobody pushed is still nobody's afterwards.
func TestAVersionIsComparedWithoutBeingRecorded(t *testing.T) {
	pool, _ := opened(t)
	check := func(v Version) error {
		t.Helper()
		return pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			return ns.CheckVersion(ctx, v)
		})
	}

	if err := check(aVersion("b4a0d2f", aTree())); err != nil {
		t.Fatalf("a commit nobody recorded answered %v", err)
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		_, err := ns.Version(ctx, "monthly-invoicing", "b4a0d2f")
		return err
	}); !errors.Is(err, ErrNoVersion) {
		t.Fatalf("checking a version recorded it: %v", err)
	}

	if _, err := saveVersion(t, pool, aVersion("b4a0d2f", aTree())); err != nil {
		t.Fatal(err)
	}
	reordered := aTree()
	slices.Reverse(reordered)
	if err := check(aVersion("b4a0d2f", reordered)); err != nil {
		t.Errorf("the same tree again answered %v", err)
	}
	changed := aTree()
	changed[0].SHA256 = digestOf("d")
	if err := check(aVersion("b4a0d2f", changed)); !errors.Is(err, ErrOtherTree) {
		t.Errorf("another tree answered %v", err)
	}
	// The seeded version, recorded before a version carried its tree.
	if err := check(aVersion("a3f9c1e", aTree())); !errors.Is(err, ErrOtherTree) {
		t.Errorf("a tree for a version recorded without one answered %v", err)
	}
}

// The redemption reads the tree through the installation door and reads nothing else, and it can
// tell a version that is not there from one recorded without its tree: neither of them is an
// empty directory.
func TestTheTreeOfOneVersionIsReadOnItsOwn(t *testing.T) {
	pool, super := opened(t)
	if _, err := saveVersion(t, pool, aVersion("b4a0d2f", aTree())); err != nil {
		t.Fatal(err)
	}

	var tree []TreeFile
	var none, bare error
	if err := pool.Installation(t.Context(), Redemption, func(ctx context.Context, w *Wide) error {
		var err error
		tree, err = w.Tree(ctx, "finance", "monthly-invoicing", "b4a0d2f")
		if err != nil {
			return err
		}
		_, none = w.Tree(ctx, "finance", "monthly-invoicing", "deadbee")
		// The seeded version, recorded before a version carried its tree.
		_, bare = w.Tree(ctx, "finance", "monthly-invoicing", "a3f9c1e")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var read []string
	for _, f := range tree {
		read = append(read, f.Path+" "+f.Mode)
	}
	if got, want := strings.Join(read, ", "), "agentiik.yaml 0644, scripts/again.sh 0755, scripts/render.sh 0755"; got != want {
		t.Errorf("the tree reads %s, want %s", got, want)
	}
	if !errors.Is(none, ErrNoVersion) {
		t.Errorf("a commit nobody recorded answered %v", none)
	}
	if !errors.Is(bare, ErrNoTree) || errors.Is(bare, ErrNoVersion) {
		t.Errorf("a version with no tree answered %v", bare)
	}

	// And the column holds the wire's names and nothing else, with the tree out of the
	// document a graph is rebuilt from.
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	var raw []byte
	var inGraph bool
	if err := conn.QueryRow(t.Context(),
		`select tree, graph ? 'tree' from workflow_versions where commit = 'b4a0d2f'`).Scan(&raw, &inGraph); err != nil {
		t.Fatal(err)
	}
	if inGraph {
		t.Error("the tree is still in the graph column, which holds what it takes to rebuild the version and nothing else")
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		var keys []string
		for k := range e {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if got := strings.Join(keys, ","); got != "mode,path,sha256,size" {
			t.Errorf("a stored entry carries %s", got)
		}
	}
}

// The digest each tag was resolved to at the push is part of what the graph is rebuilt from, so
// it is kept beside the manifests in the graph column and read back with them. The first push of
// a commit settles it: the same commit pushed again after the tag moved changes nothing a run of
// it names.
func TestTheDigestsAVersionsTagsWereResolvedToAreKept(t *testing.T) {
	pool, super := opened(t)
	pinned := map[string]string{
		"ghcr.io/acme/agk-invoice:1.4.0": "ghcr.io/acme/agk-invoice@sha256:" + digestOf("e"),
		"alpine:3.21":                    "alpine@sha256:" + digestOf("f"),
	}
	v := aVersion("b4a0d2f", aTree())
	v.Images = pinned
	if _, err := saveVersion(t, pool, v); err != nil {
		t.Fatal(err)
	}

	// Pushed again once the tag had moved, which is the same version.
	moved := aVersion("b4a0d2f", aTree())
	moved.Images = map[string]string{
		"ghcr.io/acme/agk-invoice:1.4.0": "ghcr.io/acme/agk-invoice@sha256:" + digestOf("d"),
		"alpine:3.21":                    "alpine@sha256:" + digestOf("f"),
	}
	if saved, err := saveVersion(t, pool, moved); err != nil || saved.New {
		t.Fatalf("the same commit pushed again saved as %+v: %v", saved, err)
	}

	var back Version
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		var err error
		back, err = ns.Version(ctx, "monthly-invoicing", "b4a0d2f")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(back.Images, pinned) {
		t.Errorf("the version reads its images back as %v, want %v", back.Images, pinned)
	}

	// In the graph column, beside what else the graph is rebuilt from.
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	var inGraph bool
	if err := conn.QueryRow(t.Context(),
		`select graph ? 'images' from workflow_versions where commit = 'b4a0d2f'`).Scan(&inGraph); err != nil {
		t.Fatal(err)
	}
	if !inGraph {
		t.Error("the images are not in the graph column, which holds what it takes to rebuild the version")
	}

	// And a version that names none, all its images written by digest, holds nothing for them.
	if _, err := saveVersion(t, pool, aVersion("c5b1e3a", aTree())); err != nil {
		t.Fatal(err)
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		var err error
		back, err = ns.Version(ctx, "monthly-invoicing", "c5b1e3a")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if back.Images != nil {
		t.Errorf("a version whose images are all digests reads back %v", back.Images)
	}
}
