package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// The command line read the way somebody attacking it would read it, against the daemon of
// this machine. Each test here is a thing that went wrong once, written down so that it
// cannot go wrong again quietly.
//
// The subject is the one the driver cannot defend on its own. A task's directory is private
// to the runner account and a secret value is written inside it, and the driver's whole
// argument for that being safe is that "the writable leaf is reachable only through parents
// nobody but the runner can enter". A bind mount does not go through those parents. So the
// rule the driver relies on is a rule this package has to keep: whatever directory a task's
// files are prepared in must not be inside the tree that every container gets bound at
// /agk/repo.

// TestARealStepCannotReadAnotherStepsSecretThroughTheRepositoryMount is the adversary that
// found it.
//
// Two steps with no edge between them, so both run at once. One mounts the secret and sleeps
// holding it; the other is given no secret at all and reads everything it can reach under the
// repository mount. A step that was given no secret has nothing to mask, which is what makes
// this the worst case: the value it found would be written to its log, to its payload and to
// the terminal in the clear, because the masker of that task holds no values.
//
// The assertion is on the peeking step's own output, which is where a secret it found would
// land, and on the log of that container, which is what a person reads afterwards.
func TestARealStepCannotReadAnotherStepsSecretThroughTheRepositoryMount(t *testing.T) {
	needsARealDaemon(t)
	const secret = "sk-not-through-the-repository-mount"
	dir := oneEntryPoint(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: peeking, namespace: adversary }
secrets:
  billing_api: { provider: builtin, path: finance/billing-api }
outputs:
  seen: { from: { step: peeker, port: out } }
steps:

  holder:
    image: alpine:3.21
    secrets: [billing_api]
    outputs: [out]
    script:
      - |
        # The value is held mounted while the other step looks for it, and its length is
        # all that is ever printed.
        wc -c < /agk/secrets/billing_api
        sleep 6

  peeker:
    image: alpine:3.21
    outputs: [out]
    script:
      - |
        # Given no secret of its own, so nothing of what it prints is masked. It waits
        # until the other container is certainly up, then reads the repository mount.
        sleep 2
        find /agk/repo -type f 2>/dev/null | head -50
        for f in $(find /agk/repo -type f 2>/dev/null | head -50); do cat "$f" 2>/dev/null; done
`)

	code, out, errs := runner(t, dir, "run", "--local", "--secret", "billing_api="+secret, "--logs")
	if code != exitSucceeded {
		t.Fatalf("the exit code is %d, want %d\n%s\n%s", code, exitSucceeded, out, errs)
	}

	// Standard output is the report and standard error is everything said on the way to
	// it, the logs of every container included, so between them they are every byte this
	// command line produced.
	for what, said := range map[string]string{"the report": out, "the narration and the logs": errs} {
		if strings.Contains(said, secret) {
			t.Errorf("%s carries the secret value: a step that was given no secret read it through %s\n%s", what, "/agk/repo", said)
		}
	}

	// And on disk, which outlives the terminal: the logs, the state, the run record and
	// every output envelope.
	walk(t, filepath.Join(dir, ".agk"), func(path string, doc []byte) {
		if strings.Contains(string(doc), secret) {
			t.Errorf("%s carries the secret value", path)
		}
	})
}

// TestARealSecretAStepPublishedDoesNotReachTheTerminal is the same subject read from the other
// end: not a step that went looking for a value, but a step that was given one and published
// it.
//
// A step that puts the value it was given into a field of an item it publishes has that
// envelope written to the object store, to the state of the run, to the envelope on disk and to
// standard output. The log was masked; the payload was not, which made -o json the shortest
// route from /agk/secrets/<name> to a terminal. It is one literal match and it belongs on both,
// and this is that rule read through the whole command line rather than in the driver alone.
//
// What this does not claim is the bytes of an artifact. A brick that writes the value into a
// file it attaches has it in the store, and the envelope asserts that file's digest, which a
// consumer verifies: rewriting the bytes would refuse a downstream step for a reason nobody
// could find. The documentation answers that case already, masking being "a guard against
// accident, never against intent".
func TestARealSecretAStepPublishedDoesNotReachTheTerminal(t *testing.T) {
	needsARealDaemon(t)
	const secret = "sk-published-by-the-step-itself"
	dir := oneEntryPoint(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: publishes, namespace: adversary }
secrets:
  billing_api: { provider: builtin, path: finance/billing-api }
outputs:
  billed: { from: { step: only, port: out } }
steps:
  only:
    image: alpine:3.21
    secrets: [billing_api]
    outputs: [out]
    script:
      - |
        # The value as it arrived, in a field of an item, which is the case a literal
        # match is for.
        token=$(cat /agk/secrets/billing_api)
        printf '{"meta":{"run_id":"%s","step":"%s","port":"out","attempt":%s,"count":1,"produced_at":"2026-01-01T00:00:00Z"},"items":[{"id":"bill-1","data":{"token":"%s"},"files":[]}]}' \
          "$AGK_RUN_ID" "$AGK_STEP" "$AGK_ATTEMPT" "$token" > /agk/out/ports/out.json
`)

	code, out, errs := runner(t, dir, "run", "--local", "--secret", "billing_api="+secret, "-o", "json", "--logs")
	if code != exitSucceeded {
		t.Fatalf("the exit code is %d, want %d\n%s\n%s", code, exitSucceeded, out, errs)
	}

	// Standard output is the answer, which with -o json is the envelopes themselves, and
	// this is the stream that would have been piped into something else.
	if strings.Contains(out, secret) {
		t.Errorf("the envelopes on standard output carry the value in the clear:\n%s", out)
	}
	if !strings.Contains(out, "[masked]") {
		t.Errorf("nothing in the envelopes was masked, and the step published the value it was given:\n%s", out)
	}
	if strings.Contains(errs, secret) {
		t.Errorf("the narration, the report or a log carries the value in the clear:\n%s", errs)
	}

	// And the envelope on disk and the state beside it, which is what outlives the pipe.
	walk(t, filepath.Join(dir, ".agk", "runs"), func(path string, doc []byte) {
		if strings.Contains(string(doc), secret) {
			t.Errorf("%s carries the value in the clear", path)
		}
	})
}

// TestARealRunLeavesNoTaskDirectoryBehind holds the other half of the same rule, which is the
// half a person notices.
//
// A task's directory is "created fresh, owned by an unprivileged account, and removed with the
// container". The driver removes the task's own directory and the run and step directories
// above it are nobody else's to remove, so a laptop that ran one workflow a hundred times was
// left with a hundred empty skeletons inside the tree it ran from.
func TestARealRunLeavesNoTaskDirectoryBehind(t *testing.T) {
	needsARealDaemon(t)
	dir := oneEntryPoint(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: tidy, namespace: adversary }
outputs:
  out: { from: { step: only, port: out } }
steps:
  only:
    image: alpine:3.21
    outputs: [out]
    script:
      - echo tidy
`)

	code, out, errs := runner(t, dir, "run", "--local")
	if code != exitSucceeded {
		t.Fatalf("the exit code is %d, want %d\n%s\n%s", code, exitSucceeded, out, errs)
	}

	// Nothing of a task's preparation is left anywhere under the tree. The tree is what
	// gets bound at /agk/repo, so this is the same rule as the test above read from the
	// host side, and it holds whether or not a secret was involved.
	var left []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == "work" {
			left = append(left, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading the tree: %s", err)
	}
	if len(left) > 0 {
		t.Errorf("the tree still holds %v: a task's directory is prepared outside the tree, because the tree is bound into every container", left)
	}
}

// TestARealRunLeavesNoContainerAndNoNetworkBehind asks the daemon itself, which is the only
// witness that counts.
//
// A container left running holds the tree it was given bound into it, and on a laptop nothing
// ever reaps it: there is no controller to adopt it and no runner sweeping on startup, so it is
// there until somebody types docker rm. The same goes for the network each task gets of its own.
//
// The question is asked by this run's own label and not by counting what is on the machine.
// Counting is how this test would pass or fail on whatever else happens to be running, and a
// label is what the driver puts there for exactly this: "a startup sweep can tell a container
// of this driver's from everything else on the host".
//
// Four steps, because a fan-out is where the count of containers stops being one and a step
// that fails is where a cleanup path is most likely to be skipped.
func TestARealRunLeavesNoContainerAndNoNetworkBehind(t *testing.T) {
	needsARealDaemon(t)
	dir := oneEntryPoint(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: sweeping, namespace: adversary }
inputs:
  orders: { schema: { type: array }, required: true }
outputs:
  out: { from: { step: fan, port: out } }
steps:

  seed:
    image: alpine:3.21
    outputs: [out]
    params:
      orders: ${{ workflow.inputs.orders }}
    script:
      - |
        printf '{"meta":{"run_id":"%s","step":"%s","port":"out","attempt":%s,"count":3,"produced_at":"2026-01-01T00:00:00Z"},"items":[{"id":"a","data":{},"files":[]},{"id":"b","data":{},"files":[]},{"id":"c","data":{},"files":[]}]}' \
          "$AGK_RUN_ID" "$AGK_STEP" "$AGK_ATTEMPT" > /agk/out/ports/out.json

  fan:
    image: alpine:3.21
    needs:
      - { step: seed, port: out, as: in }
    strategy: { fan_out: item }
    outputs: [out]
    script:
      - |
        id=$(sed -n 's/.*{"id":"\([^"]*\)".*/\1/p' /agk/in/in/envelope.json)
        printf '{"meta":{"run_id":"%s","step":"%s","port":"out","attempt":%s,"count":1,"produced_at":"2026-01-01T00:00:00Z"},"items":[{"id":"%s","data":{},"files":[]}]}' \
          "$AGK_RUN_ID" "$AGK_STEP" "$AGK_ATTEMPT" "$id" > /agk/out/ports/out.json

  # A step that fails, under continue_on_error so the run still reaches succeeded and the
  # outputs are handed back: what is being asked is about containers, and a failed step is
  # where a cleanup is most easily skipped.
  breaks:
    image: alpine:3.21
    needs:
      - { step: seed, port: out, as: in }
    continue_on_error: true
    outputs: [out]
    script:
      - exit 9
`)

	code, out, errs := runner(t, dir, "run", "--local", "--input", `orders=["a","b","c"]`)
	if code != exitSucceeded {
		t.Fatalf("the exit code is %d, want %d\n%s\n%s", code, exitSucceeded, out, errs)
	}

	// The run the envelopes say they came from, read off the report rather than guessed at.
	run := runIDIn(t, out)

	socket, _ := dockertest.Socket()
	cli, err := docker.Dial(socket)
	if err != nil {
		t.Fatalf("dialling %s: %s", socket, err)
	}
	defer cli.Close()

	left, err := cli.ContainerList(t.Context(), docker.Filters{}.Add("label", driver.LabelRun+"="+string(run)))
	if err != nil {
		t.Fatalf("asking the daemon what is left of run %s: %s", run, err)
	}
	for _, c := range left {
		t.Errorf("container %s is still there after run %s: %s, labelled %s", c.ID, run, c.State, c.Labels[driver.LabelTask])
	}

	nets, err := cli.NetworkList(t.Context(), docker.Filters{}.Add("label", driver.LabelRun+"="+string(run)))
	if err != nil {
		t.Fatalf("asking the daemon what networks are left of run %s: %s", run, err)
	}
	for _, n := range nets {
		t.Errorf("network %s is still there after run %s: a task gets a network of its own and it goes with the task", n.Name, run)
	}
}

// runIDIn reads the run identifier out of the report, which names it on its last line.
func runIDIn(t *testing.T, report string) agk.RunID {
	t.Helper()
	for _, line := range strings.Split(report, "\n") {
		if rest, ok := strings.CutPrefix(line, "run "); ok {
			id, _, _ := strings.Cut(rest, ":")
			return agk.RunID(id)
		}
	}
	t.Fatalf("the report names no run:\n%s", report)
	return ""
}

// walk reads every file under a directory and hands it over. A directory that is not there
// is not a failure: the assertion is about what is in the files, and a run that wrote none
// carries no secret in any.
func walk(t *testing.T, root string, each func(string, []byte)) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		doc, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		each(path, doc)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading %s: %s", root, err)
	}
}
