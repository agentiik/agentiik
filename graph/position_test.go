package graph

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/brick"
)

// A refusal names the line of the value it refuses in the file that wrote it: a step's image may
// come from a block in an included file and its outputs from the entry point, and a person given a
// rule and a step still has to find which of the files wrote the value.
func TestARefusalIsPlacedInTheFileThatWroteTheValue(t *testing.T) {
	files := map[string]string{
		"agentiik.yaml": `apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: placed }
include:
  - path: fragments/bricks.yaml
steps:
  fetch:
    extends: .fetcher
    outputs: [out]
  load:
    extends: .loader
    needs:
      - { step: fetch, port: rows, as: in }
    outputs: [out]
`,
		"fragments/bricks.yaml": `.fetcher:
  image: ghcr.io/acme/agk-fetch@sha256:6a851b18304b7f94542f6b2c5d159ace56a5a8ab2faad1fa9ce7c63d14ad171f
.loader:
  extends: .base
.base:
  image: ghcr.io/acme/agk-load@sha256:0e355d4c7f028d2185b286e7d3ac914be0a588b222cb4f3a95fc00ec918ea9b1
`,
	}
	wf := loaded(t, files, nil)

	// The edge is the entry point's, and so is its port.
	err := Check(wf)
	var r *Refusal
	if !errors.As(err, &r) || r.Rule != RuleEdgePortNotDeclared {
		t.Fatalf("the edge was refused by %v", err)
	}
	if want := (Position{File: "agentiik.yaml", Line: 13, Column: 30}); r.At != want {
		t.Errorf("the edge's port is refused at %s, and it is written at %s", r.At, want)
	}

	// The image is a block's, two blocks away in the included file.
	files["agentiik.yaml"] = strings.Replace(files["agentiik.yaml"], "port: rows", "port: out", 1)
	wf = loaded(t, files, nil)
	_, err = Build(wf, map[string]brick.Manifest{})
	if !errors.As(err, &r) || r.Rule != RuleManifestMissing || r.Step != "fetch" {
		t.Fatalf("the brick with no manifest was refused by %v", err)
	}
	if want := (Position{File: "fragments/bricks.yaml", Line: 2, Column: 10}); r.At != want {
		t.Errorf("the image is refused at %s, and it is written at %s", r.At, want)
	}
	if got := wf.StepAt("load", "image"); got != (Position{File: "fragments/bricks.yaml", Line: 6, Column: 10}) {
		t.Errorf("the image of load is placed at %s, which is not where .base writes it", got)
	}
	if got := wf.StepAt("load", "needs", 0, "port"); got != (Position{File: "agentiik.yaml", Line: 13, Column: 30}) {
		t.Errorf("the edge of load is placed at %s", got)
	}
}

// A path include that leaves the tree, names nothing the commit holds, or comes back to a file
// still being resolved is refused by a rule of its own, placed at the include that does it: the
// last of a ring is the include that closes it, in the file that writes it.
func TestAnIncludeIsRefusedWhereItIsWritten(t *testing.T) {
	entry := `apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: including }
include:
  - path: %s
steps:
  one:
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    script: [echo]
    outputs: [out]
`
	for _, c := range []struct {
		include string
		files   map[string]string
		rule    Rule
		at      Position
	}{
		{"../elsewhere.yaml", nil, RuleIncludeLeavesTree, Position{"agentiik.yaml", 5, 11}},
		{"./common.yaml", map[string]string{"fragments/common.yaml": ".x:\n  timeout: 1m\n"}, RuleIncludeMissing, Position{"agentiik.yaml", 5, 11}},
		{"fragments/a.yaml", map[string]string{
			"fragments/a.yaml": "include:\n  - path: ./b.yaml\n",
			"fragments/b.yaml": "vars: { v: 1 }\ninclude:\n  - path: /fragments/a.yaml\n",
		}, RuleIncludeCycle, Position{"fragments/b.yaml", 3, 11}},
	} {
		files := map[string]string{"agentiik.yaml": strings.Replace(entry, "%s", c.include, 1)}
		for k, v := range c.files {
			files[k] = v
		}
		_, err := Load(tree(files), "agentiik.yaml", nil)
		var r *Refusal
		if !errors.As(err, &r) || r.Rule != c.rule {
			t.Errorf("including %s was refused by %v, and the rule is %s", c.include, err, c.rule)
			continue
		}
		if r.At != c.at {
			t.Errorf("including %s is refused at %s, and the include is written at %s", c.include, r.At, c.at)
		}
	}
}

// A step keeps every hidden block it inherits from, the one it names first and each block that
// one extends after it, so that a run detail can say where a setting came from.
func TestAStepKeepsItsWholeExtendsChain(t *testing.T) {
	wf := loaded(t, map[string]string{
		"agentiik.yaml": `apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: chained }
include:
  - path: blocks.yaml
.nearest:
  extends: .middle
steps:
  one:
    extends: .nearest
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    script: [echo]
    outputs: [out]
  two:
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    script: [echo]
    outputs: [out]
`,
		"blocks.yaml": ".middle:\n  extends: .outermost\n.outermost:\n  timeout: 1m\n",
	}, nil)
	if got := wf.Steps["one"].Extends; !slices.Equal(got, []string{".nearest", ".middle", ".outermost"}) {
		t.Errorf("one extends %v", got)
	}
	if got := wf.Steps["two"].Extends; got != nil {
		t.Errorf("two extends %v, and extends nothing", got)
	}
}

// A file is relocated to an absolute path, since "a relative path has nothing inside a container to
// be relative to". A version about to be made is refused one relative, in the entry point, a block
// or defaults of a file it includes; one already stored is read back as it was accepted.
func TestAFileRelocatedToARelativePathIsRefusedWhereAVersionIsMade(t *testing.T) {
	entry := `apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: relocating }
include:
  - path: common.yaml
steps:
  one:
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    script: [echo]
    outputs: [out]
`
	for where, files := range map[string]map[string]string{
		"a step of the entry point": {
			"agentiik.yaml": strings.Replace(entry, "    outputs: [out]", "    outputs: [out]\n    files:\n      - { from: ./certs/ca.pem, to: etc/ca.pem }", 1),
			"common.yaml":   "vars: { v: 1 }\n",
		},
		"defaults of an included file": {
			"agentiik.yaml": entry,
			"common.yaml":   "defaults:\n  files:\n    - { from: ./certs/ca.pem, to: etc/ca.pem }\n",
		},
		"a block of an included file": {
			"agentiik.yaml": entry,
			"common.yaml":   ".certs:\n  files:\n    - { from: ./certs/ca.pem, to: etc/ca.pem }\n",
		},
	} {
		if _, err := Load(tree(files), "agentiik.yaml", nil); err == nil || !strings.Contains(err.Error(), "absolute path") {
			t.Errorf("a file relocated to a relative path in %s was accepted, or refused with %v", where, err)
		}
		if _, err := LoadStored(tree(files), "agentiik.yaml", nil); err != nil {
			t.Errorf("a stored version relocating a file to a relative path in %s was refused when read back: %v", where, err)
		}
	}
}

// The resolved graph is written as the wire's resolvedGraph: every keyword where resolution leaves
// something other than the language's default, the script keywords on a script step alone, a
// call in its long form, and no namespace.
func TestTheResolvedGraphWritesEachKindOfStepAsTheWireDoes(t *testing.T) {
	wf := loaded(t, map[string]string{
		"agentiik.yaml": `apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: kinds, namespace: finance }
concurrency: { group: kinds, cancel_in_progress: false }
defaults:
  before_script: [echo defaults]
  when: [succeeded]
steps:
  split:
    image: ghcr.io/acme/agk-split@sha256:6c3e2a1f09d8b7c6e5f4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2
    outputs: [ok]
    cache: true
  report:
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    needs: [{ step: split, port: ok, as: in }, { step: check, port: out, as: in }]
    merge: zip
    when: [failed, succeeded]
    continue_on_error: true
    strategy: { fan_out: batch(50), max_parallel: 2 }
    files: [./sql/**, { from: ./certs/ca.pem, to: /etc/ssl/ca.pem, mode: "0444" }]
    script: [agk emit out]
    outputs: [out]
  check:
    workflow: finance/vat-check@v1.2.0
    needs: [{ step: split, port: ok, as: in }]
    idempotent: false
`,
	}, nil)
	m, err := brick.ParseManifest([]byte(`apiVersion: agentiik.dev/v1
kind: Brick
metadata: { name: split-orders, version: 0.1.0 }
spec:
  outputs: { ok: {} }
  runtime: { user: "65532:65532" }
`))
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(wf, map[string]brick.Manifest{wf.Steps["split"].Image: m})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := g.Resolved("a3f9c1e04b7d2e8f6a1c3b5d7e9f0a2b4c6d8e0f")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(doc, &got); err != nil {
		t.Fatal(err)
	}
	if _, held := got["namespace"]; held {
		t.Error("the record writes a namespace, and a namespace is the repository's")
	}
	if !reflect.DeepEqual(got["concurrency"], map[string]any{"group": "kinds", "cancel_in_progress": false}) {
		t.Errorf("concurrency is written %v, and it is written as the entry point writes it", got["concurrency"])
	}
	steps := got["steps"].(map[string]any)
	want := map[string]map[string]any{
		"split": {
			"kind": "brick", "image": wf.Steps["split"].Image,
			"brick":   map[string]any{"name": "split-orders", "version": "0.1.0"},
			"outputs": []any{"ok"}, "cache": true, "idempotent": true,
		},
		"report": {
			"kind": "script", "image": wf.Steps["report"].Image,
			"needs":             []any{map[string]any{"step": "split", "port": "ok", "as": "in"}, map[string]any{"step": "check", "port": "out", "as": "in"}},
			"merge":             "zip",
			"when":              []any{"failed", "succeeded"},
			"continue_on_error": true,
			"strategy":          map[string]any{"fan_out": "batch(50)", "max_parallel": float64(2)},
			"files":             []any{map[string]any{"from": "./sql/**"}, map[string]any{"from": "./certs/ca.pem", "to": "/etc/ssl/ca.pem", "mode": "0444"}},
			"before_script":     []any{"echo defaults"},
			"script":            []any{"agk emit out"},
			"shell":             []any{"/bin/sh", "-e"},
			"outputs":           []any{"out"},
			"idempotent":        true,
		},
		"check": {
			"kind": "workflow", "workflow": map[string]any{"workflow": "finance/vat-check", "ref": "v1.2.0"},
			"needs":      []any{map[string]any{"step": "split", "port": "ok", "as": "in"}},
			"idempotent": false,
		},
	}
	for name, w := range want {
		if !reflect.DeepEqual(steps[name], any(w)) {
			t.Errorf("%s is written\n%v\nand the wire writes it\n%v", name, steps[name], w)
		}
	}
	if !reflect.DeepEqual(got["order"], []any{"split", "check", "report"}) {
		t.Errorf("the order is %v", got["order"])
	}
}

// Parameters merge by name, so a parameter is placed in the layer that wrote it, whichever layer
// last wrote params; and a keyword a step takes from defaults is placed in the defaults block of
// the file that wrote it, an edge's input port at the edge.
func TestAValueTakenFromAnotherLayerIsPlacedWhereThatLayerWritesIt(t *testing.T) {
	entry := `apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: layered }
include:
  - path: fragments/base.yaml
secrets: [billing]
steps:
  fetch:
    image: ghcr.io/acme/agk-fetch@sha256:6a851b18304b7f94542f6b2c5d159ace56a5a8ab2faad1fa9ce7c63d14ad171f
    outputs: [out]
  load:
    extends: .base
    needs:
      - { step: fetch, port: out, as: rows }
    params:
      other: x
    outputs: [out]
`
	base := `.base:
  image: ghcr.io/acme/agk-load@sha256:0e355d4c7f028d2185b286e7d3ac914be0a588b222cb4f3a95fc00ec918ea9b1
  params:
    who: %s
`
	files := map[string]string{"agentiik.yaml": entry, "fragments/base.yaml": strings.Replace(base, "%s", "${{ item.name }}", 1)}
	err := Check(loaded(t, files, nil))
	var r *Refusal
	if !errors.As(err, &r) || r.Rule != RuleExpressionItemOutsideFanOutItem {
		t.Fatalf("the parameter reading item was refused by %v", err)
	}
	if want := (Position{File: "fragments/base.yaml", Line: 4, Column: 10}); r.At != want {
		t.Errorf("the parameter reading item is refused at %s, and it is written at %s", r.At, want)
	}

	files["fragments/base.yaml"] = strings.Replace(base, "%s", "nobody", 1)
	wf := loaded(t, files, nil)
	m, err := brick.ParseManifest([]byte(`apiVersion: agentiik.dev/v1
kind: Brick
metadata: { name: load, version: 1.0.0 }
spec:
  inputs: { in: {} }
  outputs: { out: {} }
  params:
    who: { type: string, enum: [somebody] }
    other: { type: string }
  runtime: { user: "65532:65532" }
`))
	if err != nil {
		t.Fatal(err)
	}
	manifests := map[string]brick.Manifest{wf.Steps["fetch"].Image: m, wf.Steps["load"].Image: m}
	_, err = Build(wf, manifests)
	if !errors.As(err, &r) || r.Rule != RuleStepInputPortNotInManifest {
		t.Fatalf("the edge's input port was refused by %v", err)
	}
	if want := (Position{File: "agentiik.yaml", Line: 14, Column: 39}); r.At != want {
		t.Errorf("the edge's input port is refused at %s, and it is written at %s", r.At, want)
	}

	files["agentiik.yaml"] = strings.Replace(entry, "as: rows", "as: in", 1)
	wf = loaded(t, files, nil)
	_, err = Build(wf, manifests)
	if !errors.As(err, &r) || r.Rule != RuleParamsAgainstManifest {
		t.Fatalf("the parameter was refused by %v", err)
	}
	if want := (Position{File: "fragments/base.yaml", Line: 4, Column: 10}); r.At != want {
		t.Errorf("the parameter is refused at %s, and it is written at %s", r.At, want)
	}

	files["fragments/base.yaml"] = "defaults:\n  secrets: [ledger]\n"
	files["agentiik.yaml"] = strings.Replace(entry, "    extends: .base\n", "", 1)
	err = Check(loaded(t, files, nil))
	if !errors.As(err, &r) || r.Rule != RuleSecretNotDeclared || r.Step != "fetch" {
		t.Fatalf("the secret defaults mount was refused by %v", err)
	}
	if want := (Position{File: "fragments/base.yaml", Line: 2, Column: 13}); r.At != want {
		t.Errorf("the secret defaults mount is refused at %s, and it is written at %s", r.At, want)
	}
}
