package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/mcp"
)

// "Prove that operator alone lists and calls a collection's tools but reaches no entry point, no step
// and no endpoint the steps talk to through any accepted request." The documentation's reason: "The
// MCP surface is declared, never inferred: nothing about a tool comes from the graph ... So operator,
// the role without workflow:read, is exactly a client's grant: it lists and calls the tools and cannot
// read the entry point, the steps or the endpoints they talk to."

// payrollPublished is finance/payroll publishing itself as a tool, its step talking to a ledger the
// tool says nothing of.
const payrollPublished = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: payroll, namespace: finance }
inputs:
  orders: { schema: { type: array, description: The month's orders. } }
outputs:
  invoices: { from: { step: normalize, port: ok } }
mcp:
  name: run_payroll
  description: Turns a month's orders into the payroll's invoices.
  output: invoices
  mode: async
steps:
  normalize:
    image: ` + theImage + `
    network: egress
    egress:
      allow: ["ledger.internal.example.com:443"]
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
`

// payrollCommit is the commit payrollPublished is pushed as, after theCommit.
const payrollCommit = "b4a0d2f6e3c9581a0f72d4b9c1e5a8f3b0d6c2e4"

func TestAnOperatorCallsACollectionsToolAndReadsNothingOfTheWorkflow(t *testing.T) {
	x := someTenants(t)
	push := aPushOf(t, payrollPublished)
	push.Parent = theCommit
	x.must("PUT", "/api/v1/finance/workflows/payroll/versions/"+payrollCommit, x.as["alice"], push, http.StatusOK)

	// oscar operates payroll and holds nothing else: he assembles a collection of it with his own
	// server's tools.
	oscar := x.as["oscar"]
	made, err := x.mcpAsked(oscar, "tools/call", map[string]any{"name": "collection.create", "arguments": map[string]any{"name": "payroll-desk"}})
	if err != nil || made["isError"] == true {
		t.Fatalf("collection.create answered %v %v", made, err)
	}
	collection := made["structuredContent"].(map[string]any)
	id := collection["id"].(string)
	added, err := x.mcpAsked(oscar, "tools/call", map[string]any{"name": "collection.add", "arguments": map[string]any{"collection": id, "namespace": "finance", "workflow": "payroll"}})
	if err != nil || added["isError"] == true {
		t.Fatalf("collection.add answered %v %v", added, err)
	}

	// What his client is told at the collection's address: the one tool, declared, and nothing of
	// the graph behind it.
	path := "/mcp/collections/" + id
	listed, err := x.mcpAskedAt(path, oscar, "tools/list", nil)
	tools, _ := listed["tools"].([]any)
	if err != nil || len(tools) != 1 || tools[0].(map[string]any)["name"] != "run_payroll" {
		t.Fatalf("the collection lists %v %v", listed, err)
	}
	said, _ := json.Marshal(listed)
	for _, internal := range []string{"normalize", "agk-invoice", theImage, "ledger.internal.example.com", "egress", "rejected", "agentiik.yaml"} {
		if strings.Contains(string(said), internal) {
			t.Errorf("the tool tells a client %q, which is the workflow's and no tool's: %s", internal, said)
		}
	}

	// And he calls it: a run of his, through the collection.
	called, err := x.mcpAskedAt(path, oscar, "tools/call", map[string]any{"name": "run_payroll", "arguments": map[string]any{"orders": []any{}}})
	if err != nil || called["isError"] == true {
		t.Fatalf("the call answered %v %v", called, err)
	}
	structured, _ := called["structuredContent"].(map[string]any)
	run, _ := structured["run"].(string)
	if run == "" {
		t.Fatalf("an async call answered no run: %v", called)
	}

	// A collection offers tools and nothing else: no resources, no prompts, no tree.
	for _, method := range []string{"resources/list", "resources/templates/list", "prompts/list"} {
		if res, err := x.mcpAskedAt(path, oscar, method, nil); err == nil || err.Code != mcp.CodeMethodNotFound {
			t.Errorf("%s on the collection answered %v %v", method, res, err)
		}
	}
	if res, err := x.mcpAskedAt(path, oscar, "resources/read", map[string]any{"uri": "agentiik://finance/payroll/tree/main/agentiik.yaml"}); err == nil || err.Code != mcp.CodeMethodNotFound {
		t.Errorf("resources/read on the collection answered %v %v", res, err)
	}

	// Nor does anything else he may ask reach the entry point: not his own server's tools, not its
	// resources, not the API and not git. Each is refused as a workflow he cannot see is.
	if res, err := x.mcpAsked(oscar, "tools/call", map[string]any{"name": "workflow.get", "arguments": map[string]any{"namespace": "finance", "workflow": "payroll"}}); err == nil {
		t.Errorf("workflow.get is offered to an operator and answered %v", res)
	}
	if res, err := x.mcpAsked(oscar, "tools/call", map[string]any{"name": "workflow.validate", "arguments": map[string]any{"namespace": "finance", "workflow": "payroll", "files": map[string]any{}}}); err == nil {
		t.Errorf("workflow.validate is offered to an operator and answered %v", res)
	}
	if _, err := x.mcpAsked(oscar, "resources/read", map[string]any{"uri": "agentiik://finance/payroll/tree/main/agentiik.yaml"}); err == nil {
		t.Error("an operator read the entry point as a resource")
	}
	for _, p := range []string{
		"/api/v1/finance/workflows/payroll",
		"/api/v1/finance/workflows/payroll/tree/main",
		"/api/v1/finance/workflows/payroll/tree/main?path=agentiik.yaml",
		"/api/v1/finance/workflows/payroll/versions/" + payrollCommit,
		"/finance/payroll.git/info/refs?service=git-upload-pack",
	} {
		w := x.ask(t.Context(), "GET", p, oscar, nil)
		if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "ledger.internal.example.com") || strings.Contains(w.Body.String(), theImage) {
			t.Errorf("an operator asking %s was answered %d %.300s", p, w.Code, w.Body)
		}
	}

	// What he reads of the run he started is the run's, as run:read gives it, "run state, per-step
	// state, timings and log lines", each step by its name, image and ports as the run view draws
	// them; and nothing of "the queries, endpoints and business rules inside it", which the role
	// "deliberately lacks workflow:read" to keep from him.
	res, err := x.mcpAsked(oscar, "tools/call", map[string]any{"name": "run.get", "arguments": map[string]any{"run": run}})
	if err != nil || res["isError"] == true {
		t.Fatalf("run.get of his own run answered %v %v", res, err)
	}
	read, _ := json.Marshal(res)
	for _, internal := range []string{"ledger.internal.example.com", "egress", "workflow.inputs.orders"} {
		if strings.Contains(string(read), internal) {
			t.Errorf("run.get tells an operator %q: %s", internal, read)
		}
	}
}
