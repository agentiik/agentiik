package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// The command line against the daemon of this machine, for the two things a person actually
// does with it: run a workflow and read why one failed. It is skipped where there is no
// daemon, so that the suite stays green in CI and this machine tests for real.
//
// The milestone test is the other half of this file's subject and lives beside it: a fan-out
// and a merge twice over, with the envelopes compared. What is held here is the report, which
// that test has no failure to read.

// daemonOrSkip skips unless there is a daemon with the fixture's image on it.
func needsARealDaemon(t *testing.T) {
	t.Helper()
	socket, ok := dockertest.Socket()
	if !ok {
		t.Skip("no Docker daemon on this machine")
	}
	cli, err := docker.Dial(socket)
	if err != nil {
		t.Skipf("the daemon at %s did not answer: %v", socket, err)
	}
	defer cli.Close()
	if _, err := cli.ImageInspect(t.Context(), "alpine:3.21"); err != nil {
		t.Skip("alpine:3.21 is not on this machine: docker pull alpine:3.21")
	}
}

// tree writes one entry point into a directory of its own and answers with it.
func oneEntryPoint(t *testing.T, doc string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, entryPoint), []byte(doc), 0o644); err != nil {
		t.Fatalf("writing the entry point: %s", err)
	}
	return dir
}

func TestARealRunReportsASucceededWorkflowAndItsOutputs(t *testing.T) {
	needsARealDaemon(t)
	dir := oneEntryPoint(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: one, namespace: finance }
inputs:
  cycle: { schema: { type: string }, required: true }
outputs:
  billed: { from: { step: only, port: out } }
steps:
  only:
    image: alpine:3.21
    outputs: [out]
    script:
      - |
        printf '{"meta":{"run_id":"%s","step":"%s","port":"out","attempt":%s,"count":1,"produced_at":"2026-01-01T00:00:00Z"},"items":[{"id":"bill-1","data":{"cycle":"x"},"files":[]}]}' \
          "$AGK_RUN_ID" "$AGK_STEP" "$AGK_ATTEMPT" > /agk/out/ports/out.json
`)

	code, out, errs := runner(t, dir, "run", "--local", "--input", "cycle=2026-01")
	if code != exitSucceeded {
		t.Fatalf("the exit code is %d, want %d\n%s\n%s", code, exitSucceeded, out, errs)
	}
	// The report is the command's answer, so it is on standard output, and it names what
	// the run did rather than that it finished.
	for _, want := range []string{"finance/one succeeded", "billed: 1 item", "run "} {
		if !strings.Contains(out, want) {
			t.Errorf("the report is %q, and %s is missing", out, want)
		}
	}
	// The narration is on standard error, and the step is named there and not in the pipe.
	if !strings.Contains(errs, "only") {
		t.Errorf("the narration is %q, and it has to name the step", errs)
	}
	// And the envelope is on disk where the report says it is.
	outputs, err := filepath.Glob(filepath.Join(dir, ".agk", "runs", "*", "outputs", "billed.json"))
	if err != nil || len(outputs) != 1 {
		t.Fatalf("the output envelope is not where the layout names it: %v %v", outputs, err)
	}
	doc, err := os.ReadFile(outputs[0])
	if err != nil {
		t.Fatalf("reading it: %s", err)
	}
	if !strings.Contains(string(doc), "bill-1") {
		t.Errorf("the envelope is %s", doc)
	}
}

func TestARealRunWithJSONPutsTheEnvelopesOnStandardOutputAlone(t *testing.T) {
	needsARealDaemon(t)
	dir := oneEntryPoint(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: one, namespace: finance }
outputs:
  billed: { from: { step: only, port: out } }
steps:
  only:
    image: alpine:3.21
    outputs: [out]
    script:
      - |
        printf '{"meta":{"run_id":"%s","step":"%s","port":"out","attempt":%s,"count":1,"produced_at":"2026-01-01T00:00:00Z"},"items":[{"id":"bill-1","data":{"n":1},"files":[]}]}' \
          "$AGK_RUN_ID" "$AGK_STEP" "$AGK_ATTEMPT" > /agk/out/ports/out.json
`)

	code, out, errs := runner(t, dir, "run", "--local", "-o", "json")
	if code != exitSucceeded {
		t.Fatalf("the exit code is %d\n%s\n%s", code, out, errs)
	}
	// Standard output carries the answer and nothing else, which is what makes the pipe
	// into jq work: the report moved to standard error for this one.
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("standard output is %q, and -o json puts one object keyed by name on it", out)
	}
	if !strings.Contains(out, "bill-1") || !strings.Contains(out, "billed") {
		t.Errorf("the document is %q", out)
	}
	if !strings.Contains(errs, "succeeded") {
		t.Errorf("the report did not move to standard error: %q", errs)
	}
}

func TestARealFailureNamesTheStepTheExitCodeAndTheLog(t *testing.T) {
	needsARealDaemon(t)
	dir := oneEntryPoint(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: one, namespace: finance }
steps:
  only:
    image: alpine:3.21
    outputs: [out]
    script:
      - |
        echo "the last thing the container said" >&2
        exit 17
`)

	code, out, errs := runner(t, dir, "run", "--local")
	// It ran, and it did not succeed, which is exit 3 and not exit 1: nothing about the
	// workflow was refused.
	if code != exitNotSucceeded {
		t.Fatalf("the exit code is %d, want %d\n%s\n%s", code, exitNotSucceeded, out, errs)
	}
	if out != "" {
		t.Errorf("a failure reached standard output: %q", out)
	}
	for _, want := range []string{
		"finance/one failed",
		"step only",
		"exit code 17",
		"application failure",
		"the last thing the container said",
		".log",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("the report is:\n%s\nand %q is missing from it", errs, want)
		}
	}
	// The step, then the exit code, then what was refused, in that order, read off the
	// report's own line: the narration above it has already said the step failed, which is
	// what a narration is for, and the order is a rule about the report.
	report := ""
	for _, line := range strings.Split(errs, "\n") {
		if strings.HasPrefix(line, "step only") {
			report = line
			break
		}
	}
	if report == "" {
		t.Fatalf("there is no failure report in:\n%s", errs)
	}
	if step, exit := strings.Index(report, "step only"), strings.Index(report, "exit code 17"); step < 0 || exit < step {
		t.Errorf("the report reads %q, and it names the step before the exit code", report)
	}
}
