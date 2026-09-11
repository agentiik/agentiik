package graph

import "testing"

// TestOneGrammarGovernsEveryNameInTheFile holds the sentence that says so: a name is
// "letters, digits, hyphens and underscores, beginning with a letter or a digit", so that
// "one name survives a URL, a directory and a tool list unchanged".
func TestOneGrammarGovernsEveryNameInTheFile(t *testing.T) {
	for name, accepted := range map[string]bool{
		"orders":      true,
		"my-port":     true,
		"my_port":     true,
		"0rders":      true,
		"in":          true,
		"-orders":     false,
		"_orders":     false,
		"my port":     false,
		"my.port":     false,
		"out,error":   false,
		"":            false,
		"finance/x":   false,
		"créditeurs":  false,
		"HttpRequest": true,
	} {
		err := identifier(name, "the port", "steps.one")
		if accepted && err != nil {
			t.Errorf("%q was refused: %v", name, err)
		}
		if !accepted && err == nil {
			t.Errorf("%q was accepted", name)
		}
	}
}

// TestAParameterNameIsNarrower holds the exception: a parameter "is exported as
// AGK_PARAM_<NAME> to a script, so api-key is refused".
func TestAParameterNameIsNarrower(t *testing.T) {
	for name, accepted := range map[string]bool{
		"endpoint":   true,
		"_endpoint":  true,
		"endpoint_2": true,
		"api-key":    false,
		"2endpoint":  false,
		"end.point":  false,
	} {
		err := parameter(name, "steps.one.params")
		if accepted && err != nil {
			t.Errorf("%q was refused: %v", name, err)
		}
		if !accepted && err == nil {
			t.Errorf("%q was accepted", name)
		}
	}
}

// TestAWorkflowIsNamedByItsNamespaceAndItsName holds both halves of a reference to the
// one grammar, and the @ref the short form of a call appends.
func TestAWorkflowIsNamedByItsNamespaceAndItsName(t *testing.T) {
	r, err := parseWorkflowRef("finance/common@v2.1.0", "include[0]")
	if err != nil {
		t.Fatal(err)
	}
	if r.Namespace != "finance" || r.Name != "common" || r.Ref != "v2.1.0" {
		t.Fatalf("read %+v", r)
	}
	if got := r.text(); got != "finance/common@v2.1.0" {
		t.Fatalf("written back as %s", got)
	}

	for _, bad := range []string{"common", "finance/", "/common", "finance/common@", "fin ance/common"} {
		if _, err := parseWorkflowRef(bad, "include[0]"); err == nil {
			t.Errorf("%q was read as a workflow reference", bad)
		}
	}
}
