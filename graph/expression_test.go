package graph

import (
	"errors"
	"strings"
	"testing"
)

// The exposed-context table says which roots each position reads. These hold the table
// as the documentation writes it: the rule is what a keyword may read where it sits, and
// not which function reads it.

// TestAPositionReadsTheRootsTheTableGivesIt walks the table's third column. Each case is
// one root written in one keyword, and whether the table says it may be.
func TestAPositionReadsTheRootsTheTableGivesIt(t *testing.T) {
	for _, c := range []struct {
		name     string
		step     string
		accepted bool
		why      string
	}{
		{
			name:     "a condition reads port metadata",
			step:     `if: ${{ inputs.in.count > 0 }}`,
			accepted: true,
			why:      "inputs is the metadata of the current step's input ports, and it is available in a step",
		},
		{
			name:     "a condition reads the status of another step",
			step:     `if: ${{ steps.normalize.status == "succeeded" }}`,
			accepted: true,
			why:      "steps.<id>.status is available in a step",
		},
		{
			name:     "a condition reads a variable",
			step:     `if: ${{ vars.currency == "EUR" }}`,
			accepted: true,
			why:      "vars is available everywhere",
		},
		{
			name:     "a condition reads the trigger",
			step:     `if: ${{ trigger.body.cycle == "standard" }}`,
			accepted: false,
			why:      "trigger is available in the on block and in step parameters, and a condition is neither",
		},
		{
			name:     "a condition reads an event",
			step:     `if: ${{ event.type == "com.example.order.approved" }}`,
			accepted: false,
			why:      "event is available to event triggers alone",
		},
		{
			name: "a parameter reads the trigger",
			step: `params:
      currency: ${{ trigger.body.currency }}`,
			accepted: true,
			why:      "trigger is available in the on block and in step parameters",
		},
		{
			name: "a parameter reads a secret",
			step: `params:
      token: ${{ secrets.billing }}`,
			accepted: true,
			why:      "secrets is exposed to params and to secrets",
		},
		{
			name: "an input port is fed from a workflow input",
			step: `inputs:
      in: ${{ workflow.inputs.orders }}`,
			accepted: true,
			why:      "workflow is available everywhere",
		},
		{
			name: "an input port is fed from a secret",
			step: `inputs:
      in: ${{ secrets.billing }}`,
			accepted: false,
			why:      "secrets is exposed to params and to secrets and nowhere else",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := Check(parseOrFail(t, workflowWith(c.step)))
			switch {
			case c.accepted && err != nil:
				t.Fatalf("the expression was refused, and %s: %v", c.why, err)
			case !c.accepted && err == nil:
				t.Fatalf("the expression was accepted, and %s", c.why)
			}
		})
	}
}

// TestItemIsReadOnlyWhereThereIsAnItem holds the decision the documentation states in
// its own words: "item contents are not exposed to controller expressions, except under
// fan_out: item where only the current item is".
func TestItemIsReadOnlyWhereThereIsAnItem(t *testing.T) {
	for _, c := range []struct {
		fanOut   string
		accepted bool
	}{
		{fanOut: "item", accepted: true},
		{fanOut: "batch(50)", accepted: false},
		{fanOut: "none", accepted: false},
	} {
		t.Run(c.fanOut, func(t *testing.T) {
			err := Check(parseOrFail(t, workflowWith(`strategy:
      fan_out: `+c.fanOut+`
    params:
      customer: ${{ item.data.customer_id }}`)))
			if c.accepted {
				if err != nil {
					t.Fatalf("item was refused under fan_out: %s, where each shard receives a single-item envelope: %v", c.fanOut, err)
				}
				return
			}
			held(t, err, RuleExpressionItemOutsideFanOutItem)
		})
	}
}

// TestAMatrixReadsItsCombination holds the other half of the shard position: a matrix
// fan-out is a fan-out even where the file does not spell fan_out out, and the
// combination is what its shards read.
func TestAMatrixReadsItsCombination(t *testing.T) {
	err := Check(parseOrFail(t, workflowWith(`strategy:
      matrix:
        region: [fr, be, ch]
    params:
      region: ${{ matrix.region }}`)))
	if err != nil {
		t.Fatalf("a matrix shard was refused its own combination: %v", err)
	}
}

// TestAnExpressionIsRefusedWhereItWasWritten holds what a refusal is for. The person
// reading it is holding the file, so it names the keyword and the line.
func TestAnExpressionIsRefusedWhereItWasWritten(t *testing.T) {
	err := Check(parseOrFail(t, workflowWith(`params:
      currency: ${{ globals.currency }}`)))
	held(t, err, RuleExpressionUnknownRoot)

	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("the refusal carries no rule: %v", err)
	}
	if r.Step != "invoice" {
		t.Errorf("the refusal names the step %q", r.Step)
	}
	for _, want := range []string{"steps.invoice.params.currency", "globals", "written at line"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("the refusal does not say %q: %s", want, r.Detail)
		}
	}
}

// TestASecretFillsAWholeValueOrNothing holds the one rule an opaque value adds to the
// two interpolation rules: an expression that fills the whole value keeps its type, and
// a secret has no text for an embedded one to be converted to.
func TestASecretFillsAWholeValueOrNothing(t *testing.T) {
	if err := Check(parseOrFail(t, workflowWith(`params:
      token: ${{ secrets.billing }}`))); err != nil {
		t.Fatalf("a secret filling the whole value was refused: %v", err)
	}
	held(t, Check(parseOrFail(t, workflowWith(`params:
      token: "Bearer ${{ secrets.billing }}"`))), RuleExpressionSecretInText)
}

// TestAnExpressionThatIsNotOneIsRefusedBeforeTheRun holds the reason the expressions are
// compiled at validation: a run that cannot evaluate a condition is a run that should
// never have been registered.
func TestAnExpressionThatIsNotOneIsRefusedBeforeTheRun(t *testing.T) {
	for _, source := range []string{
		`if: ${{ inputs.in.count > }}`,
		`if: ${{ inputs.in.count`,
		`if: ${{ "text" - 1 }}`,
	} {
		held(t, Check(parseOrFail(t, workflowWith(source))), RuleExpressionDoesNotCompile)
	}
}

// TestAFanOutStillToArriveIsJudgedByLoadAndNotByParse holds the reading taken where the
// file has not been given its includes. A step whose strategy is in a block an included
// file carries has no fan_out in hand, and refusing its params would refuse a workflow
// the language accepts.
func TestAFanOutStillToArriveIsJudgedByLoadAndNotByParse(t *testing.T) {
	const doc = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: regional-vat
  namespace: finance
include:
  - path: ./common-bricks.yaml
steps:
  rate-table:
    extends: .region-matrix
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    params:
      region: ${{ matrix.region }}
    outputs: [out]
`
	if err := Check(parseOrFail(t, doc)); err != nil {
		t.Fatalf("a step whose strategy has not arrived was refused for reading it: %v", err)
	}
}

// workflowWith writes the smallest entry point that carries one step, with the keywords
// under test written into it.
func workflowWith(step string) string {
	return `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: monthly-invoicing
  namespace: finance
vars:
  currency: EUR
secrets: [billing]
steps:
  normalize:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    outputs: [ok]
  invoice:
    image: ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    needs:
      - { step: normalize, port: ok, as: in }
    secrets: [billing]
    outputs: [out]
    ` + step + "\n"
}

func parseOrFail(t *testing.T, doc string) *Workflow {
	t.Helper()
	wf, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("reading the workflow: %v", err)
	}
	return wf
}
