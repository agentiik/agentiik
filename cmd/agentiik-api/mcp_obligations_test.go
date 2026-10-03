package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/mcp"
)

// "Test the four obligations (inputs validated, access controlled, invocations rate limited, outputs
// sanitised) on the user's server and on a collection", and "Record MCP commits, grants, secret
// declarations, run creations and cancellations in the audit log beside API ones, naming principal
// and tool." Outputs sanitised is the driver's, which masks an envelope before it is stored and a log
// before it is shipped, and is held where a result is read (api's collection tests): what a tool
// answers is what was stored, never anything a container wrote that the driver did not see.
func TestTheObligationsHoldOnBothServers(t *testing.T) {
	x := someTenants(t)
	push := aPushOf(t, payrollPublished)
	push.Parent = theCommit
	x.must("PUT", "/api/v1/finance/workflows/payroll/versions/"+payrollCommit, x.as["alice"], push, http.StatusOK)
	alice := x.as["alice"]
	call := func(as asker, path, name string, arguments map[string]any) (map[string]any, *mcp.Error) {
		t.Helper()
		return x.mcpAskedAt(path, as, "tools/call", map[string]any{"name": name, "arguments": arguments})
	}
	made, _ := call(alice, "/mcp", "collection.create", map[string]any{"name": "payroll-desk"})
	collection := "/mcp/collections/" + made["structuredContent"].(map[string]any)["id"].(string)
	if res, err := call(alice, "/mcp", "collection.add", map[string]any{"collection": made["structuredContent"].(map[string]any)["id"], "namespace": "finance", "workflow": "payroll"}); err != nil || res["isError"] == true {
		t.Fatalf("collection.add answered %v %v", res, err)
	}
	runs := func() int {
		t.Helper()
		listed, _ := x.answer(x.must("GET", "/api/v1/runs?namespace=finance&limit=200", alice, nil, http.StatusOK))["runs"].([]any)
		return len(listed)
	}

	// Inputs validated: an argument off its schema is the protocol's error, and no run exists.
	before := runs()
	if _, err := call(alice, "/mcp", "workflow.run", map[string]any{"namespace": "finance", "workflow": "payroll", "inputs": "every order"}); err == nil || err.Code != mcp.CodeInvalidParams {
		t.Errorf("workflow.run with inputs that are no object answered %v", err)
	}
	if _, err := call(alice, collection, "run_payroll", map[string]any{"orders": "every order"}); err == nil || err.Code != mcp.CodeInvalidParams {
		t.Errorf("run_payroll with orders that are no array answered %v", err)
	}
	if after := runs(); after != before {
		t.Errorf("arguments refused started %d runs", after-before)
	}

	// Access controlled: a tool somebody may not use is absent, and a collection nobody else's.
	if res, err := call(x.as["victor"], "/mcp", "workflow.run", map[string]any{"namespace": "finance", "workflow": "payroll"}); err == nil {
		t.Errorf("a viewer called workflow.run and was answered %v", res)
	}
	if w := x.ask(t.Context(), "POST", collection, x.as["mallory"], `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`); w.Code != http.StatusNotFound {
		t.Errorf("somebody else's request to the collection answered %d", w.Code)
	}
	if after := runs(); after != before {
		t.Errorf("refused callers started %d runs", after-before)
	}

	// The arguments right, the collection's tool starts a run.
	if res, err := call(alice, collection, "run_payroll", map[string]any{"orders": []any{}}); err != nil || res["isError"] == true {
		t.Fatalf("run_payroll answered %v %v", res, err)
	}
	before = runs()

	// Rate limited: one more run fits in the hour, which the user's server starts, and then neither
	// server starts another, each saying when one more fits.
	if res, err := call(x.as["carol"], "/mcp", "namespace.quotas", map[string]any{"namespace": "finance", "quotas": map[string]any{"max_runs_per_hour": before + 1}}); err != nil || res["isError"] == true {
		t.Fatalf("namespace.quotas answered %v %v", res, err)
	}
	started, err := call(alice, "/mcp", "workflow.run", map[string]any{"namespace": "finance", "workflow": "payroll", "inputs": map[string]any{"orders": []any{}}})
	if err != nil || started["isError"] == true {
		t.Fatalf("the run that fits answered %v %v", started, err)
	}
	run, _ := started["structuredContent"].(map[string]any)["run"].(string)
	for name, at := range map[string]string{"workflow.run": "/mcp", "run_payroll": collection} {
		arguments := map[string]any{"orders": []any{}}
		if name == "workflow.run" {
			arguments = map[string]any{"namespace": "finance", "workflow": "payroll", "inputs": arguments}
		}
		res, err := call(alice, at, name, arguments)
		seconds, _ := res["structuredContent"].(map[string]any)["retry_after_seconds"].(float64)
		if err != nil || res["isError"] != true || seconds <= 0 {
			t.Errorf("%s past max_runs_per_hour answered %v %v", name, res, err)
		}
	}
	if after := runs(); after != before+1 {
		t.Errorf("%d runs were started past the quota", after-before-1)
	}

	// Recorded, naming the principal and the tool: a grant, a secret declaration, a run started and
	// one cancelled, each as an API route's act records it.
	for name, arguments := range map[string]map[string]any{
		"grant.create":   {"namespace": "finance", "workflow": "payroll", "principal": "victor", "role": "operator"},
		"secret.declare": {"namespace": "finance", "name": "ledger-copy", "provider": "env", "path": ledgerVariable},
	} {
		if res, err := call(alice, "/mcp", name, arguments); err != nil || res["isError"] == true {
			t.Fatalf("%s answered %v %v", name, res, err)
		}
	}
	if res, err := call(alice, "/mcp", "run.cancel", map[string]any{"run": run}); err != nil || res["isError"] == true {
		t.Fatalf("run.cancel answered %v %v", res, err)
	}
	var entries []audit.Entry
	if err := x.in.pool.Installation(t.Context(), db.AuditLog, func(ctx context.Context, w *db.Wide) error {
		var err error
		entries, err = w.AuditEntries(ctx, 0, 10000)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	marked := map[string]bool{}
	for _, e := range entries {
		var d map[string]any
		if err := json.Unmarshal([]byte(e.Detail), &d); err != nil {
			t.Fatal(err)
		}
		if tool, ok := d["tool"].(string); ok && d["through"] == "mcp" {
			marked[e.Actor+" "+e.Action+" "+tool] = true
		}
	}
	for _, want := range []string{
		"alice grant.create grant.create", "alice secret.write secret.declare", "alice run.trigger workflow.run",
		"alice run.cancel run.cancel", "alice run.trigger run_payroll",
	} {
		if !marked[want] {
			t.Errorf("the audit log records no %q through MCP: %v", want, marked)
		}
	}
}
