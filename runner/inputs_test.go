package runner

import (
	"errors"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
)

// invoiceEnvelope is one item naming one stored artifact.
func invoiceEnvelope(t *testing.T, s *objectStore) agk.Envelope {
	t.Helper()
	pdf := []byte("%PDF-1.7 the whole of an invoice")
	e := agk.Empty(storeRun, "fetch", "out", 1, time.Date(2026, 9, 10, 6, 0, 0, 0, time.UTC))
	item := agk.NewItem(map[string]any{"invoice": "INV-2026-0917"})
	item.Files = []agk.File{{
		Name: "invoice.pdf", URI: agk.URI{Run: storeRun, Step: "fetch", Port: "out", Name: "invoice.pdf"},
		MediaType: "application/pdf", Size: int64(len(pdf)), SHA256: s.put(t, pdf),
	}}
	e.Items = []agk.Item{item}
	e.Meta.Count = 1
	return e
}

// An envelope naming an artifact the redemption gives no URL for is a redemption that does not
// answer the task, found before the driver would find it laying the inputs down.
func TestAnArtifactTheRedemptionGivesNoURLForIsRefused(t *testing.T) {
	s := newObjectStore(t)
	m, r := s.taskFor(t, map[agk.Port]agk.Envelope{"in": invoiceEnvelope(t, s)}, nil, nil)
	r.Inputs[0].Artifacts = []RedeemedArtifact{}
	_, err := Assemble(t.Context(), m, r, Assembly{WorkRoot: t.TempDir()})
	if !errors.Is(err, ErrAnswerUnusable) {
		t.Errorf("an artifact with no URL answered %v", err)
	}
}

// A URL the store refuses is a fetch that may pass, since redeeming again mints fresh ones, and is
// neither of the two refusals that never pass.
func TestAnEnvelopeTheStoreWillNotServeIsAFetchThatMayPass(t *testing.T) {
	s := newObjectStore(t)
	m, r := s.taskFor(t, map[agk.Port]agk.Envelope{"in": invoiceEnvelope(t, s)}, nil, nil)
	hex, _ := hexOf(r.Inputs[0].Envelope.Digest)
	expired, err := s.signed.Presign(t.Context(), artifact.MethodGet, artifact.Key("finance", hex), storeRun, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	r.Inputs[0].Envelope.URL = expired
	_, err = Assemble(t.Context(), m, r, Assembly{WorkRoot: t.TempDir()})
	switch {
	case err == nil:
		t.Fatal("an envelope was read through an expired URL")
	case errors.Is(err, ErrNotAsNamed), errors.Is(err, ErrAnswerUnusable):
		t.Errorf("an expired URL is refused as one that never passes: %s", err)
	case !errors.Is(err, artifact.ErrNotSigned):
		t.Errorf("an expired URL answered %s", err)
	}
}
