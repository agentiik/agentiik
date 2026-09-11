package agk_test

import (
	"bytes"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/fixtures"
)

// at is the timestamp the documentation prints, so that what a test writes and what the
// documentation shows are the same document.
var at = time.Date(2026, 9, 10, 6, 0, 12, 418000000, time.UTC)

func sample(t *testing.T) agk.Envelope {
	t.Helper()
	return agk.Envelope{
		Meta: agk.Meta{
			RunID:      "01JMZ8W4K2R7Q0E3N5T9ZQ4XKB",
			Step:       "normalize",
			Port:       "ok",
			Attempt:    1,
			Count:      1,
			ProducedAt: at,
		},
		Items: []agk.Item{{
			ID:    "01JMZ8W4K7A1B2C3D4E5F6G7H8",
			Data:  map[string]any{"customer_id": "C-1042"},
			Files: []agk.File{},
		}},
	}
}

// TestTheReleasedCorpus holds this package to the fixtures agentiik/schemas releases:
// every document the corpus says is an envelope is read, and every document it says is
// not is refused, whole.
func TestTheReleasedCorpus(t *testing.T) {
	cases, err := fixtures.Envelopes()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.File, func(t *testing.T) {
			b, err := fs.ReadFile(fixtures.FS, c.File)
			if err != nil {
				t.Fatal(err)
			}
			e, err := agk.Decode(bytes.NewReader(b), agk.DefaultLimits())
			switch {
			case c.Valid && err != nil:
				t.Fatalf("refused a document the corpus accepts (%s): %v", c.Covers, err)
			case c.Valid:
				if e.Meta.Count != len(e.Items) {
					t.Fatalf("read %d items under a count of %d", len(e.Items), e.Meta.Count)
				}
			case err == nil:
				t.Fatalf("accepted a document the corpus refuses: %s", c.Rule)
			case !errors.Is(err, agk.ErrEnvelopeRejected):
				t.Fatalf("refused %s without rejecting the envelope: %v", c.File, err)
			}
		})
	}
}

// TestTheEnvelopeIsAClosedDocument states the rule the corpus states, on the members
// the corpus does not have a fixture for: a departure is refused whole, and the refusal
// names where in the document it is.
func TestTheEnvelopeIsAClosedDocument(t *testing.T) {
	const meta = `"meta":{"run_id":"01JMZ8W4K2R7Q0E3N5T9","step":"normalize","port":"ok","attempt":1,"count":1,"produced_at":"2026-09-10T06:00:12.418Z"}`
	const file = `{"name":"purchase-order.pdf","uri":"agk://run/01JMZ8W4K2R7Q0E3N5T9/normalize/ok/purchase-order.pdf","media_type":"application/pdf","size":481233}`

	cases := []struct {
		name  string
		doc   string
		names string
	}{
		{"an envelope carrying a member of its own", `{` + meta + `,"items":[],"shard":"3/8"}`, "shard"},
		{"an item carrying a member of its own", `{` + meta + `,"items":[{"id":"i1","data":{},"files":[],"shard":3}]}`, "items[0].shard"},
		{"an item whose data is a list", `{` + meta + `,"items":[{"id":"i1","data":[],"files":[]}]}`, "items[0].data"},
		{"an item whose data is null", `{` + meta + `,"items":[{"id":"i1","data":null,"files":[]}]}`, "items[0].data"},
		{"an item whose files are null", `{` + meta + `,"items":[{"id":"i1","data":{},"files":null}]}`, "items[0].files"},
		{"an item with an empty identifier", `{` + meta + `,"items":[{"id":"","data":{},"files":[]}]}`, "items[0].id"},
		{"an attached file carrying a member of its own", `{` + meta + `,"items":[{"id":"i1","data":{},"files":[{"name":"a.pdf","uri":"agk://run/r/s/p/a.pdf","media_type":"application/pdf","size":1,"sha256":"` + strings.Repeat("a", 64) + `","retain":"90d"}]}]}`, "items[0].files[0].retain"},
		{"a file addressed by a store key rather than a URI", `{` + meta + `,"items":[{"id":"i1","data":{},"files":[{"name":"a.pdf","uri":"sha256/` + strings.Repeat("a", 64) + `","media_type":"application/pdf","size":1,"sha256":"` + strings.Repeat("a", 64) + `"}]}]}`, "items[0].files[0].uri"},
		{"a file whose name is not the last segment of its URI", `{` + meta + `,"items":[{"id":"i1","data":{},"files":[{"name":"invoice.pdf","uri":"agk://run/r/s/p/a.pdf","media_type":"application/pdf","size":1,"sha256":"` + strings.Repeat("a", 64) + `"}]}]}`, "items[0].files[0].name"},
		{"a file whose media type is not one", `{` + meta + `,"items":[{"id":"i1","data":{},"files":[{"name":"a.pdf","uri":"agk://run/r/s/p/a.pdf","media_type":"pdf","size":1,"sha256":"` + strings.Repeat("a", 64) + `"}]}]}`, "items[0].files[0].media_type"},
		{"a file whose digest is in capitals", `{` + meta + `,"items":[{"id":"i1","data":{},"files":[{"name":"a.pdf","uri":"agk://run/r/s/p/a.pdf","media_type":"application/pdf","size":1,"sha256":"` + strings.Repeat("A", 64) + `"}]}]}`, "items[0].files[0].sha256"},
		{"metadata naming a step that is not an identifier", `{"meta":{"run_id":"r","step":"normalize step","port":"ok","attempt":1,"count":0,"produced_at":"2026-09-10T06:00:12.418Z"},"items":[]}`, "meta.step"},
		{"metadata with no publication time", `{"meta":{"run_id":"r","step":"normalize","port":"ok","attempt":1,"count":0},"items":[]}`, "meta.produced_at"},
		{"a publication time that is not a timestamp", `{"meta":{"run_id":"r","step":"normalize","port":"ok","attempt":1,"count":0,"produced_at":"yesterday"},"items":[]}`, "meta.produced_at"},
		{"a count that disagrees with the items", `{` + meta + `,"items":[]}`, "meta.count"},
		{"a second document after the first", `{` + meta + `,"items":[]}{"meta":{}}`, "not a JSON document"},
		{"a document that is not an object", `[]`, "an envelope is a JSON object"},
		{"an attached file that is not an object", `{` + meta + `,"items":[{"id":"i1","data":{},"files":["purchase-order.pdf"]}]}`, "items[0].files[0]"},
		{"an attached file missing its digest", `{` + meta + `,"items":[{"id":"i1","data":{},"files":[` + file + `]}]}`, "items[0].files[0].sha256"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := agk.Decode(strings.NewReader(c.doc), agk.DefaultLimits())
			if err == nil {
				t.Fatal("accepted")
			}
			if !errors.Is(err, agk.ErrEnvelopeRejected) {
				t.Fatalf("refused without rejecting the envelope: %v", err)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Fatalf("the refusal does not name %s: %v", c.names, err)
			}
		})
	}
}

// TestARefusalNamesTheStepAndThePort states what a person reading a log needs from it.
func TestARefusalNamesTheStepAndThePort(t *testing.T) {
	const doc = `{"meta":{"run_id":"01JMZ8W4K2R7Q0E3N5T9","step":"normalize","port":"ok","attempt":1,"count":1,"produced_at":"2026-09-10T06:00:12.418Z"},"items":[{"id":"","data":{},"files":[]}]}`

	_, err := agk.Decode(strings.NewReader(doc), agk.DefaultLimits())
	if err == nil {
		t.Fatal("accepted an item with no identifier")
	}
	for _, want := range []string{"step normalize", "port ok", "items[0].id", "envelope rejected"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not say %q: %v", want, err)
		}
	}
}

func TestEncodeWritesTheEmptyFormsRatherThanNull(t *testing.T) {
	e := agk.Envelope{
		Meta:  agk.Meta{RunID: "r", Step: "s", Port: "p", Attempt: 1, ProducedAt: at},
		Items: []agk.Item{{ID: "i1"}},
	}

	var b bytes.Buffer
	if _, err := e.Encode(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `"data":{}`) || !strings.Contains(b.String(), `"files":[]`) {
		t.Fatalf("an item with nothing attached was not written as empty: %s", b.String())
	}

	e.Items = nil
	b.Reset()
	if _, err := e.Encode(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `"items":[]`) {
		t.Fatalf("an envelope carrying nothing was not written as empty: %s", b.String())
	}
}

func TestEncodeAndDecodeAreTheSameDocument(t *testing.T) {
	e := sample(t)
	e.Items[0].Files = []agk.File{{
		Name:      "purchase-order.pdf",
		URI:       agk.URI{Run: e.Meta.RunID, Step: e.Meta.Step, Port: e.Meta.Port, Name: "purchase-order.pdf"},
		MediaType: "application/pdf",
		Size:      481233,
		SHA256:    strings.Repeat("c1f4a91d", 8),
	}}

	var first bytes.Buffer
	n, err := e.Encode(&first)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(first.Len()) {
		t.Fatalf("Encode says it wrote %d bytes and wrote %d", n, first.Len())
	}

	back, err := agk.Decode(bytes.NewReader(first.Bytes()), agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	if _, err := back.Encode(&second); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatalf("the document changed on the way through:\n%s\n%s", first.String(), second.String())
	}
}

// TestDecodeKeepsNumbersAsTheyWereWritten matters beyond tidiness: an envelope is
// content-addressed once it is written as an artifact, and a total that arrives as
// 1290.5 where the emitter wrote 1290.50 is a different document.
func TestDecodeKeepsNumbersAsTheyWereWritten(t *testing.T) {
	const doc = `{"meta":{"run_id":"r","step":"s","port":"p","attempt":1,"count":1,"produced_at":"2026-09-10T06:00:12.418Z"},"items":[{"id":"i1","data":{"total":1290.50,"id":9007199254740993},"files":[]}]}`

	e, err := agk.Decode(strings.NewReader(doc), agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if _, err := e.Encode(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"total":1290.50`, `"id":9007199254740993`} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("%s did not survive the trip: %s", want, b.String())
		}
	}
}

// TestAPortNobodyWroteIsNotAnError is the rule a declared output port relies on: the
// container wrote nothing, and what is published is an envelope holding nothing.
func TestAPortNobodyWroteIsNotAnError(t *testing.T) {
	e := agk.Empty("01JMZ8W4K2R7Q0E3N5T9ZQ4XKB", "normalize", "rejected", 1, at)

	if err := e.Validate(agk.DefaultLimits()); err != nil {
		t.Fatalf("an empty envelope was refused: %v", err)
	}
	if e.Meta.Count != 0 || len(e.Items) != 0 {
		t.Fatalf("Empty holds %d items under a count of %d", len(e.Items), e.Meta.Count)
	}

	var b bytes.Buffer
	if _, err := e.Encode(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `"items":[]`) {
		t.Fatalf("an empty envelope was not written as empty: %s", b.String())
	}
	back, err := agk.Decode(bytes.NewReader(b.Bytes()), agk.DefaultLimits())
	if err != nil {
		t.Fatalf("an empty envelope did not read back: %v", err)
	}
	if back.Meta.Port != "rejected" || back.Meta.ProducedAt != at {
		t.Fatalf("an empty envelope lost its metadata: %+v", back.Meta)
	}
}

func TestNewItemMintsAnIdentifier(t *testing.T) {
	item := agk.NewItem(map[string]any{"customer_id": "C-1042"})

	if len(item.ID) != 26 {
		t.Fatalf("NewItem minted %q, which is not a twenty-six character identifier", item.ID)
	}
	if item.Files == nil {
		t.Fatal("NewItem left files absent rather than empty")
	}
	if agk.NewItem(nil).ID == item.ID {
		t.Fatal("NewItem minted the same identifier twice")
	}
}
