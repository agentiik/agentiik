package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What agk brick test refuses before it reaches a daemon. Each of these is the command line
// being wrong or the cases being unreadable, and none of them needs Docker: a harness that had
// to start a container to find out that its --ignore was misspelled would be a harness nobody
// runs locally.

func TestAnImageIsNamedOrTheCommandLineIsWrong(t *testing.T) {
	e, out, errs := reading(t)
	code := run(t.Context(), e, []string{"brick", "test"})
	if code != exitUsage {
		t.Fatalf("the exit code is %d and a command line that is wrong leaves with %d", code, exitUsage)
	}
	if out.String() != "" {
		t.Errorf("a refused command line wrote to standard output: %s", out)
	}
	if !strings.Contains(errs.String(), "--image") {
		t.Errorf("the refusal does not name the flag: %s", errs)
	}
}

func TestAMemberNobodyCanHoldAsideIsRefusedNamingTheOnesThereAre(t *testing.T) {
	e, _, errs := reading(t)
	code := run(t.Context(), e, []string{"brick", "test", "--image", "counter:test", "--ignore", "item.id"})
	if code != exitUsage {
		t.Fatalf("the exit code is %d: %s", code, errs)
	}
	for _, want := range []string{"item.id", "items.id", "meta.run_id"} {
		if !strings.Contains(errs.String(), want) {
			t.Errorf("the refusal does not name %q: %s", want, errs)
		}
	}
}

func TestCasesThatAreNotThereAreRefusedNamingTheDirectory(t *testing.T) {
	e, _, errs := reading(t)
	code := run(t.Context(), e, []string{"brick", "test", "--image", "counter:test", "--cases", "testdata/no-such-cases"})
	if code != exitRefused {
		t.Fatalf("the exit code is %d and nothing ran, which leaves with %d", code, exitRefused)
	}
	if !strings.Contains(errs.String(), "no-such-cases") {
		t.Errorf("the refusal does not name the directory: %s", errs)
	}
}

// TestACaseIsACaseNameAndAStepName, because the case's own name is the step the brick runs as,
// which is what AGK_STEP carries and what every envelope it writes has to agree with.
func TestACaseIsACaseNameAndAStepName(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "not a step name"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := caseNames(dir)
	if err == nil {
		t.Fatal("a directory that cannot be a step name was taken as a case")
	}
	if !strings.Contains(err.Error(), "AGK_STEP") {
		t.Errorf("the refusal does not say why a case is named the way it is: %v", err)
	}

	// A name that is a step name is a case, and a dot directory is not: a fixture tree
	// carries .DS_Store and a harness that took it for a case would fail on somebody's
	// laptop alone.
	for _, name := range []string{"counts-three-items", ".hidden"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(dir, "not a step name")); err != nil {
		t.Fatal(err)
	}
	names, err := caseNames(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "counts-three-items" {
		t.Errorf("the cases are %v", names)
	}
}

// TestTheCasesOfTheFixtureBrickAreReadInNameOrder, so that a harness reports them the same way
// twice.
func TestTheCasesOfTheFixtureBrickAreReadInNameOrder(t *testing.T) {
	names, err := caseNames("testdata/brick/cases")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"counts-three-items", "refuses-an-empty-batch"}
	if len(names) != len(want) {
		t.Fatalf("the cases are %v and they are %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("case %d is %s and it is %s", i, names[i], want[i])
		}
	}
}

// TestACaseThatExpectsAFailureAndAnEnvelopeIsRefused: a step that failed publishes nothing, so
// there is nothing on a port to expect, and a case saying both is a case contradicting itself.
func TestACaseThatExpectsAFailureAndAnEnvelopeIsRefused(t *testing.T) {
	code, err := exitOf("testdata/brick/cases/refuses-an-empty-batch")
	if err != nil {
		t.Fatal(err)
	}
	if code != 2 {
		t.Errorf("the case expects exit code %d and it expects 2", code)
	}
	code, err = exitOf("testdata/brick/cases/counts-three-items")
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Errorf("a case that says nothing about the exit code expects %d and it expects 0", code)
	}
}
