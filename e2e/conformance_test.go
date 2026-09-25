package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/diff"
)

// milestone is the fixture v0.1.0's closing test runs twice with agk run --local, which this one
// runs once there and once on the installation.
const milestone = "cmd/agk/testdata/milestone"

// milestoneBricks are its three bricks, by the image the fixture names each as.
var milestoneBricks = map[string]string{
	"agk-milestone-split:test":  "split",
	"agk-milestone-charge:test": "charge",
	"agk-milestone-total:test":  "total",
}

// milestoneScriptImage is the image its script step names.
const milestoneScriptImage = "alpine:3.21"

// milestoneOutputs are the outputs it declares, which are the envelopes compared.
var milestoneOutputs = []agk.Port{"charged", "rejected", "invoiced", "archived"}

// milestoneSecret is the value of billing_api on both sides, the value the fixture's own test
// supplies.
const milestoneSecret = "sk-milestone"

// milestoneWorkflow is the fixture's workflow as the installation takes it, from the fixture's
// own file rather than a copy of it, so that what is compared is the workflow v0.1.0's proof runs.
// Three things change and nothing else: each image is the one pushed to the registry, which
// both sides run, since agk run --local on this machine finds each under the same name; the
// namespace is the installation's; and every step runs on the runners' label, by a default, since
// a step with no runs_on of its own goes to a pool no runner joined.
func milestoneWorkflow(t *testing.T, module string, images map[string]string) string {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join(module, milestone, "agentiik.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(doc)
	replace := func(old, new string) {
		t.Helper()
		if n := strings.Count(text, old); n != 1 {
			t.Fatalf("the fixture writes %q %d times, and the test rewrites it where it is written once", old, n)
		}
		text = strings.Replace(text, old, new, 1)
	}
	for image, pushed := range images {
		replace("image: "+image+"\n", "image: "+pushed+"\n")
	}
	replace("\n  namespace: finance\n", "\n  namespace: "+Namespace+"\n")
	replace("\nsteps:\n", "\ndefaults:\n  runs_on: ["+Label+"]\n\nsteps:\n")
	return text
}

// Issue #163: one workflow run with agk run --local and again on a server produces the same
// envelopes.
//
// The workflow is v0.1.0's milestone fixture: a brick fed from the workflow's inputs, a fan-out of
// three containers each given a secret and attaching an artifact, a merge of two edges under
// wait_all, and a script step using the static helper. Its three bricks are built on this
// machine and pushed to the registry, and so is the script step's image. agk run --local runs it
// on this machine's daemon, with the secret on its command line and the helper the runner image
// carries. Then it is pushed to the installation the way agk push pushes it, through
// version.Capture with the digest the pushing daemon holds for each tag, and not by the agk binary,
// since this machine's daemon does not resolve the registry's name that agk push would pin each
// tag through. billing_api is declared builtin with the same value through the API, and the run is
// started there with the same inputs and runs on the two runners.
//
// Every declared output of the server run is then compared with the local run's, member by member
// through internal/diff under diff.Default, the rule agk brick test and the milestone proof use:
// meta.run_id, meta.produced_at and the run segment of every artifact URI are held aside, since
// they say which run this was, and item identities, item data, counts, port names, file names,
// media types, sizes and every sha256 are compared.
func TestOneWorkflowRunLocallyAndOnTheInstallationProducesTheSameEnvelopes(t *testing.T) {
	in := Stand(t)

	images := map[string]string{}
	manifests := map[string][]byte{}
	pins := map[string]string{}
	for image, dir := range milestoneBricks {
		tag, pinned := in.BrickAt(filepath.Join(milestone, "bricks", dir))
		manifest, err := os.ReadFile(filepath.Join(in.module, milestone, "bricks", dir, "brick.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		images[image], manifests[tag], pins[tag] = tag, manifest, pinned
	}
	tag, pinned := in.Image(milestoneScriptImage, "alpine")
	images[milestoneScriptImage], pins[tag] = tag, pinned
	document := milestoneWorkflow(t, in.module, images)
	inputs, err := os.ReadFile(filepath.Join(in.module, milestone, "inputs.json"))
	if err != nil {
		t.Fatal(err)
	}

	local, localRun := in.runLocally(document, inputs)

	// The same workflow on the installation, with the same inputs and the same secret.
	secret := milestoneSecret
	in.Operator("PUT", "/api/v1/"+Namespace+"/secrets/billing_api", api.Declare{Provider: "builtin", Value: &secret}, http.StatusCreated, nil)
	commit := randomHex(20)
	in.Push("monthly-invoicing", commit, document, manifests, pins)
	var started map[string]any
	if err := json.Unmarshal(inputs, &started); err != nil {
		t.Fatal(err)
	}
	run := in.Start("monthly-invoicing", commit, started)
	ended := in.Wait(run, 5*time.Minute)
	if ended.State != "succeeded" {
		t.Fatalf("run %s ended %s on the installation: %s\nits steps' reasons: %q", run, ended.State, ended.Answer, ended.Reasons)
	}
	server := map[agk.Port]agk.Envelope{}
	for _, name := range milestoneOutputs {
		var raw json.RawMessage
		in.Operator("GET", "/api/v1/runs/"+run+"/outputs/"+string(name), nil, http.StatusOK, &raw)
		e, err := agk.Decode(bytes.NewReader(raw), agk.DefaultLimits())
		if err != nil {
			t.Fatalf("the output %s of run %s: %s", name, run, err)
		}
		server[name] = e
	}

	// Two runs and not one read twice, and envelopes with something in them to compare: the
	// fan-out's two charges and one rejection, the merge's invoice and the script step's
	// archive, each attaching what its step attached.
	if agk.RunID(run) == localRun {
		t.Fatalf("both runs are %s", run)
	}
	for name, want := range map[agk.Port]int{"charged": 2, "rejected": 1, "invoiced": 1, "archived": 1} {
		if got := len(local[name].Items); got != want {
			t.Fatalf("the local run's %s carries %d items, want %d: a comparison of what is not there compares nothing", name, got, want)
		}
		for _, item := range local[name].Items {
			if len(item.Files) != 1 {
				t.Fatalf("the local run's %s item %s attaches %d files, want the one its step attached", name, item.ID, len(item.Files))
			}
		}
	}

	found := diff.Envelopes(local, server, diff.Default)
	for _, d := range found {
		t.Errorf("%s", d)
	}
	if len(found) > 0 {
		t.Fatalf("run %s on the installation produced %d differences from run %s run locally, over the same workflow and the same inputs", run, len(found), localRun)
	}
}

// runLocally runs document with agk run --local on this machine's daemon, from a directory of its
// own holding it and inputs, and answers the envelope of every declared output and the run they
// came from. The secret is on the command line and the helper is the one the runner image
// carries, so that nothing but the place differs from the server run.
func (in *Installation) runLocally(document string, inputs []byte) (map[agk.Port]agk.Envelope, agk.RunID) {
	in.t.Helper()
	tree := in.mkdir(0o755, "milestone")
	for name, content := range map[string][]byte{"agentiik.yaml": []byte(document), "inputs.json": inputs} {
		if err := os.WriteFile(filepath.Join(tree, name), content, 0o644); err != nil {
			in.t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(in.ctx, filepath.Join(in.bin, "agk"), "run", "--local",
		"--inputs", "inputs.json",
		"--secret", "billing_api="+milestoneSecret,
		"--helper", in.helper,
		"-o", "json")
	cmd.Dir = tree
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		in.t.Fatalf("agk run --local: %s\n%s\n%s", err, stdout.String(), stderr.String())
	}
	in.t.Logf("agk run --local says:\n%s", stderr.String())

	var named map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &named); err != nil {
		in.t.Fatalf("agk run --local -o json wrote what is not one object keyed by output name: %s\n%s", err, stdout.String())
	}
	out := map[agk.Port]agk.Envelope{}
	for name, doc := range named {
		e, err := agk.Decode(bytes.NewReader(doc), agk.DefaultLimits())
		if err != nil {
			in.t.Fatalf("the local run's output %s: %s", name, err)
		}
		out[agk.Port(name)] = e
	}
	if len(out) != len(milestoneOutputs) {
		in.t.Fatalf("agk run --local answered %d outputs, and the workflow declares %d: %s", len(out), len(milestoneOutputs), stdout.String())
	}
	return out, out["invoiced"].Meta.RunID
}

func TestTheMilestoneRewrittenForTheInstallationChangesItsImagesNamespaceAndPoolAndNothingElse(t *testing.T) {
	module, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	images := map[string]string{milestoneScriptImage: "registry:5000/agk-e2e/alpine:test"}
	for image, dir := range milestoneBricks {
		images[image] = "registry:5000/agk-e2e/" + dir + ":test"
	}
	load := func(doc string) *graph.Workflow {
		t.Helper()
		wf, err := graph.Load(fstest.MapFS{"agentiik.yaml": &fstest.MapFile{Data: []byte(doc)}}, "agentiik.yaml", nil)
		if err != nil {
			t.Fatal(err)
		}
		return wf
	}
	original, err := os.ReadFile(filepath.Join(module, milestone, "agentiik.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	was, is := load(string(original)), load(milestoneWorkflow(t, module, images))

	if is.Metadata.Namespace != Namespace || is.Metadata.Name != was.Metadata.Name {
		t.Errorf("the workflow is %s/%s, want %s/%s", is.Metadata.Namespace, is.Metadata.Name, Namespace, was.Metadata.Name)
	}
	if len(is.Steps) != len(was.Steps) {
		t.Fatalf("the workflow has %d steps, and the fixture %d", len(is.Steps), len(was.Steps))
	}
	for name, step := range is.Steps {
		before := was.Steps[name]
		if step.Image != images[before.Image] {
			t.Errorf("step %s names %s, want %s pushed as %s", name, step.Image, before.Image, images[before.Image])
		}
		if len(step.RunsOn) != 1 || step.RunsOn[0] != Label {
			t.Errorf("step %s runs on %q, want the runners' label %s", name, step.RunsOn, Label)
		}
		// And the step is otherwise the fixture's, keyword for keyword.
		before.Image, before.RunsOn = step.Image, step.RunsOn
		if !reflect.DeepEqual(step, before) {
			t.Errorf("step %s is %+v, and the fixture's is %+v once its image and pool are rewritten", name, step, before)
		}
	}
	for _, same := range []struct {
		what    string
		was, is any
	}{
		{"inputs", was.Inputs, is.Inputs},
		{"outputs", was.Outputs, is.Outputs},
		{"vars", was.Vars, is.Vars},
		{"secrets", was.Secrets, is.Secrets},
	} {
		if !reflect.DeepEqual(same.was, same.is) {
			t.Errorf("the workflow's %s are %+v, and the fixture's %+v", same.what, same.is, same.was)
		}
	}
}
