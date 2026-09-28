package version_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/repo"
	"github.com/agentiik/agentiik/version"
)

// objects is a repository holding the objects put in it, which is what a pre-receive hook reads a
// pushed tip's tree through.
type objects map[repo.ID]held

type held struct {
	t    repo.Type
	data []byte
}

func (o objects) put(t repo.Type, data []byte) repo.ID {
	id := repo.HashObject(t, data)
	o[id] = held{t: t, data: data}
	return id
}

func (o objects) tree(t *testing.T, entries ...repo.TreeEntry) repo.ID {
	t.Helper()
	data, err := repo.EncodeTree(entries)
	if err != nil {
		t.Fatal(err)
	}
	return o.put(repo.TypeTree, data)
}

func (o objects) OpenObject(_ context.Context, id repo.ID) (repo.ObjectReader, error) {
	h, ok := o[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", repo.ErrMissing, id)
	}
	return &reading{Reader: bytes.NewReader(h.data), t: h.t, size: int64(len(h.data))}, nil
}

type reading struct {
	*bytes.Reader
	t    repo.Type
	size int64
}

func (r *reading) Close() error    { return nil }
func (r *reading) Type() repo.Type { return r.t }
func (r *reading) Size() int64     { return r.size }

// A tree read out of git's objects, as the hook will read a pushed tip's, is judged as the push route
// judges one: a symbolic link and a submodule, which a git tree holds and a push of files cannot, are
// each refused by their rule, and a tree holding neither resolves to its graph.
func TestATreeReadOutOfGitIsJudgedAsAPushedOne(t *testing.T) {
	o := objects{}
	fragments := o.tree(t, repo.TreeEntry{Name: "bricks.yaml", Mode: repo.ModeFile, ID: o.put(repo.TypeBlob, aRepository()["fragments/bricks.yaml"].Data)})
	entry := repo.TreeEntry{Name: "agentiik.yaml", Mode: repo.ModeFile, ID: o.put(repo.TypeBlob, aRepository()["agentiik.yaml"].Data)}
	dir := repo.TreeEntry{Name: "fragments", Mode: repo.ModeTree, ID: fragments}

	for _, c := range []struct {
		extra repo.TreeEntry
		rule  graph.Rule
	}{
		{repo.TreeEntry{Name: "current", Mode: repo.ModeSymlink, ID: o.put(repo.TypeBlob, []byte("agentiik.yaml"))}, version.RuleSymlinkInTree},
		{repo.TreeEntry{Name: "rounding", Mode: repo.ModeSubmodule, ID: repo.HashObject(repo.TypeCommit, []byte("elsewhere"))}, version.RuleSubmoduleInTree},
	} {
		root := o.tree(t, entry, c.extra, dir)
		_, err := version.Check(t.Context(), repo.NewTreeFS(t.Context(), o, root), everything())
		if r := refusedBy(t, err, c.rule); r.At.File != c.extra.Name {
			t.Errorf("%s is refused naming %q", c.extra.Name, r.At.File)
		}
	}

	checked, err := version.Check(t.Context(), repo.NewTreeFS(t.Context(), o, o.tree(t, entry, dir)), everything())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checked.Resolved(); err != nil {
		t.Fatal(err)
	}
}
