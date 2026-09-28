package graph

import (
	"errors"
	"fmt"
	"io/fs"
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

// repositories are the other repositories a workflow include reaches, each ref with its tree,
// every one answered at the same commit.
type repositories map[WorkflowRef]map[string]string

const libraryCommit = "c7972389ef8c63fa88ce6c3893d8c75ba8bbb7b3"

func (r repositories) Include(ref WorkflowRef) (fs.FS, string, error) {
	files, ok := r[ref]
	if !ok {
		return nil, "", fmt.Errorf("no repository %s", ref.text())
	}
	return tree(files), libraryCommit, nil
}

func loaded(t *testing.T, files map[string]string, remote Remote) *Workflow {
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

// TestAWorkflowIncludeArrivesThroughWhatReachesRepositories holds the boundary: resolving one
// "reaches another repository at a tag or a commit, requires workflow:read on it", which is not
// something the evaluator does. It is handed the other repository's tree or it refuses, and it
// reads that tree's root agentiik.yaml as a fragment.
func TestAWorkflowIncludeArrivesThroughWhatReachesRepositories(t *testing.T) {
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

	ref := WorkflowRef{Namespace: "finance", Name: "common", Ref: "v2.1.0"}
	wf := loaded(t, files, repositories{ref: {"agentiik.yaml": ".shared:\n  timeout: 45s\n"}})
	if got := wf.Steps["one"].Timeout; time.Duration(got) != 45*time.Second {
		t.Fatalf("the fragment contributed %s", got)
	}
	if got := wf.Included(); len(got) != 1 || got[0].Workflow != ref || got[0].Commit != libraryCommit {
		t.Fatalf("the include was recorded as %+v, and a workflow include is recorded with the commit its ref resolved to", got)
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

// TestAnIncludedFileAndItsIncluderNameSecretsTogether holds what a name is: a secret the
// workflow uses, and not a value one file overrides in another. Where it lives is the
// namespace's declaration, so a name written in both files is one secret and nothing collides.
func TestAnIncludedFileAndItsIncluderNameSecretsTogether(t *testing.T) {
	wf := loaded(t, map[string]string{
		"agentiik.yaml": `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: shared-secrets }
include:
  - path: ./common.yaml
secrets: [ledger, billing]
steps:
  invoice:
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    secrets: [billing, bearer, ledger]
    outputs: [out]
`,
		"common.yaml": `
secrets: [billing, bearer]
`,
	}, nil)

	if got := strings.Join(wf.Secrets, ","); got != "billing,bearer,ledger" {
		t.Fatalf("the two files named %s, want each secret once in the order it was first named", got)
	}
	if err := Check(wf); err != nil {
		t.Fatalf("a step mounting secrets named across the two files was refused: %v", err)
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
// what it read would change under a commit nobody pinned. So the library's own path
// includes resolve in the library, and a file it lacks is refused in its name however
// many files of that name this repository holds.
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

	ref := WorkflowRef{Namespace: "finance", Name: "common", Ref: "v2.1.0"}
	library := map[string]string{"agentiik.yaml": "include:\n  - path: ./common-bricks.yaml\n"}
	_, err := Load(tree(files), "agentiik.yaml", repositories{ref: library})
	var r *Refusal
	if !errors.As(err, &r) || r.Rule != RuleIncludeMissing {
		t.Fatalf("a fetched fragment resolved a path include against this repository's tree: %v", err)
	}
	if r.At.File != "finance/common@v2.1.0:agentiik.yaml" || r.At.Line != 2 {
		t.Fatalf("the refusal is placed at %s, and the include is written on line 2 of the library's root file", r.At)
	}

	library["common-bricks.yaml"] = ".shared:\n  timeout: 30s\n"
	files["agentiik.yaml"] = strings.Replace(files["agentiik.yaml"], "    outputs: [out]", "    extends: .shared\n    outputs: [out]", 1)
	wf := loaded(t, files, repositories{ref: library})
	if got := wf.Steps["one"].Timeout; time.Duration(got) != 30*time.Second {
		t.Fatalf("the library's own include contributed %s, and it is the library's file that is read", got)
	}
	if got := wf.Included(); len(got) != 1 || got[0].Path != "" {
		t.Fatalf("the includes applied were recorded as %+v: a path include of another repository is that repository's", got)
	}
}

// A workflow include is remembered apart from every file of this tree, a file whose path spells
// the include included: each is applied, whichever comes first.
func TestAWorkflowIncludeIsNotAFileOfTheTreeSpelledTheSame(t *testing.T) {
	ref := WorkflowRef{Namespace: "finance", Name: "lib", Ref: "v1"}
	for _, order := range []string{
		"  - path: finance/lib@v1\n  - workflow: finance/lib\n    ref: v1\n",
		"  - workflow: finance/lib\n    ref: v1\n  - path: finance/lib@v1\n",
	} {
		wf := loaded(t, map[string]string{
			"agentiik.yaml": `apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: both }
include:
` + order + `steps:
  a:
    extends: .fromlib
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    script: [echo]
    outputs: [out]
  b:
    extends: .fromfile
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    script: [echo]
    outputs: [out]
`,
			"finance/lib@v1": ".fromfile:\n  timeout: 1m\n",
		}, repositories{ref: {"agentiik.yaml": ".fromlib:\n  timeout: 2m\n"}})
		if len(wf.Included()) != 2 {
			t.Errorf("including %q applied %+v", order, wf.Included())
		}
	}
}

// A library's root file is open while its own includes resolve, so that one of them including it
// again is refused as the ring it is, where that include is written in the library.
func TestALibraryIncludingItsRootAgainIsACycle(t *testing.T) {
	ref := WorkflowRef{Namespace: "finance", Name: "common", Ref: "v2.1.0"}
	_, err := Load(tree(map[string]string{
		"agentiik.yaml": `apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: ringed }
include:
  - workflow: finance/common
    ref: v2.1.0
steps:
  a:
    image: alpine:3.21@sha256:5f8b1e1ad4503f1abb00387333cc6ebac7c77193d6adf4c3917794e7102dc704
    script: [echo]
    outputs: [out]
`,
	}), "agentiik.yaml", repositories{ref: {
		"agentiik.yaml":   "include:\n  - path: ./blocks/one.yaml\n",
		"blocks/one.yaml": "include:\n  - path: ../agentiik.yaml\n",
	}})
	var r *Refusal
	if !errors.As(err, &r) || r.Rule != RuleIncludeCycle {
		t.Fatalf("the library including its root again was refused by %v", err)
	}
	if want := (Position{File: "finance/common@v2.1.0:blocks/one.yaml", Line: 2, Column: 11}); r.At != want {
		t.Errorf("the ring is refused at %s, and it is closed at %s", r.At, want)
	}
}
