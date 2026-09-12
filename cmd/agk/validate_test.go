package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// reading is an Env for the commands that read: two buffers, the package directory, and a clock
// that does not move.
//
// It is the whole of what a test of this command line needs, which is the point of Env being a
// value: a test drives argv and reads bytes.
func reading(t *testing.T) (Env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	out, errs := &bytes.Buffer{}, &bytes.Buffer{}
	return Env{
		Out: out, Err: errs, Dir: dir,
		Now:    func() time.Time { return time.Date(2026, 1, 1, 6, 0, 0, 0, time.UTC) },
		Getenv: func(string) string { return "" },
	}, out, errs
}

func TestAWorkflowOfScriptStepsIsValidWithNoDaemonInReach(t *testing.T) {
	e, out, errs := reading(t)
	code := run(t.Context(), e, []string{"validate", "-f", "testdata/scripted/agentiik.yaml"})
	if code != exitSucceeded {
		t.Fatalf("the exit code is %d and the workflow is valid: %s%s", code, out, errs)
	}
	// A control names its effect: the workflow, the counts, and not that something was
	// validated.
	want := "finance/reading-commands is valid: 2 steps, 1 edge, 1 declared input, 1 declared output"
	if got := out.String(); !strings.Contains(got, want) {
		t.Errorf("the answer reads\n\t%s\nand it names what the workflow is made of:\n\t%s", strings.TrimSpace(got), want)
	}
	// graph.Images names the image of a non-script step, and a script step's image is a
	// base image: there is nothing to read a manifest of and no daemon was needed.
	if got := errs.String(); got != "" {
		t.Errorf("something was said on the way to a validate that needed no daemon: %s", got)
	}
}

// TestSkippingTheManifestsSaysSo, because a validate that silently skipped the port check is a
// validate that passes a workflow the pre-receive hook will reject.
func TestSkippingTheManifestsSaysSo(t *testing.T) {
	e, out, _ := reading(t)
	code := run(t.Context(), e, []string{"validate", "-f", "testdata/brick-workflow/agentiik.yaml", "--manifests", "skip"})
	if code != exitSucceeded {
		t.Fatalf("the exit code is %d: %s", code, out)
	}
	got := out.String()
	for _, want := range []string{"is valid", "--manifests skip", "1 referenced image"} {
		if !strings.Contains(got, want) {
			t.Errorf("the answer does not say %q:\n%s", want, got)
		}
	}
}

func TestAManifestsFlagThatIsNeitherIsTheCommandLineBeingWrong(t *testing.T) {
	e, _, errs := reading(t)
	code := run(t.Context(), e, []string{"validate", "--manifests", "maybe"})
	if code != exitUsage {
		t.Fatalf("the exit code is %d and a command line that is wrong leaves with %d", code, exitUsage)
	}
	for _, want := range []string{"maybe", manifestsRead, manifestsSkip} {
		if !strings.Contains(errs.String(), want) {
			t.Errorf("the refusal does not name %q: %s", want, errs)
		}
	}
}

func TestACycleIsRefusedByTheRuleThatRefusesIt(t *testing.T) {
	e, out, errs := reading(t)
	code := run(t.Context(), e, []string{"validate", "-f", "testdata/cyclic/agentiik.yaml"})
	if code != exitRefused {
		t.Fatalf("the exit code is %d and a refused workflow leaves with %d", code, exitRefused)
	}
	if out.String() != "" {
		t.Errorf("a refused workflow wrote to standard output: %s", out)
	}
	got := errs.String()
	// Identifiers are never prettified: the rule prints as the corpus spells it.
	if !strings.Contains(got, "cycle-in-graph") {
		t.Errorf("the refusal does not name the rule: %s", got)
	}
	for _, step := range []string{"first", "second"} {
		if !strings.Contains(got, step) {
			t.Errorf("the refusal does not name the step %s: %s", step, got)
		}
	}
	// The package the refusal came from is not the reader's business.
	if strings.HasPrefix(got, "graph: ") {
		t.Errorf("the refusal still says which package refused: %s", got)
	}
}

// TestAWorkflowIncludeIsRefusedNamingTheRepositoryAndTheRef: resolving one is reaching another
// repository, and there is no server here.
func TestAWorkflowIncludeIsRefusedNamingTheRepositoryAndTheRef(t *testing.T) {
	e, _, errs := reading(t)
	code := run(t.Context(), e, []string{"validate", "-f", "testdata/remote/agentiik.yaml"})
	if code != exitRefused {
		t.Fatalf("the exit code is %d: %s", code, errs)
	}
	for _, want := range []string{"finance/common", "v2.1.0"} {
		if !strings.Contains(errs.String(), want) {
			t.Errorf("the refusal does not name %q: %s", want, errs)
		}
	}
}

// TestAnExtendsNobodyCarriesIsRefused is the inheritance half of what validate resolves: the
// scripted fixture extends a block an included file carries, and this one extends nothing.
func TestAnExtendsNobodyCarriesIsRefused(t *testing.T) {
	e, _, errs := reading(t)
	code := run(t.Context(), e, []string{"validate", "-f", "testdata/extends-nothing/agentiik.yaml"})
	if code != exitRefused {
		t.Fatalf("the exit code is %d: %s", code, errs)
	}
	for _, want := range []string{"only", ".nobody-carries-this"} {
		if !strings.Contains(errs.String(), want) {
			t.Errorf("the refusal does not name %q: %s", want, errs)
		}
	}
}

// TestWithNoDaemonTheManifestsCannotBeReadAndNothingAboutTheFileIsRefused: exit 4 and not exit 1.
// Telling somebody their file is wrong because their Docker is not running is telling them to fix
// the wrong thing.
func TestWithNoDaemonTheManifestsCannotBeReadAndNothingAboutTheFileIsRefused(t *testing.T) {
	if _, ok := dockertest.Socket(); ok {
		t.Skip("this machine has a daemon, so there is nothing here to be unable to reach")
	}
	e, _, errs := reading(t)
	code := run(t.Context(), e, []string{"validate", "-f", "testdata/brick-workflow/agentiik.yaml"})
	if code != exitNoOutcome {
		t.Fatalf("the exit code is %d and a daemon that could not be reached leaves with %d: %s", code, exitNoOutcome, errs)
	}
}

func TestAnEntryPointThatIsNotThereIsRefused(t *testing.T) {
	e, _, errs := reading(t)
	code := run(t.Context(), e, []string{"validate", "-f", "testdata/no-such-file.yaml"})
	if code != exitRefused {
		t.Fatalf("the exit code is %d: %s", code, errs)
	}
	if !strings.Contains(errs.String(), "no-such-file.yaml") {
		t.Errorf("the refusal does not name the file it could not read: %s", errs)
	}
}

// TestTheTableAndTheDispatchCannotDisagree reads the table the usage text is printed from, which
// is the same one the dispatch reads.
func TestTheTableAndTheDispatchCannotDisagree(t *testing.T) {
	e, out, errs := reading(t)
	if code := run(t.Context(), e, []string{"--help"}); code != exitSucceeded {
		t.Fatalf("the exit code of --help is %d", code)
	}
	got := out.String()
	for _, c := range commands {
		if !strings.Contains(got, c.name) {
			t.Errorf("the usage text does not carry %s, which the table does", c.name)
		}
	}
	if errs.String() != "" {
		t.Errorf("--help wrote to standard error: %s", errs)
	}

	// A verb nobody knows is the command line being wrong, and the answer names what
	// there is.
	e, _, errs = reading(t)
	if code := run(t.Context(), e, []string{"brick", "frobnicate"}); code != exitUsage {
		t.Errorf("an unknown verb leaves with %d and it leaves with %d", code, exitUsage)
	}
	if !strings.Contains(errs.String(), "brick frobnicate") {
		t.Errorf("the refusal does not name what was typed: %s", errs)
	}

	// A verb of the table that reaches an installation refuses by name, and it is exit 1
	// and not exit 2: the command line was right.
	e, _, errs = reading(t)
	if code := run(t.Context(), e, []string{"push"}); code != exitRefused {
		t.Errorf("agk push leaves with %d and a verb that reaches an installation leaves with %d", code, exitRefused)
	}
	if !strings.Contains(errs.String(), "push") {
		t.Errorf("the refusal does not name the verb: %s", errs)
	}
}

func TestTheVersionIsAFlagAndNotACommand(t *testing.T) {
	e, out, _ := reading(t)
	if code := run(t.Context(), e, []string{"--version"}); code != exitSucceeded {
		t.Fatalf("--version leaves with %d", code)
	}
	if !strings.HasPrefix(out.String(), "agk ") {
		t.Errorf("--version answers %q", out)
	}
	for _, c := range commands {
		if c.name == "version" {
			t.Error("version is in the command table, and the documented table is exactly the table")
		}
	}
}

// TestTheStepNamedForAnImageIsTheStepThatRunsItAsABrick, because the step travels with the
// image so that a refusal names a line somebody can go and read.
//
// No manifest is read of a script step's image, so a script step is not the step a manifest
// was read for even when it names an image a brick step names too, which a step running a
// script inside its own brick's image does. agk run --local chooses the same way, and a
// refusal written in two different words by validate and by run is the thing one reader of a
// workflow exists to prevent.
func TestTheStepNamedForAnImageIsTheStepThatRunsItAsABrick(t *testing.T) {
	wf, err := graph.Parse([]byte(`
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: shared
  namespace: finance
steps:
  a-script:
    image: example/brick:1
    inputs:
      in: "a value"
    script:
      - echo a script inside the brick's own image
    outputs: [out]
  z-brick:
    image: example/brick:1
    needs:
      - { step: a-script, port: out, as: in }
    outputs: [ok]
`))
	if err != nil {
		t.Fatal(err)
	}
	referenced := references(wf)
	if len(referenced) != 1 {
		t.Fatalf("the workflow names %d images to read a manifest of, and one step of the two runs a brick: %+v", len(referenced), referenced)
	}
	if got := referenced[0].Step; got != "z-brick" {
		t.Errorf("the image is read for step %s, want z-brick: a script step has no manifest to be held to, whatever it sorts before", got)
	}
}

// TestAskingACommandForItsFlagsIsNotAUsageError, because -h is a control the usage text
// offers every command, and the process leaving with 2 would tell somebody they had made a
// mistake by reading the help. agk -h answers 0 and so does every verb of the table.
func TestAskingACommandForItsFlagsIsNotAUsageError(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"validate", "-h"}, {"graph", "-h"}, {"run", "-h"}, {"brick", "test", "-h"}} {
		e, out, errs := reading(t)
		if code := run(t.Context(), e, args); code != exitSucceeded {
			t.Errorf("agk %s leaves with %d, want %d", strings.Join(args, " "), code, exitSucceeded)
		}
		if said := out.String() + errs.String(); !strings.Contains(said, "Usage:") {
			t.Errorf("agk %s said %q, and -h answers with the flags of the command that was typed", strings.Join(args, " "), said)
		}
	}

	// And a flag nobody defined is still the usage error it was.
	e, _, _ := reading(t)
	if code := run(t.Context(), e, []string{"validate", "--nosuchflag"}); code != exitUsage {
		t.Errorf("a flag that is not defined leaves with %d, want %d", code, exitUsage)
	}
}
