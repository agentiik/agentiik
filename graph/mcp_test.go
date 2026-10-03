package graph

import (
	"strings"
	"testing"
	"testing/fstest"
)

// published writes a workflow whose boundary a tool can be a view of, with the mcp block the
// test is about written into it.
func published(block string) string {
	return `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: invoicing, namespace: finance }
inputs:
  orders:
    schema: { type: object }
    required: true
outputs:
  invoices:
    from: { step: archive, port: out }
mcp:
` + block + `
steps:
  archive:
    image: ghcr.io/acme/agk-archive@sha256:44de908840a673cc25120ecd3e506292d691f72937ce1da04a6ee4252ff4c115
    outputs: [out]
`
}

// TestAWorkflowIsOneTool holds the block's one shape: "one block, one tool, whose arguments are
// the workflow's inputs". It names no input, and its name is the workflow's where none is
// written, so that the smallest tool is a description alone.
func TestAWorkflowIsOneTool(t *testing.T) {
	wf := parsed(t, published(`  description: Issue one invoice per order.`))
	if wf.MCP == nil || wf.MCP.Name != "invoicing" || wf.MCP.Named {
		t.Fatalf("a tool with no name was read as %+v, and its name is the workflow's", wf.MCP)
	}
	if wf.MCP.Mode != ToolSync || wf.MCP.Output != "" {
		t.Fatalf("a tool writing no mode and no output was read as %+v", wf.MCP)
	}
	if err := Check(wf); err != nil {
		t.Fatalf("the smallest tool was refused: %v", err)
	}

	named := parsed(t, published(`  name: create_invoices
  description: Issue one invoice per order.
  output: invoices`))
	if named.MCP.Name != "create_invoices" || !named.MCP.Named || named.MCP.Output != "invoices" {
		t.Fatalf("the tool was read as %+v", named.MCP)
	}

	// Several tools, or an input mapped into one, are the language before v0.7.0 and are
	// refused where a version is made: a connector offering several tools is a collection.
	err := refused(t, published(`  name: invoicing
  tools:
    - name: create_invoice
      description: Issue one invoice.
      input: { from: { input: orders } }`))
	if !strings.Contains(err.Error(), "collection") {
		t.Fatalf("the refusal of a tool list does not say where several tools go: %v", err)
	}
	refused(t, published(`  description: Issue one invoice.
  input: orders`))
}

// TestAToolIsAViewOfTheWorkflowsOwnBoundary holds the rule and its reason: the output a tool
// returns is "the name of one declared workflow output", never a step or a port.
func TestAToolIsAViewOfTheWorkflowsOwnBoundary(t *testing.T) {
	held(t, checked(t, published(`  description: Issue one invoice.
  output: nowhere`)), RuleMCPOutputNotDeclared)

	// An output taken from a step port is refused when the document is read: output is a
	// name, and a name is the workflow's own.
	refused(t, published(`  description: Issue one invoice.
  output: { from: { step: archive, port: out } }`))
}

// TestEveryInputOfAToolCarriesASchema holds the rule and what it is good for: "every input is
// an argument, and a tool has to publish an inputSchema".
func TestEveryInputOfAToolCarriesASchema(t *testing.T) {
	doc := strings.Replace(published(`  description: Reconcile a whole month.`), "outputs:", "  period:\n    required: true\noutputs:", 1)
	err := refused(t, doc)
	if !strings.Contains(err.Error(), "period") {
		t.Fatalf("the refusal does not name the input with no schema: %v", err)
	}

	// The same workflow publishing nothing needs no schema on it.
	if _, err := Parse([]byte(strings.Replace(doc, "mcp:\n  description: Reconcile a whole month.\n", "", 1))); err != nil {
		t.Fatalf("a workflow publishing no tool was held to its rule: %v", err)
	}
}

// TestAToolWithoutADescriptionIsPublishedToNobodysBenefit holds the first of the refusals:
// "a tool published without a description is exactly the tool a model has no way to decide
// to call".
func TestAToolWithoutADescriptionIsPublishedToNobodysBenefit(t *testing.T) {
	err := refused(t, published(`  name: create_invoice`))
	if !strings.Contains(err.Error(), "description") {
		t.Fatalf("the refusal does not name the description: %v", err)
	}
	refused(t, published(`  name: create invoice
  description: Issue one invoice.`))
}

// TestASyncCallIsCappedAtTwoMinutes holds the ceiling and the form it is written in: "the cap
// is on the value and not on the unit, so 120s, 2m and 120000ms are the longest forms it
// accepts and an hour cannot be written at all".
func TestASyncCallIsCappedAtTwoMinutes(t *testing.T) {
	for _, timeout := range []string{"120s", "2m", "120000ms"} {
		if _, err := Parse([]byte(published(`  description: Issue one invoice.
  mode: sync
  timeout: ` + timeout))); err != nil {
			t.Errorf("a sync tool waiting %s was refused: %v", timeout, err)
		}
	}
	for _, timeout := range []string{"121s", "3m", "1h"} {
		_, err := Parse([]byte(published(`  description: Issue one invoice.
  mode: sync
  timeout: ` + timeout)))
		held(t, err, RuleMCPSyncTimeoutAboveCeiling)
	}
}

// TestATimeoutOnAnAsyncToolMeansNothing holds the bullet: an async call "returns the run
// identifier at once", so there is nothing for a timeout to bound.
func TestATimeoutOnAnAsyncToolMeansNothing(t *testing.T) {
	_, err := Parse([]byte(published(`  description: Reconcile a whole month.
  mode: async
  timeout: 60s`)))
	held(t, err, RuleMCPTimeoutOnAsyncTool)

	if _, err := Parse([]byte(published(`  description: Reconcile a whole month.
  mode: async`))); err != nil {
		t.Fatalf("an async tool with no timeout was refused: %v", err)
	}
}

// TestTheHintsArePassedThroughUnchanged holds what an annotation is and is not: "they are hints
// to a client, never a permission", and a hint the file does not write is not a hint written
// false.
func TestTheHintsArePassedThroughUnchanged(t *testing.T) {
	wf := parsed(t, published(`  name: create_invoice
  title: Create an invoice
  description: Issue one invoice.
  output: invoices
  annotations:
    readOnlyHint: false
    idempotentHint: true`))

	tool := wf.MCP
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
	refused(t, published(`  description: Issue one invoice.
  annotations:
    allowed: true`))
}

// TestAVersionStoredWithAToolListPublishesNothing holds the upgrade: a version stored before
// v0.7.0 may carry the list of tools the language had then, which nothing ever served. Read
// back, it keeps building and running, and publishes no tool; where a version is made, the list
// is refused.
func TestAVersionStoredWithAToolListPublishesNothing(t *testing.T) {
	doc := published(`  name: invoicing
  description: The invoicing tools.
  tools:
    - name: create_invoice
      description: Issue one invoice.
      input: { from: { input: orders } }`)
	files := fstest.MapFS{"agentiik.yaml": &fstest.MapFile{Data: []byte(doc)}}
	if _, err := Load(files, "agentiik.yaml", nil); err == nil {
		t.Fatal("a new version listing tools was loaded")
	}
	wf, err := LoadStored(files, "agentiik.yaml", nil)
	if err != nil {
		t.Fatalf("a stored version listing tools was refused when read back: %v", err)
	}
	if wf.MCP != nil {
		t.Fatalf("a stored version listing tools was read as publishing %+v", wf.MCP)
	}
	if err := Check(wf); err != nil {
		t.Fatalf("a stored version listing tools was refused when checked: %v", err)
	}
}
