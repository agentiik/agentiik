package brick_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/brick"
)

// published is what the runner knows about a batch and the container does not decide.
// The port is left out on purpose: it is the name of the file the envelope was found in.
var published = agk.Meta{
	RunID:      "01JMZ8W4K2R7Q0E3N5T9",
	Step:       "normalize",
	Attempt:    1,
	ProducedAt: time.Date(2026, 9, 10, 6, 0, 12, 418000000, time.UTC),
}

// wrote lays a container's output down where the runner collects it, and returns the
// directory that is the host side of /agk/out.
func wrote(t *testing.T, ports map[agk.Port]agk.Envelope) string {
	t.Helper()
	dir := t.TempDir()
	if ports == nil {
		return dir
	}
	if err := os.MkdirAll(filepath.Join(dir, "ports"), 0o755); err != nil {
		t.Fatal(err)
	}
	for port, e := range ports {
		f, err := os.Create(filepath.Join(dir, "ports", string(port)+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Encode(f); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestAPortTheContainerNeverWroteIsNotAnError holds the rule to its words: an output
// port declared but never written by the container publishes an empty envelope, and that
// is success rather than a failure of the step.
func TestAPortTheContainerNeverWroteIsNotAnError(t *testing.T) {
	dir := wrote(t, map[agk.Port]agk.Envelope{
		"ok": envelope("normalize", "ok", agk.Item{ID: "01JMZ8W4K7A1B2C3D4E5", Data: map[string]any{"total": 88}, Files: []agk.File{}}),
	})

	// The step declares three ports and the container had something to say on one.
	out, err := brick.Collect(dir, []agk.Port{"ok", "rejected", "error"}, published, agk.DefaultLimits())
	if err != nil {
		t.Fatalf("a port nobody wrote was an error: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("collected %d ports, want one envelope per declared port", len(out))
	}
	if got := out["ok"].Meta.Count; got != 1 {
		t.Errorf("port ok holds %d items, want the one the container wrote", got)
	}
	for _, port := range []agk.Port{"rejected", "error"} {
		e := out[port]
		if e.Meta.Count != 0 || len(e.Items) != 0 {
			t.Errorf("port %s: got %d items, want the empty envelope a port nobody wrote publishes", port, len(e.Items))
		}
		// The empty envelope is a whole envelope: a downstream step reads a batch of
		// nothing, so the metadata saying where it came from has to be there.
		if e.Meta.RunID != published.RunID || e.Meta.Step != published.Step || e.Meta.Port != port || e.Meta.Attempt != published.Attempt {
			t.Errorf("port %s: the empty envelope carries %+v", port, e.Meta)
		}
		if err := e.Validate(agk.DefaultLimits()); err != nil {
			t.Errorf("port %s: the empty envelope is not one: %v", port, err)
		}
	}
}

// TestABrickThatWroteNothingPublishesEveryPortEmpty: a brick that writes nothing under
// /agk/out/ports/ and exits 0 is a legitimate brick, and one that never made the
// directory has done no less than one that left it empty.
func TestABrickThatWroteNothingPublishesEveryPortEmpty(t *testing.T) {
	for _, name := range []string{"no directory at all", "an empty directory"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if name == "an empty directory" {
				if err := os.MkdirAll(filepath.Join(dir, "ports"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			out, err := brick.Collect(dir, []agk.Port{"out", "error"}, published, agk.DefaultLimits())
			if err != nil {
				t.Fatalf("a brick that wrote nothing failed: %v", err)
			}
			if len(out) != 2 {
				t.Fatalf("collected %d ports, want one envelope per declared port", len(out))
			}
			for port, e := range out {
				if len(e.Items) != 0 {
					t.Errorf("port %s carries %d items", port, len(e.Items))
				}
			}
		})
	}
}

// TestCollectRefusesAPortTheStepDoesNotDeclare: the declared names are the ones the
// container was given as AGK_OUT_PORTS, and a batch written under another name is a
// batch no edge reads, which is worth refusing rather than losing in silence.
func TestCollectRefusesAPortTheStepDoesNotDeclare(t *testing.T) {
	dir := wrote(t, map[agk.Port]agk.Envelope{
		"out":        envelope("normalize", "out"),
		"undeclared": envelope("normalize", "undeclared"),
	})

	_, err := brick.Collect(dir, []agk.Port{"out"}, published, agk.DefaultLimits())
	if err == nil {
		t.Fatal("a port the step does not declare was collected")
	}
	if !strings.Contains(err.Error(), "undeclared.json") {
		t.Errorf("the refusal does not name the file: %v", err)
	}
}

// TestTheMetadataAContainerWritesIsTheMetadataItWasGiven. The run, the step, the port and
// the attempt all reach the container, so an envelope disagreeing on one of them is not
// the batch that belongs on this port and is refused rather than corrected.
func TestTheMetadataAContainerWritesIsTheMetadataItWasGiven(t *testing.T) {
	for _, c := range []struct {
		name  string
		meta  agk.Meta
		names string
	}{
		{"another run", agk.Meta{RunID: "01JMZ8W4K2R7Q0E3N5TX", Step: "normalize", Port: "out", Attempt: 1, ProducedAt: published.ProducedAt}, "run_id"},
		{"another step", agk.Meta{RunID: published.RunID, Step: "invoice", Port: "out", Attempt: 1, ProducedAt: published.ProducedAt}, "step"},
		{"another port", agk.Meta{RunID: published.RunID, Step: "normalize", Port: "error", Attempt: 1, ProducedAt: published.ProducedAt}, "port"},
		{"another attempt", agk.Meta{RunID: published.RunID, Step: "normalize", Port: "out", Attempt: 2, ProducedAt: published.ProducedAt}, "attempt"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := wrote(t, map[agk.Port]agk.Envelope{"out": {Meta: c.meta, Items: []agk.Item{}}})

			_, err := brick.Collect(dir, []agk.Port{"out"}, published, agk.DefaultLimits())
			if !errors.Is(err, agk.ErrEnvelopeRejected) {
				t.Fatalf("an envelope carrying %s was accepted: %v", c.name, err)
			}
			if !strings.Contains(err.Error(), "meta."+c.names) {
				t.Errorf("the refusal does not name the member: %v", err)
			}
		})
	}
}

// TestCollectStampsThePublicationTime: a port is published when the emitting step ends,
// which is a moment only the runner is in a position to know.
func TestCollectStampsThePublicationTime(t *testing.T) {
	e := envelope("normalize", "out")
	e.Meta.ProducedAt = time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := wrote(t, map[agk.Port]agk.Envelope{"out": e})

	out, err := brick.Collect(dir, []agk.Port{"out"}, published, agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !out["out"].Meta.ProducedAt.Equal(published.ProducedAt) {
		t.Fatalf("produced_at is %s, want the moment the step ended", out["out"].Meta.ProducedAt)
	}
}

// TestCollectAppliesTheSizeRules: what a container wrote is read under the same limits
// as anything else that travels on a port, and an envelope longer than may travel is an
// application failure of the step that emitted it.
func TestCollectAppliesTheSizeRules(t *testing.T) {
	dir := wrote(t, map[agk.Port]agk.Envelope{
		"out": envelope("normalize", "out", agk.Item{ID: "01JMZ8W4K7A1B2C3D4E5", Data: map[string]any{"report": strings.Repeat("x", 4096)}, Files: []agk.File{}}),
	})

	l := agk.DefaultLimits()
	l.EnvelopeMaxBytes = 512
	_, err := brick.Collect(dir, []agk.Port{"out"}, published, l)
	if !errors.Is(err, agk.ErrStepFailed) {
		t.Fatalf("an envelope above envelope_max_bytes was not an application failure of the step: %v", err)
	}
}

// TestCollectRefusesMetadataAnEnvelopeCouldNotCarry. The metadata is stamped on every
// port nobody wrote, so a run or an attempt that could not travel would travel unnoticed
// on exactly the ports there is nothing else to check.
func TestCollectRefusesMetadataAnEnvelopeCouldNotCarry(t *testing.T) {
	_, err := brick.Collect(t.TempDir(), []agk.Port{"out"}, agk.Meta{Step: "normalize", Attempt: 1, ProducedAt: published.ProducedAt}, agk.DefaultLimits())
	if err == nil {
		t.Fatal("a batch with no run was published")
	}
}

// TestAnEnvelopeIsAFileTheContainerWrote: what is collected is read on the runner's side
// of the boundary, so a link left where an envelope belongs is refused rather than
// followed to a file the container could not have read itself.
func TestAnEnvelopeIsAFileTheContainerWrote(t *testing.T) {
	// The file behind the link is an envelope this port would otherwise have been glad
	// to publish, so what the refusal rests on is the link and not the contents.
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.json")
	f, err := os.Create(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := envelope("normalize", "out").Encode(f); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		lay  func(t *testing.T, portsDir string)
	}{
		{
			name: "a link to a file outside the mount",
			lay: func(t *testing.T, portsDir string) {
				if err := os.Symlink(elsewhere, filepath.Join(portsDir, "out.json")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a directory where an envelope belongs",
			lay: func(t *testing.T, portsDir string) {
				if err := os.Mkdir(filepath.Join(portsDir, "out.json"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			portsDir := filepath.Join(dir, "ports")
			if err := os.MkdirAll(portsDir, 0o755); err != nil {
				t.Fatal(err)
			}
			c.lay(t, portsDir)

			if _, err := brick.Collect(dir, []agk.Port{"out"}, published, agk.DefaultLimits()); err == nil {
				t.Fatal("it was collected as though the container had written an envelope")
			}
		})
	}
}
