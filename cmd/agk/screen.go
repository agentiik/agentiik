package main

import (
	"io"
	"os"
	"strings"

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
