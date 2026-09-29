package api_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/purge"
	"github.com/agentiik/agentiik/repo/store"
)

// "Repack and garbage-collect a repository's packfiles without interrupting a clone in progress": a
// repository pushed to more times than the bound is repacked by the controller's pass into one pack,
// and a clone made before goes on fetching, as a fresh clone does; the packs replaced are kept for
// the grace, then collected, and every clone is whole throughout.
func TestARepackAndTheCollectionLeaveEveryCloneWhole(t *testing.T) {
	g := servingGit(t, holdingRepositories())
	work := g.newClone("alice")
	work.write("agentiik.yaml", workflowDocument)
	work.commit("first")
	work.must("push", "-q", "origin", "main")
	before := g.cloned("bob")
	for i := range db.RepackAbove {
		work.write("scripts/step.sh", fmt.Sprintf("echo %d\n", i))
		work.commit(fmt.Sprintf("push %d", i))
		work.must("push", "-q", "origin", "main")
	}
	head := work.must("rev-parse", "HEAD")

	packs, err := store.New(artifact.Dir(g.objects))
	if err != nil {
		t.Fatal(err)
	}
	p := &purge.Purger{Pool: g.pool, Objects: artifact.Dir(g.objects), Packs: packs}
	purged, err := p.Pass(t.Context())
	if err != nil || purged.Repacked != 1 || purged.Packs != 0 {
		t.Fatalf("the pass repacked %d repositories and collected %d packs: %v", purged.Repacked, purged.Packs, err)
	}
	var live int
	if err := g.pool.In(t.Context(), "finance", func(ctx context.Context, n *db.NS) error {
		r, err := n.Repository(ctx, "monthly-invoicing")
		live = len(r.Packs)
		return err
	}); err != nil || live != 1 {
		t.Fatalf("after the repack the repository reads %d live packs: %v", live, err)
	}
	wholeAt := func(c *clone, what string) {
		t.Helper()
		if got := c.must("rev-parse", "origin/main"); got != head {
			t.Errorf("%s reads main at %s, where it is %s", what, got, head)
		}
		c.must("fsck", "--strict", "--no-dangling")
	}
	before.must("fetch", "-q", "origin")
	wholeAt(before, "a clone made before the repack, fetching after it")
	wholeAt(g.cloned("bob"), "a clone made after the repack")

	if _, err := dbtest.Superuser(t, g.super).Exec(t.Context(), `update git_packs set superseded_at = now() - interval '2 days' where state = 'superseded'`); err != nil {
		t.Fatal(err)
	}
	purged, err = p.Pass(t.Context())
	if err != nil || purged.Repacked != 0 || purged.Packs != db.RepackAbove+1 {
		t.Fatalf("the pass after the grace repacked %d repositories and collected %d packs: %v", purged.Repacked, purged.Packs, err)
	}
	wholeAt(g.cloned("bob"), "a clone made after the collection")
	before.must("fetch", "-q", "origin")
	wholeAt(before, "a clone made before the repack, fetching after the collection")
}
