package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/agentiik/agentiik/cmd/agk/internal/shown"
)

// What a container wrote, and what an installation says about it, reaches a terminal as agk
// console draws it: each escape sequence dropped whole and every other control but a tab shown as
// its escape (package shown). A brick, or data it echoes, can write OSC 52 and replace the
// clipboard of whoever reads its log, rename their window or rewrite their screen, and the person
// reading a run's logs is rarely the person who wrote its workflow. To a file or a pipe the bytes
// are written as they are, for a script that wants them.

// shows says whether w is a terminal, read from the file itself as attached reads standard input
// and output: a character device is what a terminal is to the system. Each stream is read on its
// own, since agk logs writes to standard output and agk run --local to standard error, and TERM is
// not read, since a dumb terminal still receives the bytes.
func shows(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// screened is w where it is no terminal, and w writing each line as agk console draws it where it
// is one.
func (e Env) screened(w io.Writer) io.Writer {
	if e.Shows == nil || !e.Shows(w) {
		return w
	}
	return screen{w}
}

// jsonShown is a JSON document as it is written to w: as it was encoded to a file or a pipe, and
// to a terminal with DEL and every C1 control written as its \u escape, and a byte that is not
// UTF-8 as \ufffd. encoding/json escapes every C0 control and leaves these as they are, and a
// terminal may act on a C1 control as it does on ESC: U+009B is CSI. None can stand anywhere but
// inside a string, where its escape is the same character to whatever parses the document, so the
// document read off the screen is the one encoded; a byte that is not UTF-8 is read as U+FFFD by a
// JSON reader anyway. The document is not passed through shown.Text, which writes DEL as \x7f, an
// escape JSON does not have.
func (e Env) jsonShown(w io.Writer, doc []byte) []byte {
	if e.Shows == nil || !e.Shows(w) {
		return doc
	}
	var b strings.Builder
	for i := 0; i < len(doc); {
		r, size := utf8.DecodeRune(doc[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			b.WriteString(`\ufffd`)
		case r == 0x7f || r >= 0x80 && r <= 0x9f:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.Write(doc[i : i+size])
		}
		i += size
	}
	return []byte(b.String())
}

// screen writes each line written to it as shown.Text has it, the newlines between kept. Each line
// is written whole, one call of fmt each, so a tab is counted from where the terminal counts it;
// a sequence split across two writes would be shown in part as text, and still reach the terminal
// as no control.
type screen struct{ w io.Writer }

func (s screen) Write(p []byte) (int, error) {
	lines := strings.Split(string(p), "\n")
	for i, l := range lines {
		lines[i] = shown.Text(l)
	}
	if _, err := io.WriteString(s.w, strings.Join(lines, "\n")); err != nil {
		return 0, err
	}
	return len(p), nil
}
