package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/agentiik/agentiik/api"
	versions "github.com/agentiik/agentiik/version"
)

const (
	taggedCommit = "c41d9e2a7b3f5e8d1c0a9b6e4f2d8c7a5b3e1f09"
	headCommit   = "5d0b7e2c9a4f1e3d8c6b0a2f4e6d8c0b2a4f6e8d"
)

// aLibraryInstallation serves finance/common as the tree route does: its tag v2.1.0 at one commit,
// its branch main at another, each a version holding the library's root and the file it includes.
// It counts the files it was asked for.
func aLibraryInstallation(t *testing.T) (remote, *atomic.Int32) {
	t.Helper()
	files := map[string]string{
		"agentiik.yaml":   "include:\n  - path: ./blocks/api.yaml\n",
		"blocks/api.yaml": ".api-brick:\n  timeout: 2m\n",
		"README.md":       "never read\n",
	}
	var fetched atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const route = "/api/v1/finance/workflows/common/tree/"
		if r.Header.Get("Authorization") != "Bearer the-token" || !strings.HasPrefix(r.URL.Path, route) {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"no such thing, or not yours"}`))
			return
		}
		var commit string
		switch ref := strings.TrimPrefix(r.URL.Path, route); ref {
		case "refs/tags/v2.1.0", taggedCommit:
			commit = taggedCommit
		case "main", "refs/heads/main", headCommit:
			commit = headCommit
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"names no branch, no tag and no version of the workflow"}`))
			return
		}
		if path, asked := r.URL.Query()["path"]; asked {
			fetched.Add(1)
			w.Write([]byte(files[path[0]]))
			return
		}
		tree := api.Tree{Commit: commit}
		for path, body := range files {
			tree.Entries = append(tree.Entries, api.TreeFile{Path: path, Mode: "0644", Size: int64(len(body))})
		}
		json.NewEncoder(w).Encode(tree)
	}))
	t.Cleanup(server.Close)
	return remote{base: server.URL, token: "the-token"}, &fetched
}

func includingCommon(ref string) fstest.MapFS {
	return fstest.MapFS{"agentiik.yaml": &fstest.MapFile{Data: []byte(`apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing }
include:
  - workflow: finance/common
    ref: ` + ref + `
steps:
  normalize:
    extends: .api-brick
    image: docker.io/library/alpine@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    script: ["./normalize.sh"]
    outputs: [ok]
`)}}
}

// agk push judges a workflow include as the hook will, reading the library on the installation it
// pushes to: at a tag or at a commit written whole, the files resolution reads and those alone, kept
// as the hook keeps them.
func TestAWorkflowIncludeIsReadThroughTheInstallation(t *testing.T) {
	at, fetched := aLibraryInstallation(t)
	for ref, commit := range map[string]string{"v2.1.0": taggedCommit, taggedCommit: taggedCommit} {
		before := fetched.Load()
		checked, err := versions.Check(context.Background(), includingCommon(ref), versions.Checking{Resolvers: versions.Resolvers{Include: at.include}})
		if err != nil {
			t.Errorf("an include at %s was refused: %v", ref, err)
			continue
		}
		kept := checked.Version.Libraries["finance/common@"+ref]
		if kept.Commit != commit || len(kept.Files) != 2 || fetched.Load()-before != 2 {
			t.Errorf("an include at %s kept %s and %d files, fetching %d", ref, kept.Commit, len(kept.Files), fetched.Load()-before)
		}
	}

	// A branch is refused as the hook refuses it, whether it is written in full or goes unfound
	// as a tag, and so are a commit abbreviated and a ref naming nothing.
	for ref, says := range map[string]string{
		"refs/heads/main": "a workflow include is pinned to a tag or a commit",
		"main":            "names no tag of finance/common",
		taggedCommit[:9]:  "a workflow include names a commit whole",
		"v9":              "names no tag of finance/common",
	} {
		_, err := versions.Check(context.Background(), includingCommon(ref), versions.Checking{Resolvers: versions.Resolvers{Include: at.include}})
		if err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("an include at %s was answered %v", ref, err)
		}
	}

	// And a credential the installation does not take is said as agk says one.
	at.token = "another"
	if _, err := versions.Check(context.Background(), includingCommon("v2.1.0"), versions.Checking{Resolvers: versions.Resolvers{Include: at.include}}); err == nil || !strings.Contains(err.Error(), "finance/common is no workflow you may read") {
		t.Errorf("an include read with another credential was answered %v", err)
	}
}

// A library has no metadata.name, so --name names the repository it is pushed to, and the push
// says what was pushed.
func TestALibraryIsPushedUnderTheNameGiven(t *testing.T) {
	dir := repository(t)
	write(t, dir, "agentiik.yaml", "include:\n  - path: ./blocks/api.yaml\n")
	write(t, dir, "blocks/api.yaml", ".api-brick:\n  timeout: 2m\n")
	commitAll(t, dir, "a library")

	code, _, errs, got := pushing(t, dir, 0)
	if code != exitRefused || got != nil || !strings.Contains(errs, "--name names the repository") {
		t.Errorf("a library pushed with no name exited %d, pushing %v:\n%s", code, got != nil, errs)
	}
	code, out, errs, got := pushing(t, dir, 0, "--name", "common")
	if code != exitSucceeded || got == nil || !strings.Contains(out, "finance/common@") || !strings.Contains(out, "a library, which other workflows include and nothing runs: 2 files, 1 included file") {
		t.Errorf("a library pushed as common exited %d:\n%s%s", code, out, errs)
	}

	// Given for a workflow, the name is the one its metadata writes.
	other := repository(t)
	if code, _, errs, _ := pushing(t, other, 0, "--name", "common"); code != exitRefused || !strings.Contains(errs, "metadata-name-not-repository") {
		t.Errorf("a workflow pushed under another name exited %d:\n%s", code, errs)
	}
}
