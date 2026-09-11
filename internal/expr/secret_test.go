package expr_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/internal/expr"
)

// TestASecretFillsAValueAndNothingElse. secrets is exposed to params and to secrets, and
// what an expression may do with one there is hand it on: a secret fills a whole value
// and arrives as the reference it is, with the value still where it was, which is
// nowhere in this process.
func TestASecretFillsAValueAndNothingElse(t *testing.T) {
	ctx := expr.Context{Secrets: map[string]expr.Secret{"billing": {Name: "billing"}}}
	tpl, err := expr.Interpolate(expr.ScopeParams, "${{ secrets.billing }}")
	if err != nil {
		t.Fatalf("refused a secret filling a whole param: %v", err)
	}
	got, err := expr.Evaluate(tpl, ctx)
	if err != nil {
		t.Fatal(err)
	}
	secret, ok := got.(expr.Secret)
	if !ok {
		t.Fatalf("evaluated to %#v, and a secret evaluates to a reference", got)
	}
	if secret.Name != "billing" {
		t.Fatalf("references %q", secret.Name)
	}
}

// TestASecretIsNeverConvertedToText holds the one rule that keeps interpolation from
// being a way out. Every route from a secret to a string is refused, and refused while
// the workflow is being read rather than while a run is under way.
func TestASecretIsNeverConvertedToText(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"embedded in a string", "Bearer ${{ secrets.billing }}"},
		{"embedded beside another expression", "${{ vars.user }}:${{ secrets.billing }}"},
		{"concatenated inside the expression", `${{ "Bearer " + secrets.billing }}`},
		{"converted inside the expression", "${{ string(secrets.billing) }}"},
		{"compared against a string", `${{ secrets.billing != "" }}`},
		{"read for its size", "${{ size(secrets.billing) > 0 }}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := expr.Interpolate(expr.ScopeParams, c.value)
			if err == nil {
				t.Fatalf("accepted %q", c.value)
			}
		})
	}
}

// TestTheRefusalOfASecretInTextSaysThatItIsOpaque, so that a caller tells it apart from
// a compilation failure by asking rather than by reading.
func TestTheRefusalOfASecretInTextSaysThatItIsOpaque(t *testing.T) {
	_, err := expr.Interpolate(expr.ScopeParams, "Bearer ${{ secrets.billing }}")
	if !errors.Is(err, expr.ErrSecretOpaque) {
		t.Fatalf("refused it as %v, and it is a secret that would have to become text", err)
	}
}

// TestASecretPrintsNothing. A secret that reaches a format verb, a log line or a
// document is a mistake, and the mistake prints nothing and writes nothing.
func TestASecretPrintsNothing(t *testing.T) {
	s := expr.Secret{Name: "billing"}
	if got := fmt.Sprintf("%v", s); got != "" {
		t.Fatalf("printed %q", got)
	}
	if got := s.String(); got != "" {
		t.Fatalf("stringified to %q", got)
	}
	if _, err := json.Marshal(s); err == nil {
		t.Fatal("a secret was written into a document")
	} else if !errors.Is(err, expr.ErrSecretOpaque) {
		t.Fatalf("refused it as %v", err)
	}
}

// TestTwoSecretsCompareByWhatTheyReference, which is the one comparison a secret has: it
// is a name, and the value behind it is not here to be compared.
func TestTwoSecretsCompareByWhatTheyReference(t *testing.T) {
	ctx := expr.Context{Secrets: map[string]expr.Secret{
		"billing": {Name: "billing"},
		"same":    {Name: "billing"},
		"other":   {Name: "other"},
	}}
	cases := []struct {
		src  string
		want bool
	}{
		{"secrets.billing == secrets.same", true},
		{"secrets.billing == secrets.other", false},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			p, err := expr.Compile(expr.ScopeParams, c.src)
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.Eval(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("evaluated to %v", got)
			}
		})
	}
}

// TestASecretInAConditionIsRefusedByTheRootAndNotByTheType. The two refusals read
// differently and a caller acts on them differently: one is a position that exposes no
// secret at all, the other is a secret that cannot become what the expression asks for.
func TestASecretInAConditionIsRefusedByTheRootAndNotByTheType(t *testing.T) {
	_, err := expr.Compile(expr.ScopeStep, `secrets.billing != ""`)
	var undeclared *expr.UndeclaredRoot
	if !errors.As(err, &undeclared) {
		t.Fatalf("refused it as %v", err)
	}
	if undeclared.Name != "secrets" {
		t.Fatalf("names %q", undeclared.Name)
	}
	if strings.Contains(err.Error(), "overload") {
		t.Fatalf("refused it for its type rather than for its position: %v", err)
	}
}
