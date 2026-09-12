package main

import (
	"path/filepath"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/driver"
)

// This file is what makes the duplication in contract.go honest.
//
// The shipped binary may link package agk and the standard library and nothing else, so the
// paths of the contract and the names of its variables are spelled here rather than imported
// from brick, which names the same paths but pulls a YAML parser in behind them, and from
// driver, which names the same variables behind a Docker client. A test is not the shipped
// binary: it may import both, and it does, so that the day one of those constants moves this
// fails rather than a container quietly reading the wrong directory.

// TestThePathsAreTheOnesBrickNames compares every path this program uses against the package
// that owns the other side of the mount.
func TestThePathsAreTheOnesBrickNames(t *testing.T) {
	for _, c := range []struct {
		what  string
		here  string
		there string
	}{
		{"the root", Root, brick.Root},
		{"the input mount", InDir, brick.InDir},
		{"the writable half", OutDir, brick.OutDir},
		{"the ports directory", OutPortsDir, brick.OutPortsDir},
		{"the files directory", OutFilesDir, brick.OutFilesDir},
		{"the mount of this program", BinPath, driver.BinPath},
	} {
		if c.here != c.there {
			t.Errorf("%s is %q here and %q there", c.what, c.here, c.there)
		}
	}
}

// TestTheVariablesAreTheOnesTheDriverSets compares the four this program reads against the
// driver that writes them into the container's environment.
func TestTheVariablesAreTheOnesTheDriverSets(t *testing.T) {
	for _, c := range []struct{ here, there string }{
		{EnvRunID, driver.EnvRunID},
		{EnvStep, driver.EnvStep},
		{EnvAttempt, driver.EnvAttempt},
		{EnvOutPorts, driver.EnvOutPorts},
	} {
		if c.here != c.there {
			t.Errorf("the variable is %q here and %q in the driver that sets it", c.here, c.there)
		}
	}
}

// TestTheWorkingPathsAreTheConstantsAtTheRoot holds the relocation the tests rely on. Every
// verb joins the root it was given rather than using the constants, so that a test can run
// against a temporary directory; with the root at /agk the two have to be the same paths, or
// the tests would be testing a layout no container has.
func TestTheWorkingPathsAreTheConstantsAtTheRoot(t *testing.T) {
	e := env{Root: Root}
	for _, c := range []struct{ got, want string }{
		{e.inDir(), InDir},
		{e.outDir(), OutDir},
		{e.outPortsDir(), OutPortsDir},
		{e.outFilesDir(), OutFilesDir},
	} {
		if c.got != c.want {
			t.Errorf("the working path is %q and the contract names %q", c.got, c.want)
		}
	}
}

// TestItemsReadsWhatWriteInputsWrote is the stronger half of holding the duplication: rather
// than comparing a string for the envelope's file name, which brick does not export, the
// input side is laid down by brick itself and read by the verb that reads it.
func TestItemsReadsWhatWriteInputsWrote(t *testing.T) {
	h := newHarness(t)
	store, err := artifact.New(artifact.Dir(t.TempDir()), "finance", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	// One port, as the driver would bind it, written by the package that owns that
	// side of the mount.
	want := batch("ok", 2)
	mounts, err := brick.WriteInputs(t.Context(), store, h.env.inDir(), map[agk.Port]agk.Envelope{"in": want})
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 || mounts[0].Target != InDir+"/in" {
		t.Fatalf("the mount is %+v, and the contract binds %s/<port>", mounts, InDir)
	}

	h.ok("items")

	got := lines(h.out.String())
	if len(got) != len(want.Items) {
		t.Fatalf("read %d items back out of the mount brick wrote, and it holds %d", len(got), len(want.Items))
	}
	for i, line := range got {
		if id := object(t, line)["id"]; id != want.Items[i].ID {
			t.Errorf("item %d reads back as %v, and brick wrote %s", i+1, id, want.Items[i].ID)
		}
	}
}

// TestCollectReadsWhatEmitAndAttachWrote is the same test from the other end, and it is the
// whole contract this program exists to honour: what the two writing verbs leave under
// /agk/out is what the runner collects, refusing nothing.
func TestCollectReadsWhatEmitAndAttachWrote(t *testing.T) {
	h := newHarness(t)
	h.ok("emit", "out", "--from", h.write("r.json", `[{"invoice":"INV-1042"},{"invoice":"INV-1043"}]`), "--id", "invoice")
	h.ok("emit", "error", "--from", h.write("e.json", `{"message":"one was refused"}`))
	h.ok("attach", h.write("purchase-order.pdf", "%PDF-1.7\n"), "--port", "out", "--item", "INV-1043")

	// The metadata the runner collects under is the metadata the container was given,
	// which is what emit stamped. produced_at is the runner's own.
	m := agk.Meta{RunID: fixtureRun, Step: fixtureStep, Attempt: 1, ProducedAt: h.env.Now()}
	out, err := brick.Collect(h.env.outDir(), []agk.Port{"out", "error"}, m, agk.DefaultLimits())
	if err != nil {
		t.Fatalf("the runner refuses what this wrote: %s", err)
	}

	if n := len(out["out"].Items); n != 2 {
		t.Errorf("port out comes back with %d items", n)
	}
	if n := len(out["error"].Items); n != 1 {
		t.Errorf("port error comes back with %d items", n)
	}
	files := out["out"].Items[1].Files
	if len(files) != 1 {
		t.Fatalf("the second item comes back with %d files", len(files))
	}
	// The bytes are under /agk/out/files/ by the name the entry gives them, which is
	// where the driver reads them and what it verifies the digest against.
	if _, err := readEnvelope(portPath(h.env, "out")); err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(files[0].Name); got != "purchase-order.pdf" {
		t.Errorf("the entry names %q", got)
	}
}
