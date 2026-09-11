package graph

import (
	"strings"
	"testing"

	"github.com/agentiik/agentiik/brick"
)

// builtWith runs Build over a document and one manifest, and returns what it refused, so
// that each test below names the rule and nothing else.
func builtWith(t *testing.T, doc, manifest string) error {
	t.Helper()
	wf := parsed(t, doc)
	_, err := Build(wf, manifests(t, wf, manifest))
	return err
}

// step writes one step around a body, so that the rules below read as the one line that
// differs between them.
func step(body string) string {
	return `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing }
steps:
  normalize:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
` + body
}

// TestOutputsAreASubsetOfTheManifest holds the rule as the table writes it: "declared
// output ports. Must be a subset of the ports in the brick manifest."
func TestTheOutputsAreASubsetOfTheManifest(t *testing.T) {
	err := builtWith(t, step("    outputs: [ok, maybe]\n"), normalizeManifest)
	held(t, err, RuleStepOutputNotInManifest)
	if !strings.Contains(err.Error(), "ok, rejected") {
		t.Fatalf("the refusal does not say what the manifest does declare: %v", err)
	}

	if err := builtWith(t, step("    outputs: [ok, rejected]\n"), normalizeManifest); err != nil {
		t.Fatalf("a step declaring a subset of the manifest's ports was refused: %v", err)
	}
}

// TestAJoinWritesAPortTheBrickNeverWrites records the reading the group took: a key join
// publishes what it could not match "on the step's own unmatched output port, without the
// container ever touching it, which is why a step doing a join declares it in outputs".
// Holding that port to the manifest would ask a brick to declare a port it never writes.
func TestAJoinWritesAPortTheBrickNeverWrites(t *testing.T) {
	const joined = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: reconciliation }
steps:
  left:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    outputs: [ok]
  right:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    outputs: [ok]
  match:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    needs:
      - { step: left, port: ok, as: orders }
      - { step: right, port: ok, as: customers }
    merge: { join: { on: "$.data.customer_id" } }
    outputs: [ok, unmatched]
`
	if err := builtWith(t, joined, normalizeManifest); err != nil {
		t.Fatalf("a step doing a join was refused its unmatched port: %v", err)
	}

	// Without the join it is an ordinary port, and the manifest answers for it.
	held(t, builtWith(t, strings.Replace(joined, `    merge: { join: { on: "$.data.customer_id" } }`, "    merge: wait_all", 1), normalizeManifest), RuleStepOutputNotInManifest)
}

// TestAnInputPortIsOneTheBrickDeclares holds the rule and its reason: "the runner mounts
// one directory per port the brick declares, and there is no directory for a port the
// manifest never named". A port is fed two ways, and both are held to it.
func TestAnInputPortIsOneTheBrickDeclares(t *testing.T) {
	held(t, builtWith(t, step("    inputs:\n      suppliers: ${{ workflow.inputs.suppliers }}\n    outputs: [ok]\n"), normalizeManifest), RuleStepInputPortNotInManifest)

	const edged = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing }
steps:
  upstream:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    outputs: [ok]
  normalize:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    needs: [{ step: upstream, port: ok, as: suppliers }]
    outputs: [ok]
`
	held(t, builtWith(t, edged, normalizeManifest), RuleStepInputPortNotInManifest)
	if err := builtWith(t, strings.Replace(edged, "as: suppliers", "as: orders", 1), normalizeManifest); err != nil {
		t.Fatalf("an edge feeding a port the manifest declares was refused: %v", err)
	}
}

// TestAScriptStepIsHeldToNoManifest holds the sentence that exempts it: "the image is
// treated as a base image and nothing about its ports is inferred".
func TestAScriptStepIsHeldToNoManifest(t *testing.T) {
	wf := parsed(t, step("    script: [echo one]\n    outputs: [whatever]\n"))
	if _, err := Build(wf, nil); err != nil {
		t.Fatalf("a script step was held to a manifest nobody read: %v", err)
	}
}

// TestParamsAreValidatedAgainstTheManifestSchema holds the table's own words: "brick
// parameters, validated against the manifest schema once expressions are resolved".
func TestParamsAreValidatedAgainstTheManifestSchema(t *testing.T) {
	held(t, builtWith(t, step("    params:\n      currency: euro\n    outputs: [ok]\n"), normalizeManifest), RuleParamsAgainstManifest)
	held(t, builtWith(t, step("    params:\n      retries: 9\n    outputs: [ok]\n"), normalizeManifest), RuleParamsAgainstManifest)
	held(t, builtWith(t, step("    params:\n      elsewhere: 1\n    outputs: [ok]\n"), normalizeManifest), RuleParamsAgainstManifest)

	if err := builtWith(t, step("    params:\n      currency: EUR\n      retries: 2\n    outputs: [ok]\n"), normalizeManifest); err != nil {
		t.Fatalf("parameters the manifest accepts were refused: %v", err)
	}
}

// TestAParameterWrittenAsAnExpressionWaits is the other half of the same sentence: a
// value the run has yet to settle is checked where it is resolved, and not before.
func TestAParameterWrittenAsAnExpressionWaits(t *testing.T) {
	if err := builtWith(t, step("    params:\n      currency: ${{ vars.currency }}\n    outputs: [ok]\n"), normalizeManifest); err != nil {
		t.Fatalf("a parameter written as an expression was validated before it had a value: %v", err)
	}

	// Once the run has resolved it, the same schema answers, and the same rule refuses
	// it. This is the call the evaluator makes when it builds a task.
	m, err := brick.ParseManifest([]byte(normalizeManifest))
	if err != nil {
		t.Fatal(err)
	}
	st := Step{Params: map[string]any{"currency": "euro"}}
	held(t, paramsAgainstManifest("normalize", st, m, map[string]any{"currency": "euro"}, true), RuleParamsAgainstManifest)
	if err := paramsAgainstManifest("normalize", st, m, map[string]any{"currency": "EUR"}, true); err != nil {
		t.Fatalf("a resolved parameter the schema accepts was refused: %v", err)
	}
}

// TestARequiredParameterIsRefusedAtValidation holds the decision written beside the
// parameter table: required: true says a step must supply it, "and a missing one is
// refused when the workflow is validated rather than inside a container".
func TestARequiredParameterIsRefusedAtValidation(t *testing.T) {
	const required = `
apiVersion: agentiik.dev/v1
kind: Brick
metadata: { name: normalize, version: 2.0.0 }
spec:
  outputs:
    ok: {}
  params:
    currency: { type: string, required: true }
  runtime:
    user: "65532:65532"
`
	err := builtWith(t, step("    outputs: [ok]\n"), required)
	held(t, err, RuleParamsAgainstManifest)
	if !strings.Contains(err.Error(), "currency") {
		t.Fatalf("the refusal does not name the parameter: %v", err)
	}

	if err := builtWith(t, step("    params:\n      currency: EUR\n    outputs: [ok]\n"), required); err != nil {
		t.Fatalf("a step supplying the required parameter was refused: %v", err)
	}
}
