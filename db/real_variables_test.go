package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

// A namespace's variables, against a real PostgreSQL: what is kept, which workflow is shown which,
// what bounds them together, what a rename and a removal do to them, and what a run keeps of them.

func setVariable(t *testing.T, pool *Pool, namespace string, v Variable) (Variable, bool, error) {
	t.Helper()
	var out Variable
	var created bool
	err := pool.In(t.Context(), namespace, func(ctx context.Context, ns *NS) error {
		var err error
		out, created, err = ns.SetVariable(ctx, v)
		return err
	})
	return out, created, err
}

// variable is a variable written as the API writes one, its value measured the way it measures.
func variable(name, value, visibility string, workflows ...string) Variable {
	v := Variable{Name: name, Value: json.RawMessage(value), Bytes: len(value), Values: 1, Visibility: visibility, UpdatedBy: "alice"}
	if visibility == VisibilitySelected {
		v.Workflows = append([]string{}, workflows...)
	}
	return v
}

func shownTo(t *testing.T, pool *Pool, namespace, workflow string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := pool.In(t.Context(), namespace, func(ctx context.Context, ns *NS) error {
		var err error
		out, err = ns.VariablesFor(ctx, workflow)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// A variable is written, read back, replaced and removed, one at a time, and a selected one keeps
// its workflows sorted and each once.
func TestAVariableRoundTrips(t *testing.T) {
	pool, _ := opened(t)

	_, created, err := setVariable(t, pool, "finance", variable("ledger_url", `"https://ledger.example.com/api"`, VisibilityAll))
	if err != nil || !created {
		t.Fatalf("the first write answered created %v: %v", created, err)
	}
	written, created, err := setVariable(t, pool, "finance", variable("reminder_days", `[7, 14, 30]`, VisibilitySelected, "payment-reminders", "monthly-invoicing", "payment-reminders"))
	if err != nil || !created {
		t.Fatalf("the second write answered created %v: %v", created, err)
	}
	if !slices.Equal(written.Workflows, []string{"monthly-invoicing", "payment-reminders"}) {
		t.Errorf("the workflows were kept as %q", written.Workflows)
	}
	if written.UpdatedAt.IsZero() || written.UpdatedAt.Location().String() != "UTC" {
		t.Errorf("the write answered the time %v", written.UpdatedAt)
	}

	var listed []Variable
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		listed, err = ns.Variables(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].Name != "ledger_url" || listed[1].Name != "reminder_days" {
		t.Fatalf("the namespace lists %+v", listed)
	}
	if listed[0].Workflows != nil || listed[1].Visibility != VisibilitySelected || string(listed[1].Value) != "[7, 14, 30]" {
		t.Errorf("the listing reads %+v", listed)
	}

	// Replaced whole: selected for none, which keeps it aside.
	replaced, created, err := setVariable(t, pool, "finance", variable("reminder_days", `45`, VisibilitySelected))
	if err != nil || created {
		t.Fatalf("a replacement answered created %v: %v", created, err)
	}
	if replaced.Workflows == nil || len(replaced.Workflows) != 0 || string(replaced.Value) != "45" {
		t.Errorf("the replacement reads %+v", replaced)
	}
	var one Variable
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		one, err = ns.Variable(ctx, "reminder_days")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if one.Workflows == nil || len(one.Workflows) != 0 {
		t.Errorf("a variable selected for none reads its workflows as %#v", one.Workflows)
	}

	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error { return ns.RemoveVariable(ctx, "reminder_days") }); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func(context.Context, *NS) error{
		func(ctx context.Context, ns *NS) error { _, err := ns.Variable(ctx, "reminder_days"); return err },
		func(ctx context.Context, ns *NS) error { return ns.RemoveVariable(ctx, "reminder_days") },
	} {
		if err := pool.In(t.Context(), "finance", read); !errors.Is(err, ErrNoVariable) {
			t.Errorf("a removed variable answered %v", err)
		}
	}
}

// A workflow is shown every variable read by all and those selected for it by name, whether or not
// a workflow of that name exists yet, and never another namespace's.
func TestAWorkflowIsShownWhatItsVariablesSelect(t *testing.T) {
	pool, _ := opened(t)
	for _, v := range []Variable{
		variable("currency", `"EUR"`, VisibilityAll),
		variable("dunning_days", `30`, VisibilitySelected, "monthly-invoicing"),
		variable("payroll_day", `25`, VisibilitySelected, "payroll"),
		variable("aside", `true`, VisibilitySelected),
	} {
		if _, _, err := setVariable(t, pool, "finance", v); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := setVariable(t, pool, "team-ops", variable("currency", `"CHF"`, VisibilityAll)); err != nil {
		t.Fatal(err)
	}

	for workflow, want := range map[string]map[string]any{
		"monthly-invoicing": {"currency": "EUR", "dunning_days": json.Number("30")},
		// No workflow is called payroll yet, and the one created under that name will read it.
		"payroll": {"currency": "EUR", "payroll_day": json.Number("25")},
		"nightly": {"currency": "EUR"},
	} {
		if got := shownTo(t, pool, "finance", workflow); !maps.Equal(got, want) {
			t.Errorf("finance shows %s %v, want %v", workflow, got, want)
		}
	}
	if got := shownTo(t, pool, "team-ops", "monthly-invoicing"); !maps.Equal(got, map[string]any{"currency": "CHF"}) {
		t.Errorf("team-ops shows %v", got)
	}
	if got := shownTo(t, pool, "team-ops", "nothing-here"); got == nil {
		t.Error("a workflow shown nothing was answered nil, which a caller cannot tell from variables not read")
	}
}

// A namespace's variables are bounded together, by count, by weight and by values, and a variable
// replaced is counted once: the bounds hold whatever order two writers come in.
func TestANamespacesVariablesAreBoundedTogether(t *testing.T) {
	pool, super := opened(t)
	conn := superuser(t, super)
	if _, err := conn.Exec(t.Context(),
		`insert into namespace_variables (namespace, name, value, value_bytes, value_count, visibility, updated_by)
		 select 'finance', 'v' || i, 'true', 4, 1, 'all', 'alice' from generate_series(1, $1) i`, VariablesMax); err != nil {
		t.Fatal(err)
	}
	var full *VariablesFull
	if _, _, err := setVariable(t, pool, "finance", variable("one_more", `1`, VisibilityAll)); !errors.As(err, &full) {
		t.Fatalf("the variable past %d answered %v", VariablesMax, err)
	}
	if _, created, err := setVariable(t, pool, "finance", variable("v1", `false`, VisibilityAll)); err != nil || created {
		t.Fatalf("replacing a variable of a full namespace answered created %v: %v", created, err)
	}
	// As heavy as they may be, 64 values of 64 KiB, one of which is replaced by one as heavy.
	fill := func(rows, bytes, values int) {
		t.Helper()
		if _, err := conn.Exec(t.Context(), `delete from namespace_variables where namespace = 'finance'`); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(t.Context(),
			`insert into namespace_variables (namespace, name, value, value_bytes, value_count, visibility, updated_by)
			 select 'finance', 'v' || i, 'true', $2, $3, 'all', 'alice' from generate_series(1, $1) i`, rows, bytes, values); err != nil {
			t.Fatal(err)
		}
	}
	fill(VariablesMaxBytes/(64<<10), 64<<10, 1)
	if _, _, err := setVariable(t, pool, "finance", variable("one_more", `1`, VisibilityAll)); !errors.As(err, &full) {
		t.Errorf("a variable past the namespace's weight answered %v", err)
	}
	heavy := variable("v1", `"x"`, VisibilityAll)
	heavy.Bytes = 64 << 10
	if _, _, err := setVariable(t, pool, "finance", heavy); err != nil {
		t.Errorf("a variable replaced by one as heavy, in a namespace at its weight, answered %v", err)
	}
	fill(1, 1, VariablesMaxValues-1)
	if _, _, err := setVariable(t, pool, "finance", variable("one_more", `1`, VisibilityAll)); err != nil {
		t.Errorf("a variable taking the namespace to its values answered %v", err)
	}
	if _, _, err := setVariable(t, pool, "finance", variable("two_more", `2`, VisibilityAll)); !errors.As(err, &full) {
		t.Errorf("a variable past the namespace's values answered %v", err)
	}
	// Another namespace counts its own.
	if _, _, err := setVariable(t, pool, "team-ops", variable("one_more", `1`, VisibilityAll)); err != nil {
		t.Errorf("team-ops was refused for finance's variables: %v", err)
	}
}

// A rename carries the workflow's name in every list of its namespace, kept sorted and each once,
// and a namespace's variables go with it when it is removed.
func TestARenameAndARemovalCarryTheVariables(t *testing.T) {
	pool, _ := opened(t)
	for _, v := range []Variable{
		variable("dunning_days", `30`, VisibilitySelected, "monthly-invoicing"),
		variable("both", `1`, VisibilitySelected, "invoicing", "monthly-invoicing", "zeta"),
		variable("other", `2`, VisibilitySelected, "payroll"),
	} {
		if _, _, err := setVariable(t, pool, "finance", v); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.RenameWorkflow(ctx, "monthly-invoicing", "invoicing")
	}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]string{
		"dunning_days": {"invoicing"}, "both": {"invoicing", "zeta"}, "other": {"payroll"},
	} {
		var v Variable
		if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			var err error
			v, err = ns.Variable(ctx, name)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(v.Workflows, want) {
			t.Errorf("after the rename %s names %q, want %q", name, v.Workflows, want)
		}
	}

	if err := pool.Installation(t.Context(), NamespaceAdministration, func(ctx context.Context, w *Wide) error {
		_, err := w.CreateNamespace(ctx, Namespace{Name: "spare", Kind: NamespaceShared})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := setVariable(t, pool, "spare", variable("currency", `"EUR"`, VisibilityAll)); err != nil {
		t.Fatal(err)
	}
	if err := pool.Installation(t.Context(), NamespaceAdministration, func(ctx context.Context, w *Wide) error {
		return w.RemoveNamespace(ctx, "spare")
	}); err != nil {
		t.Fatalf("a namespace holding nothing but a variable could not be removed: %s", err)
	}
	if _, _, err := setVariable(t, pool, "spare", variable("currency", `"EUR"`, VisibilityAll)); !errors.Is(err, ErrNoNamespace) {
		t.Errorf("a variable written into a removed namespace answered %v", err)
	}
}

// A run keeps the variables it read, written once with it, each number as it was written, read for
// its first decision and by a replay; a run that read none keeps nothing, and answers none.
func TestARunKeepsTheVariablesItRead(t *testing.T) {
	pool, _ := opened(t)
	const kept, none agk.RunID = "01M2Z8V1P9C4XQ7K2N4D6F8H0C", "01M2Z8V1P9C4XQ7K2N4D6F8H0D"
	read := map[string]any{"dunning_days": json.Number("30"), "rate": json.Number("1.50"), "hosts": []any{"a", "b"}}
	for run, vars := range map[agk.RunID]map[string]any{kept: read, none: {}} {
		if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			return ns.CreateRun(ctx, NewRun{
				ID: run, Workflow: "monthly-invoicing", Commit: "a3f9c1e", Trigger: agk.TriggerManual, TriggeredBy: "alice",
				Steps: []agk.Step{"normalize"}, NamespaceVars: vars,
			})
		}); err != nil {
			t.Fatal(err)
		}
	}

	var e Evaluation
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		var err error
		e, err = w.Run(ctx, kept)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(e.NamespaceVars) != fmt.Sprint(read) {
		t.Errorf("the controller reads the variables %v, want %v", e.NamespaceVars, read)
	}
	if n, ok := e.NamespaceVars["dunning_days"].(json.Number); !ok || n != "30" {
		t.Errorf("an int was read back as %#v", e.NamespaceVars["dunning_days"])
	}
	if n, ok := e.NamespaceVars["rate"].(json.Number); !ok || n != "1.50" {
		t.Errorf("a double was read back as %#v", e.NamespaceVars["rate"])
	}

	for run, want := range map[agk.RunID]int{kept: 3, none: 0} {
		var got map[string]any
		if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			var err error
			got, err = ns.NamespaceVarsOf(ctx, run)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got == nil || len(got) != want {
			t.Errorf("run %s answers the variables %#v for its replay", run, got)
		}
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		_, err := ns.NamespaceVarsOf(ctx, "01M2Z8V1P9C4XQ7K2N4D6F8H0Z")
		return err
	}); !errors.Is(err, ErrNoRun) {
		t.Errorf("the variables of no run answered %v", err)
	}
}
