package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agentiik/agentiik/internal/dockertest"
)

// The half of the reading commands that needs a real daemon: the manifest of an image, read off
// the image, and a brick run against its cases.
//
// Everything here skips when there is no daemon and runs when there is, exactly as
// driver/real_test.go does, so that CI stays green and this machine tests for real. The brick is
// built from testdata rather than pulled, because the point of it is to be a plain image that
// was never pushed anywhere, which is the image a laptop meets.

// counterImage is the brick built from testdata/brick.
const counterImage = "agk-counter-brick:test"

// realDaemon skips unless there is a daemon and a docker command to build the fixture with, and
// builds the fixture brick once per test binary.
//
// Built and not adopted from whatever the daemon already holds under that tag. An earlier build
// of a fixture that has since been edited is the worst kind of green: the test passes against
// the brick of a week ago and says nothing about the one in the tree. The daemon's own layer
// cache is what makes doing it properly cost nothing when nothing changed.
func realDaemon(t *testing.T) {
	t.Helper()
	if _, ok := dockertest.Socket(); !ok {
		t.Skip("no Docker daemon on this machine")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker command to build the fixture brick with")
	}
	builtFixture.Do(func() {
		fixtureBuild, fixtureErr = exec.Command("docker", "build", "-t", counterImage, "testdata/brick").CombinedOutput()
	})
	if fixtureErr != nil {
		t.Skipf("the fixture brick could not be built: %v\n%s", fixtureErr, fixtureBuild)
	}
}

// What the one build left behind, so that every test after the first is told the same thing.
var (
	builtFixture sync.Once
	fixtureBuild []byte
	fixtureErr   error
)

// TestTheManifestOfAReferencedImageIsWhatTheStepIsHeldTo is the fourth clause of agk validate:
// "checks ports against the manifests of the referenced images".
func TestTheManifestOfAReferencedImageIsWhatTheStepIsHeldTo(t *testing.T) {
	realDaemon(t)

	e, out, errs := reading(t)
	code := run(t.Context(), e, []string{"validate", "-f", "testdata/brick-workflow/agentiik.yaml"})
	if code != exitSucceeded {
		t.Fatalf("the exit code is %d: %s%s", code, out, errs)
	}
	got := out.String()
	t.Logf("agk validate says:\n%s", got)
	// The line about the image says which brick it turned out to be and what it reads and
	// writes, which is the half of the contract the file has to match.
	for _, want := range []string{counterImage, "counter 0.1.0", "reads orders", "writes ok rejected", "is valid", "held to the manifests"} {
		if !strings.Contains(got, want) {
			t.Errorf("the answer does not say %q", want)
		}
	}
}

// TestAPortTheBrickDoesNotWriteIsRefusedByTheRuleThatRefusesIt, which is a rule no JSON Schema
// can express and that nothing but the manifest can answer for.
func TestAPortTheBrickDoesNotWriteIsRefusedByTheRuleThatRefusesIt(t *testing.T) {
	realDaemon(t)

	e, _, errs := reading(t)
	code := run(t.Context(), e, []string{"validate", "-f", "testdata/brick-workflow-wrong-port/agentiik.yaml"})
	if code != exitRefused {
		t.Fatalf("the exit code is %d and a refused workflow leaves with %d: %s", code, exitRefused, errs)
	}
	got := errs.String()
	t.Logf("refused: %s", strings.TrimSpace(got))
	for _, want := range []string{"step count", "port invoices", "step-output-not-in-manifest"} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal does not name %q: %s", want, got)
		}
	}
}

// TestTheFixtureBrickMatchesItsCases is issue #82 against the daemon of this machine: a brick run
// against sample envelopes, compared against expected outputs, including the digest of the
// artifact it attached.
func TestTheFixtureBrickMatchesItsCases(t *testing.T) {
	realDaemon(t)

	e, out, errs := reading(t)
	code := run(t.Context(), e, []string{"brick", "test", "--image", counterImage, "--cases", "testdata/brick/cases"})
	t.Logf("agk brick test says:\n%s", out.String())
	if said := errs.String(); said != "" {
		t.Logf("and on the way:\n%s", said)
	}
	if code != exitSucceeded {
		t.Fatalf("the exit code is %d and every case matches", code)
	}
	got := out.String()
	for _, want := range []string{
		"counter 0.1.0 on " + counterImage,
		"case counts-three-items matched",
		"case reads-the-repository matched",
		"case refuses-an-empty-batch matched",
		"3 cases, 3 matched",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the report does not say %q", want)
		}
	}
}

// TestABrickThatDerivesItsIdentitiesCanBeHeldToThemExactly is why --ignore exists and why
// items.id is not held aside by anything but a choice: the fixture brick derives every identity
// from its payload, so a case may ask for them to be compared and get an answer.
func TestABrickThatDerivesItsIdentitiesCanBeHeldToThemExactly(t *testing.T) {
	realDaemon(t)

	e, out, errs := reading(t)
	code := run(t.Context(), e, []string{"brick", "test", "--image", counterImage,
		"--cases", "testdata/brick/cases", "--ignore", "meta.run_id,files.uri"})
	if code != exitSucceeded {
		t.Fatalf("the exit code is %d with the identities compared: %s%s", code, out, errs)
	}
	if !strings.Contains(out.String(), "3 cases, 3 matched") {
		t.Errorf("the report reads %s", out)
	}
}

// TestTheSameCasesRunTwiceAnswerTheSameWay, which is the brick's half of what the milestone
// sentence says about a workflow: the same inputs produce the same envelopes, and the comparison
// that says so holds aside the run and nothing else.
func TestTheSameCasesRunTwiceAnswerTheSameWay(t *testing.T) {
	realDaemon(t)

	var answers []string
	for range 2 {
		e, out, _ := reading(t)
		if code := run(t.Context(), e, []string{"brick", "test", "--image", counterImage, "--cases", "testdata/brick/cases"}); code != exitSucceeded {
			t.Fatalf("the exit code is %d", code)
		}
		answers = append(answers, out.String())
	}
	if answers[0] != answers[1] {
		t.Errorf("two runs of the same cases answered differently:\n%s\n%s", answers[0], answers[1])
	}
}

// TestACaseWhoseExpectationMovedIsNamedMemberByMember: a report that said only that something
// did not match would leave the reader to diff two documents themselves.
func TestACaseWhoseExpectationMovedIsNamedMemberByMember(t *testing.T) {
	realDaemon(t)

	// A copy of the cases with one member changed, because the committed fixture is the one
	// every other test here runs against.
	cases := filepath.Join(t.TempDir(), "cases")
	if err := copyTree("testdata/brick/cases", cases); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cases, "counts-three-items", "out", "ok.json")
	document, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(document), `"items_seen":3`, `"items_seen":99`, 1)
	if moved == string(document) {
		t.Fatal("the fixture no longer carries the member this test moves")
	}
	if err := os.WriteFile(path, []byte(moved), 0o644); err != nil {
		t.Fatal(err)
	}

	e, out, errs := reading(t)
	code := run(t.Context(), e, []string{"brick", "test", "--image", counterImage, "--cases", cases})
	if code != exitRefused {
		t.Fatalf("the exit code is %d and a case that did not match leaves with %d: %s%s", code, exitRefused, out, errs)
	}
	t.Logf("the report reads:\n%s", errs.String())
	for _, want := range []string{"case counts-three-items", "exit code 0", "data.items_seen", "want 99", "got 3"} {
		if !strings.Contains(errs.String(), want) {
			t.Errorf("the report does not say %q", want)
		}
	}
	// The case that was right is still right, and the summary says both.
	if !strings.Contains(out.String(), "3 cases, 2 matched") {
		t.Errorf("the summary does not say what matched: %s", out)
	}
}

// copyTree copies a directory of documents, which is what a test needs to change one of them
// without changing the fixture.
func copyTree(from, to string) error {
	return filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rest, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rest)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		document, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, document, 0o644)
	})
}
