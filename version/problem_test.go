package version

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/language"
)

// rulesDeclared are the rules the two packages declare, read from their sources, so that a rule
// added there is one this test knows of without a list kept beside it.
func rulesDeclared(t *testing.T) []graph.Rule {
	t.Helper()
	var rules []graph.Rule
	for _, file := range []string{"../graph/refusal.go", "tree.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok || len(spec.Values) != len(spec.Names) {
				return true
			}
			for i, name := range spec.Names {
				lit, ok := spec.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || !strings.HasPrefix(name.Name, "Rule") {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				rules = append(rules, graph.Rule(v))
			}
			return true
		})
	}
	if len(rules) < 50 {
		t.Fatalf("read %d rules out of the sources, which hold more: the reading is wrong", len(rules))
	}
	return rules
}

func TestEveryRuleSaysWhatItExpects(t *testing.T) {
	for _, rule := range rulesDeclared(t) {
		if expected[rule] == "" {
			t.Errorf("the rule %s says nothing of what it expects at the node it refuses", rule)
		}
	}
}

func TestEveryTopicARuleIsPlacedByIsOne(t *testing.T) {
	names := map[string]bool{}
	for _, n := range language.Names() {
		names[n] = true
	}
	for rule, topic := range topics {
		if !names[topic] {
			t.Errorf("the rule %s is placed by the topic %s, which the language does not have", rule, topic)
		}
	}
}

func TestARefusalIsExplainedWithItsNodeAndItsTopic(t *testing.T) {
	r := &graph.Refusal{Step: "load", Port: "rows", Rule: graph.RuleEdgePortNotDeclared, Detail: "fetch declares no port rows",
		At: graph.Position{File: "agentiik.yaml", Line: 13, Column: 30, Pointer: "/steps/load/needs/0/port"}}
	p, ok := Explain(r)
	if !ok {
		t.Fatal("a refusal is not explained")
	}
	want := Problem{File: "agentiik.yaml", Line: 13, Column: 30, Pointer: "/steps/load/needs/0/port", Rule: "edge-port-not-declared",
		Detail: "step load: port rows: fetch declares no port rows", Expected: expected[graph.RuleEdgePortNotDeclared], Topic: "needs"}
	if p != want {
		t.Errorf("the refusal is explained as %+v, and it is %+v", p, want)
	}
	lines := p.Lines()
	if len(lines) != 4 || lines[0] != "agentiik.yaml:13:30: edge-port-not-declared" || !strings.HasPrefix(lines[2], "at /steps/load/needs/0/port, expected ") || lines[3] != "read workflow.language, topic needs" {
		t.Errorf("the problem reads %q", lines)
	}

	// A refusal about the tree names no node, and is placed by its rule.
	p, _ = Explain(&graph.Refusal{Rule: RuleImageNotPinned, At: graph.Position{File: "agentiik.yaml"}})
	if p.Topic != "steps" || p.Pointer != "" {
		t.Errorf("a refusal naming no node is explained as %+v", p)
	}
	if _, ok := Explain(errStore); ok {
		t.Error("an error that is no refusal is explained as one")
	}
}

var errStore = &storeError{}

type storeError struct{}

func (*storeError) Error() string { return "the store could not be reached" }
