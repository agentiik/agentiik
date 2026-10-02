package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
)

// A namespace's workflows are listed to whoever reads the runs of one, each with its newest run, and
// nothing of its repository or its file: oscar, an operator of one workflow, finds that one and no
// other; alice, who reads the namespace's runs, finds both, the one never run among them; and
// nobody, who reads nothing there, is answered an empty list, as for a namespace that does not exist.
func TestANamespacesWorkflowsAreListedToWhoeverReadsTheirRuns(t *testing.T) {
	finance := api.Target{Namespace: "finance"}
	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	h, _, _ := servingTo(t, granted{
		"alice": {{api.WorkflowWrite, finance}, {api.WorkflowRead, finance}, {api.WorkflowRun, finance}, {api.RunRead, finance}},
		"oscar": {{api.WorkflowRun, invoicing}, {api.RunRead, invoicing}},
	})
	if w, _ := call(t, h, "PUT", pushTo, "alice", declaringPush(t, declaringWorkflow, map[string]string{"schemas/order.json": orderSchema})); w.Code != http.StatusOK {
		t.Fatalf("the push answered %d: %s", w.Code, w.Body)
	}
	if w := sent(t, h, "POST", "/api/v1/finance/workflows", "alice", `{"name":"ledger-export"}`); w.Code != http.StatusCreated {
		t.Fatalf("creating a workflow answered %d: %s", w.Code, w.Body)
	}
	w := sent(t, h, "POST", startAt, "alice", `{"commit":"`+aCommit+`","inputs":{"orders":[]}}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("starting a run answered %d: %s", w.Code, w.Body)
	}
	var started struct{ Run string }
	json.Unmarshal(w.Body.Bytes(), &started)

	type listed struct {
		Name      string
		CreatedAt string `json:"created_at"`
		Latest    *struct {
			Run         string
			State       string
			TriggerKind string `json:"trigger_kind"`
			CreatedAt   string `json:"created_at"`
		}
	}
	list := func(who string) []listed {
		t.Helper()
		w := sent(t, h, "GET", "/api/v1/finance/workflows", who, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s listing the workflows was answered %d: %s", who, w.Code, w.Body)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("the listing is answered with Cache-Control %q", got)
		}
		for _, inside := range []string{"default_branch", "head", "clone_url", "graph", "normalize"} {
			if strings.Contains(w.Body.String(), inside) {
				t.Errorf("the listing answers %q, which is reading the workflow: %s", inside, w.Body)
			}
		}
		var out struct{ Workflows []listed }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Workflows
	}

	all := list("alice")
	if len(all) != 2 || all[0].Name != "ledger-export" || all[1].Name != "monthly-invoicing" {
		t.Fatalf("alice is listed %+v", all)
	}
	if all[0].Latest != nil || all[0].CreatedAt == "" {
		t.Errorf("a workflow never run is listed as %+v", all[0])
	}
	if l := all[1].Latest; l == nil || l.Run != started.Run || l.State != "queued" || l.TriggerKind != "manual" || l.CreatedAt == "" {
		t.Errorf("the newest run of monthly-invoicing is listed as %+v", all[1].Latest)
	}

	if mine := list("oscar"); len(mine) != 1 || mine[0].Name != "monthly-invoicing" || mine[0].Latest == nil {
		t.Errorf("the operator of one workflow is listed %+v", mine)
	}
	if none := list("nobody"); len(none) != 0 {
		t.Errorf("someone reading nothing in the namespace is listed %+v", none)
	}
	if w := sent(t, h, "GET", "/api/v1/nowhere/workflows", "alice", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"workflows":[]`) {
		t.Errorf("a namespace that does not exist was answered %d: %s", w.Code, w.Body)
	}
}
