package expr_test

import (
	"reflect"
	"testing"

	"github.com/agentiik/agentiik/internal/expr"
)

// TestTheTableIsTheTable holds each position to the row it is: what a position exposes
// is the documentation's third column read the other way round, and nothing else.
func TestTheTableIsTheTable(t *testing.T) {
	cases := []struct {
		scope expr.Scope
		roots []expr.Root
	}{
		{expr.ScopeTrigger, []expr.Root{
			expr.RootWorkflow, expr.RootRun, expr.RootTrigger, expr.RootEvent, expr.RootVars,
		}},
		{expr.ScopeStep, []expr.Root{
			expr.RootWorkflow, expr.RootRun, expr.RootVars, expr.RootInputs, expr.RootSteps,
		}},
		{expr.ScopeParams, []expr.Root{
			expr.RootWorkflow, expr.RootRun, expr.RootTrigger, expr.RootVars,
			expr.RootInputs, expr.RootSteps, expr.RootSecrets,
		}},
		{expr.ScopeShard, []expr.Root{
			expr.RootWorkflow, expr.RootRun, expr.RootVars, expr.RootInputs,
			expr.RootSteps, expr.RootItem, expr.RootMatrix,
		}},
		{expr.ScopeShardParams, []expr.Root{
			expr.RootWorkflow, expr.RootRun, expr.RootTrigger, expr.RootVars,
			expr.RootInputs, expr.RootSteps, expr.RootItem, expr.RootMatrix, expr.RootSecrets,
		}},
	}
	for _, c := range cases {
		t.Run(c.scope.String(), func(t *testing.T) {
			if got := c.scope.Roots(); !reflect.DeepEqual(got, c.roots) {
				t.Fatalf("exposes %v, and it exposes %v", got, c.roots)
			}
		})
	}
}

// TestWhereEachRootIsAvailable reads the same table down its own column, which is how the
// documentation writes it: workflow, run and vars everywhere, the trigger in the on block
// and in step parameters, the event in event triggers, port and step metadata in a step,
// item and matrix in a shard, and secrets only in params and secrets.
func TestWhereEachRootIsAvailable(t *testing.T) {
	all := []expr.Scope{
		expr.ScopeTrigger, expr.ScopeStep, expr.ScopeParams,
		expr.ScopeShard, expr.ScopeShardParams,
	}
	cases := []struct {
		root   expr.Root
		scopes []expr.Scope
	}{
		{expr.RootWorkflow, all},
		{expr.RootRun, all},
		{expr.RootVars, all},
		{expr.RootTrigger, []expr.Scope{expr.ScopeTrigger, expr.ScopeParams, expr.ScopeShardParams}},
		{expr.RootEvent, []expr.Scope{expr.ScopeTrigger}},
		{expr.RootInputs, []expr.Scope{expr.ScopeStep, expr.ScopeParams, expr.ScopeShard, expr.ScopeShardParams}},
		{expr.RootSteps, []expr.Scope{expr.ScopeStep, expr.ScopeParams, expr.ScopeShard, expr.ScopeShardParams}},
		{expr.RootItem, []expr.Scope{expr.ScopeShard, expr.ScopeShardParams}},
		{expr.RootMatrix, []expr.Scope{expr.ScopeShard, expr.ScopeShardParams}},
		{expr.RootSecrets, []expr.Scope{expr.ScopeParams, expr.ScopeShardParams}},
	}
	for _, c := range cases {
		t.Run(c.root.String(), func(t *testing.T) {
			want := make(map[expr.Scope]bool, len(c.scopes))
			for _, sc := range c.scopes {
				want[sc] = true
			}
			for _, sc := range all {
				exposed := false
				for _, r := range sc.Roots() {
					exposed = exposed || r == c.root
				}
				if exposed != want[sc] {
					t.Fatalf("%s in %s: exposed %v, and the table says %v", c.root, sc, exposed, want[sc])
				}
			}
		})
	}
}

// TestThereAreTenRootsAndNoEleventh. The table is closed, and a name outside it is a
// refusal rather than an empty value.
func TestThereAreTenRootsAndNoEleventh(t *testing.T) {
	ten := []string{
		"workflow", "run", "trigger", "event", "vars",
		"inputs", "steps", "item", "matrix", "secrets",
	}
	for _, name := range ten {
		r, ok := expr.ParseRoot(name)
		if !ok {
			t.Fatalf("%s is a root of the table and is not read as one", name)
		}
		if r.String() != name {
			t.Fatalf("%s is read back as %s", name, r)
		}
	}
	for _, name := range []string{"globals", "env", "secret", "items", "", "Workflow"} {
		if _, ok := expr.ParseRoot(name); ok {
			t.Fatalf("%q is read as a root", name)
		}
	}
}

// TestTheTableIsNotReachableThroughWhatItHandsOut: a caller holding the roots of a
// position cannot widen that position by writing into the slice it was given.
func TestTheTableIsNotReachableThroughWhatItHandsOut(t *testing.T) {
	roots := expr.ScopeStep.Roots()
	roots[0] = expr.RootSecrets
	if again := expr.ScopeStep.Roots(); again[0] != expr.RootWorkflow {
		t.Fatalf("a step now exposes %v", again)
	}
	if _, err := expr.Compile(expr.ScopeStep, "secrets.billing"); err == nil {
		t.Fatal("a step now reads a secret")
	}
}
