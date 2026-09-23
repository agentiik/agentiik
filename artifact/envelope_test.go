package artifact_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
)

// oversized is an Objects holding one object far longer than an envelope may be, and counting
// what is read of it.
type oversized struct {
	size int64
	read *countingReader
}

func (o *oversized) Has(context.Context, string) (bool, error)    { return true, nil }
func (o *oversized) Put(context.Context, string, io.Reader) error { return errors.New("read only") }
func (o *oversized) Open(context.Context, string) (io.ReadCloser, error) {
	o.read = &countingReader{r: io.LimitReader(zeros{}, o.size)}
	return io.NopCloser(o.read), nil
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// A digest the controller reads back is one a runner named, and a runner can upload an object as
// long as an artifact may be. Reading stops one byte past what an envelope may be, rather than
// holding the whole object before finding out, and what was read is refused for good.
func TestAnEnvelopeIsReadNoFurtherThanAnEnvelopeMayRun(t *testing.T) {
	objects := &oversized{size: 64 << 20}
	l := agk.DefaultLimits()
	l.EnvelopeMaxBytes = 1024

	_, err := artifact.GetEnvelope(t.Context(), objects, "acme", digestOf([]byte("anything")), l)
	if !errors.Is(err, artifact.ErrNotAnEnvelope) {
		t.Errorf("an object of 64 MiB read back as an envelope answered %v", err)
	}
	if objects.read == nil {
		t.Fatal("the object was never opened")
	}
	if objects.read.n > l.EnvelopeMaxBytes+1 {
		t.Errorf("%d bytes were read of an object that could not be an envelope past the %d-th", objects.read.n, l.EnvelopeMaxBytes+1)
	}
}

// What a digest names and is not that envelope is the same on every read, and says so. What is not
// there, or could not be read, is not: an upload may land, and a store may answer next time.
func TestWhatIsNotTheEnvelopeADigestNamesIsToldApartFromWhatCouldNotBeRead(t *testing.T) {
	envelope := agk.Empty("01JMZ8W4K2R7Q0E3N5T9", "normalize", "ok", 1, time.Date(2026, 9, 10, 6, 0, 0, 0, time.UTC))
	var encoded bytes.Buffer
	if _, err := envelope.Encode(&encoded); err != nil {
		t.Fatal(err)
	}
	notJSON := []byte("{not an envelope")
	notAnEnvelope := []byte(`{"meta":{"port":"ok","count":3},"items":[]}`)
	other := digestOf([]byte("other"))

	for _, c := range []struct {
		name  string
		held  map[string][]byte
		asked string
		want  error
	}{
		{"bytes that are not JSON", map[string][]byte{digestOf(notJSON): notJSON}, digestOf(notJSON), artifact.ErrNotAnEnvelope},
		{"JSON that is not an envelope", map[string][]byte{digestOf(notAnEnvelope): notAnEnvelope}, digestOf(notAnEnvelope), artifact.ErrNotAnEnvelope},
		{"bytes another digest names", map[string][]byte{other: encoded.Bytes()}, other, artifact.ErrNotAnEnvelope},
		{"a digest nothing is held under", map[string][]byte{}, other, fs.ErrNotExist},
		{"the envelope itself", map[string][]byte{digestOf(encoded.Bytes()): encoded.Bytes()}, digestOf(encoded.Bytes()), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			objects := newMemory()
			for digest, b := range c.held {
				objects.blobs[artifact.Key("acme", digest)] = b
			}
			_, err := artifact.GetEnvelope(t.Context(), objects, "acme", c.asked, agk.DefaultLimits())
			switch {
			case c.want == nil && err != nil:
				t.Errorf("the envelope its digest names was refused: %s", err)
			case c.want != nil && !errors.Is(err, c.want):
				t.Errorf("it answered %v, want %v", err, c.want)
			case c.want != artifact.ErrNotAnEnvelope && errors.Is(err, artifact.ErrNotAnEnvelope):
				t.Errorf("it was refused for good, and it may read differently next time: %s", err)
			}
		})
	}
}

// A runner names its envelopes by digest before it uploads them, so the name it records is only
// worth anything if it is the one the upload writes. EnvelopeDigest answers what PutEnvelope then
// stores the envelope under, and the same length.
func TestAnEnvelopeIsNamedBeforeItIsStoredAsItIsStored(t *testing.T) {
	envelope := agk.Empty("01JMZ8W4K2R7Q0E3N5T9", "normalize", "ok", 1, time.Date(2026, 9, 10, 6, 0, 0, 0, time.UTC))
	envelope.Items = []agk.Item{agk.NewItem(map[string]any{"invoice": "INV-2026-0917"})}
	envelope.Meta.Count = 1

	named, size, err := artifact.EnvelopeDigest(envelope)
	if err != nil {
		t.Fatal(err)
	}
	objects := newMemory()
	stored, storedSize, err := artifact.PutEnvelope(t.Context(), objects, "acme", envelope)
	if err != nil {
		t.Fatal(err)
	}
	if named != stored || size != storedSize {
		t.Errorf("the envelope was named %s of %d bytes and stored as %s of %d", named, size, stored, storedSize)
	}
	if b := objects.blobs[artifact.Key("acme", named)]; digestOf(b) != named || int64(len(b)) != size {
		t.Errorf("the store holds %d bytes under %s, and they are not what that digest names", len(b), named)
	}
}
