package graph

import (
	"strings"
	"testing"
)

// published writes a workflow whose boundary a tool can be a view of, with the tool block
// the test is about written into it.
func published(tools string) string {
	return `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: invoicing, namespace: finance }
inputs:
  orders:
    schema: { type: object }
    required: true
  period:
    required: true
outputs:
  invoices:
    from: { step: archive, port: out }
mcp:
  name: invoicing
  tools:
` + tools + `
steps:
  archive:
    image: ghcr.io/acme/agk-archive@sha256:44de908840a673cc25120ecd3e506292d691f72937ce1da04a6ee4252ff4c115
    outputs: [out]
`
}

// TestAToolIsAViewOfTheWorkflowsOwnBoundary holds the rule and its reason: "the reference
// is to a workflow input or output, never to a step or a port: a tool is a view of the
// workflow's own boundary, which is what keeps the graph free to change beneath it".
func TestAToolIsAViewOfTheWorkflowsOwnBoundary(t *testing.T) {
	held(t, checked(t, published(`    - name: create_invoice
      description: Issue one invoice.
      input: { from: { input: nowhere } }`)), RuleMCPToolInputNotDeclared)

	held(t, checked(t, published(`    - name: create_invoice
      description: Issue one invoice.
      input: { from: { input: orders } }
      output: { from: { output: nowhere } }`)), RuleMCPToolInputNotDeclared)

	// A tool input taken from a step port is refused when the document is read: the
	// from block carries one key and it is the workflow's own.
	refused(t, published(`    - name: create_invoice
      description: Issue one invoice.
      input: { from: { step: archive, port: out } }`))
}

// TestAToolHasToPublishAnInputSchema holds the rule and what it is good for: "a tool has
// to publish an inputSchema, so the input it maps has to have one: the surest way to
// discover a workflow input that was never given a schema".
func TestAToolHasToPublishAnInputSchema(t *testing.T) {
	held(t, checked(t, published(`    - name: reconcile_month
      description: Reconcile a whole month.
      input: { from: { input: period } }`)), RuleMCPToolInputWithoutSchema)
}

// TestTwoToolsCannotShareAName holds the rule and the reason it is a rule: "a tool
// identifier is unique within the workflow, because clients hold it. Renaming one is
// removing a tool and adding another."
func TestTwoToolsCannotShareAName(t *testing.T) {
	held(t, checked(t, published(`    - name: create_invoice
      description: Issue one invoice for a customer.
      input: { from: { input: orders } }
    - name: create_invoice
      description: Issue every invoice of a period.
      input: { from: { input: orders } }`)), RuleMCPDuplicateToolName)
}

// TestAToolWithoutADescriptionIsPublishedToNobodysBenefit holds the first of the six
// bullets: name, description and input are the three the language requires.
func TestAToolWithoutADescriptionIsPublishedToNobodysBenefit(t *testing.T) {
	err := refused(t, published(`    - name: create_invoice
      input: { from: { input: orders } }`))
	if !strings.Contains(err.Error(), "description") {
		t.Fatalf("the refusal does not name the description: %v", err)
	}
	refused(t, published(`    - description: Issue one invoice.
      input: { from: { input: orders } }`))
	refused(t, published(`    - name: create_invoice
      description: Issue one invoice.`))
}

// TestASyncCallIsCappedAtTwoMinutes holds the ceiling and the form it is written in: "the
// cap is on the value and not on the unit, so 120s, 2m and 120000ms are the longest forms
// it accepts and an hour cannot be written at all".
func TestASyncCallIsCappedAtTwoMinutes(t *testing.T) {
	for _, timeout := range []string{"120s", "2m", "120000ms"} {
		if _, err := Parse([]byte(published(`    - name: create_invoice
      description: Issue one invoice.
      input: { from: { input: orders } }
      mode: sync
      timeout: ` + timeout))); err != nil {
			t.Errorf("a sync tool waiting %s was refused: %v", timeout, err)
		}
	}
	for _, timeout := range []string{"121s", "3m", "1h"} {
		_, err := Parse([]byte(published(`    - name: create_invoice
      description: Issue one invoice.
      input: { from: { input: orders } }
      mode: sync
      timeout: ` + timeout)))
		held(t, err, RuleMCPSyncTimeoutAboveCeiling)
	}
}

// TestATimeoutOnAnAsyncToolMeansNothing holds the bullet: an async call "returns the run
// identifier at once", so there is nothing for a timeout to bound.
func TestATimeoutOnAnAsyncToolMeansNothing(t *testing.T) {
	_, err := Parse([]byte(published(`    - name: reconcile_month
      description: Reconcile a whole month.
      input: { from: { input: orders } }
      mode: async
      timeout: 60s`)))
	held(t, err, RuleMCPTimeoutOnAsyncTool)

	if _, err := Parse([]byte(published(`    - name: reconcile_month
      description: Reconcile a whole month.
      input: { from: { input: orders } }
      mode: async`))); err != nil {
		t.Fatalf("an async tool with no timeout was refused: %v", err)
	}
}

// TestTheHintsArePassedThroughUnchanged holds what an annotation is and is not: "they are
// hints to a client, never a permission", and a hint the file does not write is not a hint
// written false.
func TestTheHintsArePassedThroughUnchanged(t *testing.T) {
	wf := parsed(t, published(`    - name: create_invoice
      title: Create an invoice
      description: Issue one invoice.
      input: { from: { input: orders } }
      output: { from: { output: invoices } }
      annotations:
        readOnlyHint: false
        idempotentHint: true`))

	tool := wf.MCP.Tools[0]
	if tool.Title != "Create an invoice" {
		t.Errorf("the title was read as %q", tool.Title)
	}
	if tool.Annotations.ReadOnlyHint == nil || *tool.Annotations.ReadOnlyHint {
		t.Error("readOnlyHint: false was not read as written")
	}
	if tool.Annotations.IdempotentHint == nil || !*tool.Annotations.IdempotentHint {
		t.Error("idempotentHint: true was not read as written")
	}
	if tool.Annotations.DestructiveHint != nil || tool.Annotations.OpenWorldHint != nil {
		t.Error("a hint the file never wrote was read as written")
	}
	if err := Check(wf); err != nil {
		t.Fatalf("a published surface that holds together was refused: %v", err)
	}
}
