package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/cmd/agk/internal/local"
)

// What a person at a terminal is owed, held as tests because each of these was wrong once.
//
// #voice governs every line printed here, and two of its rules are the ones a command line
// breaks quietly: a sentence has to be a sentence, and an error has to say where. Neither is
// caught by a test that only reads the exit code, which is why these read the bytes.

// TestNoRefusalSaysThereIsNoASomething reads every refusal the absent verbs write.
//
// Each was built by putting "there is no %s" round a noun phrase that carried its own
// article, so the table printed "there is no a server to register the workflow with" and
// "there is no an installation to sign in against" for as long as nobody read one out loud.
// A command line whose refusals are not English is a command line that reads as broken at
// exactly the moment somebody is already stuck.
func TestNoRefusalSaysThereIsNoASomething(t *testing.T) {
	// The article doubled, and the determiner that cannot follow "no" either: "there is
	// no the brick templates" is the same mistake made with a definite article.
	doubled := []string{"no a ", "no an ", "no the "}

	// The verbs that do something here rather than waiting for an installation. push joined
	// them when the API arrived, and logs and status when runs could be read through it: none
	// of them names what is missing any more, each goes and does it.
	built := map[string]bool{
		"validate": true, "graph": true, "run": true, "brick test": true, "push": true,
		"logs": true, "status": true,
	}
	for _, c := range commands {
		if built[c.name] {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			e, out, errs := reading(t)
			code := c.run(t.Context(), e, nil)
			// The command line was right: it named a verb the documentation lists,
			// spelled as the documentation spells it, and what is missing is on the
			// other side of it.
			if code != exitRefused {
				t.Errorf("the exit code is %d and a verb that names what it waits for is refused with %d", code, exitRefused)
			}
			said := errs.String()
			if out.String() != "" {
				t.Errorf("a refusal reached standard output: %s", out)
			}
			for _, wrong := range doubled {
				if strings.Contains(said, wrong) {
					t.Errorf("the refusal reads\n\t%s\nand %q is not English: the phrase already carries its article", strings.TrimSpace(said), wrong)
				}
			}
			// And it still has to say what it waits for, which is the whole reason
			// these verbs answer at all rather than reading as unknown commands.
			if !strings.Contains(said, "refused") {
				t.Errorf("the refusal does not say it refused: %s", said)
			}
		})
	}
}

// TestAnEntryPointThatIsNotThereNamesWhereItWasRead is the other half of the voice rule: an
// error names where.
//
// Every command that reads a workflow goes through one loader, and that loader handed the
// file name to an fs.FS rooted at the directory, so the directory never reached the message.
// Run from anywhere, agk validate said "open agentiik.yaml: no such file or directory", and
// -f nope/agentiik.yaml lost the nope/ entirely. A person who mistyped a path or is standing
// in the wrong directory is then told the one thing they already knew.
func TestAnEntryPointThatIsNotThereNamesWhereItWasRead(t *testing.T) {
	dir := t.TempDir()

	for _, c := range []struct {
		what  string
		args  []string
		names string
	}{
		{"the default entry point", []string{"validate", "--manifests", "skip"}, filepath.Join(dir, entryPoint)},
		{"an entry point given with -f", []string{"validate", "--manifests", "skip", "-f", "nope/agentiik.yaml"}, filepath.Join(dir, "nope", "agentiik.yaml")},
		{"a drawing of one", []string{"graph", "-f", "nope/agentiik.yaml"}, filepath.Join(dir, "nope", "agentiik.yaml")},
		// The commonest way -f is mistyped, since the tree and the entry point differ
		// by one segment.
		{"a directory where the entry point was meant", []string{"validate", "--manifests", "skip", "-f", "."}, dir},
	} {
		t.Run(c.what, func(t *testing.T) {
			code, out, errs := runner(t, dir, c.args...)
			if code != exitRefused {
				t.Fatalf("the exit code is %d, want %d\n%s%s", code, exitRefused, out, errs)
			}
			if !strings.Contains(errs, c.names) {
				t.Errorf("the refusal reads\n\t%s\nand it does not say where it looked:\n\t%s", strings.TrimSpace(errs), c.names)
			}
		})
	}
}

// TestTheWorkRootIsNotUnderTheTreeThatEveryContainerReads is the host-side reading of the
// rule cmd/agk/internal/local keeps, written here because this is where the two paths meet:
// the directory handed to the driver as the repository tree and the directory handed to it as
// the work root are both chosen by this command.
//
// The tree is bound read-only at /agk/repo in every container of the run, and the work root
// is where a task's secret values are written on a platform with no tmpfs. One inside the
// other is a step reading another step's secret, which adversary_real_test.go proves against
// the daemon. This is the same rule with no daemon in reach, so it holds in CI too.
func TestTheWorkRootIsNotUnderTheTreeThatEveryContainerReads(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, entryPoint), []byte("apiVersion: agentiik.dev/v1\nkind: Workflow\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, _, _ := reading(t)
	e.Dir = tree

	// Both the default and a working directory somebody pointed back into the tree, since
	// --dir is the flag that would otherwise put it back.
	for _, dir := range []string{"", ".agk", filepath.Join(tree, "somewhere", "else")} {
		root := workingDir(e, dir, tree)
		layout, err := local.NewLayout(root)
		if err != nil {
			t.Fatalf("the layout at %s: %s", root, err)
		}
		t.Cleanup(func() { os.RemoveAll(layout.WorkRoot()) })
		rel, err := filepath.Rel(tree, layout.WorkRoot())
		if err != nil {
			continue // A work root on another volume is outside the tree by construction.
		}
		if !strings.HasPrefix(rel, "..") {
			t.Errorf("--dir %q puts the work root at %s, which is inside %s: the tree is bound read-only at /agk/repo in every container, so a task's secret values would be readable by every other task of the run", dir, layout.WorkRoot(), tree)
		}
	}
}

// TestNoRefusalNamesAPackageOfThisModule holds the rule print.go states: the leading graph:,
// driver: or brick: is trimmed, because "the person holding a YAML file at a terminal knows
// which command they typed and has never heard of the packages".
//
// It was stated as a list of prefixes, and a list is the thing that goes out of date. The one
// it was missing is this binary's own: a working directory that could not be prepared reached
// the terminal as "local: the object store /dev/null/nope/objects could not be prepared",
// naming a package nothing outside this module can even import.
//
// The five below are the packages that prefix a refusal with their own name, which is what
// `grep -r '"<name>: '` over each of them says. A package that starts doing it later is a
// package this list has to gain, and this test is where that is noticed.
func TestNoRefusalNamesAPackageOfThisModule(t *testing.T) {
	for _, prefix := range []string{"artifact: ", "brick: ", "driver: ", "graph: ", "local: "} {
		if got := trim(prefix + "something was refused"); got != "something was refused" {
			t.Errorf("a refusal reading %q reaches the terminal as %q, and %q is a package nobody typing this command has heard of", prefix+"something was refused", got, strings.TrimSuffix(prefix, ": "))
		}
	}

	// And a message that merely begins with a word and a colon is left alone, because a
	// step, a port and a flag all read that way and every one of them is the where a
	// refusal owes the reader.
	for _, kept := range []string{
		"step charge: exit code 42, application failure",
		"input cycle: required: no value supplied and the input declares no default",
		"--secret-file billing_api: open ./nope.key: no such file or directory",
	} {
		if got := trim(kept); got != kept {
			t.Errorf("%q was trimmed to %q: what is taken off the front is a package of this module and nothing else", kept, got)
		}
	}
}
