package repo

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestAPackGitWritesIsReadWholeAndWrittenAsOneGitReads(t *testing.T) {
	s := newSample(t)
	want := s.objects(t)
	for _, c := range []struct {
		name string
		args []string
		kind byte
	}{
		{"with its deltas made against IDs", nil, kindRefDelta},
		{"with its deltas made against offsets", []string{"--delta-base-offset"}, kindOfsDelta},
	} {
		t.Run(c.name, func(t *testing.T) {
			pack := s.pack(t, "", append([]string{"--all"}, c.args...)...)
			if n := kinds(t, pack)[c.kind]; n == 0 {
				t.Fatalf("git wrote a pack with no entry of kind %d, and this test is about reading them", c.kind)
			}
			visited := map[ID]Type{}
			out, u := unpack(t, pack, UnpackOptions{Visit: func(id ID, typ Type, data []byte) error {
				if got := HashObject(typ, data); got != id {
					t.Errorf("Visit was handed %s as %s", got, id)
				}
				visited[id] = typ
				return nil
			}})
			got := map[ID]Type{}
			for _, o := range u.Objects {
				got[o.ID] = o.Type
			}
			if !maps.Equal(got, want) {
				t.Fatalf("read %d objects, and git says the pack holds %d", len(got), len(want))
			}
			for id, typ := range want {
				if _, ok := visited[id]; ok != (typ != TypeBlob) {
					t.Errorf("the %s %s was handed to Visit: %t", typ, id, ok)
				}
			}
			if k := kinds(t, out); k[kindOfsDelta]+k[kindRefDelta] != 0 {
				t.Errorf("the pack written holds %d deltas, and it holds every object whole", k[kindOfsDelta]+k[kindRefDelta])
			}
			st := store(t, out, u)
			dir := t.TempDir()
			for name, data := range map[string][]byte{"ours.pack": out, "ours.idx": st.idx} {
				if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// git verify-pack reads the pack through the index, checking every object's CRC32 and
			// ID against it, and the pack's checksum.
			verified := string(git(t, dir, nil, "verify-pack", "-v", "ours.idx"))
			if !strings.Contains(verified, "non delta: "+strconv.Itoa(len(want))+" objects") {
				t.Errorf("git verify-pack says:\n%s", verified)
			}
			// git index-pack writes the index of the same pack, byte for byte.
			git(t, dir, nil, "index-pack", "--strict", "-o", "git.idx", "ours.pack")
			theirs, err := os.ReadFile(filepath.Join(dir, "git.idx"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(theirs, st.idx) {
				t.Errorf("git index-pack wrote an index of %d bytes that differs from the %d written here", len(theirs), len(st.idx))
			}
			// And a repository made of the pack and the index is one git fsck --strict passes.
			b := bare(t, s.refs(t), st)
			if out, err := gitErr(b, nil, nil, "fsck", "--strict", "--full", "--no-dangling"); err != nil || len(out) > 0 {
				t.Errorf("git fsck --strict on a repository of the pack written here: %v\n%s", err, out)
			}
			if log := git(t, b, nil, "log", "--format=%s", "main"); !strings.Contains(string(log), "merge side") {
				t.Errorf("git log reads:\n%s", log)
			}
		})
	}
}

func TestAThinPackIsResolvedAgainstWhatTheRepositoryHolds(t *testing.T) {
	s := newSample(t)
	baseOut, baseU := unpack(t, s.pack(t, s.second.String()+"\n"), UnpackOptions{})
	base := store(t, baseOut, baseU)
	thin := s.pack(t, "^"+s.second.String()+"\n", "--all", "--thin", "--delta-base-offset")
	k := kinds(t, thin)
	if k[kindRefDelta] == 0 {
		t.Fatalf("git wrote a thin pack with no delta against an object it left out: %v", k)
	}
	out, u := unpack(t, thin, UnpackOptions{Bases: base})
	want := s.objects(t, "--all", "^"+s.second.String())
	got := map[ID]Type{}
	for _, o := range u.Objects {
		got[o.ID] = o.Type
	}
	if !maps.Equal(got, want) {
		t.Fatalf("read %d objects from the thin pack, and git says it holds %d", len(got), len(want))
	}
	for id := range got {
		if base.Has(id) {
			t.Errorf("the pack written holds %s, a base the repository holds already", id)
		}
	}
	b := bare(t, s.refs(t), base, store(t, out, u))
	if out, err := gitErr(b, nil, nil, "fsck", "--strict", "--full", "--no-dangling"); err != nil || len(out) > 0 {
		t.Errorf("git fsck --strict on a repository of the base and the thin pack resolved: %v\n%s", err, out)
	}

	for _, bases := range []Lookup{nil, noBases{}} {
		_, err := Unpack(context.Background(), bytes.NewReader(thin), int64(len(thin)), io.Discard, UnpackOptions{Bases: bases})
		if err == nil || !strings.Contains(err.Error(), "neither the pack nor the repository holds") {
			t.Errorf("a thin pack with nothing to resolve it against is %v", err)
		}
	}
}

func TestAStoredPackServesEveryObjectAsGitDoes(t *testing.T) {
	s := newSample(t)
	want := s.objects(t)
	contents := s.contents(t, slices.Collect(maps.Keys(want)))
	gitPack := s.pack(t, "", "--all", "--delta-base-offset")
	out, u := unpack(t, gitPack, UnpackOptions{})
	ours := store(t, out, u)

	// A pack git wrote, deltas and all, read through the index git writes for it.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git.pack"), gitPack, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, nil, "index-pack", "-o", "git.idx", "git.pack")
	gitIdx, err := os.ReadFile(filepath.Join(dir, "git.idx"))
	if err != nil {
		t.Fatal(err)
	}
	x, err := ParseIdx(gitIdx)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := OpenPack(bytes.NewReader(gitPack), int64(len(gitPack)), x)
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]*Pack{"written here": ours.Pack, "written by git": theirs} {
		for id, typ := range want {
			gotType, data, err := ReadObject(context.Background(), p, id, 1<<30)
			if err != nil {
				t.Fatalf("the pack %s: %s", name, err)
			}
			if gotType != typ || !bytes.Equal(data, contents[id]) {
				t.Errorf("the pack %s serves %s as a %s of %d bytes, and git as a %s of %d", name, id, gotType, len(data), typ, len(contents[id]))
			}
		}
		if _, err := p.OpenObject(context.Background(), ID{1}); !errors.Is(err, ErrMissing) {
			t.Errorf("the pack %s answers an object it does not hold with %v", name, err)
		}
	}
}

func TestAStoredEntryIsCopiedAsItIsAndCheckedAsItIs(t *testing.T) {
	s := newSample(t)
	out, u := unpack(t, s.pack(t, "", "--all"), UnpackOptions{})
	from := store(t, out, u)

	var copied bytes.Buffer
	w, err := NewPackWriter(&copied, len(u.Objects))
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range slices.Backward(u.Objects) {
		if err := w.Copy(from.Pack, o.ID); err != nil {
			t.Fatal(err)
		}
	}
	checksum, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	for i, o := range w.Objects() {
		if o.ID != u.Objects[i].ID || o.CRC32 != u.Objects[i].CRC32 {
			t.Fatalf("the copy of %s has the CRC32 %08x, and the entry copied %08x", o.ID, o.CRC32, u.Objects[i].CRC32)
		}
	}
	b := bare(t, s.refs(t), store(t, copied.Bytes(), &Unpacked{Objects: w.Objects(), Checksum: checksum}))
	if out, err := gitErr(b, nil, nil, "fsck", "--strict", "--full", "--no-dangling"); err != nil || len(out) > 0 {
		t.Errorf("git fsck --strict on a repository of the pack copied: %v\n%s", err, out)
	}

	// One byte of a stored entry changed is found by its CRC32 as it is copied.
	o := u.Objects[0]
	broken := slices.Clone(out)
	broken[o.Offset+4] ^= 0xff
	x, err := ParseIdx(from.idx)
	if err != nil {
		t.Fatal(err)
	}
	p, err := OpenPack(bytes.NewReader(broken), int64(len(broken)), x)
	if err != nil {
		t.Fatal(err)
	}
	w, _ = NewPackWriter(io.Discard, 1)
	if err := w.Copy(p, o.ID); err == nil || !strings.Contains(err.Error(), "CRC32") {
		t.Errorf("an entry changed in the store is copied with %v", err)
	}
	if _, err := w.Close(); err == nil {
		t.Error("a pack whose copy failed is closed as if it had not")
	}
}

func TestAPackWrittenHereIsReadByGitIndexPack(t *testing.T) {
	var pack bytes.Buffer
	w, err := NewPackWriter(&pack, 3)
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := w.Add(TypeBlob, []byte("hello\n"))
	tree, err := EncodeTree([]TreeEntry{{Name: "hello.txt", Mode: ModeFile, ID: blob}})
	if err != nil {
		t.Fatal(err)
	}
	treeID, _ := w.Add(TypeTree, tree)
	commit, err := (&Commit{Tree: treeID, Author: sig, Committer: sig, Message: "hello\n"}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	commitID, _ := w.Add(TypeCommit, commit)
	if _, err := w.Add(TypeBlob, []byte("one too many")); err == nil {
		t.Error("a pack begun with three objects takes a fourth")
	}
	checksum, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.pack"), pack.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	printed := string(git(t, dir, nil, "index-pack", "--strict", "-o", "p.idx", "p.pack"))
	if strings.TrimSpace(printed) != hex.EncodeToString(checksum[:]) {
		t.Errorf("git index-pack names the pack %s, and its checksum is %s", printed, hex.EncodeToString(checksum[:]))
	}
	b := bare(t, map[string]ID{"refs/heads/main": commitID}, store(t, pack.Bytes(), &Unpacked{Objects: w.Objects(), Checksum: checksum}))
	if show := string(git(t, b, nil, "show", "main:hello.txt")); show != "hello\n" {
		t.Errorf("git reads hello.txt as %q", show)
	}

	short, _ := NewPackWriter(io.Discard, 2)
	short.Add(TypeBlob, nil)
	if _, err := short.Close(); err == nil {
		t.Error("a pack begun with two objects is closed with one")
	}
	twice, _ := NewPackWriter(io.Discard, 2)
	twice.Add(TypeBlob, []byte("same"))
	twice.Add(TypeBlob, []byte("same"))
	if _, err := twice.Close(); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("a pack holding one object twice is closed with %v", err)
	}
}

// sig is who writes the objects the tests build.
var sig = Signature{Name: "Ada Lovelace", Email: "ada@example.com", When: 1700000000, Zone: "+0100"}
