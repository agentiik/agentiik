package graph

import (
	"bytes"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/agentiik/agentiik/agk"
)

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

// TestANameIsNoLongerThanADirectoryHoldsOne holds the other half of "one name survives a
// URL, a directory and a tool list unchanged": no filesystem holds a name longer than 255
// characters, so neither does the grammar.
func TestANameIsNoLongerThanADirectoryHoldsOne(t *testing.T) {
	longest := strings.Repeat("n", 255)
	if err := identifier(longest, "the input", "inputs"); err != nil {
		t.Errorf("a name of 255 characters was refused: %v", err)
	}
	err := identifier(longest+"n", "the input", "inputs")
	if err == nil {
		t.Fatal("a name of 256 characters was accepted")
	}
	// The bound, and where the name was written, and not the whole name printed back.
	if said := err.Error(); !strings.Contains(said, "at most 255") || !strings.Contains(said, "inputs") || strings.Contains(said, longest) {
		t.Errorf("the refusal reads %q", said)
	}
}

// named is a name of n characters, which is what the bounds below are about.
func named(prefix string, n int) string { return prefix + strings.Repeat("x", n-len(prefix)) }

// portPlaces are every place a workflow file names a port or a workflow output, each written
// with the name the test gives it.
func portPlaces(name string) map[string]string {
	withStep := func(lines string) string {
		return strings.Replace(minimal, "    outputs: [out]\n", "    outputs: [out]\n"+lines, 1)
	}
	return map[string]string{
		"a workflow output":                        strings.Replace(minimal, "steps:", "outputs:\n  "+name+":\n    from: { step: reconcile, port: out }\nsteps:", 1),
		"the port a workflow output is taken from": strings.Replace(minimal, "steps:", "outputs:\n  unmatched:\n    from: { step: reconcile, port: "+name+" }\nsteps:", 1),
		"a port a step declares":                   strings.Replace(minimal, "outputs: [out]", "outputs: [out, "+name+"]", 1),
		"a port a step feeds":                      withStep("    inputs:\n      " + name + ": 1\n"),
		"the port an edge takes":                   withStep("  report:\n    image: alpine:3.21\n    script: [\"true\"]\n    needs: [{ step: reconcile, port: " + name + " }]\n"),
		"the port an edge feeds":                   withStep("  report:\n    image: alpine:3.21\n    script: [\"true\"]\n    needs: [{ step: reconcile, as: " + name + " }]\n"),
		"a port a hidden block declares":           strings.Replace(minimal, "steps:", ".brick:\n  outputs: ["+name+"]\nsteps:", 1),
		"a tool's output": published(`    - name: create_invoice
      description: Issue one invoice.
      input: { from: { input: orders } }
      output: { from: { output: ` + name + ` } }`),
	}
}

// TestAPortIsNoLongerThanItsFileHoldsIt holds the Names table's bound where a version is made: "a
// port or a workflow output at most 250", since "a port or a workflow output [becomes] the file
// <name>.json, hence 250". It is held wherever the file names one, and the refusal says where and
// the bound, and not the whole name.
func TestAPortIsNoLongerThanItsFileHoldsIt(t *testing.T) {
	for place, doc := range portPlaces(named("p", 250)) {
		if _, err := Parse([]byte(doc)); err != nil {
			t.Errorf("%s of 250 characters was refused: %v", place, err)
		}
	}
	long := named("p", 251)
	for place, doc := range portPlaces(long) {
		_, err := Parse([]byte(doc))
		if err == nil {
			t.Errorf("%s of 251 characters was read without complaint", place)
			continue
		}
		if said := err.Error(); !strings.Contains(said, "at most 250") || !strings.Contains(said, "<name>.json") || strings.Contains(said, long) {
			t.Errorf("%s of 251 characters was refused with %q", place, said)
		}
	}
}

// TestAFragmentHoldsItsPortsToTheSameBound holds it in an included file too, which is where a
// hidden block is most often written.
func TestAFragmentHoldsItsPortsToTheSameBound(t *testing.T) {
	fragment := []byte(".brick:\n  outputs: [" + named("p", 251) + "]\nsteps:\n  report:\n    outputs: [" + named("q", 251) + "]\n")
	if _, err := ParseFragment(fragment); err == nil || !strings.Contains(err.Error(), "at most 250") {
		t.Errorf("a fragment naming a port of 251 characters was read, or refused with %v", err)
	}
	if _, err := parseFragment(fragment); err != nil {
		t.Errorf("the stored reading of that fragment refused it: %v", err)
	}
}

// TestAStoredVersionIsReadBackPastThePortBound holds the other half: a version recorded before the
// bound was, naming a port and an output of 251 to 255 characters in its entry point and in a file
// it includes, is loaded, checked and built by LoadStored as it was accepted, so that its runs, its
// replays and the rebuild of its graph go on after an upgrade. Load, which a new version is made
// with, refuses the same tree.
func TestAStoredVersionIsReadBackPastThePortBound(t *testing.T) {
	port, output := named("rejected-", 251), named("unmatched-", 255)
	files := tree(map[string]string{
		"agentiik.yaml": `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: nightly-reconciliation
include:
  - path: common.yaml
outputs:
  ` + output + `:
    from: { step: reconcile, port: ` + port + ` }
steps:
  reconcile:
    extends: .script
    outputs: [out, ` + port + `]
  report:
    extends: .script
    needs: [{ step: reconcile, port: ` + port + `, as: ` + port + ` }]
    outputs: [out]
`,
		"common.yaml": `
.script:
  image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
  script: ["true"]
  outputs: [` + named("spare-", 252) + `]
`,
	})

	if _, err := Load(files, "agentiik.yaml", nil); err == nil || !strings.Contains(err.Error(), "at most 250") {
		t.Fatalf("a new version naming a port of 251 characters was loaded, or refused with %v", err)
	}

	wf, err := LoadStored(files, "agentiik.yaml", nil)
	if err != nil {
		t.Fatalf("a stored version naming a port of 251 characters was refused when read back: %v", err)
	}
	if err := Check(wf); err != nil {
		t.Fatalf("it was refused when checked: %v", err)
	}
	g, err := Build(wf, nil)
	if err != nil {
		t.Fatalf("it was refused when built: %v", err)
	}
	if got := g.Consumers("reconcile", agk.Port(port)); len(got) != 1 || got[0] != "report" {
		t.Errorf("the edge on the port of 251 characters reaches %v", got)
	}
	if _, held := g.Workflow().Outputs[output]; !held {
		t.Errorf("the output of 255 characters is not among %d outputs", len(g.Workflow().Outputs))
	}

	// The grammar and the 255 characters no filesystem goes past are not relaxed for it.
	past := bytes.Replace(files["agentiik.yaml"].Data, []byte(output), []byte(output+"x"), 1)
	files["agentiik.yaml"] = &fstest.MapFile{Data: past}
	if _, err := LoadStored(files, "agentiik.yaml", nil); err == nil || !strings.Contains(err.Error(), "at most 255") {
		t.Errorf("a stored version naming an output of 256 characters was read back, or refused with %v", err)
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
