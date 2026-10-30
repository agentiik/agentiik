package main

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
)

// "Test the tool set per role (viewer, operator, editor, owner, administrator), recording what each
// is offered and refused." victor views finance, oscar operates finance/payroll, walter edits
// finance/monthly-invoicing, alice owns finance and carol administers the installation holding no
// grant. Each is offered what the routes behind the tools let them use somewhere and nothing else,
// "so a principal who can write nowhere never sees workflow_commit"; and a tool they are not offered
// is no tool of theirs to call, answered as one the server does not have.
//
// And the credentials a route refuses for what they are rather than for what they hold, which no
// grant changes: finance/nightly, a service account operating payroll as oscar does, and mallory's
// token narrowed to hr, which she owns, create no namespace, which "is a person's"; the narrowed
// token reaches no collection, nor does the bootstrap token, which is nobody and owns none.
func TestEachRoleIsOfferedTheToolsItMayUse(t *testing.T) {
	x := someTenants(t)
	x.must("POST", "/api/v1/finance/workflows/payroll/grants", x.as["alice"], api.GrantRequest{Principal: "finance/nightly", Role: "operator"}, http.StatusCreated)
	x.as["the bootstrap token"] = asker{bearer: theToken}
	anyone := "workflow_language workflow_schema namespace_list namespace_get"
	everybody := anyone + " namespace_create"
	collections := "collection_list collection_get collection_create collection_update collection_delete collection_remove"
	offered := map[string]string{
		// workflow:read and run:read on finance.
		"victor": everybody + " workflow_validate secret_list workflow_list workflow_get run_list run_get run_logs " + collections,
		// workflow:run and run:read on payroll alone: no reading its tree, and so no validating a
		// draft of it either.
		"oscar": everybody + " workflow_list workflow_run run_list run_get run_logs run_cancel run_replay " + collections + " collection_add",
		// An editor's columns on one workflow: no workflow made, and no secret, which are a
		// namespace's.
		"walter": everybody + " workflow_validate workflow_commit workflow_list workflow_get workflow_update workflow_pin workflow_run run_list run_get run_logs run_output run_artifact run_cancel run_replay " + collections + " collection_add",
		// The owner's every column on the namespace, but nothing an administrator hands out.
		"alice": everybody + " workflow_validate workflow_commit namespace_update namespace_delete grant_list grant_create grant_revoke secret_list secret_declare secret_remove workflow_list workflow_get workflow_create workflow_update workflow_pin workflow_delete workflow_run run_list run_get run_logs run_output run_artifact run_cancel run_replay " + collections + " collection_add",
		// The installation's, and nothing of any namespace's workflows.
		"carol": everybody + " namespace_update namespace_delete namespace_quotas grant_list grant_create grant_revoke " + collections + " runner_list",
		// oscar's grant, as a service account: no namespace made, and collections of its own.
		"finance/nightly": anyone + " workflow_list workflow_run run_list run_get run_logs run_cancel run_replay " + collections + " collection_add",
		// An owner's columns on hr through the scope, and neither a namespace nor a collection.
		"mallory's token for hr": anyone + " workflow_validate workflow_commit namespace_update namespace_delete grant_list grant_create grant_revoke secret_list secret_declare secret_remove workflow_list workflow_get workflow_create workflow_update workflow_pin workflow_delete workflow_run run_list run_get run_logs run_output run_artifact run_cancel run_replay",
		// An administrator owning every namespace while it lasts, naming the owner of one it
		// creates, and owning no collection.
		"the bootstrap token": everybody + " workflow_validate workflow_commit namespace_update namespace_delete namespace_quotas grant_list grant_create grant_revoke secret_list secret_declare secret_remove workflow_list workflow_get workflow_create workflow_update workflow_pin workflow_delete workflow_run run_list run_get run_logs run_output run_artifact run_cancel run_replay runner_list",
	}

	var every []string
	got := map[string][]string{}
	for who := range offered {
		res, err := x.mcpAsked(x.as[who], "tools/list", nil)
		if err != nil {
			t.Fatalf("tools/list as %s answered %v", who, err)
		}
		for _, tool := range res["tools"].([]any) {
			name := tool.(map[string]any)["name"].(string)
			got[who] = append(got[who], name)
			if !slices.Contains(every, name) {
				every = append(every, name)
			}
		}
	}
	for who, want := range offered {
		w := strings.Fields(want)
		slices.Sort(w)
		g := slices.Sorted(slices.Values(got[who]))
		if !slices.Equal(g, w) {
			t.Errorf("%s is offered %v, want %v", who, g, w)
		}
		for _, name := range every {
			if slices.Contains(g, name) {
				continue
			}
			_, err := x.mcpAsked(x.as[who], "tools/call", map[string]any{"name": name, "arguments": map[string]any{}})
			if err == nil || err.Code != -32602 {
				t.Errorf("%s called %s, which they are not offered, and was answered %v", who, name, err)
			}
		}
	}
}
