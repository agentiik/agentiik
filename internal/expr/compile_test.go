package expr_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/internal/expr"
)

// TestTheDocumentationsOwnExpressions compiles every expression the documentation and
// the fixture corpus write, in the position each is written in. It is the first thing
// this package has to do and the thing a rewrite could quietly break.
func TestTheDocumentationsOwnExpressions(t *testing.T) {
	cases := []struct {
		src   string
		scope expr.Scope
		roots []expr.Root
	}{
		{"trigger.body.orders", expr.ScopeTrigger, []expr.Root{expr.RootTrigger}},
		{"event.data.amount > 0", expr.ScopeTrigger, []expr.Root{expr.RootEvent}},
		{"workflow.inputs.orders", expr.ScopeParams, []expr.Root{expr.RootWorkflow}},
		{"vars.currency", expr.ScopeParams, []expr.Root{expr.RootVars}},
		{"inputs.in.count > 0", expr.ScopeStep, []expr.Root{expr.RootInputs}},
		{"inputs.in.empty", expr.ScopeStep, []expr.Root{expr.RootInputs}},
		{"item.data.customer_id", expr.ScopeShardParams, []expr.Root{expr.RootItem}},
		{"matrix.region", expr.ScopeShardParams, []expr.Root{expr.RootMatrix}},
		{"matrix.profile", expr.ScopeShardParams, []expr.Root{expr.RootMatrix}},
		{"secrets.billing", expr.ScopeParams, []expr.Root{expr.RootSecrets}},
		{"trigger.body.cycle", expr.ScopeParams, []expr.Root{expr.RootTrigger}},
		{"steps.invoice.status == \"succeeded\"", expr.ScopeStep, []expr.Root{expr.RootSteps}},
		{"steps.fetch-orders.outputs.out.count", expr.ScopeStep, []expr.Root{expr.RootSteps}},
		{`"fr" in vars.regions`, expr.ScopeStep, []expr.Root{expr.RootVars}},
		{"has(inputs.in)", expr.ScopeStep, []expr.Root{expr.RootInputs}},
		{"run.attempt == 1 && workflow.namespace == vars.owner", expr.ScopeStep,
			[]expr.Root{expr.RootWorkflow, expr.RootRun, expr.RootVars}},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			p, err := expr.Compile(c.scope, c.src)
			if err != nil {
				t.Fatalf("refused in %s: %v", c.scope, err)
			}
			if p.Source() != c.src {
				t.Fatalf("the program remembers %q and the author wrote %q", p.Source(), c.src)
			}
			if got := p.Roots(); !reflect.DeepEqual(got, c.roots) {
				t.Fatalf("reads %v, and it reads %v", got, c.roots)
			}
		})
	}
}

// TestARootThePositionDoesNotExposeIsRefusedByTheCompiler is the whole reason the table
// is a set of environments: three of the rules the language states are one failure here,
// and the failure names the root so that a caller can name the rule.
func TestARootThePositionDoesNotExposeIsRefusedByTheCompiler(t *testing.T) {
	cases := []struct {
		name  string
		scope expr.Scope
		src   string
		root  string
	}{
		// The roots are workflow, run, trigger, event, vars, inputs, steps,
		// item, matrix and secrets, and globals is not one of them.
		{"a root that is not one of the ten", expr.ScopeParams, "globals.currency", "globals"},

		// item is available only under fan_out: item, which is what keeps the
		// controller from loading a namespace's data in order to schedule.
		{"item where the step is not sharded", expr.ScopeParams, "item.data.customer_id", "item"},
		{"item in a condition", expr.ScopeStep, "item.amount > 0", "item"},

		// secrets is exposed to params and to secrets and nowhere else, and a
		// secret is never reachable from a condition.
		{"a secret in a condition", expr.ScopeStep, `secrets.billing != ""`, "secrets"},
		{"a secret in a shard condition", expr.ScopeShard, "secrets.billing", "secrets"},

		// The on block reads the trigger and the event; a step reads neither.
		{"event outside a trigger", expr.ScopeStep, "event.id", "event"},
		{"the trigger in a condition", expr.ScopeStep, "trigger.body.cycle", "trigger"},

		// A trigger is decided before any step has run.
		{"port metadata in the on block", expr.ScopeTrigger, "inputs.in.count > 0", "inputs"},
		{"a step status in the on block", expr.ScopeTrigger, "steps.invoice.status", "steps"},

		// A matrix combination exists only where there is a shard.
		{"a matrix outside a shard", expr.ScopeParams, "matrix.region", "matrix"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := expr.Compile(c.scope, c.src)
			if err == nil {
				t.Fatalf("accepted %q in %s", c.src, c.scope)
			}
			var undeclared *expr.UndeclaredRoot
			if !errors.As(err, &undeclared) {
				t.Fatalf("refused %q as %v, and it is a root the position does not expose", c.src, err)
			}
			if undeclared.Name != c.root {
				t.Fatalf("names %q, and the expression reads %q", undeclared.Name, c.root)
			}
			if undeclared.Scope != c.scope {
				t.Fatalf("names %s, and the expression sits in %s", undeclared.Scope, c.scope)
			}
			if !strings.Contains(err.Error(), c.root) {
				t.Fatalf("the refusal does not name the root: %v", err)
			}
		})
	}
}

// TestTheRefusalNamesWhatThePositionDoesExpose is what makes the refusal actionable: an
// author reading it learns the list rather than being told what is not on it.
func TestTheRefusalNamesWhatThePositionDoesExpose(t *testing.T) {
	_, err := expr.Compile(expr.ScopeStep, "secrets.billing")
	if err == nil {
		t.Fatal("accepted a secret in a condition")
	}
	for _, root := range []string{"workflow", "run", "vars", "inputs", "steps"} {
		if !strings.Contains(err.Error(), root) {
			t.Fatalf("the refusal does not name %s, which a step does expose: %v", root, err)
		}
	}
}

// TestWhereARootIsReadIsWhereItIsRefused: the first name a person would see is the one
// the refusal names.
func TestWhereARootIsReadIsWhereItIsRefused(t *testing.T) {
	const src = `inputs.in.count > 0 && item.amount > 0`
	_, err := expr.Compile(expr.ScopeStep, src)
	var undeclared *expr.UndeclaredRoot
	if !errors.As(err, &undeclared) {
		t.Fatalf("accepted item in a step condition: %v", err)
	}
	if undeclared.Line != 1 || undeclared.Column != strings.Index(src, "item")+1 {
		t.Fatalf("points at line %d column %d, and item is written at column %d",
			undeclared.Line, undeclared.Column, strings.Index(src, "item")+1)
	}
}

// TestAVariableAnExpressionBindsItselfIsNotARoot keeps the rule about roots from
// catching a name that never was one. A comprehension variable is the expression's own,
// whatever it is called.
func TestAVariableAnExpressionBindsItselfIsNotARoot(t *testing.T) {
	p, err := expr.Compile(expr.ScopeStep, "[1, 2, 3].exists(item, item > 2)")
	if err != nil {
		t.Fatalf("refused an expression that reads no root at all: %v", err)
	}
	if got := p.Roots(); len(got) != 0 {
		t.Fatalf("reads %v, and it reads nothing", got)
	}
	v, err := expr.Evaluate(mustTemplate(t, expr.ScopeStep, "${{ [1, 2, 3].exists(item, item > 2) }}"), expr.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if v != true {
		t.Fatalf("evaluated to %v", v)
	}
}

// TestASyntaxErrorPointsAtTheAuthorsText: the compiler counts in the text it read, and
// that text is not always the text on disk.
func TestASyntaxErrorPointsAtTheAuthorsText(t *testing.T) {
	_, err := expr.Compile(expr.ScopeStep, "inputs.in.count >")
	if err == nil {
		t.Fatal("accepted an expression that ends on an operator")
	}
	if !strings.Contains(err.Error(), "line 1 column") {
		t.Fatalf("the refusal carries no position: %v", err)
	}
	if !strings.Contains(err.Error(), "inputs.in.count >") {
		t.Fatalf("the refusal does not show the expression: %v", err)
	}
}

// TestATypeErrorIsRefusedBeforeTheRunStarts. A condition that is not a comparison, or a
// comparison between things that do not compare, is a language error and not an
// evaluation that happens to fail on the day.
func TestATypeErrorIsRefusedBeforeTheRunStarts(t *testing.T) {
	for _, src := range []string{
		`"a" + 1`,
		`nosuchfunction(vars.a)`,
	} {
		if _, err := expr.Compile(expr.ScopeStep, src); err == nil {
			t.Fatalf("accepted %q", src)
		}
	}
}

// TestAnExpressionFillingAWholeValueKeepsItsType is the first half of the language's own
// sentence about interpolation, held at the level of one expression.
func TestAnExpressionFillingAWholeValueKeepsItsType(t *testing.T) {
	ctx := expr.Context{
		Vars: map[string]any{
			"currency": "EUR",
			"floor":    int64(3),
			"ratio":    1.5,
			"regions":  []any{"fr", "be"},
			"limits":   map[string]any{"max": int64(10)},
		},
		Inputs: map[string]expr.PortMeta{"in": {Count: 2, Bytes: 4096}},
	}
	cases := []struct {
		src  string
		want any
	}{
		{"vars.currency", "EUR"},
		{"vars.floor", int64(3)},
		{"vars.ratio", 1.5},
		{"vars.regions", []any{"fr", "be"}},
		{"vars.limits", map[string]any{"max": int64(10)}},
		{"inputs.in.count > 0", true},
		{"inputs.in.count", int64(2)},
		{"inputs.in.bytes", int64(4096)},
		{"inputs.in.empty", false},
		{"vars.nothing", nil},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			p, err := expr.Compile(expr.ScopeStep, c.src)
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.Eval(ctx)
			if c.want == nil {
				// A key the context does not carry is an evaluation
				// failure and not a silent empty value: the workflow said
				// to read it.
				if err == nil {
					t.Fatalf("read a key the context does not carry and answered %v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("evaluated to %#v, and the value is %#v", got, c.want)
			}
		})
	}
}

// TestStepMetadataIsStatusAndPortMetadata holds the shape of the steps root, which is
// what a step downstream reads about a step upstream.
func TestStepMetadataIsStatusAndPortMetadata(t *testing.T) {
	ctx := expr.Context{
		Steps: map[string]expr.StepMeta{
			"fetch-orders": {
				Status:  "succeeded",
				Outputs: map[string]expr.PortMeta{"out": {Count: 7}, "error": {Count: 0, Empty: true}},
			},
		},
	}
	cases := []struct {
		src  string
		want any
	}{
		{`steps.fetch-orders.status == "succeeded"`, true},
		{"steps.fetch-orders.outputs.out.count", int64(7)},
		{"steps.fetch-orders.outputs.error.empty", true},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			p, err := expr.Compile(expr.ScopeStep, c.src)
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.Eval(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("evaluated to %#v, and the value is %#v", got, c.want)
			}
		})
	}
}

// TestEvaluationIsPure: the same expression against the same context is the same value,
// every time. It is what lets a run replay.
func TestEvaluationIsPure(t *testing.T) {
	p, err := expr.Compile(expr.ScopeShardParams, "matrix.region + \"-\" + string(inputs.in.count)")
	if err != nil {
		t.Fatal(err)
	}
	ctx := expr.Context{
		Matrix: map[string]any{"region": "fr"},
		Inputs: map[string]expr.PortMeta{"in": {Count: 3}},
	}
	first, err := p.Eval(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		again, err := p.Eval(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("evaluated to %v and then to %v", first, again)
		}
	}
	if first != "fr-3" {
		t.Fatalf("evaluated to %v", first)
	}
}

// TestRootsAreReportedOnceAndInTheTablesOrder, so that a caller comparing them holds one
// answer and not an order that depends on where a name was written.
func TestRootsAreReportedOnceAndInTheTablesOrder(t *testing.T) {
	p, err := expr.Compile(expr.ScopeShardParams,
		`secrets.billing != secrets.other && matrix.region == vars.region && workflow.name == vars.name`)
	if err != nil {
		t.Fatal(err)
	}
	want := []expr.Root{expr.RootWorkflow, expr.RootVars, expr.RootMatrix, expr.RootSecrets}
	if got := p.Roots(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reads %v, and it reads %v", got, want)
	}
}

// TestTheEnvironmentsAreSharedAndNothingMutatesThem. A controller compiles for many
// namespaces at once and evaluates for many runs at once, and the five environments are
// built once and read from then on. Under the race detector this is the test that says
// so.
func TestTheEnvironmentsAreSharedAndNothingMutatesThem(t *testing.T) {
	ctx := expr.Context{
		Vars:   map[string]any{"region": "fr"},
		Inputs: map[string]expr.PortMeta{"in": {Count: 1}},
	}
	done := make(chan error, 8)
	for i := 0; i < cap(done); i++ {
		go func() {
			for n := 0; n < 20; n++ {
				p, err := expr.Compile(expr.ScopeStep, `inputs.in.count > 0 && vars.region == "fr"`)
				if err != nil {
					done <- err
					return
				}
				v, err := p.Eval(ctx)
				if err != nil {
					done <- err
					return
				}
				if v != true {
					done <- errors.New("evaluated to something other than true")
					return
				}
			}
			done <- nil
		}()
	}
	for i := 0; i < cap(done); i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func mustTemplate(t *testing.T, sc expr.Scope, s string) *expr.Template {
	t.Helper()
	tpl, err := expr.Interpolate(sc, s)
	if err != nil {
		t.Fatal(err)
	}
	return tpl
}
