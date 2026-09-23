package version_test

import (
	"context"
	"testing"
	"testing/fstest"

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

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
