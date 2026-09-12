package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

// TestItemsWritesOneItemPerLine is the documented pipe: agk items | jq -r
// '.data.vat_number' gives one value per item, which is only true if the output is one
// JSON object per line and the whole item is in it.
func TestItemsWritesOneItemPerLine(t *testing.T) {
	h := newHarness(t)
	h.mount("in", batch("ok", 3))

	h.ok("items")

	got := lines(h.out.String())
	if len(got) != 3 {
		t.Fatalf("wrote %d lines for 3 items: %q", len(got), h.out.String())
	}
	for i, line := range got {
		item := object(t, line)
		// The whole item and not only its payload: a script reads what is attached
		// to the item it is looking at as well as what it carries.
		for _, member := range []string{"id", "data", "files"} {
			if _, ok := item[member]; !ok {
				t.Errorf("line %d carries no %s: %s", i+1, member, line)
			}
		}
		data, ok := item["data"].(map[string]any)
		if !ok {
			t.Fatalf("line %d carries no data object: %s", i+1, line)
		}
		if data["vat_number"] == nil {
			t.Errorf("line %d carries no vat_number: %s", i+1, line)
		}
	}
}

// TestItemsOfAnEmptyBatchWritesNothing holds the empty case to being success. A step that
// rejected nothing hands a batch of nothing to the step below, and a pipe reading no lines
// is what a loop over none does.
func TestItemsOfAnEmptyBatchWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.mount("in", batch("ok", 0))

	h.ok("items")

	if h.out.Len() != 0 {
		t.Errorf("wrote %q for an envelope holding no item", h.out.String())
	}
}

// TestThePortIsImpliedWhenOneIsMountedAndNamedWhenSeveralAre is the rule in the one place
// a guess would be invisible: two ports, and a pipe reading the wrong batch is a script
// that looks right.
func TestThePortIsImpliedWhenOneIsMountedAndNamedWhenSeveralAre(t *testing.T) {
	h := newHarness(t)
	h.mount("in", batch("ok", 1))
	h.ok("items")
	if len(lines(h.out.String())) != 1 {
		t.Fatalf("one port mounted and the port was not implied: %q", h.err.String())
	}

	h.mount("reference", batch("ok", 2))
	message := h.refused("items")
	says(t, message, InDir, "in,reference", "--port")

	h.ok("items", "--port", "reference")
	if n := len(lines(h.out.String())); n != 2 {
		t.Errorf("agk items --port reference wrote %d lines, and that port carries 2 items", n)
	}
}

// TestAPortThatIsNotMountedIsRefusedNamingWhatIs keeps a typo from reading as an empty
// batch.
func TestAPortThatIsNotMountedIsRefusedNamingWhatIs(t *testing.T) {
	h := newHarness(t)
	h.mount("in", batch("ok", 1))

	message := h.refused("items", "--port", "reference")
	says(t, message, "reference", InDir, "in")
}

// TestAStepWithNoEdgeIntoItIsToldSo covers the step that reads nothing: the mount is
// empty, and the refusal says what that means rather than that a file is missing.
func TestAStepWithNoEdgeIntoItIsToldSo(t *testing.T) {
	h := newHarness(t)

	says(t, h.refused("items"), InDir, "no input port")

	// And the same when the runner bound nothing at all, which is the same step.
	if err := os.RemoveAll(h.env.inDir()); err != nil {
		t.Fatal(err)
	}
	says(t, h.refused("items"), InDir, "no input port")
}

// TestItemsRefusesAnEnvelopeThatIsNotOne holds the read to agk.Decode rather than to a
// JSON parse: the document is closed, and a member nobody recognises refuses it whole.
func TestItemsRefusesAnEnvelopeThatIsNotOne(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(h.env.inDir(), "in")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, EnvelopeFile)
	if err := os.WriteFile(path, []byte(`{"meta":{"run_id":"`+fixtureRun+`","step":"normalize","port":"ok","attempt":1,"count":1,"produced_at":"2026-09-10T06:00:12Z","shard":"1/3"},"items":[]}`), 0o444); err != nil {
		t.Fatal(err)
	}

	// The path is named, because a person reading this is inside the container
	// looking at files.
	says(t, h.refused("items"), path, "shard")
}

// TestItemsTakesNoArgument keeps a misspelled flag from being dropped in silence.
func TestItemsTakesNoArgument(t *testing.T) {
	h := newHarness(t)
	h.mount("in", batch("ok", 1))

	says(t, h.refused("items", "in"), "in", "--port")
	says(t, h.refused("items", "--ports", "in"), "ports")
}

// TestItemsReadsTheMountAndNotStandardInput states the choice: the envelope arrives on
// standard input too, and this verb deliberately does not read it from there, because a
// port is what an item came in on and standard input cannot name one.
func TestItemsReadsTheMountAndNotStandardInput(t *testing.T) {
	h := newHarness(t)
	var b []byte
	envelope := batch("ok", 2)
	b, _ = envelope.MarshalJSON()
	h.stdin(string(b))

	says(t, h.refused("items"), "no input port")

	h.mount("in", batch("ok", 1))
	h.stdin(string(b))
	h.ok("items")
	if n := len(lines(h.out.String())); n != 1 {
		t.Errorf("wrote %d lines, and the mounted port carries 1 item while standard input carries 2", n)
	}
}

// TestAnItemIsWrittenWithNoHTMLEscaping holds the output to the way a payload travels
// inside the envelope. A consumer reading a URL out of an item gets the characters the
// emitter wrote.
func TestAnItemIsWrittenWithNoHTMLEscaping(t *testing.T) {
	h := newHarness(t)
	envelope := batch("ok", 1)
	envelope.Items[0].Data = map[string]any{"endpoint": "https://vat.example.com/v2?a=1&b=2"}
	h.mount("in", envelope)

	h.ok("items")

	if got := h.out.String(); !strings.Contains(got, "&b=2") {
		t.Errorf("the line escapes what a payload carries: %s", got)
	}
}

// TestEveryItemOfTheMountedEnvelopeIsWritten is the count, so that a verb that wrote the
// first item and stopped could not pass.
func TestEveryItemOfTheMountedEnvelopeIsWritten(t *testing.T) {
	h := newHarness(t)
	const n = 50
	h.mount("in", batch("ok", n))

	h.ok("items")

	got := lines(h.out.String())
	if len(got) != n {
		t.Fatalf("wrote %d lines for %d items", len(got), n)
	}
	seen := map[string]bool{}
	for _, line := range got {
		seen[object(t, line)["id"].(string)] = true
	}
	if len(seen) != n {
		t.Errorf("wrote %d distinct identifiers for %d items", len(seen), n)
	}
}

// TestTheMountedPortsAreReadInOrder keeps a refusal reading the same way twice: a
// directory is walked in whatever order the filesystem feels like.
func TestTheMountedPortsAreReadInOrder(t *testing.T) {
	h := newHarness(t)
	for _, port := range []agk.Port{"zeta", "alpha", "middle"} {
		h.mount(port, batch("ok", 1))
	}
	ports, err := mountedPorts(h.env)
	if err != nil {
		t.Fatal(err)
	}
	if got := portList(ports); got != "alpha,middle,zeta" {
		t.Errorf("the ports read as %s, and a refusal that named a different one on each run is one nobody trusts", got)
	}
}
