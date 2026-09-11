package graph

import (
	"testing"

	"github.com/agentiik/agentiik/agk"
)

func TestParsePathReadsWhatAKeyJoinNeeds(t *testing.T) {
	for _, path := range []string{
		"$.data.customer_id",
		"$.id",
		"$.data.order.customer.id",
		`$.data["customer id"]`,
		"$.files[0].name",
	} {
		if _, err := parsePath(path); err != nil {
			t.Errorf("parsePath(%q): %v", path, err)
		}
	}
}

func TestParsePathRefusesWhatCannotMatch(t *testing.T) {
	for _, c := range []struct{ path, why string }{
		{"", "a join matches items on a JSON path"},
		{"data.customer_id", "a path that does not start at the item"},
		{"$", "the item itself is not a value to match by"},
		{"$.", "a dot naming nothing"},
		{"$.customer_id", "a member an item does not carry"},
		{"$.data[0", "a bracket left open"},
		{"$.data[one]", "an index that is not a number"},
	} {
		if _, err := parsePath(c.path); err == nil {
			t.Errorf("parsePath(%q) was accepted, and %s", c.path, c.why)
		}
	}
}

func TestPathReadsTheValueTheItemCarries(t *testing.T) {
	it := agk.Item{
		ID: "01ITEM",
		Data: map[string]any{
			"customer_id": "c-1",
			"order":       map[string]any{"total": 12.5},
			"note":        nil,
		},
		Files: []agk.File{{Name: "invoice.pdf"}},
	}

	for _, c := range []struct {
		path string
		want any
		ok   bool
	}{
		{"$.data.customer_id", "c-1", true},
		{"$.id", "01ITEM", true},
		{"$.data.order.total", 12.5, true},
		{"$.files[0].name", "invoice.pdf", true},
		{"$.data.missing", nil, false},
		{"$.files[1].name", nil, false},
		{"$.data.order.total.deeper", nil, false},
	} {
		got, ok := parsePathOrFail(t, c.path).value(it)
		if ok != c.ok {
			t.Errorf("%s: carried %v, want %v", c.path, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("%s: read %v, want %v", c.path, got, c.want)
		}
	}
}

// A key that is null is a key the item does not carry: matching every item missing the
// key against every other would join rows that have nothing in common.
func TestNullIsNotAKey(t *testing.T) {
	sel := parsePathOrFail(t, "$.data.customer_id")
	if _, ok := sel.key(agk.Item{ID: "01", Data: map[string]any{"customer_id": nil}}); ok {
		t.Error("an item whose key is null matched")
	}
	if _, ok := sel.key(agk.Item{ID: "01", Data: map[string]any{}}); ok {
		t.Error("an item missing the key matched")
	}
}

// Two items match when the document says they carry the same value, so the comparison is
// made on the encoding and a number is not the string of it.
func TestKeysCompareAsTheDocumentWritesThem(t *testing.T) {
	sel := parsePathOrFail(t, "$.data.k")
	number, _ := sel.key(agk.Item{ID: "a", Data: map[string]any{"k": 7.0}})
	text, _ := sel.key(agk.Item{ID: "b", Data: map[string]any{"k": "7"}})
	if number == text {
		t.Errorf("7 and %q compared equal, as %q", "7", number)
	}
	same, _ := sel.key(agk.Item{ID: "c", Data: map[string]any{"k": 7.0}})
	if number != same {
		t.Errorf("the same number compared unequal: %q and %q", number, same)
	}
}

func parsePathOrFail(t *testing.T, path string) selector {
	t.Helper()
	sel, err := parsePath(path)
	if err != nil {
		t.Fatalf("parsePath(%q): %v", path, err)
	}
	return sel
}
