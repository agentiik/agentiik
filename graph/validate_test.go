package graph

import (
	"strings"
	"testing"
)

// checked reads a document that has to be readable and returns what Check made of it, so
// that a test says which rule refused it rather than where.
func checked(t *testing.T, doc string) error {
	t.Helper()
	return Check(parsed(t, doc))
}

// TestACycleIsRejectedAtValidation holds the rule and the reason: "cycles are rejected at
// validation, before registration. Looping is done by calling a sub-workflow, with a
// configurable maximum depth."
func TestACycleIsRejectedAtValidation(t *testing.T) {
	err := checked(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing }
steps:
  normalize:
    image: a@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    needs: [{ step: archive, port: out, as: in }]
    outputs: [ok]
  invoice:
    image: b@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    needs: [{ step: normalize, port: ok, as: in }]
    outputs: [out]
  archive:
    image: c@sha256:44de908840a673cc25120ecd3e506292d691f72937ce1da04a6ee4252ff4c115
    needs: [{ step: invoice, port: out, as: in }]
    outputs: [out]
`)
	held(t, err, RuleCycleInGraph)
	for _, name := range []string{"normalize", "invoice", "archive"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not name %s, so the cycle cannot be followed: %v", name, err)
		}
	}
}

// TestAStepThatNeedsItselfIsACycleToo is the shortest cycle there is, and the one a
// reader is most likely to write by hand.
func TestAStepThatNeedsItselfIsACycleToo(t *testing.T) {
	held(t, checked(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: looping }
steps:
  one:
    image: a@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    needs: [{ step: one, port: out, as: in }]
    outputs: [out]
`), RuleCycleInGraph)
}

// TestAnEdgeNamesSomethingThatExists holds what an edge is: "an edge is the only form of
// dependency there is, so it has to name something that exists".
func TestAnEdgeNamesSomethingThatExists(t *testing.T) {
	held(t, checked(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing }
steps:
  archive:
    image: c@sha256:44de908840a673cc25120ecd3e506292d691f72937ce1da04a6ee4252ff4c115
    needs: [{ step: enrich, port: out, as: in }]
    outputs: [out]
`), RuleNeedsUnknownStep)
}

// TestAnEdgeCannotInventAPort holds the other half: "normalize publishes ok and rejected,
// and an edge cannot invent a third port on it".
func TestAnEdgeCannotInventAPort(t *testing.T) {
	err := checked(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing }
steps:
  normalize:
    image: a@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    outputs: [ok, rejected]
  archive:
    image: c@sha256:44de908840a673cc25120ecd3e506292d691f72937ce1da04a6ee4252ff4c115
    needs: [{ step: normalize, port: maybe, as: in }]
    outputs: [out]
`)
	held(t, err, RuleEdgePortNotDeclared)
	if !strings.Contains(err.Error(), "ok, rejected") {
		t.Fatalf("the refusal does not say what the step does publish: %v", err)
	}
}

// TestAnEdgeOntoASubWorkflowCallIsNotCheckedHere records the reading beside the rule that
// takes it: the ports of a call are the declared outputs of the workflow it calls, which
// this commit does not carry and this package never fetches.
func TestAnEdgeOntoASubWorkflowCallIsNotCheckedHere(t *testing.T) {
	if err := checked(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: dunning }
steps:
  remind:
    workflow: finance/common@v2.1.0
  escalate:
    workflow: finance/common@v2.1.0
    needs: [remind]
`); err != nil {
		t.Fatalf("an edge onto a sub-workflow call was refused: %v", err)
	}
}

// TestAWorkflowOutputIsTakenFromAStepThatExists holds the rule the corpus names, and the
// port beside it: an output is "a view of one step port".
func TestAWorkflowOutputIsTakenFromAStepThatExists(t *testing.T) {
	held(t, checked(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing }
outputs:
  invoices:
    from: { step: publish, port: out }
steps:
  archive:
    image: c@sha256:44de908840a673cc25120ecd3e506292d691f72937ce1da04a6ee4252ff4c115
    outputs: [out]
`), RuleOutputFromUnknownStep)

	held(t, checked(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing }
outputs:
  invoices:
    from: { step: archive, port: elsewhere }
steps:
  archive:
    image: c@sha256:44de908840a673cc25120ecd3e506292d691f72937ce1da04a6ee4252ff4c115
    outputs: [out]
`), RuleEdgePortNotDeclared)
}

// TestASecretIsMountedByTheNameTheWorkflowGivesIt holds the rule: "secrets are mounted by
// the name the secrets block gives them, and only secrets of the owning namespace can be
// referenced".
func TestASecretIsMountedByTheNameTheWorkflowGivesIt(t *testing.T) {
	held(t, checked(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing }
steps:
  invoice:
    image: b@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    secrets: [billing]
    outputs: [out]
`), RuleSecretNotDeclared)

	if err := checked(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing }
secrets:
  billing:
    provider: vault
    path: kv/data/agentiik/billing
steps:
  invoice:
    image: b@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    secrets: [billing]
    outputs: [out]
`); err != nil {
		t.Fatalf("a step mounting a declared secret was refused: %v", err)
	}
}

// TestAScriptStepDeclaresItsPorts holds the sentence that makes it a rule: "the image is
// a base image, no manifest is read and nothing about its ports is inferred, so outputs
// must be declared".
func TestAScriptStepDeclaresItsPorts(t *testing.T) {
	held(t, checked(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: order-loading }
steps:
  normalize:
    image: python:3.13-slim@sha256:1c4d5e543ecc82da20b5839890bfa7ea486a005598b2a3bde6b385c4e2b59a02
    script:
      - python /agk/repo/scripts/normalize.py
`), RuleScriptWithoutOutputs)
}

// TestAStepRunsSomething is the reading taken where the documentation is silent: the
// language requires no keyword of a step, and a step that runs neither an image nor
// another workflow is a step nothing could ever start.
func TestAStepRunsSomething(t *testing.T) {
	err := checked(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: empty }
steps:
  one:
    outputs: [out]
`)
	if err == nil {
		t.Fatal("a step that runs nothing was accepted")
	}
	if !strings.Contains(err.Error(), "one") {
		t.Fatalf("the refusal does not name the step: %v", err)
	}
}

// A matrix fan-out is the cartesian product of variable lists, and every combination is a
// shard. A step that names the strategy and declares no list is a product of nothing.
// Left standing it runs as one shard carrying an empty combination, so an expression
// reading the combination resolves to nothing inside a container instead of the file
// being refused before the push.
func TestAMatrixFanOutWithNoMatrixIsAShardOfNothing(t *testing.T) {
	wf := parsed(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: regional-vat
steps:
  rate-table:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    strategy:
      fan_out: matrix
    outputs: [out]
`)
	err := Check(wf)
	if err == nil {
		st := wf.Steps["rate-table"]
		shards, _ := fanOut("rate-table", &st, nil)
		t.Fatalf("the workflow was accepted, and the step runs as %d shard carrying %v", len(shards), shards[0].Matrix)
	}
	if !strings.Contains(err.Error(), "declares no matrix") {
		t.Errorf("refused by %v, want the matrix rule", err)
	}
}
