package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// The vars a run on a server reads: "the workflow's own, and the namespace's shown to it, the file's
// value winning a name both write", read as the run kept them when it was created.

// varsWorkflow is the workflow these tests decide, writing two variables of its own.
const varsWorkflow = theWorkflow + `vars:
  currency: EUR
  dunning_days: 30
`

// storedVars are the vars the run's stored document starts the evaluator with, every number as it
// was written.
func storedVars(t *testing.T, super string, run agk.RunID) map[string]any {
	t.Helper()
	var stored []byte
	if err := dbtest.Superuser(t, super).QueryRow(t.Context(),
		`select evaluation from runs where id = $1`, string(run)).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	d := json.NewDecoder(bytes.NewReader(stored))
	d.UseNumber()
	var doc Document
	if err := d.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.State == nil {
		t.Fatal("the run's document holds no state")
	}
	return doc.State.Vars
}

// The first pass starts the evaluator with the file's vars laid over the namespace's the run kept, a
// name both write taking the file's value, and the document carries them from then on: a variable
// changed afterwards changes nothing the run reads, on this controller or on one taking it over.
func TestARunReadsItsFileVarsOverTheNamespaceVariablesItKept(t *testing.T) {
	core, _, pool, super := decidingOn(t, varsWorkflow)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.CreateRun(ctx, db.NewRun{
			ID: decidedRun, Workflow: "monthly-invoicing", Commit: "a3f9c1e",
			Trigger: agk.TriggerManual, TriggeredBy: "alice",
			Inputs: json.RawMessage(`{"orders": [{"customer_id": "C-1042"}]}`),
			Steps:  []agk.Step{"normalize", "archive"},
			// currency is one the path that creates a run leaves out, since the file writes it,
			// kept here all the same, so that the evaluator is seen to lay the file over it.
			NamespaceVars: map[string]any{
				"currency": "CHF", "ledger_url": "https://ledger.example.com/api", "rate": json.Number("1.5"),
			},
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"currency": "EUR", "dunning_days": json.Number("30"),
		"ledger_url": "https://ledger.example.com/api", "rate": json.Number("1.5"),
	}
	if got := storedVars(t, super, decidedRun); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the run reads the vars %v, want %v", got, want)
	}

	// What the run kept is changed behind it, as nothing does, and a controller taking it over
	// reads the document, never the column.
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(),
		`update runs set namespace_vars = '{"ledger_url": "https://elsewhere.example.com"}' where id = $1`, string(decidedRun)); err != nil {
		t.Fatal(err)
	}
	other, _, _, _ := resumeOn(t, pool, super, core)
	if err := other.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	if got := storedVars(t, super, decidedRun); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the run read by a controller taking it over reads the vars %v, want %v", got, want)
	}
}

// A run that kept no namespace variable, every run from before v0.6.0 among them, reads the file's
// own, as agk run --local does.
func TestARunThatKeptNoVariableReadsTheFilesOwn(t *testing.T) {
	core, _, pool, super := decidingOn(t, varsWorkflow)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"currency": "EUR", "dunning_days": json.Number("30")}
	if got := storedVars(t, super, decidedRun); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the run reads the vars %v, want %v", got, want)
	}
}
