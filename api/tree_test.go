package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// The tree a push carries, which is what every step of every run sees.

// pushed is the ordinary push with files added to its tree, which already holds the entry point.
func pushed(t *testing.T, files map[string]api.PushFile) api.Push {
	t.Helper()
	p := aPush(t)
	for path, f := range files {
		p.Tree[path] = f
	}
	return p
}

func digestOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func keyOf(namespace string, content []byte) string {
	return artifact.Key(namespace, digestOf(content))
}

const pushTo = "/api/v1/finance/workflows/monthly-invoicing/versions/a3f9c1e"

func TestAPushStoresItsTreeAsObjectsAndNamesThem(t *testing.T) {
	h, pool, _, objects := servingWithObjects(t)

	body := pushed(t, map[string]api.PushFile{
		"scripts/render.sh":  {Content: []byte("#!/bin/sh\necho hello\n"), Mode: "0755"},
		"fragments/base.yml": {Content: []byte("a fragment"), Mode: "0644"},
	})
	w, _ := call(t, h, "PUT", pushTo, "alice", body)
	if w.Code != http.StatusOK {
		t.Fatalf("pushing answered %d: %s", w.Code, w.Body)
	}

	// Every file is an object in the namespace, under the digest of its own bytes.
	for path, f := range body.Tree {
		held, err := objects.Has(context.Background(), keyOf("finance", f.Content))
		if err != nil || !held {
			t.Errorf("%s is not in the store: %v %v", path, held, err)
		}
	}

	// And the version names them, in path order, each with the mode it was pushed with.
	var v db.Version
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		var err error
		v, err = ns.Version(ctx, "monthly-invoicing", "a3f9c1e")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range v.Tree {
		paths = append(paths, f.Path+" "+f.Mode)
	}
	if got, want := strings.Join(paths, ", "), "agentiik.yaml 0644, fragments/base.yml 0644, scripts/render.sh 0755"; got != want {
		t.Errorf("the version names %s, want %s", got, want)
	}

	// A file two commits share is one object, so a version costs what changed.
	second := pushed(t, map[string]api.PushFile{
		"scripts/render.sh": {Content: []byte("#!/bin/sh\necho goodbye\n"), Mode: "0755"},
	})
	w, _ = call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/b4a0d2f", "alice", second)
	if w.Code != http.StatusOK {
		t.Fatalf("the second push answered %d: %s", w.Code, w.Body)
	}
	if held, err := objects.Has(context.Background(), keyOf("finance", []byte(workflowDocument))); err != nil || !held {
		t.Errorf("the shared file is not in the store: %v %v", held, err)
	}
}

func TestATreeThatCannotBeGivenToAContainer(t *testing.T) {
	h, _, _, objects := servingWithObjects(t)

	file := func(content string) api.PushFile { return api.PushFile{Content: []byte(content), Mode: "0644"} }
	adding := func(files map[string]api.PushFile) func(*api.Push) {
		return func(p *api.Push) {
			for path, f := range files {
				p.Tree[path] = f
			}
		}
	}
	for _, c := range []struct {
		name   string
		change func(*api.Push)
		status int
	}{
		{"a push with no tree", func(p *api.Push) { p.Tree = nil }, http.StatusBadRequest},
		{"a path that leaves the repository", adding(map[string]api.PushFile{"../outside.sh": file("x")}), http.StatusBadRequest},
		{"an absolute path", adding(map[string]api.PushFile{"/etc/passwd": file("x")}), http.StatusBadRequest},
		{"a path that is not in its cleaned form", adding(map[string]api.PushFile{"scripts/../scripts/render.sh": file("x")}), http.StatusBadRequest},
		{"the root of the repository as a file", adding(map[string]api.PushFile{".": file("x")}), http.StatusBadRequest},
		{"a file inside .git", adding(map[string]api.PushFile{".git/config": file("[core]")}), http.StatusBadRequest},
		{"a .git segment spelt in another case, further down", adding(map[string]api.PushFile{"vendor/lib/.GIT/hooks/post-checkout": file("x")}), http.StatusBadRequest},
		{"a path that is a file and a directory", adding(map[string]api.PushFile{"scripts": file("x"), "scripts/render.sh": file("y")}), http.StatusBadRequest},
		{"a mode a tree does not carry", adding(map[string]api.PushFile{"render.sh": {Content: []byte("x"), Mode: "4755"}}), http.StatusBadRequest},
		{"a mode left out", adding(map[string]api.PushFile{"render.sh": {Content: []byte("x")}}), http.StatusBadRequest},
		{"a tree without its entry point", func(p *api.Push) {
			delete(p.Tree, "agentiik.yaml")
			p.Tree["README.md"] = file("# elsewhere")
		}, http.StatusUnprocessableEntity},
		{"an entry point the tree holds other bytes of", adding(map[string]api.PushFile{"agentiik.yaml": file("kind: Workflow\n")}), http.StatusUnprocessableEntity},
		{"an include the tree does not hold", func(p *api.Push) {
			p.Includes = map[string][]byte{"fragments/base.yml": []byte("a fragment")}
		}, http.StatusUnprocessableEntity},
		{"an include the tree holds other bytes of", func(p *api.Push) {
			p.Includes = map[string][]byte{"fragments/base.yml": []byte("a fragment")}
			p.Tree["fragments/base.yml"] = file("another fragment")
		}, http.StatusUnprocessableEntity},
		{"a repository above the limit", adding(map[string]api.PushFile{"big.bin": {Content: make([]byte, api.TreeMaxBytes+1), Mode: "0644"}}), http.StatusRequestEntityTooLarge},
	} {
		p := aPush(t)
		c.change(&p)
		w, answer := call(t, h, "PUT", pushTo, "alice", p)
		if w.Code != c.status {
			t.Errorf("%s answered %d, want %d: %s", c.name, w.Code, c.status, w.Body)
			continue
		}
		if said, _ := answer["error"].(string); said == "" {
			t.Errorf("%s was refused with no reason", c.name)
		}
	}

	// Everything above was refused before anything was written: the entry point is in every one
	// of those pushes, and it is not in the store.
	if held, err := objects.Has(t.Context(), keyOf("finance", []byte(workflowDocument))); err != nil || held {
		t.Errorf("a refused push left its tree in the store: %v %v", held, err)
	}
}

// A commit that is not one, in the path or as the parent, is the caller's mistake and is refused
// as one: 400 with a sentence, before a byte of the tree is in the store. Left to the table, it
// was refused by the insert, answered 500, and left its files behind with nothing counting them.
func TestACommitThatIsNotOneIsRefusedBeforeItsTreeIsStored(t *testing.T) {
	h, _, super, objects := servingWithObjects(t)

	for _, c := range []struct {
		name string
		to   string
		push func(*api.Push)
	}{
		{"a branch where the commit goes", "/api/v1/finance/workflows/monthly-invoicing/versions/main", func(*api.Push) {}},
		{"a commit in capitals", "/api/v1/finance/workflows/monthly-invoicing/versions/A3F9C1E", func(*api.Push) {}},
		{"a parent that is a name for a commit", pushTo, func(p *api.Push) { p.Parent = "HEAD" }},
	} {
		lone := []byte("only " + c.name + " carries this file\n")
		p := pushed(t, map[string]api.PushFile{"scripts/lone.sh": {Content: lone, Mode: "0755"}})
		c.push(&p)
		w, answer := call(t, h, "PUT", c.to, "alice", p)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d: %s", c.name, w.Code, w.Body)
			continue
		}
		if said, _ := answer["error"].(string); !strings.Contains(said, "hexadecimal") {
			t.Errorf("%s was refused with %q", c.name, said)
		}
		if held, err := objects.Has(t.Context(), keyOf("finance", lone)); err != nil || held {
			t.Errorf("%s left its tree in the store: %v %v", c.name, held, err)
		}
	}

	var versions int
	if err := dbtest.Superuser(t, super).QueryRow(t.Context(),
		`select count(*) from workflow_versions where namespace = 'finance'`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 0 {
		t.Errorf("%d versions were recorded from pushes that were all refused", versions)
	}
}

// A refusal about size says what the limit is for, because a limit with no rationale is a limit
// somebody works around rather than reconsiders.
func TestTheSizeRefusalSaysWhyThereIsALimit(t *testing.T) {
	h, _, _, _ := servingWithObjects(t)
	w, answer := call(t, h, "PUT", pushTo, "alice",
		pushed(t, map[string]api.PushFile{"big.bin": {Content: make([]byte, api.TreeMaxBytes+1), Mode: "0644"}}))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized tree answered %d", w.Code)
	}
	said, _ := answer["error"].(string)
	if !strings.Contains(said, "belongs in an image or in an artifact") {
		t.Errorf("the refusal reads %q", said)
	}
	// And that it is this push's limit rather than a rule about repositories, which the
	// installation hosting the repository takes away.
	if !strings.Contains(said, "until the installation hosts the repository") {
		t.Errorf("the refusal does not say the limit is the push's: %q", said)
	}
}

// A tree at the limit fits in a push, which is the arithmetic behind the body cap: the entry point
// and its includes travel twice, once as the version and once as files of the tree, so a body
// only just large enough for the tree refuses a push the tree limit allows.
func TestATreeAtTheLimitFitsInAPush(t *testing.T) {
	h, _, _, _ := servingWithObjects(t)

	const entry = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
include:
  - path: common.yaml
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: archive, port: ok } }
steps:
  normalize:
    extends: .brick
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
  archive:
    extends: .brick
    needs: [{ step: normalize, port: ok, as: orders }]
    outputs: [ok]
`
	fragment := ".brick:\n  image: " + image + "\n"
	padding := api.TreeMaxBytes - len(entry) - len(fragment) - len("# \n")
	common := []byte("# " + strings.Repeat("x", padding) + "\n" + fragment)

	p := aPush(t)
	p.Document = []byte(entry)
	p.Includes = map[string][]byte{"common.yaml": common}
	p.Tree = map[string]api.PushFile{
		"agentiik.yaml": {Content: []byte(entry), Mode: "0644"},
		"common.yaml":   {Content: common, Mode: "0644"},
	}
	w, _ := call(t, h, "PUT", pushTo, "alice", p)
	if w.Code != http.StatusOK {
		t.Fatalf("a tree of exactly %d bytes answered %d: %s", api.TreeMaxBytes, w.Code, w.Body)
	}
}

// A body past what a push may carry is refused as too large, with a sentence, rather than as a
// request that could not be read.
func TestAPushLargerThanAPushMayBeIsTooLarge(t *testing.T) {
	h, _, _, _ := servingWithObjects(t)
	w, answer := call(t, h, "PUT", pushTo, "alice",
		pushed(t, map[string]api.PushFile{"big.bin": {Content: make([]byte, 13<<20), Mode: "0644"}}))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a push of more than the body cap answered %d", w.Code)
	}
	if said, _ := answer["error"].(string); !strings.Contains(said, "a push is at most") {
		t.Errorf("the refusal reads %q", said)
	}
}

// An installation with no object store cannot take a tree, and that is the installation's to fix
// rather than the caller's: 503, not 400.
func TestAPushToAnInstallationWithNoObjectStore(t *testing.T) {
	h, _, _ := servingOn(t, nil)
	w, answer := call(t, h, "PUT", pushTo, "alice", aPush(t))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a push with nowhere to put its tree answered %d: %s", w.Code, w.Body)
	}
	if said, _ := answer["error"].(string); !strings.Contains(said, "object store") {
		t.Errorf("the refusal reads %q", said)
	}
}

// broken is an object store that cannot be reached, whose error names things a caller has no
// business reading.
type broken struct{}

var errBroken = errors.New("dial tcp 10.0.3.7:9000: connection refused")

func (broken) Has(context.Context, string) (bool, error)           { return false, errBroken }
func (broken) Put(context.Context, string, io.Reader) error        { return errBroken }
func (broken) Open(context.Context, string) (io.ReadCloser, error) { return nil, errBroken }

// A store that fails is a 500 with a sentence, and the sentence does not carry the store's own
// error: an address inside the installation is nothing the person pushing should be shown.
func TestAPushWhoseStoreFailsSaysSoAndNothingMore(t *testing.T) {
	h, _, _ := servingOn(t, broken{})
	w, answer := call(t, h, "PUT", pushTo, "alice", aPush(t))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a push whose store failed answered %d: %s", w.Code, w.Body)
	}
	said, _ := answer["error"].(string)
	if said == "" || strings.Contains(said, "10.0.3.7") || strings.Contains(said, "dial") {
		t.Errorf("the refusal reads %q", said)
	}
}

// "A version is a commit": the same commit pushed again with the same files is the same version
// and changes nothing, and the same commit with other files is a conflict, said out loud rather
// than answered 200 while nothing was recorded.
func TestPushingOneCommitWithOtherFilesIsAConflict(t *testing.T) {
	h, pool, super, objects := servingWithObjects(t)
	first := pushed(t, map[string]api.PushFile{
		"scripts/render.sh": {Content: []byte("#!/bin/sh\necho hello\n"), Mode: "0755"},
	})
	if w, _ := call(t, h, "PUT", pushTo, "alice", first); w.Code != http.StatusOK {
		t.Fatalf("the first push answered %d: %s", w.Code, w.Body)
	}

	conn := dbtest.Superuser(t, super)
	counts := func() map[string]int {
		t.Helper()
		rows, err := conn.Query(t.Context(), `select digest, refs from artifact_objects where namespace = 'finance'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var digest string
			var refs int
			if err := rows.Scan(&digest, &refs); err != nil {
				t.Fatal(err)
			}
			out[digest] = refs
		}
		return out
	}
	recorded := func() db.Version {
		t.Helper()
		var v db.Version
		if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
			var err error
			v, err = ns.Version(ctx, "monthly-invoicing", "a3f9c1e")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return v
	}

	before, was := counts(), recorded()
	if len(before) != 2 {
		t.Fatalf("the first push counts %v, and its tree is two distinct files", before)
	}
	for digest, refs := range before {
		if refs != 1 {
			t.Errorf("%s is referenced %d times by one version", digest, refs)
		}
	}

	// The same files again: 200, and neither a count nor the row moved.
	if w, _ := call(t, h, "PUT", pushTo, "alice", first); w.Code != http.StatusOK {
		t.Fatalf("pushing the same commit again answered %d: %s", w.Code, w.Body)
	}
	if after := counts(); !sameCounts(before, after) {
		t.Errorf("pushing the same version again moved the counts from %v to %v", before, after)
	}
	if again := recorded(); !again.CreatedAt.Equal(was.CreatedAt) || !slices.Equal(again.Tree, was.Tree) {
		t.Errorf("pushing the same version again rewrote it: %+v, then %+v", was, again)
	}

	// Other files under the same commit: 409, with a sentence, and the version is as it was.
	// Refused before the files were written, too: a refused push that stored them would leave
	// objects nothing counts, as many times as anybody cared to push it.
	elsewise := []byte("#!/bin/sh\necho something else\n")
	other := pushed(t, map[string]api.PushFile{"scripts/render.sh": {Content: elsewise, Mode: "0755"}})
	w, answer := call(t, h, "PUT", pushTo, "alice", other)
	if w.Code != http.StatusConflict {
		t.Fatalf("the same commit with other files answered %d: %s", w.Code, w.Body)
	}
	if said, _ := answer["error"].(string); !strings.Contains(said, "a3f9c1e") {
		t.Errorf("the conflict does not name the commit: %q", said)
	}
	if held, err := objects.Has(t.Context(), keyOf("finance", elsewise)); err != nil || held {
		t.Errorf("a refused push left its file in the store: %v %v", held, err)
	}
	if after := counts(); !sameCounts(before, after) {
		t.Errorf("a refused push moved the counts from %v to %v", before, after)
	}
	if again := recorded(); !slices.Equal(again.Tree, was.Tree) {
		t.Errorf("a refused push rewrote the tree to %+v", again.Tree)
	}
}

func sameCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
