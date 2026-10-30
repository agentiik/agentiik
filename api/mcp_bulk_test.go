package api_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/mcp"
)

// A call of many items: workflow_run with inputs_each and run_cancel with runs, each item made as its
// own request of the route, as the caller, and answered in order.

// someOrders are inputs monthly-invoicing takes; noOrders are inputs its schema refuses.
var (
	someOrders = map[string]any{"orders": []any{map[string]any{"order": "ORD-0001"}}}
	noOrders   = map[string]any{"orders": "every order"}
)

// bulkCall calls tool on the user's server as alice, and answers its result and its structured
// content, failing the test on a protocol error.
func bulkCall(t *testing.T, c *collected, tool string, args map[string]any) (map[string]any, map[string]any) {
	t.Helper()
	_, res, rpcErr := called(t, c.rt, "alice", "tools/call", map[string]any{"name": tool, "arguments": args})
	if rpcErr != nil {
		t.Fatalf("%s answered %v", tool, rpcErr)
	}
	structured, _ := res["structuredContent"].(map[string]any)
	return res, structured
}

// statuses are the status of each item a bulk call answered, in the order it answered them.
func statuses(structured map[string]any) []int {
	var out []int
	for _, r := range structured["results"].([]any) {
		out = append(out, int(r.(map[string]any)["status"].(float64)))
	}
	return out
}

// auditedThrough are the entries of action the log records through the tool, as their targets.
func auditedThrough(t *testing.T, c *collected, action, tool string) []string {
	t.Helper()
	var out []string
	for _, e := range audited(t, c.pool) {
		if d := detailOf(t, e); e.Action == action && d["through"] == "mcp" && d["tool"] == tool && e.Actor == "alice" {
			out = append(out, fmt.Sprintf("%s %s", e.Target, e.Result))
		}
	}
	return out
}

// One run per entry of inputs_each, at the one ref, each its own request: an entry its schema
// refuses is refused alone, with the route's own words, between two that start, each started run
// recorded in the log as a call of workflow_run records it, and the call no error, since runs started.
func TestInputsEachStartsOneRunPerEntry(t *testing.T) {
	c := servingCollections(t)
	res, structured := bulkCall(t, c, "workflow_run", map[string]any{
		"namespace": "finance", "workflow": "monthly-invoicing", "ref": "main",
		"inputs_each": []any{someOrders, noOrders, someOrders},
	})
	if res["isError"] == true || !slices.Equal(statuses(structured), []int{202, 422, 202}) {
		t.Fatalf("inputs_each answered %v", res)
	}
	if structured["succeeded"] != 2.0 || structured["refused"] != 1.0 || structured["not_attempted"] != 0.0 {
		t.Errorf("inputs_each counts %v", structured)
	}
	results := structured["results"].([]any)
	var started []string
	for i, r := range results {
		r := r.(map[string]any)
		if r["item"] != float64(i) {
			t.Errorf("result %d names item %v", i, r["item"])
		}
		if r["status"] == 202.0 {
			if r["state"] != "queued" || r["commit"] != c.head("monthly-invoicing") {
				t.Errorf("a run started is answered %v", r)
			}
			started = append(started, r["run"].(string))
		}
	}
	if refused := results[1].(map[string]any); !strings.Contains(fmt.Sprint(refused["error"]), "orders") || refused["input"] != "orders" || refused["run"] != nil {
		t.Errorf("the entry its schema refuses is answered %v", refused)
	}
	text := textOf(res)
	if strings.Contains(text, "workflow_language") {
		t.Errorf("an input refused is answered as a problem of the draft: %q", text)
	}
	for _, line := range []string{"Of 3, 2 started and 1 refused.", "inputs_each[0]: started run " + started[0], "inputs_each[1]: refused, 422: ", "inputs_each[2]: started run " + started[1]} {
		if !strings.Contains(text, line) {
			t.Errorf("inputs_each says %q, and not %q", text, line)
		}
	}
	if runs := c.runs(); len(runs) != 2 || runs[0].TriggeredBy != "alice" || runs[0].Trigger.String() != "manual" {
		t.Errorf("inputs_each started %+v", runs)
	}
	if got := auditedThrough(t, c, audit.RunTrigger, "workflow_run"); !slices.Equal(got, []string{started[0] + " done", started[1] + " done"}) {
		t.Errorf("the log records the starts %v", got)
	}
}

// Each run of runs is cancelled by its own request: a run that is not there, or not the caller's to
// cancel, is refused alone and named, a run asked twice is asked twice, as two calls would, and each
// request is recorded in the log as run_cancel records it.
func TestRunsAreCancelledOneRequestEach(t *testing.T) {
	c := servingCollections(t)
	var runs []string
	for range 2 {
		status, started := c.ask("POST", "/api/v1/finance/workflows/monthly-invoicing/runs", "alice", map[string]any{"inputs": someOrders})
		if status != http.StatusAccepted {
			t.Fatalf("a run could not be started: %d %v", status, started)
		}
		runs = append(runs, started["run"].(string))
	}
	nowhere := "01M2ZZZZZZZZZZZZZZZZZZZZZZ"
	res, structured := bulkCall(t, c, "run_cancel", map[string]any{"runs": []any{runs[0], nowhere, runs[1], runs[0]}})
	if res["isError"] == true || !slices.Equal(statuses(structured), []int{202, 404, 202, 202}) {
		t.Fatalf("runs answered %v", res)
	}
	for i, want := range []string{runs[0], nowhere, runs[1], runs[0]} {
		if got := structured["results"].([]any)[i].(map[string]any)["run"]; got != want {
			t.Errorf("result %d names the run %v, where it was asked about %s", i, got, want)
		}
	}
	text := textOf(res)
	for _, line := range []string{"Of 4, 3 asked to cancel and 1 refused.", "runs[0] " + runs[0] + ": asked to cancel", "runs[1] " + nowhere + ": refused, 404: no such thing, or not yours"} {
		if !strings.Contains(text, line) {
			t.Errorf("runs says %q, and not %q", text, line)
		}
	}
	if got := auditedThrough(t, c, audit.RunCancel, "run_cancel"); !slices.Equal(got, []string{runs[0] + " done", runs[1] + " done", runs[0] + " unchanged"}) {
		t.Errorf("the log records the cancellations %v", got)
	}

	// Every one refused: the call is an error, since nothing it asked for was done.
	if res, structured := bulkCall(t, c, "run_cancel", map[string]any{"runs": []any{nowhere, nowhere}}); res["isError"] != true || structured["succeeded"] != 0.0 {
		t.Errorf("runs none of which are there answered %v", res)
	}
}

// A list holds 1 to 100 items, and a call gives the one form or the other: anything else is the
// protocol's error, and asks nothing of the API.
func TestABulkCallIsBounded(t *testing.T) {
	c := servingCollections(t)
	hundredAndOne := func(item any) []any {
		out := make([]any, 101)
		for i := range out {
			out[i] = item
		}
		return out
	}
	for name, call := range map[string]map[string]any{
		"101 runs":               {"name": "run_cancel", "arguments": map[string]any{"runs": hundredAndOne("01M2ZZZZZZZZZZZZZZZZZZZZZZ")}},
		"no runs":                {"name": "run_cancel", "arguments": map[string]any{"runs": []any{}}},
		"run and runs":           {"name": "run_cancel", "arguments": map[string]any{"run": "01M2ZZZZZZZZZZZZZZZZZZZZZZ", "runs": []any{"01M2ZZZZZZZZZZZZZZZZZZZZZZ"}}},
		"neither run nor runs":   {"name": "run_cancel", "arguments": map[string]any{}},
		"101 entries":            {"name": "workflow_run", "arguments": map[string]any{"namespace": "finance", "workflow": "monthly-invoicing", "inputs_each": hundredAndOne(someOrders)}},
		"inputs and inputs_each": {"name": "workflow_run", "arguments": map[string]any{"namespace": "finance", "workflow": "monthly-invoicing", "inputs": someOrders, "inputs_each": []any{someOrders}}},
	} {
		if _, res, rpcErr := called(t, c.rt, "alice", "tools/call", call); rpcErr == nil || rpcErr.Code != mcp.CodeInvalidParams {
			t.Errorf("%s answered %v %v", name, res, rpcErr)
		}
	}
	if runs := c.runs(); len(runs) != 0 {
		t.Errorf("calls refused started %d runs", len(runs))
	}

	// A hundred is let through.
	hundred := hundredAndOne(someOrders)[:100]
	if res, structured := bulkCall(t, c, "workflow_run", map[string]any{"namespace": "finance", "workflow": "monthly-invoicing", "inputs_each": hundred}); res["isError"] == true || structured["succeeded"] != 100.0 {
		t.Errorf("100 entries answered %v", structured)
	}
}

// A quota reached stops the starts there: the run it refused is answered as the route answers it,
// with retry_after_seconds, and the entries after it are not attempted, since the quota would refuse
// each of them for the same reason, which the answer says with how many and when to call again.
func TestAQuotaReachedStopsTheStarts(t *testing.T) {
	c := servingCollections(t)
	if _, err := dbtest.Superuser(t, c.super).Exec(t.Context(), `update namespaces set max_runs_per_hour = 2 where name = 'finance'`); err != nil {
		t.Fatal(err)
	}
	res, structured := bulkCall(t, c, "workflow_run", map[string]any{
		"namespace": "finance", "workflow": "monthly-invoicing",
		"inputs_each": []any{someOrders, someOrders, someOrders, someOrders, someOrders},
	})
	if res["isError"] == true || !slices.Equal(statuses(structured), []int{202, 202, 429}) || structured["not_attempted"] != 2.0 {
		t.Fatalf("inputs_each past the quota answered %v", res)
	}
	retry, _ := structured["retry_after_seconds"].(float64)
	refused := structured["results"].([]any)[2].(map[string]any)
	if retry <= 0 || refused["retry_after_seconds"] != retry {
		t.Errorf("the quota's refusal says to retry after %v, its result %v", structured["retry_after_seconds"], refused)
	}
	text := textOf(res)
	for _, line := range []string{"Of 5, 2 started and 1 refused; 2 not attempted.", "inputs_each[2]: refused, 429: ", fmt.Sprintf("call again with them once %d seconds have passed", int(retry))} {
		if !strings.Contains(text, line) {
			t.Errorf("inputs_each past the quota says %q, and not %q", text, line)
		}
	}
	if runs := c.runs(); len(runs) != 2 {
		t.Errorf("inputs_each past the quota started %d runs", len(runs))
	}

	// Refused from its first entry, nothing is started, and the call is an error.
	if res, structured := bulkCall(t, c, "workflow_run", map[string]any{"namespace": "finance", "workflow": "monthly-invoicing", "inputs_each": []any{someOrders, someOrders}}); res["isError"] != true || !slices.Equal(statuses(structured), []int{429}) || structured["not_attempted"] != 1.0 {
		t.Errorf("inputs_each refused at once answered %v", res)
	}
}
