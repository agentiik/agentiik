package graph

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// tree is a repository tree as a run sees it: pinned to a commit, entry point at the
// root, everything else where the author put it.
func tree(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, content := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return fsys
}

func loaded(t *testing.T, files map[string]string, remote map[WorkflowRef]Fragment) *Workflow {
	t.Helper()
	wf, err := Load(tree(files), "agentiik.yaml", remote)
	if err != nil {
		t.Fatalf("this workflow was refused when loaded: %v", err)
	}
	return wf
}

// TestAPathIncludeResolvesInsideTheSameCommit holds what an include is for and where it
// comes from: "a path include resolves inside the same commit, so it can never be stale
// and nothing has to pin it".
func TestAPathIncludeResolvesInsideTheSameCommit(t *testing.T) {
	wf := loaded(t, map[string]string{
		"agentiik.yaml": `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: regional-vat
include:
  - path: ./common-bricks.yaml
steps:
  check-vat:
    extends: .api-brick
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    outputs: [out]
`,
		"common-bricks.yaml": `
.api-brick:
  timeout: 2m
  retry: { max: 3, on: [transient] }
  network: egress
  resources: { cpu: "0.5", memory: 256Mi }
`,
	}, nil)

	st := wf.Steps["check-vat"]
	if time.Duration(st.Timeout) != 2*time.Minute {
		t.Errorf("the timeout of the block was read as %s", st.Timeout)
	}
	if st.Network != NetworkEgress || st.Retry.Max != 3 || st.Resources.CPU != "0.5" {
		t.Errorf("the step resolved to %+v", st)
	}
}

// TestResolutionIsOrdered holds the sentence that fixes the order: "includes first, in
// declaration order, then extends depth-first, then defaults, then the step's own
// values". It is an order of application, and the last of them to write a keyword is the
// one that wins, which is what makes "then the step's own values" mean anything.
func TestResolutionIsOrdered(t *testing.T) {
	files := map[string]string{
		"agentiik.yaml": `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: ordering
include:
  - path: ./blocks.yaml
defaults:
  timeout: 10m
  network: internal
steps:
  own:
    extends: .inner
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    timeout: 30m
    outputs: [out]
  inherited:
    extends: .inner
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    outputs: [out]
`,
		"blocks.yaml": `
.outer:
  timeout: 1m
  runs_on: [zone=dmz]
.inner:
  extends: .outer
  timeout: 2m
`,
	}
	wf := loaded(t, files, nil)

	// The step's own value wins over everything.
	if got := wf.Steps["own"].Timeout; time.Duration(got) != 30*time.Minute {
		t.Errorf("the step's own timeout resolved to %s", got)
	}
	// Where the step says nothing, defaults does, and it is applied after the blocks.
	if got := wf.Steps["inherited"].Timeout; time.Duration(got) != 10*time.Minute {
		t.Errorf("the timeout resolved to %s, and defaults is applied after extends", got)
	}
	// Where neither says anything, the innermost block the step extends does, and
	// depth-first means the block it names is applied after the block that one names.
	if got := wf.Steps["inherited"].RunsOn; len(got) != 1 || got[0] != "zone=dmz" {
		t.Errorf("the outer block contributed %v", got)
	}
	if got := wf.Steps["inherited"].Network; got != NetworkInternal {
		t.Errorf("the network resolved to %s", got)
	}
}

// TestBeforeScriptIsMergedOutwardsIn holds the exception to the last writer winning:
// before_script is "commands prepended to script, merged from defaults and extends
// outwards in", so every layer contributes and the step's own commands are the innermost.
func TestBeforeScriptIsMergedOutwardsIn(t *testing.T) {
	wf := loaded(t, map[string]string{
		"agentiik.yaml": `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: ordering
include:
  - path: ./blocks.yaml
defaults:
  before_script: [from-defaults]
  after_script: [from-defaults]
steps:
  one:
    extends: .block
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    before_script: [from-step]
    after_script: [from-step]
    script: [echo one]
    outputs: [out]
`,
		"blocks.yaml": `
.block:
  before_script: [from-block]
  after_script: [from-block]
`,
	}, nil)

	st := wf.Steps["one"]
	if strings.Join(st.BeforeScript, ",") != "from-defaults,from-block,from-step" {
		t.Errorf("before_script merged to %v", st.BeforeScript)
	}
	if strings.Join(st.AfterScript, ",") != "from-defaults,from-block,from-step" {
		t.Errorf("after_script merged to %v", st.AfterScript)
	}
}

// TestAWorkflowIncludeArrivesAlreadyFetched holds the boundary: resolving one "reaches
// another repository at a tag or a commit, requires workflow:read on it", which is not
// something the evaluator does. It is handed the fragment or it refuses.
func TestAWorkflowIncludeArrivesAlreadyFetched(t *testing.T) {
	files := map[string]string{
		"agentiik.yaml": `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: dunning
include:
  - workflow: finance/common
    ref: v2.1.0
steps:
  one:
    extends: .shared
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    outputs: [out]
`,
	}
	if _, err := Load(tree(files), "agentiik.yaml", nil); err == nil {
		t.Fatal("a workflow include nobody resolved was passed over")
	}

	fragment, err := ParseFragment([]byte(".shared:\n  timeout: 45s\n"))
	if err != nil {
		t.Fatal(err)
	}
	ref := WorkflowRef{Namespace: "finance", Name: "common", Ref: "v2.1.0"}
	wf := loaded(t, files, map[WorkflowRef]Fragment{ref: *fragment})
	if got := wf.Steps["one"].Timeout; time.Duration(got) != 45*time.Second {
		t.Fatalf("the fragment contributed %s", got)
	}
}

// TestAnExtendsNamingNothingIsRefusedOnceEverythingIsIn is the difference between reading
// one document and loading a tree: Parse is given one file and leaves what an include may
// still carry, and Load has them all.
func TestAnExtendsNamingNothingIsRefusedOnceEverythingIsIn(t *testing.T) {
	const entry = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: regional-vat
include:
  - path: ./blocks.yaml
steps:
  one:
    extends: .nowhere
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    outputs: [out]
`
	if _, err := Parse([]byte(entry)); err != nil {
		t.Fatalf("a document declaring includes was refused for a block an include may carry: %v", err)
	}
	_, err := Load(tree(map[string]string{"agentiik.yaml": entry, "blocks.yaml": ".elsewhere:\n  timeout: 1m\n"}), "agentiik.yaml", nil)
	if err == nil {
		t.Fatal("an extends naming a block nothing declares was accepted once every include was in")
	}
	if !strings.Contains(err.Error(), ".nowhere") {
		t.Fatalf("the refusal does not name the block: %v", err)
	}

	// A file including nothing has everything it will ever have, so the same extends is
	// refused when it is read.
	if _, err := Parse([]byte(strings.Replace(entry, "include:\n  - path: ./blocks.yaml\n", "", 1))); err == nil {
		t.Fatal("a file that includes nothing was left with an extends naming nothing")
	}
}

// TestAnIncludeCannotLeaveTheTreeOrComeBackToItself holds the two things a path include
// may not do: read a file the commit does not carry, and loop.
func TestAnIncludeCannotLeaveTheTreeOrComeBackToItself(t *testing.T) {
	out := map[string]string{
		"agentiik.yaml": `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: escaping }
include:
  - path: ../elsewhere/blocks.yaml
steps:
  one:
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    outputs: [out]
`,
	}
	if _, err := Load(tree(out), "agentiik.yaml", nil); err == nil {
		t.Error("an include reaching outside the tree was resolved")
	}

	loop := map[string]string{
		"agentiik.yaml": `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: looping }
include:
  - path: ./blocks.yaml
steps:
  one:
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    outputs: [out]
`,
		"blocks.yaml": "include:\n  - path: ./blocks.yaml\n.a:\n  timeout: 1m\n",
	}
	if _, err := Load(tree(loop), "agentiik.yaml", nil); err == nil {
		t.Error("a file including itself was resolved")
	}
}

// TestAnMCPBlockInAnIncludedFileIsRefused holds the rule and its reason: "the published
// surface is declared by the workflow that publishes it, so that reading one file tells
// you everything that workflow exposes".
func TestAnMCPBlockInAnIncludedFileIsRefused(t *testing.T) {
	_, err := Load(tree(map[string]string{
		"agentiik.yaml": `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: invoicing }
include:
  - path: ./common-bricks.yaml
steps:
  one:
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    outputs: [out]
`,
		"common-bricks.yaml": `
.api-brick:
  timeout: 2m
mcp:
  name: common
  tools:
    - name: run_common
      description: Run the shared graph.
      input: { from: { input: orders } }
`,
	}), "agentiik.yaml", nil)

	var r *Refusal
	if !errors.As(err, &r) || r.Rule != RuleMCPInIncludedFile {
		t.Fatalf("the included file was refused by %v", err)
	}
}

// TestAFragmentIsNotAnEntryPoint holds the sentence: an included file "has no apiVersion,
// no kind and no metadata", and the boundary a workflow declares is declared by the
// workflow itself.
func TestAFragmentIsNotAnEntryPoint(t *testing.T) {
	for _, key := range []string{"apiVersion: agentiik.dev/v1", "kind: Workflow", "metadata: { name: x }", "inputs:\n  orders: {}", "outputs:\n  out:\n    from: { step: one, port: out }", "on:\n  schedule:\n    - cron: \"0 6 1 * *\"", "timeout: 4h", "concurrency: { group: x }"} {
		if _, err := ParseFragment([]byte(key + "\n")); err == nil {
			t.Errorf("a fragment declaring %s was read as a fragment", strings.SplitN(key, ":", 2)[0])
		}
	}
	if _, err := ParseFragment([]byte(".api-brick:\n  timeout: 2m\nvars:\n  currency: EUR\n")); err != nil {
		t.Errorf("a fragment carrying a hidden block and a variable was refused: %v", err)
	}
}

// TestAFragmentContributesStepsAndTheEntryPointWins holds what reuse is for: "reuse
// applies to steps and defaults", and the file that includes is the file that decides.
func TestAFragmentContributesStepsAndTheEntryPointWins(t *testing.T) {
	wf := loaded(t, map[string]string{
		"agentiik.yaml": `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: shared-graph }
include:
  - path: ./steps.yaml
steps:
  shared:
    timeout: 5m
`,
		"steps.yaml": `
steps:
  shared:
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    timeout: 1m
    outputs: [out]
`,
	}, nil)

	st, ok := wf.Steps["shared"]
	if !ok {
		t.Fatal("the step the include declared is not in the graph")
	}
	if st.Image == "" || len(st.Outputs) != 1 {
		t.Fatalf("the included step contributed %+v", st)
	}
	if time.Duration(st.Timeout) != 5*time.Minute {
		t.Fatalf("the entry point's own value resolved to %s", st.Timeout)
	}
}

// TestTwoFilesMayIncludeAThird is reuse working as it should. A file already included
// contributes what it contributes once, and reading it a second time would say nothing
// new; a file that comes back to itself is the other thing, and is refused.
func TestTwoFilesMayIncludeAThird(t *testing.T) {
	wf := loaded(t, map[string]string{
		"agentiik.yaml": `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: diamond }
include:
  - path: ./left.yaml
  - path: ./right.yaml
steps:
  one:
    extends: .shared
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    outputs: [out]
`,
		"left.yaml":   "include:\n  - path: ./shared.yaml\n",
		"right.yaml":  "include:\n  - path: ./shared.yaml\n",
		"shared.yaml": ".shared:\n  timeout: 90s\n",
	}, nil)

	if got := wf.Steps["one"].Timeout; time.Duration(got) != 90*time.Second {
		t.Fatalf("the shared block contributed %s", got)
	}
}

// TestAHiddenBlockSitsWhereverItIsWritten holds the sentence about where reuse may live:
// "a hidden block may be written at the root of the entry point as well as at the root of
// an included file, and a step under steps may be named with a leading dot. Settings one
// workflow uses twice do not need a second file to live in."
func TestAHiddenBlockSitsWhereverItIsWritten(t *testing.T) {
	wf := parsed(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: regional-vat }
.api-brick:
  timeout: 2m
  network: egress
steps:
  .shared-egress:
    extends: .api-brick
    egress:
      allow: ["vat.example.com:443"]
  check-vat:
    extends: .shared-egress
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    outputs: [out]
`)
	if _, ok := wf.Steps[".shared-egress"]; ok {
		t.Fatal("a hidden block under steps was read as a step, and a hidden block is never executed")
	}
	st := wf.Steps["check-vat"]
	if time.Duration(st.Timeout) != 2*time.Minute {
		t.Errorf("the block at the root contributed %s", st.Timeout)
	}
	if st.Network != NetworkEgress {
		t.Errorf("the network resolved to %s", st.Network)
	}
	if len(st.EgressAllow) != 1 || st.EgressAllow[0] != "vat.example.com:443" {
		t.Errorf("the block among the steps contributed %v", st.EgressAllow)
	}
}

// TestAFetchedFragmentResolvesItsOwnPathsWhereItWasWritten holds the boundary a pinned
// include exists to keep. A path include "resolves inside the same commit", and the
// commit a fetched fragment was written in is the other repository's: resolving one
// against the tree in hand would read a file of this repository in another's name, and
// what it read would change under a commit nobody pinned.
func TestAFetchedFragmentResolvesItsOwnPathsWhereItWasWritten(t *testing.T) {
	files := map[string]string{
		"agentiik.yaml": `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: dunning
include:
  - workflow: finance/common
    ref: v2.1.0
steps:
  one:
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    outputs: [out]
`,
		// The file the fetched fragment would have reached for if its own paths were
		// resolved here. It belongs to this repository and says so.
		"common-bricks.yaml": ".shared:\n  timeout: 45s\n",
	}

	fragment, err := ParseFragment([]byte("include:\n  - path: ./common-bricks.yaml\n"))
	if err != nil {
		t.Fatal(err)
	}
	ref := WorkflowRef{Namespace: "finance", Name: "common", Ref: "v2.1.0"}
	_, err = Load(tree(files), "agentiik.yaml", map[WorkflowRef]Fragment{ref: *fragment})
	if err == nil {
		t.Fatal("a fetched fragment resolved a path include against this repository's tree")
	}
	if !strings.Contains(err.Error(), "common-bricks.yaml") {
		t.Fatalf("the refusal does not name the path it would have read: %v", err)
	}
}
