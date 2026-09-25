package diff

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// The fixture is one port carrying one item with one attached file, which is the smallest
// envelope that exercises every member a difference can be about.
func envelopeOf(run agk.RunID, at time.Time) agk.Envelope {
	return agk.Envelope{
		Meta: agk.Meta{
			RunID: run, Step: "invoice", Port: "out",
			Attempt: 1, Count: 1, ProducedAt: at,
		},
		Items: []agk.Item{{
			ID:   "01JMZ8V1P9C4XQ7K2N4D6F8H0B",
			Data: map[string]any{"vat_number": "FR12345678901", "amount": json.Number("120.50")},
			Files: []agk.File{{
				Name:      "invoice.pdf",
				URI:       agk.URI{Run: run, Step: "invoice", Port: "out", Name: "invoice.pdf"},
				MediaType: "application/pdf",
				Size:      2048,
				SHA256:    strings.Repeat("a", 64),
			}},
		}},
	}
}

// TestTwoRunsOfOneThingDifferByTheThreeFactsAndNothingElse is the sentence the milestone
// rests on: the same envelopes, produced by two runs, match under the default.
func TestTwoRunsOfOneThingDifferByTheThreeFactsAndNothingElse(t *testing.T) {
	first := envelopeOf("01JMZ8V1P9C4XQ7K2N4D6F8H0A", time.Unix(1000, 0).UTC())
	second := envelopeOf("01JN000000000000000000000Z", time.Unix(9999, 0).UTC())

	want := map[agk.Port]agk.Envelope{"out": first}
	got := map[agk.Port]agk.Envelope{"out": second}

	if found := Envelopes(want, got, Default); len(found) != 0 {
		t.Errorf("the two runs differ by %d members and they differ by nothing but the run they were: %v", len(found), found)
	}

	// Held aside is a choice and not a property of the values: asked to compare them,
	// it names all three.
	found := Envelopes(want, got, 0)
	members := map[string]bool{}
	for _, d := range found {
		members[d.Member] = true
	}
	for _, member := range []string{"meta.run_id", "meta.produced_at", "files[invoice.pdf].uri"} {
		if !members[member] {
			t.Errorf("comparing everything did not name %s: %v", member, found)
		}
	}
}

func TestEveryMemberThatMovesIsNamedWithItsPortItsItemAndItsMember(t *testing.T) {
	run := agk.RunID("01JMZ8V1P9C4XQ7K2N4D6F8H0A")
	at := time.Unix(1000, 0).UTC()

	for _, c := range []struct {
		name   string
		change func(*agk.Envelope)
		member string
		line   string
	}{
		{
			name:   "an item identity",
			change: func(e *agk.Envelope) { e.Items[0].ID = "01JMZ8V1P9C4XQ7K2N4D6F8H0C" },
			member: "id",
		},
		{
			name:   "a data member",
			change: func(e *agk.Envelope) { e.Items[0].Data["vat_number"] = "FR99999999999" },
			member: "data.vat_number",
			line:   `port out: item 01JMZ8V1P9C4XQ7K2N4D6F8H0B: data.vat_number: want "FR12345678901", got "FR99999999999"`,
		},
		{
			name:   "a data member that was not written",
			change: func(e *agk.Envelope) { delete(e.Items[0].Data, "amount") },
			member: "data.amount",
		},
		{
			name:   "a data member nobody declared",
			change: func(e *agk.Envelope) { e.Items[0].Data["surprise"] = true },
			member: "data.surprise",
		},
		{
			name:   "a digest",
			change: func(e *agk.Envelope) { e.Items[0].Files[0].SHA256 = strings.Repeat("b", 64) },
			member: "files[invoice.pdf].sha256",
		},
		{
			name:   "a size",
			change: func(e *agk.Envelope) { e.Items[0].Files[0].Size = 4096 },
			member: "files[invoice.pdf].size",
		},
		{
			name:   "a media type",
			change: func(e *agk.Envelope) { e.Items[0].Files[0].MediaType = "text/plain" },
			member: "files[invoice.pdf].media_type",
		},
		{
			name:   "a file name",
			change: func(e *agk.Envelope) { e.Items[0].Files[0].Name = "receipt.pdf" },
			member: "files[0].name",
		},
		{
			name:   "an attachment that is not there",
			change: func(e *agk.Envelope) { e.Items[0].Files = nil },
			member: "files",
		},
		{
			name:   "the length of the batch",
			change: func(e *agk.Envelope) { e.Items = nil; e.Meta.Count = 0 },
			member: "meta.count",
		},
		{
			name:   "the step it came from",
			change: func(e *agk.Envelope) { e.Meta.Step = "archive" },
			member: "meta.step",
		},
		{
			name:   "the attempt it was on",
			change: func(e *agk.Envelope) { e.Meta.Attempt = 2 },
			member: "meta.attempt",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			want := envelopeOf(run, at)
			got := envelopeOf(run, at)
			c.change(&got)

			found := Envelopes(map[agk.Port]agk.Envelope{"out": want}, map[agk.Port]agk.Envelope{"out": got}, Default)
			named := false
			for _, d := range found {
				if d.Member == c.member {
					named = true
				}
				if d.Port != "out" {
					t.Errorf("the difference does not name the port: %v", d)
				}
			}
			if !named {
				t.Fatalf("%s was not named as %s: %v", c.name, c.member, found)
			}
			if c.line != "" && found[0].String() != c.line {
				t.Errorf("the line reads\n\t%s\nand one line names the port, the item and the member:\n\t%s", found[0], c.line)
			}
		})
	}
}

func TestAPortOnOneSideAloneIsADifferenceAboutThePort(t *testing.T) {
	run := agk.RunID("01JMZ8V1P9C4XQ7K2N4D6F8H0A")
	at := time.Unix(1000, 0).UTC()
	out := envelopeOf(run, at)

	found := Envelopes(map[agk.Port]agk.Envelope{"out": out, "rejected": out}, map[agk.Port]agk.Envelope{"out": out}, Default)
	if len(found) != 1 || found[0].Port != "rejected" || found[0].Item != "" || found[0].Member != "" {
		t.Fatalf("a port that was expected and not published reads as %v", found)
	}
	if got, want := found[0].String(), "port rejected: want an envelope, got nothing"; got != want {
		t.Errorf("the line reads %q and it reads %q", got, want)
	}

	found = Envelopes(map[agk.Port]agk.Envelope{"out": out}, map[agk.Port]agk.Envelope{"out": out, "extra": out}, Default)
	if len(found) != 1 || found[0].Port != "extra" {
		t.Fatalf("a port that was published and not expected reads as %v", found)
	}
	if got, want := found[0].String(), "port extra: want nothing, got an envelope of 1 items"; got != want {
		t.Errorf("the line reads %q and it reads %q", got, want)
	}
}

// TestItemIdentitiesAreComparedUnlessTheCallerGivesThemUp is the assertion with the most
// behind it: an item holds its own identity, and a brick that mints a fresh one on every
// pass has thrown that away.
func TestItemIdentitiesAreComparedUnlessTheCallerGivesThemUp(t *testing.T) {
	run := agk.RunID("01JMZ8V1P9C4XQ7K2N4D6F8H0A")
	at := time.Unix(1000, 0).UTC()
	want := envelopeOf(run, at)
	got := envelopeOf(run, at)
	got.Items[0].ID = "01JMZ8V1P9C4XQ7K2N4D6F8H0C"

	if found := Envelopes(map[agk.Port]agk.Envelope{"out": want}, map[agk.Port]agk.Envelope{"out": got}, Default); len(found) != 1 || found[0].Member != "id" {
		t.Fatalf("a minted identity is %v, and the default compares identities", found)
	}
	if found := Envelopes(map[agk.Port]agk.Envelope{"out": want}, map[agk.Port]agk.Envelope{"out": got}, Default|IgnoreItemIDs); len(found) != 0 {
		t.Fatalf("items.id was held aside and the difference is still %v", found)
	}

	// With no identity to name the item by, the rank it travels at is the handle.
	got.Items[0].Data["vat_number"] = "FR99999999999"
	found := Envelopes(map[agk.Port]agk.Envelope{"out": want}, map[agk.Port]agk.Envelope{"out": got}, Default|IgnoreItemIDs)
	if len(found) != 1 || found[0].Item != "#1" {
		t.Fatalf("the item is named %q, and with identities held aside it is named by its rank", found[0].Item)
	}
}

func TestANumberIsComparedByValueAndNotBySpelling(t *testing.T) {
	run := agk.RunID("01JMZ8V1P9C4XQ7K2N4D6F8H0A")
	at := time.Unix(1000, 0).UTC()
	want := envelopeOf(run, at)
	got := envelopeOf(run, at)
	got.Items[0].Data["amount"] = json.Number("120.5000")

	if found := Envelopes(map[agk.Port]agk.Envelope{"out": want}, map[agk.Port]agk.Envelope{"out": got}, Default); len(found) != 0 {
		t.Errorf("120.50 and 120.5000 are %v apart, and they are one number", found)
	}

	got.Items[0].Data["amount"] = json.Number("120.51")
	if found := Envelopes(map[agk.Port]agk.Envelope{"out": want}, map[agk.Port]agk.Envelope{"out": got}, Default); len(found) != 1 {
		t.Errorf("120.50 and 120.51 are two numbers and the difference is %v", found)
	}
}

func TestANestedMemberIsNamedAtThePathTheDocumentWritesIt(t *testing.T) {
	run := agk.RunID("01JMZ8V1P9C4XQ7K2N4D6F8H0A")
	at := time.Unix(1000, 0).UTC()
	want := envelopeOf(run, at)
	want.Items[0].Data["customer"] = map[string]any{"name": "Acme", "regions": []any{"eu", "us"}}
	got := envelopeOf(run, at)
	got.Items[0].Data["customer"] = map[string]any{"name": "Acme", "regions": []any{"eu", "ch"}}

	found := Envelopes(map[agk.Port]agk.Envelope{"out": want}, map[agk.Port]agk.Envelope{"out": got}, Default)
	if len(found) != 1 || found[0].Member != "data.customer.regions[1]" {
		t.Fatalf("the nested member reads as %v", found)
	}

	// A member whose shape changed is one difference at the shape, not one per element.
	got.Items[0].Data["customer"] = map[string]any{"name": "Acme", "regions": "eu"}
	found = Envelopes(map[agk.Port]agk.Envelope{"out": want}, map[agk.Port]agk.Envelope{"out": got}, Default)
	if len(found) != 1 || found[0].Member != "data.customer.regions" {
		t.Fatalf("a list replaced by a string reads as %v", found)
	}
	if !strings.Contains(found[0].String(), `want ["eu","us"], got "eu"`) {
		t.Errorf("the line does not show both shapes: %s", found[0])
	}
}
