// Package shown makes text that came from an installation or a container safe to draw on a
// terminal: what agk console draws, and what agk logs and agk run --local write where their output
// is a terminal. Whoever pushed a workflow, or whatever data a brick read, wrote that text, and the
// person reading it trusts what the screen says, so nothing in it may move the cursor, paint a false
// state, rename the window, write the clipboard or draw a link whose text is not its target.
package shown

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// tabStop is every eighth column, where a terminal stops a tab unless told otherwise.
const tabStop = 8

// Text is s as it can be drawn: what it says, and nothing a terminal would act on.
//
// An escape sequence, CSI, OSC, DCS, SOS, PM, APC or one of the short ones, is dropped whole with
// its parameters, the colour a brick writes with the rest. Shown, a sequence would put a
// clipboard's worth of base64 or an address in the middle of the line being read; kept because it
// looked harmless, it would need a judgement of which sequences are, and SGR alone paints a false
// state in the colours of a true one. Every other control but a tab, C0, DEL and C1 (U+0080 to
// U+009F), is shown as Go quotes it, \r, \x1b, \u009b, and a byte that is not UTF-8 as \xff, so that
// what is drawn is text a UTF-8 terminal reads as it is written: a carriage return or a NUL in a log may be the
// very thing the person diagnosing a run needs to see. A tab is spaces to the next of every eighth
// column, counted from the start of s, so that what is measured is what is drawn. Ordinary text,
// wide and combining characters included, is returned as it is.
//
// What Text returns holds no control, so Text of it is itself: it is applied where a line is
// measured and again where it is drawn, and both agree.
func Text(s string) string {
	if plain(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	// col is the column the text written up to measured stands at, which a tab is counted from;
	// what lies between is measured only when a tab needs it.
	col, measured := 0, 0
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == 0x1b:
			if n := sequence(s[i:]); n > 0 {
				i += n
				continue
			}
			b.WriteString(quoted(s[i : i+1]))
			i++
		case c == '\t':
			col += ansi.StringWidth(b.String()[measured:])
			n := tabStop - col%tabStop
			b.WriteString(strings.Repeat(" ", n))
			col += n
			measured = b.Len()
			i++
		case c < 0x20 || c == 0x7f:
			b.WriteString(quoted(s[i : i+1]))
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 || r >= 0x80 && r <= 0x9f {
				b.WriteString(quoted(s[i : i+size]))
			} else {
				b.WriteString(s[i : i+size])
			}
			i += size
		}
	}
	return b.String()
}

// quoted is a control or a byte that is not UTF-8 as a Go string literal writes it, without its
// quotes: \r, \x1b, \x7f, \u009b, \xff.
func quoted(s string) string {
	q := strconv.Quote(s)
	return q[1 : len(q)-1]
}

// plain says whether s holds nothing Text changes: no C0 control, no DEL, no C1, which UTF-8
// writes as 0xC2 and a byte under 0xA0, and nothing that is not UTF-8. Most text is, and is
// returned after one look at each byte.
func plain(s string) bool {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c < 0x20 || c == 0x7f:
			return false
		case c == 0xc2 && i+1 < len(s) && s[i+1] < 0xa0:
			return false
		}
	}
	return utf8.ValidString(s)
}

// sequence is how many bytes the escape sequence ESC starts at the beginning of s takes, as ECMA-48
// writes them, or 0 where ESC starts none and is shown. One cut short by the end of s, or by a byte
// that cannot be in it, ends there.
func sequence(s string) int {
	if len(s) < 2 {
		return 0
	}
	switch c := s[1]; {
	case c == '[':
		// CSI: parameters and intermediates, then the final byte.
		i := 2
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x3f {
			i++
		}
		if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
			i++
		}
		return i
	case c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_':
		// OSC, DCS, SOS, PM and APC: a string, ended by BEL, ST as ESC \ or as U+009C, or by an
		// ESC starting another sequence. One never ended runs to the end of s, as a terminal
		// would swallow it.
		for i := 2; i < len(s); i++ {
			switch {
			case s[i] == 0x07:
				return i + 1
			case s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\':
				return i + 2
			case s[i] == 0x1b:
				return i
			case s[i] == 0xc2 && i+1 < len(s) && s[i+1] == 0x9c:
				return i + 2
			}
		}
		return len(s)
	case c >= 0x20 && c <= 0x2f:
		// nF, such as ESC ( B, which chooses a character set: intermediates, then the final byte.
		i := 2
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
			i++
		}
		if i < len(s) && s[i] >= 0x30 && s[i] <= 0x7e {
			i++
		}
		return i
	case c >= 0x30 && c <= 0x7e:
		// Fp, Fe and Fs, two bytes, such as ESC 7, which saves the cursor, and ESC c, which resets
		// the terminal.
		return 2
	}
	return 0
}
