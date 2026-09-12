package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/graph"
)

// The run, the step and the moment the ports are published, held still so that a URI an
// assertion writes out is the URI the collection built.
const (
	collectRun  agk.RunID = "01JMZ8W4K2R7Q0E3N5T9A1B2C3"
	collectStep agk.Step  = "normalize"
)

var collectAt = time.Date(2026, 9, 10, 6, 0, 12, 418000000, time.UTC)

// collectStore opens a store over a directory of this test's own, which is the local
// half of the same Objects a server run uses.
func collectStore(t *testing.T) *artifact.Store {
	t.Helper()
	s, err := artifact.New(artifact.Dir(t.TempDir()), "finance", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// brickTask is a step that runs a brick: an image, declared ports, and no script.
func brickTask(ports ...agk.Port) graph.Task {
	return graph.Task{
		ID:      agk.NewTaskID(collectRun, collectStep, 1, agk.Shard{}),
		Run:     collectRun,
		Step:    collectStep,
		Attempt: 1,
		Image:   "ghcr.io/agentiik/http-request@sha256:" + strings.Repeat("ab", 32),
		Outputs: ports,
	}
}

// collectScriptTask is a step written as a script, which is the only kind the shorthand
// is a rule about.
func collectScriptTask(ports ...agk.Port) graph.Task {
	t := brickTask(ports...)
	t.Script = []string{"echo hello"}
	return t
}

// outRoot is the host side of /agk/out, with the two directories a container writes
// under it.
func outRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{portsDir, filesDir} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// containerWrote lays one envelope down where the container would have written it.
func containerWrote(t *testing.T, dir string, port agk.Port, items ...agk.Item) {
	t.Helper()
	e := agk.Empty(collectRun, collectStep, port, 1, collectAt)
	e.Items = items
	e.Meta.Count = len(items)
	f, err := os.Create(filepath.Join(dir, portsDir, string(port)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := e.Encode(f); err != nil {
		t.Fatal(err)
	}
}

// containerLeft writes one file under /agk/out/files/ and returns the entry an envelope
// would reference it by, as a container computing its own digest would write it.
func containerLeft(t *testing.T, dir, name, content string, port agk.Port) agk.File {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filesDir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	return agk.File{
		Name:      name,
		URI:       agk.URI{Run: collectRun, Step: collectStep, Port: port, Name: name},
		MediaType: "text/csv",
		Size:      int64(len(content)),
		SHA256:    hex.EncodeToString(sum[:]),
	}
}

// anItem is one item of one envelope, with the files it attaches.
func anItem(id string, data map[string]any, files ...agk.File) agk.Item {
	if data == nil {
		data = map[string]any{}
	}
	if files == nil {
		files = []agk.File{}
	}
	return agk.Item{ID: id, Data: data, Files: files}
}

// gather runs the collection the way the driver will, with the defaults.
func gather(t *testing.T, s *artifact.Store, c collection) collected {
	t.Helper()
	got, err := collect(context.Background(), s, c)
	if err != nil {
		t.Fatalf("the collection failed: %v", err)
	}
	return got
}

// aCollection is what the collection is given for a task that exited 0.
func aCollection(task graph.Task, dir string) collection {
	return collection{Task: task, Dir: dir, ProducedAt: collectAt, Limits: agk.DefaultLimits()}
}

// storedBytes reads an artifact back through the store, which verifies the digest on the
// way, so an assertion about the bytes is an assertion about the reference too.
func storedBytes(t *testing.T, s *artifact.Store, f agk.File) string {
	t.Helper()
	r, err := s.Open(context.Background(), f)
	if err != nil {
		t.Fatalf("the artifact could not be read back: %v", err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("the artifact could not be read back: %v", err)
	}
	return string(b)
}

// One envelope per declared port, which is brick.Collect's rule and not this package's.
// What is held here is that collecting goes through it rather than reading the directory
// a second way.
func TestCollectPublishesOneEnvelopePerDeclaredPort(t *testing.T) {
	dir := outRoot(t)
	containerWrote(t, dir, "ok", anItem("01JMZ8W4K7A1B2C3D4E5F6G7H8", map[string]any{"total": 88}))

	got := gather(t, collectStore(t), aCollection(brickTask("ok", "rejected"), dir))

	if len(got.Outputs) != 2 {
		t.Fatalf("collected %d ports, want one envelope per declared port", len(got.Outputs))
	}
	if n := len(got.Outputs["rejected"].Items); n != 0 {
		t.Errorf("port rejected holds %d items, want the empty envelope a port nobody wrote publishes", n)
	}
	if got.Outputs["ok"].Meta.ProducedAt != collectAt.UTC() {
		t.Errorf("the port was published at %s, want the moment the runner stamped", got.Outputs["ok"].Meta.ProducedAt)
	}
}

// A file left under /agk/out/files/ and referenced by an envelope is uploaded, and the
// entry is rewritten to the artifact that now holds it.
func TestAFileTheContainerLeftIsUploadedAndReferenced(t *testing.T) {
	dir := outRoot(t)
	s := collectStore(t)
	file := containerLeft(t, dir, "report.csv", "a,b\n1,2\n", "ok")
	containerWrote(t, dir, "ok", anItem("01JMZ8W4K7A1B2C3D4E5F6G7H8", nil, file))

	got := gather(t, s, aCollection(brickTask("ok"), dir))

	items := got.Outputs["ok"].Items
	if len(items) != 1 || len(items[0].Files) != 1 {
		t.Fatalf("collected %d items, want the one the container wrote with its file", len(items))
	}
	published := items[0].Files[0]
	if want := "agk://run/" + string(collectRun) + "/" + string(collectStep) + "/ok/report.csv"; published.URI.String() != want {
		t.Errorf("the file is addressed %s, want %s", published.URI, want)
	}
	if published.SHA256 != file.SHA256 || published.Size != file.Size {
		t.Errorf("the entry says %s and %d bytes, want %s and %d", published.SHA256, published.Size, file.SHA256, file.Size)
	}
	if body := storedBytes(t, s, published); body != "a,b\n1,2\n" {
		t.Errorf("the store holds %q, want the bytes the container wrote", body)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0].URI != published.URI {
		t.Errorf("the artifacts are %v, want the one this task put in the store", got.Artifacts)
	}
}

// Two items of one envelope attaching the same file is ordinary, a fan-in of a shared
// document being the usual case, and the bytes are read once.
func TestOneFileReferencedTwiceIsUploadedOnce(t *testing.T) {
	dir := outRoot(t)
	s := collectStore(t)
	file := containerLeft(t, dir, "shared.csv", "a,b\n", "ok")
	containerWrote(t, dir, "ok",
		anItem("01JMZ8W4K7A1B2C3D4E5F6G7H8", nil, file),
		anItem("01JMZ8W4K7A1B2C3D4E5F6G7H9", nil, file),
	)

	got := gather(t, s, aCollection(brickTask("ok"), dir))

	if len(got.Artifacts) != 1 {
		t.Fatalf("the task put %d artifacts in the store, want one", len(got.Artifacts))
	}
	for _, item := range got.Outputs["ok"].Items {
		if item.Files[0].URI != got.Artifacts[0].URI {
			t.Errorf("item %s addresses %s, want the one artifact", item.ID, item.Files[0].URI)
		}
	}
}

// A files[] entry is already a reference. An item carried through from an input keeps the
// URI of the step that produced it, and nothing here claims it for this step.
func TestAFileCarriedThroughFromAnInputIsLeftAsItIs(t *testing.T) {
	dir := outRoot(t)
	upstream := agk.File{
		Name:      "invoice.pdf",
		URI:       agk.URI{Run: collectRun, Step: "fetch", Port: "out", Name: "invoice.pdf"},
		MediaType: "application/pdf",
		Size:      12,
		SHA256:    strings.Repeat("cd", 32),
	}
	containerWrote(t, dir, "ok", anItem("01JMZ8W4K7A1B2C3D4E5F6G7H8", nil, upstream))

	got := gather(t, collectStore(t), aCollection(brickTask("ok"), dir))

	published := got.Outputs["ok"].Items[0].Files[0]
	if published != upstream {
		t.Errorf("the entry was rewritten to %+v, want the reference the item arrived with", published)
	}
	if len(got.Artifacts) != 0 {
		t.Errorf("the task claimed %v, and those are the step upstream's", got.Artifacts)
	}
}

// The envelope is a document about bytes, and a consumer verifies the digest when it
// reads the artifact back. A contradiction left alone would fail a downstream step with
// nothing to say where it came from.
func TestAFileWhoseBytesAreNotWhatTheEnvelopeSaysIsRefused(t *testing.T) {
	dir := outRoot(t)
	file := containerLeft(t, dir, "report.csv", "a,b\n", "ok")
	file.SHA256 = strings.Repeat("ef", 32)
	containerWrote(t, dir, "ok", anItem("01JMZ8W4K7A1B2C3D4E5F6G7H8", nil, file))

	_, err := collect(context.Background(), collectStore(t), aCollection(brickTask("ok"), dir))
	if err == nil {
		t.Fatal("an envelope naming bytes the container did not write was collected")
	}
	if !errors.Is(err, agk.ErrEnvelopeRejected) {
		t.Errorf("the refusal does not say the envelope is rejected: %v", err)
	}
	for _, want := range []string{"step " + string(collectStep), "port ok", "/agk/out/files/report.csv"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
}

// What is read here is read on the runner's side of the boundary. A link would have the
// collection read a file the container could not reach itself.
func TestSomethingThatIsNotAFileUnderTheFilesDirectoryIsRefused(t *testing.T) {
	dir := outRoot(t)
	if err := os.Mkdir(filepath.Join(dir, filesDir, "report.csv"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry := agk.File{
		Name:      "report.csv",
		URI:       agk.URI{Run: collectRun, Step: collectStep, Port: "ok", Name: "report.csv"},
		MediaType: "text/csv",
		Size:      4,
		SHA256:    strings.Repeat("ab", 32),
	}
	containerWrote(t, dir, "ok", anItem("01JMZ8W4K7A1B2C3D4E5F6G7H8", nil, entry))

	_, err := collect(context.Background(), collectStore(t), aCollection(brickTask("ok"), dir))
	if err == nil || !errors.Is(err, agk.ErrEnvelopeRejected) {
		t.Fatalf("a directory under /agk/out/files/ was read as an artifact: %v", err)
	}
}

// A script that writes nothing and exits 0 publishes, on out, one item carrying its
// captured standard output and any file it left in /agk/out/files/.
func TestTheShorthandPublishesStandardOutputAndWhatWasLeftBesideIt(t *testing.T) {
	dir := outRoot(t)
	s := collectStore(t)
	if err := os.WriteFile(filepath.Join(dir, filesDir, "result.json"), []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := aCollection(collectScriptTask("out"), dir)
	c.Stdout = []byte("hello\n")

	got := gather(t, s, c)

	items := got.Outputs["out"].Items
	if len(items) != 1 {
		t.Fatalf("published %d items, want the one the shorthand publishes", len(items))
	}
	if got := items[0].Data[StdoutField]; got != "hello\n" {
		t.Errorf("the item carries %q under %s, want what the script printed", got, StdoutField)
	}
	if len(items[0].Files) != 1 || items[0].Files[0].Name != "result.json" {
		t.Fatalf("the item carries %v, want the file the script left", items[0].Files)
	}
	if body := storedBytes(t, s, items[0].Files[0]); body != `{"ok":true}` {
		t.Errorf("the store holds %q, want the bytes the script left", body)
	}
	// A name ending .json says more than "bytes" for nothing, and a consumer left to
	// guess is a consumer that guesses differently.
	if mt := items[0].Files[0].MediaType; !strings.HasPrefix(mt, "application/json") {
		t.Errorf("the file travels as %q, want the type its name gives it", mt)
	}
	if got.Outputs["out"].Meta.Count != 1 {
		t.Errorf("count says %d and the envelope holds one item", got.Outputs["out"].Meta.Count)
	}
}

// The captured standard output becomes the payload of an item, so a secret echoed there
// would be stored in the clear. The rule that covers the log covers this too.
func TestTheShorthandMasksWhatTheScriptEchoed(t *testing.T) {
	dir := outRoot(t)
	c := aCollection(collectScriptTask("out"), dir)
	c.Stdout = []byte("token=s3cr3t-value\n")
	c.Mask = newMasker([]byte("s3cr3t-value"))

	got := gather(t, collectStore(t), c)

	payload, _ := got.Outputs["out"].Items[0].Data[StdoutField].(string)
	if strings.Contains(payload, "s3cr3t-value") {
		t.Fatalf("the value was published in the clear: %q", payload)
	}
	if want := "token=" + maskToken + "\n"; payload != want {
		t.Errorf("published %q, want %q", payload, want)
	}
}

// A brick that writes nothing under /agk/out/ports/ and exits 0 publishes empty
// envelopes, which is the whole of what it owes the contract. A file it left that
// nothing references is not uploaded: it would be bytes in the store no envelope
// addresses.
func TestABrickThatWroteNothingPublishesEmptyEnvelopesAndUploadsNothing(t *testing.T) {
	dir := outRoot(t)
	s := collectStore(t)
	if err := os.WriteFile(filepath.Join(dir, filesDir, "stray.csv"), []byte("a,b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := aCollection(brickTask("out"), dir)
	c.Stdout = []byte("diagnostics only\n")

	got := gather(t, s, c)

	if n := len(got.Outputs["out"].Items); n != 0 {
		t.Errorf("the brick published %d items, want the empty envelope", n)
	}
	if len(got.Artifacts) != 0 {
		t.Errorf("the task put %v in the store, and nothing references them", got.Artifacts)
	}
}

// A script that wrote one of its ports has said what it publishes. Adding a batch it did
// not ask for would put items on an edge the author did not write.
func TestCollectingAScriptThatWroteAPortAddsNoShorthand(t *testing.T) {
	dir := outRoot(t)
	containerWrote(t, dir, "out", anItem("01JMZ8W4K7A1B2C3D4E5F6G7H8", map[string]any{"total": 3}))
	c := aCollection(collectScriptTask("out"), dir)
	c.Stdout = []byte("hello\n")

	got := gather(t, collectStore(t), c)

	items := got.Outputs["out"].Items
	if len(items) != 1 {
		t.Fatalf("published %d items", len(items))
	}
	if _, ok := items[0].Data[StdoutField]; ok {
		t.Errorf("the shorthand overwrote what the script published: %v", items[0].Data)
	}
}

// The shorthand is a rule about a script that exited 0. A script that failed publishes
// nothing on its behalf.
func TestAScriptThatFailedGetsNoShorthandAndUploadsNothing(t *testing.T) {
	dir := outRoot(t)
	s := collectStore(t)
	if err := os.WriteFile(filepath.Join(dir, filesDir, "half.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := aCollection(collectScriptTask("out"), dir)
	c.Stdout = []byte("hello\n")
	c.Code = 1

	got := gather(t, s, c)

	if n := len(got.Outputs["out"].Items); n != 0 {
		t.Errorf("a script that exited 1 published %d items", n)
	}
	if len(got.Artifacts) != 0 {
		t.Errorf("a script that exited 1 put %v in the store", got.Artifacts)
	}
}

// The documentation names out and no other port. A script declaring different ports gets
// no shorthand, and nothing it left is uploaded either: those bytes would be artifacts no
// envelope addresses.
func TestAScriptDeclaringNoOutPortUploadsNothing(t *testing.T) {
	dir := outRoot(t)
	s := collectStore(t)
	if err := os.WriteFile(filepath.Join(dir, filesDir, "result.json"), []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := aCollection(collectScriptTask("result"), dir)
	c.Stdout = []byte("hello\n")

	got := gather(t, s, c)

	if n := len(got.Outputs["result"].Items); n != 0 {
		t.Errorf("the shorthand published %d items on a port the documentation does not name", n)
	}
	if len(got.Artifacts) != 0 {
		t.Errorf("the task put %v in the store, and no item references them", got.Artifacts)
	}
}

// The other half of the size threshold: a value the engine itself constructed above
// inline_max_bytes is moved into the store and referenced in files[], so that the engine
// never publishes an envelope its own rule would reject.
func TestAValueAboveTheThresholdIsSpilled(t *testing.T) {
	dir := outRoot(t)
	s := collectStore(t)
	c := aCollection(collectScriptTask("out"), dir)
	c.Stdout = []byte(strings.Repeat("x", 4096))
	c.Limits = agk.Limits{InlineMaxBytes: 64, EnvelopeMaxBytes: agk.DefaultEnvelopeMaxBytes, MaxItems: agk.DefaultMaxItems, ArtifactMaxBytes: agk.DefaultArtifactMaxBytes}

	got := gather(t, s, c)

	item := got.Outputs["out"].Items[0]
	if _, ok := item.Data[StdoutField]; ok {
		t.Errorf("the value stayed inline, above the threshold")
	}
	if len(item.Files) != 1 {
		t.Fatalf("the spilled value is referenced by %d files, want one", len(item.Files))
	}
	if body := storedBytes(t, s, item.Files[0]); !strings.Contains(body, "xxxx") {
		t.Errorf("the store does not hold what left the item")
	}
	if len(got.Artifacts) != 1 {
		t.Errorf("the task put %d artifacts in the store, want the spilled one", len(got.Artifacts))
	}
}

// What travels is what this side assembled, so the size rules are measured on the
// document that will be published rather than on the one the container wrote.
func TestWhatIsPublishedIsMeasuredAsItWillTravel(t *testing.T) {
	dir := outRoot(t)
	c := aCollection(collectScriptTask("out"), dir)
	c.Stdout = []byte(strings.Repeat("x", 4096))
	// No spill, because inline_max_bytes is off, and an envelope too small to hold
	// what the shorthand put in it.
	c.Limits = agk.Limits{EnvelopeMaxBytes: 256}

	_, err := collect(context.Background(), collectStore(t), c)
	if err == nil {
		t.Fatal("an envelope above envelope_max_bytes was published")
	}
	if !errors.Is(err, agk.ErrStepFailed) {
		t.Errorf("the refusal is not an application failure of the step: %v", err)
	}
}

// A collection with no store is a collection that cannot upload what the container left.
func TestCollectingWithoutAStore(t *testing.T) {
	if _, err := collect(context.Background(), nil, aCollection(brickTask("out"), outRoot(t))); err == nil {
		t.Fatal("the collection ran with no store to upload to")
	}
}

// The capture is bounded, because a container that prints without stopping would
// otherwise exhaust the runner's memory rather than its own.
func TestTheCaptureIsBoundedAndKeepsTheBeginning(t *testing.T) {
	c := newCapture(8, nil)

	if n, err := c.Write([]byte("abcdefghijkl")); n != 12 || err != nil {
		t.Fatalf("the capture stopped the reader short: %d %v", n, err)
	}
	if got := string(c.Bytes()); got != "abcdefgh" {
		t.Errorf("captured %q, want the first eight bytes", got)
	}
	if !c.Truncated() {
		t.Errorf("the capture does not say it was cut short")
	}
}

func TestAnUnboundedCaptureKeepsEverything(t *testing.T) {
	c := newCapture(0, nil)

	c.Write([]byte("one "))
	c.Write([]byte("two"))
	if got := string(c.Bytes()); got != "one two" {
		t.Errorf("captured %q", got)
	}
	if c.Truncated() {
		t.Errorf("a capture that dropped nothing says it was cut short")
	}
}

// The cut fell wherever the limit landed, which may be inside a value. What goes with
// the rest is everything that could still have been the beginning of one.
func TestACutCaptureDropsTheBytesAValueCouldHaveBegunIn(t *testing.T) {
	c := newCapture(10, newMasker([]byte("s3cr3t")))

	c.Write([]byte("aaaas3cr3t-and-more"))
	if got := string(c.Bytes()); strings.Contains(got, "s3cr") {
		t.Fatalf("half a value survived the cut: %q", got)
	}
}

func TestTheCaptureMasksWhatItKeeps(t *testing.T) {
	c := newCapture(0, newMasker([]byte("s3cr3t")))

	c.Write([]byte("token=s3cr3t\n"))
	if got := string(c.Bytes()); strings.Contains(got, "s3cr3t") {
		t.Fatalf("the capture handed back a value in the clear: %q", got)
	}
}
