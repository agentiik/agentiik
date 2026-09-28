package db

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A workflow repository's refs and packs, against a real PostgreSQL: a ref moves by compare and
// swap, a push's refs move all together or not at all, two pushes to one repository take turns, and
// a namespace reads and moves its own repositories alone.

// Commits and tag objects, as git names them.
var (
	c1, c2, c3 = strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40)
	tagObject  = strings.Repeat("7", 40)
)

// repositories opens a database with finance and team-ops, each holding a workflow nightly its
// first push created, and answers the application's pool and the superuser's address.
func repositories(t *testing.T) (*Pool, string) {
	t.Helper()
	super, app := database(t)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(), `insert into namespaces (name) values ('finance'), ('team-ops')`); err != nil {
		t.Fatal(err)
	}
	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, namespace := range []string{"finance", "team-ops"} {
		if err := pool.In(t.Context(), namespace, func(ctx context.Context, n *NS) error {
			return n.SaveWorkflow(ctx, "nightly", "main")
		}); err != nil {
			t.Fatal(err)
		}
	}
	return pool, super
}

func repositoryOf(t *testing.T, pool *Pool, namespace, workflow string) Repository {
	t.Helper()
	var r Repository
	if err := pool.In(t.Context(), namespace, func(ctx context.Context, n *NS) error {
		var err error
		r, err = n.Repository(ctx, workflow)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

func updateRefs(t *testing.T, pool *Pool, namespace string, updates ...RefUpdate) error {
	t.Helper()
	return pool.In(t.Context(), namespace, func(ctx context.Context, n *NS) error {
		return n.UpdateRefs(ctx, "nightly", "alice", time.Time{}, updates)
	})
}

// sameRefs is whether got and want are the same refs, their times compared as instants.
func sameRefs(got, want []Ref) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		g, w := got[i], want[i]
		if !g.MovedAt.Equal(w.MovedAt) {
			return false
		}
		g.MovedAt, w.MovedAt = time.Time{}, time.Time{}
		if g != w {
			return false
		}
	}
	return true
}

// refNamed answers the ref of r named name, and whether it holds one.
func refNamed(r Repository, name string) (Ref, bool) {
	for _, ref := range r.Refs {
		if ref.Name == name {
			return ref, true
		}
	}
	return Ref{}, false
}

// A workflow its first push records is an empty repository: a key of its own, its default branch
// unborn and protected, as a repository created from v0.4.0 is, and no pack. Recording it again
// changes none of it, and a default branch no push could create is given no row.
func TestAWorkflowRecordedIsAnEmptyRepositoryWithItsDefaultBranchUnborn(t *testing.T) {
	pool, _ := repositories(t)
	r := repositoryOf(t, pool, "finance", "nightly")
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(r.Key) || r.DefaultBranch != "main" || len(r.Packs) != 0 {
		t.Errorf("the repository reads as key %q, default branch %q, packs %v", r.Key, r.DefaultBranch, r.Packs)
	}
	if len(r.Refs) != 1 || r.Refs[0] != (Ref{Name: "refs/heads/main", Protected: true}) {
		t.Errorf("the refs of an empty repository read as %+v", r.Refs)
	}
	if other := repositoryOf(t, pool, "team-ops", "nightly"); other.Key == r.Key {
		t.Errorf("two repositories share the key %s", r.Key)
	}

	if err := pool.In(t.Context(), "finance", func(ctx context.Context, n *NS) error {
		if err := n.SaveWorkflow(ctx, "nightly", "trunk"); err != nil {
			return err
		}
		return n.SaveWorkflow(ctx, "odd", "two words")
	}); err != nil {
		t.Fatal(err)
	}
	if again := repositoryOf(t, pool, "finance", "nightly"); again.Key != r.Key || again.DefaultBranch != "main" || len(again.Refs) != 1 {
		t.Errorf("recorded again naming another branch, the repository reads as %+v", again)
	}
	if odd := repositoryOf(t, pool, "finance", "odd"); odd.DefaultBranch != "two words" || len(odd.Refs) != 0 {
		t.Errorf("a workflow whose default branch no push could create reads as %+v", odd)
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, n *NS) error {
		_, err := n.Repository(ctx, "absent")
		return err
	}); !errors.Is(err, ErrNoWorkflow) {
		t.Errorf("the repository of no workflow answered %v", err)
	}
}

// A ref is created, moved and deleted only while it names what the push found it naming, and the
// default branch is not deleted at all. An annotated tag keeps the tag object git names beside the
// commit it peels to, and who moved a ref, and when, is kept with it.
func TestARefMovesOnlyFromWhereThePushFoundIt(t *testing.T) {
	pool, super := repositories(t)
	// The refs held in a collation that is not git's order, as a database created under a glibc or
	// an ICU locale sorts them, feature before Zeta: git's order is byte by byte whatever that is.
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(), `alter table workflow_refs alter column ref type text collate "und-x-icu"`); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 28, 10, 12, 0, 0, time.UTC)
	move := func(by string, updates ...RefUpdate) error {
		return pool.In(t.Context(), "finance", func(ctx context.Context, n *NS) error {
			return n.UpdateRefs(ctx, "nightly", by, at, updates)
		})
	}

	if err := move("alice", RefUpdate{Ref: "refs/heads/main", New: c1}); err != nil {
		t.Fatalf("the first push to an unborn default branch answered %v", err)
	}
	main, _ := refNamed(repositoryOf(t, pool, "finance", "nightly"), "refs/heads/main")
	if !sameRefs([]Ref{main}, []Ref{{Name: "refs/heads/main", Commit: c1, Protected: true, MovedBy: "alice", MovedAt: at}}) {
		t.Errorf("the default branch born reads as %+v", main)
	}
	for _, stale := range []RefUpdate{
		{Ref: "refs/heads/main", New: c2},
		{Ref: "refs/heads/main", Old: c2, New: c3},
		{Ref: "refs/heads/feature", Old: c1, New: c2},
		{Ref: "refs/heads/feature", Old: c1},
	} {
		if err := move("mallory", stale); !errors.Is(err, ErrStaleRef) || !strings.Contains(err.Error(), stale.Ref) {
			t.Errorf("%+v answered %v, where the ref is not where it says it found it", stale, err)
		}
	}
	if err := move("bob", RefUpdate{Ref: "refs/heads/main", Old: c1, New: c2}, RefUpdate{Ref: "refs/heads/feature", New: c3},
		RefUpdate{Ref: "refs/heads/Zeta", New: c3}, RefUpdate{Ref: "refs/tags/v1", New: tagObject, Commit: c1}); err != nil {
		t.Fatal(err)
	}
	// In git's order, byte by byte.
	r := repositoryOf(t, pool, "finance", "nightly")
	want := []Ref{
		{Name: "refs/heads/Zeta", Commit: c3, MovedBy: "bob", MovedAt: at},
		{Name: "refs/heads/feature", Commit: c3, MovedBy: "bob", MovedAt: at},
		{Name: "refs/heads/main", Commit: c2, Protected: true, MovedBy: "bob", MovedAt: at},
		{Name: "refs/tags/v1", Commit: c1, Tag: tagObject, MovedBy: "bob", MovedAt: at},
	}
	if !sameRefs(r.Refs, want) {
		t.Errorf("after one push the refs read as\n%+v\nand not\n%+v", r.Refs, want)
	}
	if tag, _ := refNamed(r, "refs/tags/v1"); tag.Target() != tagObject {
		t.Errorf("an annotated tag names %s to git", tag.Target())
	}
	if err := move("bob", RefUpdate{Ref: "refs/tags/v1", Old: c1}); !errors.Is(err, ErrStaleRef) {
		t.Errorf("deleting an annotated tag by the commit it peels to answered %v, and git names it by the tag object", err)
	}

	others := []RefUpdate{{Ref: "refs/heads/feature", Old: c3}, {Ref: "refs/heads/Zeta", Old: c3}, {Ref: "refs/tags/v1", Old: tagObject}}
	if err := move("carol", append(others, RefUpdate{Ref: "refs/heads/main", Old: c2})...); !errors.Is(err, ErrDefaultBranch) {
		t.Errorf("a push deleting the default branch answered %v", err)
	}
	if r := repositoryOf(t, pool, "finance", "nightly"); !sameRefs(r.Refs, want) {
		t.Errorf("a push refused for deleting the default branch left the refs %+v", r.Refs)
	}
	if err := move("carol", others...); err != nil {
		t.Fatal(err)
	}
	r = repositoryOf(t, pool, "finance", "nightly")
	if !sameRefs(r.Refs, want[2:3]) {
		t.Errorf("once every other ref is deleted the refs read as %+v", r.Refs)
	}
}

// A push whose refs are not all where it found them moves none of them, even where the transaction
// it was made in goes on and commits: git's atomic push, which every push is.
func TestAPushMovesEveryRefOrNone(t *testing.T) {
	pool, _ := repositories(t)
	if err := updateRefs(t, pool, "finance", RefUpdate{Ref: "refs/heads/main", New: c1}); err != nil {
		t.Fatal(err)
	}
	var stale error
	err := pool.In(t.Context(), "finance", func(ctx context.Context, n *NS) error {
		stale = n.UpdateRefs(ctx, "nightly", "alice", time.Time{}, []RefUpdate{
			{Ref: "refs/heads/feature", New: c2},
			{Ref: "refs/heads/main", Old: c1, New: c2},
			{Ref: "refs/tags/v1", Old: c3, New: c2},
		})
		// And the caller goes on, and commits what else it wrote.
		return n.SaveWorkflow(ctx, "weekly", "main")
	})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(stale, ErrStaleRef) || !strings.Contains(stale.Error(), "refs/tags/v1") {
		t.Errorf("a push one of whose refs was stale answered %v", stale)
	}
	r := repositoryOf(t, pool, "finance", "nightly")
	if len(r.Refs) != 1 || r.Refs[0].Commit != c1 {
		t.Errorf("a push refused whole left the refs %+v", r.Refs)
	}
	if weekly := repositoryOf(t, pool, "finance", "weekly"); len(weekly.Refs) != 1 {
		t.Errorf("what the transaction wrote beside the refused push was not kept: %+v", weekly)
	}
}

// Two pushes of one ref at once, from where both found it: the second waits for the first, then
// finds the ref gone from under it and moves nothing, and so does every other of many.
func TestTwoPushesOfOneRefTakeTurnsAndOneWins(t *testing.T) {
	pool, _ := repositories(t)
	if err := updateRefs(t, pool, "finance", RefUpdate{Ref: "refs/heads/main", New: c1}); err != nil {
		t.Fatal(err)
	}

	moved, release := make(chan error), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- pool.In(context.WithoutCancel(t.Context()), "finance", func(ctx context.Context, n *NS) error {
			err := n.UpdateRefs(ctx, "nightly", "alice", time.Time{}, []RefUpdate{{Ref: "refs/heads/main", Old: c1, New: c2}})
			moved <- err
			<-release
			return err
		})
	}()
	if err := <-moved; err != nil {
		close(release)
		t.Fatal(err)
	}
	// The same ref, and another one: a push waits for the push before it whatever refs either moves,
	// which is what keeps two pushes moving two refs in two orders from waiting on each other.
	second, third := make(chan error, 1), make(chan error, 1)
	go func() {
		second <- updateRefs(t, pool, "finance", RefUpdate{Ref: "refs/heads/main", Old: c1, New: c3})
	}()
	go func() {
		third <- updateRefs(t, pool, "finance", RefUpdate{Ref: "refs/heads/other", New: c3})
	}()
	// An answer here is put back, for the reads below to find.
	select {
	case err := <-second:
		t.Errorf("the second push answered %v while the first held the repository", err)
		second <- err
	case err := <-third:
		t.Errorf("a push of another ref answered %v while the first held the repository", err)
		third <- err
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; !errors.Is(err, ErrStaleRef) {
		t.Errorf("the second push, once the first had moved the ref, answered %v", err)
	}
	if err := <-third; err != nil {
		t.Errorf("a push of another ref, once the first had moved its own, answered %v", err)
	}
	if main, _ := refNamed(repositoryOf(t, pool, "finance", "nightly"), "refs/heads/main"); main.Commit != c2 {
		t.Errorf("the ref names %s, and the first push moved it to %s", main.Commit, c2)
	}

	// Many at once, creating one tag and moving the branch from where all of them found it.
	var wg sync.WaitGroup
	results := make([]error, 8)
	for i := range results {
		wg.Go(func() {
			commit := fmt.Sprintf("%040x", i+10)
			results[i] = updateRefs(t, pool, "finance",
				RefUpdate{Ref: "refs/heads/main", Old: c2, New: commit}, RefUpdate{Ref: "refs/tags/race", New: commit})
		})
	}
	wg.Wait()
	won := -1
	for i, err := range results {
		switch {
		case err == nil && won >= 0:
			t.Errorf("pushes %d and %d both moved the ref", won, i)
		case err == nil:
			won = i
		case !errors.Is(err, ErrStaleRef):
			t.Errorf("push %d answered %v", i, err)
		}
	}
	r := repositoryOf(t, pool, "finance", "nightly")
	main, _ := refNamed(r, "refs/heads/main")
	tag, _ := refNamed(r, "refs/tags/race")
	if won < 0 || main.Commit != fmt.Sprintf("%040x", won+10) || tag.Commit != main.Commit {
		t.Errorf("push %d won, and the refs read as %+v", won, r.Refs)
	}
}

// A push holds its repository against other pushes and nothing else: a version recorded, or a grant
// written on the workflow, while one is being accepted is not made to wait for it.
func TestAPushHoldsUpNothingButAnotherPush(t *testing.T) {
	pool, super := repositories(t)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(), `insert into principals (id, kind) values ('alice', 'user')`); err != nil {
		t.Fatal(err)
	}

	moved, release := make(chan error), make(chan struct{})
	pushed := make(chan error, 1)
	go func() {
		pushed <- pool.In(context.WithoutCancel(t.Context()), "finance", func(ctx context.Context, n *NS) error {
			err := n.UpdateRefs(ctx, "nightly", "alice", time.Time{}, []RefUpdate{{Ref: "refs/heads/main", New: c1}})
			moved <- err
			<-release
			return err
		})
	}()
	defer func() { close(release); <-pushed }()
	if err := <-moved; err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = pool.In(ctx, "finance", func(ctx context.Context, n *NS) error {
		v := aVersion(c2, nil)
		v.Workflow = "nightly"
		if _, err := n.SaveVersion(ctx, v); err != nil {
			return err
		}
		_, err := n.tx.Exec(ctx, `insert into grants (id, namespace, workflow, principal, role, granted_by)
			values ('01JQ3M8T', 'finance', 'nightly', 'alice', 'viewer', 'alice')`)
		return err
	})
	if err != nil {
		t.Errorf("a version and a grant written while a push held the repository answered %v", err)
	}
}

// A command git would not send, or one the table could not hold, is refused as a mistake and never
// as a stale ref, and nothing of its push is moved.
func TestARefUpdateGitWouldNotSendIsRefused(t *testing.T) {
	pool, _ := repositories(t)
	for _, c := range []struct {
		what    string
		by      string
		updates []RefUpdate
	}{
		{"no command", "alice", nil},
		{"nobody", "", []RefUpdate{{Ref: "refs/heads/main", New: c1}}},
		{"a ref of another kind", "alice", []RefUpdate{{Ref: "refs/notes/commits", New: c1}}},
		{"HEAD", "alice", []RefUpdate{{Ref: "HEAD", New: c1}}},
		{"a name git refuses", "alice", []RefUpdate{{Ref: "refs/heads/a..b", New: c1}}},
		{"a name past the bound", "alice", []RefUpdate{{Ref: "refs/heads/" + strings.Repeat("a", MaxRefBytes), New: c1}}},
		{"an abbreviated commit", "alice", []RefUpdate{{Ref: "refs/heads/main", New: "a3f9c1e"}}},
		{"an upper-case commit", "alice", []RefUpdate{{Ref: "refs/heads/main", New: strings.Repeat("A", 40)}}},
		{"neither old nor new", "alice", []RefUpdate{{Ref: "refs/heads/main"}}},
		{"a deletion naming a commit", "alice", []RefUpdate{{Ref: "refs/heads/main", Old: c1, Commit: c1}}},
		{"a branch naming an annotated tag", "alice", []RefUpdate{{Ref: "refs/heads/main", New: tagObject, Commit: c1}}},
		{"one ref twice", "alice", []RefUpdate{{Ref: "refs/heads/main", New: c1}, {Ref: "refs/heads/main", New: c2}}},
	} {
		// Beside a command that would move a ref, so that a refusal is seen to move nothing.
		updates := c.updates
		if updates != nil {
			updates = append([]RefUpdate{{Ref: "refs/heads/fine", New: c3}}, updates...)
		}
		err := pool.In(t.Context(), "finance", func(ctx context.Context, n *NS) error {
			return n.UpdateRefs(ctx, "nightly", c.by, time.Time{}, updates)
		})
		if err == nil || errors.Is(err, ErrStaleRef) {
			t.Errorf("%s answered %v", c.what, err)
		}
	}
	if r := repositoryOf(t, pool, "finance", "nightly"); len(r.Refs) != 1 {
		t.Errorf("refused pushes moved refs: %+v", r.Refs)
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, n *NS) error {
		return n.UpdateRefs(ctx, "absent", "alice", time.Time{}, []RefUpdate{{Ref: "refs/heads/main", New: c1}})
	}); !errors.Is(err, ErrNoWorkflow) {
		t.Errorf("a push to no workflow answered %v", err)
	}
}

// What a ref may be named is decided twice, by UpdateRefs before it writes and by the table's check,
// and the two are one rule: the wire's refName, git check-ref-format's rules on refs/heads/ and
// refs/tags/, and MaxRefBytes.
func TestTheRefGrammarIsOneRuleInGoAndInTheTable(t *testing.T) {
	super, _ := database(t)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	for name, want := range map[string]bool{
		"refs/heads/main": true, "refs/tags/v2.1.0": true, "refs/heads/feature/vat-rounding": true,
		"refs/heads/@": true, "refs/heads/-x": true, "refs/heads/été": true, "refs/heads/a]b": true,
		"refs/heads/" + strings.Repeat("a", MaxRefBytes-len("refs/heads/")):   true,
		"refs/heads/" + strings.Repeat("a", MaxRefBytes-len("refs/heads/")+1): false,
		"HEAD": false, "refs/notes/x": false, "refs/heads/": false, "refs/heads/a b": false,
		"refs/heads/a~1": false, "refs/heads/a^": false, "refs/heads/a:b": false, "refs/heads/a?": false,
		"refs/heads/a*": false, "refs/heads/a[": false, `refs/heads/a\b`: false, "refs/heads/.x": false,
		"refs/heads/a/.x": false, "refs/heads/a..b": false, "refs/heads/a@{b": false, "refs/heads/a/": false,
		"refs/heads/a.": false, "refs/heads/a.lock": false, "refs/heads/a.lock/b": false, "refs/heads//a": false,
		"refs/heads/a\x7f": false, "refs/heads/a\x01": false, "refs/heads/a\t": false,
	} {
		var table bool
		if err := conn.QueryRow(t.Context(), `select git_ref_name($1)`, name).Scan(&table); err != nil {
			t.Fatal(err)
		}
		if got := checkRef(name) == nil; got != want || table != want {
			t.Errorf("%q is a ref name to Go %t and to the table %t, and to git %t", name, got, table, want)
		}
	}
	if checkRef("refs/heads/\xff") == nil {
		t.Error("a name that is not UTF-8 is a ref name to Go, and the table cannot hold it")
	}
}

// A pack is recorded receiving before its bytes are written, becomes live in the push's transaction,
// and is listed once it is: one of the same name received again, its bytes being the same, is
// receiving again unless it is live, and one no longer receiving cannot be made live.
func TestAPackIsReceivedThenMadeLive(t *testing.T) {
	pool, super := repositories(t)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	key := repositoryOf(t, pool, "finance", "nightly").Key
	in := func(fn func(ctx context.Context, n *NS) error) error { return pool.In(t.Context(), "finance", fn) }
	receive := func(p Pack) (string, error) {
		var got string
		err := in(func(ctx context.Context, n *NS) error {
			var err error
			got, err = n.ReceivePack(ctx, "nightly", p)
			return err
		})
		return got, err
	}
	live := func(name string) error {
		return in(func(ctx context.Context, n *NS) error { return n.PackLive(ctx, "nightly", name) })
	}
	state := func(name string) (s string, superseded bool) {
		t.Helper()
		if err := conn.QueryRow(t.Context(), `select state, superseded_at is not null from git_packs where name = $1`, name).Scan(&s, &superseded); err != nil {
			t.Fatal(err)
		}
		return s, superseded
	}

	// Received first as what it is not, as a write refused for its size leaves it, then as it is: it
	// is recorded as it is.
	if _, err := receive(Pack{Name: c1, Size: 4095, Objects: 11}); err != nil {
		t.Fatal(err)
	}
	first := Pack{Name: c1, Size: 4096, Objects: 12}
	if got, err := receive(first); err != nil || got != key {
		t.Fatalf("receiving a pack answered %q, %v, and the repository's key is %s", got, err, key)
	}
	if r := repositoryOf(t, pool, "finance", "nightly"); len(r.Packs) != 0 {
		t.Errorf("a pack still receiving is listed: %+v", r.Packs)
	}
	if err := live(c1); err != nil {
		t.Fatal(err)
	}
	if err := live(c1); err != nil {
		t.Errorf("a pack live already, made live again, answered %v", err)
	}
	if _, err := receive(first); err != nil {
		t.Fatal(err)
	}
	if s, _ := state(c1); s != "live" {
		t.Errorf("a live pack received again is %s", s)
	}
	r := repositoryOf(t, pool, "finance", "nightly")
	if len(r.Packs) != 1 || r.Packs[0].Name != c1 || r.Packs[0].Size != 4096 || r.Packs[0].Objects != 12 {
		t.Errorf("the live packs read as %+v", r.Packs)
	}

	// A repack superseded it, and a push carrying the same objects in the same order writes the
	// same pack: it is receiving again, so that the collection of superseded packs leaves it.
	if _, err := conn.Exec(t.Context(), `update git_packs set state = 'superseded', superseded_at = now() where name = $1`, c1); err != nil {
		t.Fatal(err)
	}
	if err := live(c1); !errors.Is(err, ErrNoPack) {
		t.Errorf("a superseded pack made live answered %v", err)
	}
	if _, err := conn.Exec(t.Context(), `update git_packs set created_at = now() - interval '2 days' where name = $1`, c1); err != nil {
		t.Fatal(err)
	}
	if _, err := receive(first); err != nil {
		t.Fatal(err)
	}
	if s, superseded := state(c1); s != "receiving" || superseded {
		t.Errorf("a superseded pack received again is %s, superseded %t", s, superseded)
	}
	// Receiving from now, so that the collection of packs receiving past the grace leaves the bytes
	// this push is about to write.
	var fresh bool
	if err := conn.QueryRow(t.Context(), `select created_at > now() - interval '1 minute' from git_packs where name = $1`, c1).Scan(&fresh); err != nil || !fresh {
		t.Errorf("a pack received again is receiving from when it was first received: %v", err)
	}

	if err := live(c2); !errors.Is(err, ErrNoPack) {
		t.Errorf("a pack never received made live answered %v", err)
	}
	for _, bad := range []Pack{{Name: "pack-" + c2, Size: 1}, {Name: c2}, {Name: c2, Size: 1, Objects: -1}} {
		if _, err := receive(bad); err == nil {
			t.Errorf("a pack %+v was received", bad)
		}
	}
	err = in(func(ctx context.Context, n *NS) error {
		_, err := n.ReceivePack(ctx, "absent", Pack{Name: c2, Size: 1})
		return err
	})
	if !errors.Is(err, ErrNoWorkflow) {
		t.Errorf("a pack of no workflow answered %v", err)
	}
}

// A workflow is not deleted while a pack names its repository, since the pack's bytes would be left
// in the store with nothing naming them, and its refs go with it; its repository key is never
// changed, since every pack of it is found under that key.
func TestARepositoryKeepsItsKeyAndItsPacksKeepIt(t *testing.T) {
	pool, super := repositories(t)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if err := updateRefs(t, pool, "finance", RefUpdate{Ref: "refs/heads/main", New: c1}); err != nil {
		t.Fatal(err)
	}
	var pg *pgconn.PgError
	_, err = conn.Exec(t.Context(), `update workflows set repository = $1 where namespace = 'finance'`, strings.Repeat("a", 32))
	if !errors.As(err, &pg) || pg.Code != checkViolation || !strings.Contains(pg.Message, "never changed") {
		t.Errorf("changing a repository's key answered %v", err)
	}

	if err := pool.In(t.Context(), "finance", func(ctx context.Context, n *NS) error {
		_, err := n.ReceivePack(ctx, "nightly", Pack{Name: c1, Size: 1})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(t.Context(), `delete from workflows where namespace = 'finance' and name = 'nightly'`)
	if !errors.As(err, &pg) || pg.Code != "23503" {
		t.Errorf("deleting a workflow whose repository holds a pack answered %v", err)
	}
	for _, stmt := range []string{
		`delete from git_packs where namespace = 'finance'`,
		`delete from workflows where namespace = 'finance' and name = 'nightly'`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	var refs int
	if err := conn.QueryRow(t.Context(), `select count(*) from workflow_refs where namespace = 'finance'`).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if refs != 0 {
		t.Errorf("%d refs outlived their workflow", refs)
	}
}

// A namespace reads and moves its own repositories and nobody else's, through the same names: its
// handle reads its own refs and packs, a statement naming another's rows reads none and writes none,
// and a push or a pack naming a workflow only another holds is a push to no workflow.
func TestARepositoryIsReadAndMovedInItsOwnNamespaceAlone(t *testing.T) {
	pool, _ := repositories(t)
	for _, namespace := range []string{"finance", "team-ops"} {
		if err := updateRefs(t, pool, namespace, RefUpdate{Ref: "refs/heads/main", New: c1}); err != nil {
			t.Fatal(err)
		}
		if err := pool.In(t.Context(), namespace, func(ctx context.Context, n *NS) error {
			if _, err := n.ReceivePack(ctx, "nightly", Pack{Name: c2, Size: 1}); err != nil {
				return err
			}
			return n.PackLive(ctx, "nightly", c2)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.In(t.Context(), "team-ops", func(ctx context.Context, n *NS) error {
		return n.SaveWorkflow(ctx, "theirs", "main")
	}); err != nil {
		t.Fatal(err)
	}
	theirs := repositoryOf(t, pool, "team-ops", "nightly")

	err := pool.In(t.Context(), "finance", func(ctx context.Context, n *NS) error {
		for table, want := range map[string]int{"workflow_refs": 1, "git_packs": 1} {
			var own, all int
			// No namespace in the statement: the policy is what filters.
			if err := n.tx.QueryRow(ctx, `select count(*) filter (where namespace = 'finance'), count(*) from `+
				pgx.Identifier{table}.Sanitize()).Scan(&own, &all); err != nil {
				return err
			}
			if own != want || all != want {
				t.Errorf("through finance's handle %s reads %d rows, %d of them finance's", table, all, own)
			}
		}
		for what, stmt := range map[string]string{
			"a ref moved":       `update workflow_refs set commit = '` + c3 + `' where namespace = 'team-ops'`,
			"a ref deleted":     `delete from workflow_refs where namespace = 'team-ops'`,
			"a pack superseded": `update git_packs set state = 'superseded', superseded_at = now() where namespace = 'team-ops'`,
		} {
			tag, err := n.tx.Exec(ctx, stmt)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("%s in team-ops through finance's handle", what)
			}
		}
		for what, stmt := range map[string]string{
			"a ref": `insert into workflow_refs (namespace, workflow, ref, commit, moved_by, moved_at)
				values ('team-ops', 'nightly', 'refs/heads/planted', '` + c3 + `', 'mallory', now())`,
			"a pack": `insert into git_packs (namespace, repository, name, size, objects)
				values ('team-ops', '` + theirs.Key + `', '` + c3 + `', 1, 0)`,
		} {
			sp, err := n.tx.Begin(ctx)
			if err != nil {
				return err
			}
			if _, err := sp.Exec(ctx, stmt); err == nil {
				t.Errorf("%s was written into team-ops through finance's handle", what)
			}
			sp.Rollback(ctx)
		}
		if _, err := n.Repository(ctx, "theirs"); !errors.Is(err, ErrNoWorkflow) {
			t.Errorf("finance reading team-ops's repository answered %v", err)
		}
		if err := n.UpdateRefs(ctx, "theirs", "mallory", time.Time{}, []RefUpdate{{Ref: "refs/heads/main", New: c3}}); !errors.Is(err, ErrNoWorkflow) {
			t.Errorf("finance pushing to team-ops's repository answered %v", err)
		}
		if _, err := n.ReceivePack(ctx, "theirs", Pack{Name: c3, Size: 1}); !errors.Is(err, ErrNoWorkflow) {
			t.Errorf("finance writing a pack into team-ops's repository answered %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after := repositoryOf(t, pool, "team-ops", "nightly")
	if fmt.Sprint(after) != fmt.Sprint(theirs) {
		t.Errorf("team-ops's repository went from\n%+v\nto\n%+v", theirs, after)
	}
}
