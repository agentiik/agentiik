package graph

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// "A workflow include reads the other repository's root agentiik.yaml, written as a fragment: a
// library repository." A root file is a library's where it writes none of what says what a
// document is and what it is called, and an entry point's where it writes any of them, so that a
// workflow that forgot its apiVersion is refused as one rather than taken for a library.
func TestARootFileIsALibrarysWhenItSaysNothingOfWhatItIs(t *testing.T) {
	for doc, library := range map[string]bool{
		".api-brick:\n  timeout: 2m\n":                                       true,
		"steps:\n  lint:\n    image: alpine:3.21\n    script: [\"true\"]\n":  true,
		"include:\n  - path: ./more.yaml\n":                                  true,
		"apiVersion: agentiik.dev/v1\nkind: Workflow\nmetadata: {name: x}\n": false,
		"kind: Workflow\nsteps: {}\n":                                        false,
		"metadata: {name: x}\n":                                              false,
		"apiVersion: agentiik.dev/v1\n":                                      false,
		"- not a mapping\n":                                                  false,
		"":                                                                   false,
	} {
		if got := IsLibrary([]byte(doc)); got != library {
			t.Errorf("IsLibrary(%q) is %v", doc, got)
		}
	}
}

// "Which its own hook validates as a fragment and nothing runs": the root file read on a fragment's
// terms, what it includes resolved, and what was included answered in the order it applied. A
// step of it extending a block nothing in the library declares is left for the workflow including
// it, which may declare the block.
func TestALibraryIsValidatedAsAFragment(t *testing.T) {
	remote := repositories{
		{Namespace: "platform", Name: "base", Ref: "v1"}: {"agentiik.yaml": ".base:\n  timeout: 5m\n"},
	}
	included, err := LoadLibrary(tree(map[string]string{
		"agentiik.yaml": `
include:
  - path: ./blocks/api.yaml
  - workflow: platform/base
    ref: v1
steps:
  lint:
    extends: .declared-by-whoever-includes-this
    image: alpine:3.21
    script: ["true"]
`,
		"blocks/api.yaml": "include:\n  - path: ../shared.yaml\n.api-brick:\n  timeout: 2m\n",
		"shared.yaml":     ".shared:\n  network: egress\n",
	}), remote)
	if err != nil {
		t.Fatal(err)
	}
	want := []Included{{Path: "shared.yaml"}, {Path: "blocks/api.yaml"}, {Workflow: WorkflowRef{Namespace: "platform", Name: "base", Ref: "v1"}, Commit: libraryCommit}}
	if !slices.Equal(included, want) {
		t.Errorf("the library included %+v", included)
	}
}

// What a fragment refuses, a library's root refuses, and an include of it that leaves the tree,
// names nothing or comes back to itself is refused as anywhere else.
func TestALibraryIsRefusedWhereAFragmentIs(t *testing.T) {
	for name, c := range map[string]struct {
		files map[string]string
		rule  Rule
		says  string
	}{
		"its inputs": {
			files: map[string]string{"agentiik.yaml": "inputs:\n  orders: {schema: {type: array}}\n"},
			rule:  RuleInputsInIncludedFile,
		},
		"a file it lacks": {
			files: map[string]string{"agentiik.yaml": "include:\n  - path: ./missing.yaml\n"},
			rule:  RuleIncludeMissing,
		},
		"a file leaving it": {
			files: map[string]string{"agentiik.yaml": "include:\n  - path: ../outside.yaml\n"},
			rule:  RuleIncludeLeavesTree,
		},
		"its root again": {
			files: map[string]string{"agentiik.yaml": "include:\n  - path: ./a.yaml\n", "a.yaml": "include:\n  - path: /agentiik.yaml\n"},
			rule:  RuleIncludeCycle,
		},
		"a workflow include nothing reaches": {
			files: map[string]string{"agentiik.yaml": "include:\n  - workflow: platform/base\n    ref: v1\n"},
			says:  "nothing here reaches another repository",
		},
	} {
		_, err := LoadLibrary(tree(c.files), nil)
		var r *Refusal
		switch {
		case err == nil:
			t.Errorf("%s: the library was accepted", name)
		case c.rule != "" && (!errors.As(err, &r) || r.Rule != c.rule):
			t.Errorf("%s: refused %v, where %s was expected", name, err, c.rule)
		case c.says != "" && !strings.Contains(err.Error(), c.says):
			t.Errorf("%s: refused %v", name, err)
		}
	}
}
