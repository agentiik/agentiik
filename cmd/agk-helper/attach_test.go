package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

// emitted puts one envelope on a port, which is what agk attach adds to.
func emitted(h *harness, port agk.Port, payload string) {
	h.t.Helper()
	h.ok("emit", string(port), "--from", h.write("emit.json", payload))
}

// TestAttachWritesTheFiveMemberEntryAndTheBytesBesideIt is the whole of the verb: the copy
// under /agk/out/files/, the five members, and the address the runner will upload the bytes
// under.
func TestAttachWritesTheFiveMemberEntryAndTheBytesBesideIt(t *testing.T) {
	h := newHarness(t)
	emitted(h, "out", `{"customer_id":"C-1042"}`)
	const content = "%PDF-1.7 not really a pdf\n"
	source := h.write("purchase-order.pdf", content)

	h.ok("attach", source, "--port", "out")

	envelope := h.port("out")
	if len(envelope.Items[0].Files) != 1 {
		t.Fatalf("the item carries %d files", len(envelope.Items[0].Files))
	}
	file := envelope.Items[0].Files[0]

	sum := sha256.Sum256([]byte(content))
	for _, c := range []struct{ what, got, want string }{
		{"name", file.Name, "purchase-order.pdf"},
		{"uri", file.URI.String(), "agk://run/" + fixtureRun + "/" + fixtureStep + "/out/purchase-order.pdf"},
		{"media_type", file.MediaType, "application/pdf"},
		{"sha256", file.SHA256, hex.EncodeToString(sum[:])},
	} {
		if c.got != c.want {
			t.Errorf("%s is %q, want %q", c.what, c.got, c.want)
		}
	}
	if file.Size != int64(len(content)) {
		t.Errorf("size is %d, want %d", file.Size, len(content))
	}

	// The bytes are under /agk/out/files/ because that is the one directory the runner
	// reads an artifact from: an entry naming a file in /tmp would point at nothing.
	laid, err := os.ReadFile(filepath.Join(h.env.outFilesDir(), file.Name))
	if err != nil {
		t.Fatalf("the bytes were not laid down: %s", err)
	}
	if string(laid) != content {
		t.Errorf("the bytes under %s are not the ones attached", OutFilesDir)
	}
}

// TestTheDigestStatesAFactTheRunnerStillChecks is the division of labour, held to by
// hashing the bytes that were laid down rather than the ones that were read.
func TestTheDigestStatesAFactTheRunnerStillChecks(t *testing.T) {
	h := newHarness(t)
	emitted(h, "out", `{"a":1}`)
	source := h.write("report.csv", "a,b\n1,2\n")
	h.ok("attach", source, "--port", "out")

	file := h.port("out").Items[0].Files[0]
	laid, err := os.ReadFile(filepath.Join(h.env.outFilesDir(), file.Name))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(laid)
	if file.SHA256 != hex.EncodeToString(sum[:]) {
		t.Error("the entry disagrees with the bytes under its own name, which is what the runner refuses")
	}
	if file.Size != int64(len(laid)) {
		t.Error("the size disagrees with the bytes under its own name")
	}
}

// TestTheMediaTypeIsAlwaysWritten holds the member a brick writing its own envelope is most
// likely to leave out.
func TestTheMediaTypeIsAlwaysWritten(t *testing.T) {
	for _, c := range []struct{ name, flag, want string }{
		{"report.csv", "", "text/csv"},
		{"report.json", "", "application/json"},
		{"numbers", "", "application/octet-stream"},
		{"report.csv", "text/plain; charset=utf-8", "text/plain; charset=utf-8"},
	} {
		h := newHarness(t)
		emitted(h, "out", `{"a":1}`)
		args := []string{"attach", h.write(c.name, "1,2\n"), "--port", "out"}
		if c.flag != "" {
			args = append(args, "--media-type", c.flag)
		}
		h.ok(args...)

		got := h.port("out").Items[0].Files[0].MediaType
		// The registered type may carry parameters of its own, so the type and
		// subtype are what is held.
		if !strings.HasPrefix(got, strings.Split(c.want, ";")[0]) {
			t.Errorf("%s got media_type %q, want %q", c.name, got, c.want)
		}
	}
}

// TestTheNameIsTheFilesOwnUnlessItIsGiven covers --name, and holds the name to being one
// segment of the URI, since a name with a separator in it would address one artifact and
// mount another.
func TestTheNameIsTheFilesOwnUnlessItIsGiven(t *testing.T) {
	h := newHarness(t)
	emitted(h, "out", `{"a":1}`)
	source := h.write("tmp-4821.json", `{"ok":true}`)

	h.ok("attach", source, "--port", "out", "--name", "response.json")
	file := h.port("out").Items[0].Files[0]
	if file.Name != "response.json" {
		t.Errorf("the name is %q, and --name said response.json", file.Name)
	}
	if file.URI.Name != "response.json" {
		t.Errorf("the URI ends on %q, and a file's name is the last segment of its URI", file.URI.Name)
	}

	// An empty --name is the flag's zero value and reads as not given, which is the
	// default above; the names refused here are the ones that are not one segment.
	for _, name := range []string{"a/b.json", "..", "with space.json", "a#b.json"} {
		h := newHarness(t)
		emitted(h, "out", `{"a":1}`)
		if code := h.run("attach", source, "--port", "out", "--name", name); code != 1 {
			t.Errorf("--name %q was accepted, and a name is one segment of the agk:// URI", name)
		}
	}
}

// TestTheItemIsNamedWhenThePortCarriesSeveral is the one case a guess would be invisible: a
// valid envelope, a successful run, and one item of several carrying the report.
func TestTheItemIsNamedWhenThePortCarriesSeveral(t *testing.T) {
	h := newHarness(t)
	emitted(h, "out", `[{"invoice":"INV-1"},{"invoice":"INV-2"}]`)
	source := h.write("report.csv", "a\n")

	says(t, h.refused("attach", source, "--port", "out"), "2 items", "--item", "--all")

	id := h.port("out").Items[1].ID
	h.ok("attach", source, "--port", "out", "--item", id)
	envelope := h.port("out")
	if len(envelope.Items[0].Files) != 0 {
		t.Error("the file reached the item nobody named")
	}
	if len(envelope.Items[1].Files) != 1 {
		t.Error("the file did not reach the item that was named")
	}
}

// TestAllAttachesToEveryItem covers --all, including the one upload the runner then makes
// of it: one name on one port is one artifact, whatever number of items reference it.
func TestAllAttachesToEveryItem(t *testing.T) {
	h := newHarness(t)
	emitted(h, "out", `[{"a":1},{"a":2},{"a":3}]`)
	h.ok("attach", h.write("terms.pdf", "terms\n"), "--port", "out", "--all")

	envelope := h.port("out")
	for i, item := range envelope.Items {
		if len(item.Files) != 1 {
			t.Fatalf("item %d carries %d files", i+1, len(item.Files))
		}
		if item.Files[0].URI != envelope.Items[0].Files[0].URI {
			t.Errorf("item %d addresses the shared artifact differently", i+1)
		}
	}
}

// TestItemAndAllAreOneOrTheOther keeps two answers to one question from both being given.
func TestItemAndAllAreOneOrTheOther(t *testing.T) {
	h := newHarness(t)
	emitted(h, "out", `{"a":1}`)
	says(t, h.refused("attach", h.write("f", "x"), "--port", "out", "--item", "x", "--all"), "--item", "--all")
}

// TestAnItemNobodyHasIsRefusedNamingTheCount keeps a stale identifier from attaching to
// nothing in silence.
func TestAnItemNobodyHasIsRefusedNamingTheCount(t *testing.T) {
	h := newHarness(t)
	emitted(h, "out", `{"a":1}`)
	says(t, h.refused("attach", h.write("f", "x"), "--port", "out", "--item", "01JMZ8W4K7A1B2C3D4E5F6G7H8"), "01JMZ8W4K7A1B2C3D4E5F6G7H8", "1 item")
}

// TestTheEmitComesFirst is the order of the two verbs, said rather than discovered: an
// envelope is what a files[] entry lives in.
func TestTheEmitComesFirst(t *testing.T) {
	h := newHarness(t)
	message := h.refused("attach", h.write("f", "x"), "--port", "out")
	says(t, message, "agk emit", portPath(h.env, "out"))
}

// TestAPortCarryingNoItemHasNothingToAttachTo covers the empty batch, where there is no
// item for the file to belong to.
func TestAPortCarryingNoItemHasNothingToAttachTo(t *testing.T) {
	h := newHarness(t)
	emitted(h, "out", `[]`)
	says(t, h.refused("attach", h.write("f", "x"), "--port", "out"), "no item")
	says(t, h.refused("attach", h.write("f", "x"), "--port", "out", "--all"), "none")
}

// TestOneNameOnOnePortIsOneArtifact is the addressing contradiction, refused rather than
// resolved by letting one set of bytes overwrite the other.
func TestOneNameOnOnePortIsOneArtifact(t *testing.T) {
	h := newHarness(t)
	emitted(h, "out", `[{"a":1},{"a":2}]`)
	first := h.write("report.csv", "first\n")
	second := filepath.Join(t.TempDir(), "report.csv")
	if err := os.WriteFile(second, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h.ok("attach", first, "--port", "out", "--item", h.port("out").Items[0].ID)
	message := h.refused("attach", second, "--port", "out", "--item", h.port("out").Items[1].ID)
	says(t, message, "report.csv", "one artifact")

	// The bytes that were there are still there, and the item that referenced them
	// still does.
	laid, err := os.ReadFile(filepath.Join(h.env.outFilesDir(), "report.csv"))
	if err != nil || string(laid) != "first\n" {
		t.Errorf("the refused attach overwrote the bytes: %q, %v", laid, err)
	}
}

// TestTheSameBytesUnderOneNameIsOrdinary is the other half of that rule: a fan-in of one
// shared document costs nothing and is not a contradiction.
func TestTheSameBytesUnderOneNameIsOrdinary(t *testing.T) {
	h := newHarness(t)
	emitted(h, "out", `[{"a":1},{"a":2}]`)
	source := h.write("terms.pdf", "terms\n")
	same := filepath.Join(t.TempDir(), "terms.pdf")
	if err := os.WriteFile(same, []byte("terms\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h.ok("attach", source, "--port", "out", "--item", h.port("out").Items[0].ID)
	h.ok("attach", same, "--port", "out", "--item", h.port("out").Items[1].ID)

	envelope := h.port("out")
	if envelope.Items[0].Files[0].SHA256 != envelope.Items[1].Files[0].SHA256 {
		t.Error("two items attaching one document disagree about its digest")
	}
}

// TestAFileAlreadyUnderItsNameIsNotCopiedOntoItself is the script that wrote straight into
// /agk/out/files/ and then attached what it wrote, which the documented after_script does.
func TestAFileAlreadyUnderItsNameIsNotCopiedOntoItself(t *testing.T) {
	h := newHarness(t)
	emitted(h, "out", `{"a":1}`)
	const content = "already here\n"
	inPlace := filepath.Join(h.env.outFilesDir(), "result.json")
	if err := os.WriteFile(inPlace, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	h.ok("attach", inPlace, "--port", "out")

	laid, err := os.ReadFile(inPlace)
	if err != nil {
		t.Fatal(err)
	}
	if string(laid) != content {
		t.Fatalf("the file was truncated by being copied onto itself: %q", laid)
	}
	file := h.port("out").Items[0].Files[0]
	sum := sha256.Sum256([]byte(content))
	if file.SHA256 != hex.EncodeToString(sum[:]) || file.Size != int64(len(content)) {
		t.Error("the entry does not describe the bytes that were already there")
	}
}

// TestASecondAttachOfOneNameIsACorrection holds the replacement rule: two entries of one
// name on one item would be the same artifact claimed twice.
func TestASecondAttachOfOneNameIsACorrection(t *testing.T) {
	h := newHarness(t)
	emitted(h, "out", `{"a":1}`)
	source := h.write("report.csv", "a\n")

	h.ok("attach", source, "--port", "out")
	h.ok("attach", source, "--port", "out", "--media-type", "text/plain")

	files := h.port("out").Items[0].Files
	if len(files) != 1 {
		t.Fatalf("the item carries %d entries of one name", len(files))
	}
	if files[0].MediaType != "text/plain" {
		t.Errorf("the entry carries media_type %q, and the second attach said text/plain", files[0].MediaType)
	}
}

// TestAttachRefusesWhatIsNotAFile keeps a directory from being described as bytes with a
// size and a digest.
func TestAttachRefusesWhatIsNotAFile(t *testing.T) {
	h := newHarness(t)
	emitted(h, "out", `{"a":1}`)
	says(t, h.refused("attach", t.TempDir(), "--port", "out"), "not a file")
	if code := h.run("attach", filepath.Join(t.TempDir(), "absent"), "--port", "out"); code != 1 {
		t.Error("a file that is not there was attached")
	}
}

// TestAttachNamesItsFileAndItsPort covers the two arguments it cannot do without.
func TestAttachNamesItsFileAndItsPort(t *testing.T) {
	h := newHarness(t)
	says(t, h.refused("attach"), "no file named")
	says(t, h.refused("attach", h.write("f", "x")), "no port named")
	says(t, h.refused("attach", h.write("f", "x"), "--port", "rejected"), "rejected", "out,error")
}

// TestAMediaTypeThatIsNotOneIsRefused holds the member to being a media type, which is what
// a consumer reads to know what the bytes are.
func TestAMediaTypeThatIsNotOneIsRefused(t *testing.T) {
	h := newHarness(t)
	emitted(h, "out", `{"a":1}`)
	says(t, h.refused("attach", h.write("f.bin", "x"), "--port", "out", "--media-type", "pdf"), "media type")
}

// TestTheFileIsTakenFromEitherSideOfTheFlags is the same rule for the other verb.
func TestTheFileIsTakenFromEitherSideOfTheFlags(t *testing.T) {
	h := newHarness(t)
	emitted(h, "out", `{"a":1}`)
	h.ok("attach", "--port", "out", h.write("report.csv", "a\n"))
	if n := len(h.port("out").Items[0].Files); n != 1 {
		t.Errorf("the file named after the flags was attached %d times", n)
	}
	says(t, h.refused("attach", "--port", "out", h.write("a.csv", "a\n"), h.write("b.csv", "b\n")), "b.csv")
}
