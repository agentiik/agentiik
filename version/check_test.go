package version_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/version"
)

// aRepository is a workflow at the root of its tree whose one brick is named by a tag in a block of
// an included file, and which names a secret, so that every store a push reaches is asked of it.
func aRepository() fstest.MapFS {
	return fstest.MapFS{
		"agentiik.yaml": &fstest.MapFile{Data: []byte(`apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
include:
  - path: fragments/bricks.yaml
secrets: [billing]
steps:
  invoice:
    extends: .invoicing
    secrets: [billing]
    outputs: [ok]
`)},
		"fragments/bricks.yaml": &fstest.MapFile{Data: []byte(`.invoicing:
  image: ghcr.io/acme/agk-invoice:1.4.0
`)},
		"scripts/render.sh": &fstest.MapFile{Data: []byte("#!/bin/sh\n"), Mode: 0o755},
	}
}

const pinnedInvoice = "ghcr.io/acme/agk-invoice@sha256:8214cabcb148ac56e69ee08e1554684f7609d08b58c4527938fdcf400be68595"

// everything is a push reaching every store, each holding what aRepository needs.
func everything() version.Checking {
	return version.Checking{
		Commit: "a3f9c1e04b7d2e8f6a1c3b5d7e9f0a2b4c6d8e0f", Committed: true,
		Namespace: "finance", Repository: "monthly-invoicing",
		Resolvers: version.Resolvers{
			Pin: func(_ context.Context, ref string, _ agk.Step) (string, error) {
				if ref == "ghcr.io/acme/agk-invoice:1.4.0" {
					return pinnedInvoice, nil
				}
				return "", version.ErrNotHeld
			},
			Manifest: func(_ context.Context, image string, _ agk.Step) ([]byte, error) {
				if image != pinnedInvoice {
					return nil, version.ErrNotHeld
				}
				return []byte(manifest), nil
			},
			Secrets:   func(context.Context) ([]string, error) { return []string{"billing"}, nil },
			SecretUse: func(context.Context, []string) (bool, error) { return true, nil },
		},
	}
}

func refusedBy(t *testing.T, err error, rule graph.Rule) *graph.Refusal {
	t.Helper()
	var r *graph.Refusal
	if !errors.As(err, &r) || r.Rule != rule {
		t.Fatalf("refused by %v, and the rule is %s", err, rule)
	}
	return r
}

// What a push records is what it was judged over: the files resolution read, the manifest of the
// brick by the reference the step wrote, and the digest the tag was pinned to.
func TestACheckedTreeIsTheVersionItWouldBe(t *testing.T) {
	checked, err := version.Check(t.Context(), aRepository(), everything())
	if err != nil {
		t.Fatal(err)
	}
	v := checked.Version
	if v.Entry != "agentiik.yaml" || len(v.Includes) != 1 || v.Includes["fragments/bricks.yaml"] == nil {
		t.Errorf("the version holds %s and %v", v.Entry, keys(v.Includes))
	}
	if v.Images["ghcr.io/acme/agk-invoice:1.4.0"] != pinnedInvoice || len(v.Images) != 1 {
		t.Errorf("the version records the digests %v", v.Images)
	}
	if v.Manifests["ghcr.io/acme/agk-invoice:1.4.0"] == nil || len(v.Manifests) != 1 {
		t.Errorf("the version holds the manifests of %v", keys(v.Manifests))
	}
	if st, _ := checked.Graph.Step("invoice"); st.Image != pinnedInvoice {
		t.Errorf("the step runs %s", st.Image)
	}
	// And it rebuilds as it was checked, with nothing in reach.
	v.Workflow, v.Commit = "monthly-invoicing", checked.Commit
	if _, err := version.Build(v); err != nil {
		t.Fatalf("the version a check answered does not rebuild: %v", err)
	}
}

// A tag the namespace holds no pin for is refused where the tag is written, which is in the block
// of the included file the step extends.
func TestATagWithNoPinIsRefusedWhereItIsWritten(t *testing.T) {
	c := everything()
	c.Pin = func(context.Context, string, agk.Step) (string, error) { return "", version.ErrNotHeld }
	_, err := version.Check(t.Context(), aRepository(), c)
	r := refusedBy(t, err, version.RuleImageNotPinned)
	if want := (graph.Position{File: "fragments/bricks.yaml", Line: 2, Column: 10}); r.At != want {
		t.Errorf("the tag is refused at %s, and it is written at %s", r.At, want)
	}
}

// The tree of a commit is held to what a runner can lay out, whichever entry holds what: a name
// that is .git somewhere, a backslash, a symbolic link and a submodule. A working tree is not a
// commit's, and its own .git is the repository itself.
func TestACommittedTreeIsHeldToWhatARunnerCanLayOut(t *testing.T) {
	for _, c := range []struct {
		name string
		file *fstest.MapFile
		rule graph.Rule
		at   string
	}{
		// The directory is the entry whose name is .git, and it is refused before anything
		// under it is reached.
		{"vendor/.GIT/config", &fstest.MapFile{Data: []byte("x")}, version.RuleDotGitInTree, "vendor/.GIT"},
		{"scripts/GIT~1", &fstest.MapFile{Data: []byte("x")}, version.RuleDotGitInTree, "scripts/GIT~1"},
		{`scripts\render.sh`, &fstest.MapFile{Data: []byte("x")}, version.RuleBackslashInTree, `scripts\render.sh`},
		{"scripts/current", &fstest.MapFile{Data: []byte("render.sh"), Mode: fs.ModeSymlink | 0o777}, version.RuleSymlinkInTree, "scripts/current"},
		{"vendor/rounding", &fstest.MapFile{Mode: fs.ModeIrregular}, version.RuleSubmoduleInTree, "vendor/rounding"},
		{"data/caf\xe9.csv", &fstest.MapFile{Data: []byte("x")}, version.RuleNameNotUTF8, "data/caf\xe9.csv"},
	} {
		tree := aRepository()
		tree[c.name] = c.file
		_, err := version.Check(t.Context(), tree, everything())
		r := refusedBy(t, err, c.rule)
		if r.At.File != c.at {
			t.Errorf("%s is refused naming %q", c.name, r.At.File)
		}
	}

	working := aRepository()
	working[".git/config"] = &fstest.MapFile{Data: []byte("[core]\n")}
	c := everything()
	c.Committed = false
	if _, err := version.Check(t.Context(), working, c); err != nil {
		t.Errorf("a working tree was refused for its own .git: %v", err)
	}
}

// The entry point is agentiik.yaml at the root, spelled so. One found only in a directory is
// refused naming it and the command that makes its directory a repository of its own; with none
// anywhere, the file that is not there is named.
func TestTheEntryPointIsAtTheRoot(t *testing.T) {
	below := fstest.MapFS{}
	for name, f := range aRepository() {
		below["zeta/"+name] = f
		below["alpha/"+name] = f
	}
	_, err := version.Check(t.Context(), below, everything())
	r := refusedBy(t, err, version.RuleEntryPointBelowRoot)
	if r.At.File != "alpha/agentiik.yaml" || !strings.Contains(r.Detail, "git subtree split --prefix alpha") {
		t.Errorf("the refusal names %s: %s", r.At.File, r.Detail)
	}

	nowhere := aRepository()
	nowhere["agentiik.yml"] = nowhere["agentiik.yaml"]
	delete(nowhere, "agentiik.yaml")
	_, err = version.Check(t.Context(), nowhere, everything())
	if r := refusedBy(t, err, version.RuleEntryPointMissing); r.At.File != "agentiik.yaml" {
		t.Errorf("the refusal names %s", r.At.File)
	}

	// An entry point the caller names is read where it is, which is what agk validate -f does.
	c := everything()
	c.Entry = "flows/monthly.yaml"
	if _, err := version.Check(t.Context(), below, c); err == nil || !strings.Contains(err.Error(), "flows/monthly.yaml") {
		t.Errorf("a named entry point was not read where it was named: %v", err)
	}
}

// The name a push is made under is the name the file writes, and the namespace it writes, where it
// writes one, is the repository's.
func TestAVersionIsMadeUnderTheNameItsFileWrites(t *testing.T) {
	c := everything()
	c.Repository = "payroll"
	_, err := version.Check(t.Context(), aRepository(), c)
	if r := refusedBy(t, err, version.RuleMetadataNameNotRepository); r.At != (graph.Position{File: "agentiik.yaml", Line: 3, Column: 19}) {
		t.Errorf("the name is refused at %s", r.At)
	}

	c = everything()
	c.Namespace = "team-ops"
	_, err = version.Check(t.Context(), aRepository(), c)
	if r := refusedBy(t, err, version.RuleMetadataNamespaceNotRepository); r.At != (graph.Position{File: "agentiik.yaml", Line: 3, Column: 49}) {
		t.Errorf("the namespace is refused at %s", r.At)
	}

	// Where the caller does not know them, nothing is held to them.
	c.Namespace, c.Repository = "", ""
	if _, err := version.Check(t.Context(), aRepository(), c); err != nil {
		t.Errorf("a check knowing neither the repository nor the namespace refused the name: %v", err)
	}
}

// A secret is asked of the pusher's secret:use first, then of the namespace's declarations, and a
// pusher without secret:use learns nothing of what the namespace declares.
func TestASecretIsAskedOfThePusherBeforeTheNamespace(t *testing.T) {
	c := everything()
	asked := false
	c.SecretUse = func(context.Context, []string) (bool, error) { return false, nil }
	c.Secrets = func(context.Context) ([]string, error) { asked = true; return nil, nil }
	_, err := version.Check(t.Context(), aRepository(), c)
	var unusable *version.SecretsNotUsable
	if !errors.As(err, &unusable) || strings.Join(unusable.Named, ",") != "billing" {
		t.Fatalf("a pusher without secret:use was answered %v", err)
	}
	if asked {
		t.Error("the namespace's declarations were read for a pusher without secret:use")
	}

	c = everything()
	c.Secrets = func(context.Context) ([]string, error) { return []string{"ledger"}, nil }
	_, err = version.Check(t.Context(), aRepository(), c)
	r := refusedBy(t, err, version.RuleSecretNotDeclaredByNamespace)
	if r.At != (graph.Position{File: "agentiik.yaml", Line: 6, Column: 11}) || !strings.Contains(r.Detail, "step invoice names the secret billing") {
		t.Errorf("the secret is refused at %s: %s", r.At, r.Detail)
	}

	// Named in the files it includes as well, it is refused where it was first named:
	// resolution reads what a file includes before the file, so that is in the file the
	// included file includes.
	tree := aRepository()
	tree["fragments/secrets.yaml"] = &fstest.MapFile{Data: []byte("secrets: [billing]\n")}
	tree["fragments/bricks.yaml"] = &fstest.MapFile{Data: []byte("include:\n  - path: ./secrets.yaml\nsecrets: [billing]\n" + string(tree["fragments/bricks.yaml"].Data))}
	_, err = version.Check(t.Context(), tree, c)
	if r := refusedBy(t, err, version.RuleSecretNotDeclaredByNamespace); r.At != (graph.Position{File: "fragments/secrets.yaml", Line: 1, Column: 11}) {
		t.Errorf("the secret named first in the file an included file includes is refused at %s", r.At)
	}
}

// A commit already stored is judged by the rules it was stored under and none added since: its
// tree, where its entry point is, the name it is under and the secrets its namespace declares
// now are not asked about again. What stood then still does.
func TestAStoredCommitIsNotJudgedAgainByRulesAddedSince(t *testing.T) {
	tree := aRepository()
	tree["vendor/.git/config"] = &fstest.MapFile{Data: []byte("x")}
	c := everything()
	c.Stored, c.Repository, c.Namespace = true, "payroll", "team-ops"
	c.Secrets = func(context.Context) ([]string, error) { return nil, nil }
	if _, err := version.Check(t.Context(), tree, c); err != nil {
		t.Fatalf("a stored commit was judged again: %v", err)
	}

	c.SecretUse = func(context.Context, []string) (bool, error) { return false, nil }
	var unusable *version.SecretsNotUsable
	if _, err := version.Check(t.Context(), tree, c); !errors.As(err, &unusable) {
		t.Errorf("a stored commit naming a secret was pushed again by someone without secret:use: %v", err)
	}
}

// A resolver that cannot answer is not the file's fault, and is answered as it failed, so that a
// daemon out of reach is told apart from a workflow refused.
func TestAResolverThatFailsIsAnsweredAsItFailed(t *testing.T) {
	gone := errors.New("the daemon is not there")
	c := everything()
	c.Manifest = func(context.Context, string, agk.Step) ([]byte, error) { return nil, gone }
	if _, err := version.Check(t.Context(), aRepository(), c); !errors.Is(err, gone) {
		t.Errorf("a manifest that could not be read was answered %v", err)
	}
}

// A tree held as its files, as a push carries one, is judged from its listing: a walk of a map finds
// each directory's entries by reading every path, which cost seconds of processor for a push of a
// few hundred deep paths, well inside a push's bounds, from anybody allowed to push. Walked, this
// tree takes about a minute; listed, milliseconds, and the bound leaves room for a slow machine.
func TestATreeOfDeepPathsIsJudgedFromItsListing(t *testing.T) {
	files := version.Files{}
	for name, f := range aRepository() {
		files[name] = f
	}
	for i := range 1024 {
		files[fmt.Sprintf("x%05d/", i)+strings.Repeat("d/", 500)+"f"] = &fstest.MapFile{}
	}
	started := time.Now()
	if _, err := version.Check(t.Context(), files, everything()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("judging the tree took %s", took)
	}

	// And judged by the same rules as a walk would judge it: a directory of it named .git is a
	// segment of every path below it.
	files["vendor/.git/hooks/post-checkout"] = &fstest.MapFile{Data: []byte("#!/bin/sh\n")}
	_, err := version.Check(t.Context(), files, everything())
	if r := refusedBy(t, err, version.RuleDotGitInTree); r.At.File != "vendor/.git/hooks/post-checkout" {
		t.Errorf("the .git directory is refused naming %s", r.At.File)
	}
}
