package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/cmd/agk/internal/local"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/internal/diff"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// Issue #83, which is the sentence that closes v0.1.0:
//
//	"A multi-step workflow with a fan-out and a merge runs end to end on a laptop, and
//	running it again on the same inputs produces the same envelopes."
//
// It is a test and not a claim, so nothing here is faked. The workflow under
// testdata/milestone is run by the command line itself, run(ctx, Env, args), against the
// Docker daemon of this machine, twice, on one committed inputs file, and the envelopes the
// two runs handed back are compared member by member through internal/diff. The
// three bricks it runs are built here from plain Dockerfiles under testdata/milestone/bricks,
// which is the image a laptop meets: nothing was pushed anywhere and nothing is pulled but
// the base image. The helper mounted at /agk/bin/agk is built here too, for the architecture
// the daemon runs containers as, because the fourth step is a script step that uses it.
//
// The whole of it skips where there is no daemon, so the suite stays green in CI and this
// machine tests for real, exactly as driver/real_test.go does.
//
// # What the fixture is shaped to prove
//
// Four steps, and each one is in the sentence or in the task that frames it.
//
//	split    a brick fed from the workflow's own inputs, reading /agk/params.json and
//	         /agk/repo, publishing one item per order
//	charge   the fan-out: one container per item, so three containers and not one, each
//	         given the secret the command line supplied and attaching an artifact
//	total    the merge: one input port fed by two edges under wait_all, so the fan-in of
//	         the shards and the rejected stream arrive as one batch, with the artifacts
//	         the fan-out stored laid down beside the envelope they travelled in
//	archive  a script step that uses the static helper: agk items, agk emit and agk attach
//
// # Why the envelopes can be compared at all
//
// Every identity every step publishes is derived from the payload it came from. An item holds
// its own identity so that a shard, a merge and a replay can speak about it, and a brick that
// minted a fresh identifier on every pass would make the second run's envelopes different for
// a reason that says nothing about the engine. That is what --id is for in the helper and why
// the three bricks derive theirs, and it is why the comparison here holds item identities to
// being equal rather than holding them aside.
//
// The four declared outputs are compared, and two of them are views of the fan-out's own ports
// rather than of what the merge computed, so the items the three containers published reach the
// comparison by identity, with the length of the secret each was given and the digest of the
// receipt each attached. A proof that compared only the totals would pass a fan-out that minted
// a fresh identity per shard.
//
// What is held aside is diff.Default and nothing more: meta.run_id, meta.produced_at and the
// run segment of every artifact URI. Those three are facts about which run this was. Item
// identities, item data, counts, port names, file names, media types, sizes and every sha256
// are compared, which is the assertion the sentence is asking for: a run that produced
// different bytes would fail on a digest.

// The three bricks of the fixture, in build order, each an image this project builds and
// nobody published.
var milestoneBricks = []struct{ image, dir string }{
	{"agk-milestone-split:test", "split"},
	{"agk-milestone-charge:test", "charge"},
	{"agk-milestone-total:test", "total"},
}

// milestoneFixture is the tree the workflow sits in, inside the package's own testdata.
const milestoneFixture = "testdata/milestone"

// milestoneOutputs are the outputs the workflow declares, which are the envelopes the command
// hands back and the envelopes the two runs are compared on.
var milestoneOutputs = []agk.Port{"charged", "rejected", "invoiced", "archived"}

// milestoneSecret is the value supplied for the secret the fan-out mounts. The charge brick
// refuses a mount it can write to and fails on one that is not there, so a run that succeeded
// is the assertion that a local run mounts a secret exactly as a server run does: a file under
// /agk/secrets/, read-only, never an environment variable.
const milestoneSecret = "sk-milestone"

func TestAFanOutAndAMergeRunEndToEndAndASecondRunProducesTheSameEnvelopes(t *testing.T) {
	tree, helper := theMilestoneFixture(t)
	layout := local.Layout{Root: filepath.Join(tree, local.DefaultDir)}

	// The file the proof runs is a file the command line passes, ports and all, held to the
	// manifests of the three images that are about to run. A proof that ran a workflow
	// agk validate refuses would be proving something nobody can commit.
	code, valid, said := runner(t, tree, "validate")
	if code != exitSucceeded {
		t.Fatalf("agk validate leaves with %d on the fixture: %s%s", code, valid, said)
	}
	t.Logf("agk validate says:\n%s", valid)
	for _, want := range []string{"split-orders 0.1.0", "charge-order 0.1.0", "total-charges 0.1.0", "4 steps held to the manifests"} {
		if !strings.Contains(valid, want) {
			t.Errorf("the answer does not say %q", want)
		}
	}

	first := aMilestoneRun(t, tree, helper)
	objects := storedArtifacts(t, layout)
	second := aMilestoneRun(t, tree, helper)

	// Two runs and not one run read twice. The identifiers are minted per run, so this is
	// what says the second run happened at all.
	if first.run == second.run {
		t.Fatalf("both runs are %s: the second run is a run of its own", first.run)
	}

	// Each run is the shape the workflow describes: three containers in the fan-out, the
	// merge reading both upstream ports, the artifacts round-tripped through the store, and
	// the run labelled local on disk.
	for _, r := range []milestoneRun{first, second} {
		theShapeTheWorkflowDescribes(t, layout, r)
	}

	// And the sentence itself. The two maps are keyed by the name the workflow declares its
	// outputs under rather than by a port, which is what a difference then prints and what
	// the person reading it would look for in the file.
	found := diff.Envelopes(first.outputs, second.outputs, diff.Default)
	for _, d := range found {
		t.Errorf("%s", d)
	}
	if len(found) > 0 {
		t.Fatalf("the second run over the same inputs produced %d differences, and the milestone sentence says it produces the same envelopes", len(found))
	}
	t.Logf("%s and %s produced the same envelopes: %s", first.run, second.run, describeOutputs(second.outputs))

	// Identities are part of that comparison rather than held aside, which is the assertion
	// with the most behind it. A default that gave them up would make the sentence true of a
	// brick that mints a fresh identifier on every pass, and this is the line that would
	// have to be changed to weaken it.
	if diff.Default&diff.IgnoreItemIDs != 0 {
		t.Fatalf("the default holds item identities aside, and this proof rests on them being compared")
	}

	// Two runs over the same inputs write the same artifacts. The store is content addressed
	// and shared by every run of one working directory, so the second run added none: the
	// three receipts, the summary and the ledger are the same bytes twice.
	if after := storedArtifacts(t, layout); after != objects {
		t.Errorf("the store held %d artifacts after the first run and %d after the second: two runs over the same inputs produce the same artifacts, and the same artifact is one object", objects, after)
	}
	if objects == 0 {
		t.Errorf("the store holds no artifact, and both runs attach artifacts: a comparison with no digests in it is not the comparison this proves")
	}
	t.Logf("the object store holds %d artifacts after both runs", objects)
}

// milestoneRun is what one invocation of the command line handed back.
type milestoneRun struct {
	// run is read out of the envelopes rather than out of the report, because it is the
	// run the envelopes say they came from.
	run     agk.RunID
	outputs map[agk.Port]agk.Envelope
	out     string
	said    string
}

// aMilestoneRun runs the whole command line, the way a person types it.
//
// -o json, so that the answer is the envelopes on standard output and the report moves to
// standard error: this compares what a pipe would have carried, and not a reading of a
// directory. The inputs are one committed file, which is what "the same inputs" means here.
func aMilestoneRun(t *testing.T, tree, helper string) milestoneRun {
	t.Helper()
	code, out, said := runner(t, tree, "run", "--local",
		"--inputs", "inputs.json",
		"--secret", "billing_api="+milestoneSecret,
		"--helper", helper,
		"-o", "json")
	if code != exitSucceeded {
		t.Fatalf("the exit code is %d, want %d\n%s\n%s", code, exitSucceeded, out, said)
	}
	t.Logf("the run narrates:\n%s", said)

	// The report says what happened rather than that something did, and the container count
	// is where a fan-out shows in it: four steps and six containers is one for split, three
	// for the fan-out, one for the merge and one for the script step.
	for _, want := range []string{"finance/monthly-invoicing succeeded", "4 steps, 6 containers",
		"charged: 2 items", "rejected: 1 item", "invoiced: 1 item", "archived: 1 item"} {
		if !strings.Contains(said, want) {
			t.Errorf("the report is:\n%s\nand %q is missing from it", said, want)
		}
	}

	outputs := theEnvelopesOf(t, out)
	for _, name := range milestoneOutputs {
		if _, ok := outputs[name]; !ok {
			t.Fatalf("the output %s is not in the document: %s", name, out)
		}
	}
	if len(outputs) != len(milestoneOutputs) {
		t.Fatalf("the document carries %d outputs and the workflow declares %d: %s", len(outputs), len(milestoneOutputs), out)
	}
	return milestoneRun{run: outputs["invoiced"].Meta.RunID, outputs: outputs, out: out, said: said}
}

// theEnvelopesOf reads the document -o json writes: one object keyed by the name of each
// declared workflow output.
//
// Each envelope goes back through agk.Decode rather than through a plain unmarshal, so that
// what is compared is an envelope this module would accept and not a document that happens to
// have the right members.
func theEnvelopesOf(t *testing.T, document string) map[agk.Port]agk.Envelope {
	t.Helper()
	var named map[string]json.RawMessage
	if err := json.Unmarshal([]byte(document), &named); err != nil {
		t.Fatalf("standard output is not one object keyed by output name: %s\n%s", err, document)
	}
	out := make(map[agk.Port]agk.Envelope, len(named))
	for name, doc := range named {
		e, err := agk.Decode(bytes.NewReader(doc), agk.DefaultLimits())
		if err != nil {
			t.Fatalf("the envelope of the output %s: %s", name, err)
		}
		out[agk.Port(name)] = e
	}
	return out
}

// theShapeTheWorkflowDescribes holds one run to the graph it ran rather than only to the other
// run.
//
// Two runs that agree on the wrong thing would agree perfectly, so the facts the sentence
// rests on are each read off this run: three containers in the fan-out and not one, a merge
// that saw both upstream ports, artifacts that went into the store and came back out on the
// mount below, a script step that used the helper, and a run labelled local.
func theShapeTheWorkflowDescribes(t *testing.T, layout local.Layout, r milestoneRun) {
	t.Helper()

	// The fan-out ran one container per item, which is three logs named by the shard they
	// belong to. A run that quietly collapsed a fan-out to a single container would pass
	// everything else here.
	for i := 1; i <= 3; i++ {
		task := agk.NewTaskID(r.run, "charge", 1, agk.Shard{Index: i, Of: 3})
		path, err := layout.Log(task)
		if err != nil {
			t.Fatalf("naming the log of %s: %s", task, err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the log of shard %d of 3 is not at %s: %s", i, path, err)
		}
	}

	// The fan-out published its items under the identities the orders carry, concatenated in
	// shard order, which is what a fan-in is: three containers and one envelope. Each was given
	// the secret the command line supplied, at the length it supplied.
	charged := r.outputs["charged"]
	if len(charged.Items) != 2 {
		t.Fatalf("the output charged carries %d items, want the two orders that went through", len(charged.Items))
	}
	for i, want := range []string{"charge-a1", "charge-b2"} {
		if got := charged.Items[i].ID; got != want {
			t.Errorf("item %d of charged is %s, want %s: a fan-in concatenates the shards in order", i+1, got, want)
		}
		if got := fmt.Sprint(charged.Items[i].Data["signed_bytes"]); got != fmt.Sprint(len(milestoneSecret)) {
			t.Errorf("shard %d read %s bytes at /agk/secrets/billing_api, want the %d of the value the command line supplied", i+1, got, len(milestoneSecret))
		}
		if n := len(charged.Items[i].Files); n != 1 {
			t.Errorf("item %d of charged attaches %d files, want the receipt its container wrote", i+1, n)
		}
	}
	if refused := r.outputs["rejected"]; len(refused.Items) != 1 || refused.Items[0].ID != "reject-c3" {
		t.Errorf("the output rejected is %+v, want the one order the fan-out refused", refused)
	}

	// The merge: the two edges into charges arrived as one batch, so the totals are the
	// charges of the two orders that went through and the one that did not, and the three
	// receipts the fan-out attached were laid down beside the envelope the merge read.
	invoiced := r.outputs["invoiced"]
	if len(invoiced.Items) != 1 {
		t.Fatalf("the output invoiced carries %d items, want the one the merge publishes", len(invoiced.Items))
	}
	for member, want := range map[string]string{
		"charged_count":  "2",
		"charged_total":  "4600",
		"rejected_count": "1",
		"receipts_seen":  "3",
		"cycle":          "2026-01",
	} {
		if got := fmt.Sprint(invoiced.Items[0].Data[member]); got != want {
			t.Errorf("the invoice says %s is %s, want %s", member, got, want)
		}
	}
	if got := invoiced.Items[0].ID; got != "invoice-2026-01" {
		t.Errorf("the item is %s, want the identity the brick derived from the cycle", got)
	}
	if n := len(invoiced.Items[0].Files); n != 1 {
		t.Errorf("the invoice attaches %d files, want the summary the brick wrote", n)
	}

	// The script step used the helper mounted at /agk/bin/agk: agk emit wrote the port with
	// an identity derived from a field, and agk attach added the five-member entry.
	archived := r.outputs["archived"]
	if len(archived.Items) != 1 || archived.Items[0].ID != "archived-2026-01" {
		t.Fatalf("the output archived is %+v, want the one item agk emit --id published", archived)
	}
	if len(archived.Items[0].Files) != 1 || archived.Items[0].Files[0].Name != "ledger.txt" {
		t.Errorf("the archived item attaches %+v, want the ledger agk attach added", archived.Items[0].Files)
	}
	if got := archived.Items[0].Files[0].MediaType; got != "text/plain" {
		t.Errorf("the ledger is %s, want the media type the script named", got)
	}

	// The envelopes on disk are the envelopes the command handed back, because that is what
	// the report points a person at.
	for name, envelope := range r.outputs {
		path := filepath.Join(layout.Outputs(r.run), string(name)+".json")
		doc, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("the output envelope %s: %s", path, err)
		}
		onDisk, err := agk.Decode(bytes.NewReader(doc), agk.DefaultLimits())
		if err != nil {
			t.Fatalf("%s: %s", path, err)
		}
		if found := diff.Envelopes(map[agk.Port]agk.Envelope{name: envelope}, map[agk.Port]agk.Envelope{name: onDisk}, 0); len(found) > 0 {
			t.Errorf("%s is not what -o json wrote: %v", path, found)
		}
	}

	// And the run is labelled local, in the one file a history reads, so that no history
	// mistakes it for a server run.
	record, err := os.ReadFile(layout.RunFile(r.run))
	if err != nil {
		t.Fatalf("the run record: %s", err)
	}
	for _, want := range []string{`"local": true`, `"triggered_by": "local"`, `"state": "succeeded"`} {
		if !strings.Contains(string(record), want) {
			t.Errorf("the run record is:\n%s\nand %s is missing from it", record, want)
		}
	}
}

// theMilestoneFixture builds the three bricks and the helper, copies the tree, and ends the test
// through dockertest.Unavailable where this machine cannot run containers at all.
//
// The tree is a copy because a run writes its working directory beside the entry point, and
// the committed fixture is not a place to write into. Both runs share the one copy, which is
// what lets the object store be asked whether the second run wrote the same objects.
func theMilestoneFixture(t *testing.T) (tree, helper string) {
	t.Helper()
	if _, ok := dockertest.Socket(); !ok {
		dockertest.Unavailable(t, "no Docker daemon on this machine")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		dockertest.Unavailable(t, "no docker command to build the fixture bricks with")
	}
	daemon, err := driver.Probe(t.Context(), "")
	if err != nil {
		dockertest.Unavailable(t, "the daemon did not answer: %v", err)
	}
	t.Logf("the daemon at %s speaks API %s and runs %s/%s", daemon.Socket, daemon.APIVersion, daemon.OSType, daemon.Architecture)

	for _, brick := range milestoneBricks {
		// Built every time rather than only when the tag is absent: the Dockerfile and
		// the script beside it are part of this test, and an image left over from an
		// older fixture would be a test passing against something nobody can read. The
		// layer cache makes it cost a moment.
		out, err := exec.Command("docker", "build", "-t", brick.image, filepath.Join(milestoneFixture, "bricks", brick.dir)).CombinedOutput()
		if err != nil {
			dockertest.Unavailable(t, "the fixture brick %s could not be built: %v\n%s", brick.image, err, out)
		}
	}

	helper = theStaticHelper(t, daemon.OSType, daemon.Architecture)

	tree = filepath.Join(t.TempDir(), "milestone")
	if err := copyTree(milestoneFixture, tree); err != nil {
		t.Fatalf("copying the fixture tree: %s", err)
	}
	return tree, helper
}

// theStaticHelper builds cmd/agk-helper for the architecture the daemon runs containers as.
//
// The daemon's architecture and never this host's: that is the architecture a container runs
// natively, and an amd64 binary bound into an arm64 container gives a script an exec format
// error rather than a program. The flags are the release build's, so what is mounted here is
// what is shipped.
func theStaticHelper(t *testing.T, ostype, arch string) string {
	t.Helper()
	tool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no Go toolchain to build the static helper with")
	}
	goarch := ""
	switch arch {
	case "aarch64", "arm64":
		goarch = "arm64"
	case "x86_64", "amd64":
		goarch = "amd64"
	}
	if ostype != "linux" || goarch == "" {
		t.Skipf("the daemon runs %s/%s containers, and the helper is built for the linux machines this knows the spelling of", ostype, arch)
	}

	path := filepath.Join(t.TempDir(), "agk-"+ostype+"-"+goarch)
	cmd := exec.Command(tool, "build", "-trimpath", "-ldflags=-s -w", "-o", path, "github.com/agentiik/agentiik/cmd/agk-helper")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+ostype, "GOARCH="+goarch)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the static helper for %s/%s: %s\n%s", ostype, goarch, err, out)
	}
	return path
}

// storedArtifacts counts the artifacts the object store holds, which is how many distinct
// artifacts both runs between them produced.
//
// The envelopes are in the store too, one per port each task published, since a runner writes
// its ports there before it writes a task's ending down, and they are left out of the count. Each
// names the run it came from, so every run adds its own whatever its inputs, and the sentence
// holds the two runs' envelopes to being the same by comparing them rather than by counting them.
func storedArtifacts(t *testing.T, layout local.Layout) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(layout.Objects(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if _, err := agk.Decode(bytes.NewReader(b), agk.DefaultLimits()); err != nil {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading the object store: %s", err)
	}
	return n
}

// describeOutputs is the one line the proof logs: what was compared, by name and by count.
func describeOutputs(outputs map[agk.Port]agk.Envelope) string {
	var said []string
	for _, name := range milestoneOutputs {
		e := outputs[name]
		files := 0
		for _, item := range e.Items {
			files += len(item.Files)
		}
		// counted, because the report this sits beside says one item and not 1 items.
		said = append(said, fmt.Sprintf("%s carries %s and %s", name, counted(len(e.Items), "item", "items"), counted(files, "artifact", "artifacts")))
	}
	return strings.Join(said, ", ")
}
