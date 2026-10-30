package version

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/fixtures"
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
	if len(lines) != 4 || lines[0] != "agentiik.yaml:13:30: edge-port-not-declared" || !strings.HasPrefix(lines[2], "at /steps/load/needs/0/port, expected ") || lines[3] != "read workflow_language, topic needs" {
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

// TestEveryRefusalTheCorpusPinsIsPlaced walks every document the released corpus refuses, the
// entry points and the included files, through the reader and the checks a document alone reaches,
// and fails on a refusal Explain cannot place: one the hook would answer with no pointer, no
// expectation and no topic. It reaches what the corpus pins and nothing past it, so a refusal the
// corpus has no fixture for, as a blank description had none at the commit vendored, is held by a
// test of its own.
func TestEveryRefusalTheCorpusPinsIsPlaced(t *testing.T) {
	walked := 0
	for _, group := range []struct {
		cases func() ([]fixtures.Case, error)
		read  func(doc []byte) error
		name  func(file string) string
	}{
		{fixtures.Workflows, func(doc []byte) error {
			wf, err := graph.Parse(doc)
			if err != nil {
				return err
			}
			return graph.Check(wf)
		}, func(string) string { return "agentiik.yaml" }},
		{fixtures.Fragments, func(doc []byte) error {
			_, err := graph.ParseFragment(doc)
			return err
		}, func(file string) string { return "fragments/" + path.Base(file) }},
	} {
		cases, err := group.cases()
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			if c.Valid {
				continue
			}
			doc, err := fs.ReadFile(fixtures.FS, c.File)
			if err != nil {
				t.Fatal(err)
			}
			err = group.read(doc)
			if err == nil {
				// Refused past what one document reaches, by the brick manifest it is checked
				// against, which TestTheWorkflowCorpus holds to its rule.
				continue
			}
			walked++
			p, ok := Explain(placeShape(err, []readFile{{name: group.name(c.File), doc: doc}}, "agentiik.yaml"))
			if !ok || p.Expected == "" || p.Topic == "" {
				t.Errorf("%s is refused by %v, which is placed as %+v", c.File, err, p)
			}
		}
	}
	if walked < 80 {
		t.Fatalf("walked %d refusals, and the corpus pins more: the reading is wrong", walked)
	}
}
