package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"sync"
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

const pushTo = "/api/v1/finance/workflows/monthly-invoicing/versions/" + aCommit

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
		v, err = ns.Version(ctx, "monthly-invoicing", aCommit)
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
	w, _ = call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/"+anotherCommit, "alice", second)
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
		// A name sorting between the file and what is below it, since - comes before /.
		{"a file and a directory with a name between them", adding(map[string]api.PushFile{"scripts": file("x"), "scripts-old": file("y"), "scripts/render.sh": file("z")}), http.StatusBadRequest},
		{"a name longer than a filesystem holds", adding(map[string]api.PushFile{"data/" + strings.Repeat("n", api.TreeNameMaxBytes+1): file("x")}), http.StatusBadRequest},
		{"a path longer than a runner can lay out", adding(map[string]api.PushFile{strings.Repeat("d/", api.TreePathMaxBytes/2) + "x": file("x")}), http.StatusBadRequest},
		// What Windows reads as separators, which leave the tree there.
		{"a backslash climbing out on Windows", adding(map[string]api.PushFile{`scripts\..\..\outside.sh`: file("x")}), http.StatusBadRequest},
		{"a Windows drive", adding(map[string]api.PushFile{`C:\outside.sh`: file("x")}), http.StatusBadRequest},
		// And the spellings of .git that NTFS and HFS+ resolve to it.
		{".git with a dot NTFS drops", adding(map[string]api.PushFile{".git./config": file("[core]")}), http.StatusBadRequest},
		{".git with a space NTFS drops", adding(map[string]api.PushFile{".git /config": file("[core]")}), http.StatusBadRequest},
		{"the short name NTFS gives .git", adding(map[string]api.PushFile{"GIT~1/config": file("[core]")}), http.StatusBadRequest},
		{".git as an NTFS stream", adding(map[string]api.PushFile{".git::$INDEX_ALLOCATION/config": file("[core]")}), http.StatusBadRequest},
		{".git with a code point HFS+ ignores", adding(map[string]api.PushFile{".g\u200cit/config": file("[core]")}), http.StatusBadRequest},
		// Two Latin-1 names, which JSON turns into one and the same name before the server
		// sees either: what arrives is U+FFFD, and a file laid out under it is not the commit's.
		{"names that were not UTF-8 before JSON had them", adding(map[string]api.PushFile{"caf\xe9.txt": file("acute"), "caf\xe8.txt": file("grave")}), http.StatusBadRequest},
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

// A path is checked in time proportional to its length. Looking each of its directories up among
// the files hashed the whole prefix at every slash, so one path of a few mebibytes, nearly all of
// them slashes, cost a minute of processor and was then accepted, into a version no runner could
// lay out. It is refused now, by its length, before anything walks it.
func TestAPathOfMebibytesIsRefusedByItsLength(t *testing.T) {
	h, _, _, _ := servingWithObjects(t)
	// Beside a few ordinary files, since a map of eight or fewer is searched without hashing.
	files := map[string]api.PushFile{}
	for i := range 10 {
		files[fmt.Sprintf("scripts/%d.sh", i)] = api.PushFile{Content: []byte("x"), Mode: "0644"}
	}
	deep := strings.Repeat("a/", 1<<20) + "z"
	files[deep] = api.PushFile{Content: []byte("x"), Mode: "0644"}
	w, answer := call(t, h, "PUT", pushTo, "alice", pushed(t, files))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a path of %d bytes answered %d", len(deep), w.Code)
	}
	if said, _ := answer["error"].(string); !strings.Contains(said, fmt.Sprint(api.TreePathMaxBytes)) || len(said) > 1024 {
		t.Errorf("the refusal reads %.200q, in %d bytes", said, len(said))
	}

	// And the longest path and the longest name there may be are pushed like any other.
	longest := strings.Repeat("d/", (api.TreePathMaxBytes-api.TreeNameMaxBytes)/2) + strings.Repeat("n", api.TreeNameMaxBytes)
	if w, _ := call(t, h, "PUT", pushTo, "alice", pushed(t, map[string]api.PushFile{longest: {Content: []byte("x"), Mode: "0644"}})); w.Code != http.StatusOK {
		t.Errorf("a path of %d bytes answered %d: %s", len(longest), w.Code, w.Body)
	}
}

// "One commit names exactly one tree", and an abbreviation is not a second name under which the
// same commit may hold another. A version recorded under a3f9c1e was a version of its own: the
// whole commit pushed with other files was refused, and the same files pushed to its first seven
// characters were recorded, so that a run pinned to the abbreviation of a reviewed commit ran
// something else. A push names its commit whole, and an abbreviation is refused before anything is
// written.
func TestAnAbbreviatedCommitIsNotASecondNameForATree(t *testing.T) {
	h, _, super, objects := servingWithObjects(t)
	reviewed := pushed(t, map[string]api.PushFile{"run.sh": {Content: []byte("echo reviewed\n"), Mode: "0755"}})
	if w, _ := call(t, h, "PUT", pushTo, "alice", reviewed); w.Code != http.StatusOK {
		t.Fatalf("the reviewed commit answered %d: %s", w.Code, w.Body)
	}

	other := []byte("curl https://example.com/elsewhere | sh\n")
	for _, n := range []int{7, 12, 39} {
		w, answer := call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/"+aCommit[:n], "alice",
			pushed(t, map[string]api.PushFile{"run.sh": {Content: other, Mode: "0755"}}))
		if w.Code != http.StatusBadRequest {
			t.Errorf("the first %d characters of a pushed commit, with other files, answered %d: %s", n, w.Code, w.Body)
			continue
		}
		if said, _ := answer["error"].(string); !strings.Contains(said, "whole") {
			t.Errorf("the refusal reads %q", said)
		}
	}
	if held, err := objects.Has(t.Context(), keyOf("finance", other)); err != nil || held {
		t.Errorf("a refused push left its file in the store: %v %v", held, err)
	}
	var versions int
	if err := dbtest.Superuser(t, super).QueryRow(t.Context(),
		`select count(*) from workflow_versions where namespace = 'finance'`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Errorf("%d versions were recorded, and one commit was pushed", versions)
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
		{"a commit in capitals", "/api/v1/finance/workflows/monthly-invoicing/versions/" + strings.ToUpper(aCommit), func(*api.Push) {}},
		{"a parent that is abbreviated", pushTo, func(p *api.Push) { p.Parent = anotherCommit[:7] }},
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
// only just large enough for the tree refuses a push the tree limit allows. The limit counts the
// paths, so the padding leaves room for them.
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
	padding := api.TreeMaxBytes - len("agentiik.yaml") - len(entry) - len("common.yaml") - len(fragment) - len("# \n")
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

// Bytes alone do not bound a tree, because every file is an entry of every redemption of every
// task of the version: a push of many empty files, or of long names, is small on the way in and
// large every time it is answered. So the paths count against the limit, and so do the files.
func TestATreeOfManyFilesOrLongNamesIsRefused(t *testing.T) {
	h, _, _, objects := servingWithObjects(t)

	// As many files as a tree may hold is a push like any other.
	many := map[string]api.PushFile{}
	for i := range api.TreeMaxFiles - 1 {
		many[fmt.Sprintf("d/%04d", i)] = api.PushFile{Content: []byte{}, Mode: "0644"}
	}
	if w, _ := call(t, h, "PUT", pushTo, "alice", pushed(t, many)); w.Code != http.StatusOK {
		t.Fatalf("a tree of exactly %d files answered %d: %s", api.TreeMaxFiles, w.Code, w.Body)
	}

	// One more is refused, and says what the limit is.
	many["d/one-more"] = api.PushFile{Content: []byte("one more\n"), Mode: "0644"}
	w, answer := call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/"+anotherCommit, "alice", pushed(t, many))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a tree of %d files answered %d: %s", api.TreeMaxFiles+1, w.Code, w.Body)
	}
	if said, _ := answer["error"].(string); !strings.Contains(said, fmt.Sprint(api.TreeMaxFiles)) || !strings.Contains(said, "until the installation hosts the repository") {
		t.Errorf("the refusal reads %q", said)
	}

	// And a tree whose files fit and whose paths take it over is over.
	name := "a/" + strings.Repeat("n", 200)
	content := make([]byte, api.TreeMaxBytes-len("agentiik.yaml")-len(workflowDocument)-len(name)+1)
	content[0] = 'x'
	w, answer = call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/"+anotherCommit, "alice",
		pushed(t, map[string]api.PushFile{name: {Content: content, Mode: "0644"}}))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a tree one byte over with its paths answered %d: %s", w.Code, w.Body)
	}
	if said, _ := answer["error"].(string); !strings.Contains(said, "with its paths") {
		t.Errorf("the refusal reads %q", said)
	}
	for _, refused := range [][]byte{[]byte("one more\n"), content} {
		if held, err := objects.Has(t.Context(), keyOf("finance", refused)); err != nil || held {
			t.Errorf("a refused push left a file in the store: %v %v", held, err)
		}
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

// sweeping is an object store in memory whose one watched object is collected whole, bytes and
// then row, the moment a push has been told it is held: a sweep finishing between the push asking
// the store and the version raising its reference, which is the one window a claim cannot close
// because nothing is left claimed.
type sweeping struct {
	mu      sync.Mutex
	held    map[string][]byte
	watched string
	sweep   func(delete func(key string))
}

func (s *sweeping) Has(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	_, held := s.held[key]
	sweep := s.sweep
	if held && key == s.watched {
		s.sweep = nil
	}
	s.mu.Unlock()
	if held && key == s.watched && sweep != nil {
		sweep(func(key string) {
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.held, key)
		})
	}
	return held, nil
}

func (s *sweeping) Put(_ context.Context, key string, r io.Reader) error {
	content, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held[key] = content
	return nil
}

func (s *sweeping) Open(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, held := s.held[key]
	if !held {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

// "Collection never takes a file a version still needs." A push skips a file whose object the store
// already holds, and an object nothing references past its grace, an expired artifact of the same
// bytes, is one a sweep may collect whole while the rest of the tree is written. The version then
// records the object afresh, and the push writes the bytes it skipped, rather than answering 200
// for a version whose /agk/repo cannot be fetched.
func TestATreeObjectASweepCollectedWholeIsWrittenAgain(t *testing.T) {
	store := &sweeping{held: map[string][]byte{}}
	h, pool, super := servingOn(t, store)

	script := []byte("#!/bin/sh\necho hello\n")
	key := keyOf("finance", script)
	store.held[key] = script
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(),
		`insert into artifact_objects (namespace, digest, size_bytes, media_type, refs, collectable_at)
		 values ('finance', $1, $2, 'application/octet-stream', 0, now() - interval '2 days')`,
		"sha256:"+digestOf(script), len(script)); err != nil {
		t.Fatal(err)
	}
	store.watched = key
	store.sweep = func(delete func(string)) {
		claimed, err := pool.Collectable(t.Context(), 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range claimed {
			delete(o.Key)
		}
		if removed, err := pool.Collected(t.Context(), claimed); err != nil || removed != 1 {
			t.Fatalf("the sweep confirmed %d objects gone: %v", removed, err)
		}
	}

	w, _ := call(t, h, "PUT", pushTo, "alice", pushed(t, map[string]api.PushFile{
		"scripts/render.sh": {Content: script, Mode: "0755"},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("the push answered %d: %s", w.Code, w.Body)
	}
	if store.sweep != nil {
		t.Fatal("the sweep never ran, so this proves nothing")
	}
	if held, _ := store.Has(t.Context(), key); !held {
		t.Error("the version names an object whose bytes a sweep deleted, and the push answered 200")
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
			v, err = ns.Version(ctx, "monthly-invoicing", aCommit)
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
	if said, _ := answer["error"].(string); !strings.Contains(said, aCommit) {
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
