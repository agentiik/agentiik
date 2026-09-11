package expr_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/agentiik/agentiik/internal/expr"
)

// TestTheTwoRulesOfInterpolation is the language's own sentence: an expression that
// fills the whole value keeps its type, and an expression embedded in a string is
// converted to text.
func TestTheTwoRulesOfInterpolation(t *testing.T) {
	ctx := expr.Context{
		Vars: map[string]any{
			"currency": "EUR",
			"floor":    int64(3),
			"ratio":    1.5,
			"flag":     true,
			"regions":  []any{"fr", "be"},
			"limits":   map[string]any{"max": int64(10)},
			"nothing":  nil,
		},
		Matrix: map[string]any{"region": "fr"},
		Inputs: map[string]expr.PortMeta{"in": {Count: 3}},
	}
	cases := []struct {
		name  string
		value string
		whole bool
		want  any
	}{
		{"a number filling the whole value", "${{ vars.floor }}", true, int64(3)},
		{"a double filling the whole value", "${{ vars.ratio }}", true, 1.5},
		{"a boolean filling the whole value", "${{ vars.flag }}", true, true},
		{"a list filling the whole value", "${{ vars.regions }}", true, []any{"fr", "be"}},
		{"a map filling the whole value", "${{ vars.limits }}", true, map[string]any{"max": int64(10)}},
		{"a condition filling the whole value", "${{ inputs.in.count > 0 }}", true, true},
		{"a string filling the whole value", "${{ vars.currency }}", true, "EUR"},

		{"a number embedded in a string", "${{ vars.floor }} orders", false, "3 orders"},
		{"a string embedded in a string", "invoice-${{ matrix.region }}", false, "invoice-fr"},
		{"a boolean embedded in a string", "flag=${{ vars.flag }}", false, "flag=true"},
		{"a double embedded in a string", "ratio ${{ vars.ratio }}", false, "ratio 1.5"},
		{"two expressions in one value", "${{ matrix.region }}/${{ vars.currency }}", false, "fr/EUR"},
		{"a value carrying no expression", "invoice", false, "invoice"},
		{"an empty value", "", false, ""},

		// The documentation says an embedded expression is converted to text
		// and does not say what the text of a list is. It is written in the
		// document form every other value in this engine travels in.
		{"a list embedded in a string", "regions: ${{ vars.regions }}", false, `regions: ["fr","be"]`},
		{"a map embedded in a string", "limits: ${{ vars.limits }}", false, `limits: {"max":10}`},
		{"nothing embedded in a string", "value: ${{ vars.nothing }}", false, "value: null"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tpl, err := expr.Interpolate(expr.ScopeShardParams, c.value)
			if err != nil {
				t.Fatalf("refused %q: %v", c.value, err)
			}
			if tpl.Whole() != c.whole {
				t.Fatalf("fills the whole value: %v, and it is %v", tpl.Whole(), c.whole)
			}
			got, err := expr.Evaluate(tpl, ctx)
			if err != nil {
				t.Fatalf("evaluating %q: %v", c.value, err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("evaluated to %#v, and the value is %#v", got, c.want)
			}
		})
	}
}

// TestATimestampIsWrittenTheWayTheLanguageWritesOne. Where CEL defines a conversion to
// text, that conversion is the one used: the language an author writes in is the
// language that says what its values look like.
func TestATimestampIsWrittenTheWayTheLanguageWritesOne(t *testing.T) {
	started := time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC)
	tpl, err := expr.Interpolate(expr.ScopeStep, "run started at ${{ run.started_at }}")
	if err != nil {
		t.Fatal(err)
	}
	got, err := expr.Evaluate(tpl, expr.Context{Run: map[string]any{"started_at": started}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "run started at 2026-03-01T09:30:00Z" {
		t.Fatalf("evaluated to %q", got)
	}
}

// TestAValueCarryingNoExpressionCarriesNoProgram, which is how a caller tells a plain
// string apart without scanning it a second time.
func TestAValueCarryingNoExpressionCarriesNoProgram(t *testing.T) {
	tpl, err := expr.Interpolate(expr.ScopeStep, "ghcr.io/acme/agk-invoice@sha256:1ab7")
	if err != nil {
		t.Fatal(err)
	}
	if len(tpl.Programs()) != 0 {
		t.Fatalf("found %d expressions in a value that carries none", len(tpl.Programs()))
	}
	if tpl.Whole() {
		t.Fatal("a value that carries no expression is not filled by one")
	}
}

// TestAnExpressionInAValueIsCompiledForThePositionTheValueSitsIn: interpolation is not a
// second door into the context table.
func TestAnExpressionInAValueIsCompiledForThePositionTheValueSitsIn(t *testing.T) {
	if _, err := expr.Interpolate(expr.ScopeStep, "customer ${{ item.data.customer_id }}"); err == nil {
		t.Fatal("accepted item in a step condition")
	} else {
		var undeclared *expr.UndeclaredRoot
		if !errors.As(err, &undeclared) || undeclared.Name != "item" {
			t.Fatalf("refused it as %v, and item is the root it reads", err)
		}
	}
	if _, err := expr.Interpolate(expr.ScopeShardParams, "customer ${{ item.data.customer_id }}"); err != nil {
		t.Fatalf("refused item in the params of a sharded step: %v", err)
	}
}

// TestTheProgramsOfAValueAreTheExpressionsItCarries, in the order they were written, so
// that a caller holding more than a position can hold each of them to a rule of its own.
func TestTheProgramsOfAValueAreTheExpressionsItCarries(t *testing.T) {
	tpl, err := expr.Interpolate(expr.ScopeShardParams, "${{ matrix.region }}-${{ item.id }}")
	if err != nil {
		t.Fatal(err)
	}
	programs := tpl.Programs()
	if len(programs) != 2 {
		t.Fatalf("found %d expressions in a value that carries two", len(programs))
	}
	if programs[0].Source() != " matrix.region " || programs[1].Source() != " item.id " {
		t.Fatalf("found %q and %q", programs[0].Source(), programs[1].Source())
	}
	if got := programs[1].Roots(); len(got) != 1 || got[0] != expr.RootItem {
		t.Fatalf("the second expression reads %v", got)
	}
}

// TestWhatCloseWhatIsReadTheWayTheExpressionReadsIt: a }} inside a string literal closes
// nothing, and a map literal inside an expression closes only itself.
func TestWhatCloseWhatIsReadTheWayTheExpressionReadsIt(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  any
	}{
		{"braces inside a string literal", `${{ vars.a == "}}" }}`, true},
		{"a map literal inside an expression", `${{ {"a": 1}.a }}`, int64(1)},
		{"a map literal ending the expression", `${{ {"a": 1}.a == 1}}`, true},
		{"a brace in the literal text around it", `{ "region": "${{ vars.region }}" }`, `{ "region": "fr" }`},
	}
	ctx := expr.Context{Vars: map[string]any{"a": "}}", "region": "fr"}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tpl, err := expr.Interpolate(expr.ScopeStep, c.value)
			if err != nil {
				t.Fatalf("refused %q: %v", c.value, err)
			}
			got, err := expr.Evaluate(tpl, ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("evaluated to %#v, and the value is %#v", got, c.want)
			}
		})
	}
}

// TestAnExpressionThatNeverClosesIsRefused. A value that opens an expression and does
// not close it is a mistake in the file, not text.
func TestAnExpressionThatNeverClosesIsRefused(t *testing.T) {
	for _, value := range []string{
		"${{ vars.a",
		"${{ vars.a }",
		`${{ vars.a == "b }}`,
		"prefix ${{ vars.a",
	} {
		if _, err := expr.Interpolate(expr.ScopeStep, value); err == nil {
			t.Fatalf("accepted %q, which never closes what it opens", value)
		}
	}
}
