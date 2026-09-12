package driver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

// Masking covers the payload and not only the log. That is the reading doc.go records, and
// for as long as only the standard output shorthand was masked it was a reading the code did
// not keep: the shorthand is the one payload this side assembles, and the envelope a brick
// writes at /agk/out/ports/<port>.json is the payload every other brick produces.
//
// The adversary is not hypothetical. A brick that reads /agk/secrets/<name> and puts the value
// in a field of an item it publishes has it written to the object store, to the state of the
// run, to the envelope on disk and to standard output, in the clear, whatever the log says.
// The documentation is explicit about what literal matching buys and what it does not, and
// "the value as it arrived, in a field of an item" is the case it is for.

// TestAnEnvelopeABrickWroteIsMaskedAndNotOnlyTheShorthand is the one that was missing.
func TestAnEnvelopeABrickWroteIsMaskedAndNotOnlyTheShorthand(t *testing.T) {
	const secret = "s3cr3t-value"
	dir := outRoot(t)

	// Every place inside an envelope a value can sit: a string field, a field nested in an
	// object, a member of a list, and the text of a key.
	containerWrote(t, dir, "ok", anItem("01JMZ8W4K7A1B2C3D4E5F6G7H8", map[string]any{
		"token":  secret,
		"header": map[string]any{"authorization": "Bearer " + secret},
		"tried":  []any{secret, "something else"},
		secret:   "the key carried it",
	}))

	c := aCollection(brickTask("ok"), dir)
	c.Mask = newMasker([]byte(secret))

	got := gather(t, collectStore(t), c)

	// The whole envelope, as it would be published: a member this test forgot to name is
	// still a member a secret could travel in.
	doc, err := json.Marshal(got.Outputs["ok"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc), secret) {
		t.Errorf("the envelope carries the value in the clear:\n%s", doc)
	}
	if !strings.Contains(string(doc), maskToken) {
		t.Errorf("nothing in the envelope was masked, and four members of it carried the value:\n%s", doc)
	}
}

// TestMaskingAnEnvelopeLeavesEverythingThatIsNotAValueAlone is the other half, because a mask
// that rewrote an envelope would be worse than one that missed a secret.
//
// A number stays a number, a boolean stays a boolean, null stays null and an item's files are
// untouched: a files[] entry is a reference to bytes whose digest the envelope asserts and a
// consumer verifies, so rewriting a name or a digest would refuse a downstream step for a
// reason nobody could find. A brick that put a secret in an artifact's bytes is the case the
// documentation already answers: masking "is a guard against accident, never against intent",
// and the bytes of an artifact are not collected output.
func TestMaskingAnEnvelopeLeavesEverythingThatIsNotAValueAlone(t *testing.T) {
	const secret = "s3cr3t-value"
	dir := outRoot(t)
	s := collectStore(t)

	file := containerLeft(t, dir, "report.csv", "a,b\n1,2\n", "ok")
	containerWrote(t, dir, "ok", anItem("01JMZ8W4K7A1B2C3D4E5F6G7H8", map[string]any{
		"total":    88,
		"rate":     1.5,
		"settled":  true,
		"refunded": nil,
		"note":     "nothing secret here",
	}, file))

	c := aCollection(brickTask("ok"), dir)
	c.Mask = newMasker([]byte(secret))

	got := gather(t, s, c)
	item := got.Outputs["ok"].Items[0]

	// A number comes back as json.Number, because agk.Decode reads a payload with
	// UseNumber: an envelope's numbers travel as they were written rather than through a
	// float64 that would reprint 1e3 as 1000. That is also why the walk leaves them alone.
	for member, want := range map[string]any{
		"total":    json.Number("88"),
		"rate":     json.Number("1.5"),
		"settled":  true,
		"refunded": nil,
		"note":     "nothing secret here",
	} {
		if got := item.Data[member]; got != want {
			t.Errorf("%s came back as %#v, want %#v: masking replaces a value and changes nothing else", member, got, want)
		}
	}
	if item.ID != "01JMZ8W4K7A1B2C3D4E5F6G7H8" {
		t.Errorf("the item identity is %s: an identity is what a shard, a merge and a replay speak about", item.ID)
	}
	if len(item.Files) != 1 || item.Files[0].Name != "report.csv" {
		t.Fatalf("the item attaches %v, want the file the container left", item.Files)
	}
	if body := storedBytes(t, s, item.Files[0]); body != "a,b\n1,2\n" {
		t.Errorf("the store holds %q, want the bytes the container left", body)
	}
}

// TestATaskWithNoSecretsIsNotRewrittenAtAll, because a nil masker is the ordinary case: most
// tasks are given no secret, and a collection that copied every envelope for them would pay
// for a rule that cannot fire.
func TestATaskWithNoSecretsIsNotRewrittenAtAll(t *testing.T) {
	dir := outRoot(t)
	containerWrote(t, dir, "ok", anItem("01JMZ8W4K7A1B2C3D4E5F6G7H8", map[string]any{"total": 88}))

	c := aCollection(brickTask("ok"), dir)
	c.Mask = nil

	got := gather(t, collectStore(t), c)
	if n := got.Outputs["ok"].Items[0].Data["total"]; n != json.Number("88") {
		t.Errorf("total came back as %#v, want the number the container wrote", n)
	}
}

// maskedItems is the unit behind the three above, held on its own so that the shape of the
// walk is readable without a store or a directory in it.
func TestMaskingWalksEveryShapeAPayloadCanHave(t *testing.T) {
	m := newMasker([]byte("s3cr3t"))
	items := []agk.Item{{
		ID: "one",
		Data: map[string]any{
			"plain": "s3cr3t",
			"deep":  map[string]any{"deeper": []any{map[string]any{"here": "x s3cr3t y"}}},
			"list":  []any{"s3cr3t", json.Number("1"), true, nil},
		},
		Files: []agk.File{},
	}}

	masked := maskItems(m, items)
	doc, err := json.Marshal(masked)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc), "s3cr3t") {
		t.Errorf("a shape was walked past:\n%s", doc)
	}

	// The items handed in are not written into, because the caller may still be holding
	// them: brick.Collect returns envelopes this side then publishes, and a mask that
	// mutated them would make the collection order matter.
	if items[0].Data["plain"] != "s3cr3t" {
		t.Errorf("the items handed in were rewritten: %#v", items[0].Data)
	}
}
