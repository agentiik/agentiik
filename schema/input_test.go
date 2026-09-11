package schema

import (
	"errors"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/agentiik/agentiik/agk"
)

// mustCompile builds the schema an input declares, the way the workflow file writes it.
func mustCompile(t *testing.T, doc string) *Schema {
	t.Helper()
	s, err := NewCompiler(fstest.MapFS{}).Compile([]byte(doc))
	if err != nil {
		t.Fatalf("Compile %s: %v", doc, err)
	}
	return s
}

func TestBind(t *testing.T) {
	object := func(t *testing.T) *Schema {
		return mustCompile(t, `{ "type": "object", "required": ["id"], "properties": { "id": { "type": "string" } } }`)
	}
	array := func(t *testing.T) *Schema { return mustCompile(t, `{ "type": "array" }`) }

	cases := []struct {
		name     string
		declared func(*testing.T) map[string]Input
		supplied map[string]any
		want     map[string]any
		// refused names the input the run has to be refused by, and rule the rule.
		refused string
		rule    string
		detail  string
	}{
		{
			name: "a supplied value that satisfies its schema is bound",
			declared: func(t *testing.T) map[string]Input {
				return map[string]Input{"orders": {Schema: object(t), Required: true}}
			},
			supplied: map[string]any{"orders": map[string]any{"id": "o-1"}},
			want:     map[string]any{"orders": map[string]any{"id": "o-1"}},
		},
		{
			name: "a required input nobody supplied refuses the run",
			declared: func(t *testing.T) map[string]Input {
				return map[string]Input{"orders": {Schema: object(t), Required: true}}
			},
			supplied: map[string]any{},
			refused:  "orders",
			rule:     RuleRequired,
		},
		{
			name: "a required input carrying a default is not missing",
			declared: func(t *testing.T) map[string]Input {
				return map[string]Input{"orders": {Schema: object(t), Required: true, Default: map[string]any{"id": "none"}}}
			},
			supplied: map[string]any{},
			want:     map[string]any{"orders": map[string]any{"id": "none"}},
		},
		{
			name: "a default stands in for a value the run did not supply",
			declared: func(t *testing.T) map[string]Input {
				return map[string]Input{"customers": {Schema: array(t), Default: []any{}}}
			},
			supplied: map[string]any{},
			want:     map[string]any{"customers": []any{}},
		},
		{
			name: "a supplied value wins over the default",
			declared: func(t *testing.T) map[string]Input {
				return map[string]Input{"customers": {Schema: array(t), Default: []any{"a"}}}
			},
			supplied: map[string]any{"customers": []any{"b"}},
			want:     map[string]any{"customers": []any{"b"}},
		},
		{
			name: "an optional input with no default is absent rather than null",
			declared: func(t *testing.T) map[string]Input {
				return map[string]Input{"customers": {Schema: array(t)}}
			},
			supplied: map[string]any{},
			want:     map[string]any{},
		},
		{
			name: "a value that does not satisfy its schema refuses the run",
			declared: func(t *testing.T) map[string]Input {
				return map[string]Input{"orders": {Schema: object(t), Required: true}}
			},
			supplied: map[string]any{"orders": map[string]any{"id": 7}},
			refused:  "orders",
			rule:     RuleSchema,
			detail:   "/id: got number, want string",
		},
		{
			name: "a default that does not satisfy its own schema refuses the run",
			declared: func(t *testing.T) map[string]Input {
				return map[string]Input{"orders": {Schema: object(t), Default: []any{}}}
			},
			supplied: map[string]any{},
			refused:  "orders",
			rule:     RuleSchema,
			detail:   "the declared default does not satisfy it: got array, want object",
		},
		{
			name: "an input the workflow does not declare refuses the run",
			declared: func(t *testing.T) map[string]Input {
				return map[string]Input{"orders": {Schema: object(t)}}
			},
			supplied: map[string]any{"ordrs": map[string]any{"id": "o-1"}},
			refused:  "ordrs",
			rule:     RuleUndeclared,
		},
		{
			name: "an input without a schema is accepted as it comes",
			declared: func(t *testing.T) map[string]Input {
				return map[string]Input{"anything": {Required: true}}
			},
			supplied: map[string]any{"anything": "a bare string"},
			want:     map[string]any{"anything": "a bare string"},
		},
		{
			name: "a null a schema accepts is a value and not an absence",
			declared: func(t *testing.T) map[string]Input {
				return map[string]Input{"note": {Schema: mustCompile(t, `{ "type": ["string", "null"] }`), Default: "none"}}
			},
			supplied: map[string]any{"note": nil},
			want:     map[string]any{"note": nil},
		},
		{
			name: "a null a schema refuses is refused rather than defaulted",
			declared: func(t *testing.T) map[string]Input {
				return map[string]Input{"note": {Schema: mustCompile(t, `{ "type": "string" }`), Default: "none"}}
			},
			supplied: map[string]any{"note": nil},
			refused:  "note",
			rule:     RuleSchema,
			detail:   "got null, want string",
		},
		{
			name:     "a workflow with no inputs binds nothing",
			declared: func(t *testing.T) map[string]Input { return nil },
			supplied: nil,
			want:     map[string]any{},
		},
		{
			name: "a misspelled name is reported as the misspelling, not as what it left unsupplied",
			declared: func(t *testing.T) map[string]Input {
				return map[string]Input{"orders": {Schema: object(t), Required: true}}
			},
			supplied: map[string]any{"ordres": map[string]any{"id": "o-1"}},
			refused:  "ordres",
			rule:     RuleUndeclared,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Bind(tc.declared(t), tc.supplied)

			if tc.refused == "" {
				if err != nil {
					t.Fatalf("Bind: %v, want the run to start", err)
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("Bind: %#v, want %#v", got, tc.want)
				}
				return
			}

			if err == nil {
				t.Fatalf("Bind: the run started, want it refused by input %q", tc.refused)
			}
			if got != nil {
				t.Fatalf("Bind: returned %#v beside a refusal, want nothing", got)
			}
			var r *InputRefusal
			if !errors.As(err, &r) {
				t.Fatalf("Bind: %v, want an *InputRefusal", err)
			}
			if r.Input != tc.refused {
				t.Errorf("refused input %q, want %q", r.Input, tc.refused)
			}
			if r.Rule != tc.rule {
				t.Errorf("refused by rule %q, want %q", r.Rule, tc.rule)
			}
			if tc.detail != "" && r.Detail != tc.detail {
				t.Errorf("detail %q, want %q", r.Detail, tc.detail)
			}
			if !errors.Is(err, agk.ErrRunRefused) {
				t.Errorf("Bind: %v does not answer to agk.ErrRunRefused", err)
			}
		})
	}
}

// The inputs block the specification writes out, compiled from the tree it describes
// and bound the way a trigger fills it.
//
//	inputs:
//	  orders:
//	    schema: { $ref: "./schemas/order.json" }
//	    required: true
//	  customers:
//	    schema: { $ref: "./schemas/customer.json" }
//	    default: []
func TestBindTheDocumentedInputs(t *testing.T) {
	c := NewCompiler(fstest.MapFS{
		"schemas/order.json": &fstest.MapFile{Data: []byte(
			`{ "type": "array", "items": { "type": "object", "required": ["id"] } }`)},
		"schemas/customer.json": &fstest.MapFile{Data: []byte(
			`{ "type": "array", "items": { "type": "object", "required": ["name"] } }`)},
	})
	compile := func(doc string) *Schema {
		s, err := c.Compile([]byte(doc))
		if err != nil {
			t.Fatalf("Compile %s: %v", doc, err)
		}
		return s
	}
	declared := map[string]Input{
		"orders":    {Schema: compile(`{ "$ref": "./schemas/order.json" }`), Required: true},
		"customers": {Schema: compile(`{ "$ref": "./schemas/customer.json" }`), Default: []any{}},
	}

	t.Run("a trigger supplying orders alone starts the run on the default", func(t *testing.T) {
		bound, err := Bind(declared, map[string]any{
			"orders": []any{map[string]any{"id": "o-1"}},
		})
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}
		want := map[string]any{
			"orders":    []any{map[string]any{"id": "o-1"}},
			"customers": []any{},
		}
		if !reflect.DeepEqual(bound, want) {
			t.Fatalf("Bind: %#v, want %#v", bound, want)
		}
	})

	t.Run("a trigger supplying nothing is refused by orders", func(t *testing.T) {
		_, err := Bind(declared, nil)
		if !errors.Is(err, agk.ErrRunRefused) {
			t.Fatalf("Bind: %v, want the run refused", err)
		}
		want := "input orders: required: no value supplied and the input declares no default; run refused"
		if err.Error() != want {
			t.Fatalf("Bind: %q, want %q", err, want)
		}
	})

	t.Run("a trigger supplying an order without an id is refused by the tree's schema", func(t *testing.T) {
		_, err := Bind(declared, map[string]any{"orders": []any{map[string]any{}}})
		if !errors.Is(err, agk.ErrRunRefused) {
			t.Fatalf("Bind: %v, want the run refused", err)
		}
		want := "input orders: schema: /0: missing property 'id'; run refused"
		if err.Error() != want {
			t.Fatalf("Bind: %q, want %q", err, want)
		}
	})
}

// The error a person reads in a log names the input and the rule, in that order.
func TestInputRefusalError(t *testing.T) {
	cases := []struct {
		refusal InputRefusal
		want    string
	}{
		{
			refusal: InputRefusal{Input: "orders", Rule: RuleRequired, Detail: "no value supplied and the input declares no default"},
			want:    "input orders: required: no value supplied and the input declares no default; run refused",
		},
		{
			refusal: InputRefusal{Input: "orders", Rule: RuleSchema, Detail: "/amount: minimum: got -1, want 0"},
			want:    "input orders: schema: /amount: minimum: got -1, want 0; run refused",
		},
		{
			refusal: InputRefusal{Input: "ordrs", Rule: RuleUndeclared},
			want:    "input ordrs: undeclared; run refused",
		},
	}
	for _, tc := range cases {
		if got := tc.refusal.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}

// A run refused by two inputs at once is refused by the same one every time, or no test
// can be written against the refusal.
func TestBindRefusesInAFixedOrder(t *testing.T) {
	declared := map[string]Input{
		"alpha":   {Required: true},
		"beta":    {Required: true},
		"charlie": {Required: true},
	}
	for i := 0; i < 32; i++ {
		_, err := Bind(declared, map[string]any{})
		var r *InputRefusal
		if !errors.As(err, &r) {
			t.Fatalf("Bind: %v, want an *InputRefusal", err)
		}
		if r.Input != "alpha" {
			t.Fatalf("refused input %q, want %q on every run", r.Input, "alpha")
		}
	}

	for i := 0; i < 32; i++ {
		_, err := Bind(declared, map[string]any{"yankee": 1, "xray": 1})
		var r *InputRefusal
		if !errors.As(err, &r) {
			t.Fatalf("Bind: %v, want an *InputRefusal", err)
		}
		if r.Input != "xray" || r.Rule != RuleUndeclared {
			t.Fatalf("refused input %q by %q, want %q by %q on every run", r.Input, r.Rule, "xray", RuleUndeclared)
		}
	}
}

// The declaration is read once and handed to every run, so a run that reaches into a
// defaulted value must not be editing what the next run starts from.
func TestBindCopiesTheDefault(t *testing.T) {
	declared := map[string]Input{
		"customers": {Default: []any{map[string]any{"id": "c-1"}}},
	}

	first, err := Bind(declared, nil)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	first["customers"].([]any)[0].(map[string]any)["id"] = "edited"

	second, err := Bind(declared, nil)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	got := second["customers"].([]any)[0].(map[string]any)["id"]
	if got != "c-1" {
		t.Fatalf("the second run read %q, want %q: the first run edited the declaration", got, "c-1")
	}
}

// Bind answers with the values the graph reads and leaves what it was handed alone.
func TestBindDoesNotTouchWhatItWasGiven(t *testing.T) {
	supplied := map[string]any{"orders": map[string]any{"id": "o-1"}}
	declared := map[string]Input{
		"orders":    {Required: true},
		"customers": {Default: []any{}},
	}

	bound, err := Bind(declared, supplied)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if len(supplied) != 1 {
		t.Fatalf("what the trigger supplied now holds %d inputs, want 1", len(supplied))
	}
	if _, ok := supplied["customers"]; ok {
		t.Fatal("the default was written back into what the trigger supplied")
	}
	if _, ok := bound["customers"]; !ok {
		t.Fatal("the default is absent from the bound inputs")
	}
}
