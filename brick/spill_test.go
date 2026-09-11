package brick_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/brick"
)

// smallLimits keeps the threshold within reach of a test while leaving the other three
// rules where the documentation puts them.
func smallLimits(inline int64) agk.Limits {
	l := agk.DefaultLimits()
	l.InlineMaxBytes = inline
	return l
}

func read(t *testing.T, s *artifact.Store, f agk.File) []byte {
	t.Helper()
	rc, err := s.Open(t.Context(), f)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSpillMovesAValueAboveTheThresholdIntoFiles(t *testing.T) {
	s := newStore(t)
	heavy := strings.Repeat("a", 400)
	in := envelope("normalize", "ok", agk.Item{
		ID:    "01JMZ8W4K7A1B2C3D4E5",
		Data:  map[string]any{"customer_id": "C-1042", "body": heavy},
		Files: []agk.File{},
	})

	out, err := brick.Spill(t.Context(), s, in, smallLimits(64))
	if err != nil {
		t.Fatalf("Spill: %v", err)
	}

	item := out.Items[0]
	if _, ok := item.Data["body"]; ok {
		t.Error("the value above the threshold is still inline")
	}
	if item.Data["customer_id"] != "C-1042" {
		t.Error("a value under the threshold was moved")
	}
	if len(item.Files) != 1 {
		t.Fatalf("files: got %d, want the spilled value", len(item.Files))
	}

	file := item.Files[0]
	if file.MediaType != "application/json" {
		t.Errorf("media_type: got %q, want the type of what the artifact holds", file.MediaType)
	}
	if !strings.Contains(file.Name, "body") {
		t.Errorf("name: got %q, want a name the field can be read off", file.Name)
	}
	if file.URI.Run != in.Meta.RunID || file.URI.Step != in.Meta.Step || file.URI.Port != in.Meta.Port || file.URI.Name != file.Name {
		t.Errorf("uri: got %v, want agk://run/<run>/<step>/<port>/%s", file.URI, file.Name)
	}

	// What is stored is the JSON encoding of the value that left data, which is what a
	// consumer reads back with a JSON decoder.
	want, err := json.Marshal(heavy)
	if err != nil {
		t.Fatal(err)
	}
	got := read(t, s, file)
	if string(got) != string(want) {
		t.Fatalf("the artifact holds %q", got)
	}
	if file.Size != int64(len(want)) {
		t.Errorf("size: got %d, want %d", file.Size, len(want))
	}
	sum := sha256.Sum256(want)
	if file.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256: got %q, want the digest of the bytes stored", file.SHA256)
	}

	// The envelope handed in is not emptied underneath its caller.
	if _, ok := in.Items[0].Data["body"]; !ok {
		t.Error("Spill took the value out of the envelope it was given")
	}
	if len(in.Items[0].Files) != 0 {
		t.Error("Spill appended to the files of the envelope it was given")
	}
}

func TestTheThresholdIsWhatIsAboveIt(t *testing.T) {
	s := newStore(t)
	// The encoding of a string of n characters is n plus the two quotation marks, so
	// this value weighs exactly the threshold.
	exact := strings.Repeat("a", 62)
	out, err := brick.Spill(t.Context(), s, envelope("normalize", "ok", agk.Item{
		ID:    "01JMZ8W4K7A1B2C3D4E5",
		Data:  map[string]any{"body": exact},
		Files: []agk.File{},
	}), smallLimits(64))
	if err != nil {
		t.Fatalf("Spill: %v", err)
	}
	if len(out.Items[0].Files) != 0 {
		t.Fatal("a value at the threshold was spilled: above this, and not at it, is what the rule says")
	}
	if out.Items[0].Data["body"] != exact {
		t.Fatal("the value did not stay inline")
	}
}

func TestSpillLeavesAnEnvelopeWithNothingToSpillAlone(t *testing.T) {
	s := newStore(t)
	in := envelope("normalize", "ok",
		agk.Item{ID: "01JMZ8W4K7A1B2C3D4E5", Data: map[string]any{"total": 1290.50}, Files: []agk.File{}},
		agk.Item{ID: "01JMZ8W4K7A1B2C3D4E6", Data: map[string]any{}, Files: []agk.File{}},
	)
	out, err := brick.Spill(t.Context(), s, in, agk.DefaultLimits())
	if err != nil {
		t.Fatalf("Spill: %v", err)
	}
	if len(out.Items) != len(in.Items) {
		t.Fatalf("items: got %d, want %d", len(out.Items), len(in.Items))
	}
	for i, item := range out.Items {
		if len(item.Files) != 0 {
			t.Errorf("item %s: something was spilled", item.ID)
		}
		if item.ID != in.Items[i].ID {
			t.Errorf("item %d: identity changed from %s to %s", i, in.Items[i].ID, item.ID)
		}
	}
}

func TestSpillKeepsWhatTheItemAlreadyCarried(t *testing.T) {
	s := newStore(t)
	order := put(t, s, "normalize", "ok", "purchase-order.pdf", "the order")
	out, err := brick.Spill(t.Context(), s, envelope("normalize", "ok", agk.Item{
		ID:    "01JMZ8W4K7A1B2C3D4E5",
		Data:  map[string]any{"body": strings.Repeat("a", 400)},
		Files: []agk.File{order},
	}), smallLimits(64))
	if err != nil {
		t.Fatalf("Spill: %v", err)
	}
	files := out.Items[0].Files
	if len(files) != 2 {
		t.Fatalf("files: got %d, want the one carried and the one spilled", len(files))
	}
	if files[0] != order {
		t.Errorf("the file the item already carried was altered: %v", files[0])
	}
}

func TestTwoItemsSpillingOneFieldGetTwoNames(t *testing.T) {
	s := newStore(t)
	// Distinct values, so that two names are needed rather than one object serving both.
	out, err := brick.Spill(t.Context(), s, envelope("normalize", "ok",
		agk.Item{ID: "01JMZ8W4K7A1B2C3D4E5", Data: map[string]any{"body": strings.Repeat("a", 400)}, Files: []agk.File{}},
		agk.Item{ID: "01JMZ8W4K7A1B2C3D4E6", Data: map[string]any{"body": strings.Repeat("b", 400)}, Files: []agk.File{}},
	), smallLimits(64))
	if err != nil {
		t.Fatalf("Spill: %v", err)
	}
	first, second := out.Items[0].Files[0], out.Items[1].Files[0]
	if first.Name == second.Name {
		// One port's artifacts are laid side by side in one directory, and
		// agk://run/<run>/<step>/<port>/<name> addresses one of them.
		t.Fatalf("both items spilled under the name %q", first.Name)
	}
	// The identifier, not the rank, is what the name carries: an item keeps its identity
	// through a fan-out and a merge.
	if !strings.Contains(first.Name, "01JMZ8W4K7A1B2C3D4E5") {
		t.Errorf("name %q does not name the item that produced it", first.Name)
	}
}

func TestASpilledNameIsOneSegment(t *testing.T) {
	s := newStore(t)
	heavy := strings.Repeat("a", 400)
	out, err := brick.Spill(t.Context(), s, envelope("normalize", "ok", agk.Item{
		ID: "01JMZ8W4K7A1B2C3D4E5",
		Data: map[string]any{
			// A field of data is any JSON string, and a name is one path segment.
			"../escape":              heavy,
			"a field with spaces":    heavy,
			strings.Repeat("x", 300): heavy,
			"":                       heavy,
		},
		Files: []agk.File{},
	}), smallLimits(64))
	if err != nil {
		t.Fatalf("Spill: %v", err)
	}
	seen := make(map[string]bool)
	for _, f := range out.Items[0].Files {
		if strings.ContainsAny(f.Name, `/\`) || f.Name == "." || f.Name == ".." || f.Name == "" {
			t.Errorf("name %q is not one segment", f.Name)
		}
		if len(f.Name) > 255 {
			t.Errorf("name %q is longer than a file name may be", f.Name)
		}
		if seen[f.Name] {
			t.Errorf("name %q was used twice", f.Name)
		}
		seen[f.Name] = true
	}
	if len(seen) != 4 {
		t.Fatalf("spilled %d fields, want 4", len(seen))
	}
}

func TestWhatIsSpilledIsWhatTheRunnerWouldHaveRejected(t *testing.T) {
	s := newStore(t)
	limits := smallLimits(64)
	// The heavy value sits inside a field rather than being one, which is where the
	// runner names the most specific offender and Spill moves the field carrying it.
	in := envelope("normalize", "ok", agk.Item{
		ID: "01JMZ8W4K7A1B2C3D4E5",
		Data: map[string]any{
			"customer_id": "C-1042",
			"report":      map[string]any{"title": "September", "body": strings.Repeat("a", 400)},
		},
		Files: []agk.File{},
	})
	if err := in.Validate(limits); err == nil {
		t.Fatal("the envelope as built is one the runner would have published")
	}

	out, err := brick.Spill(t.Context(), s, in, limits)
	if err != nil {
		t.Fatalf("Spill: %v", err)
	}
	// An envelope the engine constructed and spilled is an envelope the engine's own
	// rule accepts, files[] entries and all.
	if err := out.Validate(limits); err != nil {
		t.Fatalf("the spilled envelope is still refused: %v", err)
	}
	if out.Items[0].Data["customer_id"] != "C-1042" {
		t.Error("a field under the threshold was moved")
	}
}

func TestAThresholdThatIsZeroSpillsNothing(t *testing.T) {
	s := newStore(t)
	// agk reads a limit of zero as a rule deliberately turned off. Turned off here, no
	// value leaves data, rather than every value leaving it.
	out, err := brick.Spill(t.Context(), s, envelope("normalize", "ok", agk.Item{
		ID:    "01JMZ8W4K7A1B2C3D4E5",
		Data:  map[string]any{"body": strings.Repeat("a", 400)},
		Files: []agk.File{},
	}), agk.Limits{})
	if err != nil {
		t.Fatalf("Spill: %v", err)
	}
	if len(out.Items[0].Files) != 0 {
		t.Fatal("a rule that is turned off spilled a value")
	}
}

// TestSpilledValueReachesTheNextBrickAsAPath is the whole of the threshold, end to end:
// a value the engine constructed goes to the store, travels as a files[] entry, and
// arrives in the next container as a file under its input mount.
func TestSpilledValueReachesTheNextBrickAsAPath(t *testing.T) {
	s := newStore(t)
	heavy := strings.Repeat("a", 400)
	spilled, err := brick.Spill(t.Context(), s, envelope("normalize", "ok", agk.Item{
		ID:    "01JMZ8W4K7A1B2C3D4E5",
		Data:  map[string]any{"body": heavy},
		Files: []agk.File{},
	}), smallLimits(64))
	if err != nil {
		t.Fatalf("Spill: %v", err)
	}

	dir := t.TempDir()
	mounts, err := brick.WriteInputs(t.Context(), s, dir, map[agk.Port]agk.Envelope{"in": spilled})
	if err != nil {
		t.Fatalf("WriteInputs: %v", err)
	}

	name := spilled.Items[0].Files[0].Name
	onDisk, err := os.ReadFile(filepath.Join(mounts[0].Source, name))
	if err != nil {
		t.Fatalf("the spilled value is not under the mount: %v", err)
	}
	var got string
	if err := json.Unmarshal(onDisk, &got); err != nil {
		t.Fatalf("the file under the mount is not the value: %v", err)
	}
	if got != heavy {
		t.Fatal("the value that came back is not the one that was spilled")
	}
}

// TestASpilledNameDoesNotLandOnAFileTheItemAlreadyCarries: one port lays every artifact
// it carries side by side in one directory, and agk://run/<run>/<step>/<port>/<name>
// addresses one of them, so a spilled field cannot take a name the port already holds.
func TestASpilledNameDoesNotLandOnAFileTheItemAlreadyCarries(t *testing.T) {
	s := newStore(t)
	// The name a field called report would spill under, already taken by an artifact
	// the item carries.
	taken := put(t, s, "normalize", "out", "01JMZ8W4K7A1B2C3D4E5-report.json", "the document")
	heavy := strings.Repeat("x", 300)

	e := envelope("normalize", "out", agk.Item{
		ID:    "01JMZ8W4K7A1B2C3D4E5",
		Data:  map[string]any{"report": heavy},
		Files: []agk.File{taken},
	})

	out, err := brick.Spill(t.Context(), s, e, agk.Limits{InlineMaxBytes: 128})
	if err != nil {
		t.Fatalf("Spill: %v", err)
	}
	names := make(map[string]int)
	for _, f := range out.Items[0].Files {
		names[f.Name]++
	}
	if len(out.Items[0].Files) != 2 {
		t.Fatalf("the item carries %d files, want the one it had and the one that spilled", len(out.Items[0].Files))
	}
	for name, n := range names {
		if n > 1 {
			t.Errorf("the port carries %q %d times, and one mount cannot hold both", name, n)
		}
	}
}
