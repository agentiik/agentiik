package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// packStates answers each pack of nightly's repository by name, with its state.
func packStates(t *testing.T, super string) map[string]string {
	t.Helper()
	conn := dbtest.Superuser(t, super)
	rows, err := conn.Query(t.Context(), `select name, state from git_packs where namespace = 'finance'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, state string
		if err := rows.Scan(&name, &state); err != nil {
			t.Fatal(err)
		}
		out[name] = state
	}
	return out
}

// A repack writes every object of the live packs into one pack, each once, which is then the one pack
// a fetch reads; the packs it replaced are superseded and still in the store, so that a fetch that
// began before reads on through them.
func TestARepackWritesEveryObjectOnceAndLeavesTheOldPacksToTheFetchesReadingThem(t *testing.T) {
	in, super := anInstallationWithItsSuperuser(t)
	dir := history(t)

	first := in.put(t, unpack(t, git(t, dir, []byte("HEAD~1\n"), "pack-objects", "--revs", "--stdout", "-q"), nil))
	r := in.live(t, first.Name)
	objects, err := in.store.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	second := in.put(t, unpack(t, git(t, dir, []byte("HEAD\n^HEAD~1\n"), "pack-objects", "--revs", "--thin", "--stdout", "-q"), objects))
	objects.Close()
	// The whole history again, every object of the first two packs a second time.
	again := in.put(t, unpack(t, git(t, dir, []byte("HEAD\n"), "pack-objects", "--revs", "--stdout", "-q"), nil))
	r = in.live(t, second.Name, again.Name)
	if len(r.Packs) != 3 {
		t.Fatalf("the repository holds the packs %+v", r.Packs)
	}
	reading, err := in.store.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	defer reading.Close()

	pack, replaced, err := in.store.Repack(t.Context(), in.pool, "finance", "nightly")
	if err != nil {
		t.Fatal(err)
	}
	want := objectsOf(t, dir, "HEAD")
	if replaced != 3 || pack.Objects != len(want) {
		t.Errorf("the repack replaced %d packs with one of %d objects, where the history holds %d", replaced, pack.Objects, len(want))
	}
	after := in.live(t)
	if len(after.Packs) != 1 || after.Packs[0].Name != pack.Name {
		t.Fatalf("after the repack the repository reads the packs %+v", after.Packs)
	}
	states := packStates(t, super)
	for _, p := range r.Packs {
		if states[p.Name] != "superseded" {
			t.Errorf("pack %s is %s after the repack", p.Name, states[p.Name])
		}
		if _, err := os.Stat(filepath.Join(in.dir, filepath.FromSlash(PackKey("finance", r.Key, p.Name)))); err != nil {
			t.Errorf("pack %s left the store at the repack: %s", p.Name, err)
		}
	}

	repacked, err := in.store.Open(after)
	if err != nil {
		t.Fatal(err)
	}
	defer repacked.Close()
	holds(t, repacked, want)
	holds(t, reading, want)

	if pack, replaced, err := in.store.Repack(t.Context(), in.pool, "finance", "nightly"); err != nil || replaced != 0 || pack.Name != "" {
		t.Errorf("a repository of one pack was repacked as %+v, %d, %v", pack, replaced, err)
	}
}

// The collection takes a superseded pack, and a receiving one no push made live, once each is past
// the grace, deleting their files before it forgets them; a live pack, and one within the grace, it
// leaves. A push sending again a pack being collected is refused rather than handed its files.
func TestTheCollectionTakesThePacksNoFetchReadsAnyMore(t *testing.T) {
	in, super := anInstallationWithItsSuperuser(t)
	dir := history(t)

	first := in.put(t, unpack(t, git(t, dir, []byte("HEAD~1\n"), "pack-objects", "--revs", "--stdout", "-q"), nil))
	whole := in.put(t, unpack(t, git(t, dir, []byte("HEAD\n"), "pack-objects", "--revs", "--stdout", "-q"), nil))
	r := in.live(t, first.Name, whole.Name)
	pack, _, err := in.store.Repack(t.Context(), in.pool, "finance", "nightly")
	if err != nil {
		t.Fatal(err)
	}
	refused := in.put(t, unpack(t, git(t, dir, []byte("HEAD~1\n^HEAD~1^{tree}\n"), "pack-objects", "--revs", "--stdout", "-q"), nil))
	recent := in.put(t, unpack(t, git(t, dir, []byte("HEAD\n^HEAD~1\n"), "pack-objects", "--revs", "--stdout", "-q"), nil))

	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `update git_packs set superseded_at = now() - interval '2 days' where state = 'superseded'`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `update git_packs set created_at = now() - interval '2 days' where name = $1`, refused.Name); err != nil {
		t.Fatal(err)
	}

	claimed, err := in.pool.CollectablePacks(t.Context(), 24*time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range claimed {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	wanted := []string{first.Name, whole.Name, refused.Name}
	sort.Strings(wanted)
	if len(names) != 3 || names[0] != wanted[0] || names[1] != wanted[1] || names[2] != wanted[2] {
		t.Fatalf("the collection claimed %v, where it is due %v", names, wanted)
	}

	// A push sending one of them again, by its checksum, while its files go.
	err = in.pool.In(t.Context(), "finance", func(ctx context.Context, n *db.NS) error {
		_, err := n.ReceivePack(ctx, "nightly", first)
		return err
	})
	if !errors.Is(err, db.ErrPackCollected) {
		t.Errorf("a pack received again while collected answered %v", err)
	}

	for _, c := range claimed {
		if err := in.store.Collect(t.Context(), c); err != nil {
			t.Fatal(err)
		}
		// Twice, as a pass that died after deleting the files does again.
		if err := in.store.Collect(t.Context(), c); err != nil {
			t.Errorf("collecting %s again answered %v", c.Name, err)
		}
	}
	if forgotten, err := in.pool.PacksCollected(t.Context(), claimed); err != nil || forgotten != 3 {
		t.Errorf("forgetting the collected packs answered %d, %v", forgotten, err)
	}
	states := packStates(t, super)
	if len(states) != 2 || states[pack.Name] != "live" || states[recent.Name] != "receiving" {
		t.Errorf("after the collection the packs are %v", states)
	}
	for _, name := range wanted {
		for _, key := range []string{PackKey("finance", r.Key, name), IdxKey("finance", r.Key, name)} {
			if _, err := os.Stat(filepath.Join(in.dir, filepath.FromSlash(key))); !os.IsNotExist(err) {
				t.Errorf("%s is still in the store: %v", key, err)
			}
		}
	}
	if again, err := in.pool.CollectablePacks(t.Context(), 24*time.Hour, 100); err != nil || len(again) != 0 {
		t.Errorf("a second collection claimed %v, %v", again, err)
	}
}
