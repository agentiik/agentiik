package version_test

import (
	"context"
	"maps"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/version"
)

// A version is a commit, so what is stored has to rebuild into the same workflow for ever, with
// no tree and no registry in reach. That is the whole claim of this package and every test here
// is a way of checking it.

const theImage = "ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc"

const entryPoint = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
include:
  - path: .agentiik/common.yaml
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

// An included file is a fragment rather than an entry point, so it carries no apiVersion, no kind
// and no metadata.
const common = `
.brick:
  image: ` + theImage + `
defaults:
  timeout: 10m
`

const manifest = `
apiVersion: agentiik.dev/v1
kind: Brick
metadata: { name: invoice, version: 1.0.0 }
spec:
  inputs:
    orders: {}
  outputs:
    ok: {}
    rejected: {}
  runtime: { user: "65532:65532" }
`

// tree is a repository holding rather more than the workflow reaches, which is what every
// repository is.
func tree() fstest.MapFS {
	return fstest.MapFS{
		"agentiik.yaml":         &fstest.MapFile{Data: []byte(entryPoint)},
		".agentiik/common.yaml": &fstest.MapFile{Data: []byte(common)},
		"README.md":             &fstest.MapFile{Data: []byte("# not part of the workflow")},
		"src/main.go":           &fstest.MapFile{Data: []byte("package main")},
		"testdata/big.bin":      &fstest.MapFile{Data: make([]byte, 1<<20)},
	}
}

func manifests(t *testing.T) map[string]brick.Manifest {
	t.Helper()
	m, err := brick.ParseManifest([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	return map[string]brick.Manifest{theImage: m}
}

// bundled is the tree a version's own files make, which is what a version about to be recorded is
// judged over.
func bundled(v db.Version) (fstest.MapFS, map[string]brick.Manifest, error) {
	fsys := fstest.MapFS{v.Entry: &fstest.MapFile{Data: v.Document}}
	for p, body := range v.Includes {
		fsys[p] = &fstest.MapFile{Data: body}
	}
	manifests := map[string]brick.Manifest{}
	for image, body := range v.Manifests {
		m, err := brick.ParseStoredManifest(body)
		if err != nil {
			return nil, nil, err
		}
		manifests[image] = m
	}
	return fsys, manifests, nil
}

// What is captured is what the workflow reaches, and nothing else. A repository holds a great
// deal a workflow does not name, and a version that stored all of it would grow with the
// repository rather than with the workflow.
func TestAVersionHoldsWhatTheWorkflowReaches(t *testing.T) {
	v, err := version.Capture(tree(), "agentiik.yaml", manifests(t))
	if err != nil {
		t.Fatal(err)
	}
	if v.Entry != "agentiik.yaml" || len(v.Document) == 0 {
		t.Fatalf("the capture reads %+v", v.Entry)
	}
	if len(v.Includes) != 1 {
		t.Fatalf("the capture holds %d included files: %v", len(v.Includes), keys(v.Includes))
	}
	if _, held := v.Includes[".agentiik/common.yaml"]; !held {
		t.Errorf("the included file is not among %v", keys(v.Includes))
	}
	for _, unwanted := range []string{"README.md", "src/main.go", "testdata/big.bin"} {
		if _, held := v.Includes[unwanted]; held {
			t.Errorf("the capture holds %s, which the workflow does not name", unwanted)
		}
	}
	if len(v.Manifests) != 1 {
		t.Errorf("the capture holds %d manifests", len(v.Manifests))
	}
}

// And it rebuilds into the same graph with nothing in reach: no tree, no registry, no network.
func TestAVersionRebuildsWithNothingInReach(t *testing.T) {
	v, err := version.Capture(tree(), "agentiik.yaml", manifests(t))
	if err != nil {
		t.Fatal(err)
	}
	v.Workflow, v.Commit = "monthly-invoicing", "a3f9c1e"

	g, err := version.Build(v)
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Steps(); len(got) != 2 {
		t.Fatalf("the rebuilt graph holds %v", got)
	}
	// The step's image came from a hidden block in the included file, so a rebuild that had
	// lost the include would have failed rather than quietly produced a different graph.
	step, ok := g.Step("normalize")
	if !ok || step.Image != theImage {
		t.Errorf("normalize reads image %q", step.Image)
	}
	// And the default from the included file survived resolution.
	if step.Timeout == 0 {
		t.Error("the default timeout from the included file did not survive the round trip")
	}
	if got := g.Consumers("normalize", "ok"); len(got) != 1 || got[0] != "archive" {
		t.Errorf("the edge came back as %v", got)
	}
}

// A capture whose include is missing is refused at capture rather than at the first run of it,
// which is the difference between a push that fails and a run that fails at three in the morning.
func TestACaptureOfSomethingThatDoesNotLoadIsRefused(t *testing.T) {
	broken := tree()
	delete(broken, ".agentiik/common.yaml")
	if _, err := version.Capture(broken, "agentiik.yaml", manifests(t)); err == nil {
		t.Error("a workflow whose include is missing was captured")
	}

	// And one that loads but does not check: an edge to a step nobody declared.
	bad := fstest.MapFS{"agentiik.yaml": &fstest.MapFile{Data: []byte(`
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: broken }
steps:
  archive:
    image: ` + theImage + `
    needs: [{ step: nobody, port: ok, as: x }]
    outputs: [ok]
`)}}
	if _, err := version.Capture(bad, "agentiik.yaml", manifests(t)); err == nil {
		t.Error("a workflow with an edge to nothing was captured")
	}
}

// The store reads a version back and answers the same graph twice without reading it twice, which
// is what "one graph can serve every run of a version" is worth in practice.
func TestTheStoreAnswersOneGraphPerVersion(t *testing.T) {
	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
		t.Fatal(err)
	}

	v, err := version.Capture(tree(), "agentiik.yaml", manifests(t))
	if err != nil {
		t.Fatal(err)
	}
	v.Workflow, v.Commit, v.Author = "monthly-invoicing", "a3f9c1e", "alice"

	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		if err := ns.SaveWorkflow(ctx, "monthly-invoicing", "main"); err != nil {
			return err
		}
		_, err := ns.SaveVersion(ctx, v)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	store, err := version.New(pool, version.Options{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Graph(t.Context(), "finance", "monthly-invoicing", "a3f9c1e")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Graph(t.Context(), "finance", "monthly-invoicing", "a3f9c1e")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("two asks for one version answered two graphs, and a graph is immutable for the life of a version")
	}

	// A commit nobody pushed is refused rather than answered with something near it.
	if _, err := store.Graph(t.Context(), "finance", "monthly-invoicing", "deadbee"); err == nil {
		t.Error("a commit nobody pushed answered a graph")
	}
}

// "A version is a commit", so pushing the same commit twice is the same version, and the second
// push does not overwrite what a run pinned to it is already using.
func TestPushingOneCommitTwiceLeavesItAsItWas(t *testing.T) {
	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
		t.Fatal(err)
	}

	first, err := version.Capture(tree(), "agentiik.yaml", manifests(t))
	if err != nil {
		t.Fatal(err)
	}
	first.Workflow, first.Commit, first.Author = "monthly-invoicing", "a3f9c1e", "alice"

	// A second capture of a tree that says something else, pushed at the same commit, which
	// is either a mistake or somebody trying something.
	changed := tree()
	changed["agentiik.yaml"] = &fstest.MapFile{Data: []byte(entryPoint + `
  extra:
    extends: .brick
    outputs: [ok]
`)}
	second, err := version.Capture(changed, "agentiik.yaml", manifests(t))
	if err != nil {
		t.Fatal(err)
	}
	second.Workflow, second.Commit, second.Author = "monthly-invoicing", "a3f9c1e", "mallory"

	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		if err := ns.SaveWorkflow(ctx, "monthly-invoicing", "main"); err != nil {
			return err
		}
		if _, err := ns.SaveVersion(ctx, first); err != nil {
			return err
		}
		_, err := ns.SaveVersion(ctx, second)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var back db.Version
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		var err error
		back, err = ns.Version(ctx, "monthly-invoicing", "a3f9c1e")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if back.Author != "alice" {
		t.Errorf("the version was overwritten by the second push, and it reads author %q", back.Author)
	}
	g, err := version.Build(back)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Steps()) != 2 {
		t.Errorf("the version holds %v, which is what the second push wrote", g.Steps())
	}
}

// A workflow naming its images by tag, as one written against a laptop's daemon does: a brick
// step, and a script step in a base image.
const tagged = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: normalize, port: ok } }
steps:
  normalize:
    image: ghcr.io/acme/agk-invoice:1.4.0
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
  report:
    image: alpine:3.21
    needs: [{ step: normalize, port: ok, as: in }]
    script: ["cat /agk/in/in/envelope.json"]
    outputs: [out]
`

const (
	invoiceDigest = "ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc"
	alpineDigest  = "alpine@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d"
)

// aTaggedVersion is that workflow as a push records it: the manifest under the tag the file
// writes, and the digest each tag was resolved to.
func aTaggedVersion(t *testing.T) db.Version {
	t.Helper()
	return db.Version{
		Workflow: "monthly-invoicing", Commit: "a3f9c1e",
		Entry: "agentiik.yaml", Document: []byte(tagged),
		Manifests: map[string][]byte{"ghcr.io/acme/agk-invoice:1.4.0": []byte(manifest)},
		Images: map[string]string{
			"ghcr.io/acme/agk-invoice:1.4.0": invoiceDigest,
			"alpine:3.21":                    alpineDigest,
		},
	}
}

// "Images by digest in production: a tag is a mutable pointer, and a commit must determine what
// ran." So a step the file names by tag is rebuilt naming the digest the push resolved it to, a
// script step's base image included, and is held to the manifest read out of that image.
func TestATagIsRebuiltAsTheDigestThePushResolvedItTo(t *testing.T) {
	g, err := version.Build(aTaggedVersion(t))
	if err != nil {
		t.Fatal(err)
	}
	for step, want := range map[agk.Step]string{"normalize": invoiceDigest, "report": alpineDigest} {
		if st, ok := g.Step(step); !ok || st.Image != want {
			t.Errorf("%s names %q, want %q", step, st.Image, want)
		}
	}
}

// A tag the version recorded no digest for is refused rather than dispatched: the controller
// reaches no registry to resolve it with, and no runner may take a message naming one.
func TestATagWithNoDigestRecordedIsRefused(t *testing.T) {
	v := aTaggedVersion(t)
	delete(v.Images, "alpine:3.21")
	_, err := version.Build(v)
	if err == nil {
		t.Fatal("a version naming a tag with no digest recorded for it was built")
	}
	for _, want := range []string{"report", "alpine:3.21", "agk push"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// What a version records for a tag is the tag's own repository at a sha256 digest, and only for
// a tag a step names: a file that says one image while its runs pull another is refused.
func TestADigestRecordedForSomethingElseIsRefused(t *testing.T) {
	for what, images := range map[string]map[string]string{
		"another repository": {"ghcr.io/acme/agk-invoice:1.4.0": "ghcr.io/mallory/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc"},
		"another spelling":   {"alpine:3.21": "docker.io/library/alpine@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d"},
		"a tag again":        {"alpine:3.21": "alpine:3.22"},
		"a tag and a digest": {"alpine:3.21": "alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d"},
		"a short digest":     {"alpine:3.21": "alpine@sha256:48b0309c"},
		"a tag nobody names": {"busybox:1.37": "busybox@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d"},
	} {
		v := aTaggedVersion(t)
		maps.Copy(v.Images, images)
		if _, err := version.Build(v); err == nil {
			t.Errorf("a version recording %s was built", what)
		}
	}

	// And a digest the file itself writes is kept as written, with nothing to record.
	v := aTaggedVersion(t)
	v.Document = []byte(strings.Replace(tagged, "image: alpine:3.21", "image: "+alpineDigest, 1))
	delete(v.Images, "alpine:3.21")
	g, err := version.Build(v)
	if err != nil {
		t.Fatalf("a version naming a digest of its own was refused: %v", err)
	}
	if st, _ := g.Step("report"); st.Image != alpineDigest {
		t.Errorf("report names %q", st.Image)
	}
	v.Document = []byte(strings.Replace(tagged, "image: alpine:3.21", "image: alpine@sha256:48b0309c", 1))
	if _, err := version.Build(v); err == nil || !strings.Contains(err.Error(), "sixty-four") {
		t.Errorf("a step naming a digest that is not one answered %v", err)
	}
}

// Two tags resolved to one digest are one image, held to one manifest, and a version carrying two
// different manifests for it is refused rather than holding one step to the other's.
func TestTwoTagsOfOneImageAreOneManifest(t *testing.T) {
	v := aTaggedVersion(t)
	v.Document = []byte(tagged + `
  archive:
    image: ghcr.io/acme/agk-invoice:latest
    needs: [{ step: normalize, port: ok, as: orders }]
    outputs: [ok]
`)
	v.Images["ghcr.io/acme/agk-invoice:latest"] = invoiceDigest
	v.Manifests["ghcr.io/acme/agk-invoice:latest"] = []byte(manifest)
	if _, err := version.Build(v); err != nil {
		t.Fatalf("two tags of one image with one manifest were refused: %v", err)
	}

	v.Manifests["ghcr.io/acme/agk-invoice:latest"] = []byte(strings.Replace(manifest, "version: 1.0.0", "version: 2.0.0", 1))
	if _, err := version.Build(v); err == nil {
		t.Error("two tags of one image with two manifests were built")
	}
}

// Names of 251 to 255 characters: accepted before the bound a port is written to, agk.PortMaxBytes,
// and so held by a version an installation stored then.
var (
	longPort   = "rejected-" + strings.Repeat("x", 242)
	longInput  = "orders-" + strings.Repeat("x", 246)
	longOutput = "unmatched-" + strings.Repeat("x", 245)
	longSpare  = "spare-" + strings.Repeat("x", 246)
	longUnused = "unused-" + strings.Repeat("x", 244)
)

// aVersionNaming is a version whose entry point names port, input and output, whose included file
// gives its steps a hidden block declaring spare, which each step's own outputs replace, and whose
// brick manifest declares the ports the steps use and unused besides.
func aVersionNaming(port, input, output, spare, unused string) db.Version {
	return db.Version{
		Workflow: "monthly-invoicing", Commit: "a3f9c1e", Author: "alice",
		Entry: "agentiik.yaml", Document: []byte(`
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
include:
  - path: .agentiik/common.yaml
outputs:
  ` + output + `: { from: { step: normalize, port: ` + port + ` } }
steps:
  normalize:
    extends: .brick
    outputs: [ok, ` + port + `]
  archive:
    extends: .brick
    needs: [{ step: normalize, port: ` + port + `, as: ` + input + ` }]
    outputs: [ok]
`),
		Includes: map[string][]byte{".agentiik/common.yaml": []byte(`
.brick:
  image: ` + theImage + `
  outputs: [` + spare + `]
`)},
		Manifests: map[string][]byte{theImage: []byte(`
apiVersion: agentiik.dev/v1
kind: Brick
metadata: { name: invoice, version: 1.0.0 }
spec:
  inputs:
    ` + input + `: {}
  outputs:
    ok: {}
    ` + port + `: {}
    ` + unused + `: {}
  runtime: { user: "65532:65532" }
`)},
	}
}

// A version stored before the bound a port is written to is rebuilt as it was accepted, so that its
// runs, its replays and the rebuild of its graph go on after an upgrade; a version about to be
// recorded is held to the bound in its entry point, in the file it includes and in its manifests.
func TestAVersionStoredBeforeThePortBoundIsRebuiltAsItWasAccepted(t *testing.T) {
	g, err := version.Build(aVersionNaming(longPort, longInput, longOutput, longSpare, longUnused))
	if err != nil {
		t.Fatalf("a stored version naming ports of 251 to 255 characters was refused when rebuilt: %v", err)
	}
	if got := g.Consumers("normalize", agk.Port(longPort)); len(got) != 1 || got[0] != "archive" {
		t.Errorf("the edge on the port of 251 characters reaches %v", got)
	}

	made := func(v db.Version) error {
		fsys, manifests, err := bundled(v)
		if err != nil {
			return err
		}
		_, err = version.Capture(fsys, "agentiik.yaml", manifests)
		return err
	}
	if err := made(aVersionNaming("rejected", "in", "unmatched", "spare", "unused")); err != nil {
		t.Fatalf("a new version naming no port past 250 characters was refused: %v", err)
	}
	for where, v := range map[string]db.Version{
		"its entry point":      aVersionNaming(longPort, longInput, longOutput, "spare", "unused"),
		"the file it includes": aVersionNaming("rejected", "in", "unmatched", longSpare, "unused"),
		"its manifest":         aVersionNaming("rejected", "in", "unmatched", "spare", longUnused),
	} {
		if _, err := version.Build(v); err != nil {
			t.Errorf("a stored version with a port past 250 characters in %s was refused when rebuilt: %v", where, err)
		}
		if err := made(v); err == nil || !strings.Contains(err.Error(), "at most 250") {
			t.Errorf("a new version with a port past 250 characters in %s was built, or refused with %v", where, err)
		}
	}
}

// And the store, which is what the controller decides a run from and the API starts one with,
// reads such a version out of the database and answers its graph, as it did before the upgrade.
func TestTheStoreAnswersAVersionStoredBeforeThePortBound(t *testing.T) {
	pool, super := dbtest.Open(t)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
		t.Fatal(err)
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		if err := ns.SaveWorkflow(ctx, "monthly-invoicing", "main"); err != nil {
			return err
		}
		_, err := ns.SaveVersion(ctx, aVersionNaming(longPort, longInput, longOutput, longSpare, longUnused))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	store, err := version.New(pool, version.Options{})
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.Graph(t.Context(), "finance", "monthly-invoicing", "a3f9c1e")
	if err != nil {
		t.Fatalf("a version stored before the port bound was not answered: %v", err)
	}
	if _, held := g.Workflow().Outputs[longOutput]; !held {
		t.Error("the workflow output of 255 characters did not survive the round trip")
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
