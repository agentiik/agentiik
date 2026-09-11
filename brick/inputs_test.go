package brick_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/brick"
)

const namespace = "acme"

func newStore(t *testing.T) *artifact.Store {
	t.Helper()
	s, err := artifact.New(artifact.Dir(t.TempDir()), namespace, agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func envelope(step agk.Step, port agk.Port, items ...agk.Item) agk.Envelope {
	return agk.Envelope{
		Meta: agk.Meta{
			RunID:      "01JMZ8W4K2R7Q0E3N5T9",
			Step:       step,
			Port:       port,
			Attempt:    1,
			Count:      len(items),
			ProducedAt: time.Date(2026, 9, 10, 6, 0, 12, 418000000, time.UTC),
		},
		Items: items,
	}
}

func put(t *testing.T, s *artifact.Store, step agk.Step, port agk.Port, name, content string) agk.File {
	t.Helper()
	file, err := s.Put(t.Context(), agk.URI{Run: "01JMZ8W4K2R7Q0E3N5T9", Step: step, Port: port, Name: name}, "text/plain", strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestWriteInputsGivesTheBrickADirectoryPerPort(t *testing.T) {
	s := newStore(t)
	order := put(t, s, "normalize", "ok", "purchase-order.pdf", "the order")
	dir := t.TempDir()

	mounts, err := brick.WriteInputs(t.Context(), s, dir, map[agk.Port]agk.Envelope{
		// The key is the port of the consuming step, and it is the directory the file
		// lands under, whatever the port it was emitted from was called.
		"in":    envelope("normalize", "ok", agk.Item{ID: "01JMZ8W4K7A1B2C3D4E5", Data: map[string]any{"total": 88}, Files: []agk.File{order}}),
		"extra": envelope("collect", "out"),
	})
	if err != nil {
		t.Fatalf("WriteInputs: %v", err)
	}

	if len(mounts) != 2 {
		t.Fatalf("mounts: got %d, want one per port", len(mounts))
	}
	// Sorted, so that the same step prepares the same mounts in the same order twice.
	if mounts[0].Port != "extra" || mounts[1].Port != "in" {
		t.Fatalf("mounts are not in port order: %v", mounts)
	}
	for _, m := range mounts {
		if !m.ReadOnly {
			t.Errorf("mount %s: an input a step can write to is an input a retry reads differently", m.Port)
		}
		if want := "/agk/in/" + string(m.Port); m.Target != want {
			t.Errorf("mount %s: Target is %q, want %q", m.Port, m.Target, want)
		}
		if want := filepath.Join(dir, string(m.Port)); m.Source != want {
			t.Errorf("mount %s: Source is %q, want %q", m.Port, m.Source, want)
		}
		if _, err := os.Stat(filepath.Join(m.Source, "envelope.json")); err != nil {
			t.Errorf("mount %s: no envelope.json: %v", m.Port, err)
		}
	}

	// The brick opens a path, and the bytes are there under the name the file entry
	// gave, beside the envelope.
	bytesOnDisk, err := os.ReadFile(filepath.Join(dir, "in", "purchase-order.pdf"))
	if err != nil {
		t.Fatalf("the artifact is not under the mount: %v", err)
	}
	if string(bytesOnDisk) != "the order" {
		t.Fatalf("the artifact holds %q", bytesOnDisk)
	}
	info, err := os.Stat(filepath.Join(dir, "in", "purchase-order.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Errorf("mode: got %v, want an input nothing can write to", info.Mode().Perm())
	}
}

func TestTheEnvelopeUnderTheMountKeepsTheLogicalURI(t *testing.T) {
	s := newStore(t)
	order := put(t, s, "normalize", "ok", "purchase-order.pdf", "the order")
	dir := t.TempDir()

	if _, err := brick.WriteInputs(t.Context(), s, dir, map[agk.Port]agk.Envelope{
		"in": envelope("normalize", "ok", agk.Item{ID: "01JMZ8W4K7A1B2C3D4E5", Data: map[string]any{}, Files: []agk.File{order}}),
	}); err != nil {
		t.Fatalf("WriteInputs: %v", err)
	}

	written, err := os.ReadFile(filepath.Join(dir, "in", "envelope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), order.URI.String()) {
		t.Errorf("the envelope under the mount does not carry the logical URI: %s", written)
	}
	// The URI stays logical and unfetchable from inside the container: the physical key
	// is the store's business and never travels.
	if strings.Contains(string(written), artifact.Key(namespace, order.SHA256)) {
		t.Errorf("the envelope under the mount carries a physical key: %s", written)
	}
}

func TestWriteInputsRefusesWhatItCouldNotLayDown(t *testing.T) {
	s := newStore(t)
	order := put(t, s, "normalize", "ok", "purchase-order.pdf", "the order")
	other := put(t, s, "normalize", "ok", "purchase-order.pdf", "a different order")

	renamed := func(f agk.File, name string) agk.File {
		f.Name = name
		return f
	}
	tests := []struct {
		name string
		in   map[agk.Port]agk.Envelope
		says string
	}{
		{
			name: "a file name that leaves the mount",
			in: map[agk.Port]agk.Envelope{
				"in": envelope("normalize", "ok", agk.Item{ID: "01JMZ8W4K7A1B2C3D4E5", Data: map[string]any{}, Files: []agk.File{renamed(order, "../escape")}}),
			},
			says: "escape",
		},
		{
			name: "a file name that is a path",
			in: map[agk.Port]agk.Envelope{
				"in": envelope("normalize", "ok", agk.Item{ID: "01JMZ8W4K7A1B2C3D4E5", Data: map[string]any{}, Files: []agk.File{renamed(order, "nested/order.pdf")}}),
			},
			says: "nested/order.pdf",
		},
		{
			name: "a file entry with no name",
			in: map[agk.Port]agk.Envelope{
				"in": envelope("normalize", "ok", agk.Item{ID: "01JMZ8W4K7A1B2C3D4E5", Data: map[string]any{}, Files: []agk.File{renamed(order, "")}}),
			},
			says: "name",
		},
		{
			name: "one name on one port carrying two sets of bytes",
			in: map[agk.Port]agk.Envelope{
				"in": envelope("normalize", "ok",
					agk.Item{ID: "01JMZ8W4K7A1B2C3D4E5", Data: map[string]any{}, Files: []agk.File{order}},
					agk.Item{ID: "01JMZ8W4K7A1B2C3D4E6", Data: map[string]any{}, Files: []agk.File{other}},
				),
			},
			says: "purchase-order.pdf",
		},
		{
			name: "a port that is not an identifier",
			in: map[agk.Port]agk.Envelope{
				"in.raw": envelope("normalize", "ok"),
			},
			says: "in.raw",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := brick.WriteInputs(t.Context(), s, dir, tt.in)
			if err == nil {
				t.Fatal("WriteInputs: accepted it")
			}
			if !strings.Contains(err.Error(), tt.says) {
				t.Errorf("the message does not say what was refused: %v", err)
			}
			// A name refused is a name never laid down: nothing is written beside the
			// mount it was meant to stay inside.
			if _, err := os.Stat(filepath.Join(dir, "escape")); err == nil {
				t.Fatal("a file was written outside the mount")
			}
		})
	}
}

func TestOneNameOnOnePortCarryingTheSameBytesIsWrittenOnce(t *testing.T) {
	s := newStore(t)
	order := put(t, s, "normalize", "ok", "purchase-order.pdf", "the order")
	dir := t.TempDir()

	// A fan-in where two items carry the same document is ordinary, and the second entry
	// costs nothing.
	if _, err := brick.WriteInputs(t.Context(), s, dir, map[agk.Port]agk.Envelope{
		"in": envelope("normalize", "ok",
			agk.Item{ID: "01JMZ8W4K7A1B2C3D4E5", Data: map[string]any{}, Files: []agk.File{order}},
			agk.Item{ID: "01JMZ8W4K7A1B2C3D4E6", Data: map[string]any{}, Files: []agk.File{order}},
		),
	}); err != nil {
		t.Fatalf("WriteInputs: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "in"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("the mount holds %d entries, want the envelope and one file", len(entries))
	}
}

func TestAnEmptyEnvelopeStillArrivesAsAMount(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()

	// A port that was declared and never written publishes an empty envelope, which is
	// not an error, and the step downstream still reads a directory.
	mounts, err := brick.WriteInputs(t.Context(), s, dir, map[agk.Port]agk.Envelope{
		"in": envelope("normalize", "ok"),
	})
	if err != nil {
		t.Fatalf("WriteInputs: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("mounts: got %d, want one", len(mounts))
	}
	if _, err := os.Stat(filepath.Join(dir, "in", "envelope.json")); err != nil {
		t.Fatalf("no envelope.json under the mount: %v", err)
	}
}

func TestWriteInputsIsRunAgainOnARetry(t *testing.T) {
	s := newStore(t)
	order := put(t, s, "normalize", "ok", "purchase-order.pdf", "the order")
	dir := t.TempDir()
	in := map[agk.Port]agk.Envelope{
		"in": envelope("normalize", "ok", agk.Item{ID: "01JMZ8W4K7A1B2C3D4E5", Data: map[string]any{}, Files: []agk.File{order}}),
	}
	for attempt := range 2 {
		// The inputs of the first attempt are read-only, and the second attempt has to
		// be able to lay the same files down again.
		if _, err := brick.WriteInputs(t.Context(), s, dir, in); err != nil {
			t.Fatalf("attempt %d: %v", attempt+1, err)
		}
	}
}

// TestAnArtifactCannotBeCalledEnvelopeJSON holds the mount to one file per name. The
// envelope of the port is one of the files the mount holds, so its name is spoken for:
// laying an artifact down over it would hand the brick something other than its batch
// where it reads its batch.
func TestAnArtifactCannotBeCalledEnvelopeJSON(t *testing.T) {
	s := newStore(t)
	collides := put(t, s, "normalize", "ok", "envelope.json", "not the envelope")
	dir := t.TempDir()

	_, err := brick.WriteInputs(t.Context(), s, dir, map[agk.Port]agk.Envelope{
		"in": envelope("normalize", "ok", agk.Item{ID: "01JMZ8W4K7A1B2C3D4E5", Data: map[string]any{}, Files: []agk.File{collides}}),
	})
	if err == nil {
		t.Fatal("an artifact was laid down over the envelope of its own port")
	}
	if !strings.Contains(err.Error(), "envelope.json") {
		t.Errorf("the refusal does not name the file: %v", err)
	}
}
