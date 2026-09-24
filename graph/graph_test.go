package graph

import (
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/brick"
)

// normalizeManifest is the manifest the workflow fixtures on ports are checked against:
// two input ports, two output ports, a port declared with no schema, and two parameters.
const normalizeManifest = `
apiVersion: agentiik.dev/v1
kind: Brick
metadata:
  name: normalize
  version: 2.0.0
spec:
  inputs:
    orders:
      schema: { $ref: "#/spec/definitions/order" }
    customers: {}
  outputs:
    ok:
      schema: { $ref: "#/spec/definitions/order" }
    rejected: {}
  params:
    currency: { type: string, pattern: "^[A-Z]{3}$" }
    retries: { type: integer, maximum: 5 }
  runtime:
    user: "65532:65532"
  definitions:
    order: { type: object, required: [customer_id] }
`

// manifests builds the map Build takes, giving every image of the workflow the same
// manifest, which is what a test with one brick in it needs.
func manifests(t *testing.T, wf *Workflow, doc string) map[string]brick.Manifest {
	t.Helper()
	m, err := brick.ParseManifest([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]brick.Manifest{}
	for _, image := range Images(wf) {
		out[image] = m
	}
	return out
}

func built(t *testing.T, doc string, manifest string) *Graph {
	t.Helper()
	wf := parsed(t, doc)
	g, err := Build(wf, manifests(t, wf, manifest))
	if err != nil {
		t.Fatalf("this workflow was refused when built: %v", err)
	}
	return g
}

// threeSteps is the documentation's own graph: two inputs, two outputs, and an edge that
// bypasses the middle step.
const threeSteps = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
outputs:
  invoices:
    from: { step: archive, port: ok }
steps:
  archive:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    needs:
      - { step: invoice, port: ok, as: orders }
      - { step: normalize, port: rejected, as: customers }
    outputs: [ok]
  invoice:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    needs:
      - { step: normalize, port: ok, as: orders }
    outputs: [ok]
  normalize:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    outputs: [ok, rejected]
`

// TestTheOrderFollowsTheEdgesAndNotTheFile holds the sentence that makes the graph a
// graph: "there is no implicit stage, and the order in which steps appear in the file has
// no bearing on scheduling".
func TestTheOrderFollowsTheEdgesAndNotTheFile(t *testing.T) {
	g := built(t, threeSteps, normalizeManifest)

	order := g.Order()
	want := []agk.Step{"normalize", "invoice", "archive"}
	if !slices.Equal(order, want) {
		t.Fatalf("the steps were ordered %v, and the edges say %v", order, want)
	}
	if steps := g.Steps(); !slices.Equal(steps, []agk.Step{"archive", "invoice", "normalize"}) {
		t.Fatalf("the steps of the graph are %v", steps)
	}
}

// TestTheEdgesKeepTheirDeclarationOrder is load bearing downstream: "wait_all waits for
// every upstream port, then concatenates items in edge declaration order".
func TestTheEdgesKeepTheirDeclarationOrder(t *testing.T) {
	g := built(t, threeSteps, normalizeManifest)

	edges := g.Edges("archive")
	if len(edges) != 2 || edges[0].Step != "invoice" || edges[1].Step != "normalize" {
		t.Fatalf("the edges of archive are %+v", edges)
	}
}

// TestConsumersReadsTheGraphTheOtherWayRound is what merge: first asks: whether anything
// else is still waiting on what a step publishes.
func TestConsumersReadsTheGraphTheOtherWayRound(t *testing.T) {
	g := built(t, threeSteps, normalizeManifest)

	if got := g.Consumers("normalize", "ok"); !slices.Equal(got, []agk.Step{"invoice"}) {
		t.Errorf("the consumers of normalize.ok are %v", got)
	}
	if got := g.Consumers("normalize", "rejected"); !slices.Equal(got, []agk.Step{"archive"}) {
		t.Errorf("the consumers of normalize.rejected are %v", got)
	}
	if got := g.Consumers("archive", "ok"); len(got) != 0 {
		t.Errorf("the port a workflow output takes has the step consumers %v", got)
	}
}

// TestImagesNamesTheManifestsToFetch holds what the list is for, and the one step that is
// not on it: a script step's image "is treated as a base image and nothing about its
// ports is inferred", so no manifest is read for it.
func TestImagesNamesTheManifestsToFetch(t *testing.T) {
	wf := parsed(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: order-loading }
steps:
  normalize:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    outputs: [ok]
  again:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    outputs: [ok]
  check-vat:
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    script: [echo one]
    outputs: [out]
  remind:
    workflow: finance/common@v2.1.0
`)
	images := Images(wf)
	if len(images) != 1 || images[0] != "ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0" {
		t.Fatalf("the manifests to fetch are %v", images)
	}
}

// TestAManifestThatWasNotHandedInIsARefusal holds the boundary: "reading /agk/brick.yaml
// means pulling an image, and pulling an image is executing", so the evaluator says which
// manifests it needs and refuses to guess at one it was not given.
func TestAManifestThatWasNotHandedInIsARefusal(t *testing.T) {
	wf := parsed(t, threeSteps)
	_, err := Build(wf, nil)
	held(t, err, RuleManifestMissing)
}

// TestBuildChecksBeforeItBuilds keeps the two calls from being an order somebody has to
// remember: a graph built out of a workflow whose edges name steps that do not exist is
// not a graph.
func TestBuildChecksBeforeItBuilds(t *testing.T) {
	wf := parsed(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing }
steps:
  archive:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    needs: [{ step: enrich, port: out, as: orders }]
    outputs: [ok]
`)
	_, err := Build(wf, manifests(t, wf, normalizeManifest))
	held(t, err, RuleNeedsUnknownStep)
}

// TestTheGraphCarriesTheResolvedStep is what the evaluator reads: a step whose keywords
// have already had defaults and extends applied to them, so that nothing downstream has
// to know where a value came from.
func TestTheGraphCarriesTheResolvedStep(t *testing.T) {
	g := built(t, threeSteps, normalizeManifest)

	st, ok := g.Step("normalize")
	if !ok {
		t.Fatal("the graph carries no step of that name")
	}
	if !st.Idempotent {
		t.Error("the step was not resolved: idempotent is true by default")
	}
	if _, ok := g.Step("nowhere"); ok {
		t.Error("the graph answered for a step nobody declared")
	}
	if g.Workflow() == nil || g.Workflow().Metadata.Name != "monthly-invoicing" {
		t.Error("the graph does not carry the workflow it was built from")
	}
}

// TestAWorkflowCalledInAReservedNamespaceIsRefused holds the one name the corpus does not
// write: the namespace half of a sub-workflow call, which is a namespace's name like any
// other, and so cannot be a word the API routes on.
func TestAWorkflowCalledInAReservedNamespaceIsRefused(t *testing.T) {
	_, err := Parse([]byte(`
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: order-loading }
steps:
  remind:
    workflow: runs/common@v2.1.0
`))
	if err == nil || !strings.Contains(err.Error(), `"runs"`) {
		t.Fatalf("a call into the namespace runs was read: %v", err)
	}
}
