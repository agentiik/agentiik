package shown

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// What a log line, a payload or a name of a workflow can hold, and what is drawn of it: the strings
// of agentiik/agentiik#858 and #860, each escape sequence dropped whole and every other control but
// a tab shown as its escape.
func TestTextDropsSequencesAndShowsControls(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		// C1 controls, which encoding/json leaves in a string and a terminal acting on UTF-8 C1
		// takes for CSI, OSC and ST.
		{"c1-csi \u009b2J c1-osc \u009d52;c;QzFDbGlw\u009c end", `c1-csi \u009b2J c1-osc \u009d52;c;QzFDbGlw\u009c end`},
		// A hyperlink whose text is one address and whose target another is its text alone.
		{"\x1b]8;;https://evil.example/login\ahttps://agentiik.acme.example/runs\x1b]8;;\a", "https://agentiik.acme.example/runs"},
		// A carriage return that would paint a false state over the line it is in.
		{"spoof\r\x1b[32m● succeeded\x1b[0m", `spoof\r● succeeded`},
		// The clipboard written and the window renamed, as agk logs wrote them.
		{"before \x1b]52;c;SGFja2Vk\a \x1b]0;pwned\a \u009b2J after", `before   \u009b2J after`},
		// Every kind of sequence goes whole: a reset, a test pattern, a character set, the cursor
		// saved and restored, the alternate screen, a DCS ended by ST, an OSC ended by ESC \ and
		// one by U+009C, and one never ended, which runs to the end.
		{"[\x1bc]", "[]"},
		{"[\x1b#8]", "[]"},
		{"[\x1b(B]", "[]"},
		{"[\x1b7\x1b8]", "[]"},
		{"[\x1b[?1049h]", "[]"},
		{"[\x1bP+q544e\x1b\\]", "[]"},
		{"[\x1b]0;title\x1b\\]", "[]"},
		{"[\x1b]0;title\u009c]", "[]"},
		{"[\x1b]0;title", "["},
		// A CSI cut short keeps nothing of itself, and what cut it is shown.
		{"[\x1b[12\r]", `[\r]`},
		// An ESC that starts no sequence is shown, and so is one at the very end.
		{"\x1b\x1b[31mred", `\x1bred`},
		{"end\x1b", `end\x1b`},
		{"\x1bé", `\x1bé`},
		// C0, DEL and a byte that is not UTF-8, as Go quotes them.
		{"nul \x00 bell \a bs \b ff \f vt \v lf \n del \x7f bad \xff\xfe", `nul \x00 bell \a bs \b ff \f vt \v lf \n del \x7f bad \xff\xfe`},
		// A tab is spaces to the next of every eighth column, counted from the start of the text,
		// wide and combining characters and the escapes shown included.
		{"a\tb\tc\td\t", "a       b       c       d       "},
		{"漢\tx", "漢      x"},
		{"é\tx", "é       x"},
		{"é\tx", "é       x"},
		{"\r\tx", `\r      x`},
		{"12345678\tx", "12345678        x"},
		{"\x1b[1mbold\x1b[0m\tx", "bold    x"},
	} {
		if got := Text(c.in); got != c.want {
			t.Errorf("Text(%q) is\n%q\nwhere it is\n%q", c.in, got, c.want)
		}
	}
}

// Ordinary text is drawn as it is, wide characters, emoji and combining marks included, and
// returned without a copy.
func TestTextLeavesOrdinaryTextAsItIs(t *testing.T) {
	for _, s := range []string{
		"",
		"invoice 2/3, attempt 4 | the ledger answered 503",
		"漢字 and かな, an emoji 🙂, a family 👨‍👩‍👧, a flag 🇫🇷, é and a no-break space",
		`a backslash \x1b is text`,
		"U+00A0 and after:  ÿĀ",
	} {
		if got := Text(s); got != s {
			t.Errorf("Text(%q) is %q", s, got)
		}
	}
}

// Whatever a code point from NUL to the last C1 control is, alone, between text, after an ESC or
// after a tab, what is drawn holds no control, is UTF-8, and is drawn again as it is: Text is
// applied where a line is measured and again where it is drawn.
func TestTextLeavesNoControlAndIsItsOwnResult(t *testing.T) {
	for r := rune(0); r <= 0x9f; r++ {
		c := string(r)
		for _, s := range []string{c, "a" + c + "b", "\x1b" + c + "tail", "x\t" + c + "\ty", "\x1b]" + c, "\x1b[" + c + "z"} {
			got := Text(s)
			if !utf8.ValidString(got) {
				t.Errorf("Text(%q) is %q, not UTF-8", s, got)
			}
			for _, g := range got {
				if g < 0x20 || g >= 0x7f && g <= 0x9f {
					t.Errorf("Text(%q) is %q, which holds %U", s, got, g)
				}
			}
			if again := Text(got); again != got {
				t.Errorf("Text(%q) is %q, and Text of that is %q", s, got, again)
			}
		}
	}
	// A byte of every value, not UTF-8 alone.
	for c := 0x80; c <= 0xff; c++ {
		s := "<" + string([]byte{byte(c)}) + ">"
		got := Text(s)
		if !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) || Text(got) != got {
			t.Errorf("Text(%q) is %q", s, got)
		}
	}
}
