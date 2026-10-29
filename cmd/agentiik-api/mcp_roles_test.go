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
// "so a principal who can write nowhere never sees workflow.commit"; and a tool they are not offered
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
	anyone := "workflow.language workflow.schema namespace.list namespace.get"
	everybody := anyone + " namespace.create"
	collections := "collection.list collection.get collection.create collection.update collection.delete collection.remove"
	offered := map[string]string{
		// workflow:read and run:read on finance.
		"victor": everybody + " workflow.validate secret.list workflow.list workflow.get run.list run.get run.logs " + collections,
		// workflow:run and run:read on payroll alone: no reading its tree, and so no validating a
		// draft of it either.
		"oscar": everybody + " workflow.list workflow.run run.list run.get run.logs run.cancel run.replay " + collections + " collection.add",
		// An editor's columns on one workflow: no workflow made, and no secret, which are a
		// namespace's.
		"walter": everybody + " workflow.validate workflow.commit workflow.list workflow.get workflow.update workflow.run run.list run.get run.logs run.output run.cancel run.replay " + collections + " collection.add",
		// The owner's every column on the namespace, but nothing an administrator hands out.
		"alice": everybody + " workflow.validate workflow.commit namespace.update namespace.delete grant.list grant.create grant.revoke secret.list secret.declare secret.remove workflow.list workflow.get workflow.create workflow.update workflow.delete workflow.run run.list run.get run.logs run.output run.cancel run.replay " + collections + " collection.add",
		// The installation's, and nothing of any namespace's workflows.
		"carol": everybody + " namespace.update namespace.delete namespace.quotas grant.list grant.create grant.revoke " + collections + " runner.list",
		// oscar's grant, as a service account: no namespace made, and collections of its own.
		"finance/nightly": anyone + " workflow.list workflow.run run.list run.get run.logs run.cancel run.replay " + collections + " collection.add",
		// An owner's columns on hr through the scope, and neither a namespace nor a collection.
		"mallory's token for hr": anyone + " workflow.validate workflow.commit namespace.update namespace.delete grant.list grant.create grant.revoke secret.list secret.declare secret.remove workflow.list workflow.get workflow.create workflow.update workflow.delete workflow.run run.list run.get run.logs run.output run.cancel run.replay",
		// An administrator owning every namespace while it lasts, naming the owner of one it
		// creates, and owning no collection.
		"the bootstrap token": everybody + " workflow.validate workflow.commit namespace.update namespace.delete namespace.quotas grant.list grant.create grant.revoke secret.list secret.declare secret.remove workflow.list workflow.get workflow.create workflow.update workflow.delete workflow.run run.list run.get run.logs run.output run.cancel run.replay runner.list",
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
