package expr_test

import (
	"strings"
	"testing"

	"github.com/agentiik/agentiik/internal/expr"
)

// TestTheRewriteIsWhatTheParserNeedsAndNothingMore holds the pass to what it is for. The
// left column is what an author writes and the right is what the parser reads, and the
// two are the same text everywhere except where CEL cannot read a name the language
// allows.
func TestTheRewriteIsWhatTheParserNeedsAndNothingMore(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		escaped string
	}{
		// The language's canonical expression, and the reason this pass exists:
		// in is the input port the short form of an edge feeds.
		{"the input port named in", "inputs.in.count > 0", "inputs.`in`.count > 0"},
		{"the other three keywords", "a.true && b.false || c.null", "a.`true` && b.`false` || c.`null`"},
		{"a reserved word that is not a keyword", "vars.as", "vars.`as`"},
		{"a hyphenated port", "inputs.my-port.count", "inputs.`my-port`.count"},
		{"a hyphenated step", "steps.fetch-orders.status", "steps.`fetch-orders`.status"},
		{"a port beginning with a digit", "inputs.2fa.empty", "inputs.`2fa`.empty"},

		// Everything else is left exactly as it was written.
		{"a plain selector", "workflow.inputs.orders", "workflow.inputs.orders"},
		{"a comparison", "event.data.amount > 0", "event.data.amount > 0"},
		{"a float", "vars.ratio > 1.5", "vars.ratio > 1.5"},
		{"an index", `inputs["in"].count`, `inputs["in"].count`},
		{"a method call", `vars.name.startsWith("a")`, `vars.name.startsWith("a")`},
		{"a macro", "vars.list.exists(x, x > 1)", "vars.list.exists(x, x > 1)"},
		{"an identifier already escaped by hand", "inputs.`my-port`.count", "inputs.`my-port`.count"},
		{"a ternary", "vars.a ? vars.b : vars.c", "vars.a ? vars.b : vars.c"},
		{"an optional select", "vars.?a", "vars.?a"},
		// in is a keyword after a dot and the membership operator between two
		// values, and the pass touches only the first.
		{"in as the operator it also is", `"fr" in vars.regions`, `"fr" in vars.regions`},
		{"both uses of in at once", `"fr" in inputs.in.regions`, `"fr" in inputs.` + "`in`" + `.regions`},

		// A hyphen is a name character between two names and the operator
		// otherwise, which is the reading escape.go records.
		{"a subtraction written with spaces", "inputs.in.count - vars.floor", "inputs.`in`.count - vars.floor"},
		{"a subtraction of a number", "inputs.in.count-1", "inputs.`in`.count-1"},

		// A string is text, whatever it says.
		{"a keyword inside a string", `vars.name == "a.in.b"`, `vars.name == "a.in.b"`},
		{"a brace inside a string", `vars.name == "}}"`, `vars.name == "}}"`},
		{"a raw string ending on a backslash", `vars.name == r"a\"`, `vars.name == r"a\"`},
		{"a triple quoted string", `vars.name == """a.in"""`, `vars.name == """a.in"""`},
		{"a comment", "vars.a // vars.in\n", "vars.a // vars.in\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			escaped, offsets, err := expr.Escape(c.src)
			if err != nil {
				t.Fatalf("refused %q: %v", c.src, err)
			}
			if escaped != c.escaped {
				t.Fatalf("read as %q, and the parser needs %q", escaped, c.escaped)
			}
			if len(offsets) != len(escaped)+1 {
				t.Fatalf("%d offsets for %d bytes: every byte maps back, and the end maps back too", len(offsets), len(escaped))
			}
			for i, at := range offsets {
				if at < 0 || at > len(c.src) {
					t.Fatalf("byte %d maps to offset %d, which is outside the source", i, at)
				}
				if i > 0 && at < offsets[i-1] {
					t.Fatalf("byte %d maps backwards, to %d after %d", i, at, offsets[i-1])
				}
			}
		})
	}
}

// TestAPositionStillPointsIntoTheAuthorsText is the whole reason the pass returns a map
// rather than only the rewritten text: the parser counts in the text it read, and a
// person reads the text they wrote.
func TestAPositionStillPointsIntoTheAuthorsText(t *testing.T) {
	const src = "inputs.in.count > 0"
	escaped, offsets, err := expr.Escape(src)
	if err != nil {
		t.Fatal(err)
	}
	// The count in the rewritten text has moved by the opening backtick, and the
	// offset map takes it back to where the author wrote it.
	at := strings.Index(escaped, "count")
	if at < 0 {
		t.Fatalf("the rewrite lost the field: %q", escaped)
	}
	if offsets[at] != strings.Index(src, "count") {
		t.Fatalf("count maps to offset %d, and the author wrote it at %d", offsets[at], strings.Index(src, "count"))
	}
}

// TestAnUnclosedLiteralIsRefusedWhereItOpens keeps the scan honest: a pass that guessed
// where a string ends would rewrite text that is inside one.
func TestAnUnclosedLiteralIsRefusedWhereItOpens(t *testing.T) {
	for _, src := range []string{`vars.a == "b`, "vars.a == `b", `vars.a == """b`, "vars.a == \"b\nc\""} {
		if _, _, err := expr.Escape(src); err == nil {
			t.Fatalf("accepted %q, which never closes what it opens", src)
		}
	}
}
