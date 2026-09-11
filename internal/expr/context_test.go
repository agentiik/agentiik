package expr_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/internal/expr"
)

// TestAPortIsCountEmptyAndBytesAndNothingElse is the decision the whole table is shaped
// by: item contents are not exposed to controller expressions, tests read port metadata
// instead. A controller that could read contents would load a namespace's envelopes in
// order to schedule.
func TestAPortIsCountEmptyAndBytesAndNothingElse(t *testing.T) {
	ctx := expr.Context{Inputs: map[string]expr.PortMeta{"in": {Count: 2, Empty: false, Bytes: 96}}}
	for _, src := range []string{"inputs.in.count", "inputs.in.empty", "inputs.in.bytes"} {
		p, err := expr.Compile(expr.ScopeStep, src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if _, err := p.Eval(ctx); err != nil {
			t.Fatalf("%s: %v", src, err)
		}
	}
	for _, src := range []string{"inputs.in.items", "inputs.in.data", "inputs.in.items[0]"} {
		p, err := expr.Compile(expr.ScopeStep, src)
		if err != nil {
			continue // refused already, which is the same answer sooner
		}
		if got, err := p.Eval(ctx); err == nil {
			t.Fatalf("%s read %#v, and a port exposes count, empty and bytes", src, got)
		}
	}
}

// TestARootThePositionExposesAndTheContextLeavesUnsetFails, rather than reading as an
// empty value. The workflow said to read it, and answering nothing would turn a mistake
// in the file into a decision nobody made.
func TestARootThePositionExposesAndTheContextLeavesUnsetFails(t *testing.T) {
	p, err := expr.Compile(expr.ScopeStep, "inputs.in.count > 0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Eval(expr.Context{}); err == nil {
		t.Fatal("read a port the context does not carry")
	}
}

// TestAContextCarryingMoreThanThePositionExposesChangesNothing. The activation is built
// from the table and not from what the caller happens to have filled in, so a context
// assembled once and used in several positions cannot widen any of them.
func TestAContextCarryingMoreThanThePositionExposesChangesNothing(t *testing.T) {
	full := expr.Context{
		Workflow: map[string]any{"name": "monthly-invoicing"},
		Trigger:  map[string]any{"body": map[string]any{"cycle": "2026-03"}},
		Event:    map[string]any{"id": "01JMZ"},
		Item:     map[string]any{"data": map[string]any{"customer_id": "c-1"}},
		Matrix:   map[string]any{"region": "fr"},
		Secrets:  map[string]expr.Secret{"billing": {Name: "billing"}},
	}
	for _, src := range []string{"trigger.body.cycle", "event.id", "item.data.customer_id", "matrix.region", "secrets.billing"} {
		if _, err := expr.Compile(expr.ScopeStep, src); err == nil {
			t.Fatalf("a step read %s", src)
		}
	}
	// The same context in a position that does expose them reads them.
	p, err := expr.Compile(expr.ScopeShardParams, "item.data.customer_id")
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Eval(full)
	if err != nil {
		t.Fatal(err)
	}
	if got != "c-1" {
		t.Fatalf("read %#v", got)
	}
}

// TestADocumentComesBackAsADocument: what goes into a param is what came out of JSON, in
// the Go types the rest of the engine carries a document in.
func TestADocumentComesBackAsADocument(t *testing.T) {
	ctx := expr.Context{Vars: map[string]any{
		"regions": []any{
			map[string]any{"code": "fr", "rate": 0.2},
			map[string]any{"code": "be", "rate": 0.21},
		},
	}}
	p, err := expr.Compile(expr.ScopeStep, "vars.regions")
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Eval(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"code": "fr", "rate": 0.2},
		map[string]any{"code": "be", "rate": 0.21},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("read %#v, and the document is %#v", got, want)
	}
}

// TestAListBuiltByAnExpressionIsADocumentToo, which is what makes a matrix value or a
// filtered list usable as a param.
func TestAListBuiltByAnExpressionIsADocumentToo(t *testing.T) {
	ctx := expr.Context{Vars: map[string]any{"regions": []any{"fr", "be", "ch"}}}
	p, err := expr.Compile(expr.ScopeStep, `vars.regions.filter(r, r != "ch")`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Eval(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []any{"fr", "be"}) {
		t.Fatalf("read %#v", got)
	}
}

// TestAnEvaluationFailureShowsTheExpression. A run that stops on an expression has to say
// which expression, in the text the author wrote.
func TestAnEvaluationFailureShowsTheExpression(t *testing.T) {
	p, err := expr.Compile(expr.ScopeStep, "inputs.in.count > 0")
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Eval(expr.Context{})
	if err == nil {
		t.Fatal("read a port the context does not carry")
	}
	if !strings.Contains(err.Error(), "inputs.in.count > 0") {
		t.Fatalf("the failure does not show the expression: %v", err)
	}
}
